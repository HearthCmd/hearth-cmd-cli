//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// interposeRequest is the JSON structure sent by the interpose library.
type interposeRequest struct {
	Type    string   `json:"type"`               // "open", "spawn", "connect", "read"
	Path    string   `json:"path,omitempty"`     // file path or binary path
	OldPath string   `json:"old_path,omitempty"` // source path for rename edits
	Args    []string `json:"args,omitempty"`     // argv for spawn
	Flags   string   `json:"flags,omitempty"`    // "w", "rw", "rename" for open
	Host    string   `json:"host,omitempty"`     // hostname for connect
	IP      string   `json:"ip,omitempty"`       // IP for connect
	Port    int      `json:"port,omitempty"`     // port for connect
	PID     int      `json:"pid,omitempty"`
	Project *bool    `json:"project,omitempty"` // true if file is within project dir
}

// interposeResponse is sent back to the interpose library.
type interposeResponse struct {
	Allow     bool   `json:"allow"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Message   string `json:"message,omitempty"`
}

// interposeRelay is a per-socket reference to the agent instance's Relay.
// Created by startInterposeSock, set via SetRelay after the Relay is created.
type interposeRelay struct {
	mu sync.Mutex
	r  *Relay
}

func (ir *interposeRelay) SetRelay(r *Relay) {
	ir.mu.Lock()
	ir.r = r
	ir.mu.Unlock()
}

func (ir *interposeRelay) ClearRelay(r *Relay) {
	ir.mu.Lock()
	if ir.r == r {
		ir.r = nil
	}
	ir.mu.Unlock()
}

func (ir *interposeRelay) GetRelay() *Relay {
	ir.mu.Lock()
	r := ir.r
	ir.mu.Unlock()
	return r
}

// formatToolDetail returns a human-readable summary of the tool input for display.
func formatToolDetail(toolName string, toolInput map[string]interface{}) string {
	switch toolName {
	case "Bash":
		if cmd, ok := toolInput["command"].(string); ok {
			return cmd
		}
	case "Read", "Write", "Edit":
		if fp, ok := toolInput["file_path"].(string); ok {
			return fp
		}
	case "WebFetch":
		if u, ok := toolInput["url"].(string); ok {
			return u
		}
	}
	return fmt.Sprintf("%v", toolInput)
}

// startInterposeSock creates a Unix socket listener for interpose permission
// requests and returns the socket path and a per-instance relay reference.
// The caller must call SetRelay on the returned interposeRelay once the
// Relay is created.
func startInterposeSock(id, agent string) (string, func(), *interposeRelay, error) {
	// Unix socket paths are limited to ~104 bytes on macOS, keep it short
	sockPath := "/tmp/gl-" + id[:8] + ".sock"

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		// If the socket file exists but nobody is listening, it's stale — remove and retry.
		if conn, dialErr := net.DialTimeout("unix", sockPath, 500*time.Millisecond); dialErr != nil {
			os.Remove(sockPath)
			listener, err = net.Listen("unix", sockPath)
		} else {
			conn.Close()
			// Socket is live — another agent instance owns it
		}
		if err != nil {
			return "", nil, nil, fmt.Errorf("interpose socket %s: %w", sockPath, err)
		}
	}

	ir := &interposeRelay{}
	go handleInterposeSock(listener, agent, ir)

	cleanup := func() {
		listener.Close()
		// os.Remove not needed — listener.Close() removes the socket file
	}
	return sockPath, cleanup, ir, nil
}

func handleInterposeSock(listener net.Listener, agent string, ir *interposeRelay) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return // listener closed
		}
		go handleInterposeConn(conn, agent, ir)
	}
}

// isSafeCommand checks if a Bash command can be auto-allowed without
// prompting the user. A command is safe if its base binary is read-only
// AND it contains no output redirects (which could write to files).
func isSafeCommand(cmd string) bool {
	// An agent posting a chat message the way it's taught. Recognized whole,
	// so the message itself is never scanned (see isChatReplyHeredoc).
	if isChatReplyHeredoc(cmd) {
		return true
	}

	cmdName := commandName(cmd)
	if cmdName == "" {
		return false
	}

	// A hearth command carries text that must stay text — above all a chat
	// message, where "$(…)" or a backtick in the words would run as a command.
	// So it's auto-allowed only as the exact heredoc above, or as a single
	// command in which bash expands nothing.
	if chatReplyCommandNames[cmdName] {
		return isPlainSimpleCommand(cmd)
	}

	// Check for output redirects: any unquoted > means the command can
	// write to a file, even if the binary itself is read-only.
	if mayRedirectOutput(cmd) {
		return false
	}
	return isSafeProgram(cmdName)
}

// isSafeRequest decides auto-allow for one intercepted exec. A shell running a
// script (-c) is judged on the script (isSafeCommand). Any other exec is a
// program started with its final argv: no shell will read those arguments
// again, so nothing in them can become a command. hearth itself is therefore
// safe whatever its arguments (a chat message passed inline included). Other
// programs keep the long-standing check, whose '>' scan also catches programs
// that write files through their own syntax (awk '{print > "f"}').
func isSafeRequest(req interposeRequest, cmd string) bool {
	if req.Type != "spawn" || spawnRunsShellScript(req) {
		return isSafeCommand(cmd)
	}
	prog := req.Path
	if prog == "" && len(req.Args) > 0 {
		prog = req.Args[0]
	}
	if i := strings.LastIndexByte(prog, '/'); i >= 0 {
		prog = prog[i+1:]
	}
	if chatReplyCommandNames[prog] {
		return true
	}
	return !legacyMayRedirect(cmd) && isSafeProgram(commandName(cmd))
}

// spawnRunsShellScript reports whether an exec passes a script with -c (the
// same test translateInterposeRequest uses to pick the command it shows).
func spawnRunsShellScript(req interposeRequest) bool {
	for i, a := range req.Args {
		if a == "-c" && i+1 < len(req.Args) {
			return true
		}
	}
	return false
}

// legacyMayRedirect is the original quote-aware '>' scan, kept exactly for
// direct program execs (see isSafeRequest).
func legacyMayRedirect(cmd string) bool {
	inSingle := false
	inDouble := false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if c == '\\' && inDouble && i+1 < len(cmd) {
			i++ // skip escaped char
			continue
		}
		if c == '>' && !inSingle && !inDouble {
			return true
		}
	}
	return false
}

// commandName is the basename of the first word that isn't an env assignment
// (VAR=val), or "".
func commandName(cmd string) string {
	for _, f := range strings.Fields(cmd) {
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			continue // env var assignment
		}
		if idx := strings.LastIndex(f, "/"); idx >= 0 {
			f = f[idx+1:]
		}
		return f
	}
	return ""
}

// isSafeProgram reports whether a program is read-only (writes nothing without
// a redirect).
func isSafeProgram(cmdName string) bool {
	// Read-only commands that cannot modify files without redirects
	switch cmdName {
	case "pwd", "echo", "printf", "wc", "ls", "cat", "head", "tail",
		"grep", "awk", "find", "file", "stat", "realpath", "readlink",
		"dirname", "basename", "which", "command", "type",
		"ps", "uname", "whoami", "hostname", "id", "arch", "nproc",
		"uptime", "df", "free", "env", "printenv",
		"defaults", "system_profiler", "security", "ioreg",
		"dpkg", "lsb_release", "lscpu", "lsblk",
		"date", "cal", "true", "false", "test", "[",
		"sort", "uniq", "tr", "cut", "less", "more",
		"hearth", "hearth-dev", "hearth-local":
		return true
	}
	return false
}

// mayRedirectOutput reports whether a shell command might write a file through
// an output redirect (an unquoted '>'), or contains anything this scanner does
// not model exactly as bash does. It errs toward true: a wrong true costs an
// approval prompt, a wrong false could auto-allow a write.
//
// The invariant: text is only ever passed over as inert (quoted, or a
// heredoc body) where bash is guaranteed to treat it as inert too. So the
// scanner follows bash's quoting exactly for the constructs it accepts —
// '...', "...", backslashes, $'...', $(...) (a fresh context, even inside
// double quotes) and ${...} — and refuses everything whose parsing it doesn't
// model: backticks, comments, heredocs, case (its unbalanced ')' would end a
// $(...) early), arithmetic, a '(' glued to a word (a zsh glob qualifier can
// run code), anything but plain forms inside ${...}, and anything unbalanced
// or unterminated. It has to hold for zsh as well as bash: Codex runs
// commands through zsh.
//
// Heredocs are always refused here. The one heredoc an agent is taught — a
// chat message on stdin — is recognized, in full, by isChatReplyHeredoc before
// this runs.
func mayRedirectOutput(cmd string) bool {
	end, bad := scanShell(cmd, 0, 0)
	return bad || end != len(cmd)
}

// shellWordBreak holds the characters after which a new shell word starts.
const shellWordBreak = " \t\n;&|()<>"

// atWordStart reports whether position i begins a shell word.
func atWordStart(s string, i int) bool {
	return i == 0 || strings.IndexByte(shellWordBreak, s[i-1]) >= 0
}

// scanShell scans unquoted shell text from i: to the end when closer is 0, or
// to the matching closer (')' for $(...), '}' for ${...}) at nesting depth 0,
// returning that closer's index. bad reports a possible output redirect or an
// unmodelled construct.
func scanShell(s string, i int, closer byte) (int, bool) {
	parens, braces := 0, 0
	n := len(s)
	for i < n {
		c := s[i]
		switch c {
		case '\'':
			k := strings.IndexByte(s[i+1:], '\'')
			if k < 0 {
				return i, true
			}
			i += k + 2
			continue
		case '"':
			j, bad := scanDoubleQuoted(s, i+1)
			if bad {
				return i, true
			}
			i = j
			continue
		case '\\':
			i += 2 // an escaped char (or a line continuation) is literal
			continue
		case '`':
			return i, true
		case '>':
			return i, true
		case '<':
			if i+1 < n && s[i+1] == '<' {
				if i+2 < n && s[i+2] == '<' {
					i += 3 // here-string: its word is scanned like any other
					continue
				}
				return i, true // a heredoc: never skipped here
			}
		case '#':
			if atWordStart(s, i) {
				return i, true // a comment
			}
		case 'c':
			if atWordStart(s, i) && strings.HasPrefix(s[i:], "case") &&
				(i+4 == n || strings.IndexByte(shellWordBreak, s[i+4]) >= 0) {
				return i, true
			}
		case '$':
			if i+1 < n {
				switch s[i+1] {
				case '\'':
					j, ok := skipANSICQuoted(s, i+2)
					if !ok {
						return i, true
					}
					i = j
					continue
				case '(':
					if i+2 < n && s[i+2] == '(' {
						// $((…)): arithmetic, where quotes are not quoting.
						return i, true
					}
					j, bad := scanShell(s, i+2, ')')
					if bad {
						return i, true
					}
					i = j + 1
					continue
				case '{':
					j, bad := scanParamRestricted(s, i+2)
					if bad {
						return i, true
					}
					i = j + 1
					continue
				case '[':
					return i, true // $[…]: old-style arithmetic
				}
			}
		case '(':
			if !atWordStart(s, i) {
				// Glued to a word: in zsh a glob qualifier, which can run code
				// (ls *(e:'…':)); in bash an array or function. Not modelled.
				return i, true
			}
			if i+1 < n && s[i+1] == '(' {
				return i, true // ((…)): arithmetic
			}
			parens++
		case ')':
			if parens == 0 {
				if closer == ')' {
					return i, false
				}
				return i, true // unbalanced
			}
			parens--
		case '{':
			if closer == '}' {
				braces++
			}
		case '}':
			if closer == '}' {
				if braces == 0 && parens == 0 {
					return i, false
				}
				if braces > 0 {
					braces--
				}
			}
		}
		i++
	}
	if closer != 0 || parens != 0 {
		return i, true // unterminated
	}
	return i, false
}

