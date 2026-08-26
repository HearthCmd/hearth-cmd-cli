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
