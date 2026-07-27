//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// runPlugin dispatches `hearth plugin <subcommand> ...`. Operator
// surface for managing the local plugin install dir (~/.hearth/plugins/)
// via the running daemon. The daemon is the SOT for "what's installed":
// it owns the disk scan + the report_plugin_installs WS push, so the
// CLI deliberately routes everything through IPC rather than touching
// the directory directly.
func runPlugin(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printPluginUsage()
		if len(args) == 0 {
			os.Exit(1)
		}
		return
	}
	switch args[0] {
	case "list":
		runPluginList(args[1:])
	case "install":
		runPluginInstall(args[1:])
	case "uninstall":
		runPluginUninstall(args[1:])
	case "rollback":
		runPluginRollback(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "hearth plugin: unknown subcommand %q\n", args[0])
		printPluginUsage()
		os.Exit(1)
	}
}

func printPluginUsage() {
	fmt.Fprint(os.Stderr, `Usage: hearth plugin <subcommand> [args]

Subcommands:
  list
      List plugins currently installed on this host.

  install <namespace>/<name>[@version]
      Install a plugin from the Hearth catalog, e.g.
          hearth plugin install verge_labs/google_calendar_oauth
      Browse https://github.com/HearthCmd/hearth-plugins to find the
      exact name. Every file is checked against a hash published in the
      catalog index before anything is written.

      An optional @version asserts which version you expect; the install
      fails if the catalog has moved on. It cannot fetch a superseded
      version — use a local archive to pin to something older.

  install <archive.tar.gz>
      Install from a local tar.gz instead. The archive must contain a
      manifest.yaml at its root plus the executable referenced by
      manifest.executable. This is the only way to install a plugin with
      an executable: the catalog carries declarative plugins only.

      An existing file path always wins over a catalog lookup.

      Both forms refuse if the manifest declares a min_daemon_version
      newer than this hearth binary — run 'hearth update' first.
      Flags:
          --upgrade   Replace an existing install at a different version.
          --force     Reinstall over the same version.
          --allow-breaking
                      Apply an update that removes verbs or adds required
                      config. Refused by default: removed verbs silently
                      stop matching existing permission rules, so agents
                      that relied on them start asking for approval again.

  rollback <plugin-slug>
      Restore the version an upgrade replaced. One previous version is
      kept per plugin, set aside automatically when an upgrade lands.
      The version being rolled back from becomes the new backup, so a
      rollback is itself undoable.

  uninstall <plugin-slug>
      Remove an installed plugin. Refuses if any resource_connection on
      this host references the plugin unless --force is passed.
      Flags:
          --force     Remove even when in-use; existing connections will
                      stop working until the plugin is reinstalled.

The plugin lives under ~/.hearth/plugins/<slug>/ (override:
HEARTH_PLUGINS_DIR). The daemon re-scans + re-reports to the server
on install/uninstall so the server-side plugin registry reflects the
change without a daemon restart.
`)
}

func runPluginList(args []string) {
	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "hearth plugin list: unexpected args %v\n", args)
		os.Exit(1)
	}
	resp, err := sendPluginIPC(ipcRequest{Type: "plugin_list"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth plugin: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth plugin: %s\n", resp.Message)
		os.Exit(1)
	}
	var inner struct {
		Plugins []struct {
			PluginSlug      string `json:"plugin_slug"`
			Namespace       string `json:"namespace,omitempty"`
			Author          string `json:"author,omitempty"`
			DisplayName     string `json:"display_name"`
			Version         string `json:"version"`
			SourceDir       string `json:"source_dir"`
			Verbs           int    `json:"verbs"`
			LatestVersion   string `json:"latest_version,omitempty"`
			UpdateAvailable bool   `json:"update_available,omitempty"`
		} `json:"plugins"`
		PluginsDir   string `json:"plugins_dir"`
		CatalogKnown bool   `json:"catalog_known"`
	}
	if err := json.Unmarshal(resp.Data, &inner); err != nil {
		fmt.Fprintf(os.Stderr, "hearth plugin: decode list: %v\n", err)
		os.Exit(1)
	}
	if len(inner.Plugins) == 0 {
		fmt.Fprintf(os.Stderr, "(no plugins installed in %s)\n", inner.PluginsDir)
		return
	}
	updates := 0
	for _, p := range inner.Plugins {
		author := p.Author
		if author == "" {
			author = "unknown"
		}
		// Only annotate when there is something to say. A silent row means
		// current; the "catalog unknown" case is reported once at the end
		// rather than repeated on every line.
		status := ""
		if p.UpdateAvailable {
			status = "  → v" + p.LatestVersion + " available"
			updates++
		}
		fmt.Printf("%-32s v%-10s %-20s %s  (%d verbs)%s\n",
			p.PluginSlug, p.Version, author, p.DisplayName, p.Verbs, status)
	}

	// Distinguish "everything is current" from "we could not find out".
	// Printing nothing in both cases would let an offline daemon read as a
	// clean bill of health.
	if !inner.CatalogKnown {
		fmt.Fprintln(os.Stderr, "\n(update status unknown — daemon offline, or the server has not read the catalog yet)")
		return
	}
	if updates > 0 {
		fmt.Fprintf(os.Stderr, "\n%d update(s) available. Install with: hearth plugin install --upgrade <slug>\n", updates)
	}
}

