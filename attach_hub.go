//go:build darwin || linux

package main

import (
	"io"
	"sync"
)

// attachHub is the per-relay fan-out + replay buffer for
// `hearth agent attach`. Hangs off Relay. The PTY read goroutine
// calls Feed() with every chunk; each accepted attach connection
// receives a goroutine that drains the hub.
//
// Fan-out is a slice of channels, one per attached client. Each
// channel is buffered; if a slow client backs up beyond the buffer
// we drop bytes for THAT client (closing its channel) rather than
// blocking the PTY read loop. The phone WS path is independent (it
// reads the transcript JSONL via the streamer); attach starvation
// can't stall it.
//
// The ring buffer keeps the most recent N bytes for replay on
// connect. Sized to cover a status-line + a few lines of output;
// the user gets enough context to know what's on screen without
// the daemon needing to remember everything.
type attachHub struct {
	mu      sync.Mutex
	ring    *attachRing
	clients []*attachClient
}

type attachClient struct {
	ch     chan []byte
	closed bool
}

const (
	attachRingSize          = 64 * 1024
	attachClientBufferBytes = 64 * 1024
)

func newAttachHub() *attachHub {
	return &attachHub{ring: newAttachRing(attachRingSize)}
}

// Feed records PTY output into the ring buffer and broadcasts to
// every attached client. Called from the PTY read loop on every
// non-empty read. Safe for concurrent attach/detach.
func (h *attachHub) Feed(p []byte) {
	if len(p) == 0 {
		return
	}
	h.mu.Lock()
	h.ring.write(p)
	clients := h.clients
	h.mu.Unlock()
	// Copy outside the lock so a slow client can't block adds/removes.
	for _, c := range clients {
		if c.closed {
			continue
		}
		buf := make([]byte, len(p))
		copy(buf, p)
		select {
		case c.ch <- buf:
		default:
			// Slow consumer — drop. Closing the channel signals the
			// per-client goroutine to tear down its socket; daemon
			// resources reclaim themselves.
			h.removeClient(c)
		}
	}
}

// Attach registers a new client. Returns the channel the caller's
// writer goroutine should drain, plus the ring-buffer snapshot to
// flush before the first live byte. Detach() must be called to
// release resources.
func (h *attachHub) Attach() (*attachClient, []byte) {
	c := &attachClient{ch: make(chan []byte, 64)}
	h.mu.Lock()
	snapshot := h.ring.snapshot()
	h.clients = append(h.clients, c)
	h.mu.Unlock()
	return c, snapshot
}

// Detach removes a client and closes its channel. Idempotent.
func (h *attachHub) Detach(c *attachClient) {
	h.removeClient(c)
}

func (h *attachHub) removeClient(target *attachClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if target.closed {
		return
	}
	target.closed = true
	close(target.ch)
	out := h.clients[:0]
	for _, c := range h.clients {
		if c != target {
			out = append(out, c)
		}
	}
	h.clients = out
}

// CloseAll terminates every attached client. Called when the relay
// shuts down — the per-client goroutines see their channel close
// and exit, which closes their sockets.
func (h *attachHub) CloseAll() {
	h.mu.Lock()
	clients := h.clients
	h.clients = nil
	h.mu.Unlock()
	for _, c := range clients {
		if !c.closed {
			c.closed = true
			close(c.ch)
		}
	}
}

// attachRing is a fixed-capacity byte ring. write appends (possibly
// dropping older bytes); snapshot returns the current contents in
// chronological order. Single-writer assumed (always behind hub.mu).
type attachRing struct {
	buf  []byte
	pos  int  // next write index
	full bool // ring has wrapped at least once
}

func newAttachRing(capacity int) *attachRing {
	return &attachRing{buf: make([]byte, capacity)}
}

func (r *attachRing) write(p []byte) {
	// If the input is bigger than the ring, only the tail matters.
	if len(p) >= len(r.buf) {
		copy(r.buf, p[len(p)-len(r.buf):])
		r.pos = 0
		r.full = true
		return
	}
	n := copy(r.buf[r.pos:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
		r.full = true
	}
	r.pos = (r.pos + len(p)) % len(r.buf)
	if r.pos == 0 || r.full {
		// If we wrote to or wrapped past the end, the ring is full
		// (or already was). Pos==0 immediately after a perfectly-
		// aligned write also means "we just wrapped."
		if n < len(p) {
			r.full = true
		}
	}
}

func (r *attachRing) snapshot() []byte {
	if !r.full {
		out := make([]byte, r.pos)
		copy(out, r.buf[:r.pos])
		return out
	}
	out := make([]byte, len(r.buf))
	copy(out, r.buf[r.pos:])
	copy(out[len(r.buf)-r.pos:], r.buf[:r.pos])
	return out
}

// writeAll wraps io.Writer.Write so the caller doesn't have to
// retry on partial writes. Used by the per-client goroutines.
func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}
