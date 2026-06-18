package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// resource_decl.go — the declarative executor for plugin_installs.source
// == 'declarative'. Runs a single verb in-process by templating
// {config, credentials, args} into the manifest's VerbHTTPSpec, firing
// the HTTP request, mapping the response to an InvokeResult or a
// PluginError of the standard vocabulary.
//
// Design constraints (see hearth-cmd/docs/ha-yaml-adapter-plan.md):
//   - Templates are mustache-shaped {{scope.key.sub}}; unknown keys
//     are errors, not empty strings.
//   - Response body capped at declarativeMaxRespBody to keep a buggy
//     upstream from OOMing the daemon.
//   - Status codes outside the declared success set map to the
//     PluginError vocabulary so callers don't need a separate
//     translation layer.
//   - JSONPath extraction is a deliberately tiny subset: $.field +
//     dotted descent. No wildcards, no filters. If the manifest
//     needs more it's a sign we should bind a real engine; for HA
//     v1 we don't.

const (
	declarativeHTTPTimeout = 30 * time.Second
	declarativeMaxRespBody = 1 << 20 // 1 MiB; same backstop the executor budget assumes
)

// DeclarativeExecutor runs declarative HTTP verbs in-daemon. Holds a
// reusable http.Client so connection pooling works across invokes.
// One executor per daemon is fine; it's stateless beyond the client.
// OAuthTokenExchanger is implemented by the Daemon to exchange a
// decrypted refresh token for a short-lived access token via the
// server's exchange_oauth_token WS handler. The server holds the
// OAuth client_secret and calls the upstream provider.
type OAuthTokenExchanger interface {
	ExchangeOAuthToken(ctx context.Context, provider string, refreshToken []byte) (accessToken string, expiresIn int, err error)
}

type DeclarativeExecutor struct {
	client          *http.Client
	gcpTokenCache   *gcpTokenCache
	oauthExchanger  OAuthTokenExchanger // nil when not configured
	oauthTokenCache *oauthAccessCache
}

func NewDeclarativeExecutor() *DeclarativeExecutor {
	return &DeclarativeExecutor{
		client:          &http.Client{Timeout: declarativeHTTPTimeout},
		gcpTokenCache:   newGCPTokenCache(),
		oauthTokenCache: newOAuthAccessCache(),
	}
}

// SetOAuthExchanger wires the daemon's WS-based token exchanger into
// the executor. Called once after the executor is created, before any
// invocations that may use oauth2_* credential types.
func (x *DeclarativeExecutor) SetOAuthExchanger(e OAuthTokenExchanger) {
	x.oauthExchanger = e
}

// DeclarativeInvokeInput collects everything the executor needs to
// fire one verb call. Config + Credentials + Args become the template
// scope; Spec is the verb's VerbHTTPSpec from the manifest.
//
// Credentials is map[string]any rather than map[string]string so that
// typed credentials (e.g. service_account_json) can expose sub-keys
// such as {{credentials.my_key.access_token}}. Plain string credentials
// remain accessible as {{credentials.my_key}} (a scalar string value).
// Use expandCredentials (resource_decl_gcp.go) to build this map from
// the raw secrets map + manifest credential definitions.
type DeclarativeInvokeInput struct {
	Spec        *VerbHTTPSpec
	Config      map[string]any
	Credentials map[string]any
	Args        map[string]any
}

