//go:build darwin || linux

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
)

// resolveSecretBindings is the daemon-side resolver behind the
// `hearth resource invoke --secret HA_TOKEN=sec-abc` flow.
//
// Input: env-name → secret-id map from the IPC frame, plus the
// calling principal. For each id:
//  1. Server's secrets_get authorizes via Authorize(principal, secret:<id>,
//     'secret.use'). On ask the server fans out a permission_request
//     and the WS call blocks until the human responds.
//  2. Server returns ciphertext.
//  3. Daemon decrypts locally with d.secretsPrivKey (host-pinning rail —
//     the row's host_id must match this daemon's host, which the server
//     enforces).
//
// Returns env-name → cleartext map. On any error returns the partial
// map already resolved (so the caller can wipe what it got) plus a
// *PluginError suitable for surfacing to the agent.
func (d *Daemon) resolveSecretBindings(
	bindings map[string]string,
	principalKind, principalID string,
) (map[string]string, *PluginError) {
	if len(bindings) == 0 {
		return nil, nil
	}
	if d.secretsPrivKey == nil {
		return nil, &PluginError{Code: ErrInternal, Message: "secrets keypair not loaded"}
	}
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		return nil, &PluginError{Code: ErrTransport, Message: "daemon offline from server"}
	}
	out := make(map[string]string, len(bindings))
	for envName, secretID := range bindings {
		if secretID == "" {
			return out, &PluginError{Code: ErrBadArgs,
				Message: fmt.Sprintf("empty secret id for env %q", envName)}
		}
		cleartext, perr := d.fetchAndDecryptSecret(secretID, principalKind, principalID)
		if perr != nil {
			return out, perr
		}
		out[envName] = cleartext
	}
	return out, nil
}

func (d *Daemon) fetchAndDecryptSecret(secretID, principalKind, principalID string) (string, *PluginError) {
	requestID := generateUUID()
	payload, _ := json.Marshal(map[string]string{
		"id":             secretID,
		"principal_kind": principalKind,
		"principal_id":   principalID,
		"request_id":     requestID,
	})
	raw, err := d.daemonWS.SendWSRequest(requestID, "secrets_get", payload)
	if err != nil {
		return "", &PluginError{Code: ErrTransport, Message: "ws_request: " + err.Error()}
	}
	var resp struct {
		Type       string `json:"type"`
		Error      string `json:"error"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", &PluginError{Code: ErrInternal, Message: "decode secrets_get: " + err.Error()}
	}
	if resp.Type == "error" {
		// Map common server-side errors to plugin error codes the
		// caller can act on.
		code := ErrInternal
		switch {
		case resp.Error == "secret use denied by IAM rule",
			resp.Error == "secret use denied by human":
			code = ErrForbidden
		case resp.Error == "secret not found",
			resp.Error == "secret is pinned to a different host":
			code = ErrBadArgs
		}
		return "", &PluginError{Code: code, Message: "secret " + secretID + ": " + resp.Error}
	}
	envelope, err := base64.StdEncoding.DecodeString(resp.Ciphertext)
	if err != nil {
		return "", &PluginError{Code: ErrInternal, Message: "decode ciphertext: " + err.Error()}
	}
	cleartext, err := decryptSecretEnvelope(d.secretsPrivKey, envelope)
	if err != nil {
		return "", &PluginError{Code: ErrInternal, Message: "decrypt: " + err.Error()}
	}
	return string(cleartext), nil
}

// handleSecretResolve is the daemon-side endpoint behind the
// `hearth run` wrapper. Takes a {env_name → secret_id} map and the
// calling principal, returns {env_name → cleartext}. Same IAM-gated
// resolveSecretBindings path the plugin-invoke flow uses.
//
// Wire: response carries cleartext on the unix socket. Same threat
// model as the existing `hearth secret set` path — local socket,
// same-user-only, file-permission gated.
func (d *Daemon) handleSecretResolve(conn net.Conn, req ipcRequest) {
	if len(req.SecretBindings) == 0 {
		sendControl(conn, ipcResponse{Type: "error", Message: "no bindings to resolve"})
		return
	}
	principalKind, principalID, identityErr := d.derivePrincipal(conn,
		req.ResourcePrincipalKind, req.ResourcePrincipalID, "secret_resolve")
	if identityErr != nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         identityErr.Message,
			ResourceErrCode: string(identityErr.Code),
		})
		return
	}
	cleartexts, perr := d.resolveSecretBindings(req.SecretBindings, principalKind, principalID)
	if perr != nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         perr.Message,
			ResourceErrCode: string(perr.Code),
		})
		return
	}
	sendControl(conn, ipcResponse{
		Type:             "secret_resolve_response",
		SecretCleartexts: cleartexts,
	})
}

// zeroSecretMap overwrites every cleartext byte in the map. Best-effort
// hygiene — Go strings are immutable so the underlying memory may
// still be in a string interning pool, but at least the map values get
// ExchangeOAuthToken implements OAuthTokenExchanger. The daemon sends
// the plaintext refresh token to the server over the authenticated
// daemon WebSocket; the server holds the OAuth client_secret and calls
// the upstream provider (Google), then returns a short-lived access
// token. The refresh token is transmitted in plaintext only over this
// TLS-encrypted, authenticated channel — it never touches disk.
func (d *Daemon) ExchangeOAuthToken(_ context.Context, provider string, refreshToken []byte) (string, int, error) {
	payload, err := json.Marshal(map[string]string{
		"provider":      provider,
		"refresh_token": string(refreshToken),
	})
	if err != nil {
		return "", 0, fmt.Errorf("exchange_oauth_token marshal: %w", err)
	}
	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "exchange_oauth_token", payload)
	if err != nil {
		return "", 0, fmt.Errorf("exchange_oauth_token ws: %w", err)
	}
	var resp struct {
		Type        string `json:"type"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", 0, fmt.Errorf("exchange_oauth_token decode: %w", err)
	}
	if resp.Error != "" {
		return "", 0, fmt.Errorf("exchange_oauth_token: %s", resp.Error)
	}
	return resp.AccessToken, resp.ExpiresIn, nil
}

// cleared. Call after the plugin Invoke returns.
func zeroSecretMap(m map[string]string) {
	for k := range m {
		// Allocate a fresh empty string; the GC reclaims the prior
		// backing bytes when no other reference exists. This is what
		// you can do at the Go-string level; for true zeroing you'd
		// need a []byte all the way through.
		m[k] = ""
	}
}
