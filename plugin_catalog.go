//go:build darwin || linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Installing a plugin by name from the first-party catalog.
//
// `hearth plugin install verge_labs/google_calendar_oauth` resolves that slug
// against HearthCmd/hearth-plugins, verifies what it downloads, and hands a
// staged directory to the same install tail a local archive uses.
//
// Why this is not the `--from-url` that was declined in 2026-05:
//
//   - The repo is a compile-time constant. There is no flag, and no
//     environment variable in a release build, that points this somewhere
//     else. An operator cannot be talked into fetching from an attacker's
//     host, because the code cannot express that.
//   - Every file is checked against a sha256 recorded in the catalog index,
//     and (phase 1b) the index itself is checked against a pinned public key.
//     "curl | verify by hand" was the workflow being replaced; this does the
//     verifying, rather than trusting the operator to remember.
//   - Declarative plugins only. A manifest is YAML describing HTTP calls. A
//     binary plugin is arbitrary code running as the daemon user, and no
//     amount of hash-checking makes those the same risk. Binary plugins keep
//     the local-archive path, where a human handled the file.

// catalogRepo is the only source this code will fetch from. Deliberately a
// constant and not configuration: the moment it is settable, every argument
// for why fetching-by-name is safe stops holding.
const catalogRepo = "HearthCmd/hearth-plugins"

// catalogIndexSchema is the index.json format version this binary parses.
// A catalog published with a newer schema is refused rather than
// misinterpreted — same reasoning as manifest_schema.
const catalogIndexSchema = 1

// catalogFetchTimeout bounds the whole download. The catalog is tens of
// kilobytes; anything approaching this is a hung connection, not a slow one.
const catalogFetchTimeout = 30 * time.Second

// catalogMaxBytes caps what we will read from the network before giving up,
// so a malicious or broken response cannot exhaust memory or disk on the
// host. The real catalog is ~21KB; 32MB is room for orders of magnitude of
// growth while still being a hard ceiling.
const catalogMaxBytes = 32 << 20

// CatalogIndex is the parsed index.json. One signature over this document
// covers the whole catalog including every version number, which is what
// makes a rollback attack detectable.
type CatalogIndex struct {
	Schema         int                     `json:"schema"`
	CatalogVersion string                  `json:"catalog_version"`
	Plugins        map[string]CatalogEntry `json:"plugins"`

	// Verification records what happened when this index's signature was
	// checked. Populated by fetchCatalogIndex, not by the wire format, and
	// carried here so the install path can report the outcome instead of
	// leaving the operator to assume it.
	Verification *catalogVerification `json:"-"`
}

// CatalogEntry is one plugin's record in the index.
type CatalogEntry struct {
	Version          string            `json:"version"`
	DisplayName      string            `json:"display_name"`
	Description      string            `json:"description"`
	AuthScheme       string            `json:"auth_scheme,omitempty"`
	ManifestSchema   int               `json:"manifest_schema"`
	MinDaemonVersion string            `json:"min_daemon_version,omitempty"`
	Files            map[string]string `json:"files"` // filename -> sha256 hex
}

// parseCatalogSlug splits "verge_labs/google_drive_oauth@0.1.3" into its slug
// and optional version. The version acts as an assertion — "install this
// exact version, or tell me the catalog has moved on" — rather than a
// historical lookup: the index only ever describes current versions.
// Installing a superseded version is phase 4 (rollback), and until then the
// local-archive path is how you pin to something older.
func parseCatalogSlug(arg string) (slug, wantVersion string, err error) {
	slug = strings.TrimSpace(arg)
	if i := strings.Index(slug, "@"); i >= 0 {
		wantVersion = strings.TrimSpace(slug[i+1:])
		slug = strings.TrimSpace(slug[:i])
		if wantVersion == "" {
			return "", "", fmt.Errorf("%q: empty version after '@'", arg)
		}
	}
	if slug == "" {
		return "", "", fmt.Errorf("%q: empty plugin slug", arg)
	}
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%q: want <namespace>/<name>, e.g. verge_labs/google_calendar_oauth", arg)
	}
	// Guard the slug before it is ever used to index into an extracted
	// tree. A slug is an identifier, not a path.
	for _, p := range parts {
		if p == "." || p == ".." || strings.ContainsAny(p, `\:`) {
			return "", "", fmt.Errorf("%q: invalid plugin slug", arg)
		}
	}
	return slug, wantVersion, nil
}

// looksLikeCatalogSlug reports whether an install argument should be resolved
// against the catalog rather than opened as a file.
//
// The rule is deliberately "does this path exist on disk" rather than a
// pattern match, because both forms contain slashes and a pattern would make
// `hearth plugin install ./verge_labs/ha` ambiguous. An existing path always
// wins, so a local archive is never silently fetched from the network.
func looksLikeCatalogSlug(arg string) bool {
	if _, err := os.Stat(arg); err == nil {
		return false
	}
	_, _, err := parseCatalogSlug(arg)
	return err == nil
}

func catalogAssetURL(name string) string {
	return fmt.Sprintf("https://github.com/%s/releases/latest/download/%s", catalogRepo, name)
}

