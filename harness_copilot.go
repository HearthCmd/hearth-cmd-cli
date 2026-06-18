//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"time"
)

// Copilot (GitHub Copilot CLI) — `--allow-all` puts copilot in its
// non-interactive mode (the hearth interpose layer is the gate, not
// copilot's own per-tool prompt) and `--resume <id>` always attaches
// to (or creates) the session at that UUID. Unlike claude, copilot's
// --resume is happy on both "first spawn" and "carry forward" cases,
// so we don't need a session-id-vs-resume split — see the
// claude_session_id_vs_resume feedback memory for why claude needs it
// and copilot doesn't.
//
// Other copilot quirks NOT yet encoded as Harness methods (they stay
// as callsite special-cases until a second harness needs the same
// shape):
//
//   - Trust pre-accept: copilot's "Confirm folder trust" prompt eats
//     the first inject if not pre-accepted. `preAcceptCopilotTrust` in
//     agent_setup.go writes the cwd into ~/.copilot/config.json's
//     trustedFolders array (note the JSONC `//` header). Called from
//     daemon_session.go after spawn.
//   - Spawn-helper dyld re-sign: now SPI via
//     EnsureHelperEntitlements (calls into ensureCopilotHelpers in
//     connect.go, which globs ~/.copilot/pkg/*/*/prebuilds/<arch>/
//     and re-signs each spawn-helper).
//   - Instruction file: `.github/copilot-instructions.md` is written
//     by PreSpawn (installHearthInstructions).
//   - Session-id minting + resume-id reporting: SessionIDPolicy
//     returns SessionIDMint and ReportsResumeID returns true; the
//     daemon mints a UUID at spawn and reports it back for the next
//     wake. Both behaviors are now SPI methods rather than callsite
//     allowlists.
//
// All of those would become SPI methods (PostSpawn, NeedsHelpers,
// SessionIDPolicy, ResumeReportable, InstructionFile) once a second
// harness needs any of them.
//
// SupportsResume: true. Copilot accepts --resume <uuid> and reattaches
// to the prior session's context.
type copilotHarness struct{}

// copilotStream wraps transformCopilotEvent for the SPI's per-line
// streaming contract. Stateless — copilot's event entries carry all
// the context they need on each line.
type copilotStream struct{}

func (copilotStream) TransformLine(line string) [][]byte {
	out := transformCopilotEvent(line)
	if out == "" {
		return nil
	}
	return [][]byte{[]byte(out)}
}

func (copilotHarness) NewStreamTransformer() StreamTransformer { return copilotStream{} }

func (copilotHarness) Name() string         { return "copilot" }
func (copilotHarness) Binary() string       { return "copilot" }
func (copilotHarness) ServerName() string   { return "copilot" }
func (copilotHarness) SupportsResume() bool { return true }

func (copilotHarness) Argv(ctx HarnessCtx) []string {
	args := []string{"--allow-all"}
	if ctx.AgentSessionID != "" {
		args = append(args, "--resume", ctx.AgentSessionID)
	}
	return args
}

// TranscriptPath: copilot writes ~/.copilot/session-state/<id>/events.jsonl.
// When the AgentSessionID is known (mint or carry-forward) we go
// straight to it; on the empty path we fall back to the newest
// session dir, which is the legacy behavior preserved here for the
// rare codepath that calls deriveTranscriptPath without a session id.
func (copilotHarness) TranscriptPath(ctx HarnessCtx) string {
	if ctx.AgentSessionID != "" {
		return deriveCopilotTranscriptPathByID(ctx.AgentSessionID)
	}
	return deriveCopilotTranscriptPath()
}

func (copilotHarness) SubmitDelay() time.Duration   { return 50 * time.Millisecond }
func (copilotHarness) PostSubmit(_ *os.Process) error { return nil }

// PreSpawn: trust-pre-accept cwd in ~/.copilot/config.json's
// trustedFolders (otherwise the first injected message is eaten by
// the "Confirm folder trust" prompt) AND write the hearth
// instructions to <cwd>/.github/copilot-instructions.md. The
// spawn-helper dyld re-sign still lives in setupInterpose
// (lifecycle-coupled to the interpose chunk); not absorbed here yet.
func (copilotHarness) PreSpawn(ctx HarnessCtx) error {
	preAcceptCopilotTrust(ctx.Cwd)
	return installHearthInstructions("copilot", ctx.AIAgentInstanceID, ctx.IdentityPrompt, ctx.Cwd)
}

func (copilotHarness) InstallSkill(ctx HarnessCtx, connectionID, pluginSlug string, skillContent []byte) error {
	return appendSkillToInstructionFile(
		filepath.Join(ctx.Cwd, ".github", "copilot-instructions.md"),
		connectionID, pluginSlug, skillContent,
	)
}

func (copilotHarness) RemoveSkill(ctx HarnessCtx, connectionID, _ string) error {
	return stripSkillFromInstructionFile(
		filepath.Join(ctx.Cwd, ".github", "copilot-instructions.md"),
		connectionID,
	)
}

func (copilotHarness) SessionIDPolicy() SessionIDPolicy  { return SessionIDMint }
func (copilotHarness) ReportsResumeID() bool             { return true }
func (copilotHarness) AssignedSessionID(_ string) string { return "" }
func (copilotHarness) NeedsInjectGate() bool             { return false }
func (copilotHarness) SupportsAttach() bool              { return true }
func (copilotHarness) WarmupPayload() []byte             { return nil }

// Copilot configures its model in its own UI; no env var to set.
func (copilotHarness) ModelEnv(_ string) (string, string, bool) { return "", "", false }

// EnsureHelperEntitlements re-signs copilot's spawn-helper(s) so
// DYLD_INSERT_LIBRARIES survives the bash-tool fan-out. Copilot ships
// a Mach-O spawn-helper under ~/.copilot/pkg/*/*/prebuilds/<arch>/
// (and ~/Library/Caches/copilot/pkg/...); without the entitlement,
// macOS strips the dyld env var and inner commands (find, cat, etc.)
// run un-interposed.
func (copilotHarness) EnsureHelperEntitlements() { ensureCopilotHelpers() }

// MinimumVersion: copilot v1.0.40 was the version when our trust-
// pre-accept and events.jsonl handling were last validated. Earlier
// versions may have used a different config-file shape or session
// path. Bump when we revalidate.
func (copilotHarness) MinimumVersion() string { return "1.0.40" }

// KnownTestedVersions: 1.0.40 was verified end-to-end during the
// 2026-05-13 SPI work (spawn, trust pre-accept, file-write via
// spawn-helper, sleep/wake resume). Add as you verify more.
func (copilotHarness) KnownTestedVersions() []string {
	return []string{"1.0.40"}
}

// probeCopilotVersion runs `copilot --version`. The output prefix
// varies by version but always contains a semver; extractFirstSemver
// is robust to whatever leading/trailing text wraps it.
func probeCopilotVersion() (string, error) {
	return runVersionCommand("copilot", []string{"--version"})
}

func init() {
	registerHarness(copilotHarness{})
	registerVersionProbe("copilot", probeCopilotVersion)
}
