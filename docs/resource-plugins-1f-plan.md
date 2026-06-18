# Resource plugins — sub-phase 1f implementation plan

**Status:** planning. Branch `resource-plugins` (CLI head d7dbce5,
1e landed; server head 1ed42c6, 1e landed). Predecessor:
`docs/resource-plugins-1e-plan.md`. Authoritative architecture:
`/Users/mattbeller/projects/hearth-cmd/docs/external-resource-adapters.md`.

**Goal of 1f:** spawned agents can invoke plugin verbs. After 1f a
running agent process can shell out to `hearth resource invoke`,
have the call recognized as an agent-origin invoke (not the
operator's), get authorized under the agent's principal, and see
the plugin's response. The agent's system prompt knows what
verbs / connections are available.

**Out of scope for 1f:**
- Ask wiring for `external_resource.*` actions. Still stubbed
  as deny per 1e. iOS-side verb rendering blocks it; orthogonal
  workstream.
- Daemon-local rules cache + evaluator (1g).
- Credential broker (1d).
- Per-agent fine-grained IAM UX. Rules are seeded by re-using the
  operator's defaults at every owned-agent's principal scope; the
  user can later edit them in mobile UX (phase 2) without code
  change.

## 1. Architecture summary

```
agent process (e.g. claude) runs:
   $ hearth resource invoke ha-home turn_on '{"entity_id":"light.kitchen"}'
   │
   │  (Bash tool call gets gated by tool.Bash authz)
   │  → seeded auto-allow rule for Bash(hearth resource invoke *)
   │     skips the human prompt; allows the inner subcommand to run
   ▼
hearth CLI (runResourceInvoke):
   │  reads HEARTH_AGENT_INSTANCE_ID from env (injected at spawn)
   │  sends ipcRequest{Type: "resource_invoke",
   │                   PrincipalKind: "agent",
   │                   PrincipalID:   <ai_agent_instance_id>,
   │                   ResourceConnectionID, ResourceVerb, ResourceArgs}
   ▼
daemon: handleResourceInvoke
   │  preflightAuthorizeResourceInvoke with principal_kind=agent
   ▼
server: handleAuthorizeResourceInvoke
   s.Authorize(Principal{Kind: agent, ID: <agent_id>}, ...)
   │  matches rules at ('principal', <agent_id>) scope
   │  (rules seeded at agent spawn time + 1e boot seed extended)
   ▼
Allow → daemon dispatches PluginSupervisor.Invoke
Deny  → daemon returns ErrForbidden to agent (stdin/stdout pipe)
```

## 2. Wire shape changes

### `ipcRequest` extension (daemon.go)

```go
ResourcePrincipalKind string `json:"resource_principal_kind,omitempty"`
ResourcePrincipalID   string `json:"resource_principal_id,omitempty"`
```

CLI populates these from `HEARTH_AGENT_INSTANCE_ID` if set;
empty → kind defaults to "human", id defaults to `d.humanUserID`
(today's behavior). Field names use the `resource_` prefix to
match the existing 1c-era fields.

### `authorize_resource_invoke` server payload

Already carries `principal_id` and `principal_kind` (1e). No wire
change server-side. The daemon now forwards the CLI-supplied
fields verbatim.

The 1e principal-spoof guard at the server "principal_id must
match authenticated daemon's humanUserID when principal_kind=human"
must be extended to also accept principal_kind=agent and validate
the agent belongs to the authenticated daemon's user. Adds a
single owner-lookup query.

## 3. Server-side changes (hearth-cmd)

### Owner-validation for agent principals

`handleAuthorizeResourceInvoke` already validates the human case.
Extend:

```go
case "agent":
    if !s.daemonOwnsAgent(humanUserID, req.PrincipalID) {
        writeAuthzErr(w, "agent_instance not owned by authenticated daemon's user")
        return
    }
```

`daemonOwnsAgent(userID, agentID)` SELECTs from `ai_agent_instances`
joined with whichever owner table relates them to humans. Cached
read; cheap.

### Re-seed per owned agent

`handleSeedResourceConnectionRules` currently writes each rule
under `('principal', humanUserID)`. 1f extends it: also enumerate
the human's owned ai_agent_instances at seed time and write a
parallel rule under `('principal', <agent_id>)` for each.

```go
agents := s.ownedAgentInstanceIDs(humanUserID)
// for each rule, writeRule once per (humanUserID + each agentID)
```

Same `writeRule` chokepoint; just N+1 INSERTs per rule. Response
shape grows a `principals` field for log visibility:

```json
{"type":"seed_resource_connection_rules_response",
 "seeded":12, "skipped":3,
 "principals":["alice", "agt-7f3a", "agt-8c91"]}
```

### `seed_rules_for_new_agent` hook

When a new ai_agent_instance is created, the existing rules at
the owner-human's principal scope don't auto-extend to it. Add a
post-create hook that, for each existing
`('principal', humanUserID, resource_kind='external_resource')`
rule, writes a mirror under the new agent's principal.

Hook into the existing `create_ai_agent_instance` handler. The
hook is idempotent (INSERT OR IGNORE via writeRule) so a
duplicate-create can't multiply rows.

## 4. Daemon-side changes (hearth-cmd-cli)

### CLI: pick up agent context from env

`runResourceInvoke` (cmd_resource.go) reads
`HEARTH_AGENT_INSTANCE_ID`; when set, includes it as
`resource_principal_id` with `resource_principal_kind: "agent"`
on the outgoing IPC. When unset, today's human-principal behavior
is preserved.

No CLI flag; pure env-based. Agents inherit the env from spawn;
humans running the CLI never have it set.

### Daemon: pass principal through preflight

`handleResourceInvoke` accepts the new fields and threads them
into `preflightAuthorizeResourceInvoke`. Default fallback
(kind="human", id=d.humanUserID) when fields absent.

`preflightAuthorizeResourceInvoke` already takes principal as
parameters; just plumbing.

### Daemon: agent spawn injects env + seeds Bash auto-allow

Two changes at agent-spawn time (the existing
`buildAgentCommand` / `newAgentInstance` path):

1. **Env injection.** Add `HEARTH_AGENT_INSTANCE_ID=<id>` to the
   agent process's environment. Mirrors existing pattern (the
   spawn path already builds an env list).

2. **Bash auto-allow rule seed.** Send a `seed_tool_rule` (new
   minimal WS msg, or extend existing rule-write paths) writing
   one rule:
   ```
   principal_scope_kind  = 'principal'
   principal_scope_value = <ai_agent_instance_id>
   resource_kind         = 'tool'
   action                = 'Bash'
   predicate             = 'hearth resource invoke *'
   decision              = 'allow'
   ```
   Idempotent. Lets the agent's Bash interpose see `hearth
   resource invoke ...` as auto-allowed at the tool layer; the
   actual permission gate is the daemon's external_resource authz
   downstream.

   The agent's existing seed-from-default-rules pipeline (3.
   above) already handles external_resource rules. This is the
   one additional rule we need for the Bash interpose path.

### Daemon: prompt-side verb surfacing

A new helper `buildResourcePluginPrompt(devConns, registry) string`
renders per-verb detail from each dev connection's manifest:

```
You have access to external resources via plugins. Invoke verbs
via Bash:

  hearth resource invoke <connection-id> <verb> [args-json]

Available connections:

  ha-home (Home Assistant)
    turn_on   — Turn a switchable entity on.
                args: { "entity_id": "<ha entity id>" }
    turn_off  — Turn a switchable entity off.
                args: { "entity_id": "<ha entity id>" }
    lock      — Lock a lockable entity.
                args: { "entity_id": "<ha entity id>" }
    ...

  gdrive-personal (Google Drive)
    read      — Read the contents of a file.
                args: { "file_id": "<drive file id>" }
    ...

Resource invokes are authorized separately from Bash tool calls
and have their own audit log. Errors come back as
"hearth resource: <code>: <message>".
```

Source data: for each `DevConnection`, look up its
`PluginManifest`, enumerate `manifest.Verbs[]` with each verb's
`Description` and `Args` shape. Empty when no dev connections —
omit the block entirely so non-plugin agents don't pay tokens.

Injection: prepend (or append, TBD per-harness) to the existing
`hearthSystemPrompt`. Most harnesses already accept
`--append-system-prompt`; the new block joins in the same way.

## 5. Step-by-step commit plan

| # | Title | Files | Repo | Scope |
|---|-------|-------|------|-------|
| 1 | `resource plugins: scoping plan for sub-phase 1f` | `docs/resource-plugins-1f-plan.md` | cli | S |
| 2 | `plugins: seed handler writes rules under owned agents too` | resource_plugins_ws.go + helper + tests | server | M |
| 3 | `plugins: seed-on-agent-create hook` | edits create_ai_agent_instance path + tests | server | M |
| 4 | `plugins: server authorize accepts agent principal (owner-validated)` | resource_plugins_ws.go + tests | server | S |
| 5 | `plugins: CLI + daemon thread principal_kind through IPC` | cmd_resource.go, daemon.go, resource_authorize.go, tests | cli | M |
| 6 | `plugins: agent spawn injects HEARTH_AGENT_INSTANCE_ID + tool.Bash auto-allow seed` | edits agent spawn path + tests | cli | M |
| 7 | `plugins: agent system prompt lists available verbs` | new resource_prompt.go + edits agent_setup.go + tests | cli | M |

Server commits (2–4) ship first since #5 (daemon) depends on the
server accepting agent principals. Both repos already on
`resource-plugins`.

## 6. Tests

### Server (#2–#4)
- `TestSeedResourceRules_AlsoSeedsPerAgent` — set up two owned
  agents; assert rules written under three principal scopes
  (human + two agents); response `principals` field listed.
- `TestSeedRulesForNewAgent_MirrorsExisting` — write a human-scope
  external_resource rule; create an agent; assert the rule is
  mirrored under the new agent's scope.
- `TestAuthorizeResourceInvoke_AgentPrincipalAllowed` — seed an
  agent-scoped allow rule; authz with kind=agent matches.
- `TestAuthorizeResourceInvoke_AgentNotOwnedRejected` — kind=agent
  but ai_agent_instance is owned by some other human → error.

### Daemon (#5–#7)
- `TestHandleResourceInvoke_AgentPrincipal` — IPC with
  resource_principal_kind="agent" forwards to authz with
  PrincipalKind=agent (verify via fakeAuthzWS pinning the
  payload).
- `TestCLI_PicksUpAgentEnvVar` — runResourceInvoke with
  HEARTH_AGENT_INSTANCE_ID set populates the ipcRequest fields.
- `TestBuildResourcePluginPrompt_OmitsWhenEmpty` — no dev
  connections → empty string (no token cost).
- `TestBuildResourcePluginPrompt_RendersVerbs` — fixture manifest
  → expected text shape.

## 7. Risks / open questions

1. **Owner trust on the daemon socket.** The daemon trusts the
   `HEARTH_AGENT_INSTANCE_ID` env var as set by the CLI. Any
   process running as the same Unix user could spoof it. The
   socket is already user-permissioned, so the threat model is
   "the operator could already authorize as themselves" — same
   trust level as today. Worth noting in passing; not a 1f blocker.
2. **HEARTH_AGENT_INSTANCE_ID set in operator's shell by accident.**
   If the operator exports it in their interactive shell, every
   `hearth resource invoke` they run would authorize as the agent.
   Mostly self-inflicted, but the CLI could warn ("looks like
   HEARTH_AGENT_INSTANCE_ID is set; running as agent X — is that
   what you wanted?") on stderr. Punt; flag.
3. **Agent retiring doesn't clean up its rules.** Per-agent rule
   rows accumulate. Acceptable for 1f; phase 2's CRUD UX can
   prune.
4. **System prompt token budget.** Per-verb detail can grow
   unbounded with many connections × verbs. For HA-class plugins
   that's 20+ verbs × multiple connections. Plan §4 keeps the
   block omitted when empty; if it bites with many verbs, switch
   to a lazy "ask for verb list via `hearth resource verbs <conn>`"
   discovery pattern in a later sub-phase.
5. **Bash auto-allow rule shape brittleness.** The seeded rule
   gates on `Bash(hearth resource invoke *)`. If the operator
   later renames or wraps the CLI, the rule misses. Acceptable;
   we can re-seed.
6. **Server owner-lookup query.** `daemonOwnsAgent(userID,
   agentID)` runs on every agent invoke. Should be indexed; the
   existing `ai_agent_instances` table is keyed on id, so the
   lookup is cheap. No new index expected.
7. **Ask still stubbed.** Agents whose verbs miss seeded
   rules see deny (`reason=ask_not_implemented`). Document
   in the prompt? Or let it surface naturally? Lean naturally —
   operator can read the error and seed a rule manually.

## Cross-references

- `hearth-cmd/cmd/hearth-cloud/resource_plugins_ws.go` — 1e
  handlers, extended in commits 2–4.
- `hearth-cmd-cli/cmd_resource.go` — 1c CLI, extended in commit 5.
- `hearth-cmd-cli/daemon.go:handleResourceInvoke` — 1c+1e
  handler, extended in commit 5.
- `hearth-cmd-cli/resource_authorize.go` — 1e preflight,
  extended in commit 5.
- `hearth-cmd-cli/agent_setup.go` — agent spawn entry point;
  commits 6 + 7 hook here.
- `hearth-cmd-cli/agent.go` — `hearthSystemPrompt` is the
  injection point for commit 7.
- `docs/resource-plugins-1e-plan.md` — predecessor; §9.5 flagged
  the principal-shift discussed here.
