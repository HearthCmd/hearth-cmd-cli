//go:build darwin || linux

package main

import (
	"strings"
	"testing"
)

// Slack-style threads: `hearth chat reply --thread / --channel` and the
// thread-aware mention prompt (docs/chat-threads-plan.md, phase 3).

func TestChatReplyPayload(t *testing.T) {
	base := ipcRequest{ChatRoomID: "room1", ChatAgentInstanceID: "agent1", ChatText: "hi"}

	// No flag: no thread field, so the relay places it where the agent was asked.
	p := chatReplyPayload(base)
	if _, ok := p["progress"]; ok {
		t.Errorf("plain reply carried progress: %v", p)
	}
	withProgress := base
	withProgress.ChatProgress = true
	if pp := chatReplyPayload(withProgress); pp["progress"] != true {
		t.Errorf("--progress payload = %v", pp)
	}
	if _, ok := p["thread_root_id"]; ok {
		t.Errorf("plain reply carried thread_root_id: %v", p)
	}
	if _, ok := p["channel"]; ok {
		t.Errorf("plain reply carried channel: %v", p)
	}
	if p["room_id"] != "room1" || p["ai_agent_instance_id"] != "agent1" || p["text"] != "hi" {
		t.Errorf("base fields wrong: %v", p)
	}

	// --thread.
	withThread := base
	withThread.ChatThreadRootID = "root1"
	if p := chatReplyPayload(withThread); p["thread_root_id"] != "root1" {
		t.Errorf("--thread payload = %v", p)
	}

	// --channel.
	withChannel := base
	withChannel.ChatChannel = true
	if p := chatReplyPayload(withChannel); p["channel"] != true {
		t.Errorf("--channel payload = %v", p)
	}
}

func TestBuildChatMentionPrompt_Channel(t *testing.T) {
	out := string(buildChatMentionPrompt("room1", "", "Dana", "@agent thoughts?", []string{"[Sam]: hello"}))
	if !strings.HasPrefix(out, "hearth/1 ") {
		t.Errorf("missing hearth/1 envelope: %q", out)
	}
	for _, want := range []string{
		"--- Recent org chat ---",
		"[Org Chat from Dana]: @agent thoughts?",
		"hearth chat reply --room room1 <<'HEARTH_MSG_",
		"never write the line HEARTH_MSG_",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("channel prompt missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--thread") {
		t.Errorf("channel prompt should not mention --thread:\n%s", out)
	}
}

func TestBuildChatMentionPrompt_Thread(t *testing.T) {
	out := string(buildChatMentionPrompt("room1", "root9", "Dana", "@agent thoughts?", []string{"[Sam]: dinner plans?"}))
	for _, want := range []string{
		"--- This thread so far ---",
		"[Org Chat thread, from Dana]: @agent thoughts?",
		"hearth chat reply --room room1 --thread root9 <<'HEARTH_MSG_",
		"--channel",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("thread prompt missing %q:\n%s", want, out)
		}
	}
}
