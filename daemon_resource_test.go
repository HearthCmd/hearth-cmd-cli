//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAuthzWS is a stub authzWS for tests. Returns a canned
// response on every SendWSRequest. Set CanConnect=false to
// simulate an offline daemon WS.
type fakeAuthzWS struct {
	CanConnect bool
	Response   []byte
	Err        error

	mu    sync.Mutex
	calls []fakeAuthzCall
}

type fakeAuthzCall struct {
	MsgType string
	Data    json.RawMessage
}

func (f *fakeAuthzWS) IsConnected() bool { return f.CanConnect }

func (f *fakeAuthzWS) SendWSRequest(_, msgType string, data json.RawMessage) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeAuthzCall{MsgType: msgType, Data: append(json.RawMessage(nil), data...)})
	f.mu.Unlock()
	return f.Response, f.Err
}

// SendWSRequestTimeout ignores the timeout — the stub responds
// synchronously. Lets handleResourceInvoke exercise the Ask-
// path code without a real WS.
func (f *fakeAuthzWS) SendWSRequestTimeout(_, msgType string, data json.RawMessage, _ time.Duration) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeAuthzCall{MsgType: msgType, Data: append(json.RawMessage(nil), data...)})
	f.mu.Unlock()
	return f.Response, f.Err
}

func allowAuthzWS() *fakeAuthzWS {
	return &fakeAuthzWS{
		CanConnect: true,
		Response:   []byte(`{"type":"authorize_resource_invoke_response","decision":"allow","rule_id":"00000000-0000-0000-0000-000000000001"}`),
	}
}

func denyAuthzWS(reason string) *fakeAuthzWS {
	body, _ := json.Marshal(map[string]interface{}{
		"type":     "authorize_resource_invoke_response",
		"decision": "deny",
		"reason":   reason,
	})
	return &fakeAuthzWS{CanConnect: true, Response: body}
}

// newSupervisedDaemon constructs a *Daemon whose pluginSupervisor is
// wired to the echo plugin built by plugin_process_test.go's
// TestMain. No socket, no real WS, no agent state — just enough
// surface for handleResourceInvoke to round-trip. The authzWS
// defaults to an allow-everything stub so existing happy-path tests
// don't drift; tests that care about the authz outcome pass a
// custom fakeAuthzWS via newSupervisedDaemonWithAuthz.
func newSupervisedDaemon(t *testing.T) *Daemon {
	t.Helper()
	return newSupervisedDaemonWithAuthz(t, allowAuthzWS())
}

func newSupervisedDaemonWithAuthz(t *testing.T, ws authzWS) *Daemon {
	t.Helper()
	s := newTestSupervisor(t, echoTestConns)
	return &Daemon{
		pluginSupervisor: s,
		resourceConnections: s.connections,
		plugins:          s.registry,
		humanUserID:      "test-user",
		resourceAuthzWS:  ws,
	}
}

// ipcRoundTrip hands req to a Daemon and returns the single
// ipcResponse it sends back. Uses net.Pipe so we don't need a real
// unix socket; handleConn writes to one end, we read from the other.
func ipcRoundTrip(t *testing.T, d *Daemon, req ipcRequest) ipcResponse {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		d.handleConn(serverConn)
		close(done)
	}()

	reqBytes, _ := json.Marshal(req)
	reqBytes = append(reqBytes, '\n')
	if _, err := clientConn.Write(reqBytes); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_ = clientConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(clientConn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = clientConn.Close()
	<-done

	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode response: %v (raw=%s)", err, line)
	}
	return resp
}

func TestHandleResourceInvoke_EchoSuccess(t *testing.T) {
	d := newSupervisedDaemon(t)
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "echo",
		ResourceArgs:         json.RawMessage(`{"hi":"there"}`),
	})
	if resp.Type != "resource_invoke_response" {
		t.Fatalf("Type = %q; want resource_invoke_response (Message=%q)", resp.Type, resp.Message)
	}
	if resp.ResourceStdout != `{"hi":"there"}` {
		t.Errorf("Stdout = %q", resp.ResourceStdout)
	}
	if resp.ResourceExitCode != 0 {
		t.Errorf("ExitCode = %d", resp.ResourceExitCode)
	}
	if resp.ResourceErrCode != "" {
		t.Errorf("ErrCode = %q; want empty", resp.ResourceErrCode)
	}
}

