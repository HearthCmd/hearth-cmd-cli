package main

import (
	"encoding/json"
	"time"
)

// Presence-grace reap (§B5 of docs/display-browser-screens-plan.md). An ephemeral
// (is_temp) screen's whole reason to exist is a browser tab someone opened; when they
// close it or walk away with the laptop, /ws/screen drops. So the display server —
// which knows the difference between "no browser" and "just rebooting" — watches for
// an is_temp screen's LAST browser leaving and, after a grace window with no
// reconnect, asks the relay to remove the screen (display_screen_reap). The relay,
// not the display server, is authoritative for the removal, and only honors a reap of
// an is_temp screen bound to this host (handleDisplayScreenReap). A permanent screen
// is never reaped — a browser disconnect there just means "offline," not "delete."

const defaultReapGrace = 15 * time.Minute

// scheduleReap arms (or re-arms) the grace timer for an ephemeral screen whose last
// browser just left. No-op for a non-ephemeral or unknown screen.
func (d *displayServer) scheduleReap(screenID string) {
	if screenID == "" {
		return
	}
	cred, ok := d.knownScreen(screenID)
	if !ok || !cred.IsTemp {
		return
	}
	grace := d.reapGrace
	if grace <= 0 {
		grace = defaultReapGrace
	}
	d.reapMu.Lock()
	if t := d.reapTimers[screenID]; t != nil {
		t.Stop()
	}
	d.reapTimers[screenID] = time.AfterFunc(grace, func() { d.fireReap(screenID) })
	d.reapMu.Unlock()
}

// cancelReap stops a pending reap — a browser (re)connected within the grace window.
func (d *displayServer) cancelReap(screenID string) {
	d.reapMu.Lock()
	if t := d.reapTimers[screenID]; t != nil {
		t.Stop()
		delete(d.reapTimers, screenID)
	}
	d.reapMu.Unlock()
}

// fireReap runs when the grace window elapses. It re-checks (a reconnect can race the
// timer, and the screen may have been reaped/changed) before asking the relay to
// remove the screen.
func (d *displayServer) fireReap(screenID string) {
	d.reapMu.Lock()
	delete(d.reapTimers, screenID)
	d.reapMu.Unlock()

	if d.liveConnCount(screenID) > 0 {
		return // a browser came back
	}
	cred, ok := d.knownScreen(screenID)
	if !ok || !cred.IsTemp {
		return // no longer ephemeral/known
	}
	d.sendReap(screenID)
}

// sendReap asks the relay to remove an ephemeral screen. Best-effort over the same
// daemon link display_state reports go out on; the relay gates it (host + is_temp).
func (d *displayServer) sendReap(screenID string) {
	d.mu.Lock()
	tx := d.relayTx
	d.mu.Unlock()
	if tx == nil || !tx.IsConnected() {
		return
	}
	b, _ := json.Marshal(map[string]interface{}{
		"type": "display_screen_reap",
		"data": map[string]interface{}{"screen_id": screenID},
	})
	tx.SendText(b)
}

// liveConnCount reports how many browsers are currently connected to a screen.
func (d *displayServer) liveConnCount(screenID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st := d.screens[d.screenKey(screenID)]; st != nil {
		return len(st.subs)
	}
	return 0
}
