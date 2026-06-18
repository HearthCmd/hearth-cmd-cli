# Resource plugins — sub-phase 1b implementation plan

**Status:** planning. Branch `resource-plugins` (head 94cfb16, 1a
landed). Authoritative architecture:
`hearth-cmd/docs/external-resource-adapters.md`. Predecessor:
`docs/resource-plugins-1a-plan.md`.

**Goal of 1b:** subprocess supervisor + line-delimited JSON-RPC
framing. After 1b, the daemon can launch a plugin binary (resolving
`executable` relative to `SourceDir`), call `Init`/`Invoke`/
`Shutdown`, and surface a small typed error vocabulary to a Go
caller. No `hearth resource` CLI, no IPC route, no IAM call yet —
those are 1c.

**Out of scope for 1b:**
- `Onboard` method (deferred until onboarding shape settled).
- Real server-fed Resource Connection records (phase 2).
- IAM `authorize()` integration (1c).
- `hearth resource` CLI subcommand + IPC route (1c).
- Concurrent in-flight requests per process. Serialize; `id` is
  forward-compat for v2.
- Idle teardown.

## 1. Existing structure survey

**Subprocess patterns.** Daemon's agent management
(`daemon_agent.go` + `relay.go`) uses a PTY relay — not a fit for
plugins which are headless. We reuse only the shape: spawn-on-
demand, goroutine per process for stdout demux, goroutine per
process for stderr forwarding, `cmd.Wait()` observable.
`classifyExit` in `daemon_agent.go:152` is the only direct
reusable helper (we'll write a small `wasExitNonzero` wrapper).

**Daemon struct (`daemon.go:117`).** 1a added `plugins
*PluginRegistry`. 1b adds two fields: `devConnections
*DevConnectionStore` and `pluginSupervisor *PluginSupervisor`. Each
owns its own mutex; no piggyback on `d.mu`.

**Shutdown ordering.** `Daemon.Shutdown()` at `daemon.go:546`. 1b
inserts `d.pluginSupervisor.ShutdownAll()` between the instance Stop
loop and `d.agentWg.Wait()`. Plugins don't push to the WS, so
position doesn't matter for that invariant; we put it before
agentWg.Wait() so future hooks can emit final audit events.

**Logging.** `log.Printf("daemon: ...")` is the convention. Plugin
stderr forwarding mirrors: `log.Printf("plugin %s: %s", connID,
line)`.

## 2. Proposed types and surface

Four new files:
- `plugin_rpc.go` — JSON-RPC wire types + error vocabulary.
- `plugin_process.go` — `PluginProcess` (one live subprocess).
- `plugin_supervisor.go` — `PluginSupervisor` (map + lazy launch +
  backoff).
- `dev_connections.go` — placeholder Resource Connection loader.

Plus tests + test plugins (`testdata/plugins/{echo,crashy}/`).

### `plugin_rpc.go` — wire types

```go
type rpcRequest struct {
    ID     string          `json:"id"`
    Method string          `json:"method"` // "Init" | "Invoke" | "Shutdown"
    Params json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
    ID     string          `json:"id"`
    Result json.RawMessage `json:"result,omitempty"`
    Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
    Code    string `json:"code"`
    Message string `json:"message"`
}

type InitParams struct {
    ConnectionID string            `json:"connection_id"`
    Credentials  map[string]string `json:"credentials"`
    Snapshot     json.RawMessage   `json:"snapshot,omitempty"`
}

type InvokeParams struct {
    Verb string          `json:"verb"`
    Args json.RawMessage `json:"args,omitempty"`
}

type InvokeResult struct {
    Stdout   string `json:"stdout"`
    ExitCode int    `json:"exit_code"`
}

type ErrorCode string
const (
    ErrBadArgs      ErrorCode = "bad_args"
    ErrUnauthorized ErrorCode = "unauthorized"
    ErrUnavailable  ErrorCode = "unavailable"
    ErrForbidden    ErrorCode = "forbidden"
    ErrInternal     ErrorCode = "internal"
    ErrTransport    ErrorCode = "transport" // daemon-side only
)

type PluginError struct {
    Code    ErrorCode
    Message string
}
func (e *PluginError) Error() string { return string(e.Code) + ": " + e.Message }
```

### `plugin_process.go` — one live subprocess

```go
type processState int
const (
    stateInitializing processState = iota
    stateReady
    stateDead
)

type PluginProcess struct {
    connID    string
    installID string
    manifest  PluginManifest
    creds     map[string]string

    mu     sync.Mutex // serializes Invoke + lifecycle
    cmd    *exec.Cmd
    stdin  io.WriteCloser
    stdout *bufio.Reader
    state  processState
    seq    uint64
}
```

Single mutex covers per-process serialization. An `Invoke` holds `mu`
across write-request + read-response. Counter, state transitions
also under `mu`.

Methods: `init`, `invoke`, `shutdown`, `isDead`.

Per-process goroutines:
- **stderr forwarder** — `bufio.Scanner` on stderr; each line →
  `log.Printf("plugin %s: %s", logPrefix, line)`. Exits on pipe
  close.
- **wait** — `cmd.Wait()`; on return takes `mu`, marks
  `stateDead`, closes stdin.

### `plugin_supervisor.go` — connections → processes

```go
type PluginSupervisor struct {
    registry *PluginRegistry
    devConns *DevConnectionStore

    mu         sync.Mutex
    procs      map[string]*PluginProcess
    backoff    map[string]*backoffState
    spawnLocks map[string]*sync.Mutex
}

func NewPluginSupervisor(reg *PluginRegistry, dev *DevConnectionStore) *PluginSupervisor
func (s *PluginSupervisor) Invoke(ctx context.Context, connID, verb string, args json.RawMessage) (InvokeResult, error)
func (s *PluginSupervisor) EnsureShutdown(connID string) error
func (s *PluginSupervisor) ShutdownAll() error
```

`Invoke` flow:
1. Look up dev connection → `pluginInstallID`, `credentials`.
2. Look up manifest from `d.plugins`.
3. Under `mu`: get/create `spawnLocks[connID]`, check `procs[connID]`.
4. If alive → release `mu`, call `proc.invoke`.
5. Else → release `mu`, take `spawnLocks[connID]`, double-check, honor
   backoff, spawn + init, store, invoke.

### `dev_connections.go` — placeholder

See §5.

## 3. Concurrency model

**Per-process serialization.** Each `PluginProcess` has `sync.Mutex`.
Invoke holds it across encode → write → read → decode. Trade-off:
no pipeline parallelism per connection. Acceptable for 1b.

**Per-connection spawn serialization.** Supervisor map allows
concurrent reads (different connIDs don't block each other) and
prevents concurrent spawns of the same connID via per-connID
spawn locks + double-checked locking.

```go
s.mu.Lock()
proc, ok := s.procs[connID]
spawnLock := s.spawnLocks[connID]
if spawnLock == nil { spawnLock = &sync.Mutex{}; s.spawnLocks[connID] = spawnLock }
s.mu.Unlock()

if ok && !proc.isDead() {
    return proc.invoke(ctx, ...)
}

spawnLock.Lock(); defer spawnLock.Unlock()
// double-check
s.mu.Lock()
proc = s.procs[connID]
s.mu.Unlock()
if proc != nil && !proc.isDead() { return proc.invoke(ctx, ...) }

// honor backoff, spawn, init, stash, invoke
```

Use `sync.Mutex` not RWMutex for `s.mu` (every Invoke may create a
spawnLock entry).

**Cancellation.** Caller's `context.Context` closes stdin/stdout
when cancelled so blocked `Scan` returns. Cancellation transitions
the process to `stateDead` (half-consumed request stream is
unrecoverable in 1b). Next Invoke respawns.

**Stderr forwarder.** Dedicated per-process goroutine; emits via
`log.Printf`. Tracked in per-process WaitGroup so `shutdown()` can
wait for the drain.

**Wait goroutine.** Per process, `cmd.Wait()`; on return takes
`proc.mu`, marks `stateDead`, closes stdin. Doesn't touch
supervisor map.

## 4. JSON-RPC wire format

Line-delimited JSON. Stderr is human-readable, not JSON.

**Request:**
```
{"id":"3","method":"Invoke","params":{"verb":"turn_on","args":{...}}}\n
```

**Response (success):**
```
{"id":"3","result":{"stdout":"{\"status\":\"ok\"}","exit_code":0}}\n
```

**Response (plugin error):**
```
{"id":"3","error":{"code":"unauthorized","message":"token rejected"}}\n
```

**`id`:** monotonic uint64 per process, base-10 string. Reset on
respawn. Not UUIDs — per-process-local, shorter, forward-compat.

**Response-id matching.** Serialized, so each Invoke expects exactly
one response with the matching id. Mismatch → protocol violation →
close process → `ErrTransport`.

**Plugin error vs transport error.** Both surface as `*PluginError`:
- Plugin `{"error":{"code":...}}` → `*PluginError{Code: ..., ...}`.
  Process stays alive.
- EOF / nonzero exit mid-call / context cancel / decode failure / id
  mismatch → `*PluginError{Code: ErrTransport, ...}`. Process →
  `stateDead`.

**Timeouts.** Caller-supplied via context. No method-specific defaults.

**Max line size.** `bufio.Scanner` default is 64KB; bump to 1MB
explicitly via `scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)`.
Document the cap in `plugin_process.go`.

## 5. Placeholder Resource Connection

`HEARTH_DEV_CONNECTIONS=/path/to/connections.yaml`. File:

```yaml
connections:
  - connection_id: ha-home
    plugin_install_id: ha
    credentials:
      ha_url: ws://homeassistant.local:8123/api/websocket
      ha_token: dev-token-here
  - connection_id: echo-test
    plugin_install_id: echo
    credentials: {}
```

```go
type DevConnection struct {
    ConnectionID    string            `yaml:"connection_id"`
    PluginInstallID string            `yaml:"plugin_install_id"`
    Credentials     map[string]string `yaml:"credentials"`
}

type DevConnectionStore struct {
    mu       sync.RWMutex
    byConnID map[string]DevConnection
}

func NewDevConnectionStore() *DevConnectionStore
func (s *DevConnectionStore) LoadFromEnv() error
func (s *DevConnectionStore) Get(connID string) (DevConnection, bool)
func (s *DevConnectionStore) List() []DevConnection
```

Boot wiring (after 1a's `d.plugins.Load` block):
```go
d.devConnections = NewDevConnectionStore()
if err := d.devConnections.LoadFromEnv(); err != nil {
    log.Printf("daemon: dev connections load error: %v", err)
}
if path := os.Getenv("HEARTH_DEV_CONNECTIONS"); path != "" {
    log.Printf("daemon: WARNING dev-mode connections loaded from %s (not for production)", path)
}
d.pluginSupervisor = NewPluginSupervisor(d.plugins, d.devConnections)
```

Production surface: one env-var check, one file read, one warning
log. Replaced by server-fed connection store in phase 2; supervisor
dependency surface unchanged.

## 6. Test plugin

`testdata/plugins/echo/{manifest.yaml, main.go}` — standalone
`package main`, no external deps. Verbs: `echo` (round-trip args),
`fail` (return structured error), `exit` (crash via os.Exit(1)),
`log_stderr` (marker to stderr + echo). `Init` returns ok;
`Shutdown` flushes response then exits 0.

Second plugin `testdata/plugins/crashy/` whose Init responds with
`os.Exit(1)` — used by backoff-cap test.

**Build dance.** `TestMain(m *testing.M)` in
`plugin_supervisor_test.go`:

```go
func TestMain(m *testing.M) {
    for _, name := range []string{"echo", "crashy"} {
        src := filepath.Join("testdata", "plugins", name, "main.go")
        out := filepath.Join("testdata", "plugins", name, "hearth-plugin-"+name)
        cmd := exec.Command("go", "build", "-o", out, src)
        cmd.Stderr = os.Stderr
        if err := cmd.Run(); err != nil {
            fmt.Fprintf(os.Stderr, "build %s plugin: %v\n", name, err)
            os.Exit(1)
        }
        defer os.Remove(out)
    }
    os.Exit(m.Run())
}
```

Output paths inside `testdata/`; added to `.gitignore`.

## 7. Crash respawn + backoff

```go
var backoffSchedule = []time.Duration{0, 100*ms, 500*ms, 2*s, 10*s, 30*s}
const healthyUptimeReset = 30 * time.Second

type backoffState struct {
    attempts      int
    nextAllowedAt time.Time
    lastSpawnAt   time.Time
    everReady     bool // did the previous process reach Ready?
}
```

Schedule: 0 / 100ms / 500ms / 2s / 10s / 30s (cap).

Reset: if `time.Since(b.lastSpawnAt) >= healthyUptimeReset` AND
`b.everReady`, reset `attempts = 0`. `everReady` prevents endless
reset of a plugin that crashes during `Init`.

Lazy, not background. Next Invoke checks `isDead()`, observes
backoff, **waits bounded by `ctx`**; if deadline fires first,
returns `ErrTransport`.

## 8. Daemon integration

**Struct extension** (`daemon.go:117`):
```go
plugins          *PluginRegistry      // 1a
devConnections   *DevConnectionStore  // 1b
pluginSupervisor *PluginSupervisor    // 1b
```

**Boot wiring** in `runDaemonForeground` — extend 1a block at
`daemon.go:469-476` (see §5).

**Shutdown wiring** in `Daemon.Shutdown()` at `daemon.go:546`,
between instance Stop loop and `d.agentWg.Wait()`:

```go
if d.pluginSupervisor != nil {
    if err := d.pluginSupervisor.ShutdownAll(); err != nil {
        log.Printf("daemon: plugin shutdown: %v", err)
    }
}
```

Nil-safe: all consumers (none in 1b) must guard `if d.pluginSupervisor == nil`.

## 9. Tests

**`plugin_rpc_test.go`** (pure Go, no subprocess):
- Round-trip encode/decode Init/Invoke/Shutdown.
- Error response → `*PluginError`.
- Unknown error codes pass through opaque + log warn.

**`plugin_process_test.go`** (spawns echo):
- `TestProcess_InitInvokeShutdown` — full happy path.
- `TestProcess_PluginError` — `fail` verb returns
  `*PluginError{Code:"internal"}`.
- `TestProcess_TransportErrorOnExit` — `exit` verb →
  `ErrTransport`, `isDead()` true.
- `TestProcess_ContextCancel` — cancel mid-call → `ErrTransport`,
  process dies.

**`plugin_supervisor_test.go`** (via dev connections):
- `TestSupervisor_LaunchAndInvoke` — first Invoke spawns + echoes.
- `TestSupervisor_ProcessReused` — second Invoke uses same PID.
- `TestSupervisor_CrashRespawn` — `exit` kills; next Invoke spawns
  fresh PID.
- `TestSupervisor_BackoffCap` — uses `crashy`; 10 rapid Invokes;
  schedule honored.
- `TestSupervisor_Shutdown` — `EnsureShutdown(connID)` clean.
- `TestSupervisor_StderrForwarded` — log buffer contains marker
  prefixed with conn id.
- `TestSupervisor_UnknownConnection` → `ErrBadArgs`.

**`dev_connections_test.go`** — tempfile + env, missing file,
malformed YAML.

Log capture via `bytes.Buffer` + `log.SetOutput` (mirrors 1a tests
and `harness_test.go`).

Tests are `//go:build darwin || linux` (the codebase rule).

## 10. Step-by-step commit plan

| # | Title | Files | Scope | After |
|---|---|---|---|---|
| 1 | `plugins: JSON-RPC wire types + error vocabulary` | new `plugin_rpc.go`, `plugin_rpc_test.go`, `.gitignore` updates | S | wire types compile; round-trip tests pass |
| 2 | `plugins: echo + crashy test plugins under testdata/` | new `testdata/plugins/{echo,crashy}/{manifest.yaml,main.go}` | S | standalone `go build` succeeds |
| 3 | `plugins: PluginProcess (subprocess + framing)` | new `plugin_process.go`, `plugin_process_test.go` (TestMain build step) | M | direct-process tests pass |
| 4 | `plugins: DevConnectionStore (HEARTH_DEV_CONNECTIONS)` | new `dev_connections.go`, `dev_connections_test.go` | S | YAML loader tested in isolation |
| 5 | `plugins: PluginSupervisor with lazy launch` | new `plugin_supervisor.go`, `plugin_supervisor_test.go` | M | supervisor tests pass (launch, reuse, plugin error, crash respawn, stderr forward, unknown conn) |
| 6 | `plugins: backoff cap + healthy-uptime reset` | edits `plugin_supervisor.go`, `TestSupervisor_BackoffCap` (uses `crashy`) | S | backoff schedule honored |
| 7 | `plugins: wire supervisor into daemon boot + shutdown` | edits `daemon.go` (struct + runDaemonForeground + Shutdown) | S | daemon boots with supervisor, ShutdownAll wired |

Commits 1–6 don't touch `daemon.go`; commit 7 is the integration.

## 11. Risks / open questions

1. **`TestMain` `go build` cost.** 5–15s cold cache. Acceptable; defer
   mtime-based skip until it bites.
2. **1MB line cap.** Adequate for echo; HA registry could exceed.
   Theoretical for 1b; bump to 8MB if needed.
3. **Context cancellation kills process.** Strict but safe.
   Forward-compat `Abort` method when streaming verbs land.
4. **Per-process serialization is one-way forward-compat.** Plugin
   authors who write concurrent-capable plugins get quietly
   serialized in 1b. Document in plugin-author guide.
5. **Stderr ordering vs response.** Tests must tolerate any ordering;
   stderr is log, not data.
6. **`HEARTH_DEV_CONNECTIONS` ships in production binaries.** Dev
   affordance; revisit before public release. Phase 2 replaces it.
7. **3s shutdown timeout** before SIGKILL. Tunable later.
8. **Manifest `executable` security gate.** Currently any non-empty
   string is accepted. Reject absolute paths / `../` traversal in a
   later sub-phase. Flagged here.
9. **No `Init` handshake.** Supervisor sends `Init` immediately after
   `cmd.Start()`. If plugin crashes before reading, EOF on stdout →
   `ErrTransport` — correct outcome.
10. **No reload IPC yet.** Supervisor caches `SourceDir + Executable`
    at spawn; binary changes on disk aren't picked up until a future
    `hearth plugin reload`. Post-1c.

## Cross-references

- `hearth-cmd/docs/external-resource-adapters.md` §"Daemon ↔
  plugin RPC", §"Error vocabulary".
- `docs/resource-plugins-1a-plan.md` — predecessor.
- `daemon.go:117` — `Daemon{}` struct.
- `daemon.go:469-476` — 1a boot hook (1b extends).
- `daemon.go:546` — `Daemon.Shutdown` (1b inserts plugin
  shutdown).
- `daemon_agent.go:152` — `classifyExit` pattern.