// scanDoubleQuoted scans a double-quoted string's contents from i (just past
// the opening quote) and returns the index just past the closing quote.
func scanDoubleQuoted(s string, i int) (int, bool) {
	n := len(s)
	for i < n {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return i + 1, false
		case '`':
			return i, true
		case '$':
			if i+1 < n {
				switch s[i+1] {
				case '(':
					if i+2 < n && s[i+2] == '(' {
						return i, true // $((…)): arithmetic
					}
					// Command substitution parses a fresh context, even here.
					j, bad := scanShell(s, i+2, ')')
					if bad {
						return i, true
					}
					i = j + 1
					continue
				case '{':
					j, bad := scanParamRestricted(s, i+2)
					if bad {
						return i, true
					}
					i = j + 1
					continue
				}
			}
		}
		i++
	}
	return i, true // unterminated
}

// scanParamRestricted scans a ${...} from i (just past "${") and returns the
// index of its closing brace. Only plain forms pass — "${HOME}/x",
// ${x:-default}, ${x#prefix}. Quoting inside it (whose treatment has varied
// between bash versions, and differs inside double quotes), backslashes,
// backticks, substitutions, subscripts (evaluated as arithmetic) and zsh
// parameter flags are refused.
func scanParamRestricted(s string, i int) (int, bool) {
	depth := 0
	for ; i < len(s); i++ {
		switch s[i] {
		case '\'', '"', '`', '\\', '[', '(':
			// '(' also covers zsh parameter flags like ${(e)x}, which
			// evaluate the value as code.
			return i, true
		case '{':
			depth++
		case '}':
			if depth == 0 {
				return i, false
			}
			depth--
		}
	}
	return i, true
}

