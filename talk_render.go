//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// =============================================================================
// styles
// =============================================================================

var (
	pillFocusedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("15")).
				Background(lipgloss.Color("33")).
				Padding(0, 1)
	pillStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			Padding(0, 1)

	statusStyle = lipgloss.NewStyle().Faint(true)
	emptyStyle  = lipgloss.NewStyle().Faint(true).Italic(true)

	youStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("33"))

	agentStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("76"))

	toolUseStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("36"))
	toolResultStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	activityStyle = lipgloss.NewStyle().Faint(true).Italic(true)
)

// =============================================================================
// pills
// =============================================================================

func renderPills(instances []talkInstance, focusedID string) string {
	if len(instances) == 0 {
		return emptyStyle.Render("(no active agent instances — spawn one with 'hearth hh agent create')")
	}
	parts := make([]string, 0, len(instances))
	for _, s := range instances {
		label := s.project
		if label == "" {
			label = "agent"
		}
		// pid_status='running' is the only "ready" state. Anything else
		// (never_spawned, spawn_failed, exited, killed, host_disconnected)
		// gets the ⟳ glyph so the user sees the agent isn't live yet.
		starting := s.pidStatus != "" && s.pidStatus != "running"
		marker := "○ "
		if starting {
			marker = "⟳ "
		}
		if s.aiAgentInstanceID == focusedID {
			if starting {
				marker = "⟳ "
			} else {
				marker = "● "
			}
			parts = append(parts, pillFocusedStyle.Render(marker+label))
		} else {
			parts = append(parts, pillStyle.Render(marker+label))
		}
	}
	return strings.Join(parts, " ")
}

// =============================================================================
// transcript content blocks
// =============================================================================

