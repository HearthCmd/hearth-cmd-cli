# Resource plugins — phase 2 implementation plan

**Status:** planning. Branches `resource-plugins` (CLI head 8627d90,
1h-CLI landed; server head 21f8e0c, 1h-server landed; web head
21f8e0c too — same monorepo). Predecessor:
`docs/resource-plugins-2a-plan.md` (server-side plugin registry —
**required**; this plan FKs into it). Authoritative architecture:
`/Users/mattbeller/projects/hearth-cmd/docs/external-resource-adapters.md`
§"Plugin Types and Resource Connections".

**Goal of phase 2:** server stores Resource Connections; the
webview can list/create/delete them. The daemon stops reading
`HEARTH_DEV_CONNECTIONS` for the connection list (it still reads
yaml as a seed-credentials path only) and fetches from the server
at boot/reconnect.

**Deliberately not-thoughtful:** per scoping, the webview CRUD
surface is a temporary view — a holdover until the long-term UX
for permissions/plugins/connections placement gets sketched. Just
functional. No new affordances, no per-plugin custom forms, no
"connection health" indicators, no edit-in-place. Delete + recreate
is fine.

**Out of scope for phase 2:**
- Update / edit of an existing connection. Delete + recreate covers it.
- Per-plugin custom forms in the webview. A connection is
  {connection_id, plugin_install_id, display_name?} — no plugin-
  shape-aware fields.
- Mobile-driven secret setting from the webview. The
  `hearth secret set` CLI still owns that surface. The webview
  form has no credentials block.
- Retiring `HEARTH_DEV_CONNECTIONS` entirely. The yaml stays as
  the credential-seed bootstrap path; only the connection-list
  portion of it is superseded. (When phase 2 settles and the
  webview CRUD is loadbearing, retirement is a follow-up commit.)
- (No-longer-out-of-scope, since 2a lands first: server validates
  `(host_id, install_id)` against `plugin_installs` at create
  time. A mis-typed install_id is refused at the API, not at
  invoke time.)
- Host pinning UI beyond a host picker in the create form. A
  connection is explicitly bound to one host via the FK to
  `plugin_installs`. Beyond the picker, no host-aware affordances.
- Cross-org connection sharing. Connection rows are scoped to one
  org via `organization_id`. Same scope as rules.

## 1. Architecture summary

```
webview                  server                       daemon
───────                  ──────                       ──────
list connections ───►   SELECT FROM
                         resource_connections
                         WHERE org=...
                                ▲
create  ─────────►     INSERT (org, conn_id,
                       plugin_install_id, name)
delete  ─────────►     DELETE WHERE id=...

                                                     boot / reconnect
                                                          │
                       connections_list ◄────────────────┘
                       (org-scoped list)
                                                          ▼
                                                     DevConnectionStore.swap()
                                                     (in-memory; secrets
                                                      vault unchanged)
```

Server is SOT. Daemon's `devConnections` becomes a server-fed
cache. The yaml-bootstrap path stays for credential seeding only.

## 2. Storage shape — `resource_connections`

```sql
CREATE TABLE IF NOT EXISTS resource_connections (
    id              TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    host_id         TEXT NOT NULL,
    install_id      TEXT NOT NULL,
    display_name    TEXT,
    created_by      TEXT,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (host_id, install_id)
        REFERENCES plugin_installs (host_id, install_id)
);
CREATE INDEX IF NOT EXISTS idx_resource_connections_org
    ON resource_connections (organization_id);
```

`id` is the connection_id used by every other surface (rules,
secrets, invoke path). Operator-chosen; webview prompts for it.

`(host_id, install_id)` FKs into `plugin_installs` (sub-phase 2a).
A connection is explicitly bound to one host's installed plugin —
mis-typed install_id is refused at the API. The host pin matches
1d secrets-vault semantics (Fork-1 option c) and is no longer
implicit.

## 3. Wire shapes

```
msg_type=resource_connections_list  →  {connections:[{id, host_id, install_id, display_name}]}
msg_type=resource_connection_create →  {id, host_id, install_id, display_name?}
msg_type=resource_connection_delete →  {id}
```

All three go through `dispatchOrganizationCRUD`, same path the 1d
secrets endpoints use. Authz: standard org-membership gate (the
caller's currentOrgID).

## 4. Commit sequence

Six commits — server-then-daemon-then-webview, identical to 1h.

1. **server: schema + migration + helpers.** New
   `resource_connections` table, schema test, idempotent migration.
   Plus a `listResourceConnections(orgID)` / `createResourceConnection`
   / `deleteResourceConnection` helper trio. No WS endpoints yet.
2. **server: WS endpoints + dispatch routes.** Three handlers
   wired into `dispatchOrganizationCRUD`. Tests cover happy path
   + cross-org access refusal + duplicate-id rejection.
3. **CLI: daemon fetches connection list at boot + reconnect.**
   New goroutine analogous to `secretsBootstrapFromDevConnsAtBoot`
   that pulls the list and calls `devConnections.swap(...)`. The
   yaml `LoadFromEnv` still runs at boot but its connections get
   overwritten as soon as the server reply lands. Yaml-only
   credentials (still keyed by connection_id) survive — the
   bootstrap path reads them and pushes encrypted to the secrets
   vault for any connection that exists in either source.
4. **CLI: yaml is credential-seed-only.** Add a comment + log line
   explaining the new role. Keep yaml parsing unchanged (operators
   may still list connections in yaml for transitional setups);
   document that the connections list is informational once the
   daemon connects to the server.
5. **web: resource_connections store slice + list/create/delete
   actions.** New types in `store/types.ts`, store entries in
   `store/app.ts`, `wsRequest()` wrappers in `api/ws.ts`. The
   `resource_connections_list` push handler replaces the slice.
6. **web: temporary CRUD view.** New screen (e.g.
   `ResourceConnectionsScreen`) reachable from somewhere
   intentionally unloved — sheet pulled from a hidden affordance
   or a /debug-style route. List + "Add connection" form (id
   text input + host+install_id picker populated from the 2a
   `plugin_installs_list` reader + optional display name) +
   per-row delete button with a confirm.

The webview view doesn't need to look nice — per scoping. The
HTML can be a flat list with bare buttons; the CSS can copy
`AlwaysAllowMenu` patterns or just use defaults.

## 5. Dogfooding plan

After commit 6:
1. Pull, rebuild, redeploy server.
2. Open webview, navigate to the new connections screen.
3. Create `echo-test` (plugin_install_id `echo`).
4. `hearth secret set echo-test echo_token --value foo`.
5. Restart daemon; confirm log line shows it fetched the
   connection from server.
6. `hearth resource invoke echo-test whoami` works end-to-end.
7. Delete `echo-test` in the webview.
8. Daemon's next reconnect drops it from `devConnections`.
9. Invoke again → "unknown connection" error.

Failure modes worth poking:
- Webview create with a duplicate id → 409-style error rendered
  to the form.
- Cross-org access — alice can't see/delete bob's connections.
- Daemon offline at boot — falls back to whatever yaml carries,
  no crash.

## 6. Yaml retirement (not in phase 2)

Once the webview view is load-bearing, retire
`HEARTH_DEV_CONNECTIONS` as a separate commit. The memory
`project_secrets_yaml_bootstrap_temporary` will be the trigger to
remember to do this. Until then yaml stays.
