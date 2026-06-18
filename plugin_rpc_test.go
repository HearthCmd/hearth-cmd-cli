package main

import (
	"encoding/json"
	"testing"
)

func TestRPCRequest_RoundTrip_Init(t *testing.T) {
	params, err := json.Marshal(InitParams{
		ConnectionID: "ha-home",
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := rpcRequest{ID: "1", Method: "Init", Params: params}

	wire, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var got rpcRequest
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "1" || got.Method != "Init" {
		t.Errorf("envelope wrong: %+v", got)
	}

	var p InitParams
	if err := json.Unmarshal(got.Params, &p); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if p.ConnectionID != "ha-home" {
		t.Errorf("connection_id = %q", p.ConnectionID)
	}
}

func TestRPCRequest_RoundTrip_Invoke(t *testing.T) {
	params, _ := json.Marshal(InvokeParams{
		Verb: "turn_on",
		Args: json.RawMessage(`{"entity_id":"light.kitchen"}`),
	})
	req := rpcRequest{ID: "42", Method: "Invoke", Params: params}

	wire, _ := json.Marshal(req)
	var got rpcRequest
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Method != "Invoke" {
		t.Errorf("method = %q", got.Method)
	}
	var p InvokeParams
	if err := json.Unmarshal(got.Params, &p); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if p.Verb != "turn_on" {
		t.Errorf("verb = %q", p.Verb)
	}
	if string(p.Args) != `{"entity_id":"light.kitchen"}` {
		t.Errorf("args = %s", string(p.Args))
	}
}

func TestRPCRequest_Shutdown_NoParams(t *testing.T) {
	// Shutdown has no params block. Marshal should omit it.
	req := rpcRequest{ID: "99", Method: "Shutdown"}
	wire, _ := json.Marshal(req)
	if got := string(wire); got != `{"id":"99","method":"Shutdown"}` {
		t.Errorf("shutdown wire = %s", got)
	}
}

func TestRPCResponse_SuccessRoundTrip(t *testing.T) {
	result, _ := json.Marshal(InvokeResult{
		Stdout:   `{"status":"ok"}`,
		ExitCode: 0,
	})
	resp := rpcResponse{ID: "3", Result: result}
	wire, _ := json.Marshal(resp)

	var got rpcResponse
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Error != nil {
		t.Errorf("Error should be nil for success: %+v", got.Error)
	}
	var r InvokeResult
	if err := json.Unmarshal(got.Result, &r); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if r.Stdout != `{"status":"ok"}` || r.ExitCode != 0 {
		t.Errorf("result wrong: %+v", r)
	}
}

func TestRPCResponse_ErrorRoundTrip(t *testing.T) {
	wire := []byte(`{"id":"7","error":{"code":"unauthorized","message":"token rejected"}}`)
	var got rpcResponse
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Error == nil {
		t.Fatal("Error nil")
	}
	if got.Error.Code != "unauthorized" {
		t.Errorf("Code = %q", got.Error.Code)
	}
	if got.Error.Message != "token rejected" {
		t.Errorf("Message = %q", got.Error.Message)
	}
	if got.Result != nil {
		t.Errorf("Result should be nil on error path, got %s", string(got.Result))
	}
}

func TestErrorCodeVocabulary(t *testing.T) {
	known := []ErrorCode{
		ErrBadArgs, ErrUnauthorized, ErrUnavailable, ErrForbidden, ErrInternal,
	}
	for _, c := range known {
		if !isKnownWireErrorCode(c) {
			t.Errorf("%s should be a known wire code", c)
		}
	}
	// ErrTransport is daemon-side only — must NOT be in the wire set.
	if isKnownWireErrorCode(ErrTransport) {
		t.Error("ErrTransport must not be in the wire vocabulary")
	}
	if isKnownWireErrorCode(ErrorCode("weird_new_code")) {
		t.Error("unknown codes must not be reported as known")
	}
}

func TestPluginError_ErrorString(t *testing.T) {
	e := &PluginError{Code: ErrUnauthorized, Message: "token rejected"}
	if got := e.Error(); got != "unauthorized: token rejected" {
		t.Errorf("Error() = %q", got)
	}
}
