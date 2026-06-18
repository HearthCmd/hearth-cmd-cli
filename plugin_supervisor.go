package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// backoffSchedule indexes the wait between spawn attempt N and
// attempt N+1. attempts=1 → 100ms wait before the next try, etc.
// The last slot is the cap: attempts past len(schedule)-1 reuse it.
// Package var (not const) so tests can swap in a sped-up schedule.
var backoffSchedule = []time.Duration{
	0,
	100 * time.Millisecond,
	500 * time.Millisecond,
	2 * time.Second,
	10 * time.Second,
	30 * time.Second,
}

// healthyUptimeReset is how long a process must run after reaching
// Ready before its connection's backoff counter resets to zero. A
// plugin that crashes during Init never sets everReady, so its
// attempts keep climbing — preventing endless reset of a plugin
// that flakes during the handshake itself.
var healthyUptimeReset = 30 * time.Second

// backoffState tracks per-connection respawn pacing. attempts is
// the count of consecutive failed spawns since the last reset.
// everReady gates the healthy-uptime reset path; lastSpawnAt is the
// timestamp of the most recent StartPlugin attempt (success or
// failure).
type backoffState struct {
	attempts      int
	nextAllowedAt time.Time
	lastSpawnAt   time.Time
	everReady     bool
}

func backoffDelay(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	if attempts >= len(backoffSchedule) {
		return backoffSchedule[len(backoffSchedule)-1]
	}
	return backoffSchedule[attempts]
}

// PluginSupervisor manages the live set of plugin subprocesses, one
// per active Resource Connection. Lazy launch (the first Invoke
// against a connection spawns the process), reuse on subsequent
// calls, crash respawn on next call after death. Owned by *Daemon
// (commit 7 wires it in), not a package global.
//
// Concurrency strategy: a single sync.Mutex (s.mu) guards the maps
// (procs / spawnLocks / backoff). Per-connection spawnLocks ensure
// only one goroutine ever spawns the same connection's process at a
// time. Once a process exists, its own internal mutex serializes
// I/O — see plugin_process.go for the per-process locking story.
//
// Crash backoff: per-connection exponential schedule
// (backoffSchedule) gates respawn attempts after spawn or Init
// failure. Counter resets after the previous process stayed alive
// past healthyUptimeReset — but only if it ever reached Ready, so a
// plugin that crashes during Init keeps climbing the schedule.
type PluginSupervisor struct {
	registry    *PluginRegistry
	connections *ResourceConnectionStore
	// localDB backs Tier 2 plugin_state. Optional — nil when the
	// daemon couldn't open ~/.hearth/daemon.db; the inbound handler
	// surfaces a forbidden error to State* calls in that case.
	// Phase 3 step 5; see daemon_db.go and docs/resource-plugins-3-plan.md §3.5.
	localDB *DaemonDB

	mu         sync.Mutex
	procs      map[string]*PluginProcess
	spawnLocks map[string]*sync.Mutex
	backoff    map[string]*backoffState
}

func NewPluginSupervisor(reg *PluginRegistry, connections *ResourceConnectionStore, localDB *DaemonDB) *PluginSupervisor {
	return &PluginSupervisor{
		registry:    reg,
		connections: connections,
		localDB:     localDB,
		procs:       map[string]*PluginProcess{},
		spawnLocks:  map[string]*sync.Mutex{},
		backoff:     map[string]*backoffState{},
	}
}

