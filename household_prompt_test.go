package main

import (
	"strings"
	"testing"
)

// The daemon only PLACES the household context — the relay composes it. That is
// what keeps "the handbook grew sections" a server change rather than a CLI
// release, so this must stay a pass-through and not start rendering.
func TestBuildHouseholdPrompt(t *testing.T) {
	const block = "This household is The Bellers.\n\nThe people who live here:\n  Matt (owner)"
	if got := buildHouseholdPrompt(block); got != block {
		t.Fatalf("prompt = %q, want it passed through unchanged", got)
	}
	for _, empty := range []string{"", "   ", "\n\n"} {
		if got := buildHouseholdPrompt(empty); got != "" {
			t.Fatalf("buildHouseholdPrompt(%q) = %q, want empty", empty, got)
		}
	}
}

// An older server sends no household_context at all, and an agent must spawn
// exactly as it did before — degrade-to-nothing, not an empty heading claiming
// the household is empty.
func TestBuildHouseholdPromptAbsentIsInert(t *testing.T) {
	identity := buildIdentityPrompt("Bob", "Gardener", "Keeps the garden", "The Bellers")
	if identity == "" {
		t.Fatal("identity prompt should be non-empty for this fixture")
	}
	if hp := buildHouseholdPrompt(""); hp != "" {
		t.Fatalf("absent household context produced %q", hp)
	}
	// And the identity prompt itself is untouched by the addition.
	if !strings.Contains(identity, "Your name is Bob") {
		t.Fatalf("identity prompt changed shape: %q", identity)
	}
}
