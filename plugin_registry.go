package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// PluginRegistry holds the in-memory set of plugin manifests
// discovered from disk. Owned by *Daemon (not a package global) so
// tests construct their own instances trivially.
//
// Concurrency: Load uses an atomic swap behind a write lock so
// readers always see either the previous set or the new one — never
// a half-built map. This is the contract that lets a future `hearth
// plugin reload` IPC handler share the same Load entry point with
// daemon boot.
type PluginRegistry struct {
	mu           sync.RWMutex
	byPluginSlug map[string]PluginManifest
	order        []string // slugs in sorted order; for deterministic ListPlugins
}

func NewPluginRegistry() *PluginRegistry {
	return &PluginRegistry{
		byPluginSlug: map[string]PluginManifest{},
	}
}

// Load discovers plugin installs under dir, reads + validates each
// install's manifest.yaml, and atomically replaces the registry's
// contents.
//
// Layout: the relative path from dir to the install dir, with `/`
// separators, MUST equal the manifest's plugin_slug:
//
//	~/.hearth/plugins/echo/manifest.yaml         → slug "echo"
//	~/.hearth/plugins/acme/ha/manifest.yaml      → slug "acme/ha"
//
// Two levels of nesting are supported (operator can namespace
// community plugins to avoid collisions: anthropic/ha vs acme/ha
// can coexist). Deeper nesting is not scanned.
//
// Any directory that contains a manifest.yaml is expected to be a
// valid plugin install. A malformed manifest, a failed validation, or
// a slug/path mismatch is a hard error — Load returns immediately
// without updating the registry. A missing or empty plugins dir is
// not an error.
//
// Symlinks are followed (use os.Stat, not DirEntry.IsDir), so the
// dev workflow `ln -s ~/myrepo/hearth-plugin-ha ~/.hearth/plugins/ha`
// works without special-casing.
func (r *PluginRegistry) Load(dir string) error {
	next := map[string]PluginManifest{}
	var nextOrder []string

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("daemon: loaded 0 plugins from %s (directory missing)", dir)
			r.swap(next, nextOrder)
			return nil
		}
		return fmt.Errorf("plugin registry read %s: %w", dir, err)
	}

	for _, entry := range entries {
		topName := entry.Name()
		topDir := filepath.Join(dir, topName)

		fi, err := os.Stat(topDir)
		if err != nil || !fi.IsDir() {
			continue
		}

		// Flat install: ~/.hearth/plugins/<topName>/manifest.yaml.
		if hasManifest(topDir) {
			m, err := loadInstall(dir, topName)
			if err != nil {
				return err
			}
			next[m.PluginSlug] = m
			nextOrder = append(nextOrder, m.PluginSlug)
			continue
		}

		// Namespaced install: ~/.hearth/plugins/<topName>/<sub>/manifest.yaml.
		// topName acts as a namespace ("acme"); each sub-entry with
		// a manifest is its own install with slug "<topName>/<sub>".
		// Sub-entries without manifests are silently skipped (no
		// log spam for incidental directories).
		subEntries, err := os.ReadDir(topDir)
		if err != nil {
			continue
		}
		for _, sub := range subEntries {
			subDir := filepath.Join(topDir, sub.Name())
			sfi, err := os.Stat(subDir)
			if err != nil || !sfi.IsDir() || !hasManifest(subDir) {
				continue
			}
			relSlug := topName + "/" + sub.Name()
			m, err := loadInstall(dir, relSlug)
			if err != nil {
				return err
			}
			next[m.PluginSlug] = m
			nextOrder = append(nextOrder, m.PluginSlug)
		}
	}

	sort.Strings(nextOrder)
	r.swap(next, nextOrder)

	if len(nextOrder) == 0 {
		log.Printf("daemon: loaded 0 plugins from %s", dir)
	} else {
		log.Printf("daemon: loaded %d plugins from %s: %s",
			len(nextOrder), dir, strings.Join(nextOrder, ", "))
	}
	return nil
}

// loadInstall parses + validates one install at rootDir/relSlug,
// where relSlug is the slash-separated path expected to equal the
// manifest's plugin_slug. Returns an error for any failure — the
// caller treats this as fatal.
func loadInstall(rootDir, relSlug string) (PluginManifest, error) {
	installDir := filepath.Join(rootDir, filepath.FromSlash(relSlug))
	manifestPath := filepath.Join(installDir, "manifest.yaml")

	m, err := ReadManifestFile(manifestPath)
	if err != nil {
		return PluginManifest{}, fmt.Errorf("plugin %s: parse error: %w", relSlug, err)
	}
	if err := ValidateManifest(m); err != nil {
		return PluginManifest{}, fmt.Errorf("plugin %s: %w", relSlug, err)
	}

	// Invariant: install directory path (relative to the registry
	// root, slash-separated) must equal manifest.plugin_slug. The
	// two-layer (operator-chosen dir name + manifest slug) model
	// was the source of the ha-home / ha-rental confusion —
	// multi-instance lives at the resource_connections layer, not
	// here. Namespaced slugs (acme/ha) require the install to live
	// at acme/ha/manifest.yaml.
	if relSlug != m.PluginSlug {
		return PluginManifest{}, fmt.Errorf("plugin %s: directory path does not match manifest.plugin_slug=%q", relSlug, m.PluginSlug)
	}
	if m.MinDaemonVersion != "" && !semverGTE(version, m.MinDaemonVersion) {
		return PluginManifest{}, fmt.Errorf("plugin %s requires daemon >= %s (this binary is %q); upgrade hearth to use this plugin",
			relSlug, m.MinDaemonVersion, version)
	}

	absDir, absErr := filepath.Abs(installDir)
	if absErr != nil {
		absDir = installDir
	}
	m.SourceDir = absDir
	m.Source = ClassifyManifestSource(m)

	if len(m.Verbs) == 0 {
		log.Printf("plugin %s: zero verbs declared (degenerate but legitimate)", relSlug)
	}
	return m, nil
}

func hasManifest(installDir string) bool {
	_, err := os.Stat(filepath.Join(installDir, "manifest.yaml"))
	return err == nil
}

func (r *PluginRegistry) swap(next map[string]PluginManifest, order []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byPluginSlug = next
	r.order = order
}

// GetPluginBySlug returns the manifest for the plugin installed
// under <plugins-root>/<slug>/. Concurrency-safe.
func (r *PluginRegistry) GetPluginBySlug(slug string) (PluginManifest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.byPluginSlug[slug]
	return m, ok
}

// ListPlugins returns the registered manifests in deterministic
// (alphabetical-by-slug) order. Caller iterates without holding
// the registry lock.
func (r *PluginRegistry) ListPlugins() []PluginManifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]PluginManifest, 0, len(r.order))
	for _, slug := range r.order {
		out = append(out, r.byPluginSlug[slug])
	}
	return out
}