// Invoke dispatches a verb call to the plugin subprocess backing
// the named Resource Connection. Lazy-launches the subprocess on
// first call or after a previous process died. Calls against
// different connections proceed in parallel; calls against the
// same connection serialize on that process's mutex.
//
// Returns *PluginError for all error paths — both plugin-reported
// errors (process still alive) and transport failures (process
// transitioned to Dead, will respawn on next call).
func (s *PluginSupervisor) Invoke(ctx context.Context, connID, verb string, args json.RawMessage, secretBindings map[string]string, bindingID string) (InvokeResult, error) {
	if s == nil {
		return InvokeResult{}, &PluginError{Code: ErrInternal, Message: "plugin supervisor not initialized"}
	}
	conn, ok := s.connections.Get(connID)
	if !ok {
		return InvokeResult{}, &PluginError{Code: ErrBadArgs, Message: "unknown connection: " + connID}
	}
	manifest, ok := s.registry.GetPluginBySlug(conn.PluginSlug)
	if !ok {
		return InvokeResult{}, &PluginError{
			Code:    ErrUnavailable,
			Message: fmt.Sprintf("plugin install %q referenced by connection %q is not registered", conn.PluginSlug, connID),
		}
	}

	proc, err := s.ensureProcess(ctx, connID, manifest)
	if err != nil {
		return InvokeResult{}, err
	}
	return proc.Invoke(ctx, verb, args, secretBindings, bindingID)
}

