//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// knownAgents lists the valid agent runtime values.
var knownAgents = map[string]bool{
	"claude":  true,
	"copilot": true,
	"codex":   true,
	"gemini":  true,
	"pi":      true,
}

const defaultAgent = "claude"

// resolveAgent resolves the agent runtime from the caller's preference, then
// the HEARTH_AGENT env var, then falls back to the default. Org-spawned
// agents always pass an explicit value derived from the harness; this fallback
// chain exists for edge cases like the stream subcommand without --agent.
func resolveAgent(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("HEARTH_AGENT"); v != "" {
		return v
	}
	return defaultAgent
}

// agentBinary returns the CLI binary name for the given agent runtime.
// All known harnesses now live in the registry; this function survives
// only as a defensive shim that returns "claude" on an unknown value
// so a typo doesn't try to spawn an empty binary name. See
// harness_iface.go.
func agentBinary(agent string) string {
	if h, ok := getHarness(agent); ok {
		return h.Binary()
	}
	return "claude"
}

// agentServerName returns the agent identifier sent to the server.
// Registry lookup with claude-code as the unknown-default. See
// agentBinary above.
func agentServerName(agent string) string {
	if h, ok := getHarness(agent); ok {
		return h.ServerName()
	}
	return "claude-code"
}

// agentSupportsResume returns whether the agent supports --resume with
// a session ID. Registry lookup; unknown agents default to false.
func agentSupportsResume(agent string) bool {
	if h, ok := getHarness(agent); ok {
		return h.SupportsResume()
	}
	return false
}

// hearthSystemPrompt is appended to the agent's system prompt to teach it
// how to interpret permission denials from the hearth interpose library.
const hearthSystemPrompt = `Tool calls are managed by a permission system called hearth. ` +
	`If a command exits with code 126, or a file operation fails with "Permission denied", ` +
	`the user has explicitly denied this action. ` +
	`Do not retry the same action. Try a different approach or ask the user what they'd like instead. ` +
	`If a command exits with code 127, or a file operation fails with "Operation not permitted", ` +
	`the user wants you to stop. Do not continue with any further tool calls. ` +
	`Explain what you were doing and wait for new instructions.` +
	"\n\n" +
	`Multiple humans in the same household may share this conversation with you. ` +
	`Each user message you receive is wrapped in a small hearth envelope: ` +
	`a single header line of the form ` + "`hearth/1 {\"from\":{\"id\":\"...\",\"name\":\"Alice\"},\"mid\":\"...\",...}`" +
	`, followed by a blank line, followed by the actual message body. ` +
	`The header tells you who is talking — use the from.name field to address them and to keep track ` +
	`of who said what across turns. ` +
	`Other fields (mid, ts, mentions, reply_to) may be present and may be safely ignored unless you need them. ` +
	`Treat the body after the blank line as a normal user turn from that person. ` +
	`Do NOT echo the hearth/1 header or its JSON in your replies; refer to senders by name instead.` +
	"\n\n" +
	`Because multiple humans share this conversation, not every message is for you. ` +
	`If a message is clearly directed at another participant — for example, the body opens with "@Bob ..." or "Bob, ...", ` +
	`or otherwise reads as one human talking to another rather than instructing or asking you — ` +
	`produce NO output at all. No message, no tool calls, no acknowledgment. ` +
	`Specifically, do NOT say things like "That's directed at Bob, I'll let him answer," ` +
	`"This is for Bob," "Waiting for Bob," "I'll step aside," or any other variation. ` +
	`Those meta-acknowledgments ARE chiming in — they put your voice into a turn that wasn't yours. ` +
	`Treat these messages exactly like the warmup ping described below: stay completely silent ` +
	`and wait for the next turn. ` +
	`Respond only when you are the addressee, when the message is clearly directed at the group as a whole, ` +
	`or when no specific addressee is indicated and acting is the obviously expected behavior.` +
	"\n\n" +
	`Occasionally, you may receive an internal startup ping wrapped exactly as ` +
	"`<hearth-warmup>...</hearth-warmup>`" +
	`. These are not messages from any human and require no action. Produce no output, no tool calls, ` +
	`and no acknowledgment when you receive one — simply wait for the next turn.` +
	"\n\n" +
	`Some hearth/1 envelopes carry a "kind":"system_event" field instead of (or alongside) the usual from/body shape. ` +
	`These are server-emitted audit annotations addressed to the humans in the conversation — for example, ` +
	`"permission_dispatch" announcing who was notified to approve a tool call, or "permission_resolved" ` +
	`announcing the outcome. They are NOT messages to you and require no response. ` +
	`Treat them exactly like the warmup ping: produce no output, no tool calls, no acknowledgment, ` +
	`and never paraphrase or summarize them back to the humans. Wait for the next real turn.` +
	"\n\n" +
	`## Org Chat` + "\n\n" +
	`You may receive messages from the org's shared chat room. They arrive as a turn starting with ` +
	"`[Org Chat from <name>]: <message>`" + `, optionally preceded by a context block ` +
	`of recent chat history. When you receive one you have been @mentioned and should reply. ` +
	`Reply by running this Bash command:` + "\n\n" +
	"  hearth chat reply --room <room_id> \"your response here\"\n\n" +
	`The room_id and exact command syntax will appear in the turn you receive. ` +
	`You can send multiple replies for a multi-part response. Keep replies concise. ` +
	`Do not call ` + "`hearth chat reply`" + ` unprompted — only when you receive an [Org Chat] mention.`

