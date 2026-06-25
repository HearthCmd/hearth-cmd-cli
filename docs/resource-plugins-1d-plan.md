# Resource plugins — sub-phase 1d implementation plan

**Status:** planning. Branches `resource-plugins` (CLI head 4258e30,
1f landed; server head 8dee38a, 1f landed). Predecessor:
`docs/resource-plugins-1f-plan.md`. Authoritative architecture:
`/Users/mattbeller/projects/hearth-cmd/docs/external-resource-adapters.md`
§"Credential broker".

**Goal of 1d:** plugin credentials stop living in plaintext on the
daemon host. They get stored encrypted on the server via the
hearth X25519 + ChaCha20Poly1305 envelope, decrypted only
inside the plugin's owning daemon process, and passed to the plugin
via Init params. Plugin stdout/stderr is scrubbed for credential
echoes as a backstop.

**Out of scope for 1d:**
- Mobile UX for secret CRUD. Phone-side flow is server-ready (the
  WS endpoints we ship would accept a phone client encrypting
  locally to the host's pubkey), but Swift-side crypto and UI are
  separate workstreams. Confirmed in scoping discussion.
- Multi-host failover for connections. Each connection is pinned
  to one host (Fork-1 option (c)). Multi-host needs are handled by
  the operator running `secret set` on each host separately —
  schema accepts N rows per (connection, key) differing by host_id.
- Other vault scope kinds (`human`, `position`, `agent`, `host`).
  Schema is generalized as `(scope_kind, scope_value, key_name)`
  but only `kind=connection` is wired this sub-phase.
- Out-of-band pubkey verification (QR / fingerprint). The phone +
  daemon trust the server's claim about host pubkeys. Same trust
  model as everything else; flag for hardening pre-launch.
- Key rotation UX. "Rotate refuses if secrets exist unless
  confirm-wipe" carries over verbatim, but the UX to drive it lives
  later.

## 1. Architecture summary

```
phone (or CLI)              server                       daemon (host Y)
─────────────              ──────                       ──────────────
fetch host Y pubkey ────► returns Y_pub
encrypt cleartext
  with Y_pub locally
  (X25519 envelope)
secrets_put ─────────────► validate writer authz,
{scope, value, key,         store {scope, value, key,
 host_id, ciphertext}        host_id, ciphertext} ◄──── secrets_get at Init
                             server NEVER decrypts        returns ciphertext
                                                          ▲
                                                          │ daemon decrypts with
                                                          │ its own privkey,
                                                          │ passes via Init params
                                                          ▼
                                                       plugin process
                                                          │
                                                          │ stdout/stderr piped
                                                          │ through scrubber
                                                          ▼
                                                       agent terminal / log
```

End-to-end: cleartext lives on the originating device (phone or
operator CLI) and inside the daemon's plugin process. The server
holds ciphertext only. Plugin output is scrubbed before reaching
the agent.

## 2. Wire shapes

### Server-side, msg_type=`secrets_put`

```json
{
  "scope_kind":   "connection",     // 1d only kind wired
  "scope_value":  "echo-test",      // connection_id
  "key_name":     "ha_token",       // manifest-declared name
  "host_id":      "<host uuid>",    // pinning target
  "ciphertext":   "<base64 envelope>",
  "expires_at":   null              // optional
}
```

Server validates:
- Authenticated principal (daemon or io_device) is owned by the
  user that owns this connection.
- For `kind=connection`: the named key matches a credential
  declared in the connection's plugin manifest.
- Recipient pubkey for `host_id` exists in the host record
  (validation is shape-only; server can't actually verify the
  ciphertext was encrypted to it).

INSERT OR REPLACE on `(scope_kind, scope_value, key_name, host_id)`
so re-setting overwrites cleanly. Returns
`{"type":"secrets_put_response","stored":1}`.

### Server-side, msg_type=`secrets_get`

```json
{
  "scope_kind":   "connection",
  "scope_value":  "echo-test",
  "key_name":     "ha_token"
}
```

Server resolves `host_id` via the calling daemon's authenticated
host. If no row matches `(scope_kind, scope_value, key_name,
host_id=<this daemon>)`, returns
`{"type":"error","error":"secret not found for this host"}`. The
daemon will then propagate "missing credential" to the plugin
caller.

Returns:
```json
{
  "type":      "secrets_get_response",
  "ciphertext":"<base64 envelope>",
  "expires_at": null
}
```

### Server-side, msg_type=`secrets_list`

```json
{
  "scope_kind":  "connection",       // optional filter
  "scope_value": "echo-test"         // optional filter
}
```

Returns metadata only, never ciphertext:
```json
{
  "type": "secrets_list_response",
  "secrets": [
    {"scope_kind":"connection", "scope_value":"echo-test",
     "key_name":"ha_token", "host_id":"...", "set_by":"<io_device id>",
     "set_at":"...", "expires_at": null}
  ]
}
```

### Server-side, msg_type=`secrets_delete`

```json
{
  "scope_kind":   "connection",
  "scope_value":  "echo-test",
  "key_name":     "ha_token",
  "host_id":      "<host uuid>"   // required: secrets are per-host-pinned
}
```

### Server-side, msg_type=`host_pubkey_get`

```json
{ "host_id": "<host uuid>" }
```

Returns:
```json
{ "type":"host_pubkey_response", "pubkey":"<base64 X25519 32-byte>" }
```

Phone calls this before encrypting. Daemons doing local
yaml-bootstrap don't need it (they encrypt to their own pubkey).

### Daemon ↔ plugin, Init params (unchanged)

```json
{"id":"1","method":"Init",
 "params":{"connection_id":"echo-test",
           "credentials":{"ha_url":"...","ha_token":"<cleartext>"}}}
```

Same shape as 1b. The daemon's NEW work is fetching ciphertext for
each manifest-declared credential and decrypting before assembling
this map.

## 3. Schema

### Server-side: new `host_public_keys` column on `hosts`

Single column add: `public_key BLOB` (raw 32-byte X25519). NULL
until the daemon enrolls its key (1d commit 2). Phones fetch via
`host_pubkey_get`.

### Server-side: new `secrets` table

```sql
CREATE TABLE IF NOT EXISTS secrets (
    scope_kind   TEXT NOT NULL,         -- 'connection' (1d), more later
    scope_value  TEXT NOT NULL,         -- e.g. connection_id
    key_name     TEXT NOT NULL,         -- e.g. 'ha_token'
    host_id      TEXT NOT NULL REFERENCES hosts(host_id) ON DELETE CASCADE,
    ciphertext   BLOB NOT NULL,
    expires_at   DATETIME,
    set_by       TEXT,                  -- io_device_id (phone) or 'daemon-bootstrap'
    set_at       DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (scope_kind, scope_value, key_name, host_id)
);
```

Generalized `(scope_kind, scope_value)` key is forward-compat per
scoping discussion — only `'connection'` writes are accepted in 1d
(write handler validates).

## 4. Daemon-side changes (hearth-cmd-cli)

### Crypto primitives

`secrets_crypto.go` implements:
- `generateKeypair() (*ecdh.PrivateKey, error)`
- `encryptSecret(recipientPub *ecdh.PublicKey, plaintext []byte) ([]byte, error)`
- `decryptSecret(priv *ecdh.PrivateKey, blob []byte) ([]byte, error)`
- Helper for marshaling pubkeys to/from base64 for wire transport.

Wire format: `ephemeral_pubkey(32) || nonce(12) || ciphertext+tag`.

### Key management

`~/.hearth/key` (mode 0600, dir 0700). Daemon checks at boot:
- File exists → load private key, derive pubkey.
- File missing → generate keypair, write private key, derive pubkey.
- File unreadable / malformed → fatal error (refuse to start).

After the daemon's WS connects + authenticates, send
`enroll_host_pubkey {pubkey: <base64>}` to register the public
half on the server's `hosts.public_key`. Idempotent — sending the
same pubkey is a no-op; sending a different one triggers a
"rotation requires confirm-wipe" path (deferred to 1d cleanup).

### Secret-fetch at plugin Init

The existing `StartPlugin` flow reads `credentials` from
`DevConnection.Credentials` (the plaintext yaml map). 1d replaces
this with:

```go
creds := map[string]string{}
for _, c := range manifest.Credentials {
    if !c.Secret {
        // Non-secret config (e.g. ha_url) stays in the yaml path.
        // Architecture explicitly distinguishes config from secret.
        creds[c.Name] = devConn.Credentials[c.Name]
        continue
    }
    raw, err := d.fetchSecret("connection", connID, c.Name)
    if err != nil { return ..., err }
    plain, err := decryptSecret(d.privateKey, raw)
    if err != nil { return ..., err }
    creds[c.Name] = string(plain)
    // zero plaintext-bearing slice on Init exit (best-effort)
}
```

`fetchSecret` is a thin wrapper around `SendWSRequest` for
`secrets_get`. Decryption is local, in-memory only.

### YAML bootstrap path

On daemon boot, after the supervisor + dev connections wire up:

```
for each devConnection:
  for each c in connection's plugin manifest.Credentials where c.Secret:
    if devConn.Credentials[c.Name] != "" AND server doesn't already have it:
      encrypt to own pubkey, secrets_put
```

Idempotent: if the server already has the secret for this
(connection, key, host_id), skip. Logs at INFO level. **Does not**
delete or modify the yaml file — operator hygiene applies.

### Stdout/stderr scrubber

`secrets_scrubber.go` implements `encodingsOf` + the streaming
scrub function. Covers 8 encoding forms (plaintext, hex up/down,
base64 std/url × padded/unpadded, URL-encoded).

Wire into `plugin_process.go`:
- `forwardStderr`: wrap the bufio.Scanner with a scrubber that
  carries a buffer of `maxFormLen-1` bytes to handle splits.
- `Invoke`: scrub `result.Stdout` before returning to the caller.
  This is single-frame (no streaming carry needed since the
  result comes back in one Scanner.Scan).

The scrubber needs the live secret values. Option: PluginProcess
holds a reference to the credential map it Init'd with, and the
scrubber runs against those values. Trade-off: the credential map
has to live in process memory for the process's lifetime anyway
(plugins might log warnings late in the process), so this is just
a reference.

### CLI subcommand: `hearth secret`

Three operator commands. All operate against the local daemon's
WS via the `secrets_*` endpoints.

```
hearth secret list [--connection <id>]
hearth secret set <connection> <key> [--from-stdin | --value <v>]
hearth secret delete <connection> <key>
```

`set` reads the cleartext, encrypts to *this daemon's* pubkey,
POSTs to `secrets_put`. Implicit pinning per Fork-1 option (c) —
the host running the CLI is the host the secret pins to.
`--from-stdin` is the default; `--value` is dev-friendly but
appears in shell history.

## 5. Server-side changes (hearth-cmd)

### Schema migration

One column add (`hosts.public_key`), one new table (`secrets`).
Migration file in the existing migrate.go pattern.

### WS handlers

New `secrets_ws.go` (mirrors `resource_plugins_ws.go` shape):
- `handleSecretsPut`
- `handleSecretsGet`
- `handleSecretsList`
- `handleSecretsDelete`
- `handleHostPubkeyGet`
- `handleEnrollHostPubkey`

Each validates write/read authz (caller is principal owning the
scoped resource), and validates the manifest constraint for
`scope_kind=connection`.

The manifest constraint is awkward: the server doesn't have plugin
manifests today. Either:
- (a) Daemon includes the manifest's credential names list when
  calling `secrets_put` (server validates against the inline list).
- (b) Server caches a manifest digest at the `seed_resource_connection_rules`
  call from 1e, validates against the cached version.
- (c) Skip the constraint server-side; trust the daemon to filter.

Lean (a) for 1d — minimum surface, no new server-side state. The
daemon already knows the manifest at the call site.

## 6. Step-by-step commit plan

| # | Title | Files | Repo | Scope |
|---|-------|-------|------|-------|
| 1 | `resource plugins: scoping plan for sub-phase 1d` | new doc | cli | S |
| 2 | `secrets: server schema (hosts.public_key + secrets table) + migration` | schema.sql, migrate.go | server | S |
| 3 | `secrets: server WS handlers (put/get/list/delete + pubkey)` | new secrets_ws.go + dispatch wire-up + tests | server | M |
| 4 | `secrets: daemon X25519 keypair + ~/.hearth/key + enroll on connect` | new secrets_crypto.go + secrets_keystore.go + boot wiring + tests | cli | M |
| 5 | `secrets: daemon yaml-bootstrap encrypts cleartext on first boot` | edits resource_authorize.go (or new) + tests | cli | M |
| 6 | `secrets: daemon fetches + decrypts at plugin Init` | edits plugin_supervisor.go / plugin_process.go + tests | cli | M |
| 7 | `secrets: stdout/stderr scrubber (8 encoding forms, lifted)` | new secrets_scrubber.go + plugin_process integration + tests | cli | M |
| 8 | `secrets: hearth secret CLI subcommand (list/set/delete)` | new cmd_secret.go + edits main.go + tests | cli | S |

Server commits (2, 3) ship first so the daemon has endpoints to
talk to. Daemon commits 4–8 land on the CLI repo.

## 7. Tests

- Server: `secrets_ws_test.go` covers each handler's authz, the
  manifest-name constraint (with daemon-supplied list), idempotent
  put, list scoping, delete by host.
- Daemon crypto: round-trip encrypt/decrypt; key load + persist.
- Daemon bootstrap: yaml-with-credentials → encrypted → server
  has ciphertext; second boot is a no-op (skipped via "already
  present" check).
- Daemon Init fetch: stub WS returns ciphertext, daemon decrypts,
  Init params carry expected cleartext (via existing
  `fakeAuthzWS`-style stub).
- Scrubber: each encoding form replaces with `***`; carry-buffer
  handles split-across-reads case.
- CLI: `hearth secret set` end-to-end against the test daemon.

## 8. Risks / open questions

1. **Pubkey substitution attack** by a compromised server is
   undetectable client-side without out-of-band verification.
   Same trust we already extend to the server. Hardening lands
   pre-launch via QR-code pubkey verification.
2. **YAML-bootstrap persists as a plaintext attack surface.**
   Memory written: `project_secrets_yaml_bootstrap_temporary.md`.
   Remove when phase-2 mobile UX ships secret-set.
3. **Scrubber misses non-credential secrets.** If a plugin
   internally derives a session token from a long-lived credential
   and prints the token, the scrubber doesn't catch it (no value
   match). Plugin authors are responsible for not printing
   secrets; scrubber is a backstop, not a guarantee.
4. **Init credential map sits in plugin process memory** for the
   process's lifetime. Plugin authors who memory-dump or ptrace
   their own process can read them. Acceptable; it's the plugin's
   own credentials.
5. **Host-pinning semantics on CLI invoke vs Init.** With Fork-1
   (c), if an operator on a non-pinned host runs `hearth resource
   invoke <pinned-elsewhere-connection> verb`, the daemon's
   plugin Init fails to fetch the secret and `secret not found
   for this host`. Surface this as a clear "connection is pinned
   to host X" error rather than the generic ErrUnavailable.
6. **Manifest credential-names constraint** is enforced via
   daemon-supplied list (option (a) in §5). Trust the daemon to
   filter correctly. A malicious daemon could store anything; the
   trust boundary already includes the daemon.
7. **Key rotation refuses-with-secrets dance** ports cleanly but
   the UX to drive a rotation isn't shipped in 1d. If a daemon's key file is lost, the
   recovery path is "wipe server's secrets for this host, re-run
   yaml bootstrap." Document.

## Cross-references

- `hearth-cmd/docs/external-resource-adapters.md` §"Credential
  broker" — the architecture committed to.
- `cli/secrets_crypto.go` — crypto primitives (shipped).
- `cli/secrets_scrubber.go` — output scrubber (shipped).
- `~/projects/permit/sql/schema.sql` — secrets-table reference.
- `hearth-cmd-cli/dev_connections.go` — yaml-bootstrap source.
- `hearth-cmd-cli/plugin_process.go` — Init injection point +
  scrubber integration.
- `hearth-cmd-cli/plugin_manifest.go` — `PluginCredential` with
  `Secret bool`; constraint source.
- `docs/resource-plugins-1f-plan.md` — predecessor.
- `MEMORY.md` → `project_secrets_yaml_bootstrap_temporary.md` —
  remove yaml path when phase 2 ships.
