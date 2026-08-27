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

// transcriptFrame builds the `transcript` WebSocket frame that carries one
// already-JSON transcript line. It is the SINGLE place the frame is
// constructed so the live-tail path (here) and the history-replay path
// (replayTranscriptHistory) cannot drift.
//
// retry marks a re-send of an entry the server has already processed once —
// i.e. a history replay for a newly-connected io_device rebuilding its
// transcript replica. The relay keys one-shot side effects off this flag,
// chiefly the @mention native push: it fires only for a live (retry=false)
// entry, never a replay. The live tail passes retry=false; replay passes
// retry=true. A divergence here previously re-pushed the same @mention on
// every replay (once per device open / reconnect) — see
// processTranscriptEntry / maybePushMentions in the relay.
func transcriptFrame(agent, dataLine string, retry bool) []byte {
	if retry {
		return []byte(fmt.Sprintf(`{"type":"transcript","retry":true,"agent":%q,"data":%s}`, agent, dataLine))
	}
	return []byte(fmt.Sprintf(`{"type":"transcript","agent":%q,"data":%s}`, agent, dataLine))
}

// transcriptObserver is implemented by *agentWS. The bridge tail already reads
// every line of the agent's transcript, which makes it the one place that sees
// turn boundaries and knows what became of an injected payload — so the
// readiness tracker and the inbox's confirmation oracle both feed from here.
// Optional: the direct-WS and test paths don't implement it.
type transcriptObserver interface {
	ObserveTranscriptLine(line []byte)
}

// tailBridge tails the bridge file and sends each line over the WebSocket
// as a JSON transcript message. Blocks until done is closed or an error occurs.
// After done is closed, drains any remaining lines before returning.
func tailBridge(path string, ws WSConn, done <-chan struct{}, agent string) {
	observer, _ := ws.(transcriptObserver)
	observe := func(line string) {
		if observer != nil {
			observer.ObserveTranscriptLine([]byte(line))
		}
	}
	log.Printf("bridge: starting tail for %s (agent=%s)", path, agent)
	// Wait for the bridge file to appear (hook creates it)
	var f *os.File
	for {
		select {
		case <-done:
			log.Printf("bridge: done before file appeared: %s", path)
			return
		default:
		}
		var err error
		f, err = os.Open(path)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	defer f.Close()
	log.Printf("bridge: opened %s", path)

	// Seek to end — no backfill, fresh agent instance
	f.Seek(0, io.SeekEnd)

	reader := bufio.NewReader(f)
	var partial string
	var linesSent int
	stopping := false
	for {
		if stopping {
			// Drain pass: give the streamer a moment to finish writing,
			// then read and send all remaining complete lines.
			time.Sleep(500 * time.Millisecond)
			for {
				line, err := reader.ReadString('\n')
				if err == nil {
					// Complete line (delimiter found)
					fullLine := trimNewline(partial + line)
					partial = ""
					if fullLine != "" {
						observe(fullLine)
						ws.SendText(transcriptFrame(agent, fullLine, false))
					}
				} else {
					// EOF or error — send any remaining buffered partial
					if partial != "" {
						observe(partial)
						ws.SendText(transcriptFrame(agent, partial, false))
					}
					return
				}
			}
		}

		select {
		case <-done:
			stopping = true
			continue
		default:
		}

		line, err := reader.ReadString('\n')
		if err == nil {
			// Complete line (delimiter found) — safe to send
			fullLine := trimNewline(partial + line)
			partial = ""
			if fullLine != "" {
				observe(fullLine)
				frame := transcriptFrame(agent, fullLine, false)
				ws.SendText(frame)
				linesSent++
				if linesSent == 1 {
					log.Printf("bridge: first transcript line sent (%d bytes)", len(frame))
				}
			}
		} else if line != "" {
			// Partial line (no newline yet) — buffer it
			partial += line
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("bridge: read error: %v", err)
				return
			}
			// EOF — wait for more data
			time.Sleep(100 * time.Millisecond)
		}
	}
}