// buildIdentityPrompt returns a short paragraph telling the agent who it
// is, what role it holds, and what household it serves. Returns "" if
// none of the fields are set, so callers can no-op the empty case
// instead of stamping a vacuous "You are ." into the prompt.
func buildIdentityPrompt(agentName, jobTitle, jobMandate, organizationName string) string {
	if agentName == "" && jobTitle == "" && jobMandate == "" && organizationName == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("Identity:")
	if agentName != "" {
		b.WriteString(" Your name is ")
		b.WriteString(agentName)
		b.WriteString(".")
	}
	if organizationName != "" {
		b.WriteString(" You serve the household called ")
		b.WriteString(organizationName)
		b.WriteString(".")
	}
	if jobTitle != "" {
		b.WriteString(" Your role is ")
		b.WriteString(jobTitle)
		b.WriteString(".")
	}
	if jobMandate != "" {
		b.WriteString(" Your mandate: ")
		b.WriteString(jobMandate)
		if !strings.HasSuffix(jobMandate, ".") {
			b.WriteString(".")
		}
	}
	return b.String()
}

// deriveTranscriptPath constructs the transcript file path for the
// given agent by delegating to its Harness.TranscriptPath. Returns ""
// if the agent is unknown or the path can't be determined yet (file
// not created, session id unknown, etc.); callers poll until it
// appears. See harness_iface.go.
func deriveTranscriptPath(agent, sessionID, cwd string) string {
	if h, ok := getHarness(agent); ok {
		return h.TranscriptPath(HarnessCtx{
			AgentSessionID: sessionID,
			Cwd:            cwd,
		})
	}
	return ""
}

// sanitizeClaudeProjectHash turns an absolute cwd into claude's on-disk
// project-directory name. Claude replaces every non-alphanumeric-and-non-dash
// character (/, _, ., etc.) with a dash — not just slashes. Example:
// /Users/mattbeller/hearth_agents/scratch/abc123
//   → -Users-mattbeller-hearth-agents-scratch-abc123
func sanitizeClaudeProjectHash(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// deriveClaudeTranscriptPathByID returns the transcript path for a known session ID.
// The file may not exist yet (the caller polls until it appears).
func deriveClaudeTranscriptPathByID(sessionID, cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projHash := sanitizeClaudeProjectHash(cwd)
	return filepath.Join(home, ".claude", "projects", projHash, sessionID+".jsonl")
}

func deriveClaudeTranscriptPath(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projHash := sanitizeClaudeProjectHash(cwd)
	projDir := filepath.Join(home, ".claude", "projects", projHash)
	entries, err := os.ReadDir(projDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = e.Name()
		}
	}
	if newest != "" {
		return filepath.Join(projDir, newest)
	}
	return ""
}

