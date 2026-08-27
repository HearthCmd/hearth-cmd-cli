//go:build darwin || linux

package main

import (
	"strings"
	"testing"
)

func TestBuildScheduledTriggerPrompt(t *testing.T) {
	got := string(buildScheduledTriggerPrompt("Morning Sweep", "check the overnight camera events"))

	if !strings.HasPrefix(got, "hearth/1 {\"kind\":\"scheduled_trigger\"}\n\n") {
		t.Errorf("prompt missing hearth/1 scheduled_trigger envelope:\n%q", got)
	}
	if !strings.Contains(got, "[Scheduled Routine: Morning Sweep]") {
		t.Errorf("prompt missing routine name banner:\n%q", got)
	}
	if !strings.Contains(got, "check the overnight camera events") {
		t.Errorf("prompt missing the task body:\n%q", got)
	}
	// The body must come after the envelope's blank line so the harness extracts
	// it the same way it does chat_context.
	if i := strings.Index(got, "\n\n"); i < 0 || !strings.Contains(got[i+2:], "check the overnight") {
		t.Error("task body should follow the envelope blank line")
	}
}

func TestBuildScheduledTriggerPrompt_NoName(t *testing.T) {
	got := string(buildScheduledTriggerPrompt("", "do the thing"))
	if strings.Contains(got, "[Scheduled Routine:") {
		t.Errorf("no name should omit the banner:\n%q", got)
	}
	if !strings.Contains(got, "do the thing") {
		t.Errorf("prompt missing body:\n%q", got)
	}
}

// The pending terminal status rides in the inbox entry's Source rather than an
// in-memory map, precisely so a daemon restart can't strand a Routine run in
// 'queued'. That only works if the round-trip is exact.
func TestTriggerSourceRoundTrip(t *testing.T) {
	const runID = "6f1d3c22-0f4e-4a3a-9b21-1f0e9d7c5a44"

	for _, terminal := range []string{runStatusSpawnedTemp, runStatusWokeExisting, runStatusDeliveredLive} {
		source := triggerSourcePrefix + runID + ":" + terminal
		if !strings.HasPrefix(source, triggerSourcePrefix) {
			t.Fatalf("source %q lost its prefix", source)
		}
		rest := strings.TrimPrefix(source, triggerSourcePrefix)
		gotRun, gotStatus, ok := strings.Cut(rest, ":")
		if !ok || gotRun != runID || gotStatus != terminal {
			t.Fatalf("round-trip of %q gave (%q, %q, %v)", source, gotRun, gotStatus, ok)
		}
	}
}

// Non-trigger traffic shares the same inbox and must not be mistaken for a run.
func TestTriggerSourcePrefixDoesNotMatchOtherProducers(t *testing.T) {
	for _, source := range []string{"relay_input", "chat_mention", "system_event", "agent_approval_request", ""} {
		if strings.HasPrefix(source, triggerSourcePrefix) {
			t.Fatalf("producer %q must not be treated as a Routine kickoff", source)
		}
	}
}

// The daemon's status strings have to equal the relay's, and they live in two
// modules that never link — so nothing but this pins them together.
func TestRunStatusVocabularyMatchesRelay(t *testing.T) {
	for name, got := range map[string]string{
		"queued":         runStatusQueued,
		"delivered_live": runStatusDeliveredLive,
		"woke_existing":  runStatusWokeExisting,
		"spawned_temp":   runStatusSpawnedTemp,
		"failed":         runStatusFailed,
	} {
		if got != name {
			t.Errorf("run status constant = %q, want %q (relay/cmd/hearth-cloud/scheduler.go is the other half)", got, name)
		}
	}
}
