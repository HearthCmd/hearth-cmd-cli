//go:build darwin || linux

package main

// Coverage for the pure-path helpers in agent.go: each harness encodes
// the working-dir into a per-project subdir using its own quirky
// scheme (claude → "/" and underscores both become "-"; pi wraps the
// path in "--..--" with separator translation). Getting any of these
// wrong silently lands the streamer on the wrong file or no file at
// all — covered in the codex/gemini integration memories.
//
// Plus: agentBinary, agentServerName, resolveAgent (env precedence),
// buildIdentityPrompt (the multi-field formatter), extractGeminiSessionID,
// matchesCodexCwd (read-first-line + JSON parse + cwd compare).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- resolveAgent precedence ----------
// (TestAgentBinary / TestAgentServerName live in harness_test.go.)

func TestResolveAgent_FlagWins(t *testing.T) {
	t.Setenv("HEARTH_AGENT", "from-env")
	if got := resolveAgent("from-flag"); got != "from-flag" {
		t.Errorf("flag should beat env, got %q", got)
	}
}

func TestResolveAgent_EnvFallback(t *testing.T) {
	t.Setenv("HEARTH_AGENT", "codex")
	if got := resolveAgent(""); got != "codex" {
		t.Errorf("env should win when flag is empty, got %q", got)
	}
}

func TestResolveAgent_DefaultWhenAbsent(t *testing.T) {
	t.Setenv("HEARTH_AGENT", "")
	got := resolveAgent("")
	if got == "" {
		t.Error("default agent must be non-empty")
	}
	// Don't pin the constant — just verify we end up at a known value.
	if !agentSupportsResume(got) && got != "pi" {
		t.Errorf("default agent %q is not in the known agent set", got)
	}
}

// ---------- buildIdentityPrompt ----------

func TestBuildIdentityPrompt_AllEmptyReturnsEmpty(t *testing.T) {
	if got := buildIdentityPrompt("", "", "", ""); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestBuildIdentityPrompt_AssemblesFields(t *testing.T) {
	got := buildIdentityPrompt("Gardener", "head of the garden", "tend to all plants", "The Smiths")
	for _, want := range []string{
		"Identity:",
		"Your name is Gardener",
		"You serve the household called The Smiths",
		"Your role is head of the garden",
		"Your mandate: tend to all plants.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output, got %q", want, got)
		}
	}
}

func TestBuildIdentityPrompt_AddsTerminalDotToMandate(t *testing.T) {
	got := buildIdentityPrompt("", "", "no trailing dot", "")
	if !strings.HasSuffix(got, ".") {
		t.Errorf("missing terminal dot: %q", got)
	}
	got2 := buildIdentityPrompt("", "", "already has one.", "")
	if strings.HasSuffix(got2, "..") {
		t.Errorf("must not double the dot: %q", got2)
	}
}

func TestBuildIdentityPrompt_OnlyAgentName(t *testing.T) {
	got := buildIdentityPrompt("Solo", "", "", "")
	if !strings.Contains(got, "Your name is Solo") {
		t.Error("solo agent name must render")
	}
	if strings.Contains(got, "household") || strings.Contains(got, "role is") || strings.Contains(got, "mandate") {
		t.Errorf("absent fields must not leak into output: %q", got)
	}
}

// (TestSanitizeClaudeProjectHash lives in harness_test.go.)

// ---------- piSessionPath ----------

func TestPiSessionPath(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "/fake-pi-base")
	got := piSessionPath("sess-abc", "/Users/alice/proj")
	want := filepath.Join("/fake-pi-base", "sessions", "--Users-alice-proj--", "sess-abc.jsonl")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPiSessionPath_TranslatesAllSeparators(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "/p")
	got := piSessionPath("s", "/foo:bar/baz\\qux")
	if !strings.Contains(got, "--foo-bar-baz-qux--") {
		t.Errorf("expected --foo-bar-baz-qux-- in path, got %q", got)
	}
}

// ---------- deriveTranscriptPath dispatch ----------

func TestDeriveTranscriptPath_UnknownAgentReturnsEmpty(t *testing.T) {
	if got := deriveTranscriptPath("unknown-agent", "id", "/cwd"); got != "" {
		t.Errorf("unknown agent should yield empty, got %q", got)
	}
}

