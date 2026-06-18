//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Pure helpers in connect.go that don't need a live exec environment.
// resolveScriptCommand / resolveInterpreter inspect on-disk file headers
// (#!shebang) and only delegate to exec.LookPath for the *interpreter*,
// which is a real binary like /bin/sh that always exists on a CI box —
// so we drive them with t.TempDir() + a shebang file pointed at /bin/sh.

// withPathPrepended makes `dir` the first PATH entry for the duration of
// the test and restores the previous value via t.Cleanup.
func withPathPrepended(t *testing.T, dir string) {
	t.Helper()
	prev := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", prev) })
	os.Setenv("PATH", dir+string(os.PathListSeparator)+prev)
}

// writeExec writes a file with mode 0755 and returns its absolute path.
func writeExec(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile %s: %v", p, err)
	}
	return p
}

func TestGenerateUUID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		u := generateUUID()
		if !re.MatchString(u) {
			t.Fatalf("uuid %q does not match v4 pattern", u)
		}
		if seen[u] {
			t.Fatalf("duplicate uuid in 50 iters: %q", u)
		}
		seen[u] = true
	}
}

// ---------- resolveInterpreter ----------

func TestResolveInterpreter_PlainBinary(t *testing.T) {
	dir := t.TempDir()
	// Magic bytes that aren't "#!" — anything else short-circuits to
	// "treat as binary, return input unchanged."
	bin := writeExec(t, dir, "binary", "\x7fELF dummy\n")
	got, err := resolveInterpreter(bin)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != bin {
		t.Errorf("plain binary should pass through; got %q want %q", got, bin)
	}
}

func TestResolveInterpreter_ShebangResolvesToInterpreter(t *testing.T) {
	dir := t.TempDir()
	script := writeExec(t, dir, "script.sh", "#!/bin/sh\necho hi\n")
	got, err := resolveInterpreter(script)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// /bin/sh exists on every CI box we run on. Don't pin the absolute
	// path (some distros symlink it) — just verify we ended up at sh.
	if filepath.Base(got) != "sh" {
		t.Errorf("expected sh interpreter, got %q", got)
	}
}

func TestResolveInterpreter_EnvShebangSkipsFlags(t *testing.T) {
	dir := t.TempDir()
	// `/usr/bin/env -S sh -c` style — function should skip `-S` and
	// pick up `sh` as the interpreter.
	script := writeExec(t, dir, "script.sh", "#!/usr/bin/env -S sh\necho hi\n")
	got, err := resolveInterpreter(script)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if filepath.Base(got) != "sh" {
		t.Errorf("expected sh, got %q", got)
	}
}

func TestResolveInterpreter_MissingFileReturnsInput(t *testing.T) {
	got, err := resolveInterpreter("/definitely/does/not/exist")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "/definitely/does/not/exist" {
		t.Errorf("missing file should return input, got %q", got)
	}
}

// ---------- resolveScriptCommand ----------
// Note: resolveScriptCommand calls exec.LookPath on the *command name*,
// so we need to put a fake script onto PATH and refer to it by name.

func TestResolveScriptCommand_PlainBinaryUnchanged(t *testing.T) {
	dir := t.TempDir()
	// Random bytes — not "#!". Should bounce out and return inputs as-is.
	writeExec(t, dir, "tool", "\x00\x01binary\n")
	withPathPrepended(t, dir)

	cmd, args := resolveScriptCommand("tool", []string{"--flag"})
	if cmd != "tool" {
		t.Errorf("plain binary: cmd should be unchanged, got %q", cmd)
	}
	if len(args) != 1 || args[0] != "--flag" {
		t.Errorf("plain binary: args should be unchanged, got %v", args)
	}
}

func TestResolveScriptCommand_ShebangRewritesToInterpreter(t *testing.T) {
	dir := t.TempDir()
	scriptPath := writeExec(t, dir, "tool.sh", "#!/bin/sh\necho hi\n")
	withPathPrepended(t, dir)

	cmd, args := resolveScriptCommand("tool.sh", []string{"--flag", "x"})
	if filepath.Base(cmd) != "sh" {
		t.Errorf("expected interpreter sh, got %q", cmd)
	}
	// args = [interpreter flags...] [script path] [original args...]
	// /bin/sh has no flags here, so we expect [scriptPath, --flag, x].
	if len(args) < 3 {
		t.Fatalf("args should include script + originals, got %v", args)
	}
	if args[0] != scriptPath {
		t.Errorf("first arg should be the resolved script path, got %q", args[0])
	}
	if args[len(args)-2] != "--flag" || args[len(args)-1] != "x" {
		t.Errorf("original args should trail, got %v", args)
	}
}

func TestResolveScriptCommand_EnvShebangSkipsFlagsAndPropagatesNone(t *testing.T) {
	dir := t.TempDir()
	scriptPath := writeExec(t, dir, "envtool", "#!/usr/bin/env -S sh\nexit 0\n")
	withPathPrepended(t, dir)

	cmd, args := resolveScriptCommand("envtool", []string{"a"})
	if filepath.Base(cmd) != "sh" {
		t.Errorf("expected sh, got %q", cmd)
	}
	// `-S` should be consumed by the env-skip loop, not surface as an
	// interpreter flag. Therefore the script path should be args[0].
	if args[0] != scriptPath {
		t.Errorf("expected script as args[0] (no -S leakage), got %v", args)
	}
	if !strings.Contains(strings.Join(args, " "), "a") {
		t.Errorf("original arg lost: %v", args)
	}
}

func TestResolveScriptCommand_MissingCommandUnchanged(t *testing.T) {
	cmd, args := resolveScriptCommand("definitely-not-a-real-binary-xyz", []string{"--x"})
	if cmd != "definitely-not-a-real-binary-xyz" {
		t.Errorf("missing command should pass through, got %q", cmd)
	}
	if len(args) != 1 || args[0] != "--x" {
		t.Errorf("args should pass through, got %v", args)
	}
}

// ---------- titleCase (organization.go) ----------

func TestTitleCase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"a", "A"},
		{"hello", "Hello"},
		{"Already", "Already"}, // already capitalized
		{"hELLO", "HELLO"},     // only first byte changes
	}
	for _, c := range cases {
		if got := titleCase(c.in); got != c.want {
			t.Errorf("titleCase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
