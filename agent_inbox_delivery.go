//go:build darwin || linux

package main

import (
	"encoding/json"
	"log"
	"strings"
	"time"
)

// agent_inbox_delivery.go — the drain loop that turns a queued payload into an
// actual turn in the agent's context. See docs/agent-inbox-spec.md §5.
//
// The contract is confirm-then-dequeue: an entry leaves the queue only when the
// transcript proves it became a real `user` turn. Anything else — the harness
// absorbed it into the running turn, or nothing showed up at all — is a
// redelivery, bounded, and then a quarantine.

const (
	// inboxConfirmTimeout is how long we wait for the transcript to say what
	// became of an injected payload. The write itself is milliseconds; this
	// covers the streamer poll, the bridge poll, and a slow harness flush.
	inboxConfirmTimeout = 20 * time.Second

	// inboxRetryBackoff keeps a failed attempt from re-firing the instant
	// the agent looks available again. Redelivery is cheap but not free —
	// each one risks landing mid-turn a second time.
	inboxRetryBackoff = 5 * time.Second

	// inboxPollInterval bounds how long the loop sleeps while holding work
	// and waiting for availability. The readiness tracker's Changed channel
	// covers explicit transitions; this catches the time-based ones (settle
	// elapsed, quiescence fallback, boot settle) that fire no event.
	inboxPollInterval = 500 * time.Millisecond
)

// inboxDeliverer owns one instance's drain loop.
//
// The three timings are fields rather than bare constants so tests can drive
// the loop without waiting out real seconds; production always gets the
// constants above.
type inboxDeliverer struct {
	instanceID string
	inbox      *agentInbox
	readiness  *agentReadiness
	write      func(payload []byte) error

	confirmTimeout time.Duration
	retryBackoff   time.Duration
	pollInterval   time.Duration

	// onResolved, when set, fires once per entry as it leaves the queue,
	// whatever the outcome. It is how a producer that needs to report what
	// happened to its message — a scheduled Routine reporting its run status
	// back to the relay — learns the answer, given the queue may hold the
	// message long after the producer's own call returned.
	onResolved func(e *inboxEntry, outcome string)

	stop chan struct{}
}

func newInboxDeliverer(instanceID string, inbox *agentInbox, readiness *agentReadiness, write func([]byte) error) *inboxDeliverer {
	return &inboxDeliverer{
		instanceID:     instanceID,
		inbox:          inbox,
		readiness:      readiness,
		write:          write,
		confirmTimeout: inboxConfirmTimeout,
		retryBackoff:   inboxRetryBackoff,
		pollInterval:   inboxPollInterval,
		stop:           make(chan struct{}),
	}
}

// Stop ends the loop. Idempotent-safe via the closed check in run().
func (d *inboxDeliverer) Stop() {
	select {
	case <-d.stop:
	default:
		close(d.stop)
	}
}

// run drains the queue for the life of the instance. Strictly serial: one
// payload in flight at a time, in enqueue order. Two messages typed in quick
// succession have to arrive in the order they were typed, and a parallel drain
// would race both the TUI and the confirmation matcher.
func (d *inboxDeliverer) run() {
	var lastAttempt time.Time

	for {
		select {
		case <-d.stop:
			return
		default:
		}

		entry, err := d.inbox.Peek()
		if err != nil {
			log.Printf("WARN daemon-inbox: peek failed for %s: %v", d.instanceID, err)
			if !d.sleep(5 * time.Second) {
				return
			}
			continue
		}
		if entry == nil {
			// Idle: block until something is enqueued.
			select {
			case <-d.inbox.Notify():
			case <-d.stop:
				return
			}
			continue
		}

		// Reap before gating on availability. An entry that has expired, or
		// that landed after we stopped waiting for it, is finished — making
		// it queue behind a busy agent would leave `hearth status` reporting
		// phantom depth for the whole turn, and in the landed case that turn
		// is the agent answering the very message we're still holding.
		if d.reap(entry) {
			continue
		}

		if wait := d.retryBackoff - time.Since(lastAttempt); entry.Attempts > 0 && wait > 0 {
			if !d.sleep(wait) {
				return
			}
			continue
		}

		if !d.readiness.Available() {
			select {
			case <-d.readiness.Changed():
			case <-time.After(d.pollInterval):
			case <-d.stop:
				return
			}
			continue
		}

		lastAttempt = time.Now()
		d.attempt(entry)
	}
}

// sleep waits for d, returning false if the loop was stopped meanwhile.
func (d *inboxDeliverer) sleep(dur time.Duration) bool {
	select {
	case <-time.After(dur):
		return true
	case <-d.stop:
		return false
	}
}

// resolve takes an entry off the queue and tells any interested producer how it
// ended. Every terminal transition goes through here so a producer cannot miss
// one — an outcome reported on some paths but not others is worse than none.
func (d *inboxDeliverer) resolve(e *inboxEntry, outcome string) {
	if outcome == inboxOutcomeQuarantined {
		d.inbox.Quarantine(e.Key, e.Reason)
	} else {
		d.inbox.Ack(e.Key, outcome)
	}
	if d.onResolved != nil {
		d.onResolved(e, outcome)
	}
}

