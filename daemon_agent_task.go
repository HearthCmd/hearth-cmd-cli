package main

import (
	"encoding/json"
	"log"
)

// handleAgentTask consumes an `agent_task` frame (onboarding LW-3): a
// system-initiated reasoning task for a specific agent — today the Onboarding
// Facilitator, asked to propose a provisioning bundle. It reuses the scheduler's
// wake+inject substrate (deliverTurn, plus a server-driven wake for an asleep
// agent) but carries none of the scheduled-trigger run/ack machinery: there is no
// run to report against; the real signal is the proposal the agent then files.
//
// Runs in its own goroutine (see daemon_ws.go dispatch) because the wake+wait
// path blocks and must not stall the WS read loop.
func (d *Daemon) handleAgentTask(raw json.RawMessage) {
	var frame struct {
		AIAgentInstanceID string `json:"ai_agent_instance_id"`
		OrganizationID    string `json:"organization_id"`
		Prompt            string `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil || frame.AIAgentInstanceID == "" || frame.Prompt == "" {
		log.Printf("daemon: agent_task unmarshal failed or missing fields: %v", err)
		return
	}
	instanceID := frame.AIAgentInstanceID
	prompt := []byte(frame.Prompt)
	const source = "agent-task:onboarding"

	// Already running → inject as a queued turn.
	if d.daemonWS.lookupAgentWS(instanceID) != nil {
		if !d.daemonWS.deliverTurn(instanceID, prompt, source, inboxTTLTrigger) {
			log.Printf("daemon: agent_task could not queue turn for live agent %s", instanceID)
		}
		return
	}

	// Asleep → ask the server to wake it (flips DB status, resolves spawn_context,
	// pushes a wake frame back to us), wait for it to come up, then inject.
	wakeReq, _ := json.Marshal(map[string]string{"id": instanceID})
	resp, err := d.daemonWS.SendWSRequest(generateUUID(), "wake_ai_agent_instance", wakeReq)
	if err != nil {
		log.Printf("daemon: agent_task wake request for %s failed: %v", instanceID, err)
		return
	}
	var wr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(resp, &wr) == nil && wr.Error != "" {
		log.Printf("daemon: agent_task wake for %s: %s", instanceID, wr.Error)
		return
	}
	if d.daemonWS.waitForLiveInstance(instanceID, scheduledSpawnWait) == nil {
		log.Printf("daemon: agent_task woken agent %s did not come up in time", instanceID)
		return
	}
	if !d.daemonWS.deliverTurn(instanceID, prompt, source, inboxTTLTrigger) {
		log.Printf("daemon: agent_task could not queue turn after waking %s", instanceID)
	}
}
