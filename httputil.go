//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// readAll is a thin wrapper so we don't import io everywhere; also drops
// errors deliberately (non-200 responses that fail to read are reported
// via the status code alone).
func readAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

// serverBaseURL derives the HTTPS base URL from the build-time wsURL.
// e.g. "wss://hearthcmd.com/ws/relay" → "https://hearthcmd.com"
func serverBaseURL() (string, error) {
	if wsURL == "" {
		return "", fmt.Errorf("no relay server URL configured")
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("bad relay URL: %w", err)
	}
	scheme := "https"
	if u.Scheme == "ws" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s", scheme, u.Host), nil
}

// requestAuthCode asks the server to email a one-time code to the given
// address. The server returns 200 regardless of whether the email exists
// or is rate-limited (intentional — see /auth/request), so any HTTP-level
// success here just means "code sent, check your inbox."
func requestAuthCode(baseURL, email string) error {
	body, err := json.Marshal(map[string]string{"email": email})
	if err != nil {
		return fmt.Errorf("failed to encode request: %w", err)
	}
	req, err := http.NewRequest("POST", baseURL+"/auth/request", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	addClientHeader(req)
	addActionHeader(req, ActionTuple{Kind: "human_user", Action: "request_otp"})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("auth request failed: %w", err)
	}
	defer resp.Body.Close()
	checkOutdated(resp)
	if resp.StatusCode != 200 {
		return fmt.Errorf("auth request failed (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// verifyAuthCode exchanges (email, 6-digit code) for a short-lived
// enrollment session token. purpose selects what the session is allowed
// to do downstream — "enroll_host" for the CLI, "enroll_device" for
// mobile clients.
func verifyAuthCode(baseURL, email, code, purpose string) (sessionToken string, isNewUser bool, err error) {
	body, err := json.Marshal(map[string]string{
		"email":   email,
		"code":    code,
		"purpose": purpose,
	})
	if err != nil {
		return "", false, fmt.Errorf("failed to encode request: %w", err)
	}
	req, err := http.NewRequest("POST", baseURL+"/auth/verify", bytes.NewReader(body))
	if err != nil {
		return "", false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	addClientHeader(req)
	addActionHeader(req, ActionTuple{Kind: "human_user", Action: "verify_otp"})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("verify request failed: %w", err)
	}
	defer resp.Body.Close()
	checkOutdated(resp)
	if resp.StatusCode != 200 {
		return "", false, fmt.Errorf("invalid or expired code")
	}
	var result struct {
		SessionToken string `json:"session_token"`
		IsNewUser    bool   `json:"is_new_user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", false, fmt.Errorf("failed to decode response: %w", err)
	}
	if result.SessionToken == "" {
		return "", false, fmt.Errorf("server returned empty session token")
	}
	return result.SessionToken, result.IsNewUser, nil
}

// reclaimableHost mirrors the server's response shape for
// /auth/session/reclaimable-hosts.
type reclaimableHost struct {
	HostID           string `json:"host_id"`
	Hostname         string `json:"hostname,omitempty"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	OrganizationSlug string `json:"organization_slug"`
	LastSeenAt       string `json:"last_seen_at,omitempty"`
}

// fetchReclaimableHosts calls /auth/session/reclaimable-hosts to find
// the user's existing hosts that are candidates for re-claim during a
// fresh `hearth login` (no host_id in the credentials file). Lets the
// CLI suggest the user's previously-enrolled host on this machine
// rather than minting a fresh host_id every time credentials get
// borked.
//
// Best-effort: on any failure we return an empty list and a nil error
// so the caller falls through to the today-behavior of fresh enroll.
// Server-side errors are logged on the server, not surfaced to the user
// (a failed peek shouldn't block login).
func fetchReclaimableHosts(baseURL, sessionToken string) ([]reclaimableHost, error) {
	req, err := http.NewRequest("GET", baseURL+"/auth/session/reclaimable-hosts", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	addClientHeader(req)
	addActionHeader(req, ActionTuple{Kind: "auth", ID: "session", Action: "list_reclaimable_hosts"})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reclaimable-hosts lookup failed: %w", err)
	}
	defer resp.Body.Close()
	checkOutdated(resp)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("reclaimable-hosts lookup failed (HTTP %d)", resp.StatusCode)
	}
	var raw struct {
		Hosts []reclaimableHost `json:"hosts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return raw.Hosts, nil
}

// fetchSessionOrgs calls /auth/session/orgs (session-token bearer) to
// peek at the user's org memberships before consuming the session via
// /hosts/enroll. Lets the CLI prompt the user when their account spans
// multiple orgs and they haven't supplied --org. Returns the empty list
// for brand-new users (no memberships yet) without raising an error;
// the caller treats that as "server will bootstrap, nothing to pick".
func fetchSessionOrgs(baseURL, sessionToken string) ([]daemonOrgEntry, error) {
	req, err := http.NewRequest("GET", baseURL+"/auth/session/orgs", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	addClientHeader(req)
	addActionHeader(req, ActionTuple{Kind: "auth", ID: "session", Action: "list_orgs"})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("session orgs lookup failed: %w", err)
	}
	defer resp.Body.Close()
	checkOutdated(resp)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("session orgs lookup failed (HTTP %d)", resp.StatusCode)
	}
	var raw struct {
		Organizations []daemonOrgEntry `json:"organizations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return raw.Organizations, nil
}

// enrollResult holds the /hosts/enroll response shape. Returned as a
// struct rather than positional args because we now carry is_new_host
// and the user's org membership list — both consumed by the CLI's
// post-enroll agent_home_path prompt.
type enrollResult struct {
	HumanUserID    string
	OrganizationID string
	IODeviceID     string
	IODeviceSecret string
	HostSecret     string
	IsNewHost      bool
	Organizations  []daemonOrgEntry
}

// enrollHost claims a host_id for the authenticated user via /hosts/enroll
// and returns the paired terminal io_device credentials, plus is_new_host
// and the user's full org membership list.
//
// mode is "fresh" on first-time setup (host_id freshly minted) or
// "reclaim" when re-enrolling an existing host (owner-guarded on the
// server). sessionToken is the single-use bearer from verifyAuthCode.
//
// targetOrgID, when non-empty, asks the server to bind the host to that
// organization. Required (in spirit) when the user is in multiple orgs;
// the CLI prompts for it before calling. Ignored on same-owner reclaim
// — a host's org is locked at first enroll and is immutable thereafter
// (transfer_host removed in phase 3 step 8). Empty here means "let the
// server pick" (default to earliest-joined org, or bootstrap-create for
// brand-new users).
func enrollHost(baseURL, sessionToken, hostID, hostname, mode, userName, orgName, targetOrgID, approvalPolicy string) (enrollResult, error) {
	payload := map[string]string{
		"host_id": hostID,
		"mode":    mode,
	}
	if hostname != "" {
		payload["hostname"] = hostname
	}
	// Optional: server applies these only on first enrollment (when the
	// bootstrap org is being created and human_users.name still equals
	// the email). Reclaim flows pass them harmlessly through.
	if userName != "" {
		payload["user_name"] = userName
	}
	if orgName != "" {
		payload["organization_name"] = orgName
	}
	if targetOrgID != "" {
		payload["organization_id"] = targetOrgID
	}
	// Approval policy: who is allowed to approve permission requests on
	// this host. "owner_only" (default) | "org_members". Server consults
	// only on fresh INSERTs — reclaim preserves the existing rule.
	if approvalPolicy != "" {
		payload["approval_policy"] = approvalPolicy
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return enrollResult{}, fmt.Errorf("failed to encode request: %w", err)
	}
	req, err := http.NewRequest("POST", baseURL+"/hosts/enroll", bytes.NewReader(body))
	if err != nil {
		return enrollResult{}, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	addClientHeader(req)
	addActionHeader(req, ActionTuple{Kind: "host", ID: hostID, Action: "enroll"})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return enrollResult{}, fmt.Errorf("host enrollment failed: %w", err)
	}
	defer resp.Body.Close()
	checkOutdated(resp)
	if resp.StatusCode != 200 {
		return enrollResult{}, fmt.Errorf("host enrollment failed (HTTP %d)", resp.StatusCode)
	}
	var raw struct {
		HostID         string           `json:"host_id"`
		HostSecret     string           `json:"host_secret"`
		IODeviceID     string           `json:"io_device_id"`
		IODeviceSecret string           `json:"io_device_secret"`
		HumanUserID    string           `json:"human_user_id"`
		OrganizationID string           `json:"organization_id"`
		IsNewHost      bool             `json:"is_new_host"`
		Organizations  []daemonOrgEntry `json:"organizations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return enrollResult{}, fmt.Errorf("failed to decode response: %w", err)
	}
	if raw.IODeviceID == "" || raw.IODeviceSecret == "" || raw.HumanUserID == "" || raw.HostSecret == "" {
		return enrollResult{}, fmt.Errorf("server returned incomplete enrollment response")
	}
	return enrollResult{
		HumanUserID:    raw.HumanUserID,
		OrganizationID: raw.OrganizationID,
		IODeviceID:     raw.IODeviceID,
		IODeviceSecret: raw.IODeviceSecret,
		HostSecret:     raw.HostSecret,
		IsNewHost:      raw.IsNewHost,
		Organizations:  raw.Organizations,
	}, nil
}

// setAgentHomePathOnServer calls /hosts/agent-home-dir to persist the
// chosen agent home directory for this host. Used by `hearth login`
// post-enroll prompt — the daemon isn't running yet, so we hit the
// HTTP endpoint directly with the just-minted io_device credentials.
// Server is the source of truth; the daemon will receive this value
// via the agent_home_path push on its next WS connect.
func setAgentHomePathOnServer(baseURL, ioDeviceID, ioDeviceSecret, hostID, dir string) error {
	_, err := deviceAuthedPost(baseURL, "/hosts/agent-home-dir", ioDeviceID, ioDeviceSecret,
		ActionTuple{Kind: "host", ID: hostID, Action: "set_agent_home_path"},
		map[string]string{
			"host_id":        hostID,
			"agent_home_path": dir,
		})
	return err
}

// deviceAuthedPost issues a device-authed POST against the invite /
// device-management endpoints. ioDeviceSecret + ioDeviceID come from
// ~/.hearth/credentials; the server validates them on every request.
// payload may be nil for endpoints that take no body.
func deviceAuthedPost(baseURL, path, ioDeviceID, ioDeviceSecret string, action ActionTuple, payload interface{}) (json.RawMessage, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
	}
	req, err := http.NewRequest("POST", baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ioDeviceSecret)
	req.Header.Set("X-IO-Device-ID", ioDeviceID)
	addClientHeader(req)
	addActionHeader(req, action)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	checkOutdated(resp)
	data, _ := readAll(resp.Body)
	if resp.StatusCode != 200 {
		var envelope struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &envelope)
		if envelope.Error != "" {
			return nil, fmt.Errorf("%s", envelope.Error)
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.RawMessage(data), nil
}
