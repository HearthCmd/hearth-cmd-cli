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

	lines, baseIndex, err := readLastNLinesIndexed(transcriptPath, limit)
	if err != nil {
		log.Printf("daemon-ws: history read failed for %s (%s): %v", aiAgentInstanceID, transcriptPath, err)
		return
	}
	if len(lines) == 0 {
		return
	}

	agentLabel := aw.agent // already the server-side label
	emitted := 0
	for i, line := range lines {
		// baseIndex+i is the line's absolute 0-based position in the file —
		// the same `seq` the live tail assigns to this entry, so the two
		// paths' entries dedup and order identically on the client.
		for _, transformed := range transformLineWithSeq(xform, line, baseIndex+i) {
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
	lines, _, err := readLastNLinesIndexed(path, n)
	return lines, err
}

// readLastNLinesIndexed is readLastNLines plus baseIndex — the absolute 0-based
// index (from the top of the file) of the first returned line. The replay path
// needs it to stamp each replayed entry with the same source-line `seq` the
// live tail assigns, so live and replay agree. It counts every line the same
// way tailAndPump does — one per logical line, from the top — so the two
// paths' ordinals cannot drift.
func readLastNLinesIndexed(path string, n int) ([]string, int, error) {
	if n <= 0 {
		return nil, 0, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 1<<16)
	ring := make([]string, n)
	head := 0
	count := 0
	total := 0 // every line seen, for the absolute base index

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			ring[head] = trimNewline(line)
			head = (head + 1) % n
			if count < n {
				count++
			}
			total++
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, 0, err
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
	// The last `count` of `total` lines are returned; out[0] is line
	// (total-count) counting from 0.
	return out, total - count, nil
}
