package main

import "testing"

// rolesCSVIncludes drives which subsystems the role-aware daemon activates.
func TestRolesCSVIncludes(t *testing.T) {
	cases := []struct {
		csv, role string
		want      bool
	}{
		{"", "display", false},
		{"agent", "display", false},
		{"display", "display", true},
		{"agent,display", "display", true},
		{" agent , display ", "display", true},
		{"agent,display", "agent", true},
		{"display", "agent", false},
	}
	for _, c := range cases {
		if got := rolesCSVIncludes(c.csv, c.role); got != c.want {
			t.Errorf("rolesCSVIncludes(%q, %q) = %v, want %v", c.csv, c.role, got, c.want)
		}
	}
}

// The daemon routes relay display frames to the display subsystem only when the
// display frame handler is wired (role-display host); otherwise a display frame
// falls through unconsumed. With it wired, the frame both routes and applies.
func TestDaemonWSDisplayFrameRouting(t *testing.T) {
	d := NewDaemonWS("ws://example/ws/daemon", "secret")
	frame := []byte(`{"type":"display_publish","cmd":"show","url":"http://recipe"}`)

	if d.handleTextFrame(frame) {
		t.Fatal("without a display func wired, a display frame must not be consumed")
	}

	ds := newDisplayServer()
	d.setDisplayFrameFunc(ds.handleRelayFrame)
	if !d.handleTextFrame(frame) {
		t.Fatal("with the display func wired, display_publish should be consumed")
	}
	if got := ds.current(); got.Payload != "http://recipe" {
		t.Fatalf("routed frame should have applied: current = %+v", got)
	}
}

// The relay sends display_screens the moment the daemon connects, before the
// daemon has started its display subsystem. That set must be held and handed
// over when the subsystem attaches (latest wins), or the display server starts
// with no screens and turns every paired browser away after a restart.
func TestDaemonWSHoldsDisplayScreensUntilAttached(t *testing.T) {
	d := NewDaemonWS("ws://example/ws/daemon", "secret")
	older := []byte(`{"type":"display_screens","screens":[{"screen_id":"old","secret_hash":"h0"}]}`)
	newer := []byte(`{"type":"display_screens","screens":[{"screen_id":"kitchen","secret_hash":"h1"}]}`)
	if !d.handleTextFrame(older) || !d.handleTextFrame(newer) {
		t.Fatal("display_screens before attach should be held (consumed)")
	}

	ds := newDisplayServer()
	if ds.screensReady() {
		t.Fatal("a fresh display server has not heard from the relay")
	}
	d.setDisplayFrameFunc(ds.handleRelayFrame)
	if !ds.screensReady() {
		t.Fatal("the held display_screens set should apply on attach")
	}
	if _, ok := ds.knownScreen("kitchen"); !ok {
		t.Fatal("the latest held set should apply")
	}
	if _, ok := ds.knownScreen("old"); ok {
		t.Fatal("an older held set must not win over the latest")
	}

	// Handed over once: attaching again must not replay it over newer state.
	d.setDisplayFrameFunc(ds.handleRelayFrame)
	d.handleTextFrame([]byte(`{"type":"display_screens","screens":[]}`))
	d.setDisplayFrameFunc(ds.handleRelayFrame)
	if _, ok := ds.knownScreen("kitchen"); ok {
		t.Fatal("a held set was replayed after a newer push")
	}
}
