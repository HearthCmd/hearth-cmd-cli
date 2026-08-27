//go:build darwin || linux

package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// agent_readiness.go — per-instance "is the harness accepting injected turns?"
// tracking, plus the confirmation oracle the inbox delivers against.
//
// Both are fed by the same source: the bridge tail (bridge.go), which already
// reads every line of the agent's transcript. See docs/agent-inbox-spec.md §3.
//
// The readiness signal is deliberately NOT the correctness mechanism. A harness
// can start a turn between our decision and our PTY write, so no busy/idle
// signal can be exact. Correctness comes from confirm-then-dequeue in the
// delivery loop; readiness just keeps the redelivery count near zero.

const (
	// settleAfterTurn is how long after an observed end-of-turn we wait
	// before declaring the agent available. The TUI needs a beat to repaint
	// its prompt and re-arm bracketed paste.
	settleAfterTurn = 750 * time.Millisecond

	// bootSettle is how long a freshly registered instance stays
	// unavailable when we've seen no transcript activity at all. Replaces
	// the hardcoded scheduledSpawnSettle in daemon_scheduled_trigger.go.
	bootSettle = 4 * time.Second

	// quietPeriod is the fallback end-of-turn signal for harnesses whose
	// explicit marker we don't recognize: this much transcript silence
	// counts as idle. Claude never needs it (turn_duration is exact); it
	// exists so an un-ported harness degrades to "occasionally redelivers"
	// rather than "never delivers".
	quietPeriod = 8 * time.Second

	// probeMemory bounds how long a landed/swallowed observation stays
	// matchable, so a waiter registered slightly after the transcript line
	// arrives still resolves.
	probeMemory = 5 * time.Minute
)

// holdReason distinguishes the two flavors of deliberate unavailability. Only
// holdNone is reachable today; holdBreak is the shape the dreaming/compaction
// window will set (docs/agent-inbox-spec.md §7). It is declared now because
// retrofitting it into a shipped state machine is worse than writing it in.
type holdReason int

const (
	holdNone holdReason = iota
	// holdBreak: the agent is deliberately unavailable for a long window.
	// The queue accumulates; the agent is NOT woken.
	holdBreak
)

// probeOutcome is what the transcript said happened to an injected payload.
type probeOutcome int

const (
	probeLanded probeOutcome = iota
	probeSwallowed
)

type probeWaiter struct {
	probe string
	out   chan probeOutcome
}

type probeRecord struct {
	text    string
	outcome probeOutcome
	at      time.Time
}

// agentReadiness tracks one instance's turn state and remembers what became of
// recently injected text. One per live AgentInstance.
type agentReadiness struct {
	instanceID string
	agent      string

	mu           sync.Mutex
	registeredAt time.Time
	sawTurnEnd   bool
	busy         bool
	lastLineAt   time.Time
	lastTurnEnd  time.Time
	asks         map[string]struct{}
	hold         holdReason

	// changed is closed and replaced whenever state moves in a way that
	// could make the instance available. Waiters re-read Available().
	changed chan struct{}

	waiters []*probeWaiter
	recent  []probeRecord
}

func newAgentReadiness(instanceID, agent string) *agentReadiness {
	return &agentReadiness{
		instanceID:   instanceID,
		agent:        agent,
		registeredAt: time.Now(),
		changed:      make(chan struct{}),
		asks:         make(map[string]struct{}),
	}
}

// Changed returns a channel closed on the next state transition. Callers
// re-check Available() after it fires — it is an edge hint, not a value.
func (r *agentReadiness) Changed() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

// notifyLocked wakes everyone waiting on Changed. Caller holds r.mu.
func (r *agentReadiness) notifyLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// Available reports whether the harness looks ready to accept an injected turn.
func (r *agentReadiness) Available() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.availableLocked(time.Now())
}

