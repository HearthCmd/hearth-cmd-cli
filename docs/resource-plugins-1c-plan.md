# Resource plugins — sub-phase 1c implementation plan

**Status:** planning. Branch `resource-plugins` (head c2c609a, 1b
landed). Predecessor: `docs/resource-plugins-1b-plan.md`. Authoritative
architecture: `hearth-cmd/docs/external-resource-adapters.md`.

**Goal of 1c:** a `hearth resource invoke` CLI subcommand + the IPC
route that reaches `PluginSupervisor.Invoke` from outside the
daemon. After 1c, a human on the daemon host can invoke a verb on
a dev-mode Resource Connection from a terminal and see the
plugin's response. No IAM call yet; that's 1e.

**Out of scope for 1c:**
- IAM `authorize()` integration. Every CLI invoke succeeds locally;
  the operator is the principal and the operator already controls
  the daemon — the security gate isn't load-bearing here. Lands in
  1e.
- Agent-side invoke path (in-process Go caller from spawned agent
  shells). Lands in 1f alongside prompt-side verb surfacing.
- Credential broker (the agent-side "ask for a verb result, never
  see the secret" wrapper). Lands in 1d.
- `hearth resource list` / `hearth resource describe`. Useful but
  cosmetic; we ship `invoke` alone and add list/describe in 1c.5
  if it bites during dogfooding.
- Streaming responses. Plugin responses are single-shot in the
  1b wire format; streaming is forward-compat work for v2.

## 1. CLI surface

```
hearth resource invoke <connection-id> <verb> [args-json]
```

- `<connection-id>` — the `connection_id` from
  `HEARTH_DEV_CONNECTIONS` (or, post-phase-2, a server-fed
  Resource Connection record).
- `<verb>` — one of the plugin's declared verbs.
- `[args-json]` — optional JSON object passed verbatim as
  `InvokeParams.Args`. Omitted → `null`. Invalid JSON → CLI exits
  with usage error, doesn't reach the daemon.

**stdin / stdout.**
- Plugin `InvokeResult.Stdout` writes to the CLI's stdout (no
  trailing newline mutation — pass through as-is).
- Plugin-reported errors render to stderr in
  `"hearth resource: <code>: <message>"` form.
- Daemon transport errors render to stderr in
  `"hearth resource: transport: <message>"` form.

**Exit codes.**
| Condition                              | Exit |
|----------------------------------------|------|
| Plugin returned `exit_code` (any int)  | passthrough |
| Plugin-reported error (`PluginError`)  | 2    |
| Transport error (`ErrTransport`)       | 3    |
| Bad CLI args / unknown connection / unknown verb (caller side) | 1 |

Rationale for splitting plugin-error from transport: a future
`hearth resource` shell wrapper or agent can distinguish "the
plugin said no" from "the plugin's process is gone" without
parsing stderr. Matches the *PluginError vocabulary already on
the wire.

**Help.** `hearth resource --help` prints the surface + a one-line
note pointing at the manifest for the list of connections + verbs.
No autodiscovery in 1c.

## 2. Existing patterns to mimic

- **Subcommand dispatch:** `main.go:46` switch on `os.Args[1]`. Add
  one case `"resource"` → `runResource(os.Args[2:])`.
- **Help / printUsage:** `main.go:89` — add one line under
  `Commands:`.
- **Socket dial + request/reply:** `organization.go:19` —
  `sendWSRequest(msgType, data)` is too WS-flavored to reuse
  verbatim; the request/reply *pattern* (DialTimeout, write JSON,
  ReadBytes('\n'), unmarshal `ipcResponse`) gets duplicated in a
  small `dialDaemon` + `sendIPC` helper colocated with
  `runResource`. Acceptable duplication; if 3+ subcommands grow
  this pattern we extract.
- **Daemon-side handler:** `daemon.go:873` switch — add one case
  `"resource_invoke"` → `d.handleResourceInvoke(conn, req)`.
  Pattern mirrors `handleUpdateShutdown` (validate, dispatch,
  single `sendControl(conn, ipcResponse{...})`).

## 3. Proposed types and surface

### IPC wire extension (`daemon.go:27`)

Add to `ipcRequest`:
```go
ResourceConnectionID string          `json:"resource_connection_id,omitempty"`
ResourceVerb         string          `json:"resource_verb,omitempty"`
ResourceArgs         json.RawMessage `json:"resource_args,omitempty"`
```

