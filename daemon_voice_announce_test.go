//go:build darwin || linux

package main

import "testing"

// The relay never names a Home Assistant service. It sends a boolean, and this
// pair is the whole vocabulary — so a compromised or buggy relay push cannot make
// this host call an arbitrary HA verb with the household's credential.
func TestSatelliteVerb(t *testing.T) {
	if got := satelliteVerb(false); got != "announce" {
		t.Errorf("satelliteVerb(false) = %q, want announce", got)
	}
	// start_conversation speaks AND re-opens the microphone; announce only speaks.
	// Picking the wrong one here is the difference between a conversation that
	// continues and one that silently ends after the agent's question.
	if got := satelliteVerb(true); got != "start_conversation" {
		t.Errorf("satelliteVerb(true) = %q, want start_conversation", got)
	}
}
