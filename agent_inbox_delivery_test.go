//go:build darwin || linux

package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Coverage for agent_inbox_delivery.go — the drain loop.
//
// The property under test throughout is confirm-then-dequeue: a message leaves
// the queue only when the transcript proves it became a real turn. Everything
// else here (readiness gating, backoff, quarantine) exists to make that
// affordable, not to make it true.

// recordingWriter stands in for the PTY. Each write is captured, and the test
// decides what the "harness" then does with it by feeding lines to readiness.
type recordingWriter struct {
	mu      sync.Mutex
	writes  [][]byte
	err     error
	written chan []byte
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{written: make(chan []byte, 64)}
}

func (w *recordingWriter) write(payload []byte) error {
	w.mu.Lock()
	w.writes = append(w.writes, payload)
	err := w.err
	w.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case w.written <- payload:
	default:
	}
	return nil
}

func (w *recordingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.writes)
}

func (w *recordingWriter) setErr(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
}

// awaitWrite blocks for the next PTY write, failing the test if none arrives.
func (w *recordingWriter) awaitWrite(t *testing.T, why string) []byte {
	t.Helper()
	select {
	case p := <-w.written:
		return p
	case <-time.After(3 * time.Second):
		t.Fatalf("expected a PTY write (%s), got none", why)
		return nil
	}
}

// deliveryHarness wires an inbox, a readiness tracker, and a fake PTY together
// with test-speed timings.
type deliveryHarness struct {
	inbox     *agentInbox
	readiness *agentReadiness
	writer    *recordingWriter
	deliverer *inboxDeliverer
}

func newDeliveryHarness(t *testing.T) *deliveryHarness {
	t.Helper()
	db := openTestDaemonDB(t)
	q := newAgentInbox(db, "inst-1")
	r := newAgentReadiness("inst-1", "claude")
	w := newRecordingWriter()

	d := newInboxDeliverer("inst-1", q, r, w.write)
	d.confirmTimeout = 250 * time.Millisecond
	d.retryBackoff = 20 * time.Millisecond
	d.pollInterval = 10 * time.Millisecond

	go d.run()
	t.Cleanup(d.Stop)
	return &deliveryHarness{inbox: q, readiness: r, writer: w, deliverer: d}
}

// idle puts the readiness tracker into a state the loop will deliver from.
func (h *deliveryHarness) idle() {
	h.readiness.ObserveLine([]byte(lineTurnDuration))
	h.readiness.mu.Lock()
	h.readiness.lastTurnEnd = time.Now().Add(-2 * settleAfterTurn)
	h.readiness.mu.Unlock()
}

// busy puts it mid-turn.
func (h *deliveryHarness) busy() {
	h.readiness.ObserveLine([]byte(lineAssistantToolUse))
}

// landed / swallowed feed back what the harness "did" with an injected mid.
func (h *deliveryHarness) landed(mid string) {
	h.readiness.ObserveLine([]byte(fmt.Sprintf(
		`{"type":"user","message":{"role":"user","content":"hearth/2 {\"mid\":\"%s\"}\n\nHi"}}`, mid)))
}

func (h *deliveryHarness) swallowed(mid string) {
	h.readiness.ObserveLine([]byte(fmt.Sprintf(
		`{"type":"attachment","attachment":{"type":"queued_command","prompt":"hearth/2 {\"mid\":\"%s\"}\n\nHi"}}`, mid)))
}

func envelope(mid string) []byte {
	return []byte(fmt.Sprintf("hearth/2 {\"from\":{\"kind\":\"device\"},\"mid\":\"%s\"}\n\nHi", mid))
}

func (h *deliveryHarness) depth(t *testing.T) int {
	t.Helper()
	n, err := h.inbox.Depth()
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	return n
}

// waitDepth polls until the queue reaches want, or fails.
func (h *deliveryHarness) waitDepth(t *testing.T, want int, why string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.depth(t) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("queue depth never reached %d (%s); still %d", want, why, h.depth(t))
}

// The core happy path.
func TestDelivery_HoldsWhileBusyThenDeliversWhenIdle(t *testing.T) {
	h := newDeliveryHarness(t)
	h.busy()

	if err := h.inbox.Enqueue("m_1", envelope("m_1"), "m_1", "relay_input", inboxTTLChat); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// This is the whole point: a message typed at an agent mid-turn must NOT
	// be written to the PTY, because the harness would absorb it and it would
	// never become a turn.
	time.Sleep(150 * time.Millisecond)
	if n := h.writer.count(); n != 0 {
		t.Fatalf("wrote %d time(s) while the agent was mid-turn; must hold", n)
	}
	if h.depth(t) != 1 {
		t.Fatal("message should still be queued")
	}

	h.idle()
	h.writer.awaitWrite(t, "agent went idle")
	h.landed("m_1")
	h.waitDepth(t, 0, "confirmed delivery should dequeue")
}

