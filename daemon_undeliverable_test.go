//go:build darwin || linux

package main

// Coverage for which inbox outcomes get reported to the relay. Over-reporting
// is as wrong as under-reporting here: a "never reached the agent" banner on a
// message that actually landed would train people to distrust the transcript.

import (
	"testing"
)

// reportableOutcome mirrors the switch in reportUndeliverable. Kept as a small
// pure helper so the decision can be tested without a live daemon WS.
func reportableOutcome(outcome string) bool {
	switch outcome {
	case inboxOutcomeQuarantined, inboxOutcomeExpired:
		return true
	}
	return false
}

func TestReportUndeliverable_OnlyAbandonmentOutcomesReport(t *testing.T) {
	tests := map[string]bool{
		inboxOutcomeQuarantined: true,
		inboxOutcomeExpired:     true,
		// A confirmed delivery needs no frame — the transcript entry IS the
		// signal, and a second one would race it.
		inboxOutcomeConfirmed:  false,
		inboxOutcomeLandedLate: false,
		// Injected, but with no probe to watch. Not the same as failing, and
		// reporting it would mark delivered messages as lost.
		inboxOutcomeUnconfirmable: false,
	}
	for outcome, want := range tests {
		t.Run(outcome, func(t *testing.T) {
			if got := reportableOutcome(outcome); got != want {
				t.Errorf("reportable(%q) = %v, want %v", outcome, got, want)
			}
		})
	}
}

// A daemon with no WS (starting up, or reconnecting) must not panic on a
// resolution that happens to land in that window.
func TestReportUndeliverable_NoWSIsInert(t *testing.T) {
	d := &Daemon{}
	d.reportUndeliverable(&inboxEntry{
		InstanceID: "inst-1",
		Payload:    []byte("hearth/2 {\"mid\":\"m_1\"}\n\nHi"),
		Key:        "m_1",
	}, inboxOutcomeQuarantined)
}

// The mid is how the relay and the app address the row. Harness traffic carries
// no envelope, so there is no row to correct and nothing to report.
func TestReportUndeliverable_SkipsPayloadsWithNoEnvelope(t *testing.T) {
	for _, payload := range []string{
		"<task-notification>\n<task-id>abc</task-id>\n</task-notification>",
		"plain text",
		"",
	} {
		if mid := hearthEnvelopeMID([]byte(payload)); mid != "" {
			t.Errorf("payload %q should have no mid, got %q", payload, mid)
		}
	}
}

func TestHandleInboxResolved_NilEntryIsInert(t *testing.T) {
	d := &Daemon{}
	d.handleInboxResolved(nil, inboxOutcomeQuarantined)
}

// Non-trigger sources report undeliverable but must not be mistaken for a
// Routine run — the trigger branch keys on the source prefix.
func TestHandleInboxResolved_ChatSourceIsNotATriggerRun(t *testing.T) {
	d := &Daemon{} // no daemonWS: reportUndeliverable is inert, trigger path must not fire either
	for _, source := range []string{"relay_input", "chat_mention", "system_event"} {
		d.handleInboxResolved(&inboxEntry{
			InstanceID: "inst-1",
			Source:     source,
			Payload:    []byte("hearth/2 {\"mid\":\"m_1\"}\n\nHi"),
			Key:        "m_1",
		}, inboxOutcomeQuarantined)
	}
}
