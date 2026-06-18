# Agent identity / anti-spoofing plan

**Status:** plan, no code yet. Tracked in
`project_agent_identity_protocol_todo.md`. This doc walks through what
we're solving, the alternatives we considered, the recommended shape,
and a phased rollout.

## What's wrong today

Every IPC path that takes an "I am acting as agent X" claim reads
the agent id from `HEARTH_AGENT_INSTANCE_ID` in the calling
process's environment. The daemon stamps that env var into agent
subprocesses at spawn; the CLI picks it up when it's invoked from
inside an agent (e.g. `hearth resource invoke`, `hearth resource refresh`,
`hearth run --secret`); the daemon forwards the value as
`principal_kind=agent, principal_id=<id>` to its own authz handler
and onward to the server.

The env value is forgeable by any process at the same UID. An agent
can `export HEARTH_AGENT_INSTANCE_ID=<sibling-id>` before running the
`hearth` CLI and the daemon will accept the claim. Surfaces:

- `hearth resource invoke` — plugin verbs IAM-authorized as the claimed agent.
- `hearth resource refresh` — same.
- `hearth run --secret` — secret use authorized as the claimed agent.
- `hearth secret set --kind agent` — set_by attribution.
- `hearth agent attach` — once it ships; same env-var trust model.

What's still safe:
- Cross-host: server's `authorize_resource_invoke` verifies the
  claimed `ai_agent_instance_id.host_id` matches the daemon's
  authenticated host. Lateral movement across hosts is blocked.
- Cross-user: OS-level UID separation prevents siblings of a
  different user from reading or rewriting the env.

What's exposed:
- **Same-host, same-user sibling impersonation.** An agent can act
  as any of its siblings on the same host. Today this is the entire
  unprotected surface.

## Threat model

Adversary: a buggy or malicious agent running at the operator's UID
on a host the operator already trusts to run their agents. We want
to prevent **lazy or accidental** spoofing — an agent that mis-sets
its env, a script that copy-pastes another agent's id, or a
compromised agent that's smart enough to call `hearth` but not smart
enough to attack the OS.

Out of scope:
- An adversary at the same UID with full ptrace capability. Such an
  adversary can `ptrace` the target agent, read its memory, inject
  syscalls, or fork from inside it. No userland mechanism stops
  that; only OS-level UID separation does.
- Kernel-level or root compromise.
- Cross-host attacks (already handled by the server).

The point of the design: raise the cost of casual spoofing from
"one line of shell" to "ptrace attach + understand the protocol."
The latter requires effort comparable to "rewrite the agent to do
whatever it wants in the first place" — different problem.

## Design alternatives considered

### A. Per-spawn secret token in env

Daemon mints a random token at agent spawn, sets
`HEARTH_AGENT_TOKEN=<random>` alongside the existing env vars, the
CLI reads + forwards it on every IPC, daemon verifies against an
in-memory `(token → agent_id)` map.

**Rejected:** the token has to land in the CLI somehow, and the
realistic rails are env vars and argv — both readable by sibling
processes via `/proc/PID/environ` (Linux) or `ps eww` (macOS). The
adversary doesn't need to forge the token; they just steal it from
the target agent's environ. Same UID grants /proc read access.

### B. Per-spawn token over inherited write-once pipe

Daemon opens a pipe, gives the agent the read end as an inherited
fd at spawn, writes the token to the pipe once. Agent reads the
token into process memory.

**Partially rejected:** the agent now holds a secret siblings can't
steal (process memory is /proc/PID/mem-readable but requires ptrace
on most modern Linux configs). But the agent has to give the token
to its CLI subprocesses, and the only universal rail for that is
env or argv — same exposure as A. To avoid env/argv leakage, the
agent would need to pass the read end of a fresh pipe to each CLI
subprocess via fd inheritance, which means agents can't use shell
indirection (`bash -c "hearth …"`). Practical for some harnesses,
breaks others; not a clean general solution.

### C. Per-agent unix socket, path randomness as secret

Daemon creates a per-agent socket at `~/.hearth/agent-sockets/<random>.sock`,
passes the path to the agent via env, agent's CLI subprocesses
connect via that path instead of the shared daemon socket.

**Rejected:** path is in the agent's env → readable by siblings via
/proc/environ. Same root cause as A. Adding file permissions
doesn't help — same UID owns both processes.

### D. Process-tree inference via SO_PEERCRED + tree walk *(recommended)*

The daemon's IPC socket can ask the kernel for the connecting
process's PID (Linux: `SO_PEERCRED`; macOS: `LOCAL_PEERPID`). This
is unforgeable from userland — only the kernel writes the answer.
The daemon then walks parent PIDs (Linux: `/proc/PID/stat`; macOS:
`sysctl KERN_PROC_PID`) until it either finds a PID in its agent
registry or hits a process it doesn't recognize.

The agent's identity is **derived**, not claimed. Env vars become
hints, not authority. A buggy agent that rewrites
`HEARTH_AGENT_INSTANCE_ID` to a sibling's id changes nothing — the
daemon still sees the real spawning agent in the process tree.

