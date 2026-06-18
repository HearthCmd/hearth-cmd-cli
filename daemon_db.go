//go:build darwin || linux

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Daemon-local sqlite. Phase 3 step 5 introduces this to back the
// plugin_state KV (Tier 2 per plan §3.5): per-binding key/value
// storage that plugins read/write via StateGet/StatePut RPCs the
// daemon mediates. The DB lives next to the rest of the daemon's
// state at ~/.hearth/daemon.db.
//
// Design notes:
//   - One file, one *sql.DB handle. Concurrent callers serialize at
//     the sqlite level; SQLite's WAL is enough for the daemon's
//     workload (low-throughput, single-process).
//   - The daemon owns the handle. Plugins NEVER see a file path,
//     never link sqlite, never even know storage exists — they call
//     two RPCs. Per-binding scoping is enforced at the RPC boundary
//     in the daemon, so a buggy/malicious plugin physically cannot
//     read another binding's state.
//   - Migrations are idempotent CREATE TABLE IF NOT EXISTS — the
//     daemon's local DB is greenfield in phase 3, so we don't need
//     the migration ladder pattern the server uses.
//
// Cleanup contract (plan §8): when a binding is hard-deleted (agent
// fire cascade, explicit revoke), DeleteForBinding wipes its rows.

// DaemonDB wraps the local sqlite handle plus the schema migrations.
type DaemonDB struct {
	db *sql.DB
}

// OpenDaemonDB opens (and creates if missing) the daemon's local
// sqlite at <home>/.hearth/daemon.db, runs idempotent migrations,
// and returns a ready handle. Callers Close on shutdown.
//
// busy_timeout is set so concurrent writes from the state RPC handler
// and any future writer (rotation, GC) don't immediately fail with
// SQLITE_BUSY — they wait up to 5s before erroring out.
func OpenDaemonDB(home string) (*DaemonDB, error) {
	if home == "" {
		return nil, fmt.Errorf("daemon DB requires non-empty home dir")
	}
	path := filepath.Join(home, ".hearth", "daemon.db")
	// _busy_timeout=5000 → 5 second SQLITE_BUSY backoff; _journal_mode=WAL
	// for concurrent readers (we have one writer today but the option is
	// cheap).
	dsn := "file:" + path + "?_busy_timeout=5000&_journal_mode=WAL"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS plugin_state (
		    binding_id TEXT NOT NULL,
		    key        TEXT NOT NULL,
		    value      BLOB NOT NULL,
		    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		    PRIMARY KEY (binding_id, key)
		)
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create plugin_state: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_plugin_state_binding ON plugin_state(binding_id)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create plugin_state index: %w", err)
	}
	// resource_entities: daemon-local cache of declarative-adapter
	// snapshots. Populated by DeclarativeExecutor.RunSnapshot
	// (triggered explicitly via the resource_refresh IPC). Read by
	// the eventual IAM evaluator when it predicates on entity kind /
	// labels / parent. Server has no copy — these are hot-path data
	// the daemon owns. See docs/external-resource-adapters.md §"IAM
	// evaluation split".
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS resource_entities (
		    connection_id TEXT NOT NULL,
		    entity_id     TEXT NOT NULL,
		    kind          TEXT NOT NULL,
		    labels_json   TEXT NOT NULL DEFAULT '{}',
		    parent        TEXT NOT NULL DEFAULT '',
		    fetched_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		    PRIMARY KEY (connection_id, entity_id)
		)
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create resource_entities: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_resource_entities_conn ON resource_entities(connection_id)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create resource_entities index: %w", err)
	}
	log.Printf("daemon: local sqlite ready (%s)", path)
	return &DaemonDB{db: db}, nil
}

