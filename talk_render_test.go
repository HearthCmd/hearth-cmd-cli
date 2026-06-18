//go:build darwin || linux

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Pure render helpers in talk_render.go. lipgloss outputs ANSI escape
// codes when the terminal supports color; under `go test` the
// underlying termenv detector treats the non-TTY stdout as monochrome,
// so we mostly assert with strings.Contains on the visible substring.

func TestParseHearthEnvelope(t *testing.T) {
	t.Run("no envelope", func(t *testing.T) {
		body, name := parseHearthEnvelope("plain message")
		if body != "plain message" || name != "" {
			t.Errorf("got body=%q name=%q", body, name)
		}
	})

	t.Run("strips header and extracts from.name", func(t *testing.T) {
		text := `hearth/1 {"from":{"name":"Bob"}}` + "\n\nactual body"
		body, name := parseHearthEnvelope(text)
		if body != "actual body" {
			t.Errorf("body = %q", body)
		}
		if name != "Bob" {
			t.Errorf("name = %q", name)
		}
	})

	t.Run("leading whitespace tolerated", func(t *testing.T) {
		text := "\n  \thearth/1 {\"from\":{\"name\":\"Alice\"}}\n\nhi"
		body, name := parseHearthEnvelope(text)
		if body != "hi" || name != "Alice" {
			t.Errorf("body=%q name=%q", body, name)
		}
	})

	t.Run("malformed (no double-newline) returns input", func(t *testing.T) {
		text := `hearth/1 {"from":{"name":"X"}} no break`
		body, name := parseHearthEnvelope(text)
		if body != text || name != "" {
			t.Errorf("body=%q name=%q", body, name)
		}
	})

	t.Run("unparseable JSON header still yields body, empty name", func(t *testing.T) {
		text := "hearth/1 {bogus\n\nbody"
		body, name := parseHearthEnvelope(text)
		if body != "body" || name != "" {
			t.Errorf("body=%q name=%q", body, name)
		}
	})
}