// httpGetLimited fetches a URL and returns at most catalogMaxBytes.
func httpGetLimited(url string) ([]byte, error) {
	client := &http.Client{Timeout: catalogFetchTimeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hearth/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("fetch %s: not found (404) — the catalog may not have a published release yet", url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, catalogMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if len(body) > catalogMaxBytes {
		return nil, fmt.Errorf("fetch %s: response exceeds %d bytes", url, catalogMaxBytes)
	}
	return body, nil
}

// fetchCatalogIndex downloads index.json, verifies its detached signature
// against the pinned key set, and parses it.
//
// Order matters: verify before parse. Parsing attacker-controlled JSON that
// has not been authenticated is a strictly larger attack surface than
// checking 64 bytes first, and there is no reason to accept it.
func fetchCatalogIndex() (*CatalogIndex, error) {
	body, err := httpGetLimited(catalogAssetURL("index.json"))
	if err != nil {
		return nil, err
	}

	var sig []byte
	if catalogSignatureRequired() {
		sig, err = httpGetLimited(catalogAssetURL("index.json.sig"))
		if err != nil {
			// A 404 here means the release exists but was published without
			// a signature, which is a different problem from an unreachable
			// catalog and has a different fix. Say which.
			if strings.Contains(err.Error(), "404") {
				return nil, fmt.Errorf("the latest catalog release has no index.json.sig, so it cannot be " +
					"verified; refusing to install. The release must be signed and the signature uploaded")
			}
			return nil, fmt.Errorf("catalog signature: %w", err)
		}
	}
	verification, err := verifyCatalogIndex(body, sig)
	if err != nil {
		return nil, err
	}

	idx, err := parseCatalogIndex(body)
	if err != nil {
		return nil, err
	}
	idx.Verification = verification
	return idx, nil
}

func parseCatalogIndex(body []byte) (*CatalogIndex, error) {
	var idx CatalogIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("parse catalog index: %w", err)
	}
	if idx.Schema != catalogIndexSchema {
		return nil, fmt.Errorf("catalog index schema %d is not supported by this binary (supports %d); run `hearth update`",
			idx.Schema, catalogIndexSchema)
	}
	if len(idx.Plugins) == 0 {
		return nil, fmt.Errorf("catalog index lists no plugins")
	}
	return &idx, nil
}

// sha256Hex is the digest form used throughout the index.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// extractCatalogPlugin pulls one plugin's files out of catalog.tar.gz and
// verifies each against the index entry.
//
// Returns the file contents keyed by base name. Nothing is written to disk
// here: verification completes in memory, so a tampered file never lands in
// the plugins directory even transiently.
func extractCatalogPlugin(tarball []byte, slug string, entry CatalogEntry) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, fmt.Errorf("catalog tarball: gzip: %w", err)
	}
	defer gz.Close()

	// Paths inside the tarball mirror the repo: plugins/<slug>/<file>.
	prefix := "plugins/" + slug + "/"
	found := map[string][]byte{}

	// Budget across the whole extraction, not just per file. A gzip stream
	// that fits under catalogMaxBytes compressed can expand to far more, and
	// a per-file cap alone would let many files each sit just under it. The
	// real catalog is ~21KB decompressed, so this is orders of magnitude of
	// headroom and still a hard ceiling.
	remaining := int64(catalogMaxBytes)

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("catalog tarball: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		name := path.Clean(hdr.Name)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(name, prefix)
		// Only direct children. A plugin is a flat directory; nested paths
		// would be an unexpected shape and are not part of the signed set.
		if rel == "" || strings.Contains(rel, "/") {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, remaining+1))
		if err != nil {
			return nil, fmt.Errorf("catalog tarball: read %s: %w", name, err)
		}
		if int64(len(data)) > remaining {
			return nil, fmt.Errorf("catalog tarball: contents for %s exceed the %d byte limit", slug, catalogMaxBytes)
		}
		remaining -= int64(len(data))
		found[rel] = data
	}

	if len(found) == 0 {
		return nil, fmt.Errorf("catalog tarball contains no files for %s", slug)
	}

	// Every file the index names must be present and match its hash.
	for filename, wantHash := range entry.Files {
		data, ok := found[filename]
		if !ok {
			return nil, fmt.Errorf("%s: index names %s but the catalog does not contain it", slug, filename)
		}
		if got := sha256Hex(data); got != wantHash {
			return nil, fmt.Errorf("%s: %s failed integrity check (index says %s, downloaded %s)",
				slug, filename, wantHash, got)
		}
	}

	// And nothing may be installed that the index did not name. Without this
	// the hashes would only cover an attacker-chosen subset: extra files
	// would ride along unverified.
	for filename := range found {
		if _, ok := entry.Files[filename]; !ok {
			return nil, fmt.Errorf("%s: catalog contains unlisted file %s; refusing (the index must name every installed file)",
				slug, filename)
		}
	}

	return found, nil
}

