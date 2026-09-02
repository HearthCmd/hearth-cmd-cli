//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scope must contain no dash: Claude's directory is "<source>-<scope>" and
// the reconcile pass recovers the scope by stripping the "<source>-" prefix, so
// a dash inside the slug would make that ambiguous.
func TestCatalogSkillScope(t *testing.T) {
	got := catalogSkillScope("verge_labs/music-rooms")
	if strings.Contains(strings.TrimPrefix(got, catalogSkillScopePrefix), "-") {
		t.Fatalf("scope %q contains a dash", got)
	}
	if !strings.HasPrefix(got, catalogSkillScopePrefix) {
		t.Fatalf("scope %q lacks the catalog prefix", got)
	}
	// Distinct slugs must not collide — the inline harnesses key their marker on
	// the scope alone.
	if catalogSkillScope("a/b") == catalogSkillScope("a/c") {
		t.Fatal("distinct slugs produced the same scope")
	}
}

// No skill is worth failing a spawn over: an agent with none works, it just
// knows less.
func TestParseCatalogSkills(t *testing.T) {
	ok := parseCatalogSkills(`[{"slug":"a/b","version":"1","content":"# hi"}]`)
	if len(ok) != 1 || ok[0].Slug != "a/b" || ok[0].Content != "# hi" {
		t.Fatalf("parsed = %+v", ok)
	}
	for _, junk := range []string{"", "   ", "not json", "{}", "[", `[{"slug":""}]`} {
		if got := parseCatalogSkills(junk); len(got) != 0 {
			t.Fatalf("parseCatalogSkills(%q) = %+v, want none", junk, got)
		}
	}
	// A skill with no body is dropped rather than installed empty.
	if got := parseCatalogSkills(`[{"slug":"a/b","content":"  "}]`); len(got) != 0 {
		t.Fatalf("kept an empty skill: %+v", got)
	}
}

// The reconcile pass is what makes revoking a skill actually take it off the
// host — the daemon cannot iterate catalog skills it no longer receives, so it
// sweeps by prefix instead.
func TestClaudeRemoveSkillsExcept(t *testing.T) {
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".claude", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := []string{
		catalogSkillSource + "-" + catalogSkillScope("a/keep"),
		catalogSkillSource + "-" + catalogSkillScope("a/drop"),
		"github-github_work", // a plugin skill: must survive
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	h := claudeHarness{}
	ctx := HarnessCtx{Cwd: cwd}
	if err := h.RemoveSkillsExcept(ctx, catalogSkillSource, catalogSkillScopePrefix,
		[]string{catalogSkillScope("a/keep")}); err != nil {
		t.Fatalf("RemoveSkillsExcept: %v", err)
	}

	left := map[string]bool{}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		left[e.Name()] = true
	}
	if !left[dirs[0]] {
		t.Fatalf("removed a skill that was in keep: %v", left)
	}
	if left[dirs[1]] {
		t.Fatalf("kept a revoked skill: %v", left)
	}
	// The whole point of scoping by prefix: plugin skills are not ours to touch.
	if !left["github-github_work"] {
		t.Fatalf("swept away a plugin skill: %v", left)
	}
}

// Safe on a cwd that has never had a skill installed.
func TestClaudeRemoveSkillsExceptNoDirectory(t *testing.T) {
	if err := (claudeHarness{}).RemoveSkillsExcept(
		HarnessCtx{Cwd: t.TempDir()}, catalogSkillSource, catalogSkillScopePrefix, nil); err != nil {
		t.Fatalf("RemoveSkillsExcept on a fresh cwd: %v", err)
	}
}

// Round trip through Claude's installer, which is the harness that matters most.
func TestClaudeInstallThenReconcileCatalogSkill(t *testing.T) {
	cwd := t.TempDir()
	h := claudeHarness{}
	ctx := HarnessCtx{Cwd: cwd}

	scope := catalogSkillScope("verge_labs/music_rooms")
	if err := h.InstallSkill(ctx, scope, catalogSkillSource, []byte("# Rooms\n")); err != nil {
		t.Fatalf("InstallSkill: %v", err)
	}
	path := filepath.Join(cwd, ".claude", "skills", catalogSkillSource+"-"+scope, "SKILL.md")
	if b, err := os.ReadFile(path); err != nil || string(b) != "# Rooms\n" {
		t.Fatalf("installed content = %q (err %v)", b, err)
	}

	// Now it is no longer bound: the sweep removes it.
	if err := h.RemoveSkillsExcept(ctx, catalogSkillSource, catalogSkillScopePrefix, nil); err != nil {
		t.Fatalf("RemoveSkillsExcept: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("skill survived the sweep (stat err = %v)", err)
	}
}
