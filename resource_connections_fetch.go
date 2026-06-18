//go:build darwin || linux

package main

import (
	"encoding/json"
	"log"
	"net"
	"time"
)

// snapshotStaleAfter is the threshold beyond which handleResourceInvoke
// logs a "snapshot is stale" warning. The cache itself continues to
// serve until the user runs `hearth resource refresh` (or until a
// future commit auto-refreshes on stale).
const snapshotStaleAfter = 7 * 24 * time.Hour

// handleResourceList is the daemon's IPC handler for `hearth resource
// list`. Round-trips to the server with host_id = d.hostID so the CLI
// sees only this host's connections — fresh state every call, no
// reliance on the boot-time in-memory cache (which carries only
// id/slug/host_id and would force widening to render display_name).
func (d *Daemon) handleResourceList(conn net.Conn, _ ipcRequest) {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		sendControl(conn, ipcResponse{Type: "error", Message: "daemon offline from server"})
		return
	}
	payload, _ := json.Marshal(map[string]string{"host_id": d.hostID})
	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "resource_connections_list", payload)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "ws_request: " + err.Error()})
		return
	}
	enriched := d.enrichResourceListWithSnapshotAges(raw)
	sendControl(conn, ipcResponse{Type: "resource_list_response", Data: enriched})
}

// enrichResourceListWithSnapshotAges decodes the server's
// resource_connections_list payload, looks up each declarative
// connection's latest snapshot timestamp in the daemon-local
// resource_entities cache, and re-encodes with an extra field. Only
// declarative installs get the field; binary plugins have no snapshot
// semantics. Best-effort: any decode/encode failure returns the
// original payload unchanged so the CLI still sees something.
func (d *Daemon) enrichResourceListWithSnapshotAges(raw []byte) json.RawMessage {
	if d.localDB == nil || d.plugins == nil {
		return json.RawMessage(raw)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return json.RawMessage(raw)
	}
	connsRaw, ok := envelope["resource_connections"]
	if !ok {
		return json.RawMessage(raw)
	}
	var conns []map[string]any
	if err := json.Unmarshal(connsRaw, &conns); err != nil {
		return json.RawMessage(raw)
	}
	for _, c := range conns {
		slug, _ := c["plugin_slug"].(string)
		id, _ := c["id"].(string)
		if slug == "" || id == "" {
			continue
		}
		manifest, ok := d.plugins.GetPluginBySlug(slug)
		if !ok || manifest.Source != SourceDeclarative {
			continue
		}
		found, t, err := d.localDB.LatestEntityFetchedAt(id)
		if err != nil {
			log.Printf("daemon: list-enrich %s: %v", id, err)
			continue
		}
		if !found {
			c["entity_snapshot_at"] = nil
			c["entity_snapshot_age_seconds"] = nil
			continue
		}
		c["entity_snapshot_at"] = t.UTC().Format(time.RFC3339)
		c["entity_snapshot_age_seconds"] = int(time.Since(t).Seconds())
	}
	patched, err := json.Marshal(conns)
	if err != nil {
		return json.RawMessage(raw)
	}
	envelope["resource_connections"] = patched
	out, err := json.Marshal(envelope)
	if err != nil {
		return json.RawMessage(raw)
	}
	return json.RawMessage(out)
}

// resourceConnectionsListResp mirrors the server-side response in
// hearth-cmd/cmd/hearth-cloud/resource_connections_ws.go. Kept in
// sync by hand. Per docs/resource-plugins-2-plan.md.
type resourceConnectionsListResp struct {
	Type                string                     `json:"type"`
	Error               string                     `json:"error"`
	ResourceConnections []serverResourceConnection `json:"resource_connections"`
}

// serverResourceConnection is what the server hands back in the
// list response. PluginInstallID is the server-internal UUID FK;
// the daemon doesn't need it directly — it operates on PluginSlug
// (joined in by the server) to resolve to its local install.
type serverResourceConnection struct {
	ID              string `json:"id"`   // UUID
	Slug            string `json:"slug"` // snake_case short name
	PluginInstallID string `json:"plugin_install_id"`
	HostID          string `json:"host_id"`
	PluginSlug      string `json:"plugin_slug"`
	DisplayName     string `json:"display_name,omitempty"`
	// Config is the non-sensitive per-connection JSON blob. Carried
	// into the daemon's ResourceConnection cache so declarative-verb
	// templates can reference `{{config.x}}` at invoke time. Empty
	// from older server builds; the executor treats that as `{}`.
	Config string `json:"config,omitempty"`
}

// fetchResourceConnectionsAtBoot pulls the server's authoritative
// connection list and merges it into resourceConnections. Per phase 2:
// the server is SOT for "which connections exist." Post-2b the
// in-memory store is fed by this function only — yaml is gone.
//
// Merge semantics (see mergeServerConnections):
//   - Server entries pinned to this host (row.host_id == d.hostID)
//     populate the new store.
//   - Server entries pinned to OTHER hosts are ignored. Different
//     daemons see only their own.
func (d *Daemon) fetchResourceConnectionsAtBoot() {
	if d.resourceConnections == nil || d.daemonWS == nil {
		return
	}
	if !d.waitForDaemonWS(seedWSConnectTimeout) {
		log.Printf("daemon: fetchResourceConnections skipped: WS did not connect within %s", seedWSConnectTimeout)
		return
	}

	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "resource_connections_list", nil)
	if err != nil {
		log.Printf("daemon: resource_connections_list ws_request: %v", err)
		return
	}
	var resp resourceConnectionsListResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		log.Printf("daemon: resource_connections_list decode: %v", err)
		return
	}
	if resp.Type == "error" {
		log.Printf("daemon: resource_connections_list server error: %s", resp.Error)
		return
	}

	next, kept, skipped := mergeServerConnections(
		d.resourceConnections.List(), resp.ResourceConnections, d.hostID,
	)
	d.resourceConnections.swap(next)
	log.Printf("daemon: resource_connections fetched — %d for this host, %d for other hosts ignored",
		kept, skipped)
}

// mergeServerConnections produces the new resourceConnections map for
// the swap. Pure (no WS, no resourceConnections mutation) so the
// this-host filter rule gets a focused unit test. Post-2b: no
// yaml credential preservation — the in-memory store carries
// connection metadata only.
func mergeServerConnections(
	_ []ResourceConnection,
	server []serverResourceConnection,
	thisHostID string,
) (next map[string]ResourceConnection, kept, skippedOtherHost int) {
	next = map[string]ResourceConnection{}
	for _, sc := range server {
		if sc.HostID != thisHostID {
			skippedOtherHost++
			continue
		}
		next[sc.ID] = ResourceConnection{
			ConnectionID: sc.ID,
			Slug:         sc.Slug,
			PluginSlug:   sc.PluginSlug,
			HostID:       sc.HostID,
			Config:       sc.Config,
		}
		kept++
	}
	return next, kept, skippedOtherHost
}
