//go:build darwin || linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Coverage for the "newest-on-disk" transcript resolvers in agent.go
// — the no-session-ID variants used as a fallback when the daemon
// can't (or doesn't yet) know the session UUID. Each harness has its
// own directory layout; we stand up a fake home with the expected
// shape, write multiple .jsonl files with staggered mtimes, and
// verify the resolver picks the most recent one.

func writeWithMtime(t *testing.T, path string, content string, mt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// ---------- deriveClaudeTranscriptPath ----------

func TestDeriveClaudeTranscriptPath_PicksNewest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/Users/alice/proj"
	projDir := filepath.Join(home, ".claude", "projects", "-Users-alice-proj")

	now := time.Now()
	writeWithMtime(t, filepath.Join(projDir, "old.jsonl"), "{}", now.Add(-1*time.Hour))
	writeWithMtime(t, filepath.Join(projDir, "newest.jsonl"), "{}", now)
	writeWithMtime(t, filepath.Join(projDir, "middle.jsonl"), "{}", now.Add(-30*time.Minute))

	got := deriveClaudeTranscriptPath(cwd)
	want := filepath.Join(projDir, "newest.jsonl")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDeriveClaudeTranscriptPath_IgnoresNonJSONLAndDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/x"
	projDir := filepath.Join(home, ".claude", "projects", "-x")
	now := time.Now()
	// A non-JSONL file with a newer mtime must not win.
	writeWithMtime(t, filepath.Join(projDir, "README.md"), "hi", now)
	writeWithMtime(t, filepath.Join(projDir, "a.jsonl"), "{}", now.Add(-time.Hour))
	if err := os.Mkdir(filepath.Join(projDir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := deriveClaudeTranscriptPath(cwd)
	want := filepath.Join(projDir, "a.jsonl")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDeriveClaudeTranscriptPath_MissingProjectReturnsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := deriveClaudeTranscriptPath("/never/created"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// ---------- deriveCopilotTranscriptPath / ByID ----------

func TestDeriveCopilotTranscriptPathByID_HomeOverride(t *testing.T) {
	t.Setenv("COPILOT_HOME", "/fake-copilot")
	got := deriveCopilotTranscriptPathByID("sess-1")
	want := filepath.Join("/fake-copilot", "session-state", "sess-1", "events.jsonl")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDeriveCopilotTranscriptPathByID_DefaultHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COPILOT_HOME", "")
	t.Setenv("HOME", home)
	got := deriveCopilotTranscriptPathByID("sess-2")
	want := filepath.Join(home, ".copilot", "session-state", "sess-2", "events.jsonl")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDeriveCopilotTranscriptPath_PicksNewestSessionDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("COPILOT_HOME", root)
	stateDir := filepath.Join(root, "session-state")
	now := time.Now()
	for _, c := range []struct {
		name string
		mt   time.Time
	}{
		{"old", now.Add(-2 * time.Hour)},
		{"newest", now},
		{"mid", now.Add(-time.Hour)},
	} {
		dir := filepath.Join(stateDir, c.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Create the events.jsonl inside (so the path is real even if
		// the resolver only goes off the dir mtime).
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, c.mt, c.mt); err != nil {
			t.Fatal(err)
		}
	}
	got := deriveCopilotTranscriptPath()
	want := filepath.Join(stateDir, "newest", "events.jsonl")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDeriveCopilotTranscriptPath_MissingStateDirReturnsEmpty(t *testing.T) {
	t.Setenv("COPILOT_HOME", t.TempDir()) // dir exists but session-state subdir doesn't
	if got := deriveCopilotTranscriptPath(); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestDeriveCopilotTranscriptPath_IgnoresFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("COPILOT_HOME", root)
	stateDir := filepath.Join(root, "session-state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Stray file in session-state — must NOT be selected.
	if err := os.WriteFile(filepath.Join(stateDir, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := deriveCopilotTranscriptPath(); got != "" {
		t.Errorf("expected empty when only files present, got %q", got)
	}
}

// ---------- deriveGeminiTranscriptPath / ByID ----------

func TestDeriveGeminiTranscriptPath_PicksNewest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/whatever/proj"
	chats := filepath.Join(home, ".gemini", "tmp", "proj", "chats")
	now := time.Now()
	writeWithMtime(t, filepath.Join(chats, "old.jsonl"), "{}", now.Add(-time.Hour))
	writeWithMtime(t, filepath.Join(chats, "new.jsonl"), "{}", now)

	got := deriveGeminiTranscriptPath(cwd)
	want := filepath.Join(chats, "new.jsonl")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDeriveGeminiTranscriptPath_MissingChatsDirReturnsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := deriveGeminiTranscriptPath("/never/here"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestDeriveGeminiTranscriptPathByID_MatchesHeader(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/whatever/proj"
	chats := filepath.Join(home, ".gemini", "tmp", "proj", "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}

	mkSession := func(name, sid string) string {
		hdr := struct {
			SessionID string `json:"sessionId"`
			Kind      string `json:"kind"`
		}{SessionID: sid, Kind: "main"}
		b, _ := json.Marshal(hdr)
		path := filepath.Join(chats, name)
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	a := mkSession("a.jsonl", "sess-A")
	b := mkSession("b.jsonl", "sess-B")
	_ = a

	got := deriveGeminiTranscriptPathByID("sess-B", cwd)
	if got != b {
		t.Errorf("got %q, want %q", got, b)
	}
}

func TestDeriveGeminiTranscriptPathByID_NotFoundReturnsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/whatever/proj"
	chats := filepath.Join(home, ".gemini", "tmp", "proj", "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	hdr := `{"sessionId":"only-this","kind":"main"}` + "\n"
	if err := os.WriteFile(filepath.Join(chats, "x.jsonl"), []byte(hdr), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := deriveGeminiTranscriptPathByID("missing", cwd); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestDeriveGeminiTranscriptPathByID_MissingChatsDirReturnsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := deriveGeminiTranscriptPathByID("any", "/no/such"); got != "" {
		t.Errorf("got %q", got)
	}
}

// Gemini 0.40 sanitizes the cwd basename: every char that isn't an
// ASCII alnum or a dash becomes a dash. A cwd basename of
// "gemini_test_2" lands on disk under "gemini-test-2". This test
// pins that contract — break it and hearth lookups silently miss the
// actual transcript file.
func TestSanitizeGeminiProjectDir(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"gemini_test_2", "gemini-test-2"},
		{"already-dashed", "already-dashed"},
		{"with.dot", "with-dot"},
		{"has space", "has-space"},
		{"mix_of.things and-stuff", "mix-of-things-and-stuff"},
	}
	for _, c := range cases {
		// Pass as absolute path; sanitize operates on filepath.Base.
		got := sanitizeGeminiProjectDir("/some/parent/" + c.in)
		if got != c.want {
			t.Errorf("sanitizeGeminiProjectDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDeriveGeminiTranscriptPath_SanitizesUnderscoreCwd(t *testing.T) {
	// Regression for the gemini_test_2 vs gemini-test-2 miss observed on
	// the dev box: cwd basename contains underscores; gemini writes
	// under the dash-substituted dir.
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/home/hearth/hearth_agents/verge_labs/gemini_test_2"
	chats := filepath.Join(home, ".gemini", "tmp", "gemini-test-2", "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(chats, "session.jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := deriveGeminiTranscriptPath(cwd); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
