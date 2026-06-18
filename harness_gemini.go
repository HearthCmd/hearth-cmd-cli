//go:build darwin || linux

package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// geminiStream is gemini's per-spawn stream transformer. Gemini writes
// its sessionId in the JSONL header line and DOESN'T repeat it on each
// entry, so the transformer holds onto it across calls. Until the
// sessionId is seen, every following line is parsed for a sessionId
// field — the header is normally line one, but tolerate "we joined
// mid-stream" by checking each line.
type geminiStream struct {
	sessionID string
}

func (g *geminiStream) TransformLine(line string) [][]byte {
	if g.sessionID == "" {
		var header struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal([]byte(line), &header) == nil && header.SessionID != "" {
			g.sessionID = header.SessionID
		}
	}
	entries := transformGeminiLine(line, g.sessionID)
	if len(entries) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(entries))
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			log.Printf("gemini message marshal error: %v", err)
			continue
		}
		out = append(out, b)
	}
	return out
}

func (geminiHarness) NewStreamTransformer() StreamTransformer { return &geminiStream{} }

// Gemini (gemini-cli) — `--yolo` to bypass per-tool confirmation, plus
// two TextInput quirks that the daemon papers over:
//
//  1. Submit is sluggish: a 300ms pause between paste payload and the \r
//     submit byte (vs ~50ms for everyone else) is needed for the
//     TextInput to stop swallowing characters.
//  2. Even after \r, gemini's TextInput stays in paste-buffering mode
//     and the turn never flushes. SIGWINCH knocks it back into its main
//     loop and the buffered turn submits. (This was discovered when an
//     iOS reattach happened to fire a SIGWINCH and the submit went
//     through; we now emit one deterministically.) See the
//     gemini-paste-submit-sigwinch memory entry.
//
// Resume: SessionIDPolicy = SessionIDHarnessAssigned plus
// ReportsResumeID = true. Daemon carries forward last_session_id from
// the server; on wake, Argv adds `--resume latest` (gemini's resume
// takes "latest" or an index — not a UUID — so the daemon's stored
// UUID is used only for priorSessionUsable's stat check, not in
// argv). cwd uniqueness per hearth agent ensures `latest` resolves to
// the right session.
type geminiHarness struct{}

func (geminiHarness) Name() string         { return "gemini" }
func (geminiHarness) Binary() string       { return "gemini" }
func (geminiHarness) ServerName() string   { return "gemini" }
func (geminiHarness) SupportsResume() bool { return true }

// Argv: --yolo plus, on resume, --resume latest. Gemini's --resume
// takes either "latest" or a 1-based index — NOT a session UUID like
// claude/copilot/codex. Index would be fragile (it shifts if anything
// else writes to this project), but "latest" is safe for hearth
// because every hearth agent has a unique project dir under
// ~/hearth_agents/<org>/<name>/, so gemini's "latest" for that
// project IS this agent's prior session by construction.
func (geminiHarness) Argv(ctx HarnessCtx) []string {
	args := []string{"--yolo"}
	if ctx.ResumingPriorSession {
		args = append(args, "--resume", "latest")
	}
	return args
}

// TranscriptPath: by-id lookup when AgentSessionID is known (resume
// case — match the prior UUID against JSONL headers); fall back to
// newest-in-project-dir if the by-id lookup misses. The fallback
// covers the open question of whether `gemini --resume latest`
// appends to the existing file (header matches → by-id hit) or
// starts a new one (header is a new UUID → by-id misses → newest
// wins, which is correct because the project dir is per-agent).
func (geminiHarness) TranscriptPath(ctx HarnessCtx) string {
	if ctx.AgentSessionID != "" {
		if p := deriveGeminiTranscriptPathByID(ctx.AgentSessionID, ctx.Cwd); p != "" {
			return p
		}
	}
	return deriveGeminiTranscriptPath(ctx.Cwd)
}

func (geminiHarness) SubmitDelay() time.Duration { return 300 * time.Millisecond }

func (geminiHarness) PostSubmit(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Signal(syscall.SIGWINCH)
}

// PreSpawn: write the hearth instructions to <cwd>/GEMINI.md. Gemini
// auto-loads that file as part of its system prompt — equivalent to
// claude's --append-system-prompt but on-disk.
func (geminiHarness) PreSpawn(ctx HarnessCtx) error {
	return installHearthInstructions("gemini", ctx.AIAgentInstanceID, ctx.IdentityPrompt, ctx.Cwd)
}

func (geminiHarness) InstallSkill(ctx HarnessCtx, connectionID, pluginSlug string, skillContent []byte) error {
	return appendSkillToInstructionFile(filepath.Join(ctx.Cwd, "GEMINI.md"), connectionID, pluginSlug, skillContent)
}

func (geminiHarness) RemoveSkill(ctx HarnessCtx, connectionID, _ string) error {
	return stripSkillFromInstructionFile(filepath.Join(ctx.Cwd, "GEMINI.md"), connectionID)
}

// Gemini picks its own UUID per session (stored in the JSONL header,
// not the filename like codex). SessionIDHarnessAssigned puts gemini
// on the same daemon plumbing as codex: daemon doesn't mint, but
// carries forward last_session_id from the server for the wake-resume
// branch.
func (geminiHarness) SessionIDPolicy() SessionIDPolicy { return SessionIDHarnessAssigned }
func (geminiHarness) ReportsResumeID() bool            { return true }

// AssignedSessionID reads the JSONL header to extract the sessionId
// gemini wrote there at file creation. Empty path or missing/invalid
// header → "" (handled by extractGeminiSessionID).
func (geminiHarness) AssignedSessionID(transcriptPath string) string {
	return extractGeminiSessionID(transcriptPath)
}

// Gemini exhibits the same first-inject-drop symptom as codex: input
// that arrives before bracketed-paste mode is enabled vanishes (text
// appears only as a header line in the JSONL, no user turn). Hold the
// first inject until \x1b[?2004h or the 1.5s quiet window.
func (geminiHarness) NeedsInjectGate() bool { return true }
func (geminiHarness) WarmupPayload() []byte { return nil }

// Bracketed-paste gate is one-shot at spawn — mid-session attach
// injects land post-gate. SIGWINCH-after-\r is wired through
// daemon_ws.go's inject path, so write-mode attach inherits it.
func (geminiHarness) SupportsAttach() bool { return true }

// Gemini configures its model in its own UI; no env var to set.
func (geminiHarness) ModelEnv(_ string) (string, string, bool) { return "", "", false }

// Gemini has no helper binaries to entitle.
func (geminiHarness) EnsureHelperEntitlements() {}

// MinimumVersion: pinned to the validated version. 0.40 introduced
// the cwd-basename sanitization rule (underscore → dash) our path
// lookups depend on (see project_gemini_project_dir_sanitization
// memory); 0.40.1 is what we actually verified end-to-end.
func (geminiHarness) MinimumVersion() string { return "0.40.1" }

// KnownTestedVersions: 0.40.1 was the version on the dev box during
// the 2026-05-13 validation. Add as you verify more.
func (geminiHarness) KnownTestedVersions() []string {
	return []string{"0.40.1"}
}

// probeGeminiVersion runs `gemini --version`. Output is a bare semver
// like "0.40.1" with no decorations as of 0.40.
func probeGeminiVersion() (string, error) {
	return runVersionCommand("gemini", []string{"--version"})
}

func init() {
	registerHarness(geminiHarness{})
	registerVersionProbe("gemini", probeGeminiVersion)
}
