//go:build darwin || linux

package main

import (
	"encoding/json"
	"log"
)

// reportPluginInstallsReq mirrors the server-side wire shape in
// hearth-cmd/cmd/hearth-cloud/plugin_installs_ws.go. Kept in
// sync by hand. Per docs/resource-plugins-2a-plan.md.
type reportPluginInstallsReq struct {
	Plugins []reportedPluginInstall `json:"plugins"`
}

type reportedPluginInstall struct {
	PluginSlug  string `json:"plugin_slug"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	// DefaultRules is the manifest's default_rules array, sent
	// verbatim so the server can store + later seed per-agent
	// grants without a daemon round-trip. nil/empty when the
	// manifest declared no defaults (legal).
	DefaultRules json.RawMessage `json:"default_rules,omitempty"`
	// ConfigSchema is the manifest's config_schema object, sent
	// verbatim so the server can render new-connection forms and
	// validate resource_connections.config at write time. nil/empty
	// when the manifest declared no schema (config unconstrained).
	// Phase 3 step 9.
	ConfigSchema json.RawMessage `json:"config_schema,omitempty"`
	// Source classifies execution path; 'binary' or 'declarative'.
	// Computed at manifest-load time via ClassifyManifestSource.
	// Empty in reports from pre-source-aware daemons; the server
	// normalizes to 'binary' on upsert. See
	// hearth-cmd/docs/ha-yaml-adapter-plan.md.
	Source string `json:"source,omitempty"`
	// CredentialSpecs is the manifest's credentials array, marshalled
	// to JSON so the server can store and forward it to the webview
	// wizard. Stores credential spec/metadata (names, descriptions,
	// wizard guidance) — NOT actual credential values.
	CredentialSpecs json.RawMessage `json:"credential_specs,omitempty"`
	// AuthScheme is the manifest's auth_scheme value. Informational;
	// stored on plugin_installs for the install wizard badge.
	AuthScheme string `json:"auth_scheme,omitempty"`
}

// reportPluginInstallsAtBoot pushes the local plugin registry's
// view to the server so the org-wide list (joined via
// plugin_installs_list) reflects this host. Kicked alongside
// the existing rule-seed / pubkey-enroll / secrets-bootstrap
// goroutines after d.daemonWS is constructed.
//
// Best-effort: a missing WS / failed call logs and moves on. The
// server view goes stale rather than wrong; the next reconnect
// (commit 4) re-reports.
func (d *Daemon) reportPluginInstallsAtBoot() {
	if d.plugins == nil {
		return
	}
	manifests := d.plugins.ListPlugins()
	if len(manifests) == 0 {
		return
	}
	if !d.waitForDaemonWS(seedWSConnectTimeout) {
		log.Printf("daemon: report_plugin_installs skipped: WS did not connect within %s", seedWSConnectTimeout)
		return
	}

	out := make([]reportedPluginInstall, 0, len(manifests))
	for _, m := range manifests {
		var defaultRules json.RawMessage
		if len(m.DefaultRules) > 0 {
			// Re-marshal the manifest's typed slice into raw JSON for
			// wire transport. Errors here are theoretically impossible
			// (the slice came from a successful yaml unmarshal), so we
			// log and skip default_rules rather than fail the report.
			if b, err := json.Marshal(m.DefaultRules); err == nil {
				defaultRules = b
			} else {
				log.Printf("daemon: report_plugin_installs marshal default_rules for %s: %v", m.PluginSlug, err)
			}
		}
		var configSchema json.RawMessage
		if len(m.ConfigSchema) > 0 {
			// Re-marshal the parsed schema map for the wire. Same
			// best-effort posture as default_rules: log and ship
			// without the schema rather than failing the report.
			if b, err := json.Marshal(m.ConfigSchema); err == nil {
				configSchema = b
			} else {
				log.Printf("daemon: report_plugin_installs marshal config_schema for %s: %v", m.PluginSlug, err)
			}
		}
		var credentialSpecs json.RawMessage
		if len(m.Credentials) > 0 {
			if b, err := json.Marshal(m.Credentials); err == nil {
				credentialSpecs = b
			} else {
				log.Printf("daemon: report_plugin_installs marshal credential_specs for %s: %v", m.PluginSlug, err)
			}
		}
		out = append(out, reportedPluginInstall{
			PluginSlug:      m.PluginSlug,
			DisplayName:     m.DisplayName,
			Description:     m.Description,
			Version:         m.Version,
			DefaultRules:    defaultRules,
			ConfigSchema:    configSchema,
			Source:          m.Source,
			CredentialSpecs: credentialSpecs,
			AuthScheme:      m.AuthScheme,
		})
	}
	payload, err := json.Marshal(reportPluginInstallsReq{Plugins: out})
	if err != nil {
		log.Printf("daemon: report_plugin_installs marshal: %v", err)
		return
	}
	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "report_plugin_installs", payload)
	if err != nil {
		log.Printf("daemon: report_plugin_installs ws_request: %v", err)
		return
	}
	var resp struct {
		Type     string `json:"type"`
		Error    string `json:"error"`
		Upserted int    `json:"upserted"`
		Reported int    `json:"reported"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		log.Printf("daemon: report_plugin_installs decode: %v", err)
		return
	}
	if resp.Type == "error" {
		log.Printf("daemon: report_plugin_installs server error: %s", resp.Error)
		return
	}
	log.Printf("daemon: reported %d plugin install(s) to server (upserted %d)",
		resp.Reported, resp.Upserted)
}
