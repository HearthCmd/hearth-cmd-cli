//go:build darwin || linux

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// IPC handlers for the `hearth plugin` CLI subcommand. Install
// extracts a tar.gz into ~/.hearth/plugins/<slug>/ atomically;
// uninstall removes it. Both refresh d.plugins and push the new
// list to the server via reportPluginInstallsAtBoot so the
// server-side plugin_installs table picks up the change without a
// daemon restart.

func (d *Daemon) handlePluginList(conn net.Conn, _ ipcRequest) {
	type item struct {
		PluginSlug  string `json:"plugin_slug"`
		Namespace   string `json:"namespace,omitempty"`
		Author      string `json:"author,omitempty"`
		DisplayName string `json:"display_name"`
		Version     string `json:"version"`
		SourceDir   string `json:"source_dir"`
		Verbs       int    `json:"verbs"`
	}
	list := d.plugins.ListPlugins()
	out := make([]item, 0, len(list))
	for _, m := range list {
		out = append(out, item{
			PluginSlug:  m.PluginSlug,
			Namespace:   m.Namespace,
			Author:      m.Author,
			DisplayName: m.DisplayName,
			Version:     m.Version,
			SourceDir:   m.SourceDir,
			Verbs:       len(m.Verbs),
		})
	}
	data, _ := json.Marshal(map[string]interface{}{"plugins": out, "plugins_dir": d.pluginsDir})
	sendControl(conn, ipcResponse{Type: "plugin_list_response", Data: data})
}

func (d *Daemon) handlePluginInstall(conn net.Conn, req ipcRequest) {
	if req.PluginArchivePath == "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "plugin_archive_path required"})
		return
	}
	manifest, stagingDir, err := stagePluginArchive(d.pluginsDir, req.PluginArchivePath)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: err.Error()})
		return
	}
	// stagingDir is left behind on any error past this point so the
	// operator can inspect it; cleaned up on success or on the
	// conflict-refusal path below.

	finalDir := filepath.Join(d.pluginsDir, filepath.FromSlash(manifest.PluginSlug))
	if existing, err := ReadManifestFile(filepath.Join(finalDir, "manifest.yaml")); err == nil {
		// A prior install of this slug exists. Same-version is a no-op
		// (idempotent re-install); different version requires explicit
		// --upgrade so a daemon serving live agents isn't surprised.
		if existing.Version == manifest.Version {
			os.RemoveAll(stagingDir)
			sendControl(conn, ipcResponse{Type: "error",
				Message: fmt.Sprintf("plugin %s version %s is already installed", manifest.PluginSlug, manifest.Version)})
			return
		}
		if !req.PluginUpgrade {
			os.RemoveAll(stagingDir)
			sendControl(conn, ipcResponse{Type: "error",
				Message: fmt.Sprintf("plugin %s already installed at version %s (archive is %s); pass --upgrade to replace",
					manifest.PluginSlug, existing.Version, manifest.Version)})
			return
		}
	}

	// Exec probe: make sure the OS can actually launch the bundled
	// binary on this host before swapping it into place. Catches
	// arch/format mismatches (Linux binary in a Mac tarball, etc.)
	// at install time rather than first agent invocation. Declarative
	// adapters have no executable to probe — manifest validation
	// already guaranteed Executable is empty in that case.
	if ClassifyManifestSource(manifest) == SourceBinary {
		if err := probePluginExecutable(stagingDir, manifest.Executable); err != nil {
			os.RemoveAll(stagingDir)
			sendControl(conn, ipcResponse{Type: "error", Message: "executable probe: " + err.Error()})
			return
		}
	}

	// Atomic swap. RemoveAll the old dir first (we already passed
	// the --upgrade gate); MkdirAll the parent (namespaced slugs like
	// acme/ha need acme/ to exist); Rename staging into place.
	if err := os.RemoveAll(finalDir); err != nil {
		os.RemoveAll(stagingDir)
		sendControl(conn, ipcResponse{Type: "error", Message: "remove existing install: " + err.Error()})
		return
	}
	if err := os.MkdirAll(filepath.Dir(finalDir), 0o755); err != nil {
		os.RemoveAll(stagingDir)
		sendControl(conn, ipcResponse{Type: "error", Message: "mkdir parent: " + err.Error()})
		return
	}
	if err := os.Rename(stagingDir, finalDir); err != nil {
		os.RemoveAll(stagingDir)
		sendControl(conn, ipcResponse{Type: "error", Message: "rename into place: " + err.Error()})
		return
	}

	// Refresh in-memory registry + push to server. Both errors are
	// logged but not surfaced to the CLI: the disk install succeeded
	// and the next daemon boot would self-heal.
	if err := d.plugins.Load(d.pluginsDir); err != nil {
		fmt.Fprintf(os.Stderr, "plugin install: registry reload: %v\n", err)
	}
	go d.reportPluginInstallsAtBoot()

	resp, _ := json.Marshal(map[string]string{
		"plugin_slug": manifest.PluginSlug,
		"version":     manifest.Version,
		"source_dir":  finalDir,
	})
	sendControl(conn, ipcResponse{Type: "plugin_install_response", Data: resp})
}

