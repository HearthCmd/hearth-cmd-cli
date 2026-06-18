# Resource plugins — sub-phase 1h implementation plan

**Status:** planning. Branches `resource-plugins` (CLI head da60d2f,
1d landed; server head 8dee38a, 1f landed). Predecessor:
`docs/resource-plugins-1d-plan.md`. Authoritative architecture:
`/Users/mattbeller/projects/hearth-cmd/docs/external-resource-adapters.md`
§"IAM integration" / "Decision flow".

**Goal of 1h:** the IAM tripod's third leg. Today `Authorize()` can
return Allow ✓ and Deny ✓ end-to-end; Ask short-circuits to
deny with reason `ask_not_implemented`. After 1h, Ask
fans out to a human's phone via the same rail that today carries
tool-call permission requests, blocks the plugin invoke until the
human responds, and (on "always allow") writes a real
`external_resource.<plugin>.<verb>` rule.

**The big design call (settled in scoping):** we do *not* add a
sibling frame type for resource Asks, and we do *not*
synthesize tool-call fields at the call site. Instead we
**generalize the existing `permission_request` wire frame** to
carry a polymorphic subject. Today's tool-call asks become one
`subject_kind`. Resource invokes are another. Future asks
(agent_spawn, rule_proposal, cross-user requests, meta-rules) plug
in as new `subject_kind` values without touching the rail's
cross-cutting machinery (Pro gating, push cooldown, dispatch
broadcast, missed-request replay, silent-dismiss).

This means 1h is partly a refactor of permission_request itself.
The refactor is the load-bearing part — wiring Ask for
external_resource is the *first user* of the generalized rail, not
the point of the work.

**Out of scope for 1h:**
- Mobile UX beyond the new renderer needed to display
  external_resource asks. Approve / deny / always-allow chrome,
  cooldown matrix, dispatch row — all reused unchanged.
- New subject_kinds beyond `tool_call` (preserved) and
  `external_resource_invoke` (new). Frame schema is extensible but
  we don't pre-build agent_spawn / rule_proposal.
- Per-verb "always allow this verb / always allow any verb on this
  connection" choice. 1h writes verb-specific rules; a broader-scope
  button is a phase-2 mobile rule editor concern.
- Out-of-band approval channels (someone else's phone for an agent
  invoke). The existing `eligibleApproversForAgent` fanout carries
  agent-initiated asks; human-initiated CLI invokes push only to
  the invoking human's own devices.
- Audit-log row for Ask decisions. Existing
  `request_log` / `request_outcomes` schema captures tool-call
  outcomes; extending it to resource invokes is its own follow-up
  (and lands more cleanly after 1g's daemon-local evaluator
  arrives, since that's where the audit-push wire format gets
  exercised in earnest).

## 1. Architecture summary

```
daemon                          server                       phone
──────                          ──────                       ─────
authorize_resource_invoke ────► s.Authorize → Ask
                                 │
                                 ▼
                                build PendingRequest{
                                  SubjectKind:
                                    "external_resource_invoke",
                                  Subject: {plugin_type, verb,
                                            connection_id, args,
                                            principal}
                                }
                                 │
                                 ▼
                                fanoutPermissionRequest
                                  (subject-aware) ─────────► permission_request
                                                              {subject_kind,
                                                               subject, ...}
                                                              │
                                                              ▼
                                                            iOS dispatch
                                                              on subject_kind
                                                              ├─ tool_call render
                                                              └─ external_resource
                                                                  invoke render
                                                              │
                                                              ▼
                                                            human: allow /
                                                              deny / always
                                handlePermissionResponse  ◄──┘
                                  ├─ resolve PendingRequest.Response
                                  └─ if always_allow:
                                       dispatch on SubjectKind →
                                         write rule
authorize_resource_invoke_response
  ◄──── allow / deny ──────────── (chan delivers, handler returns)
  │
  ▼
dispatch verb to plugin
  (or refuse)
```

## 2. Wire shape — generalized `permission_request`

Today's frame (`WSMessage` fields):

```
{type: "permission_request",
 request_id, tool_name, tool_input, ai_agent_instance_id,
 timeout, segments, outside_project, ...}
```

New shape, additive:

```
{type: "permission_request",
 request_id,
 subject_kind: "tool_call" | "external_resource_invoke",
 subject:      <json, kind-specific schema>,

 // legacy mirror — for kind=tool_call only, kept one release:
 tool_name, tool_input, segments, outside_project,

 ai_agent_instance_id, timeout, ...}
```

Subject schemas:

```
subject_kind = "tool_call"
subject = {tool_name, tool_input, project, segments, outside_project}

subject_kind = "external_resource_invoke"
subject = {plugin_type, verb, connection_id, args,
           principal_kind, principal_id}
```

For tool_call asks, server emits *both* the new `subject_kind` /
`subject` fields *and* the legacy top-level mirror. Clients prior
to the renderer update read the legacy fields; updated clients
prefer `subject`. After all clients ship the renderer dispatch we
remove the legacy mirror (separate commit, deferred).

