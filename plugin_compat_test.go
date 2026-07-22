package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Coverage for the min_daemon_version compatibility sentinel. The field
// existed and was enforced at load time long before these tests, but no
// shipped manifest declared it and nothing exercised it — so the fact that
// semverParts rejected our own v-prefixed release version went unnoticed,
// and the check would have refused every plugin on every release build.

// withDaemonVersion swaps the build-time `version` global for one test.
// It is set via -ldflags in real builds and is empty under `go test`.
func withDaemonVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}

func TestCheckMinDaemonVersion_NoFloorAlwaysPasses(t *testing.T) {
	if reason := checkMinDaemonVersion("v1.0.2", ""); reason != "" {
		t.Errorf("empty floor must impose no requirement, got %q", reason)
	}
	if reason := checkMinDaemonVersion("dev", ""); reason != "" {
		t.Errorf("empty floor must impose no requirement even on dev, got %q", reason)
	}
}

func TestCheckMinDaemonVersion_ReleaseBinaryMeetsFloor(t *testing.T) {
	// The regression. Before the v-strip this returned a refusal, so any
	// manifest declaring a floor was unloadable on every released binary.
	if reason := checkMinDaemonVersion("v1.0.3", "1.0.3"); reason != "" {
		t.Errorf("v1.0.3 should satisfy floor 1.0.3, got %q", reason)
	}
	if reason := checkMinDaemonVersion("v1.1.0", "1.0.3"); reason != "" {
		t.Errorf("v1.1.0 should satisfy floor 1.0.3, got %q", reason)
	}
}

func TestCheckMinDaemonVersion_BelowFloorRefused(t *testing.T) {
	reason := checkMinDaemonVersion("v1.0.2", "1.0.3")
	if reason == "" {
		t.Fatal("v1.0.2 must not satisfy floor 1.0.3")
	}
	// The message is the whole point of the sentinel: it has to tell the
	// operator what to do, not just that something is wrong.
	if !strings.Contains(reason, "1.0.3") || !strings.Contains(reason, "hearth update") {
		t.Errorf("refusal should name the floor and the remedy, got %q", reason)
	}
}

func TestCheckMinDaemonVersion_DevBuildFailsOpen(t *testing.T) {
	// Deliberately the opposite of the server gate. A developer running a
	// local build should not be unable to load plugins; the cost of being
	// wrong here is local confusion, not a security breach.
	// See docs/plugin-distribution-plan.md §3.3.
	for _, v := range []string{"dev", "", "some-build-id"} {
		if reason := checkMinDaemonVersion(v, "1.0.3"); reason != "" {
			t.Errorf("unparseable daemon version %q must fail open, got %q", v, reason)
		}
	}
}

func TestValidateManifest_RejectsUnparseableFloor(t *testing.T) {
	// A floor we can't parse would silently never be enforced, which is
	// worse than refusing the manifest outright.
	m := validManifest()
	m.MinDaemonVersion = "one point oh"
	err := ValidateManifest(m)
	if err == nil || !strings.Contains(err.Error(), "min_daemon_version") {
		t.Errorf("expected min_daemon_version validation error, got %v", err)
	}
}

func TestValidateManifest_AcceptsFloorForms(t *testing.T) {
	for _, v := range []string{"1.0.3", "v1.0.3", "1.0", "0.128.0"} {
		m := validManifest()
		m.MinDaemonVersion = v
		if err := ValidateManifest(m); err != nil {
			t.Errorf("min_daemon_version %q should validate, got %v", v, err)
		}
	}
}

// writePlugin lays down a minimal declarative install at dir/slug.
func writePlugin(t *testing.T, root, slug, manifestYAML string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(slug))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

const floorPluginManifest = `
plugin_slug: needsnew
display_name: Needs A New Daemon
version: 0.1.0
manifest_schema: 2
min_daemon_version: 1.0.3
verbs:
  - name: ping
    description: Ping.
    output: json
    http:
      method: GET
      url: "https://example.test/ping"
`

func TestRegistryLoad_RefusesPluginAboveDaemonVersion(t *testing.T) {
	withDaemonVersion(t, "v1.0.2")
	root := t.TempDir()
	writePlugin(t, root, "needsnew", floorPluginManifest)

	r := NewPluginRegistry()
	captureLog(t)
	err := r.Load(root)
	if err == nil {
		t.Fatal("Load should refuse a plugin whose floor exceeds the daemon version")
	}
	if !strings.Contains(err.Error(), "1.0.3") {
		t.Errorf("error should name the required version, got: %v", err)
	}
}

func TestRegistryLoad_AcceptsPluginAtDaemonVersion(t *testing.T) {
	// Same manifest, same floor, a binary that satisfies it — and crucially
	// a v-prefixed version, which is what every real release reports.
	withDaemonVersion(t, "v1.0.3")
	root := t.TempDir()
	writePlugin(t, root, "needsnew", floorPluginManifest)

	r := NewPluginRegistry()
	captureLog(t)
	if err := r.Load(root); err != nil {
		t.Fatalf("Load should accept a satisfied floor, got: %v", err)
	}
	if _, ok := r.GetPluginBySlug("needsnew"); !ok {
		t.Error("plugin not registered after a satisfied floor check")
	}
}