// deriveCopilotTranscriptPathByID returns the transcript path for a known session ID.
func deriveCopilotTranscriptPathByID(sessionID string) string {
	home := os.Getenv("COPILOT_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".copilot")
	}
	return filepath.Join(home, "session-state", sessionID, "events.jsonl")
}

func deriveCopilotTranscriptPath() string {
	home := os.Getenv("COPILOT_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".copilot")
	}
	stateDir := filepath.Join(home, "session-state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = e.Name()
		}
	}
	if newest != "" {
		return filepath.Join(stateDir, newest, "events.jsonl")
	}
	return ""
}

// sanitizeGeminiProjectDir mirrors gemini-cli's per-project directory
// naming under ~/.gemini/tmp/. Gemini 0.40 takes filepath.Base(cwd)
// and replaces every non-alphanumeric-and-non-dash character with a
// single dash — so cwd basename "gemini_test_2" maps to on-disk dir
// "gemini-test-2". One char in, one char out (NOT run-collapse).
// Anything quirky beyond ASCII alphanumerics and dashes (dots, spaces,
// etc.) is speculatively handled the same way; we have observations
// for `_` only, so widen the substitution rule if new failure modes
// surface.
func sanitizeGeminiProjectDir(cwd string) string {
	base := filepath.Base(cwd)
	var b strings.Builder
	b.Grow(len(base))
	for _, r := range base {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// deriveGeminiTranscriptPathByID finds the transcript file whose sessionId
// JSON field matches the given UUID.
func deriveGeminiTranscriptPathByID(sessionID, cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	chatsDir := filepath.Join(home, ".gemini", "tmp", sanitizeGeminiProjectDir(cwd), "chats")
	entries, err := os.ReadDir(chatsDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(chatsDir, e.Name())
		if extractGeminiSessionID(p) == sessionID {
			return p
		}
	}
	return ""
}

func deriveGeminiTranscriptPath(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	chatsDir := filepath.Join(home, ".gemini", "tmp", sanitizeGeminiProjectDir(cwd), "chats")
	entries, err := os.ReadDir(chatsDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = e.Name()
		}
	}
	if newest != "" {
		return filepath.Join(chatsDir, newest)
	}
	return ""
}

// extractGeminiSessionID reads the sessionId from the first line of a Gemini
// JSONL transcript. The first line is the session header
// ({"sessionId":..., "kind":"main"}); subsequent lines are turn events.
func extractGeminiSessionID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	var obj struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &obj); err != nil {
		return ""
	}
	return obj.SessionID
}

// deriveCodexTranscriptByCwd scans recent Codex transcript files and
// returns the newest one whose session_meta event has the matching cwd.
//
// Codex 0.128 dropped the AGENTS.md-based sentinel approach we used
// previously: AGENTS.md content is no longer embedded in the rollout.
// session_meta is now the very first JSONL event and includes the
// session's cwd, which uniquely identifies a hearth agent (every agent
// has a distinct working directory under ~/hearth_agents/<org>/<agent>).
//
// We accept either an absolute cwd or one with trailing slashes
// stripped — codex serializes the literal value passed to it, but the
// daemon's recorded value occasionally has a trailing separator.
func deriveCodexTranscriptByCwd(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	target := strings.TrimRight(cwd, "/")
	if target == "" {
		return ""
	}
	sessionsDir := filepath.Join(home, ".codex", "sessions")

	type candidate struct {
		path  string
		mtime time.Time
	}
	var candidates []candidate
	filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".jsonl") {
			return nil
		}
		candidates = append(candidates, candidate{path, info.ModTime()})
		return nil
	})
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].mtime.After(candidates[j].mtime)
	})

	// Walk newest first; the agent's session is almost always in the
	// most recent few. Cap the scan at 20 files to keep startup latency
	// bounded on hosts with thousands of historical rollouts.
	limit := 20
	if len(candidates) < limit {
		limit = len(candidates)
	}
	for _, c := range candidates[:limit] {
		if matchesCodexCwd(c.path, target) {
			return c.path
		}
	}
	return ""
}