Add to `ipcResponse`:
```go
ResourceStdout   string `json:"resource_stdout,omitempty"`
ResourceExitCode int    `json:"resource_exit_code,omitempty"`
ResourceErrCode  string `json:"resource_err_code,omitempty"` // ErrorCode string, "" on success
```

`Type` on success: `"resource_invoke_response"`. On error:
`"error"` (existing convention; `Message` carries the
human-readable string, `ResourceErrCode` carries the structured
code).

Wire field names use `resource_` prefix to keep them grouped on
the wire and not collide with existing `agent_*`, `ws_*` fields.

### CLI helper (`cmd_resource.go`, new file)

```go
func runResource(args []string)
// dispatches subverbs; for 1c, only "invoke"

func runResourceInvoke(args []string)
// parses connID/verb/args, dials daemon, sends ipcRequest{
//   Type: "resource_invoke",
//   ResourceConnectionID: connID,
//   ResourceVerb: verb,
//   ResourceArgs: argsJSON,
// }, reads response, maps to stdout/exit.
```

### Daemon handler (`daemon.go`, new method)

```go
func (d *Daemon) handleResourceInvoke(conn net.Conn, req ipcRequest)
```

Flow:
1. `d.pluginSupervisor == nil` → 500-ish error. Belt-and-suspenders
   guard; supervisor is wired unconditionally in 1b's boot path,
   but defensive code is cheap.
2. Validate `req.ResourceConnectionID` and `req.ResourceVerb`
   non-empty. Empty → `ipcResponse{Type:"error", Message:"..."}`.
3. Build `ctx` from `context.Background()` with a per-call timeout
   (see §4 below).
4. Call `d.pluginSupervisor.Invoke(ctx, req.ResourceConnectionID,
   req.ResourceVerb, req.ResourceArgs)`.
5. On `*PluginError`: marshal to `ipcResponse{Type:"error",
   Message: pe.Message, ResourceErrCode: string(pe.Code)}`.
6. On success: `ipcResponse{Type:"resource_invoke_response",
   ResourceStdout: r.Stdout, ResourceExitCode: r.ExitCode}`.

## 4. Timeout policy

`PluginSupervisor.Invoke` accepts a ctx; whoever calls it picks
the deadline. The CLI doesn't pass a deadline down the wire (the
IPC frame has no `timeout_seconds` field today), so the daemon
applies a default of **30s** per invoke. Configurable via
`HEARTH_RESOURCE_INVOKE_TIMEOUT` (duration string parsed with
`time.ParseDuration`); unset → 30s; malformed → log + 30s.

30s is the same default the daemon uses for `DaemonWS.SendWSRequest`
(`daemon_ws.go:524`), so it's a known-tolerable upper bound for
"a single round-trip a human is waiting on." Tightening later is
trivial (per-verb overrides in the manifest are a future hook).

The CLI itself imposes no client-side timeout — it waits as long
as the daemon does. Ctrl-C on the CLI closes the unix-socket
connection; the daemon's `handleConn` already drops on `read EOF`
but the in-flight `pluginSupervisor.Invoke` keeps running because
ctx is rooted at `Background()`, not derived from the IPC
connection. **Open question:** should ctx-cancel-on-IPC-disconnect
be wired in 1c? Sketch: derive ctx from a `context.WithCancel`,
spawn a goroutine that watches the conn for EOF and cancels. Lean
no for 1c — adds complexity for an edge case; document in §9 and
revisit when streaming lands.

## 5. Error mapping

CLI-side decoding (mirrors `PluginError`'s `Code` vocabulary from
`plugin_rpc.go`):

| `ResourceErrCode` | CLI exit | stderr prefix          |
|-------------------|----------|------------------------|
| `bad_args`        | 2        | `bad_args:`            |
| `unauthorized`    | 2        | `unauthorized:`        |
| `unavailable`     | 2        | `unavailable:`         |
| `forbidden`       | 2        | `forbidden:`           |
| `internal`        | 2        | `internal:`            |
| `transport`       | 3        | `transport:`           |
| (empty / unknown) | 2        | `error:`               |

Exit 1 is reserved for CLI-side argument errors (unknown
subcommand, malformed args-json, etc.), so the scripts wrapping
`hearth resource invoke` can distinguish "user typo" from "plugin
said no" from "plugin process dead."

## 6. Daemon tests

