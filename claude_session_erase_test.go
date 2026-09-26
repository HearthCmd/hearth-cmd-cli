//go:build darwin || linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func eraseTestWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func eraseTestExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readClaudeJSONAt(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Destroying a temp agent erases what Claude Code kept about its directory —
// transcripts, per-session backups, prompt history, settings entry — and
// nothing belonging to any other directory.
func TestEraseClaudeSessionState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude")
	cwd := "/home/u/hearth_agents/house/temp/ab12cd34"
	other := "/home/u/code/project"
	sid := "11111111-2222-4333-8444-555555555555"
	otherSid := "99999999-2222-4333-8444-555555555555"

	eraseTestWrite(t, filepath.Join(claude, "projects", sanitizeClaudeProjectHash(cwd), sid+".jsonl"), `{"type":"user"}`)
	eraseTestWrite(t, filepath.Join(claude, "file-history", sid, "backup"), "old file")
	eraseTestWrite(t, filepath.Join(claude, "session-env", sid, "env"), "x")
	eraseTestWrite(t, filepath.Join(claude, "projects", sanitizeClaudeProjectHash(other), otherSid+".jsonl"), `{"type":"user"}`)
	eraseTestWrite(t, filepath.Join(claude, "file-history", otherSid, "backup"), "keep")
	eraseTestWrite(t, filepath.Join(claude, "history.jsonl"),
		`{"display":"what's the cheapest VRAM?","project":"`+cwd+`","sessionId":"`+sid+`"}`+"\n"+
			`{"display":"keep me","project":"`+other+`","sessionId":"`+otherSid+`"}`+"\n"+
			"not json, kept\n")
	eraseTestWrite(t, filepath.Join(home, ".claude.json"),
		`{"numStartups": 3, "projects": {"`+cwd+`": {"hasTrustDialogAccepted": true}, "`+other+`": {"hasTrustDialogAccepted": true}}}`)

	eraseClaudeSessionState(cwd)

	for _, gone := range []string{
		filepath.Join(claude, "projects", sanitizeClaudeProjectHash(cwd)),
		filepath.Join(claude, "file-history", sid),
		filepath.Join(claude, "session-env", sid),
	} {
		if eraseTestExists(gone) {
			t.Errorf("still there: %s", gone)
		}
	}
	for _, kept := range []string{
		filepath.Join(claude, "projects", sanitizeClaudeProjectHash(other), otherSid+".jsonl"),
		filepath.Join(claude, "file-history", otherSid, "backup"),
	} {
		if !eraseTestExists(kept) {
			t.Errorf("another project's state was removed: %s", kept)
		}
	}
	hist, _ := os.ReadFile(filepath.Join(claude, "history.jsonl"))
	if strings.Contains(string(hist), "cheapest VRAM") || !strings.Contains(string(hist), "keep me") || !strings.Contains(string(hist), "not json, kept") {
		t.Errorf("history.jsonl = %q", hist)
	}
	cfg := readClaudeJSONAt(t, filepath.Join(home, ".claude.json"))
	projects := cfg["projects"].(map[string]any)
	if _, ok := projects[cwd]; ok {
		t.Error("~/.claude.json still has the erased directory")
	}
	if _, ok := projects[other]; !ok || cfg["numStartups"] != float64(3) {
		t.Errorf("~/.claude.json lost unrelated data: %v", cfg)
	}
}

// Refuses paths it could never have created a session for.
func TestEraseClaudeSessionState_IgnoresUnsafePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	keep := filepath.Join(home, ".claude", "projects", "-", "x.jsonl")
	eraseTestWrite(t, keep, "x")
	for _, p := range []string{"", "relative/dir", "/"} {
		eraseClaudeSessionState(p)
	}
	if !eraseTestExists(keep) {
		t.Fatal("an unsafe path erased something")
	}
}
