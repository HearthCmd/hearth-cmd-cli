//go:build darwin || linux

package main

import (
	"encoding/json"
	"log"
)

// daemon_undeliverable.go — telling the relay when the inbox gives up.
//
// The inbox abandons a message two ways: quarantine (repeated delivery attempts
// with no transcript confirmation) and expiry (the TTL elapsed while the agent
// stayed busy). Both used to be host-only knowledge — a WARN in daemon.log and
// `hearth hh agent inbox`. The person who sent the message was told nothing,
// and once the app started rendering queued messages their row sat reading
// "Waiting for the agent to finish" forever.
//
// This is the reporting half. It rides the existing inboxDeliverer.onResolved
// hook — the same one scheduled-trigger runs use — so no new mechanism, just a
// second consumer.

// reportUndeliverable tells the relay a message will never reach its agent.
//
// Only abandonment outcomes report. A confirmed delivery needs no frame (the
// transcript entry IS the signal), and an unconfirmable one was injected — we
// simply have no probe to watch, which is not the same as failing.
func (d *Daemon) reportUndeliverable(e *inboxEntry, outcome string) {
	switch outcome {
	case inboxOutcomeQuarantined, inboxOutcomeExpired:
	default:
		return
	}
	if d.daemonWS == nil || e.InstanceID == "" {
		return
	}

	// The mid is how the relay and the app address the row this message
	// occupies. A payload with no envelope has no row to correct — nothing a
	// person sent lacks one, so this is the harness's own traffic, not a
	// household's message.
	mid := hearthEnvelopeMID(e.Payload)
	if mid == "" {
		return
	}

	reason := e.Reason
	if reason == "" && outcome == inboxOutcomeExpired {
		reason = "expired before the agent became available"
	}

	frame := map[string]interface{}{
		"type":                 "agent_message_undeliverable",
		"ai_agent_instance_id": e.InstanceID,
		"data": map[string]interface{}{
			"mid":      mid,
			"outcome":  outcome,
			"reason":   reason,
			"source":   e.Source,
			"attempts": e.Attempts,
		},
	}
	b, err := json.Marshal(frame)
	if err != nil {
		log.Printf("daemon-inbox: marshal undeliverable report for %s: %v", e.Key, err)
		return
	}
	d.daemonWS.ws.SendText(b)
	log.Printf("daemon-inbox: reported %s message %s to the relay as %s", e.Source, e.Key, outcome)
}