**Why this works against the chosen threat model:**
- Casual spoofing (env rewrite) defeated — no env is consulted.
- Process-tree manipulation requires ptrace or double-forking;
  ptrace is the underlying same-UID-attack vector we already
  concede, and double-fork strands the child as a PID 1 reparent
  which we detect.

**Limitations called out explicitly:**
- Double-fork orphan: an agent that double-forks loses its tree
  identity (parent becomes PID 1 / launchd / systemd). Per the
  fallback policy below, this REJECTS the call rather than
  silently promoting to human — agents that break their process
  tree can't act through hearth at all until they fix that.
- PID reuse: the kernel reuses PIDs over time. Mitigation: store
  process start time (`/proc/PID/stat` field 22 on Linux; macOS
  sysctl) alongside the PID at registration; require the caller's
  start time to match (`agent must have been spawned before its
  CLI subprocess`). Effectively eliminates the recycle window.
- Active ptrace adversary: still wins, by design (see threat model).

### Fallback policy (load-bearing — see below for the soft-promotion concern)

| Tree walk             | Claim from CLI       | Daemon's principal      |
|-----------------------|----------------------|--------------------------|
| Finds agent X         | Empty / matches X    | `agent X`                |
| Finds agent X         | `agent Y` (≠ X)      | `agent X`, log forgery   |
| Finds no agent        | Empty                | `human` (real operator)  |
| Finds no agent        | `agent ?` (any)      | **REJECT** the call      |

The fourth row is the load-bearing one. An agent's permissions are
typically NARROWER than the operator's — the operator is the one
who granted those rules in the first place. So "tree walk failed,
fall back to the operator's human principal" would be a *soft
promotion*: an agent that breaks its process tree (double-fork,
detach into a long-lived daemon) would inadvertently get the
operator's broader rule set instead of its own narrower set.

Refusing the call on this row keeps the system fail-closed:
- Real operator at a terminal (no agent in the tree, no agent
  claim from the CLI) → `human` principal, works fine.
- Agent invoking through normal means (CLI is a direct child of
  the agent process tree) → tree walk finds the agent, works fine.
- Agent whose tree got broken (orphan, reparent to PID 1) →
  refused. Operator sees a clear error; agent gets re-spawned or
  fixed.

The CLI side cooperates by always omitting `principal_kind/id`
once Phase 2 lands — but the daemon's check is the actual safety
gate. Older CLIs that still send the env-var-derived claim hit
the second or fourth row depending on their tree.

## Recommended design (D, expanded)

### Components

1. **Agent PID registry** (daemon-private, in-memory):

   ```go
   type agentIdentity struct {
       AgentID      string
       PID          int
       SpawnTime    time.Time // for PID-recycle defense
   }
   d.agentsByPID map[int]agentIdentity
   ```

   Populated by `spawnAgentInstance` at fork/exec time; pruned on
   process exit (already detected by the supervisor for harness
   processes; reuse that signal).

2. **Peer-credential lookup on IPC accept.**

   Linux:
   ```go
   raw, _ := conn.(*net.UnixConn).SyscallConn()
   var ucred *unix.Ucred
   raw.Control(func(fd uintptr) {
       ucred, _ = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
   })
   ```

   macOS:
   ```go
   pid, _ := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
   ```

   Bundled behind a small `peerPID(conn)` helper with both build tags.

3. **Process-tree walk.**

   `resolveCallerAgent(pid int) (agentID string, ok bool)`:
   - Start at `pid`. Read its start time (for PID-recycle).
   - If `pid` is in `d.agentsByPID` AND start times match → return that agent id.
   - Else read its ppid, recurse upward.
   - Stop conditions: ppid is 1, ppid is the daemon's own PID, ppid is 0, or recursion depth exceeds (say) 32.
   - Returns `("", false)` if no agent ancestor found.

4. **Per-connection derived principal.**

   `handleConn` resolves the agent once per connection (cached on
   the conn-local state struct) and stamps `derivedPrincipal` into
   every IPC request handler. Handlers consume that instead of
   `req.ResourcePrincipalKind` / `req.ResourcePrincipalID`.

5. **Forgery telemetry (transitional).**

   During the rollout, the daemon logs a WARN whenever the
   env-var-derived claim differs from the tree-walk result.
   Operators / we get visibility into which agents (if any) are
   making spoofed claims. After enough quiet, the env-var path is
   removed entirely.

### Behavioral changes per surface

- `hearth resource invoke` (CLI): stops setting
  `ResourcePrincipalKind/ID` from env. Sends them unset; daemon
  fills from the derived identity. (Newer CLI → older daemon: the
  older daemon sees empty principal fields and authorizes as
  human — degrades safely to human-self-invoke, which is the
  pre-agent-attribution behavior anyway.)

- `hearth resource refresh`: same.

- `hearth run --secret`: same.

