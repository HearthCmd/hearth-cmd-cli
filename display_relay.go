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
		Type       string `json:"type"`
		Cmd        string `json:"cmd"`
		Kind       string `json:"kind"`
		URL        string `json:"url"`
		Markdown   string `json:"markdown"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return false
	}
	switch msg.Type {
	case "display_publish", "display_clear":
		_ = d.applyControl(controlCommand{Cmd: msg.Cmd, Kind: msg.Kind, URL: msg.URL, Markdown: msg.Markdown, TTLSeconds: msg.TTLSeconds})
		return true
	}
	return false
}

// displayStateFrame is the display_state report the display server pushes to the
// relay (docs/household-display-plan.md): what's on the screen right now, so
// display.query can answer without a round trip. The payload lives under `data`
// like every other daemon→relay frame — the relay's daemon dispatch hands
// handleDisplayState the frame's `data` field, not its top level. Pure, so it's
// unit-tested. (Putting kind/payload at the top level was a silent bug: the report
// arrived but handleDisplayState unmarshalled an empty `data` and bailed.)
func displayStateFrame(a screenAssignment) map[string]interface{} {
	data := map[string]interface{}{"kind": a.Kind, "payload": a.Payload}
	if !a.ExpiresAt.IsZero() {
		data["expires_at"] = a.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return map[string]interface{}{"type": "display_state", "data": data}
}

// reportState pushes the current content to the relay over the daemon WS. No-op
// until the relay link is up (the periodic reporter re-sends once connected, so a
// relay restart re-warms its cache within a heartbeat). Called on every change
// (via wakeAll) and on a timer.
func (d *displayServer) reportState() {
	d.mu.Lock()
	tx := d.relayTx
	d.mu.Unlock()
	if tx == nil || !tx.IsConnected() {
		return
	}
	b, _ := json.Marshal(displayStateFrame(d.current()))
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
