//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"time"
)

// Codex (OpenAI codex CLI) — `--dangerously-bypass-approvals-and-sandbox`
// to opt out of codex's own approval/sandbox loop so the hearth interpose
// layer is the only gate. SPI-encoded quirks:
//
//   - First-turn rollout flush: codex 0.128 only writes its rollout JSONL
//     on the SECOND user turn, so WarmupPayload() returns a
//     <hearth-warmup> message that AGENTS.md tells codex to ignore.
//   - Bracketed-paste gate: codex's TUI eats input that arrives before it
//     enables bracketed-paste mode; NeedsInjectGate() returns true so
//     the daemon holds the first inject until \x1b[?2004h or 1.5s quiet.
//   - Session id is harness-assigned: codex picks its own UUIDv7 at
//     first turn and writes it into the rollout filename
//     (rollout-<ISO-ts>-<UUID>.jsonl). SessionIDPolicy() returns
//     SessionIDHarnessAssigned; AssignedSessionID() extracts the UUID
//     from the path after the streamer discovers it; ReportsResumeID()
//     returns true so the daemon stores it for the next wake.
//   - Resume: `codex --dangerously-bypass-approvals-and-sandbox
//     resume <UUID>` is a subcommand (positional after the global
//     flag), not a flag. Argv assembles it when ResumingPriorSession
//     is set. `codex resume` appends to the original rollout file —
//     the streamer skips its mtime-after-spawn gate when AgentSessionID
//     is set (deterministic path via deriveCodexTranscriptByID).
//
// Codex Rust-binary dyld re-sign also SPI, via
// EnsureHelperEntitlements (calls ensureCodexBinary in connect.go).
// See project_codex_first_turn_warmup memory and
// project_codex_resume_punted memory.
//
// SupportsResume is true because codex *does* accept `resume <UUID>` on
// the command line — but we don't pass one yet at spawn-time (the resume
// argv plumbing is documented in project_codex_resume_punted and was
// punted alongside codex's broader dev-box breakage).
type codexHarness struct{}

// codexStream wraps transformCodexEvent for the SPI's per-line
// streaming contract. Stateless — codex's JSONL is self-contained per
// line (the session header doesn't carry forward into entry transforms
// the way gemini's does).
type codexStream struct{}

func (codexStream) TransformLine(line string) [][]byte {
	out := transformCodexEvent(line)
	if out == "" {
		return nil
	}
	return [][]byte{[]byte(out)}
}

func (codexHarness) NewStreamTransformer() StreamTransformer { return codexStream{} }

func (codexHarness) Name() string         { return "codex" }
func (codexHarness) Binary() string       { return "codex" }
func (codexHarness) ServerName() string   { return "codex" }
func (codexHarness) SupportsResume() bool { return true }

// Argv: `codex --dangerously-bypass-approvals-and-sandbox [resume <id>]`.
// codex's resume is a subcommand (positional, after global flags), not a
// flag — different shape from claude/copilot's --resume. When the daemon
// hands us an AgentSessionID with ResumingPriorSession set (server's
// last_session_id + priorSessionUsable check both passed), append it as
// `resume <id>` and codex re-attaches the prior conversation.
func (codexHarness) Argv(ctx HarnessCtx) []string {
	args := []string{"--dangerously-bypass-approvals-and-sandbox"}
	if ctx.ResumingPriorSession && ctx.AgentSessionID != "" {
		args = append(args, "resume", ctx.AgentSessionID)
	}
	return args
}

// TranscriptPath: deterministic path when AgentSessionID is set (resume
// case — the prior UUID maps directly to the rollout filename via
// glob); otherwise cwd lookup (fresh spawn — codex hasn't yet picked
// its UUID; the streamer's mtime gate combined with our per-agent cwd
// disambiguates).
func (codexHarness) TranscriptPath(ctx HarnessCtx) string {
	if ctx.AgentSessionID != "" {
		return deriveCodexTranscriptByID(ctx.AgentSessionID)
	}
	if ctx.Cwd == "" {
		return ""
	}
	return deriveCodexTranscriptByCwd(ctx.Cwd)
}