// Invoke runs one declarative call end-to-end. Returns a populated
// InvokeResult on success or a *PluginError of the standard vocabulary
// on any failure. The error surfaces unchanged through the daemon's
// existing IPC response path — no separate translation in
// handleResourceInvoke.
func (x *DeclarativeExecutor) Invoke(ctx context.Context, in DeclarativeInvokeInput) (InvokeResult, *PluginError) {
	if in.Spec == nil {
		return InvokeResult{}, &PluginError{Code: ErrInternal, Message: "declarative invoke: nil spec"}
	}

	scope := buildScope(in)

	url, err := substitute(in.Spec.URL, scope)
	if err != nil {
		return InvokeResult{}, templateErr("url", err)
	}
	body, err := substitute(in.Spec.Body, scope)
	if err != nil {
		return InvokeResult{}, templateErr("body", err)
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	method := strings.ToUpper(strings.TrimSpace(in.Spec.Method))
	req, rerr := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if rerr != nil {
		return InvokeResult{}, &PluginError{Code: ErrBadArgs, Message: "declarative invoke: build request: " + rerr.Error()}
	}
	for k, vTmpl := range in.Spec.Headers {
		v, herr := substitute(vTmpl, scope)
		if herr != nil {
			return InvokeResult{}, templateErr("header "+k, herr)
		}
		req.Header.Set(k, v)
	}

	resp, ferr := x.client.Do(req)
	if ferr != nil {
		// Context-cancellation maps to internal so the caller can
		// distinguish "we gave up" from "upstream said no." Timeouts
		// look like the same shape via net/url errors; both are local
		// fairness signals.
		if errors.Is(ferr, context.DeadlineExceeded) || errors.Is(ferr, context.Canceled) {
			return InvokeResult{}, &PluginError{Code: ErrInternal, Message: "declarative invoke: " + ferr.Error()}
		}
		return InvokeResult{}, &PluginError{Code: ErrUnavailable, Message: "declarative invoke: " + ferr.Error()}
	}
	defer resp.Body.Close()

	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, declarativeMaxRespBody+1))
	if rerr != nil {
		return InvokeResult{}, &PluginError{Code: ErrUnavailable, Message: "declarative invoke: read body: " + rerr.Error()}
	}
	if len(raw) > declarativeMaxRespBody {
		return InvokeResult{}, &PluginError{
			Code:    ErrUnavailable,
			Message: fmt.Sprintf("declarative invoke: response body exceeded %d bytes", declarativeMaxRespBody),
		}
	}

	if !statusInSet(resp.StatusCode, in.Spec.Response.SuccessStatus) {
		return InvokeResult{}, &PluginError{
			Code:    mapStatusToCode(resp.StatusCode),
			Message: fmt.Sprintf("upstream status %d: %s", resp.StatusCode, snippet(raw, 256)),
		}
	}

	stdout, eerr := applyOutput(in.Spec.Response.Output, raw)
	if eerr != nil {
		return InvokeResult{}, &PluginError{Code: ErrInternal, Message: "declarative invoke: extract output: " + eerr.Error()}
	}
	return InvokeResult{Stdout: stdout, ExitCode: 0}, nil
}

// buildScope assembles the lookup root for substitute(). Credentials
// live under "credentials.<name>"; config under "config.<key>"; args
// under "args.<key>". Maps are reused by reference — substitute()
// doesn't mutate.
func buildScope(in DeclarativeInvokeInput) map[string]any {
	return map[string]any{
		"config":      mapOrEmpty(in.Config),
		"credentials": credentialsScope(in.Credentials),
		"args":        mapOrEmpty(in.Args),
	}
}

func mapOrEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// substitute walks `template` and replaces every {{path.dotted}}
// occurrence with the corresponding value from scope. Unknown paths
// are errors (templates that silently render empty hide configuration
// bugs). Double-braces don't nest; the inner content is literal until
// the matching `}}`. Whitespace inside the braces is trimmed.
//
// Stringification:
//   - string → as-is
//   - bool   → "true"/"false"
//   - int / int64 / float64 → strconv.FormatFloat / strconv.FormatInt
//   - everything else (map/slice/nil) → error; declarative manifests
//     shouldn't be embedding structured values into URLs/headers.
func substitute(template string, scope map[string]any) (string, error) {
	if template == "" {
		return "", nil
	}
	var out strings.Builder
	out.Grow(len(template))
	i := 0
	for i < len(template) {
		open := strings.Index(template[i:], "{{")
		if open < 0 {
			out.WriteString(template[i:])
			break
		}
		out.WriteString(template[i : i+open])
		close := strings.Index(template[i+open:], "}}")
		if close < 0 {
			return "", fmt.Errorf("unterminated {{ near %q", template[i+open:])
		}
		path := strings.TrimSpace(template[i+open+2 : i+open+close])
		i += open + close + 2
		if path == "" {
			return "", fmt.Errorf("empty template path")
		}
		s, err := evalTemplateExpr(scope, path)
		if err != nil {
			return "", err
		}
		out.WriteString(s)
	}
	return out.String(), nil
}

// templateFuncs whitelists the one-arg helpers callable from
// {{name(path)}} inside a template. Keep the list tight — the engine's
// power-vs-foot-gun budget is meant to stay small. Add a helper only
// when a manifest can't be written cleanly without it.
var templateFuncs = map[string]func(string) string{
	// domain returns the portion of its input before the first ".".
	// Lets HA's service URLs say turn_on of "{{domain(args.entity_id)}}"
	// → "light" → /api/services/light/turn_on.
	"domain": entityIDDomain,
}