// renderTranscriptEntry parses a transcript_entry's `data` payload and returns
// rendered lines. Targets the claude transcript JSONL format
// ({type, message:{role, content:[...]}}); falls back to a faint dump of the
// raw JSON when the shape doesn't match. userName/agentName are used as the
// "<name> " prefix on user/assistant lines respectively; either can be ""
// (falls back to "you" / no prefix).
func renderTranscriptEntry(data json.RawMessage, userName, agentName string) string {
	if len(data) == 0 {
		return ""
	}
	var claude struct {
		Type    string `json:"type"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &claude); err == nil && claude.Type != "" {
		if rendered := renderClaudeMessage(claude.Message.Role, claude.Message.Content, userName, agentName); rendered != "" {
			return rendered
		}
	}
	return statusStyle.Render(strings.TrimSpace(string(data)))
}

// renderDecomposedTranscript renders a transcript entry from the server's
// pre-decomposed fields (Event, Text, ToolName, Message, ToolInput) rather
// than from raw transcript JSON.
func renderDecomposedTranscript(event, text, toolName, message string, toolInput json.RawMessage, userName, agentName string) string {
	switch event {
	case "user":
		// One entry may hold several coalesced segments (see parseHearthSegments);
		// render each on its own line so a human message is never hidden behind or
		// mangled by a leading system_event. Envelope name beats caller-supplied
		// name (handles multi-user relays).
		lines := renderUserSegments(text, userName)
		if len(lines) == 0 {
			return ""
		}
		return strings.Join(lines, "\n")
	case "text":
		if text != "" {
			if agentName != "" {
				return agentPrefix(agentName) + " " + text
			}
			return text
		}
	case "tool_use":
		label := "⚙ " + toolName
		if message != "" {
			label += " " + message
		}
		return toolUseStyle.Render(label)
	case "tool_result":
		if message != "" {
			return toolResultStyle.Render(message)
		}
	}
	return ""
}

// hearthSegment is one hearth/1 message parsed out of a (possibly coalesced)
// user transcript entry.
type hearthSegment struct {
	body     string
	fromName string
	kind     string // "" for a plain human/agent message; else "system_event", "chat_context", …
	event    string // populated when kind == "system_event"
}

// parseHearthSegments splits a user transcript entry into its ordered hearth/1
// segments. Claude coalesces queued stdin submits into ONE `user` JSONL entry
// when they pile up while the agent is mid-turn, and the daemon injects each
// inbound message (human relay_input AND system_events alike) as its own submit
// — so a single entry can concatenate several `hearth/1 {json}\n\n<body>`
// envelopes back-to-back with no separator. The merge is lossless, so we split
// them back out and render each independently; parsing only the first envelope
// (as the old parseHearthEnvelope did) mis-attributed the whole blob to its
// leading segment and mangled the human messages concatenated behind it. Text
// with no envelope returns a single segment carrying the raw text.
func parseHearthSegments(text string) []hearthSegment {
	const prefix = "hearth/1 "
	t := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(t, prefix) {
		return []hearthSegment{{body: text}}
	}
	var segs []hearthSegment
	i := 0
	for i < len(t) {
		nlRel := strings.Index(t[i:], "\n")
		if nlRel < 0 {
			// No header terminator — not a real envelope; take the remainder raw.
			segs = append(segs, hearthSegment{body: t[i:]})
			break
		}
		nl := i + nlRel
		var hdr struct {
			From struct {
				Name string `json:"name"`
			} `json:"from"`
			Kind  string `json:"kind"`
			Event string `json:"event"`
		}
		if err := json.Unmarshal([]byte(t[i+len(prefix):nl]), &hdr); err != nil {
			segs = append(segs, hearthSegment{body: t[i:]})
			break
		}
		// Body starts after the header line, skipping the optional blank line.
		bodyStart := nl + 1
		if bodyStart < len(t) && t[bodyStart] == '\n' {
			bodyStart++
		}
		// Coalesced segments are concatenated with no separator, so this body
		// runs until the next VALID hearth/1 header (or end of text).
		next := nextSegmentStart(t, bodyStart)
		bodyEnd := len(t)
		if next >= 0 {
			bodyEnd = next
		}
		segs = append(segs, hearthSegment{body: t[bodyStart:bodyEnd], fromName: hdr.From.Name, kind: hdr.Kind, event: hdr.Event})
		if next < 0 {
			break
		}
		i = next
	}
	if len(segs) == 0 {
		return []hearthSegment{{body: text}}
	}
	return segs
}

// nextSegmentStart returns the index of the next `hearth/1 {…}` whose header
// line parses as JSON, at or after `from`. Validating the JSON (not just the
// literal) keeps a body that happens to contain "hearth/1 {" from being taken
// as a boundary.
func nextSegmentStart(text string, from int) int {
	const prefix = "hearth/1 "
	const mark = "hearth/1 {"
	for pos := from; pos < len(text); {
		rel := strings.Index(text[pos:], mark)
		if rel < 0 {
			return -1
		}
		idx := pos + rel
		if nlRel := strings.Index(text[idx:], "\n"); nlRel > 0 {
			nl := idx + nlRel
			if json.Valid([]byte(text[idx+len(prefix) : nl])) {
				return idx
			}
		}
		pos = idx + 1
	}
	return -1
}

// renderUserSegments renders every hearth/1 segment of a user transcript entry
// as its own line: human/agent messages as "<name> <body>", system_events as a
// faint one-line note, chat_context scaffolding skipped. Returns nil when
// nothing is renderable. Unlike the phone app the TUI has no show-system
// toggle, so system_events render faint rather than being hidden — a power-user
// surface benefits from seeing them and there'd be no way to reveal them.
func renderUserSegments(text, userName string) []string {
	var lines []string
	for _, seg := range parseHearthSegments(text) {
		switch seg.kind {
		case "system_event":
			lines = append(lines, renderSystemSegment(seg))
		case "chat_context":
			// injected agent scaffolding — not part of the human conversation
		default:
			body := strings.TrimSpace(seg.body)
			if body == "" {
				continue
			}
			name := seg.fromName
			if name == "" {
				name = userName
			}
			lines = append(lines, userPrefix(name)+" "+body)
		}
	}
	return lines
}

// renderSystemSegment renders a hearth/1 system_event segment as a faint line,
// labeled by its event name (falling back to a truncated body).
func renderSystemSegment(seg hearthSegment) string {
	label := seg.event
	if label == "" {
		label = truncate(strings.TrimSpace(seg.body), 60)
	}
	return activityStyle.Render("· " + label)
}

// userPrefix renders the leading label for a user-attributed line.
// Falls back to "you" when name is empty.
func userPrefix(name string) string {
	if name == "" {
		name = "you"
	}
	return youStyle.Render(name)
}

// agentPrefix renders the leading label for an agent-attributed line.
// Returns "" when name is empty (caller can omit the prefix).
func agentPrefix(name string) string {
	if name == "" {
		return ""
	}
	return agentStyle.Render(name)
}

func renderClaudeMessage(role string, content json.RawMessage, userName, agentName string) string {
	// content is either an array of blocks or a plain string.
	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err == nil {
		return renderClaudeBlocks(role, blocks, userName, agentName)
	}
	var contentStr string
	if err := json.Unmarshal(content, &contentStr); err == nil {
		return renderClaudeText(role, contentStr, userName, agentName)
	}
	return ""
}

type contentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

func renderClaudeBlocks(role string, blocks []contentBlock, userName, agentName string) string {
	var lines []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text := strings.TrimSpace(b.Text)
			if text == "" {
				continue
			}
			if role == "user" {
				lines = append(lines, renderUserSegments(text, userName)...)
			} else {
				if agentName != "" {
					lines = append(lines, agentPrefix(agentName)+" "+text)
				} else {
					lines = append(lines, text)
				}
			}
		case "tool_use":
			preview := summarizeToolInput(b.Name, b.Input)
			label := "⚙ " + b.Name
			if preview != "" {
				label += " " + preview
			}
			lines = append(lines, toolUseStyle.Render(label))
		case "tool_result":
			body := contentToString(b.Content)
			if body != "" {
				lines = append(lines, toolResultStyle.Render("✓ "+truncate(body, 200)))
			} else {
				lines = append(lines, toolResultStyle.Render("✓"))
			}
		case "thinking":
			// skip
		}
	}
	return strings.Join(lines, "\n")
}

func renderClaudeText(role, text, userName, agentName string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if role == "user" {
		lines := renderUserSegments(text, userName)
		if len(lines) == 0 {
			return ""
		}
		return strings.Join(lines, "\n")
	}
	if agentName != "" {
		return agentPrefix(agentName) + " " + text
	}
	return text
}

// contentToString tries to coax a content block payload into a single line of
// readable text. tool_result contents can be a string or an array of nested
// blocks; we handle both, falling back to the raw JSON.
func contentToString(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(content, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var nested []contentBlock
	if err := json.Unmarshal(content, &nested); err == nil {
		var parts []string
		for _, b := range nested {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	return strings.TrimSpace(string(content))
}

// summarizeToolInput pulls the most useful field out of a tool_use input map
// so the rendered line shows the actual command/path/pattern instead of the
// full JSON blob. Falls back to a truncated raw dump for unknown tools.
func summarizeToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var asMap map[string]interface{}
	if err := json.Unmarshal(input, &asMap); err != nil {
		return truncate(string(input), 80)
	}
	switch name {
	case "Bash":
		if cmd, ok := asMap["command"].(string); ok {
			return "$ " + truncate(cmd, 80)
		}
	case "Read", "Write", "Edit", "NotebookEdit":
		if path, ok := asMap["file_path"].(string); ok {
			return path
		}
	case "Glob", "Grep":
		if pat, ok := asMap["pattern"].(string); ok {
			return pat
		}
	case "WebFetch":
		if u, ok := asMap["url"].(string); ok {
			return u
		}
	case "WebSearch":
		if q, ok := asMap["query"].(string); ok {
			return q
		}
	case "TodoWrite":
		return "(todos updated)"
	}
	return truncate(string(input), 80)
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// renderActivityEvent formats an activity_event message as a single faint line
// for the focused transcript pane.
func renderActivityEvent(event, toolName, project string) string {
	parts := []string{"·"}
	if event != "" {
		parts = append(parts, event)
	}
	if toolName != "" {
		parts = append(parts, toolName)
	}
	if project != "" {
		parts = append(parts, fmt.Sprintf("[%s]", project))
	}
	return activityStyle.Render(strings.Join(parts, " "))
}

// =============================================================================
// permission modal
// =============================================================================

var (
	modalBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("33")).
			Padding(1, 2)
	modalHeaderStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("33"))
	modalLabelStyle = lipgloss.NewStyle().
			Faint(true)
	modalDetailStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("36"))
	modalHelpStyle = lipgloss.NewStyle().
			Faint(true).
			Italic(true)

	missedStyle = lipgloss.NewStyle().
			Faint(true).
			Foreground(lipgloss.Color("214"))
)

// renderPermissionModal draws the centered permission request box. The caller
// is responsible for placing it in the viewport area (e.g. via lipgloss.Place).
func renderPermissionModal(p *talkPendingPermission, terminalWidth int) string {
	boxWidth := 64
	if terminalWidth > 0 && terminalWidth-4 < boxWidth {
		boxWidth = terminalWidth - 4
	}
	if boxWidth < 30 {
		boxWidth = 30
	}

	var body strings.Builder
	body.WriteString(modalHeaderStyle.Render("Permission request"))
	body.WriteString("\n\n")
	body.WriteString(modalLabelStyle.Render("Tool:    ") + p.toolName + "\n")
	if p.project != "" {
		body.WriteString(modalLabelStyle.Render("Project: ") + p.project + "\n")
	}
	body.WriteString("\n")
	if preview := summarizeToolInput(p.toolName, p.toolInput); preview != "" {
		body.WriteString(modalDetailStyle.Render(preview))
	} else {
		body.WriteString(modalLabelStyle.Render("(no preview available)"))
	}

	return modalBoxStyle.Width(boxWidth).Render(body.String())
}

// renderMissedRequest formats a single backfilled MissedRequest as a faint
// one-line entry tagged "missed".
func renderMissedRequest(toolName, project string, toolInput json.RawMessage) string {
	preview := summarizeToolInput(toolName, toolInput)
	parts := []string{"⏱ missed", toolName}
	if preview != "" {
		parts = append(parts, "—", preview)
	}
	if project != "" {
		parts = append(parts, fmt.Sprintf("[%s]", project))
	}
	return missedStyle.Render(strings.Join(parts, " "))
}

// =============================================================================
// help overlay
// =============================================================================

var helpKeyStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.Color("33"))

// renderHelp draws the centered help overlay shown when the user presses '?'.
func renderHelp(terminalWidth int) string {
	boxWidth := 56
	if terminalWidth > 0 && terminalWidth-4 < boxWidth {
		boxWidth = terminalWidth - 4
	}
	if boxWidth < 30 {
		boxWidth = 30
	}

	row := func(key, label string) string {
		return helpKeyStyle.Render(fmt.Sprintf("  %-12s", key)) + label
	}

	var b strings.Builder
	b.WriteString(modalHeaderStyle.Render("hearth talk") + "\n\n")
	b.WriteString(modalLabelStyle.Render("Normal mode") + "\n")
	b.WriteString(row("tab", "switch focused agent") + "\n")
	b.WriteString(row("shift+tab", "switch backwards") + "\n")
	b.WriteString(row("pgup / pgdn", "scroll the transcript") + "\n")
	b.WriteString(row("enter", "send the input as a message") + "\n")
	b.WriteString(row("?", "show this help") + "\n")
	b.WriteString(row("ctrl+c, esc", "quit") + "\n")
	b.WriteString("\n")
	b.WriteString(modalLabelStyle.Render("Permission modal") + "\n")
	b.WriteString(row("a", "allow") + "\n")
	b.WriteString(row("d", "deny") + "\n")
	b.WriteString(row("A", "always allow") + "\n")
	b.WriteString(row("esc", "dismiss") + "\n")
	b.WriteString("\n")
	b.WriteString(modalHelpStyle.Render("(press any key to close)"))

	return modalBoxStyle.Width(boxWidth).Render(b.String())
}

// statusHints returns a faint key-hint suffix for the bottom status bar.
// Different content depending on whether the help/modal layers are active.
func statusHints(modalActive, helpActive bool) string {
	switch {
	case modalActive:
		return "" // modal already shows its own help line
	case helpActive:
		return "(any key) close"
	default:
		return "tab switch · pgup/pgdn scroll · ? help · ctrl+c quit"
	}
}
