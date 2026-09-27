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
// displayPairingStart is the relay's /pair/start result: the code plus, for a
// host-authed display pairing, which household + computer this screen will join
// (shown on the kiosk pairing page so a screen isn't paired into the wrong home).
type displayPairingStart struct {
	Code      string
	Household string
	Host      string
}

// priorScreen, when set, is the screen this browser was until the display server
// refused its credential (id + hash of that screen's secret). The relay checks it at
// claim and, if it proves the browser was a live screen, re-pairs into that screen
// so it keeps its rules.
type priorScreen struct {
	ID         string
	SecretHash string
}

func startDisplayPairing(baseURL, hostID, hostSecret, screenID, secretHash string, isTemp bool, prior *priorScreen) (displayPairingStart, error) {
	payload := map[string]interface{}{
		"io_device_id": screenID,
		"form_factor":  "display",
		"device_name":  "Display",
		"secret_hash":  secretHash,
		"is_temp":      isTemp,
	}
	if prior != nil {
		payload["prior_screen_id"] = prior.ID
		payload["prior_secret_hash"] = prior.SecretHash
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", baseURL+"/pair/start?host_id="+url.QueryEscape(hostID), bytes.NewReader(body))
	if err != nil {
		return displayPairingStart{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+hostSecret)
	addClientHeader(req)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return displayPairingStart{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return displayPairingStart{}, fmt.Errorf("pair/start HTTP %d", resp.StatusCode)
	}
	var r struct {
		Code             string `json:"code"`
		ServingHousehold string `json:"serving_household"`
		ServingHost      string `json:"serving_host"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return displayPairingStart{}, err
	}
	if r.Code == "" {
		return displayPairingStart{}, fmt.Errorf("pair/start returned no code")
	}
	return displayPairingStart{Code: r.Code, Household: r.ServingHousehold, Host: r.ServingHost}, nil
}

// displayPairingPoll is the relay's /pair/poll result. On a claim that re-paired
// into an existing screen, IODeviceID is that screen's id and Reclaimed is true:
// the browser must adopt the id, because the relay rotated that screen's secret to
// the browser's and never created a screen under the browser's own id.
type displayPairingPoll struct {
	Status     string `json:"status"` // "pending" | "claimed" | "not_found"
	IODeviceID string `json:"io_device_id"`
	Reclaimed  bool   `json:"reclaimed"`
}

// pollDisplayPairing POSTs /pair/poll and returns the pairing status.
func pollDisplayPairing(baseURL, screenID, code string) (displayPairingPoll, error) {
	payload := map[string]string{"io_device_id": screenID, "code": code}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", baseURL+"/pair/poll", bytes.NewReader(body))
	if err != nil {
		return displayPairingPoll{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	addClientHeader(req)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return displayPairingPoll{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return displayPairingPoll{}, fmt.Errorf("pair/poll HTTP %d", resp.StatusCode)
	}
	var r displayPairingPoll
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return displayPairingPoll{}, err
	}
	return r, nil
}

// Screens pair themselves now (browser-as-screen, §B4): each kiosk browser claims
// itself via the display server's /screen/pair endpoint and holds its own
// credential (display_browser_pairing.go). The former daemon-side auto-pair of one
// fixed screen at startup was retired with that change; startDisplayPairing /
// pollDisplayPairing remain — the browser-pairing handlers drive them per browser.