// evalTemplateExpr evaluates the contents of one {{ ... }} pair. Two
// shapes are accepted:
//
//	dotted.path        — look up in scope, stringify
//	funcname(dotted)   — look up dotted, apply funcname (templateFuncs)
//
// Unknown function names and missing paths are errors. The function
// always receives the dotted lookup as a string scalar — non-scalar
// inputs error before the function runs.
func evalTemplateExpr(scope map[string]any, expr string) (string, error) {
	if i := strings.Index(expr, "("); i >= 0 {
		if !strings.HasSuffix(expr, ")") {
			return "", fmt.Errorf("template expr %q: unbalanced parentheses", expr)
		}
		name := strings.TrimSpace(expr[:i])
		arg := strings.TrimSpace(expr[i+1 : len(expr)-1])
		fn, ok := templateFuncs[name]
		if !ok {
			return "", fmt.Errorf("template expr %q: unknown function %q", expr, name)
		}
		v, err := lookupPath(scope, arg)
		if err != nil {
			return "", err
		}
		s, err := scalarString(v)
		if err != nil {
			return "", fmt.Errorf("template arg %q: %w", arg, err)
		}
		return fn(s), nil
	}
	v, err := lookupPath(scope, expr)
	if err != nil {
		return "", err
	}
	s, err := scalarString(v)
	if err != nil {
		return "", fmt.Errorf("template path %q: %w", expr, err)
	}
	return s, nil
}

func lookupPath(scope map[string]any, path string) (any, error) {
	parts := strings.Split(path, ".")
	var cur any = scope
	for idx, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("template path %q: %s is not an object", path, strings.Join(parts[:idx], "."))
		}
		next, found := m[p]
		if !found {
			return nil, fmt.Errorf("template path %q: %s not set", path, strings.Join(parts[:idx+1], "."))
		}
		cur = next
	}
	return cur, nil
}

func scalarString(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		// JSON-decoded numbers come through as float64. Render integers
		// without a trailing ".0" so URL paths look right.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10), nil
		}
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case nil:
		return "", fmt.Errorf("value is nil")
	default:
		return "", fmt.Errorf("non-scalar value of type %T", v)
	}
}

// statusInSet reports whether code is in expected. Empty/nil expected
// defaults to [200] — the common case and what manifests usually omit.
func statusInSet(code int, expected []int) bool {
	if len(expected) == 0 {
		return code == 200
	}
	for _, e := range expected {
		if code == e {
			return true
		}
	}
	return false
}

// mapStatusToCode translates an HTTP status outside the success set
// into a PluginError code. Picked to match the wire vocabulary's
// semantics: bad_args for caller-fault 4xx, forbidden/unauthorized for
// the auth-specific 4xx, unavailable for upstream 5xx.
func mapStatusToCode(status int) ErrorCode {
	switch {
	case status == 401:
		return ErrUnauthorized
	case status == 403:
		return ErrForbidden
	case status >= 400 && status < 500:
		return ErrBadArgs
	case status >= 500:
		return ErrUnavailable
	default:
		// 1xx/2xx/3xx that weren't in the success set are weird; surface
		// as internal so the operator notices.
		return ErrInternal
	}
}

// applyOutput interprets the response per the manifest's Output field:
//   - empty → return the raw body string.
//   - starts with "$" → JSONPath; descend through the decoded body and
//     stringify the leaf (or marshal back to JSON for objects/arrays).
//   - anything else → literal string (e.g. "ok" for fire-and-forget
//     verbs whose response we don't care about).
func applyOutput(output string, raw []byte) (string, error) {
	if output == "" {
		return string(raw), nil
	}
	if !strings.HasPrefix(output, "$") {
		return output, nil
	}
	return jsonPathExtract(raw, output)
}

// jsonPathExtract is the tiny JSONPath subset: $.field, $.field.sub,
// $.field.sub.deeper. No wildcards, filters, or index access. Returns
// the leaf as a string (scalars stringified the same way templates
// stringify; objects/arrays re-marshaled to JSON). Empty body and
// $-only path return the raw body verbatim.
func jsonPathExtract(raw []byte, path string) (string, error) {
	if path == "$" {
		return string(raw), nil
	}
	if !strings.HasPrefix(path, "$.") {
		return "", fmt.Errorf("jsonpath %q: must start with $.", path)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("jsonpath %q: response is not JSON: %w", path, err)
	}
	cur := doc
	parts := strings.Split(path[2:], ".")
	for idx, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", fmt.Errorf("jsonpath %q: %s is not an object", path, strings.Join(parts[:idx], "."))
		}
		next, found := m[p]
		if !found {
			return "", fmt.Errorf("jsonpath %q: %s not present", path, strings.Join(parts[:idx+1], "."))
		}
		cur = next
	}
	switch t := cur.(type) {
	case string:
		return t, nil
	case nil:
		return "", nil
	case map[string]any, []any:
		b, err := json.Marshal(t)
		if err != nil {
			return "", fmt.Errorf("jsonpath %q: remarshal: %w", path, err)
		}
		return string(b), nil
	default:
		return scalarString(cur)
	}
}

