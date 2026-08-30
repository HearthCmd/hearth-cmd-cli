package main

import (
	"strings"
	"testing"
)

func TestBuildPresencePrompt(t *testing.T) {
	got := buildPresencePrompt()
	if got == "" {
		t.Fatal("presence prompt must not be empty")
	}
	// It must teach the two-step flow: resolve the member, then assert.
	for _, want := range []string{
		"hearth hh user list",
		"hearth presence assert --human",
		"their own biometric",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("presence prompt missing %q\n---\n%s", want, got)
		}
	}
}