For external_resource asks, legacy fields stay empty. Pre-update
clients render them as a blank/unknown tool — same fallback they
get for any future ask type.

## 3. Server changes — `cmd/hearth-cloud/`

### 3a. `PendingRequest` gains SubjectKind + Subject

```go
type PendingRequest struct {
    // ...existing fields stay...
    SubjectKind string          // "tool_call" (default) | "external_resource_invoke"
    Subject     json.RawMessage // kind-specific payload
}
```

Existing tool-call writers in `handleRelayPermissionRequest` set
`SubjectKind = "tool_call"` and build the matching Subject from
their existing fields. No behavior change at this layer for
tool calls.

### 3b. `fanoutPermissionRequest` becomes subject-aware

Signature gains a `pending *PendingRequest` (or splits into
subject-shaped params) so it can read SubjectKind + Subject and:
- emit both the new fields and the legacy mirror on the WS frame;
- pick the right approver set:
  - `subject_kind=tool_call` (agent-initiated): existing
    `eligibleApproversForAgent` path, unchanged.
  - `subject_kind=external_resource_invoke` from an agent:
    same `eligibleApproversForAgent` path keyed on the agent.
  - `subject_kind=external_resource_invoke` from a human:
    push to that human's own devices only — single-element
    approver set built from the principal.
- carry the existing dispatch broadcast (notified vs authorized
  matrix) unchanged.

### 3c. `handleAuthorizeResourceInvoke` — the new caller

Replaces today's `default: deny with ask_not_implemented`
branch. On `DecisionAsk`:

```
1. Build PendingRequest{
     RequestID:    req.RequestID,
     HumanUserID:  invoking-or-owning user,
     SubjectKind:  "external_resource_invoke",
     Subject:      json({plugin_type, verb, connection_id,
                         args, principal_kind, principal_id}),
     Response:     make(chan PermissionResponse, 1),
     CreatedAt:    time.Now(),
   }
2. Register in s.pending, defer delete.
3. fanoutPermissionRequest(pending).
4. select on pending.Response with defaultTimeout.
5. Return authorize_resource_invoke_response{decision: allow/deny,
   reason: from-rule-or-human}.
```

`HumanUserID` resolution:
- principal_kind=human: req.PrincipalID directly.
- principal_kind=agent: the agent's `creator_human_user_id` (same
  lookup `daemonOwnsAgent` already runs). This is the user whose
  rule-store gets the always-allow row.

### 3d. `handlePermissionResponse` — kind-dispatch the always-allow path

Today's body matches Bash tool patterns to build a rule. Refactor
to dispatch on `pending.SubjectKind`:

```
switch pending.SubjectKind {
case "tool_call", "":
    // existing logic, untouched
case "external_resource_invoke":
    writeExternalResourceAllowRule(pending, msg)
}
```

`writeExternalResourceAllowRule` inserts:

```
action:   external_resource.<plugin_type>.<verb>
resource: external_resource:<connection_id>
decision: allow
owner:    pending.HumanUserID (or the agent's creator)
```

into the rules table. Identical insert path as the seeded
default_rules from plugin manifests; same predicate-empty shape.

### 3e. `args` capture for the Subject payload

`authorize_resource_invoke` today doesn't carry the verb's args.
Add an `args json.RawMessage` field to the request shape so the
phone can render them. The daemon already has the args at
preflight time — it's plumbed in `cmd_resource.go` and
`resource_authorize.go`. One field added end-to-end.

## 4. Daemon changes — `hearth-cmd-cli/`

### 4a. `authorize_resource_invoke` carries verb args

`ipcRequest` plumbs the marshaled args bytes to
`preflightAuthorizeResourceInvoke`; the WS payload gains `args`.
No other behavior change.

### 4b. `SendWSRequest` timeout raised for this msg type

Today's daemon WS request has a short timeout suited to the
allow-or-deny rule path. Ask holds the request open for up
to `defaultTimeout` (~10 min on the server). Two paths to pick
between (low-stakes, will land whichever is simpler in the actual
SendWSRequest plumbing):

- Per-call timeout override on SendWSRequest. The authorize path
  passes a 10-minute deadline; everything else keeps its default.
- A dedicated `SendWSRequestLong` wrapper. Same idea, slightly
  more boilerplate but no signature change to SendWSRequest.

The synchronous request/response shape stays — the daemon just
waits longer. Fire-and-forget + separate-response-frame was the
other option in scoping and was passed over.

### 4c. Caller resilience

`preflightAuthorizeResourceInvoke` should distinguish
"server returned deny" from "daemon timed out waiting" — the
latter renders as a clear local message instead of leaking the
WS timeout error. Cheap and visible during dogfooding.

## 5. Mobile changes — iOS + Android

The non-optional piece. Without a renderer, the phone receives
the new `subject_kind` but the existing tool-call render assumes
`tool_name` populated and rich. Even with the legacy mirror,
external_resource asks have no Bash/Generic fields to fill — the
fallback would look like a malformed tool call.

### 5a. Frame parser