// Confirm-then-dequeue: a write alone is not delivery.
func TestDelivery_DoesNotDequeueOnWriteAlone(t *testing.T) {
	h := newDeliveryHarness(t)
	h.idle()
	h.inbox.Enqueue("m_1", envelope("m_1"), "m_1", "relay_input", inboxTTLChat)

	h.writer.awaitWrite(t, "idle agent")
	// Say nothing back. The entry must survive the confirm window.
	time.Sleep(100 * time.Millisecond)
	if h.depth(t) != 1 {
		t.Fatal("an unconfirmed write must not dequeue the message — a PTY write is not proof it became a turn")
	}
}

// The failure the inbox exists for: the harness swallows it, and we try again.
func TestDelivery_SwallowedMessageIsRedelivered(t *testing.T) {
	h := newDeliveryHarness(t)
	h.idle()
	h.inbox.Enqueue("m_1", envelope("m_1"), "m_1", "relay_input", inboxTTLChat)

	h.writer.awaitWrite(t, "first attempt")
	h.swallowed("m_1")

	// Still queued: a swallow is not a delivery.
	time.Sleep(50 * time.Millisecond)
	if h.depth(t) != 1 {
		t.Fatal("a swallowed message must stay queued for redelivery")
	}

	h.idle()
	h.writer.awaitWrite(t, "redelivery after the agent freed up")
	h.landed("m_1")
	h.waitDepth(t, 0, "the redelivery landed")
	if n := h.writer.count(); n < 2 {
		t.Fatalf("expected at least 2 writes (original + redelivery), got %d", n)
	}
}

// Bounded retries, then quarantine — never an infinite loop, never a silent drop.
func TestDelivery_QuarantinesAfterMaxAttempts(t *testing.T) {
	h := newDeliveryHarness(t)
	h.idle()
	h.inbox.Enqueue("m_poison", envelope("m_poison"), "m_poison", "relay_input", inboxTTLChat)

	// Never confirm. Each attempt times out; keep the agent looking idle so
	// the loop retries promptly.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.idle()
		if h.depth(t) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if h.depth(t) != 0 {
		t.Fatalf("entry never left the pending queue; a poison message must stop retrying and quarantine")
	}
	entries, err := ListAgentInbox(h.inbox.db, "inst-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].State != "quarantined" {
		t.Fatalf("expected the entry quarantined (kept, visible), got %+v", entries)
	}
	if entries[0].Attempts != inboxDefaultMaxAttempts {
		t.Fatalf("attempts = %d, want exactly %d — retries must be bounded",
			entries[0].Attempts, inboxDefaultMaxAttempts)
	}
}

// A quarantined message must not block the healthy one behind it.
func TestDelivery_PoisonEntryDoesNotBlockTheQueue(t *testing.T) {
	h := newDeliveryHarness(t)
	h.inbox.Enqueue("m_poison", envelope("m_poison"), "m_poison", "relay_input", inboxTTLChat)
	h.inbox.Enqueue("m_good", envelope("m_good"), "m_good", "relay_input", inboxTTLChat)

	delivered := make(chan struct{})
	go func() {
		// Keep the agent idle and confirm only the good message.
		for {
			select {
			case <-delivered:
				return
			default:
			}
			// Order matters: `landed` records a user turn, which marks the
			// agent BUSY. Ending each cycle with idle() leaves the steady
			// state available, so the loop actually gets to run.
			h.landed("m_good")
			h.idle()
			time.Sleep(20 * time.Millisecond)
		}
	}()

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if h.depth(t) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(delivered)

	if h.depth(t) != 0 {
		t.Fatal("the good message never drained — one undeliverable entry wedged the queue")
	}
	entries, _ := ListAgentInbox(h.inbox.db, "inst-1")
	if len(entries) != 1 || entries[0].Key != "m_poison" || entries[0].State != "quarantined" {
		t.Fatalf("expected only the poison entry left, quarantined; got %+v", entries)
	}
}

// A stale message is dropped without being injected, and it says so.
func TestDelivery_ExpiredEntryIsDroppedWithoutInjecting(t *testing.T) {
	h := newDeliveryHarness(t)
	h.idle()
	h.inbox.Enqueue("m_stale", envelope("m_stale"), "m_stale", "system_event", -time.Hour)

	h.waitDepth(t, 0, "an expired entry should be dropped promptly")
	if n := h.writer.count(); n != 0 {
		t.Fatalf("wrote %d time(s); an expired message must never reach the agent", n)
	}
}

func TestDelivery_FIFOAcrossMultipleMessages(t *testing.T) {
	h := newDeliveryHarness(t)
	h.idle()
	for _, mid := range []string{"m_1", "m_2", "m_3"} {
		h.inbox.Enqueue(mid, envelope(mid), mid, "relay_input", inboxTTLChat)
	}

	for _, want := range []string{"m_1", "m_2", "m_3"} {
		h.idle()
		got := h.writer.awaitWrite(t, "next message in order")
		if mid := hearthEnvelopeMID(got); mid != want {
			t.Fatalf("delivered %q, want %q — messages must arrive in the order they were sent", mid, want)
		}
		h.landed(want)
	}
	h.waitDepth(t, 0, "all three confirmed")
}