// Close releases the handle. Idempotent.
func (d *DaemonDB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

// PluginStateGet returns the stored value for (bindingID, key).
// Found=false on miss; nil value on miss is also valid (callers
// inspect found).
func (d *DaemonDB) PluginStateGet(bindingID, key string) (value []byte, found bool, err error) {
	if bindingID == "" {
		return nil, false, fmt.Errorf("PluginStateGet: empty binding_id (daemon must inject from in-flight Invoke)")
	}
	var raw []byte
	err = d.db.QueryRow(
		`SELECT value FROM plugin_state WHERE binding_id = ? AND key = ?`,
		bindingID, key,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// PluginStatePut upserts (bindingID, key) = value. Empty value is
// allowed (plugins may use it as a deliberate placeholder); use
// PluginStateDelete to remove the row entirely.
func (d *DaemonDB) PluginStatePut(bindingID, key string, value []byte) error {
	if bindingID == "" {
		return fmt.Errorf("PluginStatePut: empty binding_id (daemon must inject from in-flight Invoke)")
	}
	if key == "" {
		return fmt.Errorf("PluginStatePut: empty key")
	}
	_, err := d.db.Exec(`
		INSERT INTO plugin_state (binding_id, key, value, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (binding_id, key) DO UPDATE
		   SET value = excluded.value,
		       updated_at = CURRENT_TIMESTAMP
	`, bindingID, key, value)
	return err
}

// PluginStateDelete removes (bindingID, key). Idempotent — deleting
// a missing key is success.
func (d *DaemonDB) PluginStateDelete(bindingID, key string) error {
	if bindingID == "" {
		return fmt.Errorf("PluginStateDelete: empty binding_id")
	}
	_, err := d.db.Exec(`DELETE FROM plugin_state WHERE binding_id = ? AND key = ?`, bindingID, key)
	return err
}

// PluginStateList returns the keys (sorted) under bindingID whose
// key starts with prefix. Empty prefix matches all keys for the
// binding. Bounded by `limit` for safety; 0 = "no list cap" — the
// daemon shouldn't have unbounded state per binding in practice.
func (d *DaemonDB) PluginStateList(bindingID, prefix string, limit int) ([]string, error) {
	if bindingID == "" {
		return nil, fmt.Errorf("PluginStateList: empty binding_id")
	}
	q := `SELECT key FROM plugin_state WHERE binding_id = ? AND key LIKE ? ESCAPE '\' ORDER BY key`
	args := []interface{}{bindingID, escapeLikeUnderscoreAndPercent(prefix) + "%"}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteForBinding nukes every row for the binding. Called when a
// binding is hard-deleted (agent fire cascade, explicit revoke) so
// the local sqlite doesn't accumulate orphaned KV.
func (d *DaemonDB) DeleteForBinding(bindingID string) (int64, error) {
	if bindingID == "" {
		return 0, nil
	}
	res, err := d.db.Exec(`DELETE FROM plugin_state WHERE binding_id = ?`, bindingID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReplaceEntities atomically swaps the daemon's snapshot of entities
// for connID. Implementation: DELETE all rows for connID, then INSERT
// the new set, in a single transaction. Callers pass the result of a
// successful RunSnapshot; partial / failed snapshots should NOT call
// this — leaving stale entities is better than leaving none.
func (d *DaemonDB) ReplaceEntities(connID string, entities []Entity) error {
	if d == nil || d.db == nil {
		return fmt.Errorf("ReplaceEntities: daemon DB not open")
	}
	if connID == "" {
		return fmt.Errorf("ReplaceEntities: empty connection_id")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM resource_entities WHERE connection_id = ?`, connID); err != nil {
		return fmt.Errorf("delete old entities: %w", err)
	}
	stmt, err := tx.Prepare(`
		INSERT INTO resource_entities
		    (connection_id, entity_id, kind, labels_json, parent, fetched_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()
	for _, e := range entities {
		labels := "{}"
		if len(e.Labels) > 0 {
			b, jerr := json.Marshal(e.Labels)
			if jerr != nil {
				return fmt.Errorf("marshal labels for %s: %w", e.EntityID, jerr)
			}
			labels = string(b)
		}
		if _, err := stmt.Exec(connID, e.EntityID, e.Kind, labels, e.Parent); err != nil {
			return fmt.Errorf("insert %s: %w", e.EntityID, err)
		}
	}
	return tx.Commit()
}

// LatestEntityFetchedAt returns the most recent fetched_at across all
// entities for connID. (true, t) on a populated cache, (false, zero)
// when the cache is empty or the connection has never been
// snapshotted. Used by the resource-list IPC enricher to surface
// "last refreshed N ago" hints, and by handleResourceInvoke to warn
// when the cache is stale.
func (d *DaemonDB) LatestEntityFetchedAt(connID string) (bool, time.Time, error) {
	if d == nil || d.db == nil {
		return false, time.Time{}, fmt.Errorf("LatestEntityFetchedAt: daemon DB not open")
	}
	var raw sql.NullString
	err := d.db.QueryRow(`
		SELECT MAX(fetched_at) FROM resource_entities WHERE connection_id = ?
	`, connID).Scan(&raw)
	if err != nil {
		return false, time.Time{}, err
	}
	if !raw.Valid || raw.String == "" {
		return false, time.Time{}, nil
	}
	// SQLite returns the column in a few possible textual shapes
	// depending on whether CURRENT_TIMESTAMP wrote it or app code did.
	// Try the common ones; give up cleanly if none match.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04:05.999999999-07:00"} {
		if t, perr := time.Parse(layout, raw.String); perr == nil {
			return true, t.UTC(), nil
		}
	}
	return false, time.Time{}, fmt.Errorf("LatestEntityFetchedAt: unparseable timestamp %q", raw.String)
}

// ListEntities returns every cached entity for connID, sorted by
// entity_id for deterministic output. Empty slice (not nil) on no
// rows. Returns an error only on actual DB failure.
func (d *DaemonDB) ListEntities(connID string) ([]Entity, error) {
	if d == nil || d.db == nil {
		return nil, fmt.Errorf("ListEntities: daemon DB not open")
	}
	rows, err := d.db.Query(`
		SELECT entity_id, kind, labels_json, parent
		  FROM resource_entities
		 WHERE connection_id = ?
		 ORDER BY entity_id
	`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entity{}
	for rows.Next() {
		var (
			e         Entity
			labelsRaw string
		)
		if err := rows.Scan(&e.EntityID, &e.Kind, &labelsRaw, &e.Parent); err != nil {
			return nil, err
		}
		if labelsRaw != "" && labelsRaw != "{}" {
			if err := json.Unmarshal([]byte(labelsRaw), &e.Labels); err != nil {
				return nil, fmt.Errorf("decode labels for %s: %w", e.EntityID, err)
			}
		} else {
			e.Labels = map[string]string{}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// escapeLikeUnderscoreAndPercent escapes the two LIKE metacharacters
// so a plugin-supplied prefix can't accidentally widen the match.
// SQLite's default escape character is ESCAPE, which we don't use —
// just stick a backslash in front of metachars and accept that a
// literal '\' in keys is also affected (acceptable: keys are
// plugin-chosen identifiers, not user input).
func escapeLikeUnderscoreAndPercent(s string) string {
	r := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%', '_', '\\':
			r = append(r, '\\', s[i])
		default:
			r = append(r, s[i])
		}
	}
	return string(r)
}
