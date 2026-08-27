//go:build darwin || linux

package main

import (
	"testing"
	"time"
)

// Coverage for agent_inbox.go — the persisted queue. The properties that matter
// are the ones the spec's corruption pass calls out: never silently drop, never
// silently wedge, and never double-deliver across a restart.

func newTestInbox(t *testing.T) (*agentInbox, *DaemonDB) {
	t.Helper()
	db := openTestDaemonDB(t)
	return newAgentInbox(db, "inst-1"), db
}

func TestInbox_EnqueuePeekAckRoundTrip(t *testing.T) {
	q, _ := newTestInbox(t)

	if e, err := q.Peek(); err != nil || e != nil {
		t.Fatalf("empty inbox should peek nil, got (%v, %v)", e, err)
	}

	if err := q.Enqueue("m_1", []byte("hello"), "m_1", "relay_input", inboxTTLChat); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	e, err := q.Peek()
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if e == nil || e.Key != "m_1" || string(e.Payload) != "hello" || e.Source != "relay_input" {
		t.Fatalf("peeked wrong entry: %+v", e)
	}
	if e.Attempts != 0 || e.MaxAttempts != inboxDefaultMaxAttempts {
		t.Fatalf("attempt counters wrong: %d/%d", e.Attempts, e.MaxAttempts)
	}

	if err := q.Ack("m_1", "confirmed"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if e, _ := q.Peek(); e != nil {
		t.Fatalf("acked entry should be gone, got %+v", e)
	}
}

func TestInbox_FIFOOrder(t *testing.T) {
	q, _ := newTestInbox(t)
	for _, k := range []string{"m_1", "m_2", "m_3"} {
		if err := q.Enqueue(k, []byte(k), k, "relay_input", inboxTTLChat); err != nil {
			t.Fatalf("enqueue %s: %v", k, err)
		}
	}
	for _, want := range []string{"m_1", "m_2", "m_3"} {
		e, _ := q.Peek()
		if e == nil || e.Key != want {
			t.Fatalf("out of order: got %v, want %s — two messages typed in a row must arrive in that order", e, want)
		}
		q.Ack(e.Key, "confirmed")
	}
}

// The idempotency key is what makes a relay retry or a daemon reconnect safe.
func TestInbox_EnqueueIsIdempotentOnKey(t *testing.T) {
	q, _ := newTestInbox(t)
	for i := 0; i < 3; i++ {
		if err := q.Enqueue("m_dup", []byte("hello"), "m_dup", "relay_input", inboxTTLChat); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	n, err := q.Depth()
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if n != 1 {
		t.Fatalf("depth = %d, want 1 — re-enqueuing the same mid must not duplicate the message", n)
	}
}

func TestInbox_QuarantineDoesNotBlockTheQueue(t *testing.T) {
	q, _ := newTestInbox(t)
	q.Enqueue("m_poison", []byte("bad"), "m_poison", "relay_input", inboxTTLChat)
	q.Enqueue("m_good", []byte("good"), "m_good", "relay_input", inboxTTLChat)

	if err := q.Quarantine("m_poison", "test"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	e, _ := q.Peek()
	if e == nil || e.Key != "m_good" {
		t.Fatalf("peek = %v, want m_good — one poison entry must never wedge the queue behind it", e)
	}
	n, _ := q.Depth()
	if n != 1 {
		t.Fatalf("pending depth = %d, want 1 (quarantined entries aren't waiting on anything)", n)
	}
}

func TestInbox_QuarantinedEntriesSurviveForInspection(t *testing.T) {
	q, db := newTestInbox(t)
	q.Enqueue("m_poison", []byte("hearth/2 {\"mid\":\"m_poison\"}\n\nimportant thing"), "m_poison", "relay_input", inboxTTLChat)
	q.Quarantine("m_poison", "no transcript confirmation after max attempts")

	entries, err := ListAgentInbox(db, "inst-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want the quarantined one kept for a human to see", len(entries))
	}
	if entries[0].State != "quarantined" || entries[0].Reason == "" {
		t.Fatalf("quarantined entry lost its state or reason: %+v", entries[0])
	}
	if got := previewPayload(entries[0].Payload); got != "important thing" {
		t.Fatalf("preview = %q, want the body after the envelope header", got)
	}
}

func TestInbox_RecordAttemptCounts(t *testing.T) {
	q, _ := newTestInbox(t)
	q.Enqueue("m_1", []byte("x"), "m_1", "relay_input", inboxTTLChat)
	for want := 1; want <= 3; want++ {
		got, err := q.RecordAttempt("m_1")
		if err != nil {
			t.Fatalf("attempt: %v", err)
		}
		if got != want {
			t.Fatalf("attempts = %d, want %d", got, want)
		}
	}
	// An attempt on a key that's already gone must not error or resurrect it.
	q.Ack("m_1", "confirmed")
	if n, err := q.RecordAttempt("m_1"); err != nil || n != 0 {
		t.Fatalf("attempt on acked key = (%d, %v), want (0, nil)", n, err)
	}
}

func TestInbox_TTLExpiry(t *testing.T) {
	q, _ := newTestInbox(t)
	// A TTL already in the past — the "queued before a three-day sleep"
	// case, where delivering is worse than dropping.
	if err := q.Enqueue("m_stale", []byte("x"), "m_stale", "system_event", -time.Hour); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	e, _ := q.Peek()
	if e == nil {
		t.Fatal("expired entries still peek — the drain loop is what drops them, with a log line")
	}
	if !e.Expired(time.Now()) {
		t.Fatal("entry past its TTL must report expired")
	}
	fresh, _ := newTestInbox(t)
	fresh.Enqueue("m_fresh", []byte("x"), "m_fresh", "relay_input", inboxTTLChat)
	f, _ := fresh.Peek()
	if f.Expired(time.Now()) {
		t.Fatal("a just-queued chat message must not be expired")
	}
}

func TestInbox_DepthCapRefusesRatherThanGrowing(t *testing.T) {
	q, _ := newTestInbox(t)
	for i := 0; i < inboxDepthCap; i++ {
		if err := q.Enqueue(generateUUID(), []byte("x"), "", "relay_input", inboxTTLChat); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := q.Enqueue(generateUUID(), []byte("x"), "", "relay_input", inboxTTLChat); err == nil {
		t.Fatal("past the cap, enqueue must fail loudly — a queue nobody drains is a bug to surface")
	}
}

// Persistence across a "restart" is the entire reason the queue is on disk
// rather than in daemon memory.
func TestInbox_SurvivesReopen(t *testing.T) {
	db := openTestDaemonDB(t)
	q1 := newAgentInbox(db, "inst-1")
	q1.Enqueue("m_1", []byte("hello"), "m_1", "relay_input", inboxTTLChat)
	q1.RecordAttempt("m_1")

	q2 := newAgentInbox(db, "inst-1") // same table, fresh handle
	e, err := q2.Peek()
	if err != nil || e == nil {
		t.Fatalf("queued message did not survive: (%v, %v)", e, err)
	}
	if e.Key != "m_1" || e.Attempts != 1 {
		t.Fatalf("entry came back wrong: %+v (attempt count must survive so retries stay bounded)", e)
	}

	// And an acked entry must NOT come back — that's the double-delivery guard.
	q2.Ack("m_1", "confirmed")
	q3 := newAgentInbox(db, "inst-1")
	if e, _ := q3.Peek(); e != nil {
		t.Fatalf("acked entry reappeared after reopen: %+v", e)
	}
}

func TestInbox_ScopedPerInstance(t *testing.T) {
	db := openTestDaemonDB(t)
	a := newAgentInbox(db, "inst-a")
	b := newAgentInbox(db, "inst-b")
	a.Enqueue("m_a", []byte("for a"), "m_a", "relay_input", inboxTTLChat)
	b.Enqueue("m_b", []byte("for b"), "m_b", "relay_input", inboxTTLChat)

	ea, _ := a.Peek()
	eb, _ := b.Peek()
	if ea == nil || eb == nil || ea.Key != "m_a" || eb.Key != "m_b" {
		t.Fatalf("inboxes crossed: a=%v b=%v", ea, eb)
	}

	// Retiring one agent must not touch another's queue.
	if err := a.DropInstance(); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if e, _ := a.Peek(); e != nil {
		t.Fatalf("retired agent's queue should be gone, got %+v", e)
	}
	if e, _ := b.Peek(); e == nil {
		t.Fatal("dropping one instance's inbox must not affect another's")
	}
}

func TestInbox_CountsForStatus(t *testing.T) {
	q, db := newTestInbox(t)
	if p, qn := CountAgentInbox(db, "inst-1"); p != 0 || qn != 0 {
		t.Fatalf("empty counts = (%d, %d), want (0, 0) — a healthy host prints nothing", p, qn)
	}
	q.Enqueue("m_1", []byte("x"), "m_1", "relay_input", inboxTTLChat)
	q.Enqueue("m_2", []byte("x"), "m_2", "relay_input", inboxTTLChat)
	q.Quarantine("m_2", "test")
	if p, qn := CountAgentInbox(db, "inst-1"); p != 1 || qn != 1 {
		t.Fatalf("counts = (%d, %d), want (1 pending, 1 quarantined)", p, qn)
	}
}

// A nil inbox is the no-local-DB degradation path. It must be inert, not panic:
// the daemon logs the condition at boot and falls back to direct writes.
func TestInbox_NilIsInert(t *testing.T) {
	var q *agentInbox
	if e, err := q.Peek(); e != nil || err != nil {
		t.Fatalf("nil peek = (%v, %v)", e, err)
	}
	if err := q.Ack("k", "x"); err != nil {
		t.Fatalf("nil ack: %v", err)
	}
	if err := q.Quarantine("k", "x"); err != nil {
		t.Fatalf("nil quarantine: %v", err)
	}
	if err := q.DropInstance(); err != nil {
		t.Fatalf("nil drop: %v", err)
	}
	if n, err := q.Depth(); n != 0 || err != nil {
		t.Fatalf("nil depth = (%d, %v)", n, err)
	}
	if err := q.Enqueue("k", []byte("x"), "", "s", time.Hour); err == nil {
		t.Fatal("enqueue into a nil inbox must report failure, not silently swallow the message")
	}
}