// isPlainSimpleCommand reports whether cmd is one simple command in which bash
// expands and interprets nothing: plain words, '...' strings, and "..." strings
// with no $, backtick, or backslash other than \" and \\. Outside quotes, no
// $, backtick, backslash, newline, operator, redirect, parenthesis or comment.
func isPlainSimpleCommand(cmd string) bool {
	n := len(cmd)
	for i := 0; i < n; i++ {
		switch c := cmd[i]; c {
		case '\'':
			k := strings.IndexByte(cmd[i+1:], '\'')
			if k < 0 {
				return false
			}
			i += k + 1
		case '"':
			j := i + 1
			for ; j < n && cmd[j] != '"'; j++ {
				switch cmd[j] {
				case '$', '`':
					return false
				case '\\':
					if j+1 < n && (cmd[j+1] == '"' || cmd[j+1] == '\\') {
						j++
						continue
					}
					return false
				}
			}
			if j >= n {
				return false
			}
			i = j
		case '$', '`', '\\', '\n', '\r', ';', '&', '|', '<', '>', '(', ')':
			return false
		case '#':
			if atWordStart(cmd, i) {
				return false
			}
		}
	}
	return true
}

// skipANSICQuoted skips a $'...' string from i (just past "$'"): a backslash
// escapes the next char, including a quote. Returns the index just past the
// closing quote.
func skipANSICQuoted(s string, i int) (int, bool) {
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '\'':
			return i + 1, true
		}
		i++
	}
	return i, false
}

// Shapes allowed in the first line of a chat-reply heredoc: plain words that no
// shell expansion or quoting can touch, and a single-quoted delimiter.
var (
	chatReplyPlainWordRe  = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)
	chatReplyHeredocOpRe  = regexp.MustCompile(`^<<'([A-Za-z0-9_]+)'$`)
	chatReplyCommandNames = map[string]bool{"hearth": true, "hearth-dev": true, "hearth-local": true}
)