// codexUUIDRe matches the canonical 8-4-4-4-12 hex UUID at the end of
// a codex rollout filename. The full filename is
// `rollout-<ISO-timestamp>-<UUID>.jsonl`; the timestamp also has
// hyphens, so we can't split on `-` — anchor on the .jsonl suffix.
var codexUUIDRe = regexp.MustCompile(`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// extractCodexUUIDFromPath pulls the session UUID out of a codex
// rollout filename. Returns "" if the path doesn't match the expected
// shape.
func extractCodexUUIDFromPath(path string) string {
	m := codexUUIDRe.FindStringSubmatch(filepath.Base(path))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// deriveCodexTranscriptByID returns the rollout path for a known codex
// session UUID. Globs `~/.codex/sessions/*/*/*/rollout-*-<id>.jsonl`
// since the YYYY/MM/DD subdirs and timestamp prefix are unknown to the
// caller. Returns "" if no match.
func deriveCodexTranscriptByID(sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	pattern := filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*-"+sessionID+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return ""
	}
	// UUID is unique; there should be exactly one match. If somehow
	// multiple (shouldn't happen — UUIDv7 collisions don't), take the
	// newest by mtime as the most defensible choice.
	if len(matches) == 1 {
		return matches[0]
	}
	var newest string
	var newestTime time.Time
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = p
		}
	}
	return newest
}

// matchesCodexCwd reads the first JSONL line of a codex rollout and
// reports whether its session_meta payload's cwd matches target. Returns
// false on any read/parse error so the scan continues to the next file.
func matchesCodexCwd(path, target string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	// session_meta with full base_instructions can be enormous (the
	// 0.128 system prompt alone is multiple KB). Use a generous buffer.
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	if !scanner.Scan() {
		return false
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &meta); err != nil {
		return false
	}
	if meta.Type != "session_meta" {
		return false
	}
	return strings.TrimRight(meta.Payload.Cwd, "/") == target
}

func deriveCodexTranscriptPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Codex stores sessions in date-nested dirs:
	// ~/.codex/sessions/YYYY/MM/DD/rollout-YYYY-MM-DDTHH-MM-SS-UUID.jsonl
	// Walk the tree to find the newest .jsonl file.
	sessionsDir := filepath.Join(home, ".codex", "sessions")
	var newest string
	var newestTime time.Time

	filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".jsonl") {
			return nil
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = path
		}
		return nil
	})
	return newest
}

// piSessionPath returns the transcript file path for a Pi session ID,
// following Pi's convention: $PI_CODING_AGENT_DIR/sessions/<normalized-cwd>/<sessionID>.jsonl
// PI_CODING_AGENT_DIR defaults to ~/.pi/agent.
func piSessionPath(sessionID, cwd string) string {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	baseDir := os.Getenv("PI_CODING_AGENT_DIR")
	if baseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		baseDir = filepath.Join(home, ".pi", "agent")
	}
	// Pi encodes CWD as: strip leading /, replace /\: with -, wrap in --
	safePath := strings.TrimLeft(cwd, "/\\")
	safePath = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(safePath)
	safePath = "--" + safePath + "--"
	return filepath.Join(baseDir, "sessions", safePath, sessionID+".jsonl")
}

func derivePiTranscriptPath(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Pi encodes CWD as: strip leading /, replace /\: with -, wrap in --
	safePath := strings.TrimLeft(cwd, "/\\")
	safePath = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(safePath)
	safePath = "--" + safePath + "--"
	sessDir := filepath.Join(home, ".pi", "agent", "sessions", safePath)
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = e.Name()
		}
	}
	if newest != "" {
		return filepath.Join(sessDir, newest)
	}
	return ""
}

