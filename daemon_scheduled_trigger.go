//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// daemon_scheduled_trigger.go — the daemon-side executor for scheduled triggers
// ("Routines"). The relay's always-on scheduler decides WHEN to fire and pushes
// a scheduled_trigger_fire frame; this runs it on the host: spawn a temp agent
// or wake an existing one, inject the kickoff as a pseudo-turn, and ack the
// outcome with scheduled_trigger_result.
//
// Delivery rides the same path as chat mentions — a hearth/1 envelope handed to
// the host-side inbox, which injects it when the harness is actually accepting
// turns and confirms against the transcript that it became one
// (docs/agent-inbox-spec.md). The durable kind=trigger event + overnight-
// approval behavior are still Phase 3.
//
// Reporting is two-phase, because "the daemon took it" and "the agent read it"
// stopped being the same event once delivery became confirm-then-dequeue:
//
//   1. on accept  -> status "queued", non-terminal. The relay's overlap gate
//                    counts it as in flight and its reaper leaves it alone for
//                    an hour, because waiting on a busy agent is legitimate.
//   2. on resolve -> the terminal status the accept promised (spawned_temp /
//                    woke_existing / delivered_live) once the transcript
//                    confirms the kickoff landed, or "failed" if the inbox gave
//                    up on it.
//
// The pending terminal status rides in the inbox entry's Source rather than an
// in-memory map, so a daemon that restarts mid-queue still reports the run's
// outcome when the message finally lands.

// scheduledSpawnWait bounds how long we wait for a freshly-spawned instance to
// register its inject hook before giving up.
const scheduledSpawnWait = 30 * time.Second

// (The old scheduledSpawnSettle constant lived here: a blind 4s pause after a
// fresh spawn, hoping the harness was ready. The host-side inbox replaced it —
// the kickoff is queued and drains on a real readiness edge, with the boot
// settle handled by agentReadiness. See docs/agent-inbox-spec.md.)

