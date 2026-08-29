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
