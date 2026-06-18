//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// resourceAuthzBypass reports whether HEARTH_RESOURCE_AUTHZ_BYPASS
// is set to a truthy value. When true, the daemon skips both the
// boot-time rule seed and the per-invoke preflight authorize call —
// behaving the way pre-1e dogfooding did. Belt-and-suspenders for
// running the daemon disconnected from the server during local dev.
// Production binaries ship with this gate available; that's a known
// dev affordance (see docs/resource-plugins-1e-plan.md §7).
func resourceAuthzBypass() bool {
	switch os.Getenv("HEARTH_RESOURCE_AUTHZ_BYPASS") {
	case "1", "true", "TRUE", "yes":
		return true
	}
	return false
}

// authzWS is the subset of DaemonWS that the resource-plugin
// authorize preflight needs. Stubbed in tests so we can exercise
// handleResourceInvoke without a live server connection.
type authzWS interface {
	IsConnected() bool
	SendWSRequest(correlationID, msgType string, data json.RawMessage) ([]byte, error)
	// SendWSRequestTimeout overrides the default 30s wait. The
	// Ask path uses it because the server blocks on a human
	// response (≈10 min); standard CRUD calls don't.
	SendWSRequestTimeout(correlationID, msgType string, data json.RawMessage, timeout time.Duration) ([]byte, error)
}

// authorizeResourceInvokeReq mirrors the server-side shape in
// hearth-cmd/cmd/hearth-cloud/resource_plugins_ws.go. Kept in sync
// by hand.
type authorizeResourceInvokeReq struct {
	PrincipalID   string          `json:"principal_id"`
	PrincipalKind string          `json:"principal_kind"`
	ConnectionID  string          `json:"connection_id"`
	PluginSlug    string          `json:"plugin_slug"`
	Verb          string          `json:"verb"`
	RequestID     string          `json:"request_id"`
	// Args carries the verb's argument JSON so the Ask renderer
	// on the phone can show the human what they're approving. Empty
	// for invokes with no args. Plumbed in 1h-commit-5; the server
	// folds it into the Subject payload at authorize time.
	Args json.RawMessage `json:"args,omitempty"`
	// Entity context resolved from the daemon's resource_entities
	// cache when args.entity_id names something the daemon knows
	// about. Server feeds into EvalContext.Entity so manifest
	// default_rules predicating on entity_kind / labels actually
	// match. Empty when the verb didn't target an entity, when the
	// snapshot is stale, or when the args use a non-standard entity
	// arg name.
	EntityKind   string            `json:"entity_kind,omitempty"`
	EntityLabels map[string]string `json:"entity_labels,omitempty"`
	EntityParent string            `json:"entity_parent,omitempty"`
	// AIAgentInstanceID is the caller's self-reported agent instance id,
	// sent even when derivePrincipal resolved the principal as "human"
	// (e.g. stale PID registry after a daemon restart). The server uses
	// it as a fallback to populate ai_agent_instance_id in the WS
	// permission_request frame so the overlay auto-pops on the transcript
	// screen. Authorization still uses PrincipalKind/PrincipalID; this
	// field only affects display routing on the phone.
	AIAgentInstanceID string `json:"ai_agent_instance_id,omitempty"`
}

// humanReadableDenyMessage translates the server's machine-readable
// deny reasons into something a CLI user actually wants to see.
// Reasons that pass through unchanged include rule-matched denies
// ("denied by rule: ...") and anything else we don't recognize —
// surfacing the raw reason is better than hiding it.
func humanReadableDenyMessage(reason string) string {
	switch reason {
	case "":
		return "denied: no matching rule"
	case "human_timeout":
		return "approval timed out — no response from the approver"
	case "human:deny":
		return "denied by approver"
	case "human:allow":
		// Shouldn't happen (handler returns decision="allow" for this),
		// but guard against future drift.
		return "denied (server reported human:allow on a deny decision)"
	}
	return fmt.Sprintf("denied: %s", reason)
}

// askWSRequestTimeout is the per-call deadline for
// authorize_resource_invoke. Comfortably longer than the server's
// defaultTimeout (≈10 min) so the human-response path returns
// normally before the daemon-side timeout fires.
const askWSRequestTimeout = 12 * time.Minute

// authorizeResourceInvokeResp is the {decision, rule_id, reason}
// payload the server returns. Type is set to either the canonical
// response or "error".
type authorizeResourceInvokeResp struct {
	Type     string `json:"type"`
	Decision string `json:"decision"`
	RuleID   string `json:"rule_id"`
	Reason   string `json:"reason"`
	Error    string `json:"error"`
	// BindingID is populated on Allow when the server resolves a live
	// agent_resource_bindings row for (principal, connection). Empty
	// when no binding exists (Shape A connections, human principals).
	// Phase 3 step 5.
	BindingID string `json:"binding_id,omitempty"`
}

