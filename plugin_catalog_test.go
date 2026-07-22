package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures in testdata/catalog are the real artifacts produced by
// hearth-plugins' scripts/release.sh, not hand-written stand-ins. That means
// these tests fail if the published index format and this parser drift apart,
// which is the failure worth catching — a fabricated fixture would happily
// agree with a parser that no longer matches reality.

func loadCatalogFixtures(t *testing.T) (*CatalogIndex, []byte) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "catalog", "index.json"))
	if err != nil {
		t.Fatalf("read index fixture: %v", err)
	}
	idx, err := parseCatalogIndex(body)
	if err != nil {
		t.Fatalf("parse index fixture: %v", err)
	}
	tarball, err := os.ReadFile(filepath.Join("testdata", "catalog", "catalog.tar.gz"))
	if err != nil {
		t.Fatalf("read tarball fixture: %v", err)
	}
	return idx, tarball
}

func TestParseCatalogSlug(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantSlug    string
		wantVersion string
		wantErr     bool
	}{
		{"bare slug", "verge_labs/google_drive_oauth", "verge_labs/google_drive_oauth", "", false},
		{"pinned version", "verge_labs/google_drive_oauth@0.1.3", "verge_labs/google_drive_oauth", "0.1.3", false},
		{"surrounding space", "  verge_labs/ha  ", "verge_labs/ha", "", false},
		{"no namespace", "google_drive_oauth", "", "", true},
		{"too many segments", "a/b/c", "", "", true},
		{"empty namespace", "/ha", "", "", true},
		{"empty name", "verge_labs/", "", "", true},
		{"empty version", "verge_labs/ha@", "", "", true},
		{"empty", "", "", "", true},
		{"parent traversal", "../etc/passwd", "", "", true},
		{"dot segment", "verge_labs/..", "", "", true},
	}
	for _, tc := range cases {
		slug, version, err := parseCatalogSlug(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: parseCatalogSlug(%q) = (%q,%q), want error", tc.name, tc.in, slug, version)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: parseCatalogSlug(%q) unexpected error: %v", tc.name, tc.in, err)
			continue
		}
		if slug != tc.wantSlug || version != tc.wantVersion {
			t.Errorf("%s: parseCatalogSlug(%q) = (%q,%q), want (%q,%q)",
				tc.name, tc.in, slug, version, tc.wantSlug, tc.wantVersion)
		}
	}
}

func TestLooksLikeCatalogSlug_ExistingPathWins(t *testing.T) {
	// A real file must never be interpreted as a catalog slug, or
	// `hearth plugin install ./verge_labs/ha` would silently hit the network
	// instead of the file the operator pointed at.
	dir := t.TempDir()
	nested := filepath.Join(dir, "verge_labs")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(nested, "ha")
	if err := os.WriteFile(archive, []byte("not really a tarball"), 0o644); err != nil {
		t.Fatal(err)
	}
	if looksLikeCatalogSlug(archive) {
		t.Errorf("an existing path must not be treated as a catalog slug: %q", archive)
	}
	if !looksLikeCatalogSlug("verge_labs/google_calendar_oauth") {
		t.Error("a non-existent namespace/name must be treated as a catalog slug")
	}
	if looksLikeCatalogSlug("./nonexistent-archive.tar.gz") {
		t.Error("a non-slug-shaped non-existent path must not be treated as a catalog slug")
	}
}

func TestParseCatalogIndex_RejectsFutureSchema(t *testing.T) {
	body := []byte(`{"schema": 99, "catalog_version": "x", "plugins": {"a/b": {}}}`)
	_, err := parseCatalogIndex(body)
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("a newer index schema must be refused, not misparsed; got %v", err)
	}
}

func TestParseCatalogIndex_RejectsEmpty(t *testing.T) {
	body := []byte(`{"schema": 1, "catalog_version": "x", "plugins": {}}`)
	if _, err := parseCatalogIndex(body); err == nil {
		t.Error("an index with no plugins must be an error, not a silent no-op")
	}
}

func TestCatalogFixture_MatchesRealPlugins(t *testing.T) {
	idx, _ := loadCatalogFixtures(t)
	if idx.CatalogVersion == "" {
		t.Error("fixture index has no catalog_version")
	}
	entry, ok := idx.Plugins["verge_labs/google_drive_oauth"]
	if !ok {
		t.Fatal("fixture index missing verge_labs/google_drive_oauth")
	}
	// Pins the phase 0 floor end-to-end: the manifest declares it, the
	// index builder carries it, and the CLI parses it.
	if entry.MinDaemonVersion != "1.1.0" {
		t.Errorf("gdrive min_daemon_version = %q, want 1.1.0", entry.MinDaemonVersion)
	}
	if entry.Files["manifest.yaml"] == "" {
		t.Error("index entry has no manifest.yaml hash")
	}
}

func TestExtractCatalogPlugin_VerifiesAndReturnsFiles(t *testing.T) {
	idx, tarball := loadCatalogFixtures(t)
	slug := "verge_labs/google_calendar_oauth"
	files, err := extractCatalogPlugin(tarball, slug, idx.Plugins[slug])
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(files["manifest.yaml"]) == 0 {
		t.Error("manifest.yaml missing or empty")
	}
	if len(files["skill.md"]) == 0 {
		t.Error("skill.md missing or empty")
	}
	// Only this plugin's files, not the whole catalog.
	for name := range files {
		if name != "manifest.yaml" && name != "skill.md" {
			t.Errorf("unexpected file extracted: %q", name)
		}
	}
}

