//go:build darwin || linux

package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"time"
)

// sessionIDForReplay returns the harness-internal session id the
// replay lookup should use, given the registered AgentWS. When non-
// empty it routes deriveTranscriptPath through the deterministic
// by-id path; empty falls back to the cwd/newest-on-disk lookup,
// which is correct only for harnesses where AgentSessionID is
// always empty by design (gemini today). Without this, the replay
// could surface a different agent's transcript when multiple
// sessions share a cwd or session-state dir.
func sessionIDForReplay(runtime, aiAgentInstanceID, agentSessionID string) string {
	_ = runtime
	_ = aiAgentInstanceID
	return agentSessionID
}

// replayTranscriptHistory reads the agent's on-disk JSONL transcript (last
// `limit` entries, default 500) and re-emits each line as a `transcript`
// frame on the agent's WebSocket. The server's existing transcript
// processing pipeline then forwards the entries to the requesting device
// just like live tail does — same dedup, same envelope-aware rendering on
// the client.
//
// No-op if the agent isn't registered locally or if no transcript file has
// been created yet (e.g. an agent that hasn't seen its first turn). Errors
// are logged but never bubbled — the client's empty-state UI handles the
// "no history available" case implicitly.
func (d *DaemonWS) replayTranscriptHistory(aiAgentInstanceID string, limit int) {
	d.mu.RLock()
	aw := d.instances[aiAgentInstanceID]
	d.mu.RUnlock()
	if aw == nil {
		log.Printf("daemon-ws: history request for unknown instance %s", aiAgentInstanceID)
		return
	}

	if limit <= 0 || limit > 5000 {
		limit = 500
	}

	// aw.agent stores the server-side label (e.g. "claude-code"), set by
	// agentServerName() at registration. The local helpers below speak
	// the runtime name (e.g. "claude"), so translate once.
	runtime := runtimeAgentFromServerName(aw.agent)

	h, ok := getHarness(runtime)
	if !ok {
		log.Printf("daemon-ws: history skipped for instance %s — agent=%s not yet supported", aiAgentInstanceID, aw.agent)
		return
	}
	xform := h.NewStreamTransformer()

	// Poll briefly for the transcript file to appear. iOS fires the
	// history request as soon as it opens the transcript view, which can
	// be before the agent has written its first turn (codex in particular
	// only creates the rollout JSONL on the first user message). Without
	// the wait, we'd return empty and iOS would never re-ask.
	const (
		maxWait  = 30 * time.Second
		pollEach = 250 * time.Millisecond
	)
	deadline := time.Now().Add(maxWait)
	var transcriptPath string
	for {
		transcriptPath = deriveTranscriptPath(runtime, sessionIDForReplay(runtime, aiAgentInstanceID, aw.agentSessionID), aw.cwd)
		if transcriptPath != "" {
			if _, err := os.Stat(transcriptPath); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			log.Printf("daemon-ws: history: no transcript file for instance %s after %s (agent=%s cwd=%s)", aiAgentInstanceID, maxWait, aw.agent, aw.cwd)
			return
		}
		time.Sleep(pollEach)
	}

	lines, err := readLastNLines(transcriptPath, limit)
	if err != nil {
		log.Printf("daemon-ws: history read failed for %s (%s): %v", aiAgentInstanceID, transcriptPath, err)
		return
	}
	if len(lines) == 0 {
		return
	}

	agentLabel := aw.agent // already the server-side label
	emitted := 0
	for _, line := range lines {
		for _, transformed := range xform.TransformLine(line) {
			if len(transformed) == 0 {
				continue
			}
			// Match the live-tail wire format in bridge.go so the server
			// processes both replay and live entries through identical
			// code.
			frame := fmt.Sprintf(`{"type":"transcript","agent":%q,"data":%s}`, agentLabel, string(transformed))
			aw.SendText([]byte(frame))
			emitted++
		}
	}
	log.Printf("daemon-ws: replayed %d history entries for instance %s (read %d, agent=%s)", emitted, aiAgentInstanceID, len(lines), aw.agent)
}

// runtimeAgentFromServerName inverts agentServerName so we can recover the
// runtime label (used by deriveTranscriptPath et al.) from the server-side
// label stored on agentWS. Unknown / already-runtime values pass through
// so the function is forgiving of either form.
func runtimeAgentFromServerName(name string) string {
	switch name {
	case "claude-code":
		return "claude"
	default:
		return name
	}
}

// readLastNLines streams the file once with a bufio.Reader and keeps a
// fixed-size ring buffer so memory stays bounded regardless of how big the
// JSONL grew. JSONL lines from claude/codex tool_results can hit several
// hundred KB each, so the underlying reader buffer is sized generously.
func readLastNLines(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 1<<16)
	ring := make([]string, n)
	head := 0
	count := 0

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			ring[head] = trimNewline(line)
			head = (head + 1) % n
			if count < n {
				count++
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}

	out := make([]string, count)
	start := 0
	if count == n {
		start = head
	}
	for i := 0; i < count; i++ {
		out[i] = ring[(start+i)%n]
	}
	return out, nil
}