// AssignedSessionID extracts codex's chosen UUID from its rollout
// filename so the daemon can report it back for future wakes. Codex
// names files `rollout-<ISO-timestamp>-<UUIDv7>.jsonl`; the UUID is
// the canonical 8-4-4-4-12 hex pattern at the tail.
func (codexHarness) AssignedSessionID(transcriptPath string) string {
	return extractCodexUUIDFromPath(transcriptPath)
}

func (codexHarness) SubmitDelay() time.Duration   { return 50 * time.Millisecond }
func (codexHarness) PostSubmit(_ *os.Process) error { return nil }

// PreSpawn: write <cwd>/AGENTS.md with the hearth instructions AND a
// per-instance sentinel comment so the streamer can disambiguate
// multiple codex sessions that share a cwd. The dyld re-sign of
// codex's Rust binary still lives in setupInterpose (lifecycle-coupled
// to the interpose chunk); not absorbed here yet.
func (codexHarness) PreSpawn(ctx HarnessCtx) error {
	return installHearthInstructions("codex", ctx.AIAgentInstanceID, ctx.IdentityPrompt, ctx.Cwd)
}

func (codexHarness) InstallSkill(ctx HarnessCtx, connectionID, pluginSlug string, skillContent []byte) error {
	return appendSkillToInstructionFile(filepath.Join(ctx.Cwd, "AGENTS.md"), connectionID, pluginSlug, skillContent)
}

func (codexHarness) RemoveSkill(ctx HarnessCtx, connectionID, _ string) error {
	return stripSkillFromInstructionFile(filepath.Join(ctx.Cwd, "AGENTS.md"), connectionID)
}

func (codexHarness) SessionIDPolicy() SessionIDPolicy { return SessionIDHarnessAssigned }
func (codexHarness) ReportsResumeID() bool            { return true }

// Codex's TUI eats input that arrives before bracketed-paste mode is
// enabled. Hold the first inject until \x1b[?2004h (or 1.5s quiet).
func (codexHarness) NeedsInjectGate() bool { return true }

// Bracketed-paste warmup gate is one-shot at spawn — by the time
// a user attaches mid-session the gate is long resolved, so
// InjectRaw passes through cleanly.
func (codexHarness) SupportsAttach() bool { return true }

// codexWarmupBytes is a paste-wrapped <hearth-warmup> message that
// codex consumes as its first user turn so its rollout JSONL flushes
// in time for the user's real first turn to land in the file (codex
// 0.128 only writes after the SECOND turn). AGENTS.md tells codex to
// ignore the warmup; the transcript transformer hides it from iOS.
var codexWarmupBytes = []byte("\x1b[200~<hearth-warmup>startup ping</hearth-warmup>\x1b[201~")

func (codexHarness) WarmupPayload() []byte { return codexWarmupBytes }

func (codexHarness) ModelEnv(modelName string) (string, string, bool) {
	if modelName == "" {
		return "", "", false
	}
	return "OPENAI_MODEL", modelName, true
}

// EnsureHelperEntitlements re-signs codex's vendored Rust binary so
// DYLD_INSERT_LIBRARIES survives. The npm-installed `codex` script is
// a Node wrapper that exec's a vendored Mach-O at
// node_modules/@openai/codex-<arch>/vendor/<triple>/codex/codex; that
// binary has hardened runtime and strips dyld env vars without the
// allow-dyld-environment-variables entitlement.
func (codexHarness) EnsureHelperEntitlements() { ensureCodexBinary() }

// MinimumVersion: codex 0.128 dropped the AGENTS.md sentinel approach
// we previously relied on. The post-0.128 code path is what's in this
// file; running an older codex would silently miss transcripts. See
// project_codex_first_turn_warmup memory.
func (codexHarness) MinimumVersion() string { return "0.128.0" }

// KnownTestedVersions: validated on the dev box during the
// 2026-05-13 SPI work (resume + spawn + dyld re-sign). Add new
// versions here as you verify them; an installed version not in
// this list emits a startup warning.
func (codexHarness) KnownTestedVersions() []string {
	return []string{"0.128.0"}
}

// probeCodexVersion runs `codex --version`. As of 0.128 the output
// is "codex-cli X.Y.Z" on stdout; extractFirstSemver grabs X.Y.Z.
func probeCodexVersion() (string, error) {
	return runVersionCommand("codex", []string{"--version"})
}

func init() {
	registerHarness(codexHarness{})
	registerVersionProbe("codex", probeCodexVersion)
}
