//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// settings.go installs/removes per-agent instruction files. Each agent
// gets a different on-disk layout (gemini → GEMINI.md at root, copilot
// → .github/copilot-instructions.md, codex → AGENTS.md with a
// per-instance sentinel). Behavior we lock in here:
//   - install creates the directory tree the agent expects
//   - codex file embeds the ai_agent_instance_id sentinel
//   - existing user-authored file is NOT overwritten
//   - existing hearth-marked file IS overwritten
//   - removeHearthInstructions only deletes hearth-marked files
//   - unknown agent is a no-op (returns nil)

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestInstallHearthInstructions_Gemini(t *testing.T) {
	cwd := t.TempDir()
	if err := installHearthInstructions("gemini", "", "", cwd); err != nil {
		t.Fatalf("err: %v", err)
	}
	body := readFile(t, filepath.Join(cwd, "GEMINI.md"))
	if !strings.Contains(body, "<!-- hearth -->") {
		t.Error("missing hearth marker")
	}
}

func TestInstallHearthInstructions_Copilot_CreatesDotGithub(t *testing.T) {
	cwd := t.TempDir()
	if err := installHearthInstructions("copilot", "", "", cwd); err != nil {
		t.Fatalf("err: %v", err)
	}
	target := filepath.Join(cwd, ".github", "copilot-instructions.md")
	body := readFile(t, target)
	if !strings.Contains(body, "<!-- hearth -->") {
		t.Error("missing hearth marker")
	}
}

func TestInstallHearthInstructions_Codex_EmbedsInstanceSentinel(t *testing.T) {
	cwd := t.TempDir()
	if err := installHearthInstructions("codex", "instance-xyz", "", cwd); err != nil {
		t.Fatalf("err: %v", err)
	}
	body := readFile(t, filepath.Join(cwd, "AGENTS.md"))
	if !strings.Contains(body, "<!-- hearth-agent-instance:instance-xyz -->") {
		t.Errorf("missing instance sentinel in body: %q", body)
	}
}

func TestInstallHearthInstructions_Codex_OmitsSentinelWhenIDEmpty(t *testing.T) {
	cwd := t.TempDir()
	if err := installHearthInstructions("codex", "", "", cwd); err != nil {
		t.Fatalf("err: %v", err)
	}
	body := readFile(t, filepath.Join(cwd, "AGENTS.md"))
	if strings.Contains(body, "hearth-agent-instance:") {
		t.Errorf("empty ID should suppress sentinel, body=%q", body)
	}
}

func TestInstallHearthInstructions_EmbedsIdentityPrompt(t *testing.T) {
	cwd := t.TempDir()
	identity := "Identity: You are Cody."
	if err := installHearthInstructions("gemini", "", identity, cwd); err != nil {
		t.Fatalf("err: %v", err)
	}
	body := readFile(t, filepath.Join(cwd, "GEMINI.md"))
	if !strings.Contains(body, identity) {
		t.Errorf("identity prompt missing from body: %q", body)
	}
}

func TestInstallHearthInstructions_PreservesUserFile(t *testing.T) {
	cwd := t.TempDir()
	target := filepath.Join(cwd, "GEMINI.md")
	original := "# my own notes\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installHearthInstructions("gemini", "", "", cwd); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := readFile(t, target); got != original {
		t.Errorf("user file was overwritten; got %q", got)
	}
}

func TestInstallHearthInstructions_OverwritesOwnFile(t *testing.T) {
	cwd := t.TempDir()
	target := filepath.Join(cwd, "GEMINI.md")
	// Pre-existing file that DOES carry our marker — we own it, so
	// re-installing should overwrite it.
	if err := os.WriteFile(target, []byte("<!-- hearth -->\nold content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installHearthInstructions("gemini", "", "freshIdentity", cwd); err != nil {
		t.Fatalf("err: %v", err)
	}
	got := readFile(t, target)
	if !strings.Contains(got, "freshIdentity") {
		t.Errorf("hearth file should have been overwritten with fresh content, got %q", got)
	}
	if strings.Contains(got, "old content") {
		t.Errorf("old content survived overwrite: %q", got)
	}
}

func TestInstallHearthInstructions_UnknownAgentIsNoop(t *testing.T) {
	cwd := t.TempDir()
	if err := installHearthInstructions("totally-unknown", "", "", cwd); err != nil {
		t.Fatalf("err: %v", err)
	}
	entries, _ := os.ReadDir(cwd)
	if len(entries) != 0 {
		t.Errorf("unknown agent should not write anything; got %d entries", len(entries))
	}
}

func TestRemoveHearthInstructions_OnlyHearthMarked(t *testing.T) {
	dir := t.TempDir()

	// User file (no marker) — must NOT be deleted.
	user := filepath.Join(dir, "USER.md")
	if err := os.WriteFile(user, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Hearth file — should be deleted.
	hearth := filepath.Join(dir, "HEARTH.md")
	if err := os.WriteFile(hearth, []byte("<!-- hearth -->\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removeHearthInstructions(user)
	if _, err := os.Stat(user); err != nil {
		t.Errorf("user file should still exist, stat err: %v", err)
	}

	removeHearthInstructions(hearth)
	if _, err := os.Stat(hearth); !os.IsNotExist(err) {
		t.Errorf("hearth file should be removed, stat err: %v", err)
	}
}

func TestRemoveHearthInstructions_MissingFileIsSilent(t *testing.T) {
	// Should not panic / not return an error path (it's void anyway).
	removeHearthInstructions("/nonexistent/path/whatever.md")
}

func TestIsHearthInstructionFile(t *testing.T) {
	if !isHearthInstructionFile("<!-- hearth -->\nfoo") {
		t.Error("expected match")
	}
	if isHearthInstructionFile("# user file\n") {
		t.Error("plain file should not match")
	}
	if isHearthInstructionFile("") {
		t.Error("empty file should not match")
	}
}