func templateErr(field string, err error) *PluginError {
	return &PluginError{Code: ErrBadArgs, Message: "declarative invoke: " + field + ": " + err.Error()}
}

// Entity is the normalized record the daemon stores per addressable
// thing a Resource Connection exposes — the unit IAM rules predicate
// on. Per docs/external-resource-adapters.md §"Entity shape". Returned
// by DeclarativeExecutor.RunSnapshot for declarative adapters; binary
// plugins emit equivalent shapes via their Onboard step.
type Entity struct {
	EntityID string
	Kind     string
	Labels   map[string]string
	Parent   string
}

// DeclarativeSnapshotInput is what RunSnapshot needs: the manifest's
// SnapshotSpec, plus the connection-scope template inputs. No Args —
// snapshots aren't per-call. Credentials follows the same map[string]any
// convention as DeclarativeInvokeInput — use expandCredentials to build it.
type DeclarativeSnapshotInput struct {
	Spec        *SnapshotSpec
	Config      map[string]any
	Credentials map[string]any
}

// RunSnapshot fires the manifest's snapshot HTTP call, parses the
// response, walks the iterate path, and emits one Entity per matched
// item with kind/labels/parent derived per ExtractSpec. Returns
// *PluginError on any failure so the daemon-side caller can surface
// the same vocabulary as a regular Invoke.
func (x *DeclarativeExecutor) RunSnapshot(ctx context.Context, in DeclarativeSnapshotInput) ([]Entity, *PluginError) {
	if in.Spec == nil {
		return nil, &PluginError{Code: ErrInternal, Message: "snapshot: nil spec"}
	}
	scope := map[string]any{
		"config":      mapOrEmpty(in.Config),
		"credentials": credentialsScope(in.Credentials),
	}

	url, err := substitute(in.Spec.HTTP.URL, scope)
	if err != nil {
		return nil, templateErr("snapshot.url", err)
	}
	body, err := substitute(in.Spec.HTTP.Body, scope)
	if err != nil {
		return nil, templateErr("snapshot.body", err)
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, rerr := http.NewRequestWithContext(ctx, strings.ToUpper(strings.TrimSpace(in.Spec.HTTP.Method)), url, bodyReader)
	if rerr != nil {
		return nil, &PluginError{Code: ErrBadArgs, Message: "snapshot: build request: " + rerr.Error()}
	}
	for k, vTmpl := range in.Spec.HTTP.Headers {
		v, herr := substitute(vTmpl, scope)
		if herr != nil {
			return nil, templateErr("snapshot.header "+k, herr)
		}
		req.Header.Set(k, v)
	}

	resp, ferr := x.client.Do(req)
	if ferr != nil {
		if errors.Is(ferr, context.DeadlineExceeded) || errors.Is(ferr, context.Canceled) {
			return nil, &PluginError{Code: ErrInternal, Message: "snapshot: " + ferr.Error()}
		}
		return nil, &PluginError{Code: ErrUnavailable, Message: "snapshot: " + ferr.Error()}
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, declarativeMaxRespBody+1))
	if rerr != nil {
		return nil, &PluginError{Code: ErrUnavailable, Message: "snapshot: read body: " + rerr.Error()}
	}
	if len(raw) > declarativeMaxRespBody {
		return nil, &PluginError{
			Code:    ErrUnavailable,
			Message: fmt.Sprintf("snapshot: response body exceeded %d bytes", declarativeMaxRespBody),
		}
	}
	if !statusInSet(resp.StatusCode, in.Spec.HTTP.Response.SuccessStatus) {
		return nil, &PluginError{
			Code:    mapStatusToCode(resp.StatusCode),
			Message: fmt.Sprintf("snapshot: upstream status %d: %s", resp.StatusCode, snippet(raw, 256)),
		}
	}

	items, eerr := jsonPathIterate(raw, in.Spec.Extract.Iterate)
	if eerr != nil {
		return nil, &PluginError{Code: ErrInternal, Message: "snapshot: " + eerr.Error()}
	}

	out := make([]Entity, 0, len(items))
	for i, item := range items {
		ent, eerr := extractEntity(item, in.Spec.Extract)
		if eerr != nil {
			return nil, &PluginError{
				Code:    ErrInternal,
				Message: fmt.Sprintf("snapshot: item[%d]: %s", i, eerr.Error()),
			}
		}
		out = append(out, ent)
	}
	return out, nil
}