func runPluginInstall(args []string) {
	upgrade := false
	force := false
	allowBreaking := false
	target := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--upgrade":
			upgrade = true
		case "--force":
			force = true
		case "--allow-breaking":
			allowBreaking = true
		default:
			if target != "" {
				fmt.Fprintf(os.Stderr, "hearth plugin install: unexpected arg %q\n", args[i])
				os.Exit(1)
			}
			target = args[i]
		}
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "Usage: hearth plugin install [--upgrade] [--force] [--allow-breaking] <namespace>/<name>[@version]")
		fmt.Fprintln(os.Stderr, "       hearth plugin install [--upgrade] [--force] <archive.tar.gz>")
		os.Exit(1)
	}

	req := ipcRequest{
		Type:                "plugin_install",
		PluginUpgrade:       upgrade,
		PluginForce:         force,
		PluginAllowBreaking: allowBreaking,
	}

	// An existing path always wins over a catalog lookup, so a local archive
	// is never silently fetched from the network instead.
	if looksLikeCatalogSlug(target) {
		slug, wantVersion, err := parseCatalogSlug(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth plugin install: %v\n", err)
			os.Exit(1)
		}
		req.PluginCatalogSlug = slug
		req.PluginCatalogVersion = wantVersion
		fmt.Fprintf(os.Stderr, "fetching %s from the catalog…\n", slug)
	} else {
		abs, err := filepath.Abs(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth plugin install: resolve path: %v\n", err)
			os.Exit(1)
		}
		if _, err := os.Stat(abs); err != nil {
			fmt.Fprintf(os.Stderr, "hearth plugin install: %q is neither an existing file nor a <namespace>/<name> plugin slug\n", target)
			os.Exit(1)
		}
		req.PluginArchivePath = abs
	}

	resp, err := sendPluginIPC(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth plugin: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth plugin: %s\n", resp.Message)
		os.Exit(1)
	}
	var inner struct {
		PluginSlug     string `json:"plugin_slug"`
		Version        string `json:"version"`
		SourceDir      string `json:"source_dir"`
		CatalogVersion string `json:"catalog_version,omitempty"`
		Signature      string `json:"signature,omitempty"`
		Unverified     string `json:"signature_unverified,omitempty"`
	}
	_ = json.Unmarshal(resp.Data, &inner)

	// Report the signature outcome before the success line. An operator who
	// cannot see whether verification happened has no way to distinguish a
	// working check from an absent one, which makes the check worth very
	// little — so say it every time, not only on failure.
	if inner.Signature != "" {
		if inner.Unverified != "" {
			fmt.Fprintf(os.Stderr, "WARNING: catalog %s\n", inner.Signature)
		} else {
			fmt.Fprintf(os.Stderr, "catalog %s\n", inner.Signature)
		}
	}
	if inner.CatalogVersion != "" {
		fmt.Fprintf(os.Stderr, "installed %s v%s at %s (catalog %s)\n",
			inner.PluginSlug, inner.Version, inner.SourceDir, inner.CatalogVersion)
	} else {
		fmt.Fprintf(os.Stderr, "installed %s v%s at %s\n", inner.PluginSlug, inner.Version, inner.SourceDir)
	}
}

func runPluginUninstall(args []string) {
	force := false
	slug := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--force":
			force = true
		default:
			if slug != "" {
				fmt.Fprintf(os.Stderr, "hearth plugin uninstall: unexpected arg %q\n", args[i])
				os.Exit(1)
			}
			slug = args[i]
		}
	}
	if slug == "" {
		fmt.Fprintln(os.Stderr, "Usage: hearth plugin uninstall [--force] <plugin-slug>")
		os.Exit(1)
	}
	resp, err := sendPluginIPC(ipcRequest{
		Type:        "plugin_uninstall",
		PluginSlug:  slug,
		PluginForce: force,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth plugin: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth plugin: %s\n", resp.Message)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "uninstalled %s\n", slug)
}

// sendPluginIPC mirrors sendSecretIPC — dial, write req, read one
// JSON line back. Lifted to avoid cross-file dependence.
func sendPluginIPC(req ipcRequest) (*ipcResponse, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 5*time.Second)
	if err != nil {
		return nil, daemonDialError(err)
	}
	defer conn.Close()

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %v", err)
	}
	reqBytes = append(reqBytes, '\n')
	if _, err := conn.Write(reqBytes); err != nil {
		return nil, fmt.Errorf("send: %v", err)
	}
	// Install can do real work (extract + exec probe + server roundtrip)
	// so give the read a generous deadline.
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read: %v", err)
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode: %v", err)
	}
	return &resp, nil
}

func runPluginRollback(args []string) {
	slug := ""
	for i := 0; i < len(args); i++ {
		if slug != "" {
			fmt.Fprintf(os.Stderr, "hearth plugin rollback: unexpected arg %q\n", args[i])
			os.Exit(1)
		}
		slug = args[i]
	}
	if slug == "" {
		fmt.Fprintln(os.Stderr, "Usage: hearth plugin rollback <plugin-slug>")
		os.Exit(1)
	}
	resp, err := sendPluginIPC(ipcRequest{Type: "plugin_rollback", PluginSlug: slug})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth plugin: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth plugin: %s\n", resp.Message)
		os.Exit(1)
	}
	var inner struct {
		PluginSlug string `json:"plugin_slug"`
		Version    string `json:"version"`
		SourceDir  string `json:"source_dir"`
	}
	_ = json.Unmarshal(resp.Data, &inner)
	fmt.Fprintf(os.Stderr, "rolled %s back to v%s at %s\n",
		inner.PluginSlug, inner.Version, inner.SourceDir)
	// Agents already running still hold the newer version's skill in their
	// prompt; only a respawn re-reads it.
	fmt.Fprintln(os.Stderr, "Agents currently running will pick this up on their next start.")
}
