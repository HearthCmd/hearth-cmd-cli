//go:build darwin || linux

package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Relay holds the state for a detached PTY — one per live agent instance.
// The PTY output is drained internally; user-visible activity flows via the
// agent's transcript JSONL + bridge streamer, not through this PTY.
type Relay struct {
	cmd    *exec.Cmd
	master *os.File
	slave  *os.File
	mu     sync.Mutex // serializes writes to master
	wsConn WSConn     // WebSocket interface
	killed bool       // true if the child was killed (not normal exit)

	// Shutdown coordination — closed when the child process exits.
	shutdownCh chan struct{}

	// onStarted fires exactly once, immediately after cmd.Start() succeeds
	// inside RunDaemon, with the child's PID. Used by the daemon to register
	// the agent in the PID → agent_id identity registry for peer-cred-based
	// authorization. Called before any PTY I/O, so the registry entry is
	// present by the time the agent's first tool call arrives.
	onStarted func(pid int)

	// onFirstOutput fires exactly once, the first time the child writes
	// anything to the PTY. Used by the daemon to flip pid_status from
	// 'spawning' to 'running' — nearly every harness is silent until its
	// UI is initialized, so first-byte is a cheap "the process woke up"
	// signal that doesn't require per-harness pattern matching.
	onFirstOutput func()

	// Inject gating. Some harnesses (codex) drop pasted input that arrives
	// before their TUI is actually consuming stdin — even after enabling
	// bracketed-paste mode, codex's input loop isn't ready until its
	// initial render settles. We approximate readiness with a quiet-window
	// heuristic: open the gate when the child has produced output and then
	// been silent for injectGateQuietWindow. injectGateTimeout caps the
	// total wait so we can't hang forever if the child keeps streaming
	// (e.g. early diagnostic noise).
	gateInject       bool
	injectGate       chan struct{}
	injectGateOpened sync.Once
	injectGateTimer  *time.Timer

	// warmupPayload, if non-empty, is written to the PTY exactly once,
	// the moment the inject gate opens. It runs ahead of any user
	// injects (which are blocked on the gate). Used to push codex past
	// its first-turn rollout-flush quirk.
	warmupPayload []byte

	// attachHub fans PTY output out to `hearth agent attach`
	// consumers and holds the ring buffer for replay-on-connect.
	// nil for agents whose harness isn't supported by attach (today
	// claude only; see daemon_session.go where the field is set).
	// Writes to master from the attach `--write` path go through the
	// existing mu lock — InjectRaw is the entry point.
	attachHub *attachHub

	// killFunc, when set, forcibly terminates the child process group.
	// Wired up in daemon_session.go after cmd.Start(); called by the
	// interpose handler when the user selects Deny & Stop.
	killFunc func()
}

// injectGateQuietWindow is how long the PTY must be silent (after the
// first output) before we declare the child ready for injects.
const injectGateQuietWindow = 1500 * time.Millisecond

// injectGateTimeout caps how long Inject will wait for the readiness
// signal before falling through and writing anyway.
const injectGateTimeout = 30 * time.Second

// EnableInjectGate turns on the bracketed-paste readiness gate for this
// relay. Must be called before the first Inject. Only used for codex
// today; other harnesses don't need it.
func (r *Relay) EnableInjectGate() {
	r.gateInject = true
	if r.injectGate == nil {
		r.injectGate = make(chan struct{})
	}
}

// SetWarmupPayload registers bytes to write to the PTY the first time
// the inject gate opens, ahead of any user injects.
func (r *Relay) SetWarmupPayload(payload []byte) {
	r.warmupPayload = payload
}

// openInjectGate signals that the child is ready to receive injects.
// Idempotent; safe to call repeatedly.
func (r *Relay) openInjectGate(reason string) {
	if r.injectGate == nil {
		return
	}
	r.injectGateOpened.Do(func() {
		if len(r.warmupPayload) > 0 {
			r.mu.Lock()
			_, err := r.master.Write(r.warmupPayload)
			r.mu.Unlock()
			if err != nil {
				log.Printf("relay: warmup write failed: %v", err)
			} else {
				log.Printf("relay: warmup payload sent (%d bytes)", len(r.warmupPayload))
				time.Sleep(50 * time.Millisecond)
				r.mu.Lock()
				_, err = r.master.Write([]byte{'\r'})
				r.mu.Unlock()
				if err != nil {
					log.Printf("relay: warmup submit failed: %v", err)
				}
			}
		}
		close(r.injectGate)
		log.Printf("relay: inject gate opened (%s)", reason)
	})
}

// Inject writes data directly to the PTY master as if it were typed.
// Safe to call from any goroutine. When the inject gate is enabled,
// blocks until the child signals readiness or the fallback timeout.
func (r *Relay) Inject(data []byte) error {
	if r.gateInject && r.injectGate != nil {
		select {
		case <-r.injectGate:
		case <-time.After(injectGateTimeout):
			log.Printf("relay: inject gate timed out after %s; writing anyway", injectGateTimeout)
			r.openInjectGate("timeout")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.master.Write(data)
	return err
}

// InjectRaw writes data to the PTY master without engaging the
// inject gate. Used by `hearth agent attach --write`: the user is
// typing live, the TUI is by definition past its bracketed-paste
// warm-up by the time they attach. Same lock as Inject so writes
// from attach and from the WS inject path can't interleave.
func (r *Relay) InjectRaw(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.master == nil {
		return io.ErrClosedPipe
	}
	_, err := r.master.Write(data)
	return err
}

// noteOutput is called from the PTY reader on every non-empty chunk.
// Each call resets the quiet-window timer; when the timer fires
// without another chunk arriving, we declare the child ready.
func (r *Relay) noteOutput() {
	if !r.gateInject || r.injectGate == nil {
		return
	}
	r.mu.Lock()
	if r.injectGateTimer == nil {
		r.injectGateTimer = time.AfterFunc(injectGateQuietWindow, func() {
			r.openInjectGate("quiet window")
		})
	} else {
		r.injectGateTimer.Reset(injectGateQuietWindow)
	}
	r.mu.Unlock()
}
