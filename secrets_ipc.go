//go:build darwin || linux

package main

import (
	"encoding/base64"
	"encoding/json"
	"net"
)

// IPC handlers for the `hearth secret` CLI subcommand. The CLI hands
// cleartext to the daemon over the local unix socket; the daemon
// encrypts to its own pubkey, then relays ciphertext to the server
// via secrets_put. Same-host cleartext crosses one process boundary
// (CLI → daemon, same user) — the off-host surface only ever sees
// the envelope.
//
// Semantics changed when the server reshaped the secrets table to a
// labeled, IAM-gated blob: no more scope_kind/scope_value/key_name
// columns, no manifest-credential-name validation. The daemon takes
// a freeform `name` and optional `purpose` and forwards verbatim.

func (d *Daemon) handleSecretList(conn net.Conn, req ipcRequest) {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		sendControl(conn, ipcResponse{Type: "error", Message: "daemon offline from server"})
		return
	}
	// Always list this host's secrets — the operator is at THIS host's
	// CLI. Cross-host listing is a webview/mobile concern.
	payload, _ := json.Marshal(map[string]string{"host_id": d.hostID})
	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "secrets_list", payload)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "ws_request: " + err.Error()})
		return
	}
	sendControl(conn, ipcResponse{Type: "secret_list_response", Data: json.RawMessage(raw)})
}

func (d *Daemon) handleSecretSet(conn net.Conn, req ipcRequest) {
	if d.secretsPrivKey == nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "secrets keypair not loaded"})
		return
	}
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		sendControl(conn, ipcResponse{Type: "error", Message: "daemon offline from server"})
		return
	}
	if req.SecretName == "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "secret set requires name"})
		return
	}
	envelope, err := encryptSecretEnvelope(d.secretsPrivKey.PublicKey(), []byte(req.SecretValue))
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "encrypt: " + err.Error()})
		return
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"name":       req.SecretName,
		"purpose":    req.SecretPurpose,
		"host_id":    d.hostID,
		"ciphertext": base64.StdEncoding.EncodeToString(envelope),
	})
	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "secrets_put", payload)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "ws_request: " + err.Error()})
		return
	}
	var resp struct {
		Type  string `json:"type"`
		Error string `json:"error"`
		ID    string `json:"id"`
	}
	_ = json.Unmarshal(raw, &resp)
	if resp.Type == "error" {
		sendControl(conn, ipcResponse{Type: "error", Message: "server: " + resp.Error})
		return
	}
	sendControl(conn, ipcResponse{Type: "secret_set_response",
		Data: mustJSON(map[string]string{"id": resp.ID, "name": req.SecretName})})
}

func (d *Daemon) handleSecretDelete(conn net.Conn, req ipcRequest) {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		sendControl(conn, ipcResponse{Type: "error", Message: "daemon offline from server"})
		return
	}
	if req.SecretID == "" {
		sendControl(conn, ipcResponse{Type: "error",
			Message: "secret delete requires id (see `hearth secret list`)"})
		return
	}
	payload, _ := json.Marshal(map[string]string{"id": req.SecretID})
	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "secrets_delete", payload)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "ws_request: " + err.Error()})
		return
	}
	var resp struct {
		Type    string `json:"type"`
		Error   string `json:"error"`
		Deleted int    `json:"deleted"`
	}
	_ = json.Unmarshal(raw, &resp)
	if resp.Type == "error" {
		sendControl(conn, ipcResponse{Type: "error", Message: "server: " + resp.Error})
		return
	}
	sendControl(conn, ipcResponse{Type: "secret_delete_response",
		Data: mustJSON(map[string]int{"deleted": resp.Deleted})})
}

func (d *Daemon) handleSecretGrant(conn net.Conn, req ipcRequest) {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		sendControl(conn, ipcResponse{Type: "error", Message: "daemon offline from server"})
		return
	}
	if req.SecretID == "" || req.SecretGrantPrincipalID == "" {
		sendControl(conn, ipcResponse{Type: "error",
			Message: "secret grant requires secret_id and principal_id"})
		return
	}
	kind := req.SecretGrantPrincipalKind
	if kind == "" {
		kind = "human"
	}
	payload, _ := json.Marshal(map[string]string{
		"secret_id":      req.SecretID,
		"principal_kind": kind,
		"principal_id":   req.SecretGrantPrincipalID,
	})
	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "secret_grant", payload)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "ws_request: " + err.Error()})
		return
	}
	var resp struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &resp)
	if resp.Type == "error" {
		sendControl(conn, ipcResponse{Type: "error", Message: "server: " + resp.Error})
		return
	}
	sendControl(conn, ipcResponse{Type: "secret_grant_response", Data: json.RawMessage(raw)})
}

func (d *Daemon) handleSecretRevoke(conn net.Conn, req ipcRequest) {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		sendControl(conn, ipcResponse{Type: "error", Message: "daemon offline from server"})
		return
	}
	if req.SecretID == "" || req.SecretGrantPrincipalID == "" {
		sendControl(conn, ipcResponse{Type: "error",
			Message: "secret revoke requires secret_id and principal_id"})
		return
	}
	kind := req.SecretGrantPrincipalKind
	if kind == "" {
		kind = "human"
	}
	payload, _ := json.Marshal(map[string]string{
		"secret_id":      req.SecretID,
		"principal_kind": kind,
		"principal_id":   req.SecretGrantPrincipalID,
	})
	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "secret_revoke", payload)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "ws_request: " + err.Error()})
		return
	}
	var resp struct {
		Type    string `json:"type"`
		Error   string `json:"error"`
		Revoked int    `json:"revoked"`
	}
	_ = json.Unmarshal(raw, &resp)
	if resp.Type == "error" {
		sendControl(conn, ipcResponse{Type: "error", Message: "server: " + resp.Error})
		return
	}
	sendControl(conn, ipcResponse{Type: "secret_revoke_response",
		Data: mustJSON(map[string]int{"revoked": resp.Revoked})})
}

func mustJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