func (r *agentReadiness) availableLocked(now time.Time) bool {
	if r.hold != holdNone {
		return false
	}
	if len(r.asks) > 0 {
		return false
	}
	if r.busy {
		// Quiescence fallback: a harness whose end-of-turn marker we don't
		// recognize would otherwise stay busy forever. Claude never reaches
		// this — turn_duration is exact.
		return now.Sub(r.lastLineAt) >= quietPeriod
	}
	if !r.sawTurnEnd {
		// No turn has completed yet. Either the agent just spawned and its
		// TUI isn't up, or we've only seen bookkeeping lines. Hold for
		// bootSettle rather than trusting the absence of evidence — writing
		// into a harness that hasn't finished starting is how the first
		// message gets eaten (the failure NeedsInjectGate exists for).
		return now.Sub(r.registeredAt) >= bootSettle
	}
	return now.Sub(r.lastTurnEnd) >= settleAfterTurn
}

// SetHold sets or clears deliberate unavailability. Nothing calls this with
// holdBreak yet — see docs/agent-inbox-spec.md §7.
func (r *agentReadiness) SetHold(h holdReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hold == h {
		return
	}
	r.hold = h
	r.notifyLocked()
}

// AskStarted / AskEnded track interpose permission requests in flight. An agent
// blocked on an approval is unambiguously busy, and that is exactly when the old
// unconditional-inject path failed most often.
//
// Tracked as a set rather than a counter because RemovePending is idempotent
// and is called from a defer — a double-remove must not drive the count below
// the truth and declare a blocked agent available.
func (r *agentReadiness) AskStarted(requestID string) {
	r.mu.Lock()
	r.asks[requestID] = struct{}{}
	r.mu.Unlock()
}

func (r *agentReadiness) AskEnded(requestID string) {
	r.mu.Lock()
	if _, ok := r.asks[requestID]; ok {
		delete(r.asks, requestID)
		if len(r.asks) == 0 {
			r.notifyLocked()
		}
	}
	r.mu.Unlock()
}

// ObserveLine feeds one bridge-shape transcript line through the harness
// classifier and folds the result into readiness + the confirmation oracle.
func (r *agentReadiness) ObserveLine(line []byte) {
	obs := observeTranscriptFor(r.agent, line)

	now := time.Now()
	r.mu.Lock()
	r.lastLineAt = now
	wake := false
	if obs.TurnStart && !obs.TurnEnd {
		r.busy = true
	}
	if obs.TurnEnd {
		r.busy = false
		r.sawTurnEnd = true
		r.lastTurnEnd = now
		wake = true
	}
	if obs.Landed != "" {
		r.recordLocked(obs.Landed, probeLanded, now)
	}
	if obs.Swallowed != "" {
		r.recordLocked(obs.Swallowed, probeSwallowed, now)
	}
	if wake {
		r.notifyLocked()
	}
	r.mu.Unlock()
}

// recordLocked resolves any waiter this text satisfies and remembers it for
// probeMemory so a waiter registered a moment later still matches. Caller holds
// r.mu.
func (r *agentReadiness) recordLocked(text string, outcome probeOutcome, now time.Time) {
	norm := normalizeProbeText(text)
	kept := r.waiters[:0]
	for _, w := range r.waiters {
		if strings.Contains(norm, w.probe) {
			select {
			case w.out <- outcome:
			default:
			}
			continue
		}
		kept = append(kept, w)
	}
	r.waiters = kept

	r.recent = append(r.recent, probeRecord{text: norm, outcome: outcome, at: now})
	// Drop anything past probeMemory, and cap the slice so a chatty agent
	// can't grow it without bound.
	cut := 0
	for cut < len(r.recent) && now.Sub(r.recent[cut].at) > probeMemory {
		cut++
	}
	r.recent = r.recent[cut:]
	if len(r.recent) > 256 {
		r.recent = r.recent[len(r.recent)-256:]
	}
}

