//go:build darwin || linux

package main

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

// agent_inbox.go — the persisted per-instance queue of turns waiting to be
// injected into a harness that isn't currently accepting them.
// See docs/agent-inbox-spec.md §4.
//
// Storage is a table in the daemon's existing local sqlite (~/.hearth/daemon.db,
// see daemon_db.go) rather than a JSONL file. The spec allowed either; sqlite
// wins because the crash-safety story we'd otherwise hand-roll — torn writes,
// half-updated counters, atomic compaction — is exactly what it already does,
// and because `hearth hh agent inbox` needs to read the queue from a second
// process, which WAL gives us for free.
//
// Timestamps are stored as Unix epoch INTEGERs, deliberately. Text DATETIMEs
// in sqlite compare lexically, which is the trap documented at length in
// CLAUDE.md §"Timestamp format"; integers can't have that class of bug.

const (
	// inboxDepthCap bounds one instance's queue. Past this, enqueue refuses
	// rather than growing without limit — a queue nobody drains is a bug to
	// surface, not a buffer to keep filling.
	inboxDepthCap = 500

	// inboxDefaultMaxAttempts is how many times one entry may be injected
	// without the transcript confirming it landed before it is quarantined.
	inboxDefaultMaxAttempts = 5
)

// Per-producer TTLs. A message delivered long after it was meant is sometimes
// worse than one never delivered — a `permission_resolved` notice arriving
// twenty minutes late is pure noise, whereas a person's chat message is still
// worth landing an hour later.
const (
	inboxTTLChat        = 24 * time.Hour
	inboxTTLTrigger     = 1 * time.Hour
	inboxTTLApproval    = 10 * time.Minute
	inboxTTLSystemEvent = 5 * time.Minute
)

// How an entry left the queue. Producers that care about the fate of what they
// queued (scheduled triggers, today) branch on these, so they are constants
// rather than log strings.
const (
	inboxOutcomeConfirmed     = "confirmed"      // the transcript proved it became a turn
	inboxOutcomeLandedLate    = "landed-late"    // same, observed after we stopped waiting
	inboxOutcomeExpired       = "expired"        // TTL elapsed before the agent was ready
	inboxOutcomeUnconfirmable = "unconfirmable"  // no probe to watch; injected best-effort
	inboxOutcomeQuarantined   = "quarantined"    // gave up after max attempts
)

// inboxEntry is one queued turn.
type inboxEntry struct {
	RowID       int64
	Key         string
	InstanceID  string
	Payload     []byte
	Probe       string
	Source      string
	EnqueuedAt  time.Time
	ExpiresAt   time.Time
	Attempts    int
	MaxAttempts int
	State       string // pending | quarantined
	Reason      string
}

// Expired reports whether this entry is past its TTL.
func (e *inboxEntry) Expired(now time.Time) bool {
	return !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt)
}

// agentInbox is the queue for one ai_agent_instance_id. The backing table is
// shared; each inbox is scoped to its instance.
type agentInbox struct {
	db         *DaemonDB
	instanceID string

	mu     sync.Mutex
	notify chan struct{} // closed+replaced on enqueue, to wake the drain loop
}

// usable reports whether this inbox has a backing store. A nil inbox (no local
// DB) must be inert everywhere rather than panicking — the daemon announces the
// degradation once at boot and falls back to direct PTY writes.
func (q *agentInbox) usable() bool {
	return q != nil && q.db != nil && q.db.db != nil
}

func newAgentInbox(db *DaemonDB, instanceID string) *agentInbox {
	return &agentInbox{
		db:         db,
		instanceID: instanceID,
		notify:     make(chan struct{}),
	}
}

// Notify returns a channel closed on the next enqueue. Edge hint, not a value.
func (q *agentInbox) Notify() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.notify
}

func (q *agentInbox) wake() {
	q.mu.Lock()
	close(q.notify)
	q.notify = make(chan struct{})
	q.mu.Unlock()
}

