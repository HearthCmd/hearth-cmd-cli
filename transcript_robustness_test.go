//go:build darwin || linux

package main

// Unit coverage for a transcript-pipeline fix:
//   - parseStreamPID: the orphan-reaper PID-file parser (O1). The reaper used
//     to Atoi the WHOLE "<pid> <uuid>" file and always failed, so it never
//     reaped a leaked `hearth stream` process after a daemon crash.

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