func TestHandleResourceInvoke_PluginError(t *testing.T) {
	d := newSupervisedDaemon(t)
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "fail",
	})
	if resp.Type != "error" {
		t.Fatalf("Type = %q; want error", resp.Type)
	}
	if resp.ResourceErrCode != string(ErrInternal) {
		t.Errorf("ErrCode = %q; want %q", resp.ResourceErrCode, ErrInternal)
	}
	if resp.Message == "" {
		t.Error("Message should be set on plugin error")
	}
}

func TestHandleResourceInvoke_TransportError(t *testing.T) {
	d := newSupervisedDaemon(t)
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "exit", // plugin os.Exit(1) mid-call
	})
	if resp.Type != "error" {
		t.Fatalf("Type = %q; want error", resp.Type)
	}
	if resp.ResourceErrCode != string(ErrTransport) {
		t.Errorf("ErrCode = %q; want transport", resp.ResourceErrCode)
	}
}

func TestHandleResourceInvoke_UnknownConnection(t *testing.T) {
	d := newSupervisedDaemon(t)
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "ghost",
		ResourceVerb:         "echo",
	})
	if resp.Type != "error" {
		t.Fatalf("Type = %q; want error", resp.Type)
	}
	if resp.ResourceErrCode != string(ErrBadArgs) {
		t.Errorf("ErrCode = %q; want bad_args", resp.ResourceErrCode)
	}
}

func TestHandleResourceInvoke_MissingFields(t *testing.T) {
	d := newSupervisedDaemon(t)

	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:         "resource_invoke",
		ResourceVerb: "echo",
	})
	if resp.Type != "error" || !strings.Contains(resp.Message, "resource_connection_id") {
		t.Errorf("missing connID: Type=%q Message=%q", resp.Type, resp.Message)
	}

	resp = ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
	})
	if resp.Type != "error" || !strings.Contains(resp.Message, "resource_verb") {
		t.Errorf("missing verb: Type=%q Message=%q", resp.Type, resp.Message)
	}
}

func TestHandleResourceInvoke_AuthzDeny(t *testing.T) {
	setBypassEnv(t, "") // guarantee deny path runs regardless of outer env
	ws := denyAuthzWS("no matching rule")
	d := newSupervisedDaemonWithAuthz(t, ws)

	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "echo",
		ResourceArgs:         json.RawMessage(`{}`),
	})
	if resp.Type != "error" {
		t.Fatalf("Type=%q Message=%q; want error", resp.Type, resp.Message)
	}
	if resp.ResourceErrCode != string(ErrForbidden) {
		t.Errorf("ErrCode = %q; want forbidden", resp.ResourceErrCode)
	}
	if !strings.Contains(resp.Message, "no matching rule") {
		t.Errorf("Message %q should include deny reason", resp.Message)
	}
	if resp.ResourceStdout != "" {
		t.Errorf("plugin must not have been invoked; got Stdout=%q", resp.ResourceStdout)
	}
	if len(ws.calls) != 1 {
		t.Errorf("expected exactly 1 authz call, got %d", len(ws.calls))
	}
}

func TestHandleResourceInvoke_AuthzWSOffline(t *testing.T) {
	setBypassEnv(t, "")
	d := newSupervisedDaemonWithAuthz(t, &fakeAuthzWS{CanConnect: false})
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "echo",
	})
	if resp.Type != "error" || resp.ResourceErrCode != string(ErrUnavailable) {
		t.Errorf("Type=%q ErrCode=%q; want error/unavailable", resp.Type, resp.ResourceErrCode)
	}
}

func TestHandleResourceInvoke_AuthzServerError(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"type": "error", "error": "db is sad"})
	d := newSupervisedDaemonWithAuthz(t, &fakeAuthzWS{CanConnect: true, Response: body})
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "echo",
	})
	if resp.Type != "error" {
		t.Fatalf("Type=%q; want error", resp.Type)
	}
	if !strings.Contains(resp.Message, "db is sad") {
		t.Errorf("Message should surface server error: %q", resp.Message)
	}
}

