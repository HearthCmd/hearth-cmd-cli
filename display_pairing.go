package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
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
func startDisplayPairing(baseURL, hostID, hostSecret, screenID, secretHash string) (string, error) {
	payload := map[string]string{
		"io_device_id": screenID,
		"form_factor":  "display",
		"device_name":  "Display",
		"secret_hash":  secretHash,
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

// provisionAndPairScreen mints (or reuses) the screen's credentials, starts a
// pairing, shows the code on the panel, and polls in the background until the
// screen is claimed. Best-effort: any failure logs and leaves the server serving
// unclaimed rather than blocking startup.
func provisionAndPairScreen(d *displayServer) {
	baseURL, err := serverBaseURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth display: %v (serving unclaimed)\n", err)
		return
	}
	hostID := readConfigValue("host_id")
	hostSecret := readConfigValue("host_secret")
	if hostID == "" || hostSecret == "" {
		fmt.Fprintln(os.Stderr, "hearth display: not enrolled; cannot pair the screen")
		return
	}

	// Reuse the screen's credentials across restarts so we re-pair the SAME
	// io_device rather than minting a new one each boot.
	screenID := readConfigValue("display_io_device_id")
	secret := readConfigValue("display_io_device_secret")
	if screenID == "" || secret == "" {
		screenID = generateUUID()
		secret = randomSecret()
		_ = writeConfigValue("display_io_device_id", screenID)
		_ = writeConfigValue("display_io_device_secret", secret)
	}
	secretHash := sha256Hex([]byte(secret))

	code, err := startDisplayPairing(baseURL, hostID, hostSecret, screenID, secretHash)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth display: pairing start failed: %v (serving unclaimed)\n", err)
		return
	}
	d.setPairing(code)
	fmt.Fprintf(os.Stderr, "hearth display: pair this screen in the Hearth app with code %s\n", code)

	go func() {
		for {
			time.Sleep(2 * time.Second)
			status, err := pollDisplayPairing(baseURL, screenID, code)
			if err != nil {
				continue
			}
			switch status {
			case "claimed":
				_ = writeConfigValue("display_claimed", "1")
				d.clearPairing()
				fmt.Fprintln(os.Stderr, "hearth display: screen claimed")
				return
			case "not_found":
				// The code expired (10-min TTL) before anyone claimed it — mint a
				// fresh one and keep showing it on the panel.
				if newCode, err := startDisplayPairing(baseURL, hostID, hostSecret, screenID, secretHash); err == nil {
					code = newCode
					d.setPairing(code)
					fmt.Fprintf(os.Stderr, "hearth display: pairing code refreshed: %s\n", code)
				}
			}
		}
	}()
}