func credentialsScope(creds map[string]any) map[string]any {
	if creds == nil {
		return map[string]any{}
	}
	return creds
}

// extractEntity pulls one Entity out of a decoded JSON item per the
// ExtractSpec. Missing entity_id is fatal (no stable key for the
// entity). Missing kind is also fatal (rules predicate on it).
// Missing labels are silently omitted. Missing parent is silently
// treated as "no parent."
func extractEntity(item any, ex ExtractSpec) (Entity, error) {
	itemBytes, err := json.Marshal(item)
	if err != nil {
		return Entity{}, fmt.Errorf("re-marshal item: %w", err)
	}
	entityID, err := jsonPathExtract(itemBytes, ex.EntityID)
	if err != nil {
		return Entity{}, fmt.Errorf("entity_id: %w", err)
	}
	if entityID == "" {
		return Entity{}, fmt.Errorf("entity_id is empty")
	}
	kind, err := resolveKind(itemBytes, entityID, ex.Kind)
	if err != nil {
		return Entity{}, fmt.Errorf("kind: %w", err)
	}
	if kind == "" {
		return Entity{}, fmt.Errorf("kind resolved to empty")
	}
	labels := map[string]string{}
	for name, path := range ex.Labels {
		v, lerr := jsonPathExtract(itemBytes, path)
		if lerr != nil || v == "" {
			continue
		}
		labels[name] = v
	}
	parent := ""
	if ex.Parent != "" {
		if v, perr := jsonPathExtract(itemBytes, ex.Parent); perr == nil {
			parent = v
		}
		// parent path missing → silently no parent; the manifest opted
		// into a path that may not exist on every item.
	}
	return Entity{EntityID: entityID, Kind: kind, Labels: labels, Parent: parent}, nil
}

func resolveKind(itemBytes []byte, entityID string, k ExtractKindSpec) (string, error) {
	switch {
	case k.Literal != "":
		return k.Literal, nil
	case k.PrefixWithEntityDomain != "":
		return k.PrefixWithEntityDomain + entityIDDomain(entityID), nil
	case k.JSONPath != "":
		return jsonPathExtract(itemBytes, k.JSONPath)
	default:
		return "", fmt.Errorf("no kind derivation specified (validation should have caught this)")
	}
}

// entityIDDomain returns the portion of an entity_id before its first
// "." — e.g. "light.kitchen" → "light", "front_door" → "front_door".
// Used by ExtractKindSpec.PrefixWithEntityDomain.
func entityIDDomain(entityID string) string {
	i := strings.Index(entityID, ".")
	if i < 0 {
		return entityID
	}
	return entityID[:i]
}

// jsonPathIterate is JSONPath with one extension over jsonPathExtract:
// a trailing [*] selects an array's elements. Supported shapes:
//
//	$[*]           — response IS an array; return its elements
//	$.field[*]     — array nested under a field
//	$.a.b.c[*]     — array nested deeper
//
// Returns the elements as []any (each is the json.Unmarshal output for
// that element). Callers re-marshal per element for nested JSONPath
// extraction.
func jsonPathIterate(raw []byte, path string) ([]any, error) {
	if !strings.HasSuffix(path, "[*]") {
		return nil, fmt.Errorf("iterate path %q: must end in [*]", path)
	}
	stripped := strings.TrimSuffix(path, "[*]")
	var arr any
	if stripped == "$" {
		// Top-level array.
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("iterate %q: response not JSON: %w", path, err)
		}
	} else {
		// Re-use scalar extractor to descend to the array. jsonPathExtract
		// returns a JSON-marshaled string for arrays — we then re-decode.
		leaf, err := jsonPathExtract(raw, stripped)
		if err != nil {
			return nil, fmt.Errorf("iterate %q: %w", path, err)
		}
		if err := json.Unmarshal([]byte(leaf), &arr); err != nil {
			return nil, fmt.Errorf("iterate %q: leaf not JSON: %w", path, err)
		}
	}
	a, ok := arr.([]any)
	if !ok {
		return nil, fmt.Errorf("iterate %q: leaf is not an array (got %T)", path, arr)
	}
	return a, nil
}

func snippet(raw []byte, max int) string {
	s := strings.TrimSpace(string(raw))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// (Phase 1 of the HA YAML plan stops here. The executor isn't yet
// reachable from handleResourceInvoke — the next commit wires the
// PluginManifest.Source branch into the invocation router so
// declarative manifests reach Invoke instead of the subprocess
// supervisor.)
