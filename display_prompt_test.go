package main

import (
	"strings"
	"testing"
)

func TestBuildDisplayPrompt_Empty(t *testing.T) {
	for _, in := range []string{"", "  ", "[]", "not json"} {
		if got := buildDisplayPrompt(in); got != "" {
			t.Errorf("buildDisplayPrompt(%q) = %q, want empty", in, got)
		}
	}
}

func TestBuildDisplayPrompt_RendersScreensAndVerb(t *testing.T) {
	got := buildDisplayPrompt(`[{"id":"disp-1","name":"Kitchen Display"},{"id":"disp-2","name":"Hallway"}]`)
	if got == "" {
		t.Fatal("expected a non-empty display prompt")
	}
	for _, want := range []string{"hearth display publish", "hearth display query", "Kitchen Display", "Hallway", "Household displays"} {
		if !strings.Contains(got, want) {
			t.Errorf("display prompt missing %q; got:\n%s", want, got)
		}
	}
}

// A screen with no name falls back to its id.
func TestBuildDisplayPrompt_NamelessFallsBackToID(t *testing.T) {
	got := buildDisplayPrompt(`[{"id":"disp-x","name":""}]`)
	if !strings.Contains(got, "disp-x") {
		t.Errorf("nameless screen should list its id; got:\n%s", got)
	}
}