// stageCatalogPlugin resolves a slug against the catalog, verifies the files,
// and writes them into a fresh staging directory under pluginsDir. Mirrors
// stagePluginArchive's contract: caller renames the staging dir into place or
// removes it.
func stageCatalogPlugin(pluginsDir, slug, wantVersion string) (PluginManifest, string, *CatalogIndex, error) {
	idx, err := fetchCatalogIndex()
	if err != nil {
		return PluginManifest{}, "", nil, err
	}
	entry, ok := idx.Plugins[slug]
	if !ok {
		return PluginManifest{}, "", nil, fmt.Errorf("no plugin %q in the catalog; browse https://github.com/%s to find the exact name",
			slug, catalogRepo)
	}
	if wantVersion != "" && entry.Version != wantVersion {
		return PluginManifest{}, "", nil, fmt.Errorf("catalog has %s at version %s, not %s (this binary cannot install superseded versions)",
			slug, entry.Version, wantVersion)
	}

	tarball, err := httpGetLimited(catalogAssetURL("catalog.tar.gz"))
	if err != nil {
		return PluginManifest{}, "", nil, err
	}
	files, err := extractCatalogPlugin(tarball, slug, entry)
	if err != nil {
		return PluginManifest{}, "", nil, err
	}

	manifestBytes, ok := files["manifest.yaml"]
	if !ok {
		return PluginManifest{}, "", nil, fmt.Errorf("%s: catalog entry has no manifest.yaml", slug)
	}
	manifest, err := ParseManifest(manifestBytes)
	if err != nil {
		return PluginManifest{}, "", nil, fmt.Errorf("%s: parse manifest: %w", slug, err)
	}
	if err := ValidateManifest(manifest); err != nil {
		return PluginManifest{}, "", nil, fmt.Errorf("%s: %w", slug, err)
	}
	if manifest.PluginSlug != slug {
		return PluginManifest{}, "", nil, fmt.Errorf("catalog entry %s contains a manifest declaring plugin_slug=%q",
			slug, manifest.PluginSlug)
	}
	// The index is what the CLI compared versions against and what a
	// signature covers; the manifest is what actually runs. If they disagree
	// the index is lying about what it published, so refuse rather than
	// install something mislabelled.
	if manifest.Version != entry.Version {
		return PluginManifest{}, "", nil, fmt.Errorf("%s: catalog index says version %s but the manifest says %s",
			slug, entry.Version, manifest.Version)
	}
	// Belt and braces against a catalog that published a binary plugin: the
	// index builder refuses these, but this path must not depend on the
	// publisher having done that correctly.
	if ClassifyManifestSource(manifest) != SourceDeclarative {
		return PluginManifest{}, "", nil, fmt.Errorf("%s: only declarative plugins can be installed from the catalog", slug)
	}

	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return PluginManifest{}, "", nil, fmt.Errorf("mkdir plugins root: %w", err)
	}
	staging, err := os.MkdirTemp(pluginsDir, ".install-")
	if err != nil {
		return PluginManifest{}, "", nil, fmt.Errorf("mkdir staging: %w", err)
	}
	for filename, data := range files {
		// filename is a verified direct child (no separators), and was
		// matched against the index's key set above.
		if err := os.WriteFile(filepath.Join(staging, filename), data, 0o644); err != nil {
			os.RemoveAll(staging)
			return PluginManifest{}, "", nil, fmt.Errorf("write %s: %w", filename, err)
		}
	}
	prov := PluginProvenance{
		CatalogVersion: idx.CatalogVersion,
		ContentHashes:  entry.Files,
	}
	if err := writePluginProvenance(staging, prov); err != nil {
		os.RemoveAll(staging)
		return PluginManifest{}, "", nil, err
	}
	return manifest, staging, idx, nil
}

// provenanceFileName records where an install came from, inside the install
// directory itself.
//
// It has to be on disk rather than in daemon memory: the registry rebuilds
// from disk at every boot and reconnect, and re-reports to the server from
// that rebuild. Provenance held only in memory would silently become NULL on
// the first daemon restart, which is precisely when you would want to know
// what a host is running.
//
// Dot-prefixed so it sorts out of the way and reads as metadata rather than
// plugin content. The registry ignores unknown files, so its presence is
// inert to everything except the reporter.
const provenanceFileName = ".provenance.json"

// PluginProvenance is what a catalog install records about its own origin.
// Local-archive installs have none, and that is not a gap to backfill — they
// genuinely have no catalog origin, and inventing one would be worse than
// recording nothing.
type PluginProvenance struct {
	CatalogVersion string            `json:"catalog_version"`
	ContentHashes  map[string]string `json:"content_hashes"`
}

func writePluginProvenance(dir string, p PluginProvenance) error {
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("encode provenance: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, provenanceFileName), append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("write provenance: %w", err)
	}
	return nil
}

// readPluginProvenance returns nil when the file is absent or unreadable.
// Provenance is informational, so a missing or corrupt record must never
// prevent a working plugin from loading.
func readPluginProvenance(dir string) *PluginProvenance {
	body, err := os.ReadFile(filepath.Join(dir, provenanceFileName))
	if err != nil {
		return nil
	}
	var p PluginProvenance
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	if p.CatalogVersion == "" {
		return nil
	}
	return &p
}
