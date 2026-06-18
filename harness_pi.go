package main

import (
	"os"
	"strings"
	"time"
)

// piStream wraps transformPiEvent for the SPI's per-line streaming
// contract. transformPiEvent joins multiple emitted entries with "\n"
// (the assistant-with-tool-calls case fans out one input line to
// several output entries), so split on "\n" so each becomes its own
// bridge line.
type piStream struct{}

func (piStream) TransformLine(line string) [][]byte {
	out := transformPiEvent(line)
	if out == "" {
		return nil
	}
	parts := strings.Split(out, "\n")
	frames := make([][]byte, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		frames = append(frames, []byte(p))
	}
	return frames
}

func (piHarness) NewStreamTransformer() StreamTransformer { return piStream{} }

// Pi (pi-coding-agent) — registers --append-system-prompt and an
// optional --session pointing at the on-disk transcript path. Unlike
// claude/copilot which take a session id, pi takes the file path
// itself, so Argv has to call piSessionPath at spawn time.
//
// SupportsResume returns false on purpose: pi's --session is plumbed by
// the daemon when carrying state across sleep/wake, but we don't show
// users a "resume?" affordance for it. The user-facing UX and the
// internal session-file handling are intentionally separate concerns.
type piHarness struct{}

func (piHarness) Name() string         { return "pi" }
func (piHarness) Binary() string       { return "pi" }
func (piHarness) ServerName() string   { return "pi" }
func (piHarness) SupportsResume() bool { return false }

func (piHarness) Argv(ctx HarnessCtx) []string {
	args := []string{"--append-system-prompt", ctx.SystemPrompt}
	if ctx.AgentSessionID != "" {
		if sessPath := piSessionPath(ctx.AgentSessionID, ctx.Cwd); sessPath != "" {
			args = append(args, "--session", sessPath)
		}
	}
	return args
}

func (piHarness) TranscriptPath(ctx HarnessCtx) string {
	if ctx.AgentSessionID != "" {
		return piSessionPath(ctx.AgentSessionID, ctx.Cwd)
	}
	return derivePiTranscriptPath(ctx.Cwd)
}

// Pi's TextInput flushes \r cleanly — no extra delay or kick needed.
func (piHarness) SubmitDelay() time.Duration       { return 50 * time.Millisecond }
func (piHarness) PostSubmit(_ *os.Process) error   { return nil }

// PreSpawn: nothing. Pi reads --append-system-prompt and has no trust
// dialog or instruction file.
func (piHarness) PreSpawn(_ HarnessCtx) error { return nil }

func (piHarness) InstallSkill(_ HarnessCtx, _, _ string, _ []byte) error { return nil }
func (piHarness) RemoveSkill(_ HarnessCtx, _, _ string) error             { return nil }

func (piHarness) SessionIDPolicy() SessionIDPolicy { return SessionIDMint }
func (piHarness) ReportsResumeID() bool            { return true }
func (piHarness) AssignedSessionID(_ string) string { return "" }
func (piHarness) NeedsInjectGate() bool            { return false }
func (piHarness) SupportsAttach() bool             { return true }
func (piHarness) WarmupPayload() []byte            { return nil }

// Pi configures its model in its own UI; no env var to set.
func (piHarness) ModelEnv(_ string) (string, string, bool) { return "", "", false }

// Pi has no helper binaries to entitle.
func (piHarness) EnsureHelperEntitlements() {}

// MinimumVersion: pinned to the validated version. Older pi versions
// may well still work, but we haven't confirmed it on the dev box —
// refuse and force an upgrade rather than guess.
func (piHarness) MinimumVersion() string { return "0.73.0" }

// KnownTestedVersions: 0.73.0 validated on the dev box 2026-05-13.
func (piHarness) KnownTestedVersions() []string {
	return []string{"0.73.0"}
}

// probePiVersion runs `pi --version`. extractFirstSemver pulls the
// version out of whatever shape pi prints.
func probePiVersion() (string, error) {
	return runVersionCommand("pi", []string{"--version"})
}

func init() {
	registerHarness(piHarness{})
	registerVersionProbe("pi", probePiVersion)
}