`daemon_resource_test.go` (new file) — integration test that:
1. Spins up a daemon with a stub registry containing the echo
   plugin (reuse `echoManifest` from `plugin_process_test.go`).
2. Loads a DevConnectionStore with a single `echo-test` connection.
3. Listens on a tempdir-socket (the existing test pattern in
   `daemon.go`'s socket setup).
4. Sends `ipcRequest{Type:"resource_invoke", ...}` over the socket.
5. Asserts response shape on success + on each error vocabulary
   item (use the echo plugin's `fail` verb for plugin error, `exit`
   verb for transport).

We don't need to test the CLI binary itself in 1c — that surface
is thin and the IPC handler covers the contract. (If it grows past
~50 LOC of CLI parsing, add a tiny `cmd_resource_test.go` for the
JSON-parse + arg-shape branches.)

## 7. Server-side: nothing

1c does not touch hearth-cmd. The IAM call lands in 1e. The
server has no notion of resource invokes yet.

Practically: a daemon running 1c-only against a server still on
main works fine; no schema or wire changes upstream.

## 8. Step-by-step commit plan

| # | Title | Files | Scope | After |
|---|---|---|---|---|
| 1 | `plugins: 1c scoping plan` | new `docs/resource-plugins-1c-plan.md` | S | this doc |
| 2 | `plugins: hearth resource invoke CLI surface` | new `cmd_resource.go`, edits `main.go` (switch + printUsage) | S | `hearth resource --help` prints; dial fails cleanly with no daemon |
| 3 | `plugins: resource_invoke IPC route` | edits `daemon.go` (ipcRequest/Response fields, switch case, handleResourceInvoke); new `daemon_resource_test.go` | M | end-to-end echo invoke green |
| 4 | `plugins: HEARTH_RESOURCE_INVOKE_TIMEOUT + error vocabulary mapping` | edits `cmd_resource.go` (exit-code map), `daemon.go` (timeout parse) | S | manual smoke of `fail` / `exit` verbs produces correct exit codes |

Three real commits; the doc is a separate prep commit.

## 9. Risks / open questions

1. **CLI-disconnect doesn't cancel in-flight invoke.** See §4.
   Acceptable for 1c; document. Pre-existing in `handleWSRequest`
   too, so we're not regressing.
2. **No `hearth resource list` / `describe`.** Operators inspect
   `HEARTH_DEV_CONNECTIONS` directly. If it bites during the HA
   plugin v1 push, add as 1c.5.
3. **Args-json on the command line is brittle.** Shells eat quotes.
   For nested objects users will reach for stdin; we'll add
   `--args-stdin` if it bites. Lean YAGNI for 1c.
4. **Default-rule seeding is not in 1c.** When 1e lands, every
   plugin invoke will go through Authorize, which today returns
   Ask for every external_resource.* action because no rules
   exist. 1e plan needs to ship default-rule seeding (read from
   `manifest.default_rules`, INSERT on plugin install) in lockstep
   with the authorize call, or the daemon has to honor the
   Ask → push → wait flow. Flagged here to prevent 1e from
   shipping half-done.
5. **No spawn_context plumbing.** The CLI principal is the daemon
   operator (humanUserID from daemon config). When the agent-side
   path lands in 1f, the principal will be the agent's
   ai_agent_instance / job_description / position, and the IAM
   call's principal kind shifts. The IPC wire will need a
   `principal_*` field tuple or we'll add a separate
   `agent_resource_invoke` IPC type. Defer to 1f.
6. **No retry / idempotency hint.** A user who Ctrl-Cs mid-invoke
   has no way to know whether the plugin side-effected. The wire
   has no idempotency-key field. Forward-compat work; flagged.

## Cross-references

- `hearth-cmd-cli/plugin_supervisor.go` — `PluginSupervisor.Invoke`
  signature and error contract.
- `hearth-cmd-cli/plugin_rpc.go` — `PluginError` vocabulary +
  wire constants.
- `hearth-cmd-cli/daemon.go:27` — `ipcRequest` (extended in
  commit 3).
- `hearth-cmd-cli/daemon.go:873` — IPC dispatch switch.
- `hearth-cmd-cli/main.go:46` — CLI subcommand switch.
- `hearth-cmd-cli/organization.go:19` — `sendWSRequest` pattern
  reference.
- `hearth-cmd/docs/external-resource-adapters.md` — Daemon ↔
  plugin RPC, error vocabulary.
- `docs/resource-plugins-1b-plan.md` — predecessor.