func TestDeriveTranscriptPath_CodexEmptyCwdReturnsEmpty(t *testing.T) {
	// Codex requires cwd as the unique key; without it no lookup is
	// possible (no fallback, see comments in deriveTranscriptPath).
	if got := deriveTranscriptPath("codex", "any-id", ""); got != "" {
		t.Errorf("codex with empty cwd should yield empty, got %q", got)
	}
}

func TestDeriveTranscriptPath_ClaudeWithIDComposesExpectedPath(t *testing.T) {
	t.Setenv("HOME", "/fake-home")
	got := deriveTranscriptPath("claude", "sess-1", "/Users/alice/proj")
	want := filepath.Join("/fake-home", ".claude", "projects", "-Users-alice-proj", "sess-1.jsonl")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDeriveTranscriptPath_CopilotByID(t *testing.T) {
	t.Setenv("HOME", "/fake-home")
	t.Setenv("COPILOT_HOME", "")
	got := deriveTranscriptPath("copilot", "sess-1", "/cwd")
	want := filepath.Join("/fake-home", ".copilot", "session-state", "sess-1", "events.jsonl")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDeriveTranscriptPath_CopilotHomeOverride(t *testing.T) {
	t.Setenv("COPILOT_HOME", "/override")
	got := deriveTranscriptPath("copilot", "sess-1", "/cwd")
	if !strings.HasPrefix(got, "/override/") {
		t.Errorf("COPILOT_HOME override should win, got %q", got)
	}
}

// ---------- extractGeminiSessionID ----------

func TestExtractGeminiSessionID_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.jsonl")
	header := `{"sessionId":"sess-99","kind":"main"}` + "\n" + `{"role":"user"}` + "\n"
	if err := os.WriteFile(path, []byte(header), 0644); err != nil {
		t.Fatal(err)
	}
	if got := extractGeminiSessionID(path); got != "sess-99" {
		t.Errorf("got %q, want sess-99", got)
	}
}

func TestExtractGeminiSessionID_MissingFileReturnsEmpty(t *testing.T) {
	if got := extractGeminiSessionID("/this/does/not/exist.jsonl"); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractGeminiSessionID_BadJSONReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.jsonl")
	_ = os.WriteFile(path, []byte("not json\n"), 0644)
	if got := extractGeminiSessionID(path); got != "" {
		t.Errorf("expected empty on bad JSON, got %q", got)
	}
}

// ---------- matchesCodexCwd ----------

func TestMatchesCodexCwd_Match(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	meta := map[string]interface{}{
		"type":    "session_meta",
		"payload": map[string]interface{}{"cwd": "/home/alice/proj"},
	}
	b, _ := json.Marshal(meta)
	_ = os.WriteFile(path, append(b, '\n'), 0644)
	if !matchesCodexCwd(path, "/home/alice/proj") {
		t.Error("expected match")
	}
}

func TestMatchesCodexCwd_TrailingSlashTolerant(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	meta := map[string]interface{}{
		"type":    "session_meta",
		"payload": map[string]interface{}{"cwd": "/home/alice/proj/"},
	}
	b, _ := json.Marshal(meta)
	_ = os.WriteFile(path, append(b, '\n'), 0644)
	// Match against the trailing-slash-stripped target.
	if !matchesCodexCwd(path, "/home/alice/proj") {
		t.Error("trailing slash on rollout's cwd should still match")
	}
}

func TestMatchesCodexCwd_WrongType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	meta := map[string]interface{}{
		"type":    "user_message",
		"payload": map[string]interface{}{"cwd": "/home/alice/proj"},
	}
	b, _ := json.Marshal(meta)
	_ = os.WriteFile(path, append(b, '\n'), 0644)
	if matchesCodexCwd(path, "/home/alice/proj") {
		t.Error("non-session_meta first line must not match")
	}
}

func TestMatchesCodexCwd_BadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	_ = os.WriteFile(path, []byte("not json\n"), 0644)
	if matchesCodexCwd(path, "/x") {
		t.Error("bad JSON must not match")
	}
}

func TestMatchesCodexCwd_MissingFile(t *testing.T) {
	if matchesCodexCwd("/nope.jsonl", "/x") {
		t.Error("missing file must not match")
	}
}

