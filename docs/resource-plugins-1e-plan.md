# Resource plugins — sub-phase 1e implementation plan

**Status:** planning. Branch `resource-plugins` (head fd450d1, 1c
landed; 1d deferred). Predecessor: `docs/resource-plugins-1c-plan.md`.
Authoritative architecture:
`/Users/mattbeller/projects/hearth-cmd/docs/external-resource-adapters.md`.

**Goal of 1e:** the daemon authorizes every plugin invoke through
the server's IAM `Authorize()` chokepoint before dispatching, and
the server seeds default IAM rules so the happy path actually
succeeds. After 1e, dogfooding `hearth resource invoke` runs the
full decision pipeline: Allow → invoke plugin, Deny → return error,
Ask → push to phone, await human response, then invoke or
deny accordingly.

**The arch-doc fork — explicit.** The authoritative architecture
(adapters.md §"IAM evaluation split") commits to **daemon-local
evaluation** of `external_resource.*` actions, with the daemon
caching a relevant slice of the rules table. 1e does *not* build
that. 1e ships a stopgap where every plugin invoke makes one
daemon→server WS round-trip to evaluate. Acceptable because:

- The eventual daemon-local engine needs a rules-sync mechanism
  + a parallel daemon-side evaluator. That's another full
  sub-phase of work (call it 1g).
- The latency cost of one WS round-trip per invoke is fine for
  dogfooding through phase 1.
- The wire format we choose here will be re-used by the audit_log
  push from the future daemon-local path, so the work isn't wasted.

This is flagged in §9 so a future reader can find the re-do
ticket.