- `hearth secret set --kind agent`: the `--kind agent` flag becomes
  inferred from the derived identity. CLI no longer takes the flag
  for self-attribution; daemon stamps `set_by_kind=agent,
  set_by_id=<derived>` when the caller resolved to an agent.
  Explicit `--kind agent` becomes a no-op or removed.

- `hearth agent attach`: same model when it ships.

### Server-side

No changes. The server already verifies the daemon's claimed
`(agent_id, host_id)` pair against the `ai_agent_instances` table.
Once the daemon's claim is trustworthy, the server's existing checks
do the rest.

### Migration / back-compat

The daemon binary and CLI binary always ship together (single
`hearth` executable, two entry points). There's no daemon-CLI
version skew to worry about — they're the same process when bundled.

The only skew is operator-host vs. server: a newer daemon talking
to an older server is fine (server reads the same fields it always
did). An older daemon talking to a newer server is also fine
(server's check on `agent.host_id` hasn't changed).

## Phasing

**Phase 0 — peer-cred plumbing.** Add `peerPID(net.Conn)` helper
(Linux + macOS). Add `agentsByPID` registry. Stamp spawn / exit
hooks. No behavior change yet — just collect telemetry on every
IPC for whether tree-walk would resolve an agent and whether the
env-var-derived claim matches. Ship + bake.

**Phase 1 — trust the derived identity.** Switch every handler that
consumes `ResourcePrincipalKind/ID` to read the derived identity
instead. CLI keeps sending env-var-derived claims; daemon ignores
them. Forgery telemetry switches from WARN to ERROR + reject
(if claim disagrees with tree walk).

**Phase 2 — remove env-var forwarding from CLI.** CLI stops setting
`ResourcePrincipalKind/ID` entirely. Daemon stops looking at those
fields. The env var `HEARTH_AGENT_INSTANCE_ID` may still ride in
the agent's env for harness-internal uses (e.g. `mock_claude.go`
uses it as a session id), but it's no longer an authz rail.

**Phase 3 — harness audit.** Confirm no harness or sub-tool relies
on the env var as security-bearing. The variable becomes purely a
convenience hint for the agent's own logging / debugging.

## Open questions

- **What about the orphan-process case?** An agent that spawns a
  long-running daemon (e.g. a build watcher) that later calls
  `hearth resource invoke` — the daemon's CLI invocation's parent
  is the agent-spawned daemon, whose parent is the agent. Tree walk
  succeeds two hops up. Good. **But** what if the agent-spawned
  daemon outlives the agent itself? Tree-walk hits a dead PID
  (or, depending on OS, finds the orphan reparented to PID 1).
  Per the fallback policy table above this REJECTS the call — the
  orphan can't act on the dead agent's behalf, and (because of the
  no-soft-promotion rule) can't silently re-authorize as the
  operator either. Open: do we want a "grace period" where the
  dead agent's children can still act on its behalf? Probably no
  — death of the agent should end its authority.

- **Should the env var keep being set?** Even after Phase 2, some
  harnesses or downstream tools may need to know "which agent am
  I" for their own purposes. Leaning yes — keep `HEARTH_AGENT_INSTANCE_ID`
  in the agent's env as a convenience signal, but treat it as
  informational, not authoritative.

- **What about `hearth` invocations from the daemon itself?**
  (Tests, maintenance scripts.) Tree walk hits the daemon's PID
  before any agent. Daemon should recognize self and fall back to
  the operator's human principal. Easy.

- **Audit logging.** Phase 0's telemetry could feed a server-side
  audit log entry per spoofed-claim attempt — useful for catching
  malicious agents post-hoc. Open question whether that's worth
  building now or deferring.

- **Windows / WSL.** Not a target today; future tablet stakes raise
  this. WSL's unix sockets support SO_PEERCRED. Native Windows
  would need named pipes + `GetNamedPipeClientProcessId`. Cross
  this bridge when we cross it.

## Why this is right

- The kernel is the only party that can answer "which PID
  connected" honestly. Anything we layer on top of userland-shared
  state (env, argv, files at known paths) is forgeable at the
  same UID.

- Tree walk fits hearth's existing process model: the daemon
  already spawns agents and tracks their PIDs for the supervisor.
  We're adding identity-resolution to an existing tracking layer,
  not building a new substrate.

- Casual spoofing is what we actually need to stop. A determined
  same-UID attacker with ptrace owns the agent's memory either way;
  no userland token scheme defeats them, and we shouldn't pretend
  otherwise.

- The migration is observable. Phase 0's telemetry tells us how
  bad the spoofing surface actually is before we tighten anything,
  and lets us bake the tree-walk implementation against real CLI
  invocation patterns (shell wrappers, harness quirks) before any
  behavior change ships.

## Cross-references

- `project_agent_identity_protocol_todo.md` — the originating note.
- `docs/resource-plugins-1f-plan.md` — current agent-principal
  plumbing through the resource_invoke chain.
- `agent_setup.go` — where `HEARTH_AGENT_INSTANCE_ID` is currently
  injected at spawn.
- `cmd_resource.go`, `cmd_run.go`, `cmd_secret.go` — current CLI
  consumers of the env var.
