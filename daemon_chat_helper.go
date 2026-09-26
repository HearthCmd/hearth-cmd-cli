package main

// Quick helpers (@helper in household chat): the daemon half.
//
// The relay spawns a throwaway temp agent and briefs it with a turn whose mid is
// "helper_<instance id>". The helper is supposed to answer with one
// `hearth chat reply`, after which the relay destroys it. This file covers the
// helper that finishes its turn WITHOUT replying (or whose reply never lands):
// once the brief is confirmed delivered, it watches the instance's readiness and
// reports `chat_helper_turn_done` when the turn has really ended — no open
// asks, nothing queued. The relay then posts the helper's last words for it and
// tears it down. The relay ignores the report for a helper that already answered.
//
// No new protocol to opt in: the mid prefix is what marks a helper, so a relay
// that doesn't summon helpers never triggers any of this.

import (
	"encoding/json"
	"log"
	"time"
)

// chatHelperMIDPrefix marks a quick helper's brief. Must match the relay's
// chatHelperBriefMID.
const chatHelperMIDPrefix = "helper_"

// chatHelperWatchLimit bounds the watcher; the relay's own deadline (30 min)
// ends the helper regardless.
const chatHelperWatchLimit = 45 * time.Minute

// chatHelperPoll re-checks availability on a timer too: settling after a turn
// is time-based, and time passing doesn't fire readiness.Changed().
const chatHelperPoll = 2 * time.Second

// isChatHelperBrief reports whether an inbox entry is instanceID's helper brief.
func isChatHelperBrief(e *inboxEntry) bool {
	return e != nil && e.InstanceID != "" && e.Key == chatHelperMIDPrefix+e.InstanceID
}

// onChatHelperBriefResolved starts the turn watcher once a helper's brief has
// actually become a turn. An expired or quarantined brief is left to the relay's
// deadline, which tells the thread the helper ran out of time.
func (d *Daemon) onChatHelperBriefResolved(e *inboxEntry, outcome string) {
	if !isChatHelperBrief(e) {
		return
	}
	switch outcome {
	case inboxOutcomeConfirmed, inboxOutcomeLandedLate, inboxOutcomeUnconfirmable:
		go d.watchChatHelperTurn(e.InstanceID, time.Now())
	}
}

// watchChatHelperTurn reports the helper's turn as done once a turn has ended
// after the brief landed and the agent is idle with an empty inbox.
func (d *Daemon) watchChatHelperTurn(instanceID string, briefAt time.Time) {
	deadline := time.Now().Add(chatHelperWatchLimit)
	for time.Now().Before(deadline) {
		aw := d.daemonWS.lookupAgentWS(instanceID)
		if aw == nil || aw.readiness == nil {
			return // destroyed (it answered) or gone
		}
		changed := aw.readiness.Changed()
		if chatHelperTurnDone(aw, briefAt) {
			d.sendChatHelperTurnDone(instanceID)
			return
		}
		select {
		case <-changed:
		case <-time.After(chatHelperPoll):
		}
	}
}

// chatHelperTurnDone: a turn ended after the brief landed, nothing is pending
// (no open ask, no queued turn), and the harness has settled.
func chatHelperTurnDone(aw *agentWS, briefAt time.Time) bool {
	if !aw.readiness.TurnEndedSince(briefAt) || !aw.readiness.Available() {
		return false
	}
	if aw.inbox != nil {
		if depth, err := aw.inbox.Depth(); err != nil || depth > 0 {
			return false
		}
	}
	return true
}

func (d *Daemon) sendChatHelperTurnDone(instanceID string) {
	b, err := json.Marshal(map[string]interface{}{
		"type":                 "chat_helper_turn_done",
		"ai_agent_instance_id": instanceID,
	})
	if err != nil {
		return
	}
	d.daemonWS.ws.SendText(b)
	log.Printf("daemon: chat helper %s finished its turn", instanceID)
}

// spawnFrameWait is how long a frame for a not-yet-registered instance waits
// for the spawn to wire it up. An instance is announced to the relay before its
// input path exists, so the first frame for a just-spawned agent (a helper's
// brief, a voice turn to a woken agent) could otherwise be dropped.
const spawnFrameWait = 60 * time.Second

// retryWhenLive re-runs handle once instanceID is registered, or drops the
// frame after spawnFrameWait. Runs off the read loop.
func (d *DaemonWS) retryWhenLive(instanceID, what string, handle func()) {
	go func() {
		if d.waitForLiveInstance(instanceID, spawnFrameWait) == nil {
			log.Printf("daemon-ws: %s for %s: instance never came up; dropping", what, instanceID)
			return
		}
		handle()
	}()
}
