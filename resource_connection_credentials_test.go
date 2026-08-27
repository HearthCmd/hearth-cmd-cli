//go:build darwin || linux

package main

import (
	"encoding/json"
	"errors"
	"testing"
)

// The refresh path autofills a connection's bound credential by asking the
// server for its {name: secret_id} map — so a refresh of an autofilled
// connection no longer demands --secret. These cover fetchConnectionCredentials
// in isolation; the wiring into handleResourceRefresh is exercised by the
// existing refresh tests (which use a canned authz stub and see nil autofill).

func TestFetchConnectionCredentials_ReturnsAutofillMap(t *testing.T) {
	ws := &fakeAuthzWS{
		CanConnect: true,
		Response: []byte(`{"type":"resource_connection_credentials_response",` +
			`"credentials":{"ha_token":"sec-1"}}`),
	}
	d := &Daemon{}
	creds, pe := d.fetchConnectionCredentials(ws, "agent", "agent-1", "conn-1")
	if pe != nil {
		t.Fatalf("unexpected error: %v", pe)
	}
	if creds["ha_token"] != "sec-1" {
		t.Fatalf("credentials = %v, want {ha_token: sec-1}", creds)
	}
	// The daemon must address the right endpoint and carry the principal + connection.
	if len(ws.calls) != 1 || ws.calls[0].MsgType != "resource_connection_credentials" {
		t.Fatalf("calls = %+v, want one resource_connection_credentials", ws.calls)
	}
	var sent map[string]string
	if err := json.Unmarshal(ws.calls[0].Data, &sent); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if sent["connection_id"] != "conn-1" || sent["principal_id"] != "agent-1" || sent["principal_kind"] != "agent" {
		t.Fatalf("payload = %v", sent)
	}
}

// A server that predates the endpoint (or otherwise errors) is soft-handled:
// no autofill, no hard failure — the caller falls back to explicit --secret.
func TestFetchConnectionCredentials_ErrorResponseIsSoft(t *testing.T) {
	ws := &fakeAuthzWS{
		CanConnect: true,
		Response:   []byte(`{"type":"error","error":"unknown msg_type: resource_connection_credentials"}`),
	}
	d := &Daemon{}
	creds, pe := d.fetchConnectionCredentials(ws, "human", "user-1", "conn-1")
	if pe != nil {
		t.Fatalf("error response should be soft, got %v", pe)
	}
	if creds != nil {
		t.Fatalf("credentials = %v, want nil", creds)
	}
}

// An unbound connection returns a nil map (normal) — the daemon then relies on
// whatever --secret the caller passed.
func TestFetchConnectionCredentials_UnboundIsNil(t *testing.T) {
	ws := &fakeAuthzWS{
		CanConnect: true,
		Response:   []byte(`{"type":"resource_connection_credentials_response","credentials":null}`),
	}
	d := &Daemon{}
	creds, pe := d.fetchConnectionCredentials(ws, "human", "user-1", "conn-1")
	if pe != nil || creds != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", creds, pe)
	}
}

// A disconnected daemon can't autofill — surfaced as ErrUnavailable so the
// refresh handler can log it and fall through.
func TestFetchConnectionCredentials_OfflineUnavailable(t *testing.T) {
	ws := &fakeAuthzWS{CanConnect: false}
	d := &Daemon{}
	_, pe := d.fetchConnectionCredentials(ws, "human", "user-1", "conn-1")
	if pe == nil || pe.Code != ErrUnavailable {
		t.Fatalf("want ErrUnavailable, got %v", pe)
	}
}

// A transport failure surfaces as a PluginError (best-effort caller logs it).
func TestFetchConnectionCredentials_TransportError(t *testing.T) {
	ws := &fakeAuthzWS{CanConnect: true, Err: errors.New("ws down")}
	d := &Daemon{}
	_, pe := d.fetchConnectionCredentials(ws, "human", "user-1", "conn-1")
	if pe == nil || pe.Code != ErrUnavailable {
		t.Fatalf("want ErrUnavailable, got %v", pe)
	}
}
