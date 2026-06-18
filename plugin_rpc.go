package main

import (
	"encoding/json"
)

// Plugin JSON-RPC wire format.
//
// Communication between the daemon and a plugin subprocess is line-
// delimited JSON: one frame per line, requests on the plugin's stdin,
// responses on its stdout. Stderr is reserved for human-readable log
// output (forwarded to the daemon log with a connection-id prefix).
//
// The wire types here mirror the contract in
// hearth-cmd/docs/external-resource-adapters.md §"Daemon ↔ plugin
// RPC". They're internal-package types — exported only where the
// daemon's caller side needs to construct a Params payload.

type rpcRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`           // "Init" | "Invoke" | "Shutdown"
	Params json.RawMessage `json:"params,omitempty"` // method-specific shape
}

type rpcResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// InitParams is the payload the daemon sends to a plugin's Init
// method, once per process lifetime, before any Invoke. Credentials
// are passed in cleartext here — they were ciphertext at rest in the
// vault and on the wire to the daemon, but the daemon decrypts before
// handing them to the plugin so the plugin author doesn't have to
// reimplement vault crypto.
//
// Snapshot is opaque in 1b — the entity-snapshot shape is set by the
// onboarding sub-phase and the supervisor just threads bytes through.
type InitParams struct {
	ConnectionID string          `json:"connection_id"`
	Snapshot     json.RawMessage `json:"snapshot,omitempty"`
	// Credentials was the old per-Init credentials map. Retired with
	// the secrets-as-IAM-gated-labeled-blobs redesign — plugins no
	// longer receive secrets at Init. Per-invoke secret bindings land
	// on InvokeParams in a follow-up (see
	// project_secrets_consumption_pipeline_todo memory).
}

// InvokeParams is the payload the daemon sends per verb call. Args is
// a raw JSON object so the daemon doesn't have to know each verb's
// argument schema — the plugin parses against its own manifest.
//
// SecretBindings is the per-invoke cleartext credential map. The
// daemon resolves the agent's `--secret HA_TOKEN=sec-abc` bindings:
// fetches each secret via the server's authorized secrets_get,
// decrypts locally, and hands cleartexts to the plugin keyed by the
// requested env-var name. Plugin authors read these as if they were
// env vars (e.g. `params.SecretBindings["HA_TOKEN"]`). The daemon
// scrubs the values out of the plugin's response before relaying.
//
// Cleartext lives in: daemon RAM (briefly), plugin process RAM
// (during the call). Never crosses back into the agent transcript
// (scrubber + the agent never had it in the first place).
type InvokeParams struct {
	Verb           string            `json:"verb"`
	Args           json.RawMessage   `json:"args,omitempty"`
	SecretBindings map[string]string `json:"secret_bindings,omitempty"`
}

// InvokeResult is what the plugin returns on success. Stdout is the
// agent-facing string forwarded verbatim through the future `hearth
// resource` subcommand's stdout. ExitCode is a forward-compat field
// matching shell-tool semantics — 0 by convention on success; plugin
// authors may set non-zero to flag a partial / soft failure that
// shouldn't be modeled as a structured error.
type InvokeResult struct {
	Stdout   string `json:"stdout"`
	ExitCode int    `json:"exit_code"`
}

// ErrorCode is the small fixed vocabulary the daemon translates
// plugin-reported errors into. Per
// hearth-cmd/docs/external-resource-adapters.md §"Error vocabulary".
// Unknown codes coming off the wire are accepted opaque and logged
// with a warning — the daemon doesn't refuse the response just
// because a plugin used a code we haven't seen yet (third-party
// authors will iterate; refusing would block legitimate use cases).
type ErrorCode string

const (
	ErrBadArgs      ErrorCode = "bad_args"
	ErrUnauthorized ErrorCode = "unauthorized"
	ErrUnavailable  ErrorCode = "unavailable"
	ErrForbidden    ErrorCode = "forbidden"
	ErrInternal     ErrorCode = "internal"

	// ErrTransport is daemon-side only — never on the wire. Surfaces
	// when the plugin process dies mid-call, a response line is
	// malformed, the caller's context fires, or response.id doesn't
	// match request.id. The accompanying *PluginError signals the
	// process is no longer usable; the supervisor will respawn on
	// next Invoke.
	ErrTransport ErrorCode = "transport"
)

// validWireErrorCodes is the set of codes a plugin may legitimately
// emit. ErrTransport is excluded — daemon-side only.
var validWireErrorCodes = map[ErrorCode]struct{}{
	ErrBadArgs:      {},
	ErrUnauthorized: {},
	ErrUnavailable:  {},
	ErrForbidden:    {},
	ErrInternal:     {},
}

// isKnownWireErrorCode reports whether code is in the wire vocabulary.
// Used by the response decoder to decide whether to log a warning
// about an unfamiliar code — the code itself is always preserved in
// the returned *PluginError.
func isKnownWireErrorCode(code ErrorCode) bool {
	_, ok := validWireErrorCodes[code]
	return ok
}

// PluginError is what the supervisor surfaces to Go callers. Wraps
// both plugin-reported errors (matching the wire vocabulary) and
// daemon-side transport errors (ErrTransport). Callers that care
// can switch on Code; most callers just propagate.
type PluginError struct {
	Code    ErrorCode
	Message string
}

func (e *PluginError) Error() string {
	return string(e.Code) + ": " + e.Message
}
