# Resource plugins — sub-phase 2b implementation plan

**Status:** planning. Branches `resource-plugins` (CLI head f45ab08;
server head e55fb1f; phase 2 + 2a landed end-to-end and dogfooded
2026-05-15). Predecessor:
`docs/resource-plugins-2-plan.md`. Authoritative architecture:
`/Users/mattbeller/projects/hearth-cmd/docs/external-resource-adapters.md`.

**Goal of 2b:** `HEARTH_DEV_CONNECTIONS` yaml support is removed
entirely, and connection changes made in the webview show up on
the daemon without a restart. Two threads, one phase:

1. **Live push**: server fans out a `resource_connections_changed`
   nudge to in-org daemons after every create/delete; daemon
   re-fetches and swaps. Removes the daemon-restart dance.

2. **Yaml retirement**: HEARTH_DEV_CONNECTIONS parsing + bootstrap
   goroutine deleted. The resolver's secret:false branch — today
   the only consumer of yaml-loaded credentials — is unified onto
   the secrets vault. `hearth secret set` becomes the sole
   credential-setting path until webview crypto ships (later
   phase).

**Out of scope for 2b:**
- Webview-driven secret setting. Per the scoping decision: keep
  `hearth secret set` as the secret-setting UX; webview crypto
  (X25519 + ChaCha20Poly1305 in the browser) is a future phase.
- Connection update / edit. Still delete-and-recreate.
- Server-side validation that an operator-created connection is
  on a host the operator has rights to manage. Org membership is
  the only gate.
- One-shot "migrate yaml to vault" CLI helper. Operators run
  `hearth secret set <conn> <key> --value <v>` per credential.
  Migration is cheap given typical credential counts; automation
  is overkill for the transition.

## 1. Architecture summary

```
                                                      ┌──────────┐
webview create/delete ───►  server  ─────fanout──────►│ daemon A │
                              │       resource_connections_changed
                              │                       └────┬─────┘
                              │                            │ refetch
                              │                            ▼
                              │                       devConnections.swap
                              │
                              └─────fanout──────────► (other in-org daemons)

resolver (plugin Init):
   for every declared credential, regardless of secret flag:
       fetch from vault, decrypt, pass to plugin
   (the `secret:` flag becomes a UI-masking/scrubber hint only;
    storage is uniform)

removed:
   HEARTH_DEV_CONNECTIONS yaml parser + LoadFromEnv
   secretsBootstrapFromDevConnsAtBoot
   DevConnection.Credentials map (no longer populated)
```

## 2. Wire shapes

```
server → daemon (in-org, broadcast):
  msg_type=resource_connections_changed
  {organization_id, change_kind: "create" | "delete", connection_id}
```

`connection_id` is informational — daemon refetches the full
list regardless. The frame's role is "something changed,
refresh," not "here's the delta."

Daemon receives via a new text-frame branch; processing is
"kick `fetchResourceConnectionsAtBoot` in a goroutine."

## 3. Commit sequence

Five commits, server-then-CLI, narrowly scoped each:

1. **server: emit resource_connections_changed after create/delete.**
   Helper `notifyResourceConnectionsChanged(orgID, kind, id)`
   walks daemonConns for daemons whose host belongs to the org
   and writes the frame. Called from the create + delete handlers
   on success. Tests: a daemon registered for the org receives
   the frame; a daemon in a different org doesn't.
2. **CLI: daemon refetches on resource_connections_changed.**
   New text-frame handler; kicks `fetchResourceConnectionsAtBoot`.
   Unit test for the frame parse + dispatch; the refetch itself
   is the same code path 2a-3 already exercised.
3. **CLI: assembleInitCredentials uses the vault for every
   declared credential.** The `secret:false` short-circuit
   through `devConn.Credentials` is removed; resolver always
   does a `fetchVaultSecret`. Tests update to seed the vault
   (or mock the WS) for previously-yaml-only credentials.
4. **CLI: drop HEARTH_DEV_CONNECTIONS parsing + bootstrap.**
   `dev_connections.go` LoadFromEnv removed (or reduced to a
   no-op that logs a one-time deprecation if the env var is
   still set, then removed in a follow-up). `secrets_bootstrap.go`
   deleted entirely. DevConnection.Credentials field removed —
   server-fetched entries never populate it. Tests deleted /
   updated.
5. **docs + memories cleanup.**
   - Delete `project_secrets_yaml_bootstrap_temporary.md`
     memory; the bootstrap no longer exists.
   - Update README / any docs that reference HEARTH_DEV_CONNECTIONS.
   - Note in architecture doc that secret-setting is `hearth secret set`
     until webview crypto lands.

## 4. Migration path for existing operators

For each connection that has secret:false credentials in
`HEARTH_DEV_CONNECTIONS`, run once:

```
hearth secret set <connection_id> <key_name> --value <v>
```

After this, restart the daemon. The vault has every credential;
the yaml file can be deleted.

If a connection has no secret:false credentials (echo-test is
this case — echo_endpoint isn't actually consulted), nothing to
migrate.

## 5. Dogfooding plan

After commit 5:
1. Pull, rebuild, redeploy server.
2. Confirm `hearth start` works without `HEARTH_DEV_CONNECTIONS`
   set (env var is now ignored).
3. Create / delete a connection in the webview — daemon log
   shows "resource_connections fetched" within ~1s without a
   restart.
4. `hearth resource invoke echo-test whoami` works against a
   connection whose echo_endpoint was set via
   `hearth secret set` (no yaml).

Failure modes:
- Plugin declares a credential nobody set → `assembleInitCredentials`
  surfaces "vault has no entry for <conn>/<key>." Clear local
  error.
- Daemon offline when the live-push fires → reconnect already
  refetches (2a-4); no additional handling needed.
