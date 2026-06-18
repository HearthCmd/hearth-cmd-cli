//go:build darwin || linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Pure helpers from daemon_history.go, daemon_agent.go, and the
// trimNewline utility in stream.go. Side-effecting functions
// (replayTranscriptHistory, spawnAgentInstance, etc.) need a daemon /
// WS fixture and stay out of scope here.

// ---------- runtimeAgentFromServerName ----------

func TestRuntimeAgentFromServerName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-code", "claude"},
		{"claude", "claude"},
		{"codex", "codex"},
		{"gemini", "gemini"},
		{"copilot", "copilot"},
		{"pi", "pi"},
		{"unknown-x", "unknown-x"}, // pass-through
		{"", ""},
	}
	for _, c := range cases {
		if got := runtimeAgentFromServerName(c.in); got != c.want {
			t.Errorf("runtimeAgentFromServerName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------- sessionIDForReplay ----------

func TestSessionIDForReplay_PassesThroughAgentSessionID(t *testing.T) {
	// sessionIDForReplay is the seam between the registered AgentWS's
	// stored session id and deriveTranscriptPath's by-id lookup. It
	// passes through unchanged for every runtime — the per-harness
	// "do I have a session id at all" question is answered by
	// SessionIDPolicy at register/discovery time, not here.
	for _, runtime := range []string{"claude", "codex", "gemini", "copilot", "pi", ""} {
		if got := sessionIDForReplay(runtime, "instance-1", "sess-abc"); got != "sess-abc" {
			t.Errorf("sessionIDForReplay(%q, _, sess-abc) = %q, want sess-abc", runtime, got)
		}
		if got := sessionIDForReplay(runtime, "instance-1", ""); got != "" {
			t.Errorf("sessionIDForReplay(%q, _, '') = %q, want empty", runtime, got)
		}
	}
}

// ---------- localAgentForHarness ----------

func TestLocalAgentForHarness(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-code", "claude"}, // server label → CLI runtime
		{"codex", "codex"},
		{"gemini", "gemini"},
		{"copilot", "copilot"},
		{"pi", "pi"},
		// Server-defined harnesses with no local CLI binary fall to "".
		{"windsurf", ""},
		{"openai-assistants", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := localAgentForHarness(c.in); got != c.want {
			t.Errorf("localAgentForHarness(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------- expandHome ----------

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct{ in, want string }{
		{"~", home},
		{"~/foo", filepath.Join(home, "foo")},
		{"~/a/b", filepath.Join(home, "a", "b")},
		{"/abs/path", "/abs/path"},
		{"relative", "relative"},
		{"~user/foo", "~user/foo"}, // only "~" or "~/" prefix expands
		{"", ""},
	}
	for _, c := range cases {
		if got := expandHome(c.in); got != c.want {
			t.Errorf("expandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------- classifyExit ----------

func TestClassifyExit_NilIsExited(t *testing.T) {
	if got := classifyExit(nil); got != "exited" {
		t.Errorf("got %q", got)
	}
}

func TestClassifyExit_NonExitErrorIsExited(t *testing.T) {
	if got := classifyExit(errors.New("plain error")); got != "exited" {
		t.Errorf("got %q", got)
	}
}

func TestClassifyExit_RealNonZeroExitIsExited(t *testing.T) {
	// `false` exits non-zero on every Unix; produces a real ExitError
	// with WaitStatus.Exited() == true.
	cmd := exec.Command("false")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected `false` to fail")
	}
	if got := classifyExit(err); got != "exited" {
		t.Errorf("got %q, want exited", got)
	}
}

func TestClassifyExit_KilledBySignalIsKilled(t *testing.T) {
	// Spawn `sleep 30` and SIGKILL it; the resulting WaitStatus has
	// Signaled() == true, so classifyExit must return "killed".
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	err := cmd.Wait()
	if err == nil {
		t.Fatal("expected non-nil err from killed process")
	}
	if got := classifyExit(err); got != "killed" {
		t.Errorf("got %q, want killed", got)
	}
}

// ---------- trimNewline (stream.go) ----------

func TestTrimNewline(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"hello", "hello"},
		{"hello\n", "hello"},
		{"hello\r\n", "hello"},
		{"hello\n\n\n", "hello"},
		{"\n", ""},
		{"\r\n\r\n", ""},
		{"mid\nline", "mid\nline"}, // only trailing newlines are trimmed
	}
	for _, c := range cases {
		if got := trimNewline(c.in); got != c.want {
			t.Errorf("trimNewline(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------- readLastNLines (daemon_history.go) ----------

func TestReadLastNLines_FewerThanN(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	if err := os.WriteFile(p, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readLastNLines(p, 10)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("got %v", got)
	}
}

func TestReadLastNLines_TruncatesToN(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	if err := os.WriteFile(p, []byte("1\n2\n3\n4\n5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readLastNLines(p, 3)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(got, ",") != "3,4,5" {
		t.Errorf("got %v, want last 3", got)
	}
}

func TestReadLastNLines_ExactlyN(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	if err := os.WriteFile(p, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readLastNLines(p, 3)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("got %v", got)
	}
}

func TestReadLastNLines_TrailingPartialLineIncluded(t *testing.T) {
	// Final line without a newline still counts — the reader sees it
	// before the EOF.
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	if err := os.WriteFile(p, []byte("a\nb\nno-newline"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readLastNLines(p, 5)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(got, ",") != "a,b,no-newline" {
		t.Errorf("got %v", got)
	}
}

func TestReadLastNLines_Empty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readLastNLines(p, 10)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestReadLastNLines_MissingFileReturnsErr(t *testing.T) {
	if _, err := readLastNLines("/no/such/file/here.log", 5); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestReadLastNLines_NonPositiveNReturnsNil(t *testing.T) {
	got, err := readLastNLines("/anything.log", 0)
	if err != nil {
		t.Errorf("n<=0 should return nil,nil; got err %v", err)
	}
	if got != nil {
		t.Errorf("expected nil slice, got %v", got)
	}
	got, err = readLastNLines("/anything.log", -3)
	if err != nil {
		t.Errorf("n<=0 should return nil,nil; got err %v", err)
	}
	if got != nil {
		t.Errorf("expected nil slice, got %v", got)
	}
}

// ---------- claudeStream (identity branch via SPI) ----------

func TestClaudeStream_Identity(t *testing.T) {
	h, ok := getHarness("claude")
	if !ok {
		t.Fatal("claude harness missing")
	}
	xform := h.NewStreamTransformer()
	got := xform.TransformLine(`{"type":"foo"}`)
	if len(got) != 1 || string(got[0]) != `{"type":"foo"}` {
		t.Errorf("expected identity, got %v", got)
	}
	if got := xform.TransformLine(""); got != nil {
		t.Errorf("empty in should produce nil, got %v", got)
	}
}
