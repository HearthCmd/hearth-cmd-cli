//go:build darwin || linux

package main

// Unit coverage for two transcript-pipeline fixes:
//   - parseStreamPID: the orphan-reaper PID-file parser (O1). The reaper used
//     to Atoi the WHOLE "<pid> <uuid>" file and always failed, so it never
//     reaped a leaked `hearth stream` process after a daemon crash.
//   - talkModel reconnect: the TUI's `subscribed` set must reset on every
//     (re)connect (T1), or the TUI goes silent after the first reconnect
//     because it thinks it's still subscribed and never re-sends subscribe_agent.

import "testing"

func TestParseStreamPID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"pid and uuid (current writer format)", "12345 abcd-ef01-2345", 12345},
		{"bare pid (legacy)", "12345", 12345},
		{"trailing newline", "12345 abcd\n", 12345},
		{"leading/trailing space", "  678  uuid  ", 678},
		{"non-numeric first field", "notapid uuid", 0},
		{"empty", "", 0},
		{"whitespace only", "   \n", 0},
		{"zero pid rejected", "0 uuid", 0},
		{"negative pid rejected", "-5 uuid", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseStreamPID([]byte(c.in)); got != c.want {
				t.Errorf("parseStreamPID(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestTalkModelReconnectResetsSubscriptions pins T1: a fresh connect clears the
// stale subscription record so the ai_agent_instances_list the server pushes on
// connect drives applyInstances to re-subscribe. Without the reset, the second
// connection never re-subscribes and the transcript freezes.
func TestTalkModelReconnectResetsSubscriptions(t *testing.T) {
	m := talkModel{
		subscribed: map[string]bool{"agent-a": true, "agent-b": true},
	}
	updated, _ := m.Update(wsConnectedMsg{})
	tm, ok := updated.(talkModel)
	if !ok {
		t.Fatalf("Update returned %T, want talkModel", updated)
	}
	if len(tm.subscribed) != 0 {
		t.Fatalf("subscribed not cleared on connect: %v", tm.subscribed)
	}
	if tm.status != "Connected" {
		t.Errorf("status = %q, want Connected", tm.status)
	}
}
