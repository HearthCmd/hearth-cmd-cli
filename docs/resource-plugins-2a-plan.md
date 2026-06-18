# Resource plugins — sub-phase 2a implementation plan

**Status:** planning. Branches `resource-plugins` (CLI head 8627d90,
phase 2 plan landed; server head 21f8e0c, 1h-server landed).
Predecessor: `docs/resource-plugins-1h-plan.md`. Successor:
`docs/resource-plugins-2-plan.md` (connections CRUD, builds on
this). Authoritative architecture:
`/Users/mattbeller/projects/hearth-cmd/docs/external-resource-adapters.md`
§"Plugin Types and Resource Connections".

**Goal of 2a:** plugins become a server-side table. The daemon
reports the plugins it discovered under `~/.hearth/plugins/*` on
boot/reconnect; the server stores them keyed by `(host_id,
install_id)`. After 2a, the answer to "which plugins are
installed across this org" is one SQL query, not a fan-out poll.
Phase 2 (connections) builds on this — connection rows FK into
the plugin table so the freeform-string footgun never lands.

**Why before phase 2:** avoids re-migrating connections. Without
this, phase 2 ships `plugin_install_id` as a freeform string and
we'd rewrite it later. Also unblocks the webview create-connection
picker — operator picks from real installed plugins rather than
typing a magic string and hoping.

**Out of scope for 2a:**
- Server-side plugin install/upgrade flow ("hearth plugin install
  <url>"). 2a is a registry, not an installer. Operator still
  copies the plugin into `~/.hearth/plugins/` by hand.
- Manifest schema in the DB row. We store enough to identify
  (host_id, install_id, plugin_type, version); the full manifest
  (verbs, credentials, default_rules) stays parsed-on-demand
  from the daemon's local copy. The future install flow can
  revisit if needed.
- Cross-host plugin coordination (e.g. "all hosts must run the
  same version"). Each host reports independently; phase 2 lets
  operators pick which host's installation to bind a connection to.
- Removal-detection of plugins. If the operator deletes a plugin
  directory, the daemon's next report just omits it; server
  marks rows for that host that weren't in the report as stale
  (timestamp), but doesn't auto-delete. Cleanup is a follow-up.

## 1. Architecture summary

```
daemon (host Y)             server
─────────────              ──────
plugin registry scans      ┌─────────────────────┐
~/.hearth/plugins/*  ───►  │ report_plugin_installs
on boot/reconnect          │   {plugins:[{install_id,
                           │     plugin_type, version}]}
                           └────────┬────────────┘
                                    ▼
                           upsert into plugin_installs
                           (host_id, install_id, ...)
                           mark host's omitted rows stale
                           (last_seen_at unchanged)
                                    ▲
                                    │
webview / future CRUD ────►  listPluginInstallsInOrg(orgID)
                             JOIN hosts ON organization_id
```

Daemon: one push at boot, one on every reconnect. No live updates
mid-session — adding a plugin requires a daemon restart, same as
today.

Server: idempotent upsert on `(host_id, install_id)`. Rows that
weren't in the latest report from a given host get `last_seen_at`
left stale; cleanup is operator-driven via a future "forget host"
command.

## 2. Storage shape — `plugin_installs`

```sql
CREATE TABLE IF NOT EXISTS plugin_installs (
    host_id      TEXT NOT NULL,
    install_id   TEXT NOT NULL,
    plugin_type  TEXT NOT NULL,
    version      TEXT,
    last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (host_id, install_id)
);
CREATE INDEX IF NOT EXISTS idx_plugin_installs_org_via_host
    ON plugin_installs (host_id);
```

PK `(host_id, install_id)` — same plugin installed at two
directory names on the same host gets two rows. Different hosts
with the same `install_id` get separate rows.

Phase-2 `resource_connections.host_id + plugin_install_id` will
FK into this composite PK.

## 3. Wire shapes

```
daemon → server:
  msg_type=report_plugin_installs
  {plugins:[{install_id, plugin_type, version}]}

server → webview (or any consumer):
  msg_type=plugin_installs_list
  {plugin_installs:[{host_id, install_id, plugin_type, version, last_seen_at}]}
```

`report_plugin_installs` is daemon-only — server intercepts in
the daemon ws_request branch (same place the 1d secrets daemon-
only messages are intercepted).

`plugin_installs_list` is the consumer-facing read. Standard
org-scoped (joins via hosts). Used by phase 2's create-connection
picker; available now to any future surface.

## 4. Commit sequence

Four commits:

1. **server: schema + migration + helpers.** New `plugin_installs`
   table, schema-landing test, `upsertPluginInstall(host_id, …)`
   helper, `listPluginInstallsInOrg(orgID)` helper. No WS endpoint
   yet.
2. **server: `report_plugin_installs` handler (daemon-only) +
   `plugin_installs_list` reader.** Daemon-only intercept for the
   write; list reader goes through `dispatchOrganizationCRUD`.
   Tests cover happy path, cross-host isolation, and the "host A
   omits a plugin → row's last_seen_at stays stale" pattern.
3. **CLI: daemon reports plugin installs at boot + reconnect.**
   New goroutine pulls the list out of `plugins.List()` (the
   in-memory registry built by `LoadPlugins`) and sends one
   `report_plugin_installs`. Triggered after `d.daemonWS` is
   constructed, alongside the existing secrets bootstrap and
   resource-rule-seed goroutines.
4. **CLI: report on every reconnect, not just boot.** Wire into
   the DaemonWS reconnect callback so the server's view stays
   accurate after a daemon flap. Tests cover the reconnect path.

After commit 4, `plugin_installs` is the source of truth for
"what's installed where." Phase 2 (connection CRUD) consumes it.

## 5. Dogfooding plan

After commit 4:
1. Pull, rebuild, redeploy server.
2. Restart daemon; check log line shows N plugins reported.
3. `sqlite3` the server DB: `SELECT * FROM plugin_installs;` —
   echo (and anything else under `~/.hearth/plugins/`) is there.
4. `rm -rf ~/.hearth/plugins/echo` (or move it aside); restart
   daemon. Row stays in the table (last_seen_at unchanged) — a
   "stale" plugin row, not deleted.
5. Put echo back, restart again. Row's last_seen_at updates.

Phase 2's create-connection form will then have a real picker
backed by this table.