func (d *Daemon) handlePluginUninstall(conn net.Conn, req ipcRequest) {
	if req.PluginSlug == "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "plugin_slug required"})
		return
	}
	slug := req.PluginSlug

	// Refuse if any local resource_connection points at this plugin
	// — the operator should drop the connection first (which cascades
	// agent grants on the server side) or pass --force.
	if !req.PluginForce {
		using := []string{}
		for _, rc := range d.resourceConnections.List() {
			if rc.PluginSlug == slug {
				using = append(using, rc.ConnectionID)
			}
		}
		if len(using) > 0 {
			sendControl(conn, ipcResponse{Type: "error",
				Message: fmt.Sprintf("plugin %s is in use by %d resource_connection(s) [%s]; remove them first or pass --force",
					slug, len(using), strings.Join(using, ", "))})
			return
		}
	}

	finalDir := filepath.Join(d.pluginsDir, filepath.FromSlash(slug))
	if _, err := os.Stat(finalDir); os.IsNotExist(err) {
		sendControl(conn, ipcResponse{Type: "error",
			Message: fmt.Sprintf("plugin %s is not installed", slug)})
		return
	}
	if err := os.RemoveAll(finalDir); err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "remove: " + err.Error()})
		return
	}

	if err := d.plugins.Load(d.pluginsDir); err != nil {
		fmt.Fprintf(os.Stderr, "plugin uninstall: registry reload: %v\n", err)
	}
	go d.reportPluginInstallsAtBoot()

	resp, _ := json.Marshal(map[string]string{"plugin_slug": slug})
	sendControl(conn, ipcResponse{Type: "plugin_uninstall_response", Data: resp})
}

// stagePluginArchive extracts a tar.gz at archivePath into a fresh
// directory under pluginsDir (`.install-<pid>-<ns>`), parses +
// validates the bundled manifest, and returns it along with the
// staging path. Caller is responsible for renaming the staging path
// into place (or removing it on a refusal). Path-traversal entries
// (absolute paths, `..`) are rejected; the manifest must live at the
// archive root (no slug-name top-level dir).
func stagePluginArchive(pluginsDir, archivePath string) (PluginManifest, string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return PluginManifest{}, "", fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return PluginManifest{}, "", fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return PluginManifest{}, "", fmt.Errorf("mkdir plugins root: %w", err)
	}
	staging, err := os.MkdirTemp(pluginsDir, ".install-")
	if err != nil {
		return PluginManifest{}, "", fmt.Errorf("mkdir staging: %w", err)
	}
	cleanupOnErr := true
	defer func() {
		if cleanupOnErr {
			os.RemoveAll(staging)
		}
	}()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return PluginManifest{}, "", fmt.Errorf("tar entry: %w", err)
		}
		name := hdr.Name
		if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
			return PluginManifest{}, "", fmt.Errorf("archive entry %q: absolute paths not allowed", name)
		}
		clean := filepath.Clean(name)
		if strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") {
			return PluginManifest{}, "", fmt.Errorf("archive entry %q: path traversal not allowed", name)
		}
		target := filepath.Join(staging, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777|0o700); err != nil {
				return PluginManifest{}, "", fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return PluginManifest{}, "", fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return PluginManifest{}, "", fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return PluginManifest{}, "", fmt.Errorf("write %s: %w", target, err)
			}
			out.Close()
		case tar.TypeSymlink:
			// Symlinks risk pointing out of the staging dir. Refuse
			// for now; revisit if a real plugin needs them.
			return PluginManifest{}, "", fmt.Errorf("archive entry %q: symlinks not allowed", name)
		default:
			// Skip device files, hard links, etc. silently.
		}
	}

	manifest, err := ReadManifestFile(filepath.Join(staging, "manifest.yaml"))
	if err != nil {
		return PluginManifest{}, "", fmt.Errorf("manifest.yaml at archive root: %w", err)
	}
	if err := ValidateManifest(manifest); err != nil {
		return PluginManifest{}, "", fmt.Errorf("manifest invalid: %w", err)
	}
	cleanupOnErr = false
	return manifest, staging, nil
}

// probePluginExecutable runs the bundled binary with --help (3s
// timeout) just to confirm the OS can exec it on this host. We don't
// care about exit code — only that exec/start doesn't fail with
// ENOEXEC, ENOENT, or wrong-arch errors that would otherwise surface
// at first verb invocation against a freshly-installed plugin.
func probePluginExecutable(stagingDir, execRel string) error {
	if execRel == "" {
		return fmt.Errorf("manifest.executable is empty")
	}
	execPath := filepath.Join(stagingDir, execRel)
	fi, err := os.Stat(execPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", execRel, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory, not a file", execRel)
	}
	if fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable (mode %v); set +x before packaging", execRel, fi.Mode())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, execPath, "--help")
	cmd.Dir = stagingDir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", execRel, err)
	}
	_ = cmd.Wait() // exit code irrelevant; we only validated exec
	return nil
}