// isChatReplyHeredoc recognizes, in full, the one command an agent is taught
// for posting a chat message (docs/agent-chat-message-transport.md):
//
//	hearth chat reply --room <id> [plain words…] <<'MARKER'
//	…the message: any text at all…
//	MARKER
//
// and nothing after it but whitespace. Only this exact shape lets the message
// pass unscanned, because only here can nothing in it run: the first line is
// plain words, the quoted marker makes bash read the body literally, the body
// ends at the first line equal to the marker, and nothing follows. A message
// that itself contains the marker line would end the heredoc early and bash
// would run the rest — so that doesn't match, and gets an approval prompt.
func isChatReplyHeredoc(cmd string) bool {
	nl := strings.IndexByte(cmd, '\n')
	if nl < 0 {
		return false
	}
	head, rest := cmd[:nl], cmd[nl+1:]
	// Blanks are exactly space and tab to bash; anything else is part of a word.
	words := strings.FieldsFunc(head, func(r rune) bool { return r == ' ' || r == '\t' })
	if len(words) < 4 {
		return false
	}
	name := words[0]
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if !chatReplyCommandNames[name] || words[1] != "chat" || words[2] != "reply" {
		return false
	}
	m := chatReplyHeredocOpRe.FindStringSubmatch(words[len(words)-1])
	if m == nil {
		return false
	}
	for _, w := range words[:len(words)-1] {
		if !chatReplyPlainWordRe.MatchString(w) {
			return false
		}
	}
	marker := m[1]
	for pos := 0; ; {
		lineEnd := strings.IndexByte(rest[pos:], '\n')
		line, next := rest[pos:], len(rest)
		if lineEnd >= 0 {
			line, next = rest[pos:pos+lineEnd], pos+lineEnd+1
		}
		if line == marker {
			return strings.Trim(rest[next:], " \t\n") == ""
		}
		if lineEnd < 0 {
			return false // no terminator
		}
		pos = next
	}
}

func handleInterposeConn(conn net.Conn, agent string, ir *interposeRelay) {
	defer conn.Close()

	// Try to receive with ancillary data (SCM_RIGHTS for seccomp fd)
	line, seccompFd := recvWithAncillary(conn)
	if line == nil {
		respond(conn, interposeResponse{Allow: false})
		return
	}

	// Duplicate keys are rejected BEFORE unmarshal, because encoding/json keeps
	// the LAST value for a repeated key. That turns any string-injection flaw in
	// the hook into value substitution rather than a parse error: a `connect`
	// request whose hostname is
	//
	//     evil.com","host":"api.anthropic.com
	//
	// arrives as {"host":"evil.com","host":"api.anthropic.com",...}, and we would
	// gate — and display on the phone — the second one while the socket goes to
	// the first. Worse than a misleading prompt: an existing auto-approve rule for
	// the allowlisted host fires and nobody is asked at all.
	//
	// The hook is being fixed to escape that field, but this check does not
	// depend on the hook being right, and it holds for every field and every
	// future request type. See docs/interpose-argv-json-escaping-bug.md and its reply.
	if err := rejectDuplicateJSONKeys(line); err != nil {
		log.Printf("Interpose: %s %v [%d bytes] %s",
			interposeParseErrorMarker, err, len(line), interposeRequestPrefix(line))
		respond(conn, interposeResponse{
			Allow:   false,
			Message: "permission gate rejected a malformed exec request (duplicate field)",
		})
		return
	}

	var req interposeRequest
	if err := json.Unmarshal(line, &req); err != nil {
		// Log a bounded, sanitized prefix. A bare "bad request" reaches the
		// operator as an inscrutable exit 126 with nothing on the server, and
		// diagnosing the last one meant reading a host's daemon log by hand.
		// The prefix is what distinguishes "arrived truncated" from "arrived
		// mis-escaped" without another round trip.
		log.Printf("Interpose: %s %v [%d bytes] %s",
			interposeParseErrorMarker, err, len(line), interposeRequestPrefix(line))
		respond(conn, interposeResponse{
			Allow:   false,
			Message: "permission gate could not parse the exec request",
		})
		return
	}

	// Handle seccomp fd handoff (Linux only)
	if req.Type == "seccomp_fd" && seccompFd >= 0 {
		log.Printf("Interpose: received seccomp notification fd %d", seccompFd)
		go runSeccompSupervisor(seccompFd, agent, ir)
		return
	}

	log.Printf("Interpose: %s %s", req.Type, interposeRequestSummary(req))

	// Translate to server permission request format
	toolName, toolInput := translateInterposeRequest(req)

	// Auto-allow safe commands (read-only with no output redirects)
	if toolName == "Bash" {
		if cmd, ok := toolInput["command"].(string); ok && isSafeRequest(req, cmd) {
			log.Printf("Interpose: auto-allow safe command: %s", cmd)
			respond(conn, interposeResponse{Allow: true})
			return
		}
	}

	payload := map[string]interface{}{
		"agent":           agent,
		"hook_event_name": "PermissionRequest",
		"tool_name":       toolName,
		"tool_input":      toolInput,
	}
	if req.Project != nil {
		payload["project_file"] = *req.Project
	}

	// Get the relay for WebSocket and terminal prompt access
	relay := ir.GetRelay()

	if relay == nil || relay.wsConn == nil {
		log.Printf("Interpose: no relay/websocket available, denying")
		respond(conn, interposeResponse{Allow: false})
		return
	}

	resp := racePermission(relay, toolName, toolInput, payload)
	log.Printf("Interpose: %s %s → %v", toolName, interposeRequestSummary(req),
		map[bool]string{true: "allow", false: "deny"}[resp.Allow])
	respond(conn, resp)
	if resp.Interrupt && relay.killFunc != nil {
		log.Printf("Interpose: deny+stop — killing agent process")
		relay.killFunc()
	}
}

