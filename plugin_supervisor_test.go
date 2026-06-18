package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// crashyManifest returns a manifest pointing at the freshly-built
// testdata/plugins/crashy binary. Its Init handler exits non-zero,
// so every StartPlugin attempt fails — used to exercise backoff.
func crashyManifest(t *testing.T) PluginManifest {
	t.Helper()
	m := echoManifest(t)
	m.PluginSlug = "crashy"
	m.PluginSlug = "crashy"
	m.Executable = "./hearth-plugin-crashy"
	m.SourceDir = strings.TrimSuffix(m.SourceDir, "echo") + "crashy"
	return m
}

// newTestSupervisorWithCrashy registers both echo and crashy in the
// registry. Useful for backoff tests that need a connection bound
// to an always-failing install. conns is a map of
// connection_id → plugin_plugin_slug; both register entries in the
// ResourceConnectionStore via direct swap (yaml-bootstrap is gone post-2b).
func newTestSupervisorWithCrashy(t *testing.T, conns map[string]string) *PluginSupervisor {
	t.Helper()
	reg := NewPluginRegistry()
	reg.byPluginSlug = map[string]PluginManifest{
		"echo":   echoManifest(t),
		"crashy": crashyManifest(t),
	}
	reg.order = []string{"crashy", "echo"}

	store := seedResourceConns(conns)
	s := NewPluginSupervisor(reg, store, nil)
	t.Cleanup(func() { _ = s.ShutdownAll() })
	return s
}

// withBackoffSchedule installs a sped-up backoff schedule for the
// duration of a test. Restores the original on cleanup.
func withBackoffSchedule(t *testing.T, schedule []time.Duration) {
	t.Helper()
	prev := backoffSchedule
	backoffSchedule = schedule
	t.Cleanup(func() { backoffSchedule = prev })
}

// echoTestConns is the default fixture: a single "echo-test"
// connection bound to the echo plugin install.
var echoTestConns = map[string]string{"echo-test": "echo"}

// newTestSupervisor wires a registry (echo manifest), a ResourceConnectionStore
// populated from conns, and the supervisor itself. Returns the supervisor
// plus a t.Cleanup-registered ShutdownAll so test failures don't leak
// subprocesses.
func newTestSupervisor(t *testing.T, conns map[string]string) *PluginSupervisor {
	t.Helper()
	reg := NewPluginRegistry()
	reg.byPluginSlug = map[string]PluginManifest{
		"echo": echoManifest(t),
	}
	reg.order = []string{"echo"}

	store := seedResourceConns(conns)
	s := NewPluginSupervisor(reg, store, nil)
	t.Cleanup(func() { _ = s.ShutdownAll() })
	return s
}

// seedResourceConns builds a ResourceConnectionStore directly. Replaces the
// pre-2b yaml LoadFromEnv path in tests.
func seedResourceConns(conns map[string]string) *ResourceConnectionStore {
	store := NewResourceConnectionStore()
	next := map[string]ResourceConnection{}
	for connID, pluginSlug := range conns {
		next[connID] = ResourceConnection{ConnectionID: connID, Slug: connID, PluginSlug: pluginSlug}
	}
	store.swap(next)
	return store
}

func TestSupervisor_LaunchAndInvoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newTestSupervisor(t, echoTestConns)

	result, err := s.Invoke(ctx, "echo-test", "echo", json.RawMessage(`{"hi":"there"}`), nil, "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Stdout != `{"hi":"there"}` {
		t.Errorf("Stdout = %q", result.Stdout)
	}
}

