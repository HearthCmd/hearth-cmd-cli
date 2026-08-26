//go:build darwin || linux

package main

import "testing"

// A4 (substrate plan): envelopeTimestamp must read the ts out of both the
// hearth/1 and the hearth/2 envelope (they carry ts identically).
func TestEnvelopeTimestamp_DualVersion(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "v1 human from",
			text: `hearth/1 {"from":{"id":"u1","name":"Matt"},"mid":"m1","ts":"2026-01-02T03:04:05Z"}` + "\n\nhi",
			want: "2026-01-02T03:04:05Z",
		},
		{
			name: "v2 typed sender + human",
			text: `hearth/2 {"from":{"kind":"device","id":"dev1"},"human":{"id":"u1","name":"Matt"},"mid":"m1","ts":"2026-06-07T08:09:10Z"}` + "\n\nhi",
			want: "2026-06-07T08:09:10Z",
		},
		{
			name: "no envelope",
			text: "just some agent output",
			want: "",
		},
		{
			name: "unknown version",
			text: `hearth/9 {"ts":"2026-01-01T00:00:00Z"}` + "\n\nx",
			want: "",
		},
	}
	for _, c := range cases {
		if got := envelopeTimestamp(c.text); got != c.want {
			t.Errorf("%s: envelopeTimestamp = %q, want %q", c.name, got, c.want)
		}
	}
}
