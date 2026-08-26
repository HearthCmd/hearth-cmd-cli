//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// daemon_scheduled_trigger.go — the daemon-side executor for scheduled triggers
// ("Routines"). The relay's always-on scheduler decides WHEN to fire and pushes
// a scheduled_trigger_fire frame; this runs it on the host: spawn a temp agent
// or wake an existing one, inject the kickoff as a pseudo-turn, and ack the
// outcome with scheduled_trigger_result.
//
// Delivery here is the v1 STOPGAP the spec calls for: the prompt rides the same
// bracketed-paste inject path as chat mentions, wrapped in a hearth/1 envelope.
// The durable kind=trigger event + overnight-approval behavior are Phase 3.

// scheduledSpawnWait bounds how long we wait for a freshly-spawned instance to
// register its inject hook before giving up.
const scheduledSpawnWait = 30 * time.Second

// scheduledSpawnSettle is a conservative pause after a fresh spawn registers,
// before injecting the kickoff. Harnesses with an inject gate (codex/gemini)
// already block injectFunc until the child is ready; claude/pi do not, so this
// settle covers their startup so the first turn isn't eaten. This is the
// acknowledged imperfect part of the stopgap — Phase 3's host-side inbox
// replaces it with a real readiness/idle signal.
const scheduledSpawnSettle = 4 * time.Second

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
		d.sendTriggerResult(frame.RunID, frame.TargetAIAgentInstanceID, "failed", "unknown target_mode")
	}
}

// fireExistingTrigger delivers into a full-time agent: inject if it's already
// running, otherwise wake it via the server (which sets status='active',
// resolves spawn_context, and pushes the spawn back to us) then inject.
func (d *Daemon) fireExistingTrigger(runID, instanceID string, prompt []byte) {
	if instanceID == "" {
		d.sendTriggerResult(runID, "", "failed", "existing mode missing target instance")
		return
	}

	// Already running → inject straight away (no readiness wait needed).
	if d.daemonWS.lookupAgentWS(instanceID) != nil {
		if d.daemonWS.injectPseudoTurn(instanceID, prompt) {
			d.sendTriggerResult(runID, instanceID, "delivered_live", "")
		} else {
			d.sendTriggerResult(runID, instanceID, "failed", "inject into live agent failed")
		}
		return
	}

	// Asleep → ask the server to wake it. wsWakeAgentInstance flips the DB
	// status and pushes a "wake" frame back to this daemon, which spawns the
	// child; we then wait for that registration, settle, and inject.
	wakeReq, _ := json.Marshal(map[string]string{"id": instanceID})
	resp, err := d.daemonWS.SendWSRequest(generateUUID(), "wake_ai_agent_instance", wakeReq)
	if err != nil {
		d.sendTriggerResult(runID, instanceID, "failed", "wake request: "+err.Error())
		return
	}
	var wr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(resp, &wr) == nil && wr.Error != "" {
		d.sendTriggerResult(runID, instanceID, "failed", "wake: "+wr.Error)
		return
	}
	if d.daemonWS.waitForLiveInstance(instanceID, scheduledSpawnWait) == nil {
		d.sendTriggerResult(runID, instanceID, "failed", "woken agent did not come up in time")
		return
	}
	time.Sleep(scheduledSpawnSettle)
	if d.daemonWS.injectPseudoTurn(instanceID, prompt) {
		d.sendTriggerResult(runID, instanceID, "woke_existing", "")
	} else {
		d.sendTriggerResult(runID, instanceID, "failed", "inject after wake failed")
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
		d.sendTriggerResult(runID, "", "failed", "marshal create payload: "+err.Error())
		return
	}

	resp, err := d.daemonWS.SendWSRequest(generateUUID(), "create_temp_agent_instance", data)
	if err != nil {
		d.sendTriggerResult(runID, "", "failed", "create temp agent: "+err.Error())
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
		d.sendTriggerResult(runID, "", "failed", "parse create_temp_agent_instance response")
		return
	}
	if wrap.Error != "" {
		d.sendTriggerResult(runID, "", "failed", "create temp agent: "+wrap.Error)
		return
	}
	instanceID := wrap.AIAgentInstance.ID
	if instanceID == "" {
		d.sendTriggerResult(runID, "", "failed", "create temp agent returned no id")
		return
	}
	if wrap.SpawnError != "" {
		// Row created but the server couldn't push the spawn — waiting for a
		// registration that will never come is pointless.
		d.sendTriggerResult(runID, instanceID, "failed", "temp agent spawn failed: "+wrap.SpawnError)
		return
	}

	// The server pushed a wake for the new instance; the daemon spawns it
	// asynchronously. Wait for that registration, then settle + inject.
	if d.daemonWS.waitForLiveInstance(instanceID, scheduledSpawnWait) == nil {
		d.sendTriggerResult(runID, instanceID, "failed", "temp agent did not spawn in time")
		return
	}
	time.Sleep(scheduledSpawnSettle)
	if d.daemonWS.injectPseudoTurn(instanceID, prompt) {
		d.sendTriggerResult(runID, instanceID, "spawned_temp", "")
	} else {
		d.sendTriggerResult(runID, instanceID, "failed", "inject into temp agent failed")
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
