//go:build darwin || linux

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
		PluginSlug      string `json:"plugin_slug"`
		Namespace       string `json:"namespace,omitempty"`
		Author          string `json:"author,omitempty"`
		DisplayName     string `json:"display_name"`
		Version         string `json:"version"`
		SourceDir       string `json:"source_dir"`
		Verbs           int    `json:"verbs"`
		LatestVersion   string `json:"latest_version,omitempty"`
		UpdateAvailable bool   `json:"update_available,omitempty"`
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

	// Ask the server what the catalog currently publishes. This rides the
	// already-open daemon WS and is answered from the relay's cached view,
	// so it costs no GitHub request from this host — which is the whole
	// reason the polling lives on the server.
	//
	// Strictly best-effort: the local registry is the source of truth for
	// what is installed, and `plugin list` has always worked with no server.
	// A disconnected daemon simply reports no update information rather than
	// failing, and the caller renders "?" instead of a false "up to date".
	latest, known := d.fetchCatalogVersions()
	if known {
		for i := range out {
			v, ok := latest[out[i].PluginSlug]
			if !ok {
				continue
			}
			out[i].LatestVersion = v
			out[i].UpdateAvailable = pluginUpdateAvailable(out[i].Version, v)
		}
	}

	data, _ := json.Marshal(map[string]interface{}{
		"plugins":       out,
		"plugins_dir":   d.pluginsDir,
		"catalog_known": known,
	})
	sendControl(conn, ipcResponse{Type: "plugin_list_response", Data: data})
}

// pluginUpdateAvailable reports whether published is strictly newer than
// installed.
//
// The comparison is done here against the LOCAL installed version rather than
// trusting the server's update_available flag, because the server's notion of
// what is installed comes from the last report and can lag — most obviously
// right after an install, before the report lands. The registry on this host
// is authoritative about this host.
//
// Strictly-greater is expressed as ">= and not <=" rather than "!= and >=",
// so two versions that differ as strings but are equal as semver ("0.1" vs
// "0.1.0") correctly report no update.
func pluginUpdateAvailable(installed, published string) bool {
	if installed == "" || published == "" {
		return false
	}
	if semverParts(installed) == nil || semverParts(published) == nil {
		return false
	}
	return semverGTE(published, installed) && !semverGTE(installed, published)
}