// reap resolves an entry that needs no further injection, returning true if it
// took the entry off the queue. Runs on every loop pass, independent of whether
// the agent is available.
func (d *inboxDeliverer) reap(e *inboxEntry) bool {
	now := time.Now()

	if e.Expired(now) {
		log.Printf("daemon-inbox: dropping expired %s message %s for %s (queued %s ago)",
			e.Source, e.Key, d.instanceID, now.Sub(e.EnqueuedAt).Round(time.Second))
		d.resolve(e, inboxOutcomeExpired)
		return true
	}

	// A redelivery races a slow transcript write: the previous attempt may
	// have landed after we stopped waiting for it. Catch that before sending
	// the same message to the agent twice.
	if e.Attempts > 0 && e.Probe != "" && d.readiness.AlreadyLanded(e.Probe) {
		log.Printf("daemon-inbox: %s message %s for %s landed after all; not resending",
			e.Source, e.Key, d.instanceID)
		d.resolve(e, inboxOutcomeLandedLate)
		return true
	}
	return false
}

// attempt injects one entry and resolves what happened to it.
func (d *inboxDeliverer) attempt(e *inboxEntry) {
	attempts, err := d.inbox.RecordAttempt(e.Key)
	if err != nil {
		log.Printf("WARN daemon-inbox: attempt bookkeeping failed for %s: %v", e.Key, err)
		return
	}
	final := attempts >= e.MaxAttempts

	// Stamp before the write: only transcript lines from this attempt onward
	// may resolve it (see AwaitProbe).
	since := time.Now()
	if err := d.write(e.Payload); err != nil {
		log.Printf("daemon-inbox: inject failed for %s message %s: %v", e.Source, e.Key, err)
		if final {
			e.Reason = "inject failed: " + err.Error()
			d.resolve(e, inboxOutcomeQuarantined)
		}
		return
	}

	if e.Probe == "" {
		// Nothing unique to watch for. Injected best-effort, same as the
		// pre-inbox behavior; don't hold a slot we can never resolve.
		d.resolve(e, inboxOutcomeUnconfirmable)
		return
	}

	outcome, ok := d.readiness.AwaitProbe(e.Probe, since, d.confirmTimeout)
	switch {
	case ok && outcome == probeLanded:
		d.resolve(e, inboxOutcomeConfirmed)

	case ok && outcome == probeSwallowed:
		// The harness took the bytes and filed them against the turn it was
		// already running instead of making them a turn. This is the exact
		// failure the inbox exists for; the next availability edge retries.
		if final {
			e.Reason = "harness absorbed it into a running turn on every attempt"
			d.resolve(e, inboxOutcomeQuarantined)
			return
		}
		log.Printf("daemon-inbox: %s message %s swallowed mid-turn by %s; will retry (attempt %d/%d)",
			e.Source, e.Key, d.instanceID, attempts, e.MaxAttempts)

	default:
		if final {
			e.Reason = "no transcript confirmation after max attempts"
			d.resolve(e, inboxOutcomeQuarantined)
			return
		}
		log.Printf("daemon-inbox: %s message %s unconfirmed for %s; will retry (attempt %d/%d)",
			e.Source, e.Key, d.instanceID, attempts, e.MaxAttempts)
	}
}

// inboxKeyAndProbe derives the idempotency key and the confirmation probe for a
// payload.
//
// Every hearth-injected turn rides a `hearth/1` or `hearth/2` envelope whose
// header carries a unique `mid`. That mid is echoed verbatim into the harness's
// transcript entry, which makes it both a natural idempotency key and an exact
// confirmation probe — far better than the fuzzy content match the spec
// originally assumed we'd need.
//
// Payloads without an envelope fall back to a UUID key and a normalized
// text-prefix probe. The harness echoes injected text verbatim, so the prefix
// matches; it is only weaker in that two identical messages are
// indistinguishable.
func inboxKeyAndProbe(payload []byte) (key, probe string) {
	if mid := hearthEnvelopeMID(payload); mid != "" {
		return mid, mid
	}
	text := normalizeProbeText(string(payload))
	if len(text) > 120 {
		text = text[:120]
	}
	return generateUUID(), text
}

// hearthEnvelopeMID pulls the `mid` out of a hearth/N envelope header, or ""
// when the payload isn't one. The header is the first line; the body follows a
// blank line (see buildHearthMsgWire on the relay side).
func hearthEnvelopeMID(payload []byte) string {
	s := string(payload)
	if !strings.HasPrefix(s, "hearth/") {
		return ""
	}
	line, _, found := strings.Cut(s, "\n")
	if !found {
		return ""
	}
	_, header, found := strings.Cut(line, " ")
	if !found {
		return ""
	}
	var env struct {
		MID string `json:"mid"`
	}
	if json.Unmarshal([]byte(header), &env) != nil {
		return ""
	}
	return env.MID
}
