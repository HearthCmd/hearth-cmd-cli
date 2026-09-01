package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// The display server is a host, and its screen is a shared io_device claimed by
// a phone (docs/household-display-plan.md §2). On startup an unclaimed box mints
// the screen's credentials, starts a pairing authenticated AS the host (so the
// relay stamps serving_host_id), shows the code on the panel, and polls until a
// trusted phone claims it — at which point the screen is bound to this host.

// randomSecret returns a 64-hex-char random secret for the screen io_device.
func randomSecret() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// startDisplayPairing POSTs /pair/start authenticated as the host (Bearer
// host_secret + ?host_id=…) so the pending pairing records serving_host_id.
// Returns the short code for the homeowner to type into their phone.
func startDisplayPairing(baseURL, hostID, hostSecret, screenID, secretHash string, isTemp bool) (string, error) {
	payload := map[string]interface{}{
		"io_device_id": screenID,
		"form_factor":  "display",
		"device_name":  "Display",
		"secret_hash":  secretHash,
		"is_temp":      isTemp,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", baseURL+"/pair/start?host_id="+url.QueryEscape(hostID), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+hostSecret)
	addClientHeader(req)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("pair/start HTTP %d", resp.StatusCode)
	}
	var r struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.Code == "" {
		return "", fmt.Errorf("pair/start returned no code")
	}
	return r.Code, nil
}

// pollDisplayPairing POSTs /pair/poll and returns the pairing status
// ("pending" | "claimed" | "not_found").
func pollDisplayPairing(baseURL, screenID, code string) (string, error) {
	payload := map[string]string{"io_device_id": screenID, "code": code}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", baseURL+"/pair/poll", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	addClientHeader(req)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("pair/poll HTTP %d", resp.StatusCode)
	}
	var r struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	return r.Status, nil
}

// Screens pair themselves now (browser-as-screen, §B4): each kiosk browser claims
// itself via the display server's /screen/pair endpoint and holds its own
// credential (display_browser_pairing.go). The former daemon-side auto-pair of one
// fixed screen at startup was retired with that change; startDisplayPairing /
// pollDisplayPairing remain — the browser-pairing handlers drive them per browser.
