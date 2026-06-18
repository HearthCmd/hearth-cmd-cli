package main

import (
	"os"
	"path/filepath"
	"time"
)

// Claude (Anthropic Claude Code) — the canonical baseline harness. Most
// of the daemon's stream/tool normalization paths target claude's shape
// directly, so claude itself contributes no transform/normalize logic
// (those methods will be passthrough when they're added to the
// interface in later per-harness ports).
//
// Implements: Name/Binary/ServerName/SupportsResume (step 1) plus
// Argv/TranscriptPath (step 2). Env (ANTHROPIC_MODEL), TransformEvent,
// NormalizeTool, and any PostSpawn hooks come in later steps.
type claudeHarness struct{}

// claudeStream is the per-spawn transformer used by tailAndPump.
// Claude's on-disk JSONL is already bridge-shape, so the transform is
// near-identity: pass each line through unchanged, except drop slash-
// command lines (the CLI records /voice, /commit, etc. in the same
// file but they shouldn't reach the server).
type claudeStream struct{}

func (claudeStream) TransformLine(line string) [][]byte {
	if line == "" || isSlashCommand(line) {
		return nil
	}
	return [][]byte{[]byte(line)}
}

func (claudeHarness) NewStreamTransformer() StreamTransformer { return claudeStream{} }

func (claudeHarness) Name() string         { return "claude" }
func (claudeHarness) Binary() string       { return "claude" }
func (claudeHarness) ServerName() string   { return "claude-code" }
func (claudeHarness) SupportsResume() bool { return true }

// Argv: --dangerously-skip-permissions (the daemon's interpose layer is
// authoritative; claude's own approval prompts are noise) +
// --append-system-prompt to inject the hearth permission instructions
// and identity. Session-id flag selection differs by intent:
// --session-id <uuid> CREATES a new session at that id (claude rejects
// if one exists); --resume <uuid> LOADS an existing one. Use the first
// on a fresh spawn so we know where the transcript lands; the second
// when carrying a session forward across sleep/wake.
func (claudeHarness) Argv(ctx HarnessCtx) []string {
	args := []string{
		"--dangerously-skip-permissions",
		"--append-system-prompt", ctx.SystemPrompt,
	}
	if ctx.AgentSessionID != "" {
		if ctx.ResumingPriorSession {
			args = append(args, "--resume", ctx.AgentSessionID)
		} else {
			args = append(args, "--session-id", ctx.AgentSessionID)
		}
	}
	return args
}

// TranscriptPath: claude writes per-cwd directories under
// ~/.claude/projects/<projHash>/, with each session as <session_id>.jsonl.
// projHash is the cwd with all non-alphanumeric chars replaced by dashes
// (sanitizeClaudeProjectHash in agent.go).
func (claudeHarness) TranscriptPath(ctx HarnessCtx) string {
	if ctx.AgentSessionID != "" {
		return deriveClaudeTranscriptPathByID(ctx.AgentSessionID, ctx.Cwd)
	}
	return deriveClaudeTranscriptPath(ctx.Cwd)
}

// Claude's TextInput flushes \r cleanly — no extra delay or kick needed.
func (claudeHarness) SubmitDelay() time.Duration         { return 50 * time.Millisecond }
func (claudeHarness) PostSubmit(_ *os.Process) error     { return nil }

// PreSpawn: flip three claude pre-acceptance flags so the first
// injected user message doesn't get eaten by an interactive dialog:
//
//   1. preAcceptClaudeTrust — per-cwd "Do you trust this folder?"
//      flag in ~/.claude.json
//   2. preAcceptClaudeBypassPrompt — global "Bypass Permissions mode
//      acceptance" in ~/.claude/settings.json (added in claude 2.1.147;
//      without it claude sits at a Y/N dialog and exits non-zero)
//   3. seedClaudeBypassSettings — project-local
//      .claude/settings.local.json with permissions.defaultMode =
//      bypassPermissions so the bash sandbox actually lets the agent
//      write outside cwd, which --dangerously-skip-permissions on the
//      CLI alone doesn't achieve
//
// All three are idempotent and best-effort; errors are logged inside.
func (claudeHarness) PreSpawn(ctx HarnessCtx) error {
	preAcceptClaudeTrust(ctx.Cwd)
	preAcceptClaudeBypassPrompt()
	seedClaudeBypassSettings(ctx.Cwd)
	return nil
}

// InstallSkill writes the skill to <cwd>/.claude/skills/<slug>-<connID>/SKILL.md
// so Claude Code's native progressive-loading picks it up automatically.
// The file is written as-is (the plugin's skill.md already carries Claude
// YAML frontmatter). Existing files are left alone — same policy as
// installHearthInstructions: user edits are preserved across restarts.
func (claudeHarness) InstallSkill(ctx HarnessCtx, connectionID, pluginSlug string, skillContent []byte) error {
	dir := filepath.Join(ctx.Cwd, ".claude", "skills", pluginSlug+"-"+connectionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(dest); err == nil {
		return nil // already present — don't clobber user edits
	}
	return os.WriteFile(dest, skillContent, 0o644)
}

// RemoveSkill removes <cwd>/.claude/skills/<pluginSlug>-<connectionID>/
// so the agent no longer sees the skill on next launch.
func (claudeHarness) RemoveSkill(ctx HarnessCtx, connectionID, pluginSlug string) error {
	dir := filepath.Join(ctx.Cwd, ".claude", "skills", pluginSlug+"-"+connectionID)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (claudeHarness) SessionIDPolicy() SessionIDPolicy { return SessionIDMint }
func (claudeHarness) ReportsResumeID() bool            { return true }
func (claudeHarness) AssignedSessionID(_ string) string { return "" }
func (claudeHarness) NeedsInjectGate() bool            { return false }
func (claudeHarness) SupportsAttach() bool             { return true }
func (claudeHarness) WarmupPayload() []byte            { return nil }

// Claude has no helper binaries to entitle.
func (claudeHarness) EnsureHelperEntitlements() {}

func (claudeHarness) ModelEnv(modelName string) (string, string, bool) {
	if modelName == "" {
		return "", "", false
	}
	return "ANTHROPIC_MODEL", modelName, true
}

// MinimumVersion: pinned to the validated version. Older claude
// versions may well still work, but we haven't confirmed it on the
// dev box — refuse and force an upgrade rather than guess.
func (claudeHarness) MinimumVersion() string { return "2.1.141" }

// KnownTestedVersions: claude has been the canonical baseline through
// every hearth-cmd-cli release. Curated set starts at 2.1.141
// (validated on the dev box 2026-05-13). 2.1.147 added the
// bypass-permissions acceptance dialog handled by
// preAcceptClaudeBypassPrompt (2026-05-21).
func (claudeHarness) KnownTestedVersions() []string {
	return []string{"2.1.141", "2.1.147"}
}

// probeClaudeVersion runs `claude --version` and extracts the leading
// semver from output like "1.0.0 (Claude Code)". Per memory
// project_claude_session_id_vs_resume the format is stable; if it
// changes, this is one of the first places to look.
func probeClaudeVersion() (string, error) {
	return runVersionCommand("claude", []string{"--version"})
}

func init() {
	registerHarness(claudeHarness{})
	registerVersionProbe("claude", probeClaudeVersion)
}