// wsPermission sends a permission request over the WebSocket and waits for
// the server's response. Returns the response or an error if ctx is cancelled.
func wsPermission(ctx context.Context, ws WSConn, requestID string, payload map[string]interface{}) (interposeResponse, error) {
	respCh := ws.RegisterPending(requestID)
	defer ws.RemovePending(requestID)

	// Copy payload and add request_id (avoid mutating shared map)
	data := make(map[string]interface{}, len(payload)+1)
	for k, v := range payload {
		data[k] = v
	}
	data["request_id"] = requestID

	msg := map[string]interface{}{
		"type": "permission_request",
		"data": data,
	}
	encoded, err := json.Marshal(msg)
	if err != nil {
		return interposeResponse{Allow: false}, err
	}

	ws.SendText(encoded)

	select {
	case respData := <-respCh:
		var resp struct {
			Behavior  string `json:"behavior"`
			Message   string `json:"message"`
			Interrupt bool   `json:"interrupt"`
		}
		if err := json.Unmarshal(respData, &resp); err != nil {
			return interposeResponse{Allow: false}, err
		}
		return interposeResponse{
			Allow:     resp.Behavior == "allow",
			Interrupt: resp.Interrupt,
			Message:   resp.Message,
		}, nil
	case <-ctx.Done():
		return interposeResponse{Allow: false}, ctx.Err()
	}
}

// racePermission sends the permission request to the server via WebSocket
// and waits for the server's decision (phone approval, rule match, or timeout).
// There's no terminal prompt fallback — agents run detached.
func racePermission(relay *Relay, toolName string, toolInput map[string]interface{}, payload map[string]interface{}) interposeResponse {
	requestID := generateUUID()
	ws := relay.wsConn
	wsCtx, wsCancel := context.WithCancel(context.Background())
	defer wsCancel()

	ch := make(chan interposeResponse, 1)
	go func() {
		log.Printf("Interpose: sending WS permission request %s for %s", requestID, toolName)
		r, err := wsPermission(wsCtx, ws, requestID, payload)
		if err != nil {
			if wsCtx.Err() != nil {
				return // cancelled
			}
			log.Printf("Interpose: WS permission error for %s (req %s): %v", toolName, requestID, err)
			ch <- interposeResponse{Allow: false}
			return
		}
		ch <- r
	}()

	select {
	case r := <-ch:
		log.Printf("Interpose: permission %s for %s",
			map[bool]string{true: "allowed", false: "denied"}[r.Allow], toolName)
		return r
	case <-relay.shutdownCh:
		log.Printf("Interpose: relay shutting down, denying %s", toolName)
		return interposeResponse{Allow: false}
	case <-time.After(600 * time.Second):
		log.Printf("Interpose: permission request timed out for %s", toolName)
		return interposeResponse{Allow: false}
	}
}

func respond(conn net.Conn, r interposeResponse) {
	data, _ := json.Marshal(r)
	data = append(data, '\n')
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	conn.Write(data)
}

