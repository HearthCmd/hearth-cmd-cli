package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestRegistryLoad_Basic(t *testing.T) {
	r := NewPluginRegistry()
	buf := captureLog(t)

	if err := r.Load("testdata/plugins/basic"); err != nil {
		t.Fatalf("Load: %v", err)
	}

	list := r.ListPlugins()
	if len(list) != 2 {
		t.Fatalf("ListPlugins len = %d; want 2", len(list))
	}
	// Sorted alphabetically by install subpath.
	if list[0].PluginSlug != "gdrive" || list[1].PluginSlug != "ha" {
		t.Errorf("unexpected order: %s, %s", list[0].PluginSlug, list[1].PluginSlug)
	}

	ha, ok := r.GetPluginBySlug("ha")
	if !ok {
		t.Fatal("ha not registered")
	}
	if ha.PluginSlug != "ha" || ha.DisplayName != "Home Assistant" {
		t.Errorf("ha manifest fields wrong: %+v", ha)
	}
	if !filepath.IsAbs(ha.SourceDir) {
		t.Errorf("SourceDir not absolute: %q", ha.SourceDir)
	}
	if !strings.HasSuffix(ha.SourceDir, "testdata/plugins/basic/ha") {
		t.Errorf("SourceDir tail wrong: %q", ha.SourceDir)
	}

	if !strings.Contains(buf.String(), "loaded 2 plugins") {
		t.Errorf("summary log not emitted: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "gdrive, ha") {
		t.Errorf("summary log missing sorted install subpaths: %q", buf.String())
	}
}

func TestRegistryLoad_FailsOnMalformed(t *testing.T) {
	// Seed the registry with a good load first so we can confirm the
	// failed load leaves the previous contents intact (atomic swap).
	r := NewPluginRegistry()
	captureLog(t)
	if err := r.Load("testdata/plugins/basic"); err != nil {
		t.Fatalf("seed Load: %v", err)
	}
	prev := r.ListPlugins()

	err := r.Load("testdata/plugins/mixed")
	if err == nil {
		t.Fatal("Load should return an error when a manifest is malformed")
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("error should name the offending plugin; got: %v", err)
	}

	// Registry should be unchanged — Load failed before swapping.
	after := r.ListPlugins()
	if len(after) != len(prev) {
		t.Errorf("registry changed after failed Load: had %d, now %d", len(prev), len(after))
	}
}

// TestRegistryLoad_RejectsDirSlugMismatch pins the invariant that
// an install directory's basename MUST equal its manifest's
// plugin_slug. The fixture has ha-home/ and ha-rental/ dirs whose
// manifests both declare plugin_slug=ha — that's the
// previously-allowed ha-home + ha-rental multi-install pattern,
// now forbidden (multi-instance lives at the resource_connections
// layer, not here). A slug mismatch is a hard error.
func TestRegistryLoad_RejectsDirSlugMismatch(t *testing.T) {
	r := NewPluginRegistry()
	captureLog(t)

	err := r.Load("testdata/plugins/multiinstall")
	if err == nil {
		t.Fatal("Load should return an error on dir/slug mismatch")
	}
	if !strings.Contains(err.Error(), "does not match manifest.plugin_slug") {
		t.Errorf("error should mention slug mismatch; got: %v", err)
	}
}

// TestRegistryLoad_NamespacedSlugs pins the two-level layout that
// supports voluntary namespacing for community plugins:
// <plugins-root>/acme/ha/manifest.yaml registers slug "acme/ha".
// Both acme/ha and anthropic/ha must coexist without colliding.
func TestRegistryLoad_NamespacedSlugs(t *testing.T) {
	r := NewPluginRegistry()
	captureLog(t)

	if err := r.Load("testdata/plugins/namespaced"); err != nil {
		t.Fatalf("Load: %v", err)
	}

	list := r.ListPlugins()
	if len(list) != 2 {
		t.Fatalf("expected 2 namespaced installs; got %d (%+v)", len(list), list)
	}
	if list[0].PluginSlug != "acme/ha" || list[1].PluginSlug != "anthropic/ha" {
		t.Errorf("sort order: got %q, %q; want acme/ha, anthropic/ha",
			list[0].PluginSlug, list[1].PluginSlug)
	}

	acme, ok := r.GetPluginBySlug("acme/ha")
	if !ok || acme.DisplayName != "Home Assistant (Acme fork)" {
		t.Errorf("acme lookup wrong: ok=%v manifest=%+v", ok, acme)
	}
	if !strings.HasSuffix(acme.SourceDir, "namespaced/acme/ha") {
		t.Errorf("SourceDir for nested slug should include the namespace: %q", acme.SourceDir)
	}
}

func TestRegistryLoad_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	r := NewPluginRegistry()
	buf := captureLog(t)

	if err := r.Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(r.ListPlugins()) != 0 {
		t.Errorf("expected empty registry, got %d entries", len(r.ListPlugins()))
	}
	if !strings.Contains(buf.String(), "loaded 0 plugins") {
		t.Errorf("expected 0-plugins summary, got %q", buf.String())
	}
}

func TestRegistryLoad_MissingDir(t *testing.T) {
	r := NewPluginRegistry()
	buf := captureLog(t)

	if err := r.Load("/does/not/exist/anywhere"); err != nil {
		t.Fatalf("Load on missing dir should not return error; got %v", err)
	}
	if len(r.ListPlugins()) != 0 {
		t.Errorf("expected empty registry for missing dir")
	}
	if !strings.Contains(buf.String(), "directory missing") {
		t.Errorf("expected 'directory missing' log, got %q", buf.String())
	}
}

func TestRegistryLoad_SkipsNonDirEntries(t *testing.T) {
	dir := t.TempDir()
	// Stray file at top level — must be skipped, not parsed as a plugin.
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	r := NewPluginRegistry()
	captureLog(t)

	if err := r.Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(r.ListPlugins()) != 0 {
		t.Errorf("expected empty registry (stray file should be skipped), got %d", len(r.ListPlugins()))
	}
}

func TestRegistryLoad_SkipsSubdirsWithoutManifest(t *testing.T) {
	// A subdirectory without manifest.yaml is silently skipped — not a
	// plugin install. Confirms we don't treat every dir as plugin-ish.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "not-a-plugin"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	r := NewPluginRegistry()
	buf := captureLog(t)

	if err := r.Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(r.ListPlugins()) != 0 {
		t.Errorf("expected empty registry, got %d", len(r.ListPlugins()))
	}
	if strings.Contains(buf.String(), "not-a-plugin") {
		t.Errorf("dir-without-manifest should be silently skipped, got log: %q", buf.String())
	}
}

func TestRegistryLoad_AtomicSwapReplacesPreviousContents(t *testing.T) {
	// Confirms that a second Load completely replaces the first.
	// Important for the future `hearth plugin reload` story.
	r := NewPluginRegistry()
	captureLog(t)

	if err := r.Load("testdata/plugins/basic"); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if len(r.ListPlugins()) != 2 {
		t.Fatalf("first Load expected 2 entries")
	}

	if err := r.Load("testdata/plugins/namespaced"); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	list := r.ListPlugins()
	if len(list) != 2 {
		t.Fatalf("second Load expected 2 entries, got %d", len(list))
	}
	if _, ok := r.GetPluginBySlug("ha"); ok {
		t.Error("'ha' should be gone after reload")
	}
	if _, ok := r.GetPluginBySlug("acme/ha"); !ok {
		t.Error("'acme/ha' should be present after reload")
	}
}