// TestHandleResourceInvoke_AuthzCallShape pins the wire payload the
// daemon sends. The server-side handler parses these fields by name;
// breaking the contract silently here would cause every plugin
// invoke to deny in prod.
func TestHandleResourceInvoke_AuthzCallShape(t *testing.T) {
	ws := allowAuthzWS()
	d := newSupervisedDaemonWithAuthz(t, ws)
	d.humanUserID = "operator-7"

	_ = ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "echo",
	})

	if len(ws.calls) != 1 {
		t.Fatalf("expected 1 authz call, got %d", len(ws.calls))
	}
	if ws.calls[0].MsgType != "authorize_resource_invoke" {
		t.Errorf("MsgType = %q", ws.calls[0].MsgType)
	}
	var sent authorizeResourceInvokeReq
	if err := json.Unmarshal(ws.calls[0].Data, &sent); err != nil {
		t.Fatalf("decode sent payload: %v", err)
	}
	if sent.PrincipalID != "operator-7" || sent.PrincipalKind != "human" {
		t.Errorf("principal = %+v", sent)
	}
	if sent.ConnectionID != "echo-test" || sent.PluginSlug != "echo" || sent.Verb != "echo" {
		t.Errorf("payload = %+v", sent)
	}
	if sent.RequestID == "" {
		t.Errorf("request_id should be set (uuid)")
	}
}

// TestHandleResourceInvoke_AuthzCallCarriesArgs asserts the 1h-commit-5
// plumbing: ResourceArgs from the IPC frame round-trips into the
// outbound authorize_resource_invoke payload's Args field. The
// server's Ask path folds this into the Subject so the phone
// renderer can show the human what they're approving.
func TestHandleResourceInvoke_AuthzCallCarriesArgs(t *testing.T) {
	ws := allowAuthzWS()
	d := newSupervisedDaemonWithAuthz(t, ws)
	d.humanUserID = "operator-7"

	argsJSON := json.RawMessage(`{"q":"who am i"}`)
	_ = ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "echo",
		ResourceArgs:         argsJSON,
	})

	if len(ws.calls) != 1 {
		t.Fatalf("expected 1 authz call, got %d", len(ws.calls))
	}
	var sent authorizeResourceInvokeReq
	if err := json.Unmarshal(ws.calls[0].Data, &sent); err != nil {
		t.Fatalf("decode sent payload: %v", err)
	}
	if string(sent.Args) != string(argsJSON) {
		t.Errorf("Args = %s; want %s", sent.Args, argsJSON)
	}
}

// setBypassEnv installs HEARTH_RESOURCE_AUTHZ_BYPASS for the
// duration of a test, restoring the prior value on cleanup. The
// bypass affects package-global env state, so tests that depend on
// it are not run in parallel.
func setBypassEnv(t *testing.T, value string) {
	t.Helper()
	prev, hadPrev := os.LookupEnv("HEARTH_RESOURCE_AUTHZ_BYPASS")
	if value == "" {
		os.Unsetenv("HEARTH_RESOURCE_AUTHZ_BYPASS")
	} else {
		os.Setenv("HEARTH_RESOURCE_AUTHZ_BYPASS", value)
	}
	t.Cleanup(func() {
		if hadPrev {
			os.Setenv("HEARTH_RESOURCE_AUTHZ_BYPASS", prev)
		} else {
			os.Unsetenv("HEARTH_RESOURCE_AUTHZ_BYPASS")
		}
	})
}

func TestResourceAuthzBypass_ParsesTruthy(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"0", false},
		{"false", false},
		{"no", false},
	}
	for _, tt := range tests {
		setBypassEnv(t, tt.val)
		if got := resourceAuthzBypass(); got != tt.want {
			t.Errorf("HEARTH_RESOURCE_AUTHZ_BYPASS=%q got %v; want %v", tt.val, got, tt.want)
		}
	}
}

// TestHandleResourceInvoke_BypassSkipsAuthz verifies the bypass
// env path doesn't hit the WS at all — even if the configured
// authzWS would deny, the invoke proceeds straight to the plugin.
func TestHandleResourceInvoke_BypassSkipsAuthz(t *testing.T) {
	setBypassEnv(t, "1")

	// A deny stub: if the bypass is honored, it must never be called.
	ws := denyAuthzWS("would have been denied")
	d := newSupervisedDaemonWithAuthz(t, ws)

	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "echo",
		ResourceArgs:         json.RawMessage(`{"hi":"there"}`),
	})
	if resp.Type != "resource_invoke_response" {
		t.Fatalf("Type=%q Message=%q; want resource_invoke_response", resp.Type, resp.Message)
	}
	if resp.ResourceStdout != `{"hi":"there"}` {
		t.Errorf("Stdout = %q", resp.ResourceStdout)
	}
	if len(ws.calls) != 0 {
		t.Errorf("bypass should have skipped the WS; got %d calls", len(ws.calls))
	}
}