**Out of scope for 1e:**
- Daemon-local rules cache + evaluator (→ 1g).
- Audit log push from daemon to server (currently the server
  writes audit_log rows since it's the evaluator).
- Server-side `resource_connections` table / mobile-UX CRUD
  (phase 2). 1e treats every connection_id we see as opaque text;
  no FK.
- Entity context plumbing. The Authorize call carries an empty
  EntityContext for now. Entity discovery + label/parent
  predicates land alongside the HA plugin's snapshot work
  (phase 3+).
- Plugin install records on the server. We don't need them yet —
  rules table's `resource_id` is free-text, and seed rules are
  keyed on `(principal, action, connection_id)` directly.

## 1. Architecture summary (stopgap)

```
hearth CLI
   │  IPC: resource_invoke
   ▼
daemon: handleResourceInvoke
   │
   ├─► (1) WS: authorize_resource_invoke {principal, action, connection_id, ...}
   │              │
   │              ▼ server: handleAuthorizeResourceInvoke
   │                  s.Authorize(...)
   │                  ├─ Allow / Deny  → return Decision
   │                  └─ Ask      → INSERT permission_request,
   │                                     push to phone, block on
   │                                     s.pending[req_id].Response,
   │                                     return final Decision
   │              ▼
   │  ◄────────  Decision
   │
   ├─► (2) if Allow → PluginSupervisor.Invoke
   │                 returns to CLI
   └─► (3) if Deny  → ipcResponse{Type:"error", ResourceErrCode:"forbidden"}

daemon-boot one-shot, separate from invoke path:
   WS: seed_resource_connection_rules {connection_id, plugin_type,
        default_rules: [...]}    ← idempotent INSERT OR IGNORE
```

## 2. Wire shapes

### Daemon → server, msg_type `seed_resource_connection_rules`

Sent once per dev connection at daemon boot, after the supervisor
is wired. Idempotent on the server side (INSERT OR IGNORE on the
existing rules UNIQUE constraint), so booting the daemon repeatedly
doesn't multiply rules.

```json
{
  "connection_id": "echo-test",
  "plugin_type":   "echo",
  "default_rules": [
    {"action": "external_resource.echo.echo",  "decision": "allow"},
    {"action": "external_resource.echo.fail",  "decision": "ask"}
  ]
}
```

Server response: `{"seeded": <int>, "skipped": <int>}` — counts
for log visibility.

**Principal scoping.** Seeded rules are written at
`principal_scope_kind='principal'` with `principal_scope_value =
<daemon's humanUserID>` (the daemon already knows this from
`d.humanUserID`). The 'principal' scope matches the principal id
in `Authorize` regardless of resource kind (see
`cmd/hearth-cloud/authorize_engine.go:182` scopeClause). So the
operator who installed the dev connection gets seeded rules; other
users don't.

This is *not* what phase 2 will do (phase 2 will scope to
household via mobile-UX-driven creation). For 1e dogfooding,
operator-scoped rules are sufficient and avoid resolving
positions / orgs from the daemon side.

**Resource ID.** Each seeded rule's `resource_kind = 'external_resource'`
and `resource_id = connection_id`, so rules are connection-specific
even though the manifest's action strings reference plugin_type.

### Daemon → server, msg_type `authorize_resource_invoke`

Sent once per `hearth resource invoke`, before the daemon
dispatches to the plugin subprocess.

```json
{
  "principal_id":   "<daemon's humanUserID>",
  "principal_kind": "human",
  "connection_id":  "echo-test",
  "plugin_type":    "echo",
  "verb":           "echo",
  "request_id":     "<uuid>"
}
```

Server response: shape mirrors the existing permission-resolution
path so the daemon doesn't need a new decoder:

```json
{
  "decision":    "allow",   // "allow" | "deny"
  "rule_id":     42,        // 0 if Ask-resolved or no rule
  "reason":      "rule matched"
}
```

The server *always* returns Allow or Deny — Ask is invisible
to the daemon. If the server's Authorize returns Ask, the
server-side handler blocks until the phone-side resolution arrives
(or the request times out, in which case the server returns Deny
with reason `permission_request_timed_out`).

**No EntityContext.** 1e carries no entity. The
`externalResourcePredicateEvaluator` matches rules whose
predicate is empty/NULL — exactly the default_rules we seed.
Predicate-bearing rules will never match in 1e; that's expected
until entity discovery lands.

## 3. Daemon-side changes

### `handleResourceInvoke` extension (`daemon.go`)

Insert before `pluginSupervisor.Invoke`:

1. Resolve `conn := d.devConnections.Get(req.ResourceConnectionID)`.
   On miss → existing ErrBadArgs path. We don't even reach
   Authorize for unknown connections.
2. Resolve manifest via `d.plugins.GetPluginByInstallID(conn.PluginInstallID)`.
   On miss → existing ErrUnavailable. Same reasoning.
3. Build a Go-side `authorizeResourceInvokeReq` (new struct in a
   new `resource_authorize.go`), marshal, hand to
   `d.daemonWS.SendWSRequest(corrID, "authorize_resource_invoke", payload)`.
4. Decode `authorizeResourceInvokeResp`. On `decision == "deny"`:
   return ipcResponse with ResourceErrCode = `forbidden` (per the
   1c §5 vocabulary) and Message = response Reason.
5. On `decision == "allow"`: fall through to existing
   `pluginSupervisor.Invoke` path. (Note that the lookups above
   are now happening twice — once in the daemon-side preflight and
   once inside the supervisor. Acceptable duplication for 1e;
   refactor candidate when 1g restructures the eval path.)

WS connection guard: if `d.daemonWS == nil` or
`!d.daemonWS.IsConnected()`, return ipcResponse with
ResourceErrCode = `unavailable` and Message = "daemon offline
from server (authorize unavailable)". This is a behavior change
from 1c, where invokes worked offline. Document in §9 and
backstop with `HEARTH_RESOURCE_AUTHZ_BYPASS=1` for local dev
without a server.

### Boot-time seed (`daemon.go`, `runDaemonForeground`)

After `d.pluginSupervisor = NewPluginSupervisor(...)`, kick off a
**non-blocking** seed pass:

```go
go d.seedResourceConnectionRules()
```

Implementation iterates `d.devConnections.List()`, looks up each
connection's manifest, sends one `seed_resource_connection_rules`
WS request per connection. Logs results. Errors are non-fatal —
seeding is best-effort, and a subsequent `authorize_resource_invoke`
without the rule will simply return Ask/Deny.

Race ordering: the seed runs as a goroutine because daemonWS may
not be connected yet at boot. The seed func internally waits up
to 60s for the WS to come up; if it doesn't, log + give up. The
goroutine doesn't block daemon shutdown.

### `HEARTH_RESOURCE_AUTHZ_BYPASS`

When `1` / `true`, `handleResourceInvoke` skips both the seed and
the authorize call. Behaves like 1c. Belt-and-suspenders for
local development without a server connection. Logged at boot if
set, mirroring `HEARTH_DEV_CONNECTIONS`.

## 4. Server-side changes

### `seed_resource_connection_rules` handler

New file `cmd/hearth-cloud/handle_seed_resource_rules.go` (or
inlined in `main.go` near the WS dispatch — let's match local
convention, which puts most handlers in main.go). The handler:

1. Decodes the wire payload (connection_id, plugin_type,
   default_rules array).
2. Validates: connection_id non-empty, plugin_type non-empty,
   each rule has action + decision, decision ∈
   {allow, deny, ask}, action starts with
   `external_resource.<plugin_type>.`.
3. Looks up the daemon's humanUserID from the WS auth context
   (existing pattern — every authenticated daemon WS frame
   already knows its user).
4. In a transaction, calls `writeRule(tx, "principal",
   humanUserID, "external_resource", &connectionID, rule.Action,
   "", rule.Decision, nil, "")` for each default rule. Counts
   inserted vs ignored.
5. Returns `{"seeded": <int>, "skipped": <int>}`.

No new tables, no schema changes.

### `authorize_resource_invoke` handler

New handler same file. The handler:

1. Decodes payload (principal_id, principal_kind, connection_id,
   plugin_type, verb, request_id).
2. Validates fields non-empty.
3. Constructs:
   ```go
   p  := Principal{ID: principalID, Kind: PrincipalKind(principalKind)}
   ac := ActionContext{
       Resource: Resource{Kind: "external_resource", ID: connectionID},
       Action:   "external_resource." + pluginType + "." + verb,
   }
   in := AuthInput{"request_id": requestID}
   ```
4. Calls `decision := s.Authorize(p, ac, in)`.
5. If `decision.Kind == DecisionAsk`:
   - Insert permission_request row mirroring the existing tool-call
     pattern (one helper, `s.insertPermissionRequestForResourceInvoke`).
   - Push to user devices via existing fan-out (helper:
     `s.fanoutPermissionRequest` — already shared from the
     host-approval work).
   - Register `s.pending[requestID]` and block on
     `pending.Response` channel with a timeout (use the same value
     as the tool-call flow, 5 minutes — see `s.pending` callers).
   - On resolution: read the human's answer, optionally
     `writeRule` if they chose "always allow" (use the existing
     mechanism — handlePermissionResponse already creates rules
     when the response carries the "remember" flag).
   - Translate the resolved decision back into the
     {allow, deny} wire response. Timeout → deny with reason
     `permission_request_timed_out`.
6. Returns `{decision, rule_id, reason}` over the WS reply.

**Ask re-uses the iOS push pipeline unchanged.** The
`permission_request` rows on the server carry an action string;
the iOS UI already handles "what does this action mean" via the
manifest registry it caches. For `external_resource.<plugin>.<verb>`,
iOS will need a render hook — but that's iOS work, not server
work. For 1e dogfooding via CLI, Ask without iOS support
times out with Deny, which is fine.

### WS msg_type registration

`main.go` daemon dispatch switch (around line 4772). Add two
cases that invoke the new handlers. Existing pattern uses
`go handler(...)` for Ask-blocking handlers so the WS reader
goroutine isn't tied up; match that.

## 5. Tests

### Daemon-side (`hearth-cmd-cli`)

`daemon_resource_test.go` extends with a `fakeServer` that satisfies
the WS contract. Two tests:

- `TestHandleResourceInvoke_AuthzAllow` — fake server returns
  `{"decision":"allow"}` → invoke proceeds, response shape matches
  the existing happy-path test.
- `TestHandleResourceInvoke_AuthzDeny` — fake server returns
  `{"decision":"deny","reason":"no matching rule"}` → invoke
  bypassed, response has ResourceErrCode = `forbidden`,
  ResourceStdout empty.
- `TestHandleResourceInvoke_BypassEnv` — `HEARTH_RESOURCE_AUTHZ_BYPASS=1`
  → skips the WS call, invokes plugin directly.

We *don't* need to test the seed path in the daemon — it's a
one-shot at boot.

### Server-side (`hearth-cmd/cmd/hearth-cloud`)

`authorize_resource_invoke_test.go`:

- `TestAuthorizeResourceInvoke_AllowedByRule` — pre-insert an
  allow rule, fire the WS handler directly, expect Allow back.
- `TestAuthorizeResourceInvoke_AskTimeout` — no rule, no
  resolver wired → blocks on pending, hits the test-configurable
  timeout (override the 5-min default via a package-level var,
  same pattern 1b used for backoffSchedule), returns Deny with
  reason `permission_request_timed_out`.
- `TestAuthorizeResourceInvoke_AskResolved` — kick off the
  handler in a goroutine, simulate the phone-side
  permission_response from another goroutine, expect Allow.

`seed_resource_rules_test.go`:

- `TestSeedResourceRules_InsertsAndIgnores` — send seed payload
  twice, assert second pass returns `seeded:0 skipped:N`.
- `TestSeedResourceRules_RejectsBadAction` — action that doesn't
  match `external_resource.<plugin_type>.*` returns an error.

## 6. Step-by-step commit plan

| # | Title | Files | Repo | Scope |
|---|-------|-------|------|-------|
| 1 | `resource plugins: scoping plan for sub-phase 1e` | new `docs/resource-plugins-1e-plan.md` | cli | S |
| 2 | `plugins: server seed_resource_connection_rules WS handler` | new handler in main.go + helper file + tests | server | M |
| 3 | `plugins: server authorize_resource_invoke WS handler + Ask` | same files + tests | server | M |
| 4 | `plugins: daemon boots seed call for dev connections` | edits daemon.go + a small seed helper | cli | S |
| 5 | `plugins: daemon authorize_resource_invoke preflight` | edits daemon.go (handleResourceInvoke), new resource_authorize.go, tests | cli | M |
| 6 | `plugins: HEARTH_RESOURCE_AUTHZ_BYPASS env-gate` | edits daemon.go + cmd_resource.go (boot log) + tests | cli | S |

Server commits land first so the daemon has something to talk to;
the daemon commits push to its branch (`resource-plugins` on the
CLI) after the server's `resource-plugins` branch has the handlers.
Both repos are already on `resource-plugins`.

## 7. Risks / open questions

1. **Ask without iOS verb-render support.** Until iOS knows
   how to render an `external_resource.<plugin>.<verb>` permission
   request prompt, the user sees a generic "approve this action"
   bubble. Acceptable for 1e dogfooding; iOS-side work tracked
   separately.
2. **Seed-vs-WS-up race.** §3 covers this; the seed goroutine
   waits up to 60s for `daemonWS.IsConnected()`. Edge case: if
   the seed sends *before* the server has set up the daemon's
   user context, the seed handler returns "unauthenticated." 60s
   is generous; in practice the daemon WS auth handshake
   completes within seconds.
3. **`d.humanUserID` empty.** Pre-login daemons have an empty
   humanUserID. `authorize_resource_invoke` against an empty
   principal returns Deny (no rules match an empty principal_id).
   Document; recommend `hearth login` before `hearth resource
   invoke`. The CLI surface could refuse early, but that's
   polish, not 1e.
4. **The arch-doc re-do.** §preamble flags that 1g will move
   evaluation daemon-side. The wire format from §2 is forward-
   compat — the `authorize_resource_invoke` request shape carries
   everything the daemon-local evaluator needs as input, and the
   response shape matches what a local Authorize would produce.
   The audit_log push from 1g will be a separate WS msg_type, not
   piggybacked on this one.
5. **No 1d before 1e.** Credentials still flow plaintext from
   `HEARTH_DEV_CONNECTIONS` → daemon → plugin process via stdin
   Init params. 1e doesn't change that surface; 1d (credential
   broker) is independent and can land before or after 1e without
   conflict.