// ensureAgentInboxSchema creates the queue table. Idempotent, same contract as
// the rest of the daemon's local migrations.
func ensureAgentInboxSchema(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_inbox (
		    key                  TEXT PRIMARY KEY,
		    ai_agent_instance_id TEXT NOT NULL,
		    payload              BLOB NOT NULL,
		    probe                TEXT NOT NULL DEFAULT '',
		    source               TEXT NOT NULL DEFAULT '',
		    enqueued_at          INTEGER NOT NULL,
		    expires_at           INTEGER NOT NULL,
		    attempts             INTEGER NOT NULL DEFAULT 0,
		    max_attempts         INTEGER NOT NULL DEFAULT 5,
		    state                TEXT NOT NULL DEFAULT 'pending',
		    quarantine_reason    TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		return fmt.Errorf("create agent_inbox: %w", err)
	}
	if _, err := db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_agent_inbox_instance ON agent_inbox(ai_agent_instance_id, state)`,
	); err != nil {
		return fmt.Errorf("create agent_inbox index: %w", err)
	}
	return nil
}

// Enqueue adds a payload to the back of the queue. The key is the idempotency
// key: re-enqueuing a key that is already pending is a no-op, so a relay retry
// or a daemon reconnect replaying the same message can't double-deliver it.
func (q *agentInbox) Enqueue(key string, payload []byte, probe, source string, ttl time.Duration) error {
	if !q.usable() {
		return fmt.Errorf("inbox unavailable")
	}
	if key == "" {
		return fmt.Errorf("inbox: empty key")
	}

	depth, err := q.Depth()
	if err != nil {
		return err
	}
	if depth >= inboxDepthCap {
		return fmt.Errorf("inbox for %s is at its %d-entry cap", q.instanceID, inboxDepthCap)
	}

	now := time.Now()
	res, err := q.db.db.Exec(`
		INSERT OR IGNORE INTO agent_inbox
		    (key, ai_agent_instance_id, payload, probe, source,
		     enqueued_at, expires_at, attempts, max_attempts, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, 'pending')`,
		key, q.instanceID, payload, probe, source,
		now.Unix(), now.Add(ttl).Unix(), inboxDefaultMaxAttempts,
	)
	if err != nil {
		return fmt.Errorf("inbox enqueue: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already queued under this key — the idempotent case, not an error.
		return nil
	}
	q.wake()
	return nil
}

// Peek returns the oldest pending entry, or nil when the queue is empty.
// Quarantined entries are skipped: one poison message must never block the
// healthy ones behind it.
func (q *agentInbox) Peek() (*inboxEntry, error) {
	if !q.usable() {
		return nil, nil
	}
	row := q.db.db.QueryRow(`
		SELECT rowid, key, payload, probe, source, enqueued_at, expires_at,
		       attempts, max_attempts, state, quarantine_reason
		  FROM agent_inbox
		 WHERE ai_agent_instance_id = ? AND state = 'pending'
		 ORDER BY rowid
		 LIMIT 1`, q.instanceID)
	e, err := scanInboxEntry(row, q.instanceID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return e, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanInboxEntry(row rowScanner, instanceID string) (*inboxEntry, error) {
	var (
		e                  inboxEntry
		enqueuedAt, expiry int64
	)
	err := row.Scan(&e.RowID, &e.Key, &e.Payload, &e.Probe, &e.Source,
		&enqueuedAt, &expiry, &e.Attempts, &e.MaxAttempts, &e.State, &e.Reason)
	if err != nil {
		return nil, err
	}
	e.InstanceID = instanceID
	e.EnqueuedAt = time.Unix(enqueuedAt, 0)
	e.ExpiresAt = time.Unix(expiry, 0)
	return &e, nil
}

// Ack removes a delivered (or deliberately abandoned) entry. `how` is logged,
// not stored — the entry is gone, and the delivered copy is in the transcript.
//
// The log line is worth its noise: "did my message actually reach the agent?"
// is the question this whole mechanism exists to answer, and until now the
// daemon log had no way to say yes.
func (q *agentInbox) Ack(key, how string) error {
	if !q.usable() {
		return nil
	}
	_, err := q.db.db.Exec(`DELETE FROM agent_inbox WHERE key = ?`, key)
	if err != nil {
		return err
	}
	log.Printf("daemon-inbox: %s message %s for agent %s", how, key, q.instanceID)
	return nil
}

// RecordAttempt bumps the attempt counter and returns the new count.
func (q *agentInbox) RecordAttempt(key string) (int, error) {
	if !q.usable() {
		return 0, nil
	}
	if _, err := q.db.db.Exec(
		`UPDATE agent_inbox SET attempts = attempts + 1 WHERE key = ?`, key,
	); err != nil {
		return 0, err
	}
	var n int
	err := q.db.db.QueryRow(`SELECT attempts FROM agent_inbox WHERE key = ?`, key).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

// Quarantine parks an entry that can't be delivered. It stays on disk so a
// human can see what was lost, stops blocking the queue, and is logged at WARN
// with everything needed to act on it. The bar from the spec is that the inbox
// never silently wedges and never silently drops; this is the "never silently"
// half.
func (q *agentInbox) Quarantine(key, reason string) error {
	if !q.usable() {
		return nil
	}
	_, err := q.db.db.Exec(
		`UPDATE agent_inbox SET state = 'quarantined', quarantine_reason = ? WHERE key = ?`,
		reason, key,
	)
	log.Printf("WARN daemon-inbox: quarantined message %s for agent %s: %s "+
		"(inspect with `hearth hh agent inbox %s`)", key, q.instanceID, reason, q.instanceID)
	return err
}

// Depth counts pending entries (quarantined ones aren't waiting on anything).
func (q *agentInbox) Depth() (int, error) {
	if !q.usable() {
		return 0, nil
	}
	var n int
	err := q.db.db.QueryRow(
		`SELECT COUNT(*) FROM agent_inbox WHERE ai_agent_instance_id = ? AND state = 'pending'`,
		q.instanceID,
	).Scan(&n)
	return n, err
}

// DropInstance removes every entry for an instance. Called when the instance is
// retired — a retired agent's queue is not going to drain, and keeping it would
// mean a wake of a same-id instance replaying ancient messages.
func (q *agentInbox) DropInstance() error {
	if !q.usable() {
		return nil
	}
	_, err := q.db.db.Exec(`DELETE FROM agent_inbox WHERE ai_agent_instance_id = ?`, q.instanceID)
	return err
}

// ListAgentInbox returns every entry for an instance, oldest first, for the
// `hearth hh agent inbox` read-only view. Pass an empty instanceID for all.
func ListAgentInbox(db *DaemonDB, instanceID string) ([]*inboxEntry, error) {
	if db == nil || db.db == nil {
		return nil, fmt.Errorf("daemon local DB unavailable")
	}
	query := `
		SELECT rowid, key, payload, probe, source, enqueued_at, expires_at,
		       attempts, max_attempts, state, quarantine_reason,
		       ai_agent_instance_id
		  FROM agent_inbox`
	args := []any{}
	if instanceID != "" {
		query += ` WHERE ai_agent_instance_id = ?`
		args = append(args, instanceID)
	}
	query += ` ORDER BY ai_agent_instance_id, rowid`

	rows, err := db.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*inboxEntry
	for rows.Next() {
		var (
			e                  inboxEntry
			enqueuedAt, expiry int64
		)
		if err := rows.Scan(&e.RowID, &e.Key, &e.Payload, &e.Probe, &e.Source,
			&enqueuedAt, &expiry, &e.Attempts, &e.MaxAttempts, &e.State,
			&e.Reason, &e.InstanceID); err != nil {
			return nil, err
		}
		e.EnqueuedAt = time.Unix(enqueuedAt, 0)
		e.ExpiresAt = time.Unix(expiry, 0)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// CountAgentInbox returns (pending, quarantined) for an instance. Used by
// `hearth status`, which prints nothing when both are zero — a healthy host
// should look exactly as it did before this existed.
func CountAgentInbox(db *DaemonDB, instanceID string) (pending, quarantined int) {
	if db == nil || db.db == nil {
		return 0, 0
	}
	db.db.QueryRow(
		`SELECT COUNT(*) FROM agent_inbox WHERE ai_agent_instance_id = ? AND state = 'pending'`,
		instanceID).Scan(&pending)
	db.db.QueryRow(
		`SELECT COUNT(*) FROM agent_inbox WHERE ai_agent_instance_id = ? AND state = 'quarantined'`,
		instanceID).Scan(&quarantined)
	return pending, quarantined
}