// translateInterposeRequest converts an interpose event to the server's
// tool_name + tool_input format, matching the hook permission request schema.
func translateInterposeRequest(req interposeRequest) (string, map[string]interface{}) {
	switch req.Type {
	case "read":
		return "Read", map[string]interface{}{
			"file_path": req.Path,
		}
	case "open":
		// Rename with old_path means the target already exists — it's an edit.
		// Compute old/new strings for display (matches Claude's Edit format).
		if req.Flags == "rename" && req.OldPath != "" {
			oldStr, newStr := computeRenameEdit(req.Path, req.OldPath)
			if oldStr != "" || newStr != "" {
				return "Edit", map[string]interface{}{
					"file_path":  req.Path,
					"old_string": oldStr,
					"new_string": newStr,
				}
			}
			return "Edit", map[string]interface{}{
				"file_path": req.Path,
			}
		}
		// If the file already exists, it's an edit; otherwise a write/create.
		toolName := "Write"
		if _, err := os.Stat(req.Path); err == nil {
			toolName = "Edit"
		}
		return toolName, map[string]interface{}{
			"file_path": req.Path,
		}
	case "spawn":
		// Codex apply_patch: the codex binary is spawned with patch content as args.
		// Translate as an Edit tool with the filename from the patch.
		if strings.Contains(req.Path, "codex") && len(req.Args) > 1 &&
			strings.Contains(req.Args[len(req.Args)-1], "*** Begin Patch") {
			patch := req.Args[len(req.Args)-1]
			// Extract filename and old/new from patch content
			filePath := "unknown"
			if idx := strings.Index(patch, "*** Update File: "); idx >= 0 {
				line := patch[idx+17:]
				if nl := strings.IndexByte(line, '\n'); nl >= 0 {
					filePath = line[:nl]
				}
			} else if idx := strings.Index(patch, "*** Add File: "); idx >= 0 {
				line := patch[idx+14:]
				if nl := strings.IndexByte(line, '\n'); nl >= 0 {
					filePath = line[:nl]
				}
			}
			oldStr, newStr := extractPatchStrings(patch)
			return "Edit", map[string]interface{}{
				"file_path":  filePath,
				"old_string": oldStr,
				"new_string": newStr,
			}
		}

		// Join args as the command string
		cmd := ""
		if len(req.Args) > 0 {
			// For shell -c, the command is typically in args[2]
			shellCmd := ""
			for i, a := range req.Args {
				if a == "-c" && i+1 < len(req.Args) {
					shellCmd = req.Args[i+1]
				}
			}
			if shellCmd != "" {
				cmd = shellCmd
			}
			if cmd == "" {
				// Not a shell -c, join all args
				for i, a := range req.Args {
					if i > 0 {
						cmd += " "
					}
					cmd += a
				}
			}
		}
		// Unwrap Claude's eval wrapper:
		//   source .../shell-snapshots/... && setopt ... && eval 'CMD' < /dev/null && pwd ...
		cmd = unwrapEvalCommand(cmd)
		// Unwrap Gemini's shopt wrapper:
		//   shopt -u ...; { ACTUAL_CMD }; __code=$?; ...
		cmd = unwrapGeminiCommand(cmd)
		// Unwrap Codex's shell snapshot wrapper:
		//   if . '.codex/shell_snapshots/UUID.sh' ...; fi\n\nexec '/bin/zsh' -c 'ACTUAL_CMD'
		cmd = unwrapCodexCommand(cmd)
		return "Bash", map[string]interface{}{
			"command": cmd,
		}
	case "connect":
		url := "https://" + req.Host
		if req.Port != 0 && req.Port != 443 {
			url = req.IP
		}
		return "WebFetch", map[string]interface{}{
			"url": url,
		}
	default:
		return "Generic", map[string]interface{}{
			"type": req.Type,
			"path": req.Path,
		}
	}
}

// unwrapEvalCommand extracts the actual command from Claude's Bash tool wrapper.
// Claude wraps commands as: source .../shell-snapshots/... && setopt ... && eval 'CMD' \< /dev/null && pwd ...
func unwrapEvalCommand(cmd string) string {
	idx := strings.Index(cmd, "&& eval ")
	if idx < 0 {
		return cmd
	}
	inner := cmd[idx+8:] // skip "&& eval "
	// Find the command between quotes, handling escaped quotes
	if len(inner) > 0 && (inner[0] == '\'' || inner[0] == '"') {
		quote := inner[0]
		inner = inner[1:]
		// Find closing delimiter: \< /dev/null or matching unescaped quote at end
		// Claude uses: eval 'cmd' \< /dev/null && pwd ...
		// or: eval "cmd" \< /dev/null && pwd ...
		endMarker := string(quote) + " \\< /dev/null"
		// The LAST occurrence is the wrapper's: a command that contains the
		// marker text itself must not cut the extracted command short.
		if end := strings.LastIndex(inner, endMarker); end >= 0 {
			inner = inner[:end]
		} else if end := strings.LastIndexByte(inner, quote); end >= 0 {
			inner = inner[:end]
		}
		if quote == '\'' {
			// Inside single quotes the only way to write ' is '\'' (close,
			// escaped quote, reopen). Undo it, or a quoted heredoc delimiter
			// or an apostrophe in a message scrambles quote tracking.
			inner = unescapeSingleQuoted(inner)
		} else {
			// Unescape \" sequences
			inner = strings.ReplaceAll(inner, "\\\"", "\"")
		}
	} else {
		inner = strings.ReplaceAll(inner, "\\\"", "\"")
	}
	if inner == "" {
		return cmd
	}
	return inner
}

// unescapeSingleQuoted undoes the shell idiom for a single quote inside a
// single-quoted string: close the quote, an escaped quote, reopen.
func unescapeSingleQuoted(s string) string {
	return strings.ReplaceAll(s, `'\''`, `'`)
}

// unwrapGeminiCommand extracts the actual command from Gemini's shell wrapper.
// Gemini wraps commands as: shopt -u ...; { ACTUAL_CMD }; __code=$?; ...
func unwrapGeminiCommand(cmd string) string {
	if !strings.HasPrefix(cmd, "shopt ") {
		return cmd
	}
	// Find "{ " and extract up to "};"
	braceStart := strings.Index(cmd, "{ ")
	if braceStart < 0 {
		return cmd
	}
	inner := cmd[braceStart+2:]
	// Find the closing "};" — but the inner command may contain "}" too,
	// so find "}; __code" which is Gemini's specific suffix
	if end := strings.Index(inner, "}; __code"); end >= 0 {
		inner = strings.TrimSpace(inner[:end])
	} else if end := strings.LastIndex(inner, "};"); end >= 0 {
		inner = strings.TrimSpace(inner[:end])
	}
	if inner == "" {
		return cmd
	}
	return inner
}

