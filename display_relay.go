package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// connectRelay dials /ws/daemon as this host (reusing the daemon transport) so
// the relay can route display.publish/clear frames to this display server, then
// applies each frame to the served content. Blocks until the WS is closed, so
// run it in a goroutine; the WSClient reconnects on drop. host_secret
// authenticates, the same bearer the agent daemon uses.
//
// NOTE: a dedicated display box (this is the v1 shape) runs only `hearth display`,
// so it holds the single /ws/daemon connection for its host_id. A box that ALSO
// runs the agent daemon would open a second connection for the same host_id — the
// role-aware unified daemon (one process, both roles, one WS) is the fix and is
// future work; don't run both modes on one box yet.
func (d *displayServer) connectRelay() {
	wsurl, err := displayRelayWSURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth display: relay connection: %v (publishes from other hosts won't reach this screen)\n", err)
		return
	}
	secret := readConfigValue("host_secret")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "hearth display: not enrolled; skipping relay connection")
		return
	}
	ws := NewWSClient(wsurl, secret, WSModeRW, nil)
	ws.textFrameFunc = d.handleRelayFrame
	d.mu.Lock()
	d.relayWS = ws
	d.relayTx = ws
	d.mu.Unlock()
	ws.Run()
}

func (d *displayServer) closeRelay() {
	d.mu.Lock()
	ws := d.relayWS
	d.mu.Unlock()
	if ws != nil {
		ws.Close()
	}
}

// handleRelayFrame applies a display_publish / display_clear frame routed from
// the relay (a text frame — see WSClient.textFrameFunc). Returns true when it
// consumed a display command, false so other frames fall through untouched.
func (d *displayServer) handleRelayFrame(data []byte) bool {
	var msg struct {
		Type       string              `json:"type"`
		ScreenID   string              `json:"screen_id"`
		Cmd        string              `json:"cmd"`
		Kind       string              `json:"kind"`
		URL        string              `json:"url"`
		Markdown   string              `json:"markdown"`
		TTLSeconds int                 `json:"ttl_seconds"`
		Screens    []displayScreenInfo `json:"screens"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return false
	}
	switch msg.Type {
	case "display_publish", "display_clear":
		// Route to the target screen the relay resolved (resolveDisplayScreen). An
		// empty screen_id, or this box's own screen, collapses onto the primary
		// screen (screenKey), so a single-screen box is unchanged.
		_ = d.applyControlForScreen(msg.ScreenID, controlCommand{Cmd: msg.Cmd, Kind: msg.Kind, URL: msg.URL, Markdown: msg.Markdown, TTLSeconds: msg.TTLSeconds})
		return true
	case "display_screens":
		// The relay's authoritative set of screens bound to this host (§B3): cache it
		// for /ws/screen validation and evict any screen that just dropped (revoked).
		d.applyDisplayScreens(msg.Screens)
		return true
	}
	return false
}

// displayStateReport is the PER-SCREEN display_state report the display server pushes
// to the relay (§B6): what's on each screen it serves, plus whether a browser is live
// on it, so display.query and the app's per-screen health answer without a round trip.
// Keyed by the relay-pushed `known` set (the screens actually bound to this host).
// Payload lives under `data` like every daemon→relay frame — the relay hands
// handleDisplayState the `data` field, not the top level. Pure, so it's unit-tested.
func (d *displayServer) displayStateReport() map[string]interface{} {
	d.mu.Lock()
	ids := make([]string, 0, len(d.known))
	for id := range d.known {
		ids = append(ids, id)
	}
	d.mu.Unlock()

	screens := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		cur := d.currentForScreen(id)
		entry := map[string]interface{}{
			"screen_id": id,
			"kind":      cur.Kind,
			"payload":   cur.Payload,
			"online":    d.liveConnCount(id) > 0,
		}
		if !cur.ExpiresAt.IsZero() {
			entry["expires_at"] = cur.ExpiresAt.UTC().Format(time.RFC3339)
		}
		// The browser-reported window size (docs/display-viewport-plan.md), present
		// only once a kiosk has reported it. Last-known: it stays in the report while
		// the screen is offline (the relay's screen_online flag carries staleness).
		if vp := d.viewportForScreen(id); vp != nil {
			entry["viewport"] = map[string]interface{}{"w": vp.W, "h": vp.H, "dpr": vp.DPR}
		}
		screens = append(screens, entry)
	}
	return map[string]interface{}{"type": "display_state", "data": map[string]interface{}{"screens": screens}}
}

// reportState pushes the per-screen content to the relay over the daemon WS. No-op
// until the relay link is up (the periodic reporter re-sends once connected, so a
// relay restart re-warms its cache within a heartbeat). Called on every change
// (via wakeScreen) and on a timer.
func (d *displayServer) reportState() {
	d.mu.Lock()
	tx := d.relayTx
	d.mu.Unlock()
	if tx == nil || !tx.IsConnected() {
		return
	}
	b, _ := json.Marshal(d.displayStateReport())
	tx.SendText(b)
}

// attachTransport points the display subsystem's reports at an externally owned
// relay link (the role-aware daemon's DaemonWS). Used by the unified daemon
// instead of connectRelay, which owns its own client. Send an immediate report so
// the relay cache warms without waiting for the first change or heartbeat.
func (d *displayServer) attachTransport(tx displayTransport) {
	d.mu.Lock()
	d.relayTx = tx
	d.mu.Unlock()
	d.reportState()
}

// displayRelayWSURL builds the /ws/daemon dial URL for this host, mirroring the
// agent daemon's buildDaemonWSURL (host_id + version handshake + io_device_id),
// but sourced from ~/.hearth config rather than a *Daemon.
func displayRelayWSURL() (string, error) {
	if wsURL == "" {
		return "", fmt.Errorf("no relay URL configured")
	}
	dialURL, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("bad relay URL: %w", err)
	}
	dialURL.Path = strings.TrimSuffix(dialURL.Path, "/relay") + "/daemon"
	q := dialURL.Query()
	q.Set("host_id", readConfigValue("host_id"))
	if hostname, err := os.Hostname(); err == nil {
		q.Set("hostname", hostname)
	}
	if version != "" {
		q.Set("version", version)
	}
	addClientQuery(q)
	if devID := readConfigValue("io_device_id"); devID != "" {
		q.Set("io_device_id", devID)
	}
	dialURL.RawQuery = q.Encode()
	return dialURL.String(), nil
}