// buildInboundHandler returns the InboundHandler closure passed to
// every plugin process. Dispatches State* RPCs against the daemon's
// local sqlite, scoped to the per-Invoke binding_id the process is
// running under. See plugin_process.go's InboundHandler doc.
func (s *PluginSupervisor) buildInboundHandler() InboundHandler {
	return func(method string, params json.RawMessage, bindingID string) (json.RawMessage, ErrorCode, string) {
		if s.localDB == nil {
			return nil, ErrUnavailable, "daemon: plugin state KV not initialized on this host"
		}
		// All State* methods need an active binding scope. Empty
		// binding_id means the agent invoked without a binding row
		// (Shape A connections; per plan §3.5 state requires a binding).
		switch method {
		case "StateGet", "StatePut", "StateDelete", "StateList":
			if bindingID == "" {
				return nil, ErrForbidden, "no binding scope for this invoke; create an agent_resource_binding to use plugin state"
			}
		}
		switch method {
		case "StateGet":
			var p struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ErrBadArgs, "StateGet params: " + err.Error()
			}
			value, found, err := s.localDB.PluginStateGet(bindingID, p.Key)
			if err != nil {
				return nil, ErrInternal, "StateGet: " + err.Error()
			}
			encodedValue := ""
			if found {
				encodedValue = base64.StdEncoding.EncodeToString(value)
			}
			result, _ := json.Marshal(map[string]interface{}{
				"value": encodedValue,
				"found": found,
			})
			return result, "", ""
		case "StatePut":
			var p struct {
				Key   string `json:"key"`
				Value string `json:"value"` // base64
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ErrBadArgs, "StatePut params: " + err.Error()
			}
			raw, err := base64.StdEncoding.DecodeString(p.Value)
			if err != nil {
				return nil, ErrBadArgs, "StatePut value: " + err.Error()
			}
			if err := s.localDB.PluginStatePut(bindingID, p.Key, raw); err != nil {
				return nil, ErrInternal, "StatePut: " + err.Error()
			}
			result, _ := json.Marshal(map[string]bool{"ok": true})
			return result, "", ""
		case "StateDelete":
			var p struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ErrBadArgs, "StateDelete params: " + err.Error()
			}
			if err := s.localDB.PluginStateDelete(bindingID, p.Key); err != nil {
				return nil, ErrInternal, "StateDelete: " + err.Error()
			}
			result, _ := json.Marshal(map[string]bool{"ok": true})
			return result, "", ""
		case "StateList":
			var p struct {
				Prefix string `json:"prefix"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ErrBadArgs, "StateList params: " + err.Error()
			}
			keys, err := s.localDB.PluginStateList(bindingID, p.Prefix, p.Limit)
			if err != nil {
				return nil, ErrInternal, "StateList: " + err.Error()
			}
			result, _ := json.Marshal(map[string][]string{"keys": keys})
			return result, "", ""
		default:
			return nil, ErrUnavailable, "unsupported inbound method: " + method
		}
	}
}

// ensureProcess returns a live PluginProcess for connID, spawning
// one if needed. Per-connID spawnLocks plus double-checked locking
// against the procs map prevent concurrent goroutines from racing
// to spawn the same connection's process.
func (s *PluginSupervisor) ensureProcess(ctx context.Context, connID string, manifest PluginManifest) (*PluginProcess, error) {
	s.mu.Lock()
	proc := s.procs[connID]
	spawnLock := s.spawnLocks[connID]
	if spawnLock == nil {
		spawnLock = &sync.Mutex{}
		s.spawnLocks[connID] = spawnLock
	}
	s.mu.Unlock()

	if proc != nil && !proc.isDead() {
		return proc, nil
	}

	spawnLock.Lock()
	defer spawnLock.Unlock()
	// Double-check: another goroutine may have spawned while we
	// waited for spawnLock.
	s.mu.Lock()
	proc = s.procs[connID]
	b := s.backoff[connID]
	if b == nil {
		b = &backoffState{}
		s.backoff[connID] = b
	}
	// Healthy-uptime reset: if the previous process reached Ready
	// AND has been alive (or was alive) past the reset window, treat
	// this as a fresh crash sequence.
	if b.everReady && !b.lastSpawnAt.IsZero() && time.Since(b.lastSpawnAt) >= healthyUptimeReset {
		b.attempts = 0
		b.nextAllowedAt = time.Time{}
	}
	waitUntil := b.nextAllowedAt
	s.mu.Unlock()
	if proc != nil && !proc.isDead() {
		return proc, nil
	}

	if wait := time.Until(waitUntil); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, &PluginError{Code: ErrTransport, Message: "backoff wait: " + ctx.Err().Error()}
		}
	}

	// Credentials are no longer resolved at Init — plugins start with
	// no secrets. Per-invoke secret bindings will arrive on
	// InvokeParams.SecretBindings in the consumption-pipeline epic.

	fresh, err := StartPlugin(ctx, manifest, connID, s.buildInboundHandler())
	s.mu.Lock()
	b.lastSpawnAt = time.Now()
	if err != nil {
		b.attempts++
		b.nextAllowedAt = time.Now().Add(backoffDelay(b.attempts))
		s.mu.Unlock()
		log.Printf("plugin %s: spawn failed (attempt %d, next allowed in %s): %v",
			connID, b.attempts, backoffDelay(b.attempts), err)
		return nil, &PluginError{Code: ErrUnavailable, Message: "spawn: " + err.Error()}
	}
	b.everReady = true
	s.procs[connID] = fresh
	s.mu.Unlock()
	return fresh, nil
}

// EnsureShutdown sends Shutdown to the plugin process backing
// connID, if alive, and removes it from the supervisor. No-op if
// no process exists. Useful for tests and for explicit teardown
// requested via a future `hearth plugin reload`-style IPC.
func (s *PluginSupervisor) EnsureShutdown(connID string) error {
	s.mu.Lock()
	proc := s.procs[connID]
	delete(s.procs, connID)
	s.mu.Unlock()
	if proc == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginShutdownGrace)
	defer cancel()
	return proc.Shutdown(ctx)
}

// ShutdownAll sends Shutdown to every live process in parallel and
// waits for them to exit. Called from Daemon.Shutdown (commit 7).
// Safe on a nil supervisor (returns nil immediately).
func (s *PluginSupervisor) ShutdownAll() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	procs := make([]*PluginProcess, 0, len(s.procs))
	for _, p := range s.procs {
		procs = append(procs, p)
	}
	s.procs = map[string]*PluginProcess{}
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Add(1)
		go func(p *PluginProcess) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), pluginShutdownGrace)
			defer cancel()
			_ = p.Shutdown(ctx)
		}(p)
	}
	wg.Wait()
	return nil
}