// unwrapCodexCommand extracts the actual command from Codex's shell wrapper.
// Codex wraps commands as:
//
//	if . '/path/.codex/shell_snapshots/UUID.sh' >/dev/null 2>&1; then :; fi\n\nexec '/bin/zsh' -c 'ACTUAL_CMD'
//
// We extract ACTUAL_CMD from the exec line.
func unwrapCodexCommand(cmd string) string {
	// Look for the exec pattern after a newline
	idx := strings.Index(cmd, "\nexec '")
	if idx < 0 {
		return cmd
	}
	execLine := cmd[idx+1:] // skip \n
	// Pattern: exec '/bin/zsh' -c 'ACTUAL_CMD'
	// Find -c ' and extract the command
	cIdx := strings.Index(execLine, " -c '")
	if cIdx < 0 {
		return cmd
	}
	inner := execLine[cIdx+5:] // skip " -c '"
	// Trim the closing quote, then undo Codex's '\'' escaping of single
	// quotes inside the single-quoted command.
	if len(inner) > 0 && inner[len(inner)-1] == '\'' {
		inner = inner[:len(inner)-1]
	}
	inner = unescapeSingleQuoted(inner)
	if inner == "" {
		return cmd
	}
	return inner
}

// extractPatchStrings parses a Codex apply_patch format and returns the
// removed lines and added lines as old_string/new_string.
// Format: lines starting with "-" are removed, "+" are added.
func extractPatchStrings(patch string) (string, string) {
	var oldLines, newLines []string
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "-") {
			oldLines = append(oldLines, line[1:])
		} else if strings.HasPrefix(line, "+") {
			newLines = append(newLines, line[1:])
		}
	}
	return strings.Join(oldLines, "\n"), strings.Join(newLines, "\n")
}

// computeRenameEdit reads the existing file (target) and the temp file (source),
// extracts the changed lines, and returns old/new strings matching Claude's Edit
// tool format. Returns ("","") on any error or if the files are identical.
func computeRenameEdit(targetPath, sourcePath string) (string, string) {
	oldBytes, err := os.ReadFile(targetPath)
	if err != nil {
		return "", ""
	}
	newBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		return "", ""
	}
	oldContent := string(oldBytes)
	newContent := string(newBytes)
	if oldContent == newContent {
		return "", ""
	}

	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	// Find first differing line
	start := 0
	for start < len(oldLines) && start < len(newLines) && oldLines[start] == newLines[start] {
		start++
	}

	// Find last differing line (from the end)
	endOld := len(oldLines) - 1
	endNew := len(newLines) - 1
	for endOld > start && endNew > start && oldLines[endOld] == newLines[endNew] {
		endOld--
		endNew--
	}

	oldStr := strings.Join(oldLines[start:endOld+1], "\n")
	newStr := strings.Join(newLines[start:endNew+1], "\n")

	// Truncate very large strings
	if len(oldStr) > 2048 {
		oldStr = oldStr[:2048] + "\n..."
	}
	if len(newStr) > 2048 {
		newStr = newStr[:2048] + "\n..."
	}

	return oldStr, newStr
}

func interposeRequestSummary(req interposeRequest) string {
	switch req.Type {
	case "open":
		return req.Path + " (" + req.Flags + ")"
	case "spawn":
		if len(req.Args) > 2 {
			return req.Path + " " + req.Args[len(req.Args)-1]
		}
		return req.Path
	case "connect":
		if req.Host != "" {
			return req.Host
		}
		return req.IP
	default:
		return req.Path
	}
}

// recvWithAncillary reads a line from the connection, optionally extracting
// an SCM_RIGHTS fd (used on Linux for seccomp notification fd handoff).
// Returns the line bytes and the fd (-1 if none).
func recvWithAncillary(conn net.Conn) ([]byte, int) {
	if runtime.GOOS == "linux" {
		// Try recvmsg to get ancillary data (SCM_RIGHTS)
		unixConn, ok := conn.(*net.UnixConn)
		if ok {
			rawConn, err := unixConn.SyscallConn()
			if err == nil {
				var buf [4096]byte
				oob := make([]byte, syscall.CmsgSpace(4))
				var n, oobn int
				var recvErr error

				rawConn.Read(func(fd uintptr) bool {
					n, oobn, _, _, recvErr = syscall.Recvmsg(int(fd), buf[:], oob, 0)
					return recvErr != syscall.EAGAIN
				})

				if recvErr == nil && n > 0 {
					seccompFd := -1
					if oobn > 0 {
						msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
						if err == nil {
							for _, msg := range msgs {
								if msg.Header.Level == syscall.SOL_SOCKET && msg.Header.Type == syscall.SCM_RIGHTS {
									fds, err := syscall.ParseUnixRights(&msg)
									if err == nil && len(fds) > 0 {
										seccompFd = fds[0]
									}
								}
							}
						}
					}

					// One recvmsg is not one request. This is a SOCK_STREAM
					// socket, so the kernel may deliver a message in pieces, and
					// the hook writes the payload and its trailing newline as two
					// separate writes — a second natural split point. Anything
					// larger than the 4 KiB buffer above was therefore handed to
					// json.Unmarshal truncated, and denied fail-closed with no
					// indication that size was the reason.
					//
					// The recvmsg has to stay: the SCM_RIGHTS fd for the seccomp
					// handoff rides the FIRST chunk, so this cannot simply become
					// a bufio read. Take the first chunk here, then read to the
					// delimiter like the darwin path always has.
					line, err := readToNewline(conn, append([]byte(nil), buf[:n]...))
					if err != nil {
						log.Printf("Interpose: %s reading request (%d bytes buffered): %v",
							interposeParseErrorMarker, len(line), err)
						if seccompFd >= 0 {
							syscall.Close(seccompFd)
						}
						return nil, -1
					}
					return line, seccompFd
				}
			}
		}
	}

	// Fallback (darwin, and linux when the socket isn't a *net.UnixConn):
	// buffered read to the delimiter. Bounded — bufio grows its buffer without
	// limit otherwise, so a hook that never sends a newline is an OOM.
	conn.SetReadDeadline(time.Now().Add(interposeReadTimeout))
	reader := bufio.NewReader(io.LimitReader(conn, interposeMaxRequestBytes))
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, -1
	}
	return line, -1
}

