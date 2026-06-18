//go:build darwin || linux

package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
)

// Per-agent-instance unix socket that powers `hearth agent attach`.
// Created when an agent spawns (claude only for v1 — see
// attachSupportedForAgent); torn down on agent exit.
//
// Frame protocol is minimal: the client sends a single-line ASCII
// handshake first ("ATTACH ro\n" or "ATTACH rw\n"), the daemon
// responds with "OK\n" + the ring-buffer snapshot, and from then
// on the connection is raw bytes both directions:
//   - server → client: live PTY output
//   - client → server: typed input, dropped server-side unless the
//     handshake declared "rw"
// Detach is just a socket close on either end.
//
// Per-harness attach support is declared on the Harness SPI via
// SupportsAttach(); see harness_iface.go. Adapters start at false
// and flip true once their input loop has been smoke-tested against
// the InjectRaw write-mode path (codex's bracketed-paste gate, for
// instance, may need bespoke handling first).

// attachSupportedForAgent returns true if the harness adapter has
// declared itself attach-capable on the SPI.
func attachSupportedForAgent(agent string) bool {
	h, ok := getHarness(agent)
	return ok && h.SupportsAttach()
}

// attachSocketPath returns the per-instance unix socket path. /tmp
// is used over $XDG_RUNTIME_DIR for path-length headroom (macOS
// caps unix socket paths at ~104 bytes). Per-uid in the filename so
// two users on the same host don't collide.
func attachSocketPath(instanceID string) string {
	return fmt.Sprintf("/tmp/hearth-attach-%d-%s.sock", os.Getuid(), instanceID)
}

// startAttachListener stands up the per-instance socket and begins
// accepting attach connections. The returned cancel func closes
// the listener (which races accepted connections to drain) and
// removes the socket file. Called from newAgentInstance for
// claude-shaped agents.
func startAttachListener(instance *AgentInstance) (cancel func(), err error) {
	if instance.relay == nil || instance.relay.attachHub == nil {
		return nil, fmt.Errorf("attach: relay hub not initialized")
	}
	sockPath := attachSocketPath(instance.aiAgentInstanceID)
	// Stale file from a previous crash — remove. Same-uid only by
	// the path convention, so we're not stomping anyone else's socket.
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("attach: listen %s: %w", sockPath, err)
	}
	// 0600 — same-uid-only is the security boundary, matching the
	// main daemon socket convention.
	if err := os.Chmod(sockPath, 0o600); err != nil {
		ln.Close()
		os.Remove(sockPath)
		return nil, fmt.Errorf("attach: chmod %s: %w", sockPath, err)
	}
	log.Printf("daemon: attach socket → %s", sockPath)
	go acceptAttachLoop(instance, ln)
	return func() {
		ln.Close()
		os.Remove(sockPath)
		if instance.relay != nil && instance.relay.attachHub != nil {
			instance.relay.attachHub.CloseAll()
		}
	}, nil
}

func acceptAttachLoop(instance *AgentInstance, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Listener closed (clean shutdown) or platform error.
			// Either way we stop accepting.
			return
		}
		go handleAttachConn(instance, conn)
	}
}

func handleAttachConn(instance *AgentInstance, conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	hdr, err := br.ReadString('\n')
	if err != nil {
		log.Printf("daemon: attach: read handshake: %v", err)
		return
	}
	hdr = strings.TrimSpace(hdr)
	allowWrite := false
	switch hdr {
	case "ATTACH ro":
		allowWrite = false
	case "ATTACH rw":
		allowWrite = true
	default:
		log.Printf("daemon: attach: bad handshake %q", hdr)
		_, _ = conn.Write([]byte("ERR bad handshake\n"))
		return
	}
	if _, err := conn.Write([]byte("OK\n")); err != nil {
		return
	}

	client, snapshot := instance.relay.attachHub.Attach()
	defer instance.relay.attachHub.Detach(client)

	// Flush the ring buffer so the attacher sees the current screen
	// state immediately. After this point the writer goroutine
	// drains live bytes off the hub.
	if len(snapshot) > 0 {
		if err := writeAll(conn, snapshot); err != nil {
			return
		}
	}

	// Writer: hub → socket. Exits when the hub closes the channel
	// (relay shutdown, hub disconnect, slow-client drop) or when
	// the socket write fails.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for chunk := range client.ch {
			if err := writeAll(conn, chunk); err != nil {
				return
			}
		}
	}()

	// Reader: socket → PTY (write mode) or /dev/null (read-only).
	// Either way we read so we notice client disconnect. Anything
	// goes wrong on the socket → return, which triggers Detach + the
	// writer goroutine's channel-closed exit.
	buf := make([]byte, 4096)
	for {
		n, err := br.Read(buf)
		if n > 0 && allowWrite {
			if werr := instance.relay.InjectRaw(buf[:n]); werr != nil {
				log.Printf("daemon: attach: InjectRaw failed: %v", werr)
				return
			}
		}
		if err != nil {
			break
		}
	}
	// Wait for the writer goroutine to wind down so we don't double-
	// close the conn.
	<-writerDone
}
