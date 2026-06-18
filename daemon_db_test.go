//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Phase 3 step 5 — daemon-local plugin_state KV tests.

func openTestDaemonDB(t *testing.T) *DaemonDB {
	t.Helper()
	// Stage a fake home dir under TempDir so OpenDaemonDB's
	// <home>/.hearth/daemon.db path is sandboxed.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".hearth"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := OpenDaemonDB(home)
	if err != nil {
		t.Fatalf("OpenDaemonDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPluginState_PutGetRoundTrip(t *testing.T) {
	db := openTestDaemonDB(t)
	if err := db.PluginStatePut("arb-1", "last_synced_at", []byte("2026-05-18T10:00:00Z")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	val, found, err := db.PluginStateGet("arb-1", "last_synced_at")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("expected found=true after Put")
	}
	if string(val) != "2026-05-18T10:00:00Z" {
		t.Errorf("value mismatch: got %q", val)
	}
}

func TestPluginState_GetMiss(t *testing.T) {
	db := openTestDaemonDB(t)
	val, found, err := db.PluginStateGet("arb-nonexistent", "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Errorf("expected found=false on miss, got value=%q", val)
	}
}

func TestPluginState_PutOverwritesExisting(t *testing.T) {
	db := openTestDaemonDB(t)
	_ = db.PluginStatePut("arb-1", "k", []byte("v1"))
	_ = db.PluginStatePut("arb-1", "k", []byte("v2"))
	val, _, _ := db.PluginStateGet("arb-1", "k")
	if string(val) != "v2" {
		t.Errorf("Put didn't overwrite: got %q", val)
	}
}

func TestPluginState_BindingScopeIsolated(t *testing.T) {
	db := openTestDaemonDB(t)
	_ = db.PluginStatePut("arb-A", "k", []byte("A-value"))
	_ = db.PluginStatePut("arb-B", "k", []byte("B-value"))
	a, _, _ := db.PluginStateGet("arb-A", "k")
	b, _, _ := db.PluginStateGet("arb-B", "k")
	if string(a) != "A-value" || string(b) != "B-value" {
		t.Errorf("cross-binding read: A=%q B=%q", a, b)
	}
}

func TestPluginState_DeleteThenGet(t *testing.T) {
	db := openTestDaemonDB(t)
	_ = db.PluginStatePut("arb-1", "k", []byte("v"))
	if err := db.PluginStateDelete("arb-1", "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, found, _ := db.PluginStateGet("arb-1", "k")
	if found {
		t.Error("expected miss after Delete")
	}
}

func TestPluginState_DeleteMissingIsNoOp(t *testing.T) {
	db := openTestDaemonDB(t)
	if err := db.PluginStateDelete("arb-1", "no-such-key"); err != nil {
		t.Errorf("delete missing should be no-op, got: %v", err)
	}
}

func TestPluginState_List(t *testing.T) {
	db := openTestDaemonDB(t)
	_ = db.PluginStatePut("arb-1", "cursor:gmail", []byte("a"))
	_ = db.PluginStatePut("arb-1", "cursor:drive", []byte("b"))
	_ = db.PluginStatePut("arb-1", "config:host", []byte("c"))

	keys, err := db.PluginStateList("arb-1", "cursor:", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("List cursor:* = %v; want 2", keys)
	}

	all, _ := db.PluginStateList("arb-1", "", 0)
	if len(all) != 3 {
		t.Errorf("List all = %v; want 3", all)
	}
}

func TestPluginState_ListEscapesWildcards(t *testing.T) {
	db := openTestDaemonDB(t)
	_ = db.PluginStatePut("arb-1", "a_b", []byte("1"))    // literal underscore
	_ = db.PluginStatePut("arb-1", "axb", []byte("2"))    // would match if _ stayed a wildcard
	_ = db.PluginStatePut("arb-1", "ayb", []byte("3"))
	keys, _ := db.PluginStateList("arb-1", "a_", 0)
	// With escape, prefix "a_" should match only "a_b", not "axb"/"ayb".
	if len(keys) != 1 || keys[0] != "a_b" {
		t.Errorf("prefix 'a_' should match only literal underscores; got %v", keys)
	}
}

func TestPluginState_DeleteForBinding(t *testing.T) {
	db := openTestDaemonDB(t)
	_ = db.PluginStatePut("arb-1", "a", []byte("x"))
	_ = db.PluginStatePut("arb-1", "b", []byte("y"))
	_ = db.PluginStatePut("arb-keep", "a", []byte("z"))

	n, err := db.DeleteForBinding("arb-1")
	if err != nil {
		t.Fatalf("DeleteForBinding: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 rows deleted, got %d", n)
	}
	// Survivor untouched.
	_, found, _ := db.PluginStateGet("arb-keep", "a")
	if !found {
		t.Error("DeleteForBinding nuked other binding's data")
	}
}

func TestPluginState_EmptyBindingIDRejected(t *testing.T) {
	db := openTestDaemonDB(t)
	if err := db.PluginStatePut("", "k", []byte("v")); err == nil {
		t.Error("expected Put with empty binding_id to error")
	}
	if _, _, err := db.PluginStateGet("", "k"); err == nil {
		t.Error("expected Get with empty binding_id to error")
	}
}