func TestSupervisor_ProcessReused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newTestSupervisor(t, echoTestConns)

	if _, err := s.Invoke(ctx, "echo-test", "echo", json.RawMessage(`{}`), nil, ""); err != nil {
		t.Fatalf("first Invoke: %v", err)
	}
	proc1 := s.procs["echo-test"]
	if proc1 == nil || proc1.cmd.Process == nil {
		t.Fatal("expected process after first Invoke")
	}
	pid1 := proc1.cmd.Process.Pid

	for i := 0; i < 3; i++ {
		if _, err := s.Invoke(ctx, "echo-test", "echo", json.RawMessage(`{}`), nil, ""); err != nil {
			t.Fatalf("repeat Invoke %d: %v", i, err)
		}
	}
	proc2 := s.procs["echo-test"]
	if proc2.cmd.Process.Pid != pid1 {
		t.Errorf("expected same PID across reuses; got %d then %d", pid1, proc2.cmd.Process.Pid)
	}
}

func TestSupervisor_PluginError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newTestSupervisor(t, echoTestConns)

	_, err := s.Invoke(ctx, "echo-test", "fail", nil, nil, "")
	var pe *PluginError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PluginError, got %T: %v", err, err)
	}
	if pe.Code != ErrInternal {
		t.Errorf("Code = %q; want internal", pe.Code)
	}
	// Process must still be alive after a plugin-reported error.
	if proc := s.procs["echo-test"]; proc == nil || proc.isDead() {
		t.Error("process should still be alive after plugin-reported error")
	}
	if _, err := s.Invoke(ctx, "echo-test", "echo", json.RawMessage(`{}`), nil, ""); err != nil {
		t.Errorf("follow-up Invoke after plugin error: %v", err)
	}
}

func TestSupervisor_CrashRespawn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newTestSupervisor(t, echoTestConns)

	if _, err := s.Invoke(ctx, "echo-test", "echo", json.RawMessage(`{}`), nil, ""); err != nil {
		t.Fatalf("warmup Invoke: %v", err)
	}
	pid1 := s.procs["echo-test"].cmd.Process.Pid

	// Kill the process via the exit verb. Returns ErrTransport.
	_, err := s.Invoke(ctx, "echo-test", "exit", nil, nil, "")
	var pe *PluginError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PluginError on exit verb, got %T: %v", err, err)
	}
	if pe.Code != ErrTransport {
		t.Errorf("Code = %q; want transport", pe.Code)
	}

	// Wait briefly for waitForExit to mark the proc dead, then a
	// follow-up Invoke must respawn a fresh process.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p := s.procs["echo-test"]; p != nil && p.isDead() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := s.Invoke(ctx, "echo-test", "echo", json.RawMessage(`{}`), nil, ""); err != nil {
		t.Fatalf("respawn Invoke: %v", err)
	}
	pid2 := s.procs["echo-test"].cmd.Process.Pid
	if pid2 == pid1 {
		t.Errorf("expected new PID after respawn; got same %d", pid1)
	}
}

func TestSupervisor_StderrForwarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logBuf := captureLog(t)
	s := newTestSupervisor(t, echoTestConns)

	if _, err := s.Invoke(ctx, "echo-test", "log_stderr", json.RawMessage(`{}`), nil, ""); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := logBuf.String()
		if strings.Contains(s, "plugin echo-test:") && strings.Contains(s, "echo plugin stderr marker") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("expected stderr marker in log; got:\n%s", logBuf.String())
}

func TestSupervisor_UnknownConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s := newTestSupervisor(t, echoTestConns)

	_, err := s.Invoke(ctx, "no-such-conn", "echo", json.RawMessage(`{}`), nil, "")
	var pe *PluginError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PluginError, got %T: %v", err, err)
	}
	if pe.Code != ErrBadArgs {
		t.Errorf("Code = %q; want bad_args", pe.Code)
	}
}

func TestSupervisor_UnknownPluginInstall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s := newTestSupervisor(t, map[string]string{"orphan": "ghost"})
	_, err := s.Invoke(ctx, "orphan", "echo", json.RawMessage(`{}`), nil, "")
	var pe *PluginError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PluginError, got %T: %v", err, err)
	}
	if pe.Code != ErrUnavailable {
		t.Errorf("Code = %q; want unavailable", pe.Code)
	}
}

