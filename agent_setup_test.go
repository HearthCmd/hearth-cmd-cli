//go:build darwin || linux

package main

// Coverage for the pure-logic agent setup helpers in agent_setup.go.
// buildAgentCommand and buildExportEnvs decide what flags get passed
// to each harness binary and what env vars wrap the spawn — the
// per-agent quirks (claude --dangerously-skip-permissions, codex
// --dangerously-bypass-approvals-and-sandbox, ANTHROPIC_MODEL vs
// OPENAI_MODEL pinning) are part of the contract with the harness
// integrations memory.

import (
	"strings"
	"testing"
)

// ---------- buildAgentCommand ----------

func TestBuildAgentCommand_ClaudeFlags(t *testing.T) {
	got, err := buildAgentCommand("claude", "you are a gardener", "/tmp/proj", "", "")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(got.Args, " ")
	if !strings.Contains(args, "--dangerously-skip-permissions") {
		t.Error("claude must launch with --dangerously-skip-permissions")
	}
	if !strings.Contains(args, "--append-system-prompt") {
		t.Error("claude must pass --append-system-prompt")
	}
	if !strings.Contains(args, "--session-id") {
		t.Error("claude must include --session-id")
	}
	// Identity prompt prefixes the standard hearth prompt.
	idx1 := strings.Index(args, "you are a gardener")
	idx2 := strings.Index(args, "Tool calls are managed by a permission system called hearth")
	if idx1 < 0 || idx2 < 0 || idx1 >= idx2 {
		t.Errorf("identity should precede hearth prompt: idx1=%d idx2=%d", idx1, idx2)
	}
	if got.AgentSessionID == "" {
		t.Error("claude must mint an AgentSessionID")
	}
	if got.AIAgentInstanceID == "" {
		t.Error("every connect should mint a fresh ai_agent_instance_id")
	}
}

func TestBuildAgentCommand_CodexFlags(t *testing.T) {
	got, _ := buildAgentCommand("codex", "", "/tmp/proj", "", "")
	args := strings.Join(got.Args, " ")
	if !strings.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Error("codex must launch with --dangerously-bypass-approvals-and-sandbox")
	}
	// codex doesn't get an agentSessionID per the comment in agent_setup.go
	// (the mtime gate logic).
	if got.AgentSessionID != "" {
		t.Errorf("codex must NOT have an agentSessionID, got %q", got.AgentSessionID)
	}
}

func TestBuildAgentCommand_CopilotFlags(t *testing.T) {
	got, _ := buildAgentCommand("copilot", "", "/tmp/proj", "", "")
	args := strings.Join(got.Args, " ")
	if !strings.Contains(args, "--allow-all") {
		t.Error("copilot must launch with --allow-all")
	}
	if !strings.Contains(args, "--resume") {
		t.Error("copilot must pass --resume with the agent session id")
	}
	if got.AgentSessionID == "" {
		t.Error("copilot must mint an AgentSessionID")
	}
}

func TestBuildAgentCommand_GeminiYolo(t *testing.T) {
	got, _ := buildAgentCommand("gemini", "", "/tmp/proj", "", "")
	if !contains(got.Args, "--yolo") {
		t.Error("gemini must launch with --yolo")
	}
}

func TestBuildAgentCommand_PiFlags(t *testing.T) {
	got, _ := buildAgentCommand("pi", "", "/tmp/proj", "", "")
	args := strings.Join(got.Args, " ")
	if !strings.Contains(args, "--append-system-prompt") {
		t.Error("pi must take --append-system-prompt (no --dangerously-skip-permissions)")
	}
	if strings.Contains(args, "--dangerously-skip-permissions") {
		t.Error("pi must NOT carry claude's permission bypass flag")
	}
	if got.AgentSessionID == "" {
		t.Error("pi must mint an AgentSessionID")
	}
}

func TestBuildAgentCommand_UnknownAgentNoFlags(t *testing.T) {
	got, _ := buildAgentCommand("brand-new-agent", "", "/tmp/proj", "", "")
	if len(got.Args) != 0 {
		t.Errorf("unknown agent should pass no flags, got %v", got.Args)
	}
}

func TestBuildAgentCommand_FreshUUIDPerCall(t *testing.T) {
	a, _ := buildAgentCommand("claude", "", "/tmp/p", "", "")
	b, _ := buildAgentCommand("claude", "", "/tmp/p", "", "")
	if a.AIAgentInstanceID == b.AIAgentInstanceID {
		t.Error("each invocation should produce a unique aiAgentInstanceID")
	}
	if a.AgentSessionID == b.AgentSessionID {
		t.Error("each invocation should produce a unique agentSessionID")
	}
}

// helper
func contains(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}

// ---------- buildExportEnvs ----------

func TestBuildExportEnvs_AlwaysSetsBaseFields(t *testing.T) {
	got := buildExportEnvs("dev1", "agent1", "myproj", "/tmp/bridge", "claude", "")
	for _, k := range []string{
		"HEARTH_DEVICE_ID", "HEARTH_AGENT_INSTANCE_ID", "HEARTH_PROJECT",
		"HEARTH_BRIDGE", "HEARTH_AGENT",
	} {
		if got[k] == "" {
			t.Errorf("%q must be set", k)
		}
	}
	if got["HEARTH_AGENT_INSTANCE_ID"] != "agent1" {
		t.Errorf("agent id wrong: %q", got["HEARTH_AGENT_INSTANCE_ID"])
	}
}

func TestBuildExportEnvs_ClaudeAnthropicModel(t *testing.T) {
	got := buildExportEnvs("d", "a", "p", "/b", "claude", "claude-opus-4-6")
	if got["ANTHROPIC_MODEL"] != "claude-opus-4-6" {
		t.Errorf("claude must set ANTHROPIC_MODEL, got %v", got)
	}
	if _, present := got["OPENAI_MODEL"]; present {
		t.Error("claude must not set OPENAI_MODEL")
	}
}

func TestBuildExportEnvs_CodexOpenAIModel(t *testing.T) {
	got := buildExportEnvs("d", "a", "p", "/b", "codex", "gpt-4o")
	if got["OPENAI_MODEL"] != "gpt-4o" {
		t.Errorf("codex must set OPENAI_MODEL, got %v", got)
	}
	if _, present := got["ANTHROPIC_MODEL"]; present {
		t.Error("codex must not set ANTHROPIC_MODEL")
	}
}

func TestBuildExportEnvs_OtherAgentsNoModelEnv(t *testing.T) {
	// gemini/copilot/pi configure model in their own UI; no
	// documented env var. modelName must be silently dropped.
	for _, agent := range []string{"gemini", "copilot", "pi"} {
		got := buildExportEnvs("d", "a", "p", "/b", agent, "some-model")
		if _, present := got["ANTHROPIC_MODEL"]; present {
			t.Errorf("%s must not set ANTHROPIC_MODEL", agent)
		}
		if _, present := got["OPENAI_MODEL"]; present {
			t.Errorf("%s must not set OPENAI_MODEL", agent)
		}
	}
}

func TestBuildExportEnvs_EmptyModelSkipsBoth(t *testing.T) {
	got := buildExportEnvs("d", "a", "p", "/b", "claude", "")
	if _, present := got["ANTHROPIC_MODEL"]; present {
		t.Error("empty modelName must skip ANTHROPIC_MODEL")
	}
}
