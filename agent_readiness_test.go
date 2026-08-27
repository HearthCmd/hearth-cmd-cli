//go:build darwin || linux

package main

import (
	"testing"
	"time"
)

// Coverage for agent_readiness.go — the turn tracker and the confirmation
// oracle. The transcript fragments here are trimmed from a real claude session
// captured off the dev host on 2026-08-27 (the one that established what
// actually happens to a mid-turn injection); keeping them verbatim in shape is
// the point, because the whole mechanism keys off exact field names.

const (
	// A hearth/2 device envelope arriving while the agent was idle. It became
	// a real user turn.
	lineUserTurn = `{"type":"user","message":{"role":"user","content":"hearth/2 {\"from\":{\"kind\":\"device\",\"id\":\"dev1\"},\"mid\":\"m_landed\"}\n\nHi"},"timestamp":"2026-08-27T14:18:15.943Z"}`

	// A tool result. Mid-turn continuation, NOT a new turn.
	lineToolResult = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"ok"}]},"timestamp":"2026-08-27T14:17:48.900Z"}`

	lineAssistantToolUse = `{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"text","text":"working"}]}}`
	lineAssistantEndTurn = `{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`
	lineTurnDuration     = `{"type":"system","subtype":"turn_duration","timestamp":"2026-08-27T14:17:50.678Z"}`

	// The swallow: claude absorbed an injected payload into the running turn
	// and filed it as an attachment instead of making it a turn.
	lineQueuedCommand = `{"type":"attachment","attachment":{"type":"queued_command","prompt":"hearth/2 {\"from\":{\"kind\":\"device\",\"id\":\"dev1\"},\"mid\":\"m_swallowed\"}\n\nHi","commandMode":"prompt"},"timestamp":"2026-08-27T14:17:48.902Z"}`
)

func TestBaseObserveTranscript_Classification(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantStart bool
		wantEnd   bool
		landed    bool
		swallowed bool
	}{
		{"real user turn", lineUserTurn, true, false, true, false},
		{"tool result is not a turn", lineToolResult, false, false, false, false},
		{"assistant tool_use", lineAssistantToolUse, true, false, false, false},
		{"assistant end_turn", lineAssistantEndTurn, true, true, false, false},
		{"queued_command is a swallow", lineQueuedCommand, false, false, false, true},
		{"garbage is inert", `not json`, false, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := baseObserveTranscript([]byte(tc.line))
			if got.TurnStart != tc.wantStart || got.TurnEnd != tc.wantEnd {
				t.Errorf("turn flags = (%v,%v), want (%v,%v)", got.TurnStart, got.TurnEnd, tc.wantStart, tc.wantEnd)
			}
			if (got.Landed != "") != tc.landed {
				t.Errorf("Landed = %q, want landed=%v", got.Landed, tc.landed)
			}
			if (got.Swallowed != "") != tc.swallowed {
				t.Errorf("Swallowed = %q, want swallowed=%v", got.Swallowed, tc.swallowed)
			}
		})
	}
}

// turn_duration is claude's exact end-of-turn marker and the base classifier
// deliberately doesn't know it — only the claude adapter does. If this ever
// flips, the quiescence fallback silently becomes the primary signal for
// claude, which would add seconds of latency to every delivery.
func TestClaudeObserveTranscript_TurnDurationEndsTurn(t *testing.T) {
	if base := baseObserveTranscript([]byte(lineTurnDuration)); base.TurnEnd {
		t.Fatal("base classifier should not know turn_duration; that's claude-specific")
	}
	got := claudeHarness{}.ObserveTranscript([]byte(lineTurnDuration))
	if !got.TurnEnd {
		t.Fatal("claude adapter must treat turn_duration as end-of-turn")
	}
}

func newTestReadiness(t *testing.T) *agentReadiness {
	t.Helper()
	r := newAgentReadiness("inst-1", "claude")
	// Skip the cold-start window; these tests are about steady state.
	r.mu.Lock()
	r.registeredAt = time.Now().Add(-2 * bootSettle)
	r.mu.Unlock()
	return r
}

func TestReadiness_BusyUntilTurnEnds(t *testing.T) {
	r := newTestReadiness(t)

	r.ObserveLine([]byte(lineUserTurn))
	if r.Available() {
		t.Fatal("a user turn just started; must not be available")
	}
	r.ObserveLine([]byte(lineAssistantToolUse))
	r.ObserveLine([]byte(lineToolResult))
	if r.Available() {
		t.Fatal("a tool result is mid-turn continuation; must stay busy")
	}

	r.ObserveLine([]byte(lineTurnDuration))
	if r.Available() {
		t.Fatal("must wait out settleAfterTurn before declaring available")
	}
	r.mu.Lock()
	r.lastTurnEnd = time.Now().Add(-2 * settleAfterTurn)
	r.mu.Unlock()
	if !r.Available() {
		t.Fatal("turn ended and settled; should be available")
	}
}

func TestReadiness_ColdStartHoldsThenReleases(t *testing.T) {
	r := newAgentReadiness("inst-1", "claude")
	if r.Available() {
		t.Fatal("a just-registered instance must not be available immediately")
	}
	r.mu.Lock()
	r.registeredAt = time.Now().Add(-2 * bootSettle)
	r.mu.Unlock()
	if !r.Available() {
		t.Fatal("after bootSettle with no activity, a fresh agent is idle")
	}
}

// A blocked-on-approval agent is the case the old unconditional-inject path
// failed most often — and the permission_resolved notice telling it the answer
// was itself being swallowed.
func TestReadiness_InFlightAskBlocksDelivery(t *testing.T) {
	r := newTestReadiness(t)
	r.ObserveLine([]byte(lineTurnDuration))
	r.mu.Lock()
	r.lastTurnEnd = time.Now().Add(-2 * settleAfterTurn)
	r.mu.Unlock()
	if !r.Available() {
		t.Fatal("precondition: idle")
	}

	r.AskStarted("req-1")
	if r.Available() {
		t.Fatal("an interpose permission request is in flight; agent is blocked")
	}
	// RemovePending is idempotent and called from a defer, so a double
	// release must not under-count and unblock early.
	r.AskStarted("req-2")
	r.AskEnded("req-1")
	r.AskEnded("req-1")
	if r.Available() {
		t.Fatal("req-2 is still outstanding; a repeated release of req-1 must not unblock")
	}
	r.AskEnded("req-2")
	if !r.Available() {
		t.Fatal("all asks released; should be available")
	}
}

func TestReadiness_QuiescenceFallbackUnwedgesUnknownHarness(t *testing.T) {
	r := newAgentReadiness("inst-1", "some-unported-harness")
	r.ObserveLine([]byte(lineAssistantToolUse)) // busy, no end marker will come
	if r.Available() {
		t.Fatal("precondition: busy")
	}
	r.mu.Lock()
	r.lastLineAt = time.Now().Add(-2 * quietPeriod)
	r.mu.Unlock()
	if !r.Available() {
		t.Fatal("transcript silence past quietPeriod must unwedge a harness with no end marker")
	}
}

func TestReadiness_HoldBlocksEvenWhenIdle(t *testing.T) {
	r := newTestReadiness(t)
	r.ObserveLine([]byte(lineTurnDuration))
	r.mu.Lock()
	r.lastTurnEnd = time.Now().Add(-2 * settleAfterTurn)
	r.mu.Unlock()

	r.SetHold(holdBreak)
	if r.Available() {
		t.Fatal("an agent on a deliberate break is not available, however idle it looks")
	}
	r.SetHold(holdNone)
	if !r.Available() {
		t.Fatal("clearing the hold should restore availability")
	}
}

func TestReadiness_ProbeLandedAndSwallowed(t *testing.T) {
	r := newTestReadiness(t)

	r.ObserveLine([]byte(lineUserTurn))
	if out, ok := r.AwaitProbe("m_landed", time.Time{}, time.Second); !ok || out != probeLanded {
		t.Fatalf("AwaitProbe(m_landed) = (%v, %v), want (probeLanded, true)", out, ok)
	}
	if !r.AlreadyLanded("m_landed") {
		t.Fatal("AlreadyLanded must see a mid that became a user turn")
	}

	r.ObserveLine([]byte(lineQueuedCommand))
	if out, ok := r.AwaitProbe("m_swallowed", time.Time{}, time.Second); !ok || out != probeSwallowed {
		t.Fatalf("AwaitProbe(m_swallowed) = (%v, %v), want (probeSwallowed, true)", out, ok)
	}
	if r.AlreadyLanded("m_swallowed") {
		t.Fatal("a swallowed message must NOT count as landed — that's the whole distinction")
	}
}

// The inject and the transcript write race, and the transcript can win. A
// waiter registered a moment late still has to resolve, or every delivery
// would time out and redeliver.
func TestReadiness_ProbeResolvesWhenWaiterRegistersLate(t *testing.T) {
	r := newTestReadiness(t)
	r.ObserveLine([]byte(lineUserTurn))
	out, ok := r.AwaitProbe("m_landed", time.Time{}, 10*time.Millisecond)
	if !ok || out != probeLanded {
		t.Fatal("a probe registered after the line arrived must still resolve from recent memory")
	}
}

func TestReadiness_ProbeTimesOutWhenNothingHappens(t *testing.T) {
	r := newTestReadiness(t)
	if _, ok := r.AwaitProbe("m_never", time.Time{}, 20*time.Millisecond); ok {
		t.Fatal("a probe nothing matched must report not-resolved, not a false landing")
	}
}

// A waiter blocked on AwaitProbe must be woken by a line that arrives after it
// registered — the ordinary case.
func TestReadiness_ProbeResolvesFromLiveLine(t *testing.T) {
	r := newTestReadiness(t)
	done := make(chan probeOutcome, 1)
	go func() {
		out, ok := r.AwaitProbe("m_landed", time.Time{}, 2*time.Second)
		if ok {
			done <- out
		}
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	r.ObserveLine([]byte(lineUserTurn))
	select {
	case out, ok := <-done:
		if !ok || out != probeLanded {
			t.Fatalf("live line should resolve the waiter as landed, got (%v, %v)", out, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was never woken by the matching transcript line")
	}
}

func TestHearthEnvelopeMID(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"hearth/2 envelope", "hearth/2 {\"from\":{\"kind\":\"device\"},\"mid\":\"m_abc\"}\n\nhello", "m_abc"},
		{"hearth/1 envelope", "hearth/1 {\"from\":{\"id\":\"\"},\"mid\":\"m_def\"}\n\nhello", "m_def"},
		{"no envelope", "just some text", ""},
		{"header only, no body", "hearth/2 {\"mid\":\"m_x\"}", ""},
		{"malformed header", "hearth/2 not-json\n\nbody", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hearthEnvelopeMID([]byte(tc.payload)); got != tc.want {
				t.Errorf("hearthEnvelopeMID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInboxKeyAndProbe(t *testing.T) {
	key, probe := inboxKeyAndProbe([]byte("hearth/2 {\"mid\":\"m_abc\"}\n\nhello"))
	if key != "m_abc" || probe != "m_abc" {
		t.Fatalf("envelope payload should key and probe on its mid, got (%q, %q)", key, probe)
	}

	// No envelope: a minted key, and a text probe the harness will echo back.
	key, probe = inboxKeyAndProbe([]byte("plain   text\nwith   whitespace"))
	if key == "" || key == probe {
		t.Fatalf("non-envelope payload should get a minted key distinct from its probe, got (%q, %q)", key, probe)
	}
	if probe != "plain text with whitespace" {
		t.Fatalf("probe should be whitespace-normalized, got %q", probe)
	}
}

func TestIsSystemEventEnvelope(t *testing.T) {
	sys := []byte("hearth/2 {\"from\":{\"kind\":\"system\"},\"mid\":\"m_1\",\"kind\":\"system_event\",\"event\":\"permission_resolved\"}\n\nBruce approved.")
	if !isSystemEventEnvelope(sys) {
		t.Fatal("a system_event envelope must be recognized so it gets the short TTL")
	}
	human := []byte("hearth/2 {\"from\":{\"kind\":\"device\"},\"mid\":\"m_2\"}\n\nHi")
	if isSystemEventEnvelope(human) {
		t.Fatal("a person's message must not be treated as a system event")
	}
	if isSystemEventEnvelope([]byte("bare text")) {
		t.Fatal("non-envelope payloads are not system events")
	}
}

// The `since` bound is what stops a redelivery from resolving against the
// PREVIOUS attempt's verdict. Without it, attempt 2 reports a swallow it never
// saw, and five attempts burn in milliseconds.
func TestReadiness_ProbeIgnoresObservationsOlderThanTheAttempt(t *testing.T) {
	r := newTestReadiness(t)
	r.ObserveLine([]byte(lineQueuedCommand)) // attempt 1 was swallowed

	cutoff := time.Now()
	time.Sleep(2 * time.Millisecond)

	if _, ok := r.AwaitProbe("m_swallowed", cutoff, 20*time.Millisecond); ok {
		t.Fatal("an observation from before this attempt must not resolve it")
	}
	if _, ok := r.AwaitProbe("m_swallowed", time.Time{}, 20*time.Millisecond); !ok {
		t.Fatal("with no cutoff the same observation should still resolve")
	}
}
