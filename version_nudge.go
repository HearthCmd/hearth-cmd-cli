//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Server-controlled update nudge. Rather than polling GitHub, the CLI asks the
// relay's /version_check endpoint whether this build is below the server's
// HEARTH_RECOMMENDED_CLI. The server answers with {recommended, update_url}
// ONLY when we're actually behind (it runs compareSemver internally), so the
// operator arms the nudge by setting one env var at release time — no client
// change, no GitHub round-trip. The hard MIN gate (426 / WS 4426) is a separate,
// blocking path handled in client_header.go; this is the soft, informational one.

// serverVersionNudge is a recommendation handed back by /version_check.
type serverVersionNudge struct {
	Recommended string
	UpdateURL   string
}

// checkServerVersion pings /version_check and returns a nudge when the server
// says this CLI is below the recommended version, or nil otherwise. Fail-soft
// by construction: no server URL, offline, an unstamped dev build, a non-200
// (including the 426 hard-gate, which the blocking paths own), or an empty
// recommendation all return nil, so a caller only ever nudges on a confident
// yes. `timeout` bounds how long a human-facing caller (e.g. `hearth status`)
// will wait.
func checkServerVersion(timeout time.Duration) *serverVersionNudge {
	// Never nag an unstamped/dev build — it isn't a real release and the
	// server would compare its "0.0.0" header as below any recommendation.
	if v := strings.TrimSpace(version); v == "" || v == "dev" {
		return nil
	}
	base, err := serverBaseURL()
	if err != nil {
		return nil
	}
	req, err := http.NewRequest("GET", base+"/version_check", nil)
	if err != nil {
		return nil
	}
	addClientHeader(req)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	// 426 = below MIN. That's the hard gate's job (it blocks + exits on the
	// next authed call / WS dial); a soft status/startup nudge must not act on
	// it. Any non-200 → stay silent.
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Recommended string `json:"recommended"`
		UpdateURL   string `json:"update_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	if strings.TrimSpace(body.Recommended) == "" {
		return nil // up to date, or no recommendation configured
	}
	return &serverVersionNudge{Recommended: body.Recommended, UpdateURL: body.UpdateURL}
}

// summary renders the one-line nudge (running → recommended).
func (n *serverVersionNudge) summary() string {
	return fmt.Sprintf("a newer version is recommended (%s → %s) — run `hearth update`",
		clientVersionValue(), n.Recommended)
}