func TestUserAndAgentPrefix(t *testing.T) {
	if !strings.Contains(userPrefix("alice"), "alice") {
		t.Error("userPrefix should include the name")
	}
	if !strings.Contains(userPrefix(""), "you") {
		t.Error("userPrefix empty should fall back to 'you'")
	}
	if got := agentPrefix(""); got != "" {
		t.Errorf("agentPrefix empty should be empty, got %q", got)
	}
	if !strings.Contains(agentPrefix("Cody"), "Cody") {
		t.Error("agentPrefix should include the name")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := truncate("with\nnewlines", 100); got != "with newlines" {
		t.Errorf("newlines should be replaced: got %q", got)
	}
	got := truncate("abcdefghij", 5)
	if got != "abcde…" {
		t.Errorf("got %q", got)
	}
}

func TestContentToString(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
	}{
		{"empty", "", ""},
		{"plain string", `"  hello  "`, "hello"},
		{"nested text blocks", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a b"},
		{"non-text blocks ignored", `[{"type":"image","text":"x"}]`, ""},
		{"unparseable falls back to raw JSON", `{"k":1}`, `{"k":1}`},
	}
	for _, tc := range cases {
		got := contentToString(json.RawMessage(tc.input))
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSummarizeToolInput(t *testing.T) {
	cases := []struct {
		tool, input, want string
	}{
		{"Bash", `{"command":"ls -la"}`, "$ ls -la"},
		{"Read", `{"file_path":"/tmp/x"}`, "/tmp/x"},
		{"Write", `{"file_path":"/tmp/x","content":"y"}`, "/tmp/x"},
		{"Edit", `{"file_path":"/a/b"}`, "/a/b"},
		{"Glob", `{"pattern":"**/*.go"}`, "**/*.go"},
		{"Grep", `{"pattern":"func main"}`, "func main"},
		{"WebFetch", `{"url":"https://x"}`, "https://x"},
		{"WebSearch", `{"query":"go testing"}`, "go testing"},
		{"TodoWrite", `{"todos":[{"text":"a"}]}`, "(todos updated)"},
	}
	for _, tc := range cases {
		got := summarizeToolInput(tc.tool, json.RawMessage(tc.input))
		if got != tc.want {
			t.Errorf("%s(%s): got %q, want %q", tc.tool, tc.input, got, tc.want)
		}
	}

	t.Run("unknown tool truncates raw JSON", func(t *testing.T) {
		got := summarizeToolInput("Mystery", json.RawMessage(`{"a":1,"b":2}`))
		if !strings.Contains(got, `"a":1`) {
			t.Errorf("got %q", got)
		}
	})

	t.Run("empty input returns empty", func(t *testing.T) {
		if got := summarizeToolInput("Bash", nil); got != "" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("unparseable JSON falls back to truncated raw", func(t *testing.T) {
		got := summarizeToolInput("Bash", json.RawMessage(`not json`))
		if got != "not json" {
			t.Errorf("got %q", got)
		}
	})
}

func TestRenderDecomposedTranscript(t *testing.T) {
	t.Run("user with envelope name", func(t *testing.T) {
		text := "hearth/1 {\"from\":{\"name\":\"Bob\"}}\n\nhi there"
		out := renderDecomposedTranscript("user", text, "", "", nil, "alice", "cody")
		if !strings.Contains(out, "Bob") {
			t.Errorf("envelope name should win: %q", out)
		}
		if !strings.Contains(out, "hi there") {
			t.Errorf("body missing: %q", out)
		}
	})

	t.Run("user without envelope falls back to userName", func(t *testing.T) {
		out := renderDecomposedTranscript("user", "raw message", "", "", nil, "alice", "")
		if !strings.Contains(out, "alice") {
			t.Errorf("got %q", out)
		}
		if !strings.Contains(out, "raw message") {
			t.Errorf("got %q", out)
		}
	})

	t.Run("user with empty body returns empty", func(t *testing.T) {
		// envelope present but body section empty
		text := "hearth/1 {}\n\n"
		out := renderDecomposedTranscript("user", text, "", "", nil, "alice", "")
		if out != "" {
			t.Errorf("expected empty for blank body, got %q", out)
		}
	})

	t.Run("text event with agent name", func(t *testing.T) {
		out := renderDecomposedTranscript("text", "hello", "", "", nil, "", "cody")
		if !strings.Contains(out, "cody") || !strings.Contains(out, "hello") {
			t.Errorf("got %q", out)
		}
	})

	t.Run("text event without agent name returns plain text", func(t *testing.T) {
		out := renderDecomposedTranscript("text", "hello", "", "", nil, "", "")
		if !strings.Contains(out, "hello") {
			t.Errorf("got %q", out)
		}
	})

	t.Run("tool_use prefixes gear glyph", func(t *testing.T) {
		out := renderDecomposedTranscript("tool_use", "", "Bash", "$ ls", nil, "", "")
		if !strings.Contains(out, "⚙") || !strings.Contains(out, "Bash") || !strings.Contains(out, "ls") {
			t.Errorf("got %q", out)
		}
	})

	t.Run("tool_result prefixes check", func(t *testing.T) {
		out := renderDecomposedTranscript("tool_result", "", "", "ok", nil, "", "")
		if !strings.Contains(out, "ok") {
			t.Errorf("got %q", out)
		}
	})

	t.Run("unknown event returns empty", func(t *testing.T) {
		if out := renderDecomposedTranscript("mystery", "x", "", "", nil, "", ""); out != "" {
			t.Errorf("got %q", out)
		}
	})
}

func TestRenderClaudeMessageFromBlocks(t *testing.T) {
	content := json.RawMessage(`[
		{"type":"text","text":"hello"},
		{"type":"tool_use","name":"Bash","input":{"command":"ls"}},
		{"type":"tool_result","content":"done"},
		{"type":"thinking","text":"hmm"}
	]`)
	out := renderClaudeMessage("assistant", content, "u", "cody")
	if !strings.Contains(out, "hello") {
		t.Errorf("text missing: %q", out)
	}
	if !strings.Contains(out, "Bash") || !strings.Contains(out, "ls") {
		t.Errorf("tool_use missing: %q", out)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("tool_result missing: %q", out)
	}
	if strings.Contains(out, "hmm") {
		t.Errorf("thinking should be skipped: %q", out)
	}
}

func TestRenderClaudeMessageFromString(t *testing.T) {
	out := renderClaudeMessage("assistant", json.RawMessage(`"plain"`), "", "cody")
	if !strings.Contains(out, "plain") || !strings.Contains(out, "cody") {
		t.Errorf("got %q", out)
	}
	out = renderClaudeMessage("user", json.RawMessage(`"hi"`), "alice", "")
	if !strings.Contains(out, "alice") || !strings.Contains(out, "hi") {
		t.Errorf("got %q", out)
	}
	// Empty after trim → empty out.
	if out := renderClaudeMessage("assistant", json.RawMessage(`"  "`), "", ""); out != "" {
		t.Errorf("blank text should produce empty output: %q", out)
	}
}

func TestRenderTranscriptEntryFallback(t *testing.T) {
	// Non-claude shape — should fall back to faint dump containing the JSON.
	out := renderTranscriptEntry(json.RawMessage(`{"weird":"shape"}`), "", "")
	if !strings.Contains(out, "weird") {
		t.Errorf("expected raw JSON in fallback: %q", out)
	}
	if got := renderTranscriptEntry(nil, "", ""); got != "" {
		t.Errorf("empty data should yield empty out, got %q", got)
	}
}

func TestRenderActivityEvent(t *testing.T) {
	out := renderActivityEvent("tool_use", "Bash", "myproj")
	for _, want := range []string{"·", "tool_use", "Bash", "[myproj]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	// Empty fields are skipped — should still render the leading dot.
	out = renderActivityEvent("", "", "")
	if !strings.Contains(out, "·") {
		t.Errorf("got %q", out)
	}
}

func TestRenderMissedRequest(t *testing.T) {
	out := renderMissedRequest("Bash", "myproj", json.RawMessage(`{"command":"ls"}`))
	for _, want := range []string{"missed", "Bash", "ls", "[myproj]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	// Without preview / project, just shows the labels.
	out = renderMissedRequest("Custom", "", nil)
	if !strings.Contains(out, "missed") || !strings.Contains(out, "Custom") {
		t.Errorf("got %q", out)
	}
}

func TestRenderPills(t *testing.T) {
	t.Run("empty list shows hint", func(t *testing.T) {
		out := renderPills(nil, "")
		if !strings.Contains(out, "no active") {
			t.Errorf("got %q", out)
		}
	})

	t.Run("focused vs unfocused glyph", func(t *testing.T) {
		instances := []talkInstance{
			{aiAgentInstanceID: "a", project: "p1", pidStatus: "running"},
			{aiAgentInstanceID: "b", project: "p2", pidStatus: "running"},
		}
		out := renderPills(instances, "a")
		if !strings.Contains(out, "p1") || !strings.Contains(out, "p2") {
			t.Errorf("got %q", out)
		}
		// Focused running instance shows ●, unfocused shows ○.
		if !strings.Contains(out, "●") {
			t.Errorf("expected ● for focused running: %q", out)
		}
		if !strings.Contains(out, "○") {
			t.Errorf("expected ○ for unfocused running: %q", out)
		}
	})

	t.Run("starting status shows reload glyph", func(t *testing.T) {
		instances := []talkInstance{
			{aiAgentInstanceID: "a", project: "p1", pidStatus: "never_spawned"},
		}
		out := renderPills(instances, "a")
		if !strings.Contains(out, "⟳") {
			t.Errorf("expected ⟳ for starting state: %q", out)
		}
	})

	t.Run("empty project label falls back to 'agent'", func(t *testing.T) {
		instances := []talkInstance{{aiAgentInstanceID: "a", project: "", pidStatus: "running"}}
		out := renderPills(instances, "")
		if !strings.Contains(out, "agent") {
			t.Errorf("got %q", out)
		}
	})
}

func TestStatusHints(t *testing.T) {
	// Coverage smoke-test — exact formatting is style-dependent, but the
	// returned string should be non-empty for the typical states.
	if statusHints(false, false) == "" {
		t.Error("default state should produce hints")
	}
	// Modal owns its own hint line — statusHints intentionally yields "".
	if statusHints(true, false) != "" {
		t.Error("modal state should suppress status hints")
	}
	if statusHints(false, true) == "" {
		t.Error("help state should produce hints")
	}
}