// If the PTY write itself fails, we don't pretend it was delivered.
func TestDelivery_WriteErrorRetriesThenQuarantines(t *testing.T) {
	h := newDeliveryHarness(t)
	h.writer.setErr(fmt.Errorf("pty closed"))
	h.idle()
	h.inbox.Enqueue("m_1", envelope("m_1"), "m_1", "relay_input", inboxTTLChat)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.idle()
		if h.depth(t) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h.depth(t) != 0 {
		t.Fatal("a failing write must stop retrying eventually")
	}
	entries, _ := ListAgentInbox(h.inbox.db, "inst-1")
	if len(entries) != 1 || entries[0].State != "quarantined" {
		t.Fatalf("expected quarantine after repeated write failure, got %+v", entries)
	}
}

// The confirm window and a slow transcript can race. Redelivering a message
// that actually landed would double-post it to the agent.
func TestDelivery_DoesNotResendSomethingThatLandedLate(t *testing.T) {
	h := newDeliveryHarness(t)
	h.idle()
	h.inbox.Enqueue("m_1", envelope("m_1"), "m_1", "relay_input", inboxTTLChat)

	h.writer.awaitWrite(t, "first attempt")

	// Keep the agent looking busy so the loop can't retry while we stage the
	// race, then let the confirm window lapse and have the transcript catch
	// up afterwards.
	h.busy()
	time.Sleep(400 * time.Millisecond)
	h.landed("m_1")

	// Deliberately do NOT go idle: a message that landed must be reaped even
	// while the agent is busy — which it now is, answering that very message.
	h.waitDepth(t, 0, "a late landing should dequeue without waiting for the agent")
	if n := h.writer.count(); n != 1 {
		t.Fatalf("wrote %d times; a message that landed late must not be sent again", n)
	}
}

func TestDelivery_StopIsIdempotent(t *testing.T) {
	h := newDeliveryHarness(t)
	h.deliverer.Stop()
	h.deliverer.Stop() // must not panic on a double close
}

// resolvedOutcomes captures the producer-facing hook the scheduled-trigger path
// depends on to close out a run.
func (h *deliveryHarness) captureResolutions() *[]string {
	var mu sync.Mutex
	got := []string{}
	h.deliverer.onResolved = func(e *inboxEntry, outcome string) {
		mu.Lock()
		got = append(got, e.Key+":"+outcome)
		mu.Unlock()
	}
	return &got
}

// Every terminal transition must report. A producer that gets told about some
// outcomes and not others is worse off than one that polls, because it can't
// tell "still waiting" from "silently finished".
func TestDelivery_ResolveHookFiresOnConfirmation(t *testing.T) {
	h := newDeliveryHarness(t)
	got := h.captureResolutions()
	h.idle()
	h.inbox.Enqueue("m_1", envelope("m_1"), "m_1", "relay_input", inboxTTLChat)

	h.writer.awaitWrite(t, "first attempt")
	h.landed("m_1")
	h.waitDepth(t, 0, "confirmed")

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(*got) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(*got) != 1 || (*got)[0] != "m_1:"+inboxOutcomeConfirmed {
		t.Fatalf("resolutions = %v, want one confirmed", *got)
	}
}

func TestDelivery_ResolveHookFiresOnExpiry(t *testing.T) {
	h := newDeliveryHarness(t)
	got := h.captureResolutions()
	h.idle()
	h.inbox.Enqueue("m_stale", envelope("m_stale"), "m_stale", "scheduled_trigger:run-1:spawned_temp", -time.Hour)

	h.waitDepth(t, 0, "expired")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(*got) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(*got) != 1 || (*got)[0] != "m_stale:"+inboxOutcomeExpired {
		t.Fatalf("resolutions = %v, want one expired — a Routine whose kickoff timed out must still resolve its run", *got)
	}
}

func TestDelivery_ResolveHookFiresOnQuarantine(t *testing.T) {
	h := newDeliveryHarness(t)
	got := h.captureResolutions()
	h.writer.setErr(fmt.Errorf("pty closed"))
	h.idle()
	h.inbox.Enqueue("m_1", envelope("m_1"), "m_1", "relay_input", inboxTTLChat)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.idle()
		if len(*got) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(*got) != 1 || (*got)[0] != "m_1:"+inboxOutcomeQuarantined {
		t.Fatalf("resolutions = %v, want one quarantined", *got)
	}
	entries, _ := ListAgentInbox(h.inbox.db, "inst-1")
	if len(entries) != 1 || entries[0].State != "quarantined" || entries[0].Reason == "" {
		t.Fatalf("quarantine should still be recorded with a reason, got %+v", entries)
	}
}