// TestHandleResourceInvoke_BypassWithOfflineWS confirms the bypass
// also short-circuits the IsConnected check. This is the
// dogfooding-without-server use case: nil/offline WS would normally
// return ErrUnavailable, but with bypass on the invoke proceeds.
func TestHandleResourceInvoke_BypassWithOfflineWS(t *testing.T) {
	setBypassEnv(t, "1")
	d := newSupervisedDaemonWithAuthz(t, &fakeAuthzWS{CanConnect: false})

	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "echo",
	})
	if resp.Type != "resource_invoke_response" {
		t.Fatalf("Type=%q; want resource_invoke_response", resp.Type)
	}
}

func TestHandleResourceInvoke_AgentPrincipalForwarded(t *testing.T) {
	ws := allowAuthzWS()
	d := newSupervisedDaemonWithAuthz(t, ws)
	d.humanUserID = "operator-7"

	_ = ipcRoundTrip(t, d, ipcRequest{
		Type:                  "resource_invoke",
		ResourceConnectionID:  "echo-test",
		ResourceVerb:          "echo",
		ResourcePrincipalKind: "agent",
		ResourcePrincipalID:   "agt-42",
	})
	if len(ws.calls) != 1 {
		t.Fatalf("expected 1 authz call, got %d", len(ws.calls))
	}
	var sent authorizeResourceInvokeReq
	if err := json.Unmarshal(ws.calls[0].Data, &sent); err != nil {
		t.Fatalf("decode sent: %v", err)
	}
	if sent.PrincipalKind != "agent" || sent.PrincipalID != "agt-42" {
		t.Errorf("principal = (%q, %q); want (agent, agt-42)", sent.PrincipalKind, sent.PrincipalID)
	}
}

func TestHandleResourceInvoke_DefaultsToHumanPrincipal(t *testing.T) {
	ws := allowAuthzWS()
	d := newSupervisedDaemonWithAuthz(t, ws)
	d.humanUserID = "operator-9"

	_ = ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "echo",
		// no principal fields → defaults
	})
	var sent authorizeResourceInvokeReq
	if err := json.Unmarshal(ws.calls[0].Data, &sent); err != nil {
		t.Fatalf("decode sent: %v", err)
	}
	if sent.PrincipalKind != "human" || sent.PrincipalID != "operator-9" {
		t.Errorf("default principal = (%q, %q); want (human, operator-9)", sent.PrincipalKind, sent.PrincipalID)
	}
}

// TestCLI_RunResourceInvoke_PicksUpAgentEnvVar removed: it tested the
// CLI's HEARTH_AGENT_INSTANCE_ID → ResourcePrincipalKind pickup, which
// was deleted in Phase 2 of docs/agent-identity-plan.md (7cad03d).
// The daemon now derives the calling agent's identity from the IPC
// socket's peer credentials; an env-var claim was forgeable and is
// no longer authoritative.

func TestResourceInvokeTimeout(t *testing.T) {
	tests := []struct {
		env  string
		want time.Duration
	}{
		{"", 30 * time.Second},
		{"5s", 5 * time.Second},
		{"2m", 2 * time.Minute},
		{"500ms", 500 * time.Millisecond},
		{"garbage", 30 * time.Second}, // falls back, logs
		{"0s", 30 * time.Second},      // non-positive falls back
		{"-1s", 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			prev, hadPrev := os.LookupEnv("HEARTH_RESOURCE_INVOKE_TIMEOUT")
			if tt.env == "" {
				os.Unsetenv("HEARTH_RESOURCE_INVOKE_TIMEOUT")
			} else {
				os.Setenv("HEARTH_RESOURCE_INVOKE_TIMEOUT", tt.env)
			}
			t.Cleanup(func() {
				if hadPrev {
					os.Setenv("HEARTH_RESOURCE_INVOKE_TIMEOUT", prev)
				} else {
					os.Unsetenv("HEARTH_RESOURCE_INVOKE_TIMEOUT")
				}
			})
			if got := resourceInvokeTimeout(); got != tt.want {
				t.Errorf("env=%q got %s; want %s", tt.env, got, tt.want)
			}
		})
	}
}

func TestHandleResourceInvoke_NoSupervisor(t *testing.T) {
	d := &Daemon{} // bare daemon, no supervisor wired
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "echo-test",
		ResourceVerb:         "echo",
	})
	if resp.Type != "error" || !strings.Contains(resp.Message, "supervisor") {
		t.Errorf("Type=%q Message=%q; want error mentioning supervisor", resp.Type, resp.Message)
	}
}
