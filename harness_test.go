//go:build darwin || linux

package main

// Coverage for the small pure helpers in agent.go and select.go that we
// rely on at the daemon's spawn path. Each is a pure mapping over a known
// set of harness/agent strings; getting one wrong silently routes spawns
// to the wrong binary or skips the model picker for the wrong harness.

import (
	"os"
	"strings"
	"testing"
)

func TestHarnessHonorsModelEnv(t *testing.T) {
	tests := []struct {
		harness string
		want    bool
	}{
		// Env-var honoring runtimes — we pass ANTHROPIC_MODEL / OPENAI_MODEL
		// when spawning, so the picker MUST collect a model selection.
		{"claude-code", true},
		{"codex", true},

		// Configure their model in their own UI, ignore env vars. The picker
		// MUST NOT prompt for a model and the create payload MUST omit
		// ai_brain_model_id (server-side check is enforced separately).
		{"gemini", false},
		{"copilot", false},
		{"pi", false},

		// Defensive: unknown values default to "configures own model" rather
		// than "honors env var" — safer to leave NULL than to lie.
		{"", false},
		{"unknown", false},
	}
	for _, tt := range tests {
		t.Run(tt.harness, func(t *testing.T) {
			if got := harnessHonorsModelEnv(tt.harness); got != tt.want {
				t.Errorf("harnessHonorsModelEnv(%q) = %v, want %v", tt.harness, got, tt.want)
			}
		})
	}
}

func TestAgentBinary(t *testing.T) {
	// Mapping from --agent flag value to the on-disk CLI binary the daemon
	// will exec. Default branch lands on "claude" — flagging this so a
	// rename to "claude-code" doesn't silently slip through.
	tests := []struct {
		agent string
		want  string
	}{
		{"gemini", "gemini"},
		{"copilot", "copilot"},
		{"codex", "codex"},
		{"pi", "pi"},
		{"claude", "claude"},
		{"", "claude"},
		{"bogus", "claude"},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			if got := agentBinary(tt.agent); got != tt.want {
				t.Errorf("agentBinary(%q) = %q, want %q", tt.agent, got, tt.want)
			}
		})
	}
}

func TestAgentServerName(t *testing.T) {
	// Mapping from --agent flag value to the identifier the daemon reports
	// to the server. Note that "claude" → "claude-code" (the historical
	// disagreement is intentional: --agent is a user shorthand, the server
	// stores the canonical harness name).
	tests := []struct {
		agent string
		want  string
	}{
		{"gemini", "gemini"},
		{"copilot", "copilot"},
		{"codex", "codex"},
		{"pi", "pi"},
		{"claude", "claude-code"},
		{"", "claude-code"},
		{"bogus", "claude-code"},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			if got := agentServerName(tt.agent); got != tt.want {
				t.Errorf("agentServerName(%q) = %q, want %q", tt.agent, got, tt.want)
			}
		})
	}
}

func TestResolveAgent(t *testing.T) {
	// Precedence: explicit flag > HEARTH_AGENT env > default.
	prev, hadPrev := os.LookupEnv("HEARTH_AGENT")
	t.Cleanup(func() {
		if hadPrev {
			os.Setenv("HEARTH_AGENT", prev)
		} else {
			os.Unsetenv("HEARTH_AGENT")
		}
	})

	t.Run("flag wins over env", func(t *testing.T) {
		os.Setenv("HEARTH_AGENT", "codex")
		if got := resolveAgent("gemini"); got != "gemini" {
			t.Errorf("flag should win, got %q", got)
		}
	})

	t.Run("env wins over default", func(t *testing.T) {
		os.Setenv("HEARTH_AGENT", "codex")
		if got := resolveAgent(""); got != "codex" {
			t.Errorf("env should win, got %q", got)
		}
	})

	t.Run("default when nothing set", func(t *testing.T) {
		os.Unsetenv("HEARTH_AGENT")
		if got := resolveAgent(""); got != defaultAgent {
			t.Errorf("expected default %q, got %q", defaultAgent, got)
		}
	})
}

func TestSanitizeClaudeProjectHash(t *testing.T) {
	// Claude replaces every non-alphanumeric-and-non-dash with a single
	// dash (one input char → one output char). Test the underscore /
	// dot / slash / multi-run edge cases the comment calls out.
	tests := []struct {
		cwd  string
		want string
	}{
		{"/Users/alice/project", "-Users-alice-project"},
		{"/home/hearth/hearth_agents/scratch", "-home-hearth-hearth-agents-scratch"},
		{"foo.bar.baz", "foo-bar-baz"},
		{"my-project", "my-project"},                        // existing dashes preserved
		{"one_two_three", "one-two-three"},
		{"a/b//c", "a-b--c"},                                 // each non-alnum char becomes its own dash
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.cwd, func(t *testing.T) {
			if got := sanitizeClaudeProjectHash(tt.cwd); got != tt.want {
				t.Errorf("sanitizeClaudeProjectHash(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
		})
	}
}

func TestBuildIdentityPrompt(t *testing.T) {
	t.Run("all empty returns empty", func(t *testing.T) {
		if got := buildIdentityPrompt("", "", "", ""); got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("includes provided fields", func(t *testing.T) {
		got := buildIdentityPrompt("Gardener", "Calendar Assistant", "Manage the calendar.", "The Smiths")
		// Loose assertion — exact prompt format may evolve; what matters is
		// each non-empty field shows up somewhere.
		for _, want := range []string{"Gardener", "Calendar Assistant", "calendar", "Smiths"} {
			if !strings.Contains(got, want) {
				t.Errorf("expected prompt to contain %q, got %q", want, got)
			}
		}
	})
}
