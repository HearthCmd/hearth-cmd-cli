//go:build darwin || linux

package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// reapTransport is a synchronized displayTransport for the reap tests — the reap
// fires from a timer goroutine, so reads of what was sent must be locked.
type reapTransport struct {
	mu        sync.Mutex
	sent      [][]byte
	connected bool
}

func (r *reapTransport) SendText(b []byte) {
	r.mu.Lock()
	r.sent = append(r.sent, append([]byte(nil), b...))
	r.mu.Unlock()
}
func (r *reapTransport) IsConnected() bool { return r.connected }

// reapedScreens returns the screen ids in the display_screen_reap frames sent so far.
func (r *reapTransport) reapedScreens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, b := range r.sent {
		var m struct {
			Type string `json:"type"`
			Data struct {
				ScreenID string `json:"screen_id"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &m) == nil && m.Type == "display_screen_reap" {
			out = append(out, m.Data.ScreenID)
		}
	}
	return out
}

func reapSetup(t *testing.T, grace time.Duration) (*displayServer, *reapTransport) {
	t.Helper()
	d := newDisplayServer()
	d.reapGrace = grace
	tx := &reapTransport{connected: true}
	d.attachTransport(tx) // sets relayTx under the mutex (happens-before the timer goroutine)
	d.applyDisplayScreens([]displayScreenInfo{
		{ScreenID: "temp", SecretHash: "h", IsTemp: true},
		{ScreenID: "perm", SecretHash: "h2", IsTemp: false},
	})
	return d, tx
}

func waitReaped(t *testing.T, tx *reapTransport, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, s := range tx.reapedScreens() {
			if s == id {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("screen %q was not reaped within the deadline", id)
}

// An ephemeral screen with no browser is reaped after the grace window; a permanent
// screen never is.
func TestPresenceGraceReap(t *testing.T) {
	d, tx := reapSetup(t, 20*time.Millisecond)

	d.scheduleReap("temp")
	waitReaped(t, tx, "temp")

	// scheduleReap on a permanent screen is a no-op — nothing ever fires.
	d.scheduleReap("perm")
	time.Sleep(80 * time.Millisecond)
	for _, s := range tx.reapedScreens() {
		if s == "perm" {
			t.Fatal("a permanent screen must never be reaped")
		}
	}
}

// A (re)connect within the grace window cancels the reap.
func TestReapCancelledByReconnect(t *testing.T) {
	d, tx := reapSetup(t, 60*time.Millisecond)
	d.scheduleReap("temp")
	d.cancelReap("temp") // browser came back
	time.Sleep(140 * time.Millisecond)
	if got := tx.reapedScreens(); len(got) != 0 {
		t.Fatalf("a cancelled reap must not fire, got %v", got)
	}
}

// If a browser is present when the timer fires (a reconnect raced it), the reap is
// skipped.
func TestReapSkippedWhenBrowserPresent(t *testing.T) {
	d, tx := reapSetup(t, 20*time.Millisecond)
	d.subscribeToScreen("temp") // a live browser
	d.scheduleReap("temp")
	time.Sleep(80 * time.Millisecond)
	if got := tx.reapedScreens(); len(got) != 0 {
		t.Fatalf("reap must skip while a browser is present, got %v", got)
	}
}
