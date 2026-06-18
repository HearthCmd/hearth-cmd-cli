//go:build darwin || linux

package main

import "testing"

func TestHumanReadableDenyMessage(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"", "denied: no matching rule"},
		{"human_timeout", "approval timed out — no response from the approver"},
		{"human:deny", "denied by approver"},
		{"denied by rule: external_resource.echo.echo", "denied: denied by rule: external_resource.echo.echo"},
		{"some_future_reason", "denied: some_future_reason"},
	}
	for _, tc := range cases {
		if got := humanReadableDenyMessage(tc.reason); got != tc.want {
			t.Errorf("reason=%q: got %q want %q", tc.reason, got, tc.want)
		}
	}
}
