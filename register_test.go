//go:build darwin || linux

package main

import "testing"

// Pure helpers in register.go that derive plausible defaults from an
// email or username — used to pre-fill the registration prompts so
// the user can usually just hit Enter.

func TestDefaultUserNameFromEmail(t *testing.T) {
	cases := []struct{ in, want string }{
		// Plain local part — title-case the whole thing.
		{"alice@example.com", "Alice"},
		// Dot separator → space + title-case each segment.
		{"matt.beller@example.com", "Matt Beller"},
		// Underscore / dash / plus all split as well.
		{"matt_beller@example.com", "Matt Beller"},
		{"matt-beller@example.com", "Matt Beller"},
		{"matt+work@example.com", "Matt Work"},
		// Mixed separators.
		{"john.q.public@example.com", "John Q Public"},
		// No @ — treat the whole thing as the local part.
		{"justname", "Justname"},
		// Empty → fallback.
		{"", "User"},
		// Already-capitalized passes through with title-casing of first letter.
		{"ALICE@example.com", "ALICE"},
		// Leading separator collapses (empty segments are skipped).
		{".alice@example.com", "Alice"},
	}
	for _, c := range cases {
		if got := defaultUserNameFromEmail(c.in); got != c.want {
			t.Errorf("defaultUserNameFromEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDefaultOrgNameFromUserName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Matt", "Matt's Household"},
		{"Matt Beller", "Matt's Household"},                       // first word only
		{"  Matt  Beller  ", "Matt's Household"},                  // trim + first word
		{"alice", "alice's Household"},                            // case preserved
		{"", "Home"},                                              // empty → Home
		{"   ", "Home"},                                           // whitespace-only → Home
		{"José Garcia", "José's Household"},                       // unicode
	}
	for _, c := range cases {
		if got := defaultOrgNameFromUserName(c.in); got != c.want {
			t.Errorf("defaultOrgNameFromUserName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