// AwaitProbe blocks until the transcript says what became of an injected
// payload, or timeout elapses. ok=false means neither happened in time.
//
// `since` bounds which already-seen observations count, and it is load-bearing
// on a redelivery: without it, attempt 2 would instantly resolve against
// attempt 1's swallow record and report a failure it never actually observed,
// burning the whole retry budget in milliseconds and quarantining a message
// that may well have landed. Pass the instant just before the PTY write.
func (r *agentReadiness) AwaitProbe(probe string, since time.Time, timeout time.Duration) (probeOutcome, bool) {
	probe = normalizeProbeText(probe)
	if probe == "" {
		return probeLanded, false
	}

	w := &probeWaiter{probe: probe, out: make(chan probeOutcome, 1)}

	r.mu.Lock()
	// A line matching this probe may already have gone by — the inject and
	// the transcript write race, and the transcript can win.
	for i := len(r.recent) - 1; i >= 0; i-- {
		if r.recent[i].at.Before(since) {
			break // older than this attempt; everything before is older still
		}
		if strings.Contains(r.recent[i].text, probe) {
			out := r.recent[i].outcome
			r.mu.Unlock()
			return out, true
		}
	}
	r.waiters = append(r.waiters, w)
	r.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case out := <-w.out:
		return out, true
	case <-timer.C:
		r.mu.Lock()
		kept := r.waiters[:0]
		for _, existing := range r.waiters {
			if existing != w {
				kept = append(kept, existing)
			}
		}
		r.waiters = kept
		r.mu.Unlock()
		// The answer may have arrived in the same instant the timer fired;
		// select picks arbitrarily when both are ready. Take it if it's
		// there rather than reporting a false timeout and redelivering.
		select {
		case out := <-w.out:
			return out, true
		default:
		}
		return probeLanded, false
	}
}

// AlreadyLanded reports whether this probe has been seen landing. Checked
// before a redelivery so a slow transcript write doesn't cause a double-send.
func (r *agentReadiness) AlreadyLanded(probe string) bool {
	probe = normalizeProbeText(probe)
	if probe == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.recent {
		if rec.outcome == probeLanded && strings.Contains(rec.text, probe) {
			return true
		}
	}
	return false
}

// normalizeProbeText collapses whitespace so a probe taken from the payload
// matches the same text after the harness has rewrapped it in its transcript.
func normalizeProbeText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// observeTranscriptFor routes a line to the harness's classifier, falling back
// to the shared base for an unregistered agent name.
func observeTranscriptFor(agent string, line []byte) TranscriptObservation {
	if h, ok := getHarness(agent); ok {
		return h.ObserveTranscript(line)
	}
	if h, ok := getHarnessByServerName(agent); ok {
		return h.ObserveTranscript(line)
	}
	return baseObserveTranscript(line)
}

// bridgeEntry is the subset of a bridge-shape transcript line the readiness
// classifier reads. Bridge output is claude-shape for every harness (that's the
// StreamTransformer contract), so one struct covers them all.
type bridgeEntry struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	Message    *bridgeMessage  `json:"message"`
	Attachment *bridgeAttached `json:"attachment"`
}

type bridgeMessage struct {
	Role       string          `json:"role"`
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
}

type bridgeAttached struct {
	Type   string `json:"type"`
	Prompt string `json:"prompt"`
}

// baseObserveTranscript is the harness-independent classifier. Claude extends
// it with one extra end-of-turn marker; the other four use it verbatim.
//
// The rules, and why each is what it is:
//
//   - a `user` entry whose content is NOT a tool_result is a real user turn:
//     it starts a turn AND is the proof an injected payload landed.
//   - a `user` entry that IS a tool_result is mid-turn continuation, not a new
//     turn, and must not reset busy.
//   - any `assistant` entry means a turn is running; stop_reason "end_turn"
//     means it just finished.
//   - an `attachment` of type `queued_command` is the harness telling us it
//     absorbed injected text into the running turn instead of making it a turn.
//     That is the swallow this whole mechanism exists to detect — see
//     docs/agent-inbox-spec.md §1.
func baseObserveTranscript(line []byte) TranscriptObservation {
	var e bridgeEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return TranscriptObservation{}
	}

	switch e.Type {
	case "user":
		if e.Message == nil {
			return TranscriptObservation{}
		}
		if isToolResultContent(e.Message.Content) {
			return TranscriptObservation{}
		}
		text := flattenContent(e.Message.Content)
		return TranscriptObservation{TurnStart: true, Landed: text}

	case "assistant":
		obs := TranscriptObservation{TurnStart: true}
		if e.Message != nil && e.Message.StopReason == "end_turn" {
			obs.TurnEnd = true
		}
		return obs

	case "attachment":
		if e.Attachment != nil && e.Attachment.Type == "queued_command" {
			return TranscriptObservation{Swallowed: e.Attachment.Prompt}
		}
	}
	return TranscriptObservation{}
}

// isToolResultContent reports whether a message's content array is a tool
// result rather than something a person (or the daemon) sent.
func isToolResultContent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}
