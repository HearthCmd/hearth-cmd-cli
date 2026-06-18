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

  install <archive>
      Install a plugin from a tar.gz archive. The archive must contain
      a manifest.yaml at its root plus the executable referenced by
      manifest.executable. Refuses if the same plugin_slug is already
      installed at the same version; refuses on version mismatch
      unless --upgrade is passed.
      Flags:
          --upgrade   Replace an existing install at a different version.

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
			PluginSlug  string `json:"plugin_slug"`
			Namespace   string `json:"namespace,omitempty"`
			Author      string `json:"author,omitempty"`
			DisplayName string `json:"display_name"`
			Version     string `json:"version"`
			SourceDir   string `json:"source_dir"`
			Verbs       int    `json:"verbs"`
		} `json:"plugins"`
		PluginsDir string `json:"plugins_dir"`
	}
	if err := json.Unmarshal(resp.Data, &inner); err != nil {
		fmt.Fprintf(os.Stderr, "hearth plugin: decode list: %v\n", err)
		os.Exit(1)
	}
	if len(inner.Plugins) == 0 {
		fmt.Fprintf(os.Stderr, "(no plugins installed in %s)\n", inner.PluginsDir)
		return
	}
	for _, p := range inner.Plugins {
		author := p.Author
		if author == "" {
			author = "unknown"
		}
		fmt.Printf("%-32s v%-10s %-20s %s  (%d verbs)\n",
			p.PluginSlug, p.Version, author, p.DisplayName, p.Verbs)
	}
}

func runPluginInstall(args []string) {
	upgrade := false
	archivePath := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--upgrade":
			upgrade = true
		default:
			if archivePath != "" {
				fmt.Fprintf(os.Stderr, "hearth plugin install: unexpected arg %q\n", args[i])
				os.Exit(1)
			}
			archivePath = args[i]
		}
	}
	if archivePath == "" {
		fmt.Fprintln(os.Stderr, "Usage: hearth plugin install [--upgrade] <archive.tar.gz>")
		os.Exit(1)
	}
	abs, err := filepath.Abs(archivePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth plugin install: resolve path: %v\n", err)
		os.Exit(1)
	}
	resp, err := sendPluginIPC(ipcRequest{
		Type:              "plugin_install",
		PluginArchivePath: abs,
		PluginUpgrade:     upgrade,
	})
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
	fmt.Fprintf(os.Stderr, "installed %s v%s at %s\n", inner.PluginSlug, inner.Version, inner.SourceDir)
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
		return nil, fmt.Errorf("cannot connect to daemon: %v\nRun 'hearth start' first", err)
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