// fetchCatalogVersions asks the server for the published version of every
// plugin installed in this host's org. Returns known=false when the server
// could not be reached or has no catalog data of its own, which the caller
// must render as "unknown" rather than "current".
func (d *Daemon) fetchCatalogVersions() (map[string]string, bool) {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		return nil, false
	}
	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "plugin_installs_list", json.RawMessage(`{}`))
	if err != nil {
		log.Printf("plugin list: catalog versions unavailable: %v", err)
		return nil, false
	}
	var resp struct {
		CatalogVersion string `json:"catalog_version"`
		PluginInstalls []struct {
			PluginSlug    string `json:"plugin_slug"`
			LatestVersion string `json:"latest_version"`
		} `json:"plugin_installs"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, false
	}
	// No catalog_version means the relay's poller has not succeeded yet or is
	// disabled. Its rows carry no version information, so saying "unknown" is
	// the honest answer.
	if resp.CatalogVersion == "" {
		return nil, false
	}
	out := make(map[string]string, len(resp.PluginInstalls))
	for _, p := range resp.PluginInstalls {
		if p.LatestVersion != "" {
			out[p.PluginSlug] = p.LatestVersion
		}
	}
	return out, true
}

// pluginInstallOutcome is what an install produced, whichever entry point
// asked for it.
type pluginInstallOutcome struct {
	PluginSlug     string
	Version        string
	SourceDir      string
	CatalogVersion string
	// Signature describes what the catalog signature check concluded.
	// Empty for local-archive installs, which have no catalog signature.
	Signature string
	// SignatureVerified is false when a catalog install went ahead without
	// a verified signature, which only a dev build with no pinned key can
	// do. Meaningless for local-archive installs.
	SignatureVerified bool
	// Diff describes what changed against the version this replaced. nil on
	// a first install (nothing to compare) and on local-archive installs of
	// a plugin that was not previously present.
	//
	// Populated even on the breaking-change REFUSAL, so the caller can tell
	// the operator what would break rather than only that something would.
	Diff *pluginUpdateDiff
}

// installPlugin is the whole install, independent of who asked.
//
// Both entry points — `hearth plugin install` over IPC and install_plugin
// pushed from the server — go through here, so neither can drift from the
// other's safety checks. That matters more than the deduplication: the
// server-driven path is the one a household member triggers from a phone,
// and it must not be a second implementation that quietly skips the
// compatibility floor or the conflict rules.
//
// The two sources differ only in how bytes are staged. Everything after —
// compat floor, conflict rules, exec probe, atomic swap, registry reload,
// re-report — is common.
func (d *Daemon) installPlugin(req ipcRequest) (*pluginInstallOutcome, error) {
	var (
		manifest    PluginManifest
		stagingDir  string
		index       *CatalogIndex
		outcomeDiff *pluginUpdateDiff
		err         error
	)

	switch {
	case req.PluginCatalogSlug != "":
		manifest, stagingDir, index, err = stageCatalogPlugin(
			d.pluginsDir, req.PluginCatalogSlug, req.PluginCatalogVersion)
	case req.PluginArchivePath != "":
		manifest, stagingDir, err = stagePluginArchive(d.pluginsDir, req.PluginArchivePath)
	default:
		return nil, fmt.Errorf("plugin_archive_path or plugin_catalog_slug required")
	}
	if err != nil {
		return nil, err
	}
	// stagingDir is left behind on unexpected errors past this point so the
	// operator can inspect it; cleaned up on success and on the deliberate
	// refusal paths below (compat floor, version conflict, exec probe).

	// Compatibility sentinel, before anything touches the live install dir.
	// "This binary can't run this plugin at all" precedes "you already have
	// it" — refusing here means the operator gets one actionable message
	// (`hearth update`) instead of discovering at first verb invocation that
	// a feature the manifest depends on isn't in their binary.
	if reason := checkMinDaemonVersion(version, manifest.MinDaemonVersion); reason != "" {
		os.RemoveAll(stagingDir)
		return nil, fmt.Errorf("plugin %s %s", manifest.PluginSlug, reason)
	}

	finalDir := filepath.Join(d.pluginsDir, filepath.FromSlash(manifest.PluginSlug))
	if existing, rerr := ReadManifestFile(filepath.Join(finalDir, "manifest.yaml")); rerr == nil {
		// A prior install of this slug exists. Same-version needs --force,
		// different version needs --upgrade, so a daemon serving live agents
		// is never surprised by either.
		//
		// Same-version used to be an unconditional refusal that --upgrade
		// could not override, which is why the documented way to push changed
		// content was to bypass this command and hand-copy files. Now that a
		// catalog exists, "reinstall this, my copy may be damaged" is an
		// ordinary thing to want, so it gets a flag instead of a workaround.
		if existing.Version == manifest.Version {
			if !req.PluginForce {
				os.RemoveAll(stagingDir)
				return nil, fmt.Errorf("plugin %s version %s is already installed; pass --force to reinstall",
					manifest.PluginSlug, manifest.Version)
			}
		} else if !req.PluginUpgrade {
			os.RemoveAll(stagingDir)
			return nil, fmt.Errorf("plugin %s already installed at version %s (incoming is %s); pass --upgrade to replace",
				manifest.PluginSlug, existing.Version, manifest.Version)
		}

		// Breaking-change preflight, while the old manifest is still on disk
		// and nothing has been swapped. --upgrade says "replace this
		// version"; it does not say "and I accept that rules referencing
		// removed verbs will silently stop matching". Those are different
		// consents and the second one is the surprising half, so it is asked
		// for separately.
		//
		// A refusal rather than a warning: a warning here goes to daemon.log,
		// which is not where anyone is looking, and by the time the breakage
		// shows up it looks like an unrelated permissions problem.
		diff := diffPluginManifests(existing, manifest)
		if diff.Breaking() {
			// Ask the relay what the removal would cost. The daemon owns the
			// decision but not the data: it can see which verbs vanish, while
			// the rules naming them live on the server. Best-effort — an
			// unreachable relay must not block an install, and Describe()
			// reports the impact as unknown rather than implying it is zero.
			d.attachOrphanImpact(&diff, manifest.PluginSlug)
		}
		if diff.Breaking() && !req.PluginAllowBreaking {
			os.RemoveAll(stagingDir)
			outcome := &pluginInstallOutcome{
				PluginSlug: manifest.PluginSlug,
				Version:    manifest.Version,
				Diff:       &diff,
			}
			return outcome, fmt.Errorf("%s. Re-run with --allow-breaking to apply it anyway",
				diff.Describe(manifest.PluginSlug))
		}
		outcomeDiff = &diff
	}

	// Exec probe: make sure the OS can actually launch the bundled
	// binary on this host before swapping it into place. Catches
	// arch/format mismatches (Linux binary in a Mac tarball, etc.)
	// at install time rather than first agent invocation. Declarative
	// adapters have no executable to probe — manifest validation
	// already guaranteed Executable is empty in that case.
	if ClassifyManifestSource(manifest) == SourceBinary {
		if perr := probePluginExecutable(stagingDir, manifest.Executable); perr != nil {
			os.RemoveAll(stagingDir)
			return nil, fmt.Errorf("executable probe: %w", perr)
		}
	}

	// Atomic swap. Move the old install aside rather than deleting it, so a
	// bad update is one command to undo; MkdirAll the parent (namespaced
	// slugs like acme/ha need acme/ to exist); Rename staging into place.
	//
	// Retaining the previous version matters more here than for most
	// software: the thing most likely to be wrong with a plugin update is
	// not a crash but a behaviour change, which you discover through an
	// agent acting oddly some time later. By then re-fetching the old
	// version is not possible — the catalog index only ever describes
	// current versions.
	if berr := backupExistingInstall(d.pluginsDir, manifest.PluginSlug, finalDir); berr != nil {
		os.RemoveAll(stagingDir)
		return nil, fmt.Errorf("set aside existing install: %w", berr)
	}
	if merr := os.MkdirAll(filepath.Dir(finalDir), 0o755); merr != nil {
		os.RemoveAll(stagingDir)
		return nil, fmt.Errorf("mkdir parent: %w", merr)
	}
	if rerr := os.Rename(stagingDir, finalDir); rerr != nil {
		os.RemoveAll(stagingDir)
		return nil, fmt.Errorf("rename into place: %w", rerr)
	}

	// Refresh in-memory registry + push to server. Both errors are
	// logged but not surfaced: the disk install succeeded and the next
	// daemon boot would self-heal.
	if lerr := d.plugins.Load(d.pluginsDir); lerr != nil {
		log.Printf("plugin install: registry reload: %v", lerr)
	}
	go d.reportPluginInstallsAtBoot()

	out := &pluginInstallOutcome{
		PluginSlug: manifest.PluginSlug,
		Version:    manifest.Version,
		SourceDir:  finalDir,
		Diff:       outcomeDiff,
	}
	if index != nil {
		out.CatalogVersion = index.CatalogVersion
		out.Signature = index.Verification.Describe()
		out.SignatureVerified = index.Verification != nil && index.Verification.Verified
	}
	return out, nil
}

// handlePluginInstall is the IPC entry point for `hearth plugin install`.
func (d *Daemon) handlePluginInstall(conn net.Conn, req ipcRequest) {
	out, err := d.installPlugin(req)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: err.Error()})
		return
	}
	resp := map[string]string{
		"plugin_slug": out.PluginSlug,
		"version":     out.Version,
		"source_dir":  out.SourceDir,
	}
	if out.CatalogVersion != "" {
		resp["catalog_version"] = out.CatalogVersion
		// Report the signature outcome to whoever asked for the install.
		// The daemon logs it too, but daemon.log is not where an operator
		// is looking at the moment they need to know whether the thing
		// they just installed was actually verified.
		resp["signature"] = out.Signature
		if !out.SignatureVerified {
			resp["signature_unverified"] = "1"
		}
	}
	data, _ := json.Marshal(resp)
	sendControl(conn, ipcResponse{Type: "plugin_install_response", Data: data})

	// Report the success so the relay cycles agents bound to this plugin.
	// Without this an operator upgrading on the host leaves every running
	// agent following the previous version's skill — including calling verbs
	// the new version removed — with nothing to explain the failures.
	//
	// After sendControl, so the CLI is never left waiting on a server
	// round-trip for something that has already finished on disk.
	go d.reportInstallOutcome(map[string]interface{}{
		"plugin_slug":     out.PluginSlug,
		"ok":              true,
		"version":         out.Version,
		"catalog_version": out.CatalogVersion,
		"signature":       out.Signature,
	})
}

// installPluginFromServer runs an app-initiated install and reports the
// outcome back so the household sees what happened.
//
// The report is the whole reason this is not fire-and-forget. A SUCCESS is
// visible on its own — report_plugin_installs refreshes the list and the new
// version shows up — but a refusal leaves the list untouched, which looks
// exactly like "still working". Signature failures and version-floor refusals
// are precisely the outcomes someone needs told about, and they are the ones
// that would otherwise be silent.
// reportInstallOutcome tells the relay what an install did.
//
// Both entry points call this on success, because a completed install is a
// state change the household needs to know about regardless of who triggered
// it: the relay cycles every running agent bound to the plugin so it re-reads
// the new skill, and refreshes the plugin list on connected phones. Reporting
// only server-initiated installs — which is what this used to do — meant an
// operator running `hearth plugin install --upgrade` on the host left every
// agent on that host running the OLD skill, silently.
//
// FAILURES are reported only by the server-driven path, deliberately. There
// the report is the response channel: nothing else tells the person who
// tapped the button that their install was refused. A CLI operator already
// has the error on their terminal, and broadcasting a breaking-change panel
// to everyone's phone for something one person is actively looking at would
// be noise.
func (d *Daemon) reportInstallOutcome(result map[string]interface{}) {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		// The install still happened; the next report_plugin_installs on
		// reconnect carries the resulting state, just without the cycle.
		return
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return
	}
	if _, err := d.daemonWS.SendWSRequest(generateUUID(), "report_plugin_install_result", payload); err != nil {
		log.Printf("daemon: install result report failed: %v", err)
	}
}

func (d *Daemon) installPluginFromServer(slug, version string, upgrade, force, allowBreaking bool) {
	log.Printf("daemon-ws: install_plugin %s (version=%q upgrade=%v force=%v allow_breaking=%v)",
		slug, version, upgrade, force, allowBreaking)

	out, err := d.installPlugin(ipcRequest{
		PluginCatalogSlug:    slug,
		PluginCatalogVersion: version,
		PluginUpgrade:        upgrade,
		PluginForce:          force,
		PluginAllowBreaking:  allowBreaking,
	})

	result := map[string]interface{}{"plugin_slug": slug}
	// A breaking-change refusal carries the diff so the UI can offer "update
	// anyway" against a specific, named consequence rather than a generic
	// retry. Without it the operator is asked to confirm something they
	// cannot see.
	if out != nil && out.Diff != nil && out.Diff.Breaking() {
		result["breaking"] = true
		result["verbs_removed"] = out.Diff.VerbsRemoved
		result["config_keys_now_required"] = out.Diff.ConfigKeysNowRequired
		// The daemon already asked the relay for this while deciding whether
		// to refuse, so report it rather than making the relay compute the
		// same thing twice. One query, one number — two independently
		// computed counts that disagreed would be worse than none.
		if out.Diff.ImpactFetched {
			result["orphaned_rule_count"] = out.Diff.OrphanedRuleCount
			result["affected_agents"] = out.Diff.AffectedAgents
		}
	}
	if err != nil {
		log.Printf("daemon-ws: install_plugin %s failed: %v", slug, err)
		result["ok"] = false
		result["error"] = err.Error()
	} else {
		log.Printf("daemon-ws: install_plugin %s installed v%s (%s)",
			out.PluginSlug, out.Version, out.Signature)
		result["ok"] = true
		result["version"] = out.Version
		result["catalog_version"] = out.CatalogVersion
		result["signature"] = out.Signature
	}

	d.reportInstallOutcome(result)
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

// ---------- rollback ----------

// pluginBackupRoot is where the previous version of an install is kept.
//
// Deliberately a SIBLING of the plugins directory, never inside it. The
// registry scans pluginsDir and treats any directory containing a
// manifest.yaml as an install whose path must equal its plugin_slug — a
// backup living under pluginsDir would either be loaded as a bogus plugin or
// fail that invariant and take the whole registry down.
func pluginBackupRoot(pluginsDir string) string {
	return filepath.Join(filepath.Dir(pluginsDir), "plugin-backups")
}

func pluginBackupDir(pluginsDir, slug string) string {
	return filepath.Join(pluginBackupRoot(pluginsDir), filepath.FromSlash(slug))
}

// backupExistingInstall moves the current install aside so it can be restored.
// A no-op when nothing is installed yet.
//
// Only ONE generation is kept. A deeper history sounds better than it is: the
// realistic recovery is "undo what just happened", and keeping N versions
// means deciding which to restore, which is a decision nobody has enough
// information to make at the moment they need it.
func backupExistingInstall(pluginsDir, slug, finalDir string) error {
	if _, err := os.Stat(finalDir); os.IsNotExist(err) {
		return nil // first install, nothing to keep
	}
	backup := pluginBackupDir(pluginsDir, slug)
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("clear previous backup: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return fmt.Errorf("mkdir backup parent: %w", err)
	}
	if err := os.Rename(finalDir, backup); err != nil {
		// rename() needs write permission on the directory holding the
		// SOURCE, because it has to unlink that entry. So a plugin whose
		// parent directory this account cannot write to fails here — and the
		// bare syscall error ("rename ...: permission denied") names the
		// operation rather than the problem or the remedy.
		//
		// This is the normal state of any host whose plugins were placed by
		// hand before the catalog existed: the documented recipe copied them
		// in as root, and an intermediate namespace directory left
		// root-owned is invisible until the first upgrade tries to move it.
		// Hit on a live host 2026-07-21.
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf(
				"%s is not writable by this account, so the existing install cannot be "+
					"moved aside. Plugins installed by hand before the catalog existed are "+
					"often left root-owned. Fix with:\n"+
					"    sudo chown -R $(id -un):$(id -gn) %s\n"+
					"underlying error: %w",
				filepath.Dir(finalDir), filepath.Dir(pluginsDir), err)
		}
		return fmt.Errorf("move aside: %w", err)
	}
	return nil
}

// handlePluginRollback restores the version an install replaced.
//
// Swaps rather than overwrites: the version being rolled back FROM becomes
// the new backup, so a rollback is itself undoable. Someone who rolls back on
// a hunch and finds they were wrong should not have destroyed the thing they
// were trying to get away from.
func (d *Daemon) handlePluginRollback(conn net.Conn, req ipcRequest) {
	if req.PluginSlug == "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "plugin_slug required"})
		return
	}
	slug := req.PluginSlug
	backup := pluginBackupDir(d.pluginsDir, slug)
	finalDir := filepath.Join(d.pluginsDir, filepath.FromSlash(slug))

	prev, err := ReadManifestFile(filepath.Join(backup, "manifest.yaml"))
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error",
			Message: fmt.Sprintf("no previous version of %s to roll back to "+
				"(one is kept only after an upgrade replaces an existing install)", slug)})
		return
	}

	// Refuse to restore something this binary cannot run. Rolling back is
	// usually a response to trouble, and swapping in a plugin that then fails
	// its version floor would replace one broken state with a different one.
	if reason := checkMinDaemonVersion(version, prev.MinDaemonVersion); reason != "" {
		sendControl(conn, ipcResponse{Type: "error",
			Message: fmt.Sprintf("previous version of %s %s", slug, reason)})
		return
	}

	// Park the current install somewhere neutral first. Renaming the backup
	// over a live directory is not atomic, and a failure midway would leave
	// the plugin absent entirely rather than merely wrong.
	parked := backup + ".rollback-tmp"
	os.RemoveAll(parked)
	haveCurrent := false
	if _, serr := os.Stat(finalDir); serr == nil {
		if rerr := os.Rename(finalDir, parked); rerr != nil {
			sendControl(conn, ipcResponse{Type: "error", Message: "park current install: " + rerr.Error()})
			return
		}
		haveCurrent = true
	}

	if err := os.MkdirAll(filepath.Dir(finalDir), 0o755); err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "mkdir parent: " + err.Error()})
		return
	}
	if err := os.Rename(backup, finalDir); err != nil {
		// Put the current version back — better the state we started in than
		// no plugin at all.
		if haveCurrent {
			os.Rename(parked, finalDir)
		}
		sendControl(conn, ipcResponse{Type: "error", Message: "restore previous version: " + err.Error()})
		return
	}
	if haveCurrent {
		// The version we just rolled away from becomes the new backup, which
		// is what makes the rollback reversible.
		os.Rename(parked, backup)
	}

	if err := d.plugins.Load(d.pluginsDir); err != nil {
		log.Printf("plugin rollback: registry reload: %v", err)
	}
	go d.reportPluginInstallsAtBoot()

	resp, _ := json.Marshal(map[string]string{
		"plugin_slug": slug,
		"version":     prev.Version,
		"source_dir":  finalDir,
	})
	sendControl(conn, ipcResponse{Type: "plugin_rollback_response", Data: resp})

	// A rollback changes the version under running agents exactly as an
	// update does, so it needs the same cycle. Reverting to an older skill
	// while agents keep following the newer one is the same failure in the
	// other direction.
	go d.reportInstallOutcome(map[string]interface{}{
		"plugin_slug": slug,
		"ok":          true,
		"version":     prev.Version,
	})
}

// attachOrphanImpact fills in what a breaking update would cost, by asking
// the relay which rules name the verbs being removed.
//
// Deliberately non-fatal. A host that cannot reach the relay can still
// install and update plugins — the whole fetch-and-verify path is local to
// the daemon and its pinned key — so losing the impact detail must not lose
// the operation. What it must NOT do is leave the count at zero and let that
// read as "nothing breaks"; ImpactFetched carries that distinction.
func (d *Daemon) attachOrphanImpact(diff *pluginUpdateDiff, slug string) {
	if len(diff.VerbsRemoved) == 0 {
		return
	}
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"plugin_slug":   slug,
		"verbs_removed": diff.VerbsRemoved,
	})
	if err != nil {
		return
	}
	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "plugin_orphan_count", payload)
	if err != nil {
		log.Printf("plugin install: orphan count unavailable: %v", err)
		return
	}
	var resp struct {
		RuleCount      int      `json:"rule_count"`
		AffectedAgents []string `json:"affected_agents"`
		Error          string   `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		log.Printf("plugin install: orphan count reply unreadable: %v", err)
		return
	}
	// A reachable server that refuses the request looks identical to an
	// unreachable one from the caller's side, so say which. The likeliest
	// cause is a relay older than this daemon -- the handler does not exist
	// there yet -- and that is worth one log line rather than silence.
	if resp.Error != "" {
		log.Printf("plugin install: orphan count refused by server (%s); "+
			"relay may predate the plugin_orphan_count handler", resp.Error)
		return
	}
	diff.OrphanedRuleCount = resp.RuleCount
	diff.AffectedAgents = resp.AffectedAgents
	diff.ImpactFetched = true
}
