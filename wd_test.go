//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Pure helpers in wd.go: defaultAgentWorkingDir, defaultAgentWorkingDirFor,
// toSnakeCase. The interactive prompt helpers / runWD itself are skipped —
// they require stdin and produce side-effects best exercised by an
// end-to-end harness.

func TestToSnakeCase(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Head Gardener", "head_gardener"},
		{"already_snake", "already_snake"},
		{"  spaces  ", "spaces"},
		{"With-dashes_AND/slashes", "with_dashes_and_slashes"},
		{"!!!Bang!!!", "bang"},
		{"123 numbers OK 456", "123_numbers_ok_456"},
		{"", ""},
		{"___", ""},
		{"AAA", "aaa"},
		{"camelCase", "camelcase"}, // toSnakeCase only collapses non-alnum, doesn't insert at case boundaries
	}
	for _, tc := range cases {
		if got := toSnakeCase(tc.in); got != tc.want {
			t.Errorf("toSnakeCase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDefaultAgentWorkingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := defaultAgentWorkingDir("acme"); got != filepath.Join(home, "hearth_agents", "acme") {
		t.Errorf("with org slug: got %q", got)
	}
	if got := defaultAgentWorkingDir(""); got != filepath.Join(home, "hearth_agents") {
		t.Errorf("empty slug: got %q", got)
	}
	// The function does NOT create the directory — verify that contract.
	if _, err := os.Stat(filepath.Join(home, "hearth_agents")); !os.IsNotExist(err) {
		t.Errorf("hearth_agents should not be created by defaultAgentWorkingDir; stat err=%v", err)
	}
}

func TestDefaultAgentWorkingDirFor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := defaultAgentWorkingDirFor("acme", "Head Gardener")
	want := filepath.Join(home, "hearth_agents", "acme", "full_time", "head_gardener")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Empty position name falls back to the org-only base.
	got = defaultAgentWorkingDirFor("acme", "")
	want = filepath.Join(home, "hearth_agents", "acme")
	if got != want {
		t.Errorf("empty position: got %q, want %q", got, want)
	}

	// Position made entirely of separators snake-cases to "" → also base.
	got = defaultAgentWorkingDirFor("acme", "___")
	want = filepath.Join(home, "hearth_agents", "acme")
	if got != want {
		t.Errorf("separator-only position: got %q, want %q", got, want)
	}
}
