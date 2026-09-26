package main

import (
	"testing"
	"time"
)

func TestIsChatHelperBrief(t *testing.T) {
	if !isChatHelperBrief(&inboxEntry{InstanceID: "i1", Key: "helper_i1"}) {
		t.Fatal("the helper's own brief not recognized")
	}
	for _, e := range []*inboxEntry{
		nil,
		{InstanceID: "i1", Key: "m_abc"},
		{InstanceID: "i1", Key: "helper_i2"}, // another helper's mid
		{InstanceID: "", Key: "helper_"},
	} {
		if isChatHelperBrief(e) {
			t.Fatalf("%+v wrongly taken for a helper brief", e)
		}
	}
}

// A helper's turn is done only after a turn ends AFTER its brief landed, and
// the agent has settled with nothing pending. A turn that ended before the
// brief (a harness warmup) must not count.
func TestChatHelperTurnDone(t *testing.T) {
	r := newTestReadiness(t)
	aw := &agentWS{aiAgentInstanceID: "inst-1", readiness: r}

	r.ObserveLine([]byte(lineTurnDuration)) // warmup turn, before the brief
	r.mu.Lock()
	r.lastTurnEnd = time.Now().Add(-time.Minute)
	r.mu.Unlock()
	briefAt := time.Now().Add(-30 * time.Second)
	if chatHelperTurnDone(aw, briefAt) {
		t.Fatal("a turn that ended before the brief counted as the helper's")
	}

	r.ObserveLine([]byte(lineUserTurn))
	if chatHelperTurnDone(aw, briefAt) {
		t.Fatal("mid-turn counted as done")
	}
	r.ObserveLine([]byte(lineTurnDuration))
	r.AskStarted("req-1")
	r.mu.Lock()
	r.lastTurnEnd = time.Now().Add(-2 * settleAfterTurn)
	r.mu.Unlock()
	if chatHelperTurnDone(aw, briefAt) {
		t.Fatal("an open ask counted as done")
	}
	r.AskEnded("req-1")
	if !chatHelperTurnDone(aw, briefAt) {
		t.Fatal("turn ended after the brief and settled; should be done")
	}
}