func TestExtractCatalogPlugin_PluginWithoutSkillFile(t *testing.T) {
	// home_assistant ships a manifest and no skill.md. The index names only
	// what exists, so this must extract cleanly rather than demanding a file
	// that was never published.
	idx, tarball := loadCatalogFixtures(t)
	slug := "verge_labs/home_assistant"
	files, err := extractCatalogPlugin(tarball, slug, idx.Plugins[slug])
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, ok := files["skill.md"]; ok {
		t.Error("home_assistant unexpectedly has a skill.md")
	}
	if len(files["manifest.yaml"]) == 0 {
		t.Error("manifest.yaml missing")
	}
}

func TestExtractCatalogPlugin_DetectsTamperedFile(t *testing.T) {
	// The whole point of the hashes. Corrupt the expected digest and the
	// extract must refuse rather than install what it downloaded.
	idx, tarball := loadCatalogFixtures(t)
	slug := "verge_labs/google_calendar_oauth"
	entry := idx.Plugins[slug]
	tampered := CatalogEntry{Version: entry.Version, Files: map[string]string{}}
	for k, v := range entry.Files {
		tampered.Files[k] = v
	}
	tampered.Files["manifest.yaml"] = strings.Repeat("0", 64)

	_, err := extractCatalogPlugin(tarball, slug, tampered)
	if err == nil {
		t.Fatal("a hash mismatch must fail the install")
	}
	if !strings.Contains(err.Error(), "integrity") {
		t.Errorf("error should name the integrity failure, got: %v", err)
	}
}

func TestExtractCatalogPlugin_RejectsUnlistedFile(t *testing.T) {
	// If the index only had to cover a subset, an attacker who could add a
	// file to the tarball would get it installed unverified. Drop a real
	// file from the index and the extract must refuse.
	idx, tarball := loadCatalogFixtures(t)
	slug := "verge_labs/google_calendar_oauth"
	entry := idx.Plugins[slug]
	partial := CatalogEntry{Version: entry.Version, Files: map[string]string{
		"manifest.yaml": entry.Files["manifest.yaml"],
	}}

	_, err := extractCatalogPlugin(tarball, slug, partial)
	if err == nil {
		t.Fatal("a file present in the tarball but absent from the index must be refused")
	}
	if !strings.Contains(err.Error(), "unlisted") {
		t.Errorf("error should name the unlisted file, got: %v", err)
	}
}

func TestExtractCatalogPlugin_MissingFileNamedByIndex(t *testing.T) {
	idx, tarball := loadCatalogFixtures(t)
	slug := "verge_labs/home_assistant"
	entry := idx.Plugins[slug]
	extra := CatalogEntry{Version: entry.Version, Files: map[string]string{}}
	for k, v := range entry.Files {
		extra.Files[k] = v
	}
	extra.Files["skill.md"] = strings.Repeat("a", 64)

	_, err := extractCatalogPlugin(tarball, slug, extra)
	if err == nil {
		t.Fatal("an index naming a file the catalog lacks must be refused")
	}
}

func TestExtractCatalogPlugin_UnknownSlug(t *testing.T) {
	idx, tarball := loadCatalogFixtures(t)
	entry := idx.Plugins["verge_labs/google_calendar_oauth"]
	if _, err := extractCatalogPlugin(tarball, "verge_labs/nope", entry); err == nil {
		t.Fatal("a slug absent from the tarball must be an error")
	}
}

func TestPluginProvenance_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := PluginProvenance{
		CatalogVersion: "2026.07.21",
		ContentHashes:  map[string]string{"manifest.yaml": "abc123"},
	}
	if err := writePluginProvenance(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readPluginProvenance(dir)
	if got == nil {
		t.Fatal("provenance did not round-trip")
	}
	if got.CatalogVersion != want.CatalogVersion {
		t.Errorf("catalog_version = %q, want %q", got.CatalogVersion, want.CatalogVersion)
	}
	if got.ContentHashes["manifest.yaml"] != "abc123" {
		t.Errorf("content_hashes did not round-trip: %+v", got.ContentHashes)
	}
}

func TestReadPluginProvenance_AbsentOrCorruptIsNil(t *testing.T) {
	// Provenance is informational. A missing or damaged record must never
	// stop a working plugin from loading.
	dir := t.TempDir()
	if got := readPluginProvenance(dir); got != nil {
		t.Errorf("absent provenance should be nil, got %+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, provenanceFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readPluginProvenance(dir); got != nil {
		t.Errorf("corrupt provenance should be nil, got %+v", got)
	}
	// Well-formed JSON but no catalog version is not provenance either.
	if err := os.WriteFile(filepath.Join(dir, provenanceFileName), []byte(`{"content_hashes":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readPluginProvenance(dir); got != nil {
		t.Errorf("provenance without a catalog_version should be nil, got %+v", got)
	}
}

func TestProvenanceFileIsValidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := writePluginProvenance(dir, PluginProvenance{
		CatalogVersion: "2026.07.21",
		ContentHashes:  map[string]string{"manifest.yaml": "deadbeef"},
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, provenanceFileName))
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]interface{}
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("provenance file is not valid JSON: %v", err)
	}
}