Read `subject_kind` + `subject` from incoming `permission_request`.
For backwards-compat, treat absent `subject_kind` as `"tool_call"`
and build a synthetic Subject from the legacy top-level fields.
This means iOS doesn't fork on payload presence; it always sees a
subject.

### 5b. Renderer dispatch

```
switch subject_kind {
case "tool_call":              existing tool-call view
case "external_resource_invoke": new ExternalResourceInvokeView
default:                        generic "unknown ask type" view
}
```

The `ExternalResourceInvokeView` is small: connection_id +
plugin_type / verb header, args JSON pretty-printed, the existing
approve / deny / always-allow buttons unchanged. Reuses the
permission-request screen chrome (header, cooldown, missed-list
integration). Android mirrors the iOS structure.

The "unknown ask type" fallback is the forward-compat hatch for
future subject_kinds against older clients.

### 5c. Always-allow button copy

For tool_call: existing copy. For external_resource_invoke:
*"Always allow this verb on this connection"*. Single button; the
broader-scope variant is deferred to phase 2.

## 6. Commit sequence

Eight commits, server-then-CLI-then-mobile-then-dogfood:

1. **server: PendingRequest gains SubjectKind + Subject (no
   behavior change).** Existing tool-call writers fill them.
   Tests assert tool-call path unchanged. Frame on the wire still
   emits only legacy top-level fields (still no mobile work
   required).
2. **server: permission_request frame emits subject_kind + subject
   alongside legacy fields.** Outbound only — clients still read
   legacy. Adds frame-shape test.
3. **server: handleAuthorizeResourceInvoke wires Ask to
   PendingRequest path.** New SubjectKind = "external_resource_invoke".
   Verb args field added to wire shape. Tests cover allow / deny
   / human-times-out / human-approves.
4. **server: always-allow kind-dispatch + external_resource rule
   writer.** Inserts the `external_resource.<plugin>.<verb>` rule.
   Test asserts subsequent same-verb invoke matches the new rule.
5. **CLI: daemon passes verb args in authorize_resource_invoke +
   raises WS request timeout for the call.** Plumbing only. No
   server-side coupling beyond the args field.
6. **CLI: clear local error message on authorize timeout vs
   server deny.** UX polish; cheap.
7. **iOS: permission_request parser reads subject_kind + subject,
   falls back to legacy fields. New ExternalResourceInvokeView
   renderer + always-allow copy.** Renderer dispatch in one place.
   This is the gating commit for mobile dogfooding.
8. **Android: mirror of iOS commit 7.** Same dispatch shape.

Order rationale: 1–4 leave the system in a working state at every
commit (tool calls unaffected; external_resource asks still
short-circuit to deny in 1, allow with no human path in 2–3 until
4 wires the always-allow rule writer end-to-end). 5–6 light up
the CLI side. 7–8 unblock dogfooding.

If iOS or Android lags, the server is still safe — external
resource Asks deny with a clean reason ("approval pending,
no phone renderer") rather than hanging. We add a feature flag in
commit 3 only if needed; current bias is to assume mobile keeps
pace.

## 7. Dogfooding plan

After commit 8:
1. Edit echo plugin manifest, set `whoami`'s default_rule to
   `ask` (currently `allow`).
2. Restart daemon, push manifest update.
3. `hearth resource invoke echo-test whoami` — daemon blocks,
   phone gets a push.
4. Approve on phone → CLI returns 0, whoami output reflects init
   state (scrubbed token still `***`).
5. Re-run, "Always allow" → rule lands in server rules table.
   Verify by inspection (`hh rule list` if/when that ships, or
   direct SQL).
6. Third invoke is auto-allow (no push, no block) — proving the
   always-allow rule fires.

Failure modes to dogfood:
- Push not delivered (Pro off, prefs off) — CLI times out with
  clean error.
- Human denies — CLI returns nonzero with deny reason.
- Human "denies and always" — symmetric rule landing on deny side.
  (Out of scope for 1h's UI but the rule-writer should handle it
  since the kind dispatch is in place.)

## 8. What this unblocks for the future

Each future ask type follows the same pattern:

- **agent_spawn** (another user asks to spawn an agent in your
  org): new `subject_kind="agent_spawn"`, Subject =
  {requested_by, agent_template, org_id, ...}. New iOS renderer.
  Always-allow writes a rule like `agent.spawn` on resource
  `org:<id>` scoped to requester.
- **rule_proposal** (an agent proposes a rule for you to approve):
  new subject_kind, Subject = {proposed_rule}. Renderer shows the
  rule prose. Always-allow writes a meta-rule
  ("always trust this agent's rule proposals on X").
- **cross-org permission** (someone outside your org asks to
  interact with one of your agents): same shape, different
  Subject, same chrome.

None of these need new push categories, new fanout code, new
cooldown logic, new missed-request handling, or new dispatch
broadcasts. They just need a Subject schema, a server-side caller
that builds a PendingRequest, a rule-writer for always-allow, and
a renderer.

That's the rail being settled in 1h.