// handleScheduledTriggerFire is wired to DaemonWS.scheduledTriggerFireFunc and
// runs in its own goroutine (it blocks on spawns and server round-trips).
func (d *Daemon) handleScheduledTriggerFire(raw json.RawMessage) {
	// Note: the frame also carries spawn_context (Phase 2b), but the daemon does
	// not use it — an asleep existing agent is woken via the server's
	// wake_ai_agent_instance (below), which resolves spawn_context itself AND
	// keeps the DB status consistent. Self-waking from the frame would leave the
	// agent's status='asleep' while its process runs.
	var frame struct {
		RunID                   string `json:"run_id"`
		ScheduledTriggerID      string `json:"scheduled_trigger_id"`
		OrganizationID          string `json:"organization_id"`
		Name                    string `json:"name"`
		TargetMode              string `json:"target_mode"`
		TargetAIAgentInstanceID string `json:"target_ai_agent_instance_id"`
		RecipeOverridesJSON     string `json:"recipe_overrides_json"`
		Prompt                  string `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil || frame.RunID == "" {
		log.Printf("daemon: scheduled_trigger_fire unmarshal failed: %v", err)
		return
	}
	log.Printf("daemon: scheduled_trigger_fire run=%s mode=%s trigger=%s", frame.RunID, frame.TargetMode, frame.ScheduledTriggerID)

	prompt := buildScheduledTriggerPrompt(frame.Name, frame.Prompt)

	switch frame.TargetMode {
	case "existing":
		d.fireExistingTrigger(frame.RunID, frame.TargetAIAgentInstanceID, prompt)
	case "temp":
		d.fireTempTrigger(frame.RunID, frame.OrganizationID, frame.Name, frame.RecipeOverridesJSON, prompt)
	default:
		log.Printf("daemon: scheduled_trigger_fire unknown target_mode %q", frame.TargetMode)
		d.sendTriggerResult(frame.RunID, frame.TargetAIAgentInstanceID, runStatusFailed, "unknown target_mode")
	}
}

// fireExistingTrigger delivers into a full-time agent: inject if it's already
// running, otherwise wake it via the server (which sets status='active',
// resolves spawn_context, and pushes the spawn back to us) then inject.
func (d *Daemon) fireExistingTrigger(runID, instanceID string, prompt []byte) {
	if instanceID == "" {
		d.sendTriggerResult(runID, "", runStatusFailed, "existing mode missing target instance")
		return
	}

	// Already running → queue it; the inbox picks the moment.
	if d.daemonWS.lookupAgentWS(instanceID) != nil {
		if d.queueTriggerTurn(runID, instanceID, prompt, runStatusDeliveredLive) {
			d.sendTriggerResult(runID, instanceID, runStatusQueued, "")
		} else {
			d.sendTriggerResult(runID, instanceID, runStatusFailed, "could not queue kickoff for live agent")
		}
		return
	}

	// Asleep → ask the server to wake it. wsWakeAgentInstance flips the DB
	// status and pushes a "wake" frame back to this daemon, which spawns the
	// child; we then wait for that registration, settle, and inject.
	wakeReq, _ := json.Marshal(map[string]string{"id": instanceID})
	resp, err := d.daemonWS.SendWSRequest(generateUUID(), "wake_ai_agent_instance", wakeReq)
	if err != nil {
		d.sendTriggerResult(runID, instanceID, runStatusFailed, "wake request: "+err.Error())
		return
	}
	var wr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(resp, &wr) == nil && wr.Error != "" {
		d.sendTriggerResult(runID, instanceID, runStatusFailed, "wake: "+wr.Error)
		return
	}
	if d.daemonWS.waitForLiveInstance(instanceID, scheduledSpawnWait) == nil {
		d.sendTriggerResult(runID, instanceID, runStatusFailed, "woken agent did not come up in time")
		return
	}
	if d.queueTriggerTurn(runID, instanceID, prompt, runStatusWokeExisting) {
		d.sendTriggerResult(runID, instanceID, runStatusQueued, "")
	} else {
		d.sendTriggerResult(runID, instanceID, runStatusFailed, "could not queue kickoff after wake")
	}
}

// fireTempTrigger mints a fresh temp agent via the existing
// create_temp_agent_instance server flow (which pushes the spawn back to this
// daemon), waits for it to come up, and injects the kickoff as its first turn.
func (d *Daemon) fireTempTrigger(runID, organizationID, name, recipeOverridesJSON string, prompt []byte) {
	payload := map[string]interface{}{
		"organization_id": organizationID,
		"host_id":         d.hostID,
		"name":            name,
	}
	// Pass through any recipe overrides the Routine pinned; unset fields fall
	// back to organization_temp_agent_defaults server-side.
	if recipeOverridesJSON != "" {
		var ov map[string]interface{}
		if json.Unmarshal([]byte(recipeOverridesJSON), &ov) == nil {
			for _, k := range []string{"harness_id", "ai_brain_model_id", "directory_path"} {
				if v, ok := ov[k]; ok {
					payload[k] = v
				}
			}
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		d.sendTriggerResult(runID, "", runStatusFailed, "marshal create payload: "+err.Error())
		return
	}

	resp, err := d.daemonWS.SendWSRequest(generateUUID(), "create_temp_agent_instance", data)
	if err != nil {
		d.sendTriggerResult(runID, "", runStatusFailed, "create temp agent: "+err.Error())
		return
	}
	var wrap struct {
		AIAgentInstance struct {
			ID string `json:"id"`
		} `json:"ai_agent_instance"`
		SpawnError string `json:"spawn_error"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(resp, &wrap); err != nil {
		d.sendTriggerResult(runID, "", runStatusFailed, "parse create_temp_agent_instance response")
		return
	}
	if wrap.Error != "" {
		d.sendTriggerResult(runID, "", runStatusFailed, "create temp agent: "+wrap.Error)
		return
	}
	instanceID := wrap.AIAgentInstance.ID
	if instanceID == "" {
		d.sendTriggerResult(runID, "", runStatusFailed, "create temp agent returned no id")
		return
	}
	if wrap.SpawnError != "" {
		// Row created but the server couldn't push the spawn — waiting for a
		// registration that will never come is pointless.
		d.sendTriggerResult(runID, instanceID, runStatusFailed, "temp agent spawn failed: "+wrap.SpawnError)
		return
	}

	// The server pushed a wake for the new instance; the daemon spawns it
	// asynchronously. Wait for that registration, then queue the kickoff —
	// the inbox holds it until the harness is actually accepting turns.
	if d.daemonWS.waitForLiveInstance(instanceID, scheduledSpawnWait) == nil {
		d.sendTriggerResult(runID, instanceID, runStatusFailed, "temp agent did not spawn in time")
		return
	}
	if d.queueTriggerTurn(runID, instanceID, prompt, runStatusSpawnedTemp) {
		d.sendTriggerResult(runID, instanceID, runStatusQueued, "")
	} else {
		d.sendTriggerResult(runID, instanceID, runStatusFailed, "could not queue kickoff for temp agent")
	}
}

// buildScheduledTriggerPrompt wraps the Routine's prompt in a hearth/1 envelope
// so the transcript renderer can tag it as an automated kickoff rather than a
// human turn. The agent extracts the body after the blank line and treats it as
// its task for this run. (Phase 3 upgrades this to a durable kind=trigger event.)
func buildScheduledTriggerPrompt(name, prompt string) []byte {
	var b []byte
	b = append(b, []byte("hearth/1 {\"kind\":\"scheduled_trigger\"}\n\n")...)
	if name != "" {
		b = append(b, []byte(fmt.Sprintf("[Scheduled Routine: %s]\n\n", name))...)
	}
	b = append(b, []byte(prompt)...)
	return b
}

// sendTriggerResult acks a fire back to the relay. The run status rides under
// "data", which is where handleScheduledTriggerResult reads it (keyed by
// run_id, not instance id — so a temp-mint failure with an empty instanceID
// still resolves its run; the relay's guard exempts this frame type). The
// top-level ai_agent_instance_id is set when known for routing/consistency.
func (d *Daemon) sendTriggerResult(runID, instanceID, status, detail string) {
	frame := map[string]interface{}{
		"type":                 "scheduled_trigger_result",
		"ai_agent_instance_id": instanceID,
		"data": map[string]interface{}{
			"run_id":               runID,
			"status":               status,
			"ai_agent_instance_id": instanceID,
			"detail":               detail,
		},
	}
	b, err := json.Marshal(frame)
	if err != nil {
		log.Printf("daemon: marshal scheduled_trigger_result: %v", err)
		return
	}
	d.daemonWS.ws.SendText(b)
	log.Printf("daemon: scheduled_trigger_result run=%s status=%s instance=%s", runID, status, instanceID)
}

// Run-status vocabulary, mirroring the relay's constants in scheduler.go. The
// daemon reports these back over scheduled_trigger_result; keeping them named
// here rather than as inline strings is what makes the two-phase handoff below
// legible.
const (
	runStatusQueued        = "queued"
	runStatusDeliveredLive = "delivered_live"
	runStatusWokeExisting  = "woke_existing"
	runStatusSpawnedTemp   = "spawned_temp"
	runStatusFailed        = "failed"
)

// triggerSourcePrefix marks an inbox entry as a Routine kickoff. The rest of the
// Source field is "<run_id>:<terminal_status>" — the run to report against, and
// the status to report if it lands. Encoding it in the row rather than holding a
// map in memory is what lets a daemon restart mid-queue and still close out the
// run.
const triggerSourcePrefix = "scheduled_trigger:"

// queueTriggerTurn hands the kickoff to the target agent's inbox, tagged so
// handleInboxResolved can report the outcome later. Returns false only when the
// instance isn't live on this daemon.
func (d *Daemon) queueTriggerTurn(runID, instanceID string, prompt []byte, terminalStatus string) bool {
	source := triggerSourcePrefix + runID + ":" + terminalStatus
	return d.daemonWS.deliverTurn(instanceID, prompt, source, inboxTTLTrigger)
}

// handleInboxResolved closes out a Routine run once its kickoff has actually
// reached the agent — or once the inbox has given up on it. Wired to
// DaemonWS.inboxResolvedFunc; ignores every entry that isn't a trigger kickoff.
func (d *Daemon) handleInboxResolved(e *inboxEntry, outcome string) {
	if e == nil {
		return
	}
	// Every abandoned message gets reported to the relay, whatever produced it,
	// so the sender's transcript row stops claiming it is on its way. Runs
	// before the trigger branch because it applies to all sources.
	d.reportUndeliverable(e, outcome)

	if !strings.HasPrefix(e.Source, triggerSourcePrefix) {
		return
	}
	rest := strings.TrimPrefix(e.Source, triggerSourcePrefix)
	runID, terminalStatus, ok := strings.Cut(rest, ":")
	if !ok || runID == "" {
		log.Printf("daemon: trigger inbox entry %s has malformed source %q", e.Key, e.Source)
		return
	}

	switch outcome {
	case inboxOutcomeConfirmed, inboxOutcomeLandedLate:
		d.sendTriggerResult(runID, e.InstanceID, terminalStatus, "")
	case inboxOutcomeExpired:
		d.sendTriggerResult(runID, e.InstanceID, runStatusFailed,
			"agent stayed busy until the kickoff expired")
	case inboxOutcomeQuarantined:
		d.sendTriggerResult(runID, e.InstanceID, runStatusFailed,
			"could not deliver kickoff: "+e.Reason)
	case inboxOutcomeUnconfirmable:
		// Injected, but with nothing unique to watch for. Report the success
		// the accept promised rather than inventing a failure — but say so,
		// since the confirmation this status normally implies is missing.
		d.sendTriggerResult(runID, e.InstanceID, terminalStatus, "delivered without transcript confirmation")
	default:
		log.Printf("daemon: unhandled inbox outcome %q for trigger run %s", outcome, runID)
	}
}