// interposeMaxRequestBytes caps one interpose request. The hook refuses to build
// anything above its own GL_MAX_REQ_BYTES (128 KiB), so this is that ceiling plus
// slack: past it we are not reading a request, we are being fed.
const interposeMaxRequestBytes = 256 * 1024

// interposeReadTimeout bounds how long we wait for the rest of a request. A hook
// that opened a connection and stalled must not hold a daemon goroutine.
const interposeReadTimeout = 5 * time.Second

// readToNewline extends have until it contains the delimiter, the cap is hit, or
// the deadline passes. A request that already arrived whole costs one IndexByte
// and no syscall, which is the overwhelmingly common case.
func readToNewline(conn net.Conn, have []byte) ([]byte, error) {
	if bytes.IndexByte(have, '\n') >= 0 {
		return have, nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(interposeReadTimeout))
	defer conn.SetReadDeadline(time.Time{})

	var chunk [4096]byte
	for len(have) < interposeMaxRequestBytes {
		n, err := conn.Read(chunk[:])
		if n > 0 {
			have = append(have, chunk[:n]...)
			if bytes.IndexByte(chunk[:n], '\n') >= 0 {
				return have, nil
			}
		}
		if err != nil {
			return have, err
		}
	}
	return have, fmt.Errorf("request exceeded %d bytes with no newline", interposeMaxRequestBytes)
}

// interposeParseErrorMarker is a stable, greppable token for every way an
// interpose request fails to become a decision. `grep interpose-bad-request` on a
// host's daemon log is the whole diagnostic, rather than knowing which phrasing
// to look for.
const interposeParseErrorMarker = "interpose-bad-request:"

// interposeRequestPrefixBytes bounds what a failed request contributes to the
// log. Enough to see the shape and where it broke; not enough to spill a
// multi-KB argument into a file we keep.
const interposeRequestPrefixBytes = 200

// interposeRequestPrefix renders a request that failed to parse as a bounded,
// single-line, ASCII-only string safe to put in a log.
//
// Arguments can carry secrets, so this is deliberately lossy: capped, control
// characters escaped, and every non-ASCII byte replaced. It exists to answer one
// question — did this arrive truncated, or mis-escaped? — not to reconstruct the
// command.
func interposeRequestPrefix(line []byte) string {
	truncated := false
	if len(line) > interposeRequestPrefixBytes {
		line = line[:interposeRequestPrefixBytes]
		truncated = true
	}

	var b strings.Builder
	b.WriteByte('"')
	for _, c := range line {
		switch {
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	if truncated {
		b.WriteString("...")
	}
	return b.String()
}

// interposeMaxJSONDepth bounds recursion in rejectDuplicateJSONKeys. Real
// requests are flat; deep nesting is someone probing the parser.
const interposeMaxJSONDepth = 32

// rejectDuplicateJSONKeys returns an error if any object in the document repeats
// a key.
//
// encoding/json silently keeps the last occurrence, which is a value-substitution
// primitive for anything that can inject a quote into a producer's string field.
// Standard library JSON offers no option for this, so it is a hand-rolled walk.
func rejectDuplicateJSONKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := checkJSONValue(dec, 0); err != nil {
		return err
	}
	return nil
}

func checkJSONValue(dec *json.Decoder, depth int) error {
	if depth > interposeMaxJSONDepth {
		return fmt.Errorf("nested deeper than %d", interposeMaxJSONDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		// A syntax error here is not this check's business — json.Unmarshal
		// reports it next, with a better message.
		return nil //nolint:nilerr // deliberate: defer to Unmarshal for syntax
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil //nolint:nilerr // defer to Unmarshal
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil
			}
			if seen[key] {
				return fmt.Errorf("duplicate key %q", key)
			}
			seen[key] = true
			if err := checkJSONValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, _ = dec.Token() // consume '}'
	case '[':
		for dec.More() {
			if err := checkJSONValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, _ = dec.Token() // consume ']'
	}
	return nil
}
