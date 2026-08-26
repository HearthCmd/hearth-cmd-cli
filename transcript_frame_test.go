//go:build darwin || linux

package main

// Cover the daemon half of the duplicate-@mention-notification fix: replayed
// history frames MUST carry retry:true so the relay suppresses the mention
// push, while live-tail frames MUST NOT (they are the one legitimate send).
// The two paths now share transcriptFrame() so they can't drift again.

import (
	"encoding/json"
	"testing"
)

// decodeFrame is the relay-side view of a transcript frame (mirrors the daemon
// frame reader in relay main.go): type + retry + agent + data.
type decodeFrame struct {
	Type  string          `json:"type"`
	Retry bool            `json:"retry"`
	Agent string          `json:"agent"`
	Data  json.RawMessage `json:"data"`
}

func TestTranscriptFrame_RetryTagging(t *testing.T) {
	const data = `{"k":1}`

	// Replay → retry:true, and the relay must decode Retry == true.
	var replay decodeFrame
	if err := json.Unmarshal(transcriptFrame("claude-code", data, true), &replay); err != nil {
		t.Fatalf("replay frame invalid JSON: %v", err)
	}
	if replay.Type != "transcript" || replay.Agent != "claude-code" {
		t.Errorf("replay frame type/agent = %q/%q", replay.Type, replay.Agent)
	}
	if !replay.Retry {
		t.Error("replay frame must be retry:true")
	}
	if string(replay.Data) != data {
		t.Errorf("replay frame data = %s, want %s", replay.Data, data)
	}

	// Live → retry absent, so the relay decodes Retry == false and fires the
	// push exactly once.
	var live decodeFrame
	if err := json.Unmarshal(transcriptFrame("claude-code", data, false), &live); err != nil {
		t.Fatalf("live frame invalid JSON: %v", err)
	}
	if live.Retry {
		t.Error("live frame must NOT be retry:true (would suppress a genuine push)")
	}
}

// echoXform emits each non-empty line as one entry; "MULTI" fans into two so we
// verify every entry from a multi-entry line is still tagged retry:true.
type echoXform struct{}

func (echoXform) TransformLine(line string) [][]byte {
	switch line {
	case "":
		return nil
	case "MULTI":
		return [][]byte{[]byte(`{"e":1}`), []byte(`{"e":2}`)}
	default:
		return [][]byte{[]byte(line)}
	}
}

func TestEmitReplayFrames_EveryFrameRetryTrue(t *testing.T) {
	var sent [][]byte
	send := func(b []byte) {
		cp := make([]byte, len(b))
		copy(cp, b)
		sent = append(sent, cp)
	}

	// One empty (yields nothing), one single-entry, one multi-entry line.
	lines := []string{`{"k":1}`, "", "MULTI"}
	n := emitReplayFrames(echoXform{}, "claude-code", lines, 0, send)

	if n != 3 { // {"k":1} → 1, "" → 0, MULTI → 2
		t.Fatalf("emitted = %d, want 3", n)
	}
	if len(sent) != 3 {
		t.Fatalf("captured %d frames, want 3", len(sent))
	}
	for i, b := range sent {
		var f decodeFrame
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("frame %d invalid JSON: %v (%s)", i, err, b)
		}
		if f.Type != "transcript" {
			t.Errorf("frame %d type = %q, want transcript", i, f.Type)
		}
		if !f.Retry {
			t.Errorf("frame %d is not retry:true — replays MUST be tagged so the relay skips the push (%s)", i, b)
		}
	}
}
