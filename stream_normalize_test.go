//go:build darwin || linux

package main

// Coverage for the per-harness normalization helpers in stream.go
// that don't already have tests in stream_test.go. These convert each
// harness's native tool-call shape into the Claude tool-name+args
// format the bridge re-emits. Wrong answers here render as the wrong
// tool icon in the UI, mis-pattern permission rules, or — worst — let
// a tool name collision auto-allow the wrong action class.

import (
	"reflect"
	"strings"
	"testing"
)

// ---------- flattenContent ----------

func TestFlattenContent_String(t *testing.T) {
	if got := flattenContent([]byte(`"hello"`)); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

func TestFlattenContent_BlockArray(t *testing.T) {
	got := flattenContent([]byte(`[{"type":"text","text":"hi "},{"type":"text","text":"there"}]`))
	if got != "hi there" {
		t.Errorf("got %q, want 'hi there'", got)
	}
}

func TestFlattenContent_EmptyAndJunk(t *testing.T) {
	if got := flattenContent(nil); got != "" {
		t.Errorf("nil should yield empty, got %q", got)
	}
	if got := flattenContent([]byte(``)); got != "" {
		t.Errorf("empty should yield empty, got %q", got)
	}
	if got := flattenContent([]byte(`{"not":"valid"}`)); got != "" {
		t.Errorf("non-string non-array should yield empty, got %q", got)
	}
}

// ---------- isSlashCommand ----------

func TestIsSlashCommand_LocalCommandSubtype(t *testing.T) {
	if !isSlashCommand(`{"type":"user","subtype":"local_command"}`) {
		t.Error("local_command subtype should match")
	}
}

func TestIsSlashCommand_CommandNameTag(t *testing.T) {
	if !isSlashCommand(`{"type":"user","message":{"content":"<command-name>plan</command-name>"}}`) {
		t.Error("<command-name> prefix should match")
	}
}

func TestIsSlashCommand_LocalCommandCaveat(t *testing.T) {
	if !isSlashCommand(`{"type":"user","message":{"content":"<local-command-caveat>foo"}}`) {
		t.Error("<local-command-caveat> prefix should match")
	}
}

func TestIsSlashCommand_RegularUserMessage(t *testing.T) {
	if isSlashCommand(`{"type":"user","message":{"content":"normal text"}}`) {
		t.Error("regular text should not be flagged as slash command")
	}
}

func TestIsSlashCommand_NotJSON(t *testing.T) {
	if isSlashCommand("not json") {
		t.Error("invalid JSON should not match")
	}
}

// ---------- normalizeGeminiToolName / Args ----------

func TestNormalizeGeminiToolName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"read_file", "Read"},
		{"write_file", "Write"},
		{"replace", "Edit"},
		{"run_shell_command", "Bash"},
		{"grep_search", "Grep"},
		{"list_directory", "Bash"},
		{"web_fetch", "WebFetch"},
		{"google_web_search", "WebSearch"},
		{"get_internal_docs", "Read"},
		{"unknown_thing", "unknown_thing"}, // pass-through
	}
	for _, c := range cases {
		if got := normalizeGeminiToolName(c.in); got != c.want {
			t.Errorf("normalizeGeminiToolName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeGeminiToolArgs_RunShellCommand(t *testing.T) {
	got := normalizeGeminiToolArgs("run_shell_command", map[string]interface{}{
		"command":     "ls -la",
		"description": "list files", // dropped — Claude's Bash doesn't use it
	})
	want := map[string]interface{}{"command": "ls -la"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNormalizeGeminiToolArgs_ListDirectory(t *testing.T) {
	got := normalizeGeminiToolArgs("list_directory", map[string]interface{}{
		"dir_path": "/tmp",
	})
	want := map[string]interface{}{"command": "ls /tmp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNormalizeGeminiToolArgs_PassThrough(t *testing.T) {
	got := normalizeGeminiToolArgs("read_file", map[string]interface{}{
		"file_path": "/tmp/x",
	})
	if got["file_path"] != "/tmp/x" {
		t.Errorf("read_file args should pass through, got %v", got)
	}
}

// ---------- normalizeCopilotToolName / Args ----------

func TestNormalizeCopilotToolName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"bash", "Bash"},
		{"edit", "Edit"},
		{"view", "Read"},
		{"create", "Write"},
		{"unknown", "unknown"}, // pass-through
	}
	for _, c := range cases {
		if got := normalizeCopilotToolName(c.in); got != c.want {
			t.Errorf("normalizeCopilotToolName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeCopilotToolArgs_RenamesPaths(t *testing.T) {
	got := normalizeCopilotToolArgs("Edit", map[string]interface{}{
		"path":     "/tmp/x",
		"old_str":  "a",
		"new_str":  "b",
		"file_text": "ignored-by-edit-but-still-renamed",
		"other":    "kept",
	})
	if got["file_path"] != "/tmp/x" {
		t.Errorf("path should rename to file_path, got %v", got)
	}
	if got["old_string"] != "a" {
		t.Errorf("old_str should rename to old_string, got %v", got)
	}
	if got["new_string"] != "b" {
		t.Errorf("new_str should rename to new_string, got %v", got)
	}
	if got["content"] != "ignored-by-edit-but-still-renamed" {
		t.Errorf("file_text should rename to content, got %v", got)
	}
	if got["other"] != "kept" {
		t.Error("unknown keys should pass through")
	}
	// Original keys must be gone.
	for _, k := range []string{"path", "old_str", "new_str", "file_text"} {
		if _, present := got[k]; present {
			t.Errorf("original key %q must be replaced, got %v", k, got)
		}
	}
}

// ---------- envelopeTimestamp ----------

func TestEnvelopeTimestamp_HappyPath(t *testing.T) {
	text := `hearth/1 {"from":{"id":"u","name":"Alice"},"mid":"m","ts":"2026-05-05T12:00:00Z"}` + "\n\nhello"
	if got := envelopeTimestamp(text); got != "2026-05-05T12:00:00Z" {
		t.Errorf("got %q, want timestamp", got)
	}
}

func TestEnvelopeTimestamp_NoEnvelope(t *testing.T) {
	if got := envelopeTimestamp("just plain text"); got != "" {
		t.Errorf("plain text should yield empty, got %q", got)
	}
}

func TestEnvelopeTimestamp_NoNewline(t *testing.T) {
	if got := envelopeTimestamp("hearth/1 {}"); got != "" {
		t.Errorf("missing newline should yield empty, got %q", got)
	}
}

func TestEnvelopeTimestamp_BadJSON(t *testing.T) {
	if got := envelopeTimestamp("hearth/1 {not json}\n\nhello"); got != "" {
		t.Errorf("bad JSON should yield empty, got %q", got)
	}
}

// ---------- extractCodexPatch ----------

func TestExtractCodexPatch_HappyPath(t *testing.T) {
	patch := "*** Begin Patch\n-foo\n+bar\n*** End Patch"
	old, new := extractCodexPatch(patch)
	if !strings.Contains(old, "foo") {
		t.Errorf("old lost: %q", old)
	}
	if !strings.Contains(new, "bar") {
		t.Errorf("new lost: %q", new)
	}
}

func TestExtractCodexPatch_OnlyAdditions(t *testing.T) {
	patch := "*** Begin Patch\n+only added\n*** End Patch"
	old, new := extractCodexPatch(patch)
	if old != "" {
		t.Errorf("expected empty old, got %q", old)
	}
	if !strings.Contains(new, "only added") {
		t.Errorf("expected 'only added', got %q", new)
	}
}

// ---------- normalizeCodexTool ----------

func TestNormalizeCodexTool_ExecCommand(t *testing.T) {
	tool, args := normalizeCodexTool("exec_command", `{"cmd":"ls -la"}`)
	if tool != "Bash" {
		t.Errorf("expected Bash, got %q", tool)
	}
	m := args.(map[string]string)
	if m["command"] != "ls -la" {
		t.Errorf("expected command=ls -la, got %v", m)
	}
}

func TestNormalizeCodexTool_ApplyPatchUpdate(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: foo/bar.go\n-old\n+new\n*** End Patch"
	tool, args := normalizeCodexTool("apply_patch", `{"patch":`+jsonString(patch)+`}`)
	if tool != "Edit" {
		t.Errorf("expected Edit, got %q", tool)
	}
	m := args.(map[string]interface{})
	if m["file_path"] != "foo/bar.go" {
		t.Errorf("file_path lost: %v", m)
	}
	if !strings.Contains(m["old_string"].(string), "old") || !strings.Contains(m["new_string"].(string), "new") {
		t.Errorf("old/new strings lost: %v", m)
	}
}

func TestNormalizeCodexTool_ApplyPatchAdd(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: new.go\n+content\n*** End Patch"
	tool, args := normalizeCodexTool("apply_patch", `{"patch":`+jsonString(patch)+`}`)
	if tool != "Edit" {
		t.Errorf("expected Edit, got %q", tool)
	}
	m := args.(map[string]interface{})
	if m["file_path"] != "new.go" {
		t.Errorf("Add File path lost: %v", m)
	}
}

func TestNormalizeCodexTool_UnknownPassThrough(t *testing.T) {
	tool, args := normalizeCodexTool("custom_tool", `{"k":"v"}`)
	if tool != "custom_tool" {
		t.Errorf("expected pass-through name, got %q", tool)
	}
	m, ok := args.(map[string]interface{})
	if !ok || m["k"] != "v" {
		t.Errorf("expected parsed JSON pass-through, got %v", args)
	}
}

func TestNormalizeCodexTool_UnknownEmptyArgs(t *testing.T) {
	tool, args := normalizeCodexTool("custom_tool", "")
	if tool != "custom_tool" {
		t.Errorf("expected pass-through name, got %q", tool)
	}
	if _, ok := args.(map[string]string); !ok {
		t.Errorf("expected empty map fallback, got %T", args)
	}
}

// helper — json-quote a raw string.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