// preflightAuthorizeResourceInvoke is the 1e stopgap chokepoint
// the daemon calls before dispatching to PluginSupervisor.Invoke.
// Returns nil on Allow; a *PluginError on Deny or transport failure.
//
// 1g moves evaluation daemon-local and this function disappears.
// Until then it's the single decision point on the daemon side.
//
// args carries the verb's argument JSON, plumbed in 1h-commit-5 so
// Ask-path renders can show the human what they're approving.
// Empty when the invoke has no args.
// Returns (bindingID, *PluginError). bindingID is the server-resolved
// agent_resource_bindings row id for this invoke, or "" when the
// server didn't populate one (Shape A, human principals, server
// running pre-step-5). Caller threads it into PluginSupervisor.Invoke
// so plugin StateGet/Put RPCs scope correctly.
func (d *Daemon) preflightAuthorizeResourceInvoke(ws authzWS, principalKind, principalID, connID, pluginType, verb string, args json.RawMessage, claimedAgentInstanceID string) (string, *PluginError) {
	if resourceAuthzBypass() {
		return "", nil
	}
	if ws == nil || !ws.IsConnected() {
		return "", &PluginError{
			Code:    ErrUnavailable,
			Message: "daemon is offline from server; authorize is unavailable",
		}
	}
	if principalID == "" {
		return "", &PluginError{
			Code:    ErrUnauthorized,
			Message: "no authenticated principal; run `hearth login` first",
		}
	}
	if principalKind == "" {
		principalKind = "human"
	}
	requestID := generateUUID()
	entKind, entLabels, entParent := d.resolveEntityForAuthorize(connID, args)
	payload, err := json.Marshal(authorizeResourceInvokeReq{
		PrincipalID:       principalID,
		PrincipalKind:     principalKind,
		ConnectionID:      connID,
		PluginSlug:        pluginType,
		Verb:              verb,
		RequestID:         requestID,
		Args:              args,
		EntityKind:        entKind,
		EntityLabels:      entLabels,
		EntityParent:      entParent,
		AIAgentInstanceID: claimedAgentInstanceID,
	})
	if err != nil {
		return "", &PluginError{Code: ErrInternal, Message: "marshal authorize request: " + err.Error()}
	}
	raw, err := ws.SendWSRequestTimeout(generateUUID(), "authorize_resource_invoke", payload, askWSRequestTimeout)
	if err != nil {
		if strings.Contains(err.Error(), "timed out") {
			return "", &PluginError{
				Code:    ErrUnavailable,
				Message: "no response from server (daemon-side timeout; check daemon WS connectivity)",
			}
		}
		return "", &PluginError{Code: ErrUnavailable, Message: "authorize ws_request: " + err.Error()}
	}
	var resp authorizeResourceInvokeResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", &PluginError{Code: ErrInternal, Message: "decode authorize response: " + err.Error()}
	}
	if resp.Type == "error" {
		return "", &PluginError{Code: ErrInternal, Message: "server authorize error: " + resp.Error}
	}
	switch resp.Decision {
	case "allow":
		return resp.BindingID, nil
	case "deny":
		return "", &PluginError{
			Code:    ErrForbidden,
			Message: humanReadableDenyMessage(resp.Reason),
		}
	default:
		return "", &PluginError{
			Code:    ErrInternal,
			Message: "unexpected authorize decision: " + resp.Decision,
		}
	}
}

// resolveEntityForAuthorize looks args.entity_id up in the daemon-local
// resource_entities cache and returns its {kind, labels, parent} so
// the server-side IAM evaluator can match predicate rules against it.
//
// Best-effort: any failure (no DB, args not JSON, no entity_id field,
// cache miss) returns empty values. The authorize call still goes
// through; predicates that need an entity context just won't match,
// and the engine falls through to ask.
//
// "entity_id" as the canonical arg name is a convention shared by
// every entity-targeting verb we ship today (HA's get_state, turn_on,
// turn_off, lock, unlock, set_scene). Adapters that use a different
// arg name for the entity reference will silently bypass entity-
// context lookup — a manifest-level "which arg is the entity"
// declaration can land later if community plugins need it.
func (d *Daemon) resolveEntityForAuthorize(connID string, args json.RawMessage) (kind string, labels map[string]string, parent string) {
	if d.localDB == nil || len(args) == 0 {
		return "", nil, ""
	}
	var probe map[string]any
	if err := json.Unmarshal(args, &probe); err != nil {
		return "", nil, ""
	}
	entID, _ := probe["entity_id"].(string)
	if entID == "" {
		return "", nil, ""
	}
	ents, err := d.localDB.ListEntities(connID)
	if err != nil {
		return "", nil, ""
	}
	for _, e := range ents {
		if e.EntityID == entID {
			return e.Kind, e.Labels, e.Parent
		}
	}
	return "", nil, ""
}

// seedWSConnectTimeout caps how long boot goroutines (plugin install
// report, resource-connection fetch, pubkey enroll) wait for the
// daemon WS to come up before giving up. Generous: the WS auth
// handshake typically completes within seconds; the 60s ceiling
// avoids flapping on slow connects. Historical name retained
// across the broader use today.
const seedWSConnectTimeout = 60 * time.Second

// waitForDaemonWS polls d.daemonWS.IsConnected() with a short
// interval. Returns true when connected; false when timeout
// elapses. Tolerates a nil d.daemonWS during the poll loop
// (boot-order race) — keeps polling until either connection or
// timeout. Each tick is 250ms; the loop is cheap and bounded.
func (d *Daemon) waitForDaemonWS(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if d.daemonWS != nil && d.daemonWS.IsConnected() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return d.daemonWS != nil && d.daemonWS.IsConnected()
}
