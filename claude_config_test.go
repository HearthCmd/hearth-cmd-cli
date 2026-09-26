//go:build darwin || linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readClaudeJSON(t *testing.T, home string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Before each Claude launch the daemon trusts the working directory and
// switches off Claude Code's daily "your closed GitHub issues" check — the
// `gh issue list -R anthropics/claude-code --author @me …` every agent asked a
// person to approve after a fleet restart. One write, other fields untouched.
func TestPreAcceptClaudeTrust_TrustsAndTurnsOffClosedIssuesCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	existing := `{"numStartups": 7, "closedIssuesLastChecked": 1790392429228,
		"projects": {"/other": {"hasTrustDialogAccepted": true, "allowedTools": ["x"]}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	preAcceptClaudeTrust("/work/agent")
	m := readClaudeJSON(t, home)
	if got, _ := m["closedIssuesLastChecked"].(float64); got != claudeClosedIssuesCheckOff {
		t.Fatalf("closedIssuesLastChecked = %v, want %v", m["closedIssuesLastChecked"], claudeClosedIssuesCheckOff)
	}
	projects := m["projects"].(map[string]any)
	if projects["/work/agent"].(map[string]any)["hasTrustDialogAccepted"] != true {
		t.Fatal("working directory not trusted")
	}
	if m["numStartups"] != float64(7) || projects["/other"].(map[string]any)["allowedTools"] == nil {
		t.Fatalf("unrelated fields not preserved: %v", m)
	}

	// Already done: nothing to change, so no rewrite.
	info, _ := os.Stat(filepath.Join(home, ".claude.json"))
	preAcceptClaudeTrust("/work/agent")
	after, _ := os.Stat(filepath.Join(home, ".claude.json"))
	if !after.ModTime().Equal(info.ModTime()) || after.Size() != info.Size() {
		t.Fatal("rewrote an already-prepared config")
	}
}

// A trusted directory from before this change still gets the check turned off.
func TestPreAcceptClaudeTrust_TurnsOffCheckForAlreadyTrustedDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	existing := `{"projects": {"/work/agent": {"hasTrustDialogAccepted": true}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	preAcceptClaudeTrust("/work/agent")
	if got, _ := readClaudeJSON(t, home)["closedIssuesLastChecked"].(float64); got != claudeClosedIssuesCheckOff {
		t.Fatalf("check not turned off for an already-trusted directory: %v", got)
	}
}

// A config that isn't valid JSON is left exactly as it is.
func TestPreAcceptClaudeTrust_LeavesInvalidJSONAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	preAcceptClaudeTrust("/work/agent")
	if b, _ := os.ReadFile(path); string(b) != "{not json" {
		t.Fatalf("invalid config was rewritten: %q", b)
	}
}