6. **`HEARTH_RESOURCE_AUTHZ_BYPASS` shipping in prod binaries.**
   Same shape as `HEARTH_DEV_CONNECTIONS`. Dev affordance;
   revisit before public release.
7. **`permission_request_timed_out` Deny is opaque to the user.**
   The CLI sees `forbidden: permission_request_timed_out`. Not
   pretty but unambiguous. UX polish later.

## Cross-references

- `hearth-cmd/docs/external-resource-adapters.md` §"IAM
  evaluation split" — the architecture we're stopgapping.
- `hearth-cmd/cmd/hearth-cloud/authorize.go` — the Authorize
  chokepoint.
- `hearth-cmd/cmd/hearth-cloud/authorize_engine.go:182` —
  scopeClause; the 'principal' scope behavior we lean on.
- `hearth-cmd/cmd/hearth-cloud/rules_write.go` — writeRule helper
  the seed handler reuses.
- `hearth-cmd/cmd/hearth-cloud/main.go:4772` — daemon WS dispatch
  switch (new cases register here).
- `hearth-cmd-cli/daemon_ws.go:524` — SendWSRequest, the
  daemon→server primitive we're piggybacking.
- `hearth-cmd-cli/daemon.go` — handleResourceInvoke (1c added it,
  1e extends it).
- `docs/resource-plugins-1c-plan.md` — predecessor.
