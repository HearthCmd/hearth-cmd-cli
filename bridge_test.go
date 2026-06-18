//go:build darwin || linux

package main

// Coverage for bridge.go's tailBridge. The function tails an agent's
// hook bridge file, framing each complete line as a transcript JSON
// frame for the WebSocket. The interesting behaviors:
//
//   - waits for the file to appear before reading (the harness creates
//     it lazily — without this the daemon would race and miss lines)
//   - frames complete-line-only (a partial line buffer flushes on
//     drain); guarantees we never split a JSON object mid-stream
//   - drains remaining lines after `done` is closed before returning
//   - emits framing of {"type":"transcript","agent":...,"data":<line>}

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubWS captures every SendText payload. Implements WSConn so we can
// drive tailBridge end-to-end without a real socket.
type stubWS struct {
	mu    sync.Mutex
	sends [][]byte
}

func (s *stubWS) SendText(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	s.sends = append(s.sends, cp)
}

func (s *stubWS) Send(data []byte)                              { s.SendText(data) }
func (s *stubWS) RegisterPending(_ string) <-chan []byte        { return nil }
func (s *stubWS) RemovePending(_ string)                        {}
func (s *stubWS) Close()                                        {}

func (s *stubWS) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.sends))
	for i, b := range s.sends {
		out[i] = string(b)
	}
	return out
}

func TestTailBridge_FramesCompleteLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.log")

	// Pre-create the file so tailBridge stops polling immediately.
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}

	ws := &stubWS{}
	done := make(chan struct{})
	go tailBridge(path, ws, done, "claude")

	// Append two complete JSON lines after a tiny delay so tailBridge has
	// time to seek-to-end + start its read loop.
	time.Sleep(50 * time.Millisecond)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"k":1}` + "\n" + `{"k":2}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Wait long enough for the 100ms EOF poll to read both lines.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(ws.snapshot()) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	close(done)
	// Drain pass adds 500ms; give it some headroom.
	time.Sleep(700 * time.Millisecond)

	got := ws.snapshot()
	if len(got) < 2 {
		t.Fatalf("expected at least 2 frames, got %d (%v)", len(got), got)
	}

	for i, frame := range got[:2] {
		var msg struct {
			Type  string          `json:"type"`
			Agent string          `json:"agent"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(frame), &msg); err != nil {
			t.Errorf("frame %d not valid JSON: %v (%q)", i, err, frame)
			continue
		}
		if msg.Type != "transcript" {
			t.Errorf("frame %d type = %q, want transcript", i, msg.Type)
		}
		if msg.Agent != "claude" {
			t.Errorf("frame %d agent = %q, want claude", i, msg.Agent)
		}
		// Data must be the original line (without trailing newline).
		var inner map[string]int
		if err := json.Unmarshal(msg.Data, &inner); err != nil {
			t.Errorf("frame %d data not parseable JSON: %q", i, msg.Data)
		}
	}
}

func TestTailBridge_DoneBeforeFileAppearsExits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "never-created.log")

	ws := &stubWS{}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		tailBridge(path, ws, done, "claude")
		close(finished)
	}()

	close(done)
	select {
	case <-finished:
		// returned promptly
	case <-time.After(2 * time.Second):
		t.Fatal("tailBridge did not exit when done closed before file appeared")
	}
	if got := ws.snapshot(); len(got) != 0 {
		t.Errorf("expected no sends when file never appeared, got %v", got)
	}
}

func TestTailBridge_DrainsBufferedPartialOnDone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.log")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}

	ws := &stubWS{}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		tailBridge(path, ws, done, "claude")
		close(finished)
	}()

	// Write a complete line then a trailing partial (no newline). The
	// drain pass on shutdown should still emit the partial.
	time.Sleep(50 * time.Millisecond)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	_, _ = f.WriteString(`{"a":1}` + "\n")
	_, _ = f.WriteString(`{"b":2}`) // no trailing \n
	f.Close()

	// Give the read loop a beat to see the complete line.
	time.Sleep(300 * time.Millisecond)
	close(done)

	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("tailBridge did not return after drain")
	}

	got := ws.snapshot()
	if len(got) < 2 {
		t.Fatalf("expected complete + partial frames, got %d", len(got))
	}
	// Last frame should carry the un-newlined partial.
	last := got[len(got)-1]
	if !strings.Contains(last, `"b":2`) {
		t.Errorf("expected partial line drained, last frame = %q", last)
	}
}