func TestBackoffDelay(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 0},
		{1, 100 * time.Millisecond},
		{2, 500 * time.Millisecond},
		{3, 2 * time.Second},
		{4, 10 * time.Second},
		{5, 30 * time.Second},
		{6, 30 * time.Second}, // cap
		{99, 30 * time.Second},
	}
	for _, tt := range tests {
		if got := backoffDelay(tt.attempts); got != tt.want {
			t.Errorf("backoffDelay(%d) = %s; want %s", tt.attempts, got, tt.want)
		}
	}
}

func TestSupervisor_BackoffCap(t *testing.T) {
	withBackoffSchedule(t, []time.Duration{
		0,
		50 * time.Millisecond,
		200 * time.Millisecond,
		200 * time.Millisecond, // cap
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newTestSupervisorWithCrashy(t, map[string]string{"crashy-test": "crashy"})

	// First Invoke: backoff=0, spawn attempt #1 fails.
	start := time.Now()
	_, err := s.Invoke(ctx, "crashy-test", "noop", nil, nil, "")
	if err == nil {
		t.Fatal("expected spawn failure")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("first attempt took %s; expected near-zero", d)
	}

	// Three more failing attempts in a row. Expected waits:
	// 50ms, 200ms, 200ms — total ≥ 450ms.
	mid := time.Now()
	for i := 0; i < 3; i++ {
		_, err := s.Invoke(ctx, "crashy-test", "noop", nil, nil, "")
		if err == nil {
			t.Fatalf("attempt %d: expected spawn failure", i+2)
		}
		var pe *PluginError
		if !errors.As(err, &pe) || pe.Code != ErrUnavailable {
			t.Errorf("attempt %d: want ErrUnavailable, got %v", i+2, err)
		}
	}
	elapsed := time.Since(mid)
	if elapsed < 400*time.Millisecond {
		t.Errorf("3 backoff-gated attempts elapsed %s; want ≥ 450ms (schedule honored)", elapsed)
	}

	// Counter should have advanced past the schedule length; cap held.
	s.mu.Lock()
	b := s.backoff["crashy-test"]
	attempts := b.attempts
	s.mu.Unlock()
	if attempts < 4 {
		t.Errorf("attempts = %d; want ≥ 4", attempts)
	}
}

func TestSupervisor_BackoffWaitRespectsContext(t *testing.T) {
	withBackoffSchedule(t, []time.Duration{0, 5 * time.Second})

	s := newTestSupervisorWithCrashy(t, map[string]string{"crashy-test": "crashy"})

	// First attempt drives attempts to 1; nextAllowedAt = now + 5s.
	if _, err := s.Invoke(context.Background(), "crashy-test", "noop", nil, nil, ""); err == nil {
		t.Fatal("expected spawn failure")
	}

	// Second attempt under a short-deadline ctx must abort during
	// the backoff wait with ErrTransport, not block ~5s.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Invoke(ctx, "crashy-test", "noop", nil, nil, "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected ctx-deadline error")
	}
	var pe *PluginError
	if !errors.As(err, &pe) || pe.Code != ErrTransport {
		t.Errorf("want ErrTransport during backoff, got %v", err)
	}
	if elapsed > 1*time.Second {
		t.Errorf("Invoke blocked %s past ctx deadline; ctx should have interrupted backoff wait", elapsed)
	}
}

func TestSupervisor_EnsureShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newTestSupervisor(t, echoTestConns)

	if _, err := s.Invoke(ctx, "echo-test", "echo", json.RawMessage(`{}`), nil, ""); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if s.procs["echo-test"] == nil {
		t.Fatal("expected process before shutdown")
	}
	if err := s.EnsureShutdown("echo-test"); err != nil {
		t.Fatalf("EnsureShutdown: %v", err)
	}
	if s.procs["echo-test"] != nil {
		t.Error("process should be removed after EnsureShutdown")
	}
	// Idempotent: shutting down again is a no-op.
	if err := s.EnsureShutdown("echo-test"); err != nil {
		t.Errorf("second EnsureShutdown: %v", err)
	}
}
