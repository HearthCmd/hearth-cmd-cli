package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Browser-driven screen pairing (§B4, made thin+stateless in §B7). The browser IS a
// screen: it mints its OWN id + secret (like a voice puck mints its own), stores them
// locally, and asks this display server to start a host-authed pairing. The display
// server is a THIN, STATELESS forward — it holds no pending state and never mints or
// stores the secret. It exists only because (a) the kiosk (served from this box) can't
// call the cloud relay's /pair/start cross-origin, and (b) /pair/start needs the host
// credential to bind serving_host_id. So this server hashes the browser's secret for
// the relay's rate-limited /pair/start and forwards /pair/poll — nothing more.
//
// The screen becomes real only when a trusted phone claims the code (the relay's
// /pair/claim, member/owner-gated + rate-limited). The relay's display_screens push
// then teaches this server the screen's secret_hash for /ws/screen validation — no
// self-add needed, because that push reliably lands before the browser's ~2s poll
// even returns "claimed".

const (
	screenPairPerIPWindow = time.Minute
	screenPairPerIPMax    = 30 // /screen/pair starts per minute per LAN client
)

// screenPairLimiter is a small per-source-IP sliding-window limiter on /screen/pair,
// so a LAN peer can't spam it to burn the host's shared relay /pair/start budget
// (which the relay caps per host IP). Nil-safe.
type screenPairLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
}

func newScreenPairLimiter() *screenPairLimiter {
	return &screenPairLimiter{hits: map[string][]time.Time{}, max: screenPairPerIPMax, window: screenPairPerIPWindow}
}

func (l *screenPairLimiter) allow(key string, now time.Time) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	ts := l.hits[key]
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	l.hits[key] = kept
	return len(kept) <= l.max
}

func clientIPForPair(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleScreenPair (POST /screen/pair) starts a host-authed pairing for a browser-
// minted screen. Body: {io_device_id, secret, is_temp}. The browser holds the secret;
// we hash it for the relay and never store it (the secret already crosses the LAN on
// every /ws/screen handshake, so receiving it here is no new exposure). Response:
// {code} (or {error}). Unauthenticated by design — the phone's claim is the boundary.
func (d *displayServer) handleScreenPair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !d.screenPairLimiter.allow(clientIPForPair(r), time.Now()) {
		writeScreenJSON(w, map[string]interface{}{"error": "rate limited; try again shortly"})
		return
	}
	var body struct {
		IODeviceID string `json:"io_device_id"`
		Secret     string `json:"secret"`
		IsTemp     bool   `json:"is_temp"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil ||
		body.IODeviceID == "" || body.Secret == "" {
		writeScreenJSON(w, map[string]interface{}{"error": "io_device_id and secret required"})
		return
	}

	baseURL, err := serverBaseURL()
	if err != nil {
		writeScreenJSON(w, map[string]interface{}{"error": "relay base URL unavailable"})
		return
	}
	hostID := readConfigValue("host_id")
	hostSecret := readConfigValue("host_secret")
	if hostID == "" || hostSecret == "" {
		writeScreenJSON(w, map[string]interface{}{"error": "display server not enrolled"})
		return
	}
	code, err := startDisplayPairing(baseURL, hostID, hostSecret, body.IODeviceID, sha256Hex([]byte(body.Secret)), body.IsTemp)
	if err != nil {
		writeScreenJSON(w, map[string]interface{}{"error": "pairing start failed"})
		return
	}
	resp := map[string]interface{}{"code": code}
	// A scan-to-claim deep link the kiosk renders as a QR: opens the app straight to
	// claiming this code. Omitted when the app base can't be derived (e.g. localhost),
	// in which case the kiosk just shows the code.
	if appBase := appBaseURL(); appBase != "" {
		resp["claim_url"] = appBase + "/pair?code=" + url.QueryEscape(code)
	}
	writeScreenJSON(w, resp)
}

// handleScreenPairPoll (GET /screen/pair/poll?screen_id=&code=) forwards the relay
// poll and returns its status. Stateless — the browser holds its credential and re-
// starts on not_found; on claimed it connects /ws/screen (the relay's display_screens
// push has already taught this server the screen's secret_hash).
func (d *displayServer) handleScreenPairPoll(w http.ResponseWriter, r *http.Request) {
	screenID := r.URL.Query().Get("screen_id")
	code := r.URL.Query().Get("code")
	if screenID == "" || code == "" {
		writeScreenJSON(w, map[string]interface{}{"status": "unknown"})
		return
	}
	baseURL, err := serverBaseURL()
	if err != nil {
		writeScreenJSON(w, map[string]interface{}{"status": "pending"})
		return
	}
	status, err := pollDisplayPairing(baseURL, screenID, code)
	if err != nil {
		writeScreenJSON(w, map[string]interface{}{"status": "pending"})
		return
	}
	writeScreenJSON(w, map[string]interface{}{"status": status})
}

func writeScreenJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
