//go:build darwin || linux

package main

import (
	"io"
	"os"
	"testing"
	"time"
)

// Tests the inject-gate state machine on Relay (memory note:
// project_codex_first_turn_warmup.md / project_gemini_paste_submit_sigwinch.md).
// We swap the PTY master with one end of an os.Pipe so writes are
// observable in-test without spinning a real PTY.

// pipeRelay returns a Relay whose master is the writer half of an os.Pipe.
// The reader half is returned for assertions.
func pipeRelay(t *testing.T) (*Relay, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = w.Close()
		_ = r.Close()
	})
	return &Relay{master: w}, r
}

// drainPipe reads up to want bytes (or EOF) and returns them. Bounded
// so a buggy test doesn't block forever.
func drainPipe(t *testing.T, r *os.File, want int) []byte {
	t.Helper()
	if err := r.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, want)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		t.Fatalf("read: %v (got %d bytes)", err, n)
	}
	return buf[:n]
}

func TestRelay_InjectWritesDirectlyWhenGateDisabled(t *testing.T) {
	r, rd := pipeRelay(t)
	if err := r.Inject([]byte("hello")); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	got := drainPipe(t, rd, 5)
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestRelay_InjectBlocksUntilGateOpens(t *testing.T) {
	r, rd := pipeRelay(t)
	r.EnableInjectGate()

	// Inject from a goroutine — must block until openInjectGate fires.
	done := make(chan error, 1)
	go func() {
		done <- r.Inject([]byte("hi"))
	}()

	// Give the goroutine a moment to actually park on the gate.
	select {
	case err := <-done:
		t.Fatalf("Inject returned before gate opened (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
	}

	r.openInjectGate("test")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Inject after open: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Inject did not return after gate opened")
	}

	got := drainPipe(t, rd, 2)
	if string(got) != "hi" {
		t.Errorf("got %q", got)
	}
}

func TestRelay_OpenInjectGateIsIdempotent(t *testing.T) {
	r, _ := pipeRelay(t)
	r.EnableInjectGate()
	// Calling openInjectGate twice must not panic on close-of-closed-chan.
	r.openInjectGate("first")
	r.openInjectGate("second-should-be-noop")
	// Channel must be closed (post-condition: select must not block).
	select {
	case <-r.injectGate:
	default:
		t.Fatal("injectGate should be closed after openInjectGate")
	}
}

func TestRelay_OpenInjectGateNoopWhenGateDisabled(t *testing.T) {
	r, _ := pipeRelay(t)
	// No EnableInjectGate → injectGate is nil; openInjectGate must just
	// return without panicking.
	r.openInjectGate("disabled")
	if r.injectGate != nil {
		t.Error("openInjectGate must not allocate a gate when disabled")
	}
}

func TestRelay_WarmupPayloadFiresOnFirstGateOpen(t *testing.T) {
	r, rd := pipeRelay(t)
	r.EnableInjectGate()
	r.SetWarmupPayload([]byte("WARMUP"))

	r.openInjectGate("test")

	// Expect "WARMUP" + a "\r" submit kick.
	got := drainPipe(t, rd, 7)
	if string(got) != "WARMUP\r" {
		t.Errorf("got %q, want %q", got, "WARMUP\r")
	}
}

func TestRelay_WarmupPayloadOnlyFiresOnce(t *testing.T) {
	r, rd := pipeRelay(t)
	r.EnableInjectGate()
	r.SetWarmupPayload([]byte("X"))

	r.openInjectGate("first")
	first := drainPipe(t, rd, 2) // "X\r"
	if string(first) != "X\r" {
		t.Fatalf("first open got %q", first)
	}

	r.openInjectGate("second")
	// No new bytes from the warmup — the next write should be from a
	// fresh Inject call, not a re-fire of the warmup.
	if err := r.Inject([]byte("Y")); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	second := drainPipe(t, rd, 1)
	if string(second) != "Y" {
		t.Errorf("expected only Inject bytes, got %q", second)
	}
}

func TestRelay_NoteOutputOpensGateAfterQuietWindow(t *testing.T) {
	// We can't speed up the const injectGateQuietWindow (1500ms) without
	// editing source, but we can verify that the timer fires within a
	// generous bound and that the gate opens automatically afterwards.
	r, _ := pipeRelay(t)
	r.EnableInjectGate()

	r.noteOutput()
	// Allow ample headroom for slow CI runners.
	deadline := time.After(injectGateQuietWindow + 2*time.Second)
	for {
		select {
		case <-r.injectGate:
			return // gate opened
		case <-deadline:
			t.Fatal("inject gate did not open within quiet-window + slack")
		case <-time.After(100 * time.Millisecond):
			// keep polling
		}
	}
}

func TestRelay_NoteOutputIsNoopWhenGateDisabled(t *testing.T) {
	r, _ := pipeRelay(t)
	// gateInject defaults to false; calling noteOutput must not allocate
	// a timer or block.
	r.noteOutput()
	if r.injectGateTimer != nil {
		t.Error("noteOutput should not allocate a timer when gate disabled")
	}
}

func TestRelay_EnableInjectGateIdempotent(t *testing.T) {
	r, _ := pipeRelay(t)
	r.EnableInjectGate()
	first := r.injectGate
	r.EnableInjectGate()
	if r.injectGate != first {
		t.Error("EnableInjectGate must reuse the existing channel")
	}
}
