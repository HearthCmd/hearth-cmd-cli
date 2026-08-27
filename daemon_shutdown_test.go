package main

import (
	"testing"
	"time"
)

// The whole point of the bounded-shutdown work (#5) is that a graceful daemon stop
// always finishes before systemd's default TimeoutStopSec (90s) turns it into a
// SIGKILL — which would strand in-flight agents without a clean host_disconnected.
// These are static budget guards: if someone bumps a timeout past the cap, the build
// fails here instead of in the field mid-shutdown.
func TestShutdownTimeoutsStayUnderSystemdCap(t *testing.T) {
	const systemdDefaultStopSec = 90 * time.Second

	if shutdownHardCap >= systemdDefaultStopSec {
		t.Fatalf("shutdownHardCap %s must stay under systemd's default %s stop timeout",
			shutdownHardCap, systemdDefaultStopSec)
	}
	// The flush wait is bounded inside the hard-cap watchdog window, so a stalled WS
	// can't itself push shutdown past the cap.
	if agentReportFlushTimeout >= shutdownHardCap {
		t.Fatalf("agentReportFlushTimeout %s must be under shutdownHardCap %s",
			agentReportFlushTimeout, shutdownHardCap)
	}
	// A single agent's stop grace must also fit comfortably inside the cap; with the
	// parallel stop loop the whole wind-down is ~one grace window regardless of N.
	if agentStopGrace >= shutdownHardCap {
		t.Fatalf("agentStopGrace %s must be under shutdownHardCap %s",
			agentStopGrace, shutdownHardCap)
	}
}
