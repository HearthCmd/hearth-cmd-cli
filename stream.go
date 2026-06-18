//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func runStream(args []string) {
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	transcriptPath := fs.String("transcript", "", "Path to transcript file")
	agentInstanceID := fs.String("agent-instance-id", "", "AI agent instance ID")
	bridge := fs.String("bridge", "", "Bridge file path")
	agentFlag := fs.String("agent", "", "Agent runtime (claude, gemini)")
	fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "hearth stream: unexpected argument %q\n", fs.Arg(0))
		os.Exit(1)
	}

	if *transcriptPath == "" || *agentInstanceID == "" || *bridge == "" {
		fmt.Fprintf(os.Stderr, "hearth stream: missing required flags (--transcript, --agent-instance-id, --bridge)\n")
		os.Exit(1)
	}

	// Write PID file for the hook to check
	pidFile := filepath.Join(os.TempDir(), "hearth-stream-"+*agentInstanceID+".pid")
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d %s", os.Getpid(), *agentInstanceID)), 0644)
	defer os.Remove(pidFile)

	agent := *agentFlag
	if agent == "" {
		agent = resolveAgent("")
	}

	h, ok := getHarness(agent)
	if !ok {
		log.Printf("stream: no harness registered for %q", agent)
		os.Exit(1)
	}
	tailAndPump(*transcriptPath, *bridge, h.NewStreamTransformer())
}

// tailAndPump waits for the harness's on-disk transcript JSONL to
// appear, then tails it line-by-line, feeding each complete line
// through the per-harness StreamTransformer and writing the resulting
// bridge-shape lines to the bridge file. The bridge file is tailed by
// `connect`, which forwards each line over the daemon WebSocket.
//
// Replaces the four `stream*Bridge` functions plus the default
// `streamToBridge` — they were all the same skeleton (wait-for-file,
// bufio partial-line buffering, per-line transform, write) with
// per-harness transform logic that now lives in the harness's
// StreamTransformer. Start from the beginning of the file — transcripts
// are fresh per agent instance, so no backfill is needed and a
// double-spawned streamer can't duplicate entries.
func tailAndPump(transcriptPath, bridgePath string, t StreamTransformer) {
	var f *os.File
	for i := 0; i < 300; i++ { // up to 30s
		var err error
		f, err = os.Open(transcriptPath)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f == nil {
		log.Printf("Transcript file never appeared: %s", transcriptPath)
		return
	}
	defer f.Close()

	bridge, err := os.OpenFile(bridgePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open bridge file: %v", err)
		return
	}
	defer bridge.Close()

	reader := bufio.NewReader(f)
	var partial string

	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			fullLine := trimNewline(partial + line)
			partial = ""
			if fullLine != "" {
				for _, out := range t.TransformLine(fullLine) {
					if len(out) == 0 {
						continue
					}
					if _, werr := fmt.Fprintln(bridge, string(out)); werr != nil {
						log.Printf("Bridge write error: %v", werr)
						return
					}
				}
			}
		} else if line != "" {
			partial += line
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("Transcript read error: %v", err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// isSlashCommand returns true if the JSONL line represents a Claude Code
// slash command (e.g. /voice, /commit) that should not be sent to the server.
//
// Claude Code's `message.content` is sometimes a plain string and sometimes
// an array of typed blocks (`[{type:"text", text:"…"}, …]`). Both shapes can
// carry the slash-command wrapper (`<command-name>/foo</command-name>…`),
// so we normalize to a single string before sniffing.
func isSlashCommand(line string) bool {
	var entry struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Message *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &entry) != nil {
		return false
	}
	if entry.Subtype == "local_command" {
		return true
	}
	if entry.Type == "user" && entry.Message != nil {
		text := flattenContent(entry.Message.Content)
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "<command-name>") || strings.HasPrefix(trimmed, "<local-command-caveat>") {
			return true
		}
	}
	return false
}

// flattenContent normalizes Claude Code's message.content (which may be a
// string or an array of `{type, text}` blocks) into a single string suitable
// for substring sniffing.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Text != "" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// transformGeminiLine parses one JSONL line from a Gemini transcript and
// converts it to zero or more bridge-shape entries. Returns nil for the
// session header (no "id" field), $set timestamp updates, and any line that
// fails to parse — those carry no rendered content.
func transformGeminiLine(line, sessionID string) []map[string]interface{} {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return nil
	}
	if _, isSet := probe["$set"]; isSet {
		return nil
	}
	if _, hasID := probe["id"]; !hasID {
		return nil
	}
	return transformGeminiMessage(json.RawMessage(line), sessionID)
}

// geminiMessage is the structure of a message in a Gemini transcript.
type geminiMessage struct {
	ID        string            `json:"id"`
	Timestamp string            `json:"timestamp"`
	Type      string            `json:"type"`
	Content   json.RawMessage   `json:"content"`
	Model     string            `json:"model"`
	Tokens    *geminiTokens     `json:"tokens,omitempty"`
	ToolCalls []geminiToolCall  `json:"toolCalls,omitempty"`
}

type geminiToolCall struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Args          map[string]interface{} `json:"args"`
	Result        json.RawMessage        `json:"result"`
	Status        string                 `json:"status"`
	Timestamp     string                 `json:"timestamp"`
	ResultDisplay json.RawMessage        `json:"resultDisplay"`
}

type geminiTokens struct {
	Input    int `json:"input"`
	Output   int `json:"output"`
	Cached   int `json:"cached"`
	Thoughts int `json:"thoughts"`
	Tool     int `json:"tool"`
	Total    int `json:"total"`
}

// transformGeminiMessage converts a Gemini transcript message to Claude Code
// transcript format so the server/phone can render it uniformly.
// Returns a slice because a single Gemini message with toolCalls produces
// multiple entries: the assistant text + tool_use/tool_result pairs.
func transformGeminiMessage(raw json.RawMessage, sessionID string) []map[string]interface{} {
	var msg geminiMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		log.Printf("gemini message parse error: %v", err)
		return nil
	}

	switch msg.Type {
	case "user":
		// Gemini user content is [{text: "..."}], extract the text
		var contentParts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Content, &contentParts); err != nil || len(contentParts) == 0 {
			return nil
		}
		text := contentParts[0].Text
		for _, p := range contentParts[1:] {
			text += "\n" + p.Text
		}
		return []map[string]interface{}{{
			"type":      "user",
			"uuid":      msg.ID,
			"timestamp": msg.Timestamp,
			"sessionId": sessionID,
			"message": map[string]interface{}{
				"role":    "user",
				"content": text,
			},
		}}

	case "gemini":
		var entries []map[string]interface{}

		var text string
		if err := json.Unmarshal(msg.Content, &text); err != nil {
			return nil
		}

		// Emit assistant text entry
		textEntry := map[string]interface{}{
			"type":      "assistant",
			"uuid":      msg.ID,
			"timestamp": msg.Timestamp,
			"sessionId": sessionID,
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": text},
				},
				"model": msg.Model,
			},
		}
		if msg.Tokens != nil {
			textEntry["message"].(map[string]interface{})["usage"] = map[string]interface{}{
				"input_tokens":  msg.Tokens.Input,
				"output_tokens": msg.Tokens.Output,
				"cache_read_input_tokens": msg.Tokens.Cached,
			}
		}
		entries = append(entries, textEntry)

		// Emit tool_use + tool_result pairs matching Claude's format
		for _, tc := range msg.ToolCalls {
			// tool_use: type "assistant" with content [{type:"tool_use",...}]
			entries = append(entries, map[string]interface{}{
				"type":      "assistant",
				"uuid":      tc.ID,
				"timestamp": tc.Timestamp,
				"sessionId": sessionID,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{
							"type":  "tool_use",
							"id":    tc.ID,
							"name":  normalizeGeminiToolName(tc.Name),
							"input": normalizeGeminiToolArgs(tc.Name, tc.Args),
						},
					},
				},
			})

			// tool_result: type "progress" with nested data.message.message
			resultContent := geminiToolResultContent(tc)
			entries = append(entries, map[string]interface{}{
				"type":      "progress",
				"uuid":      tc.ID + "_result",
				"timestamp": tc.Timestamp,
				"sessionId": sessionID,
				"data": map[string]interface{}{
					"message": map[string]interface{}{
						"type": "user",
						"message": map[string]interface{}{
							"role": "user",
							"content": []map[string]interface{}{
								{
									"type":        "tool_result",
									"tool_use_id": tc.ID,
									"content":     resultContent,
								},
							},
						},
					},
				},
			})
		}

		return entries

	default:
		// Other types — pass through with minimal wrapping
		var content interface{}
		json.Unmarshal(raw, &content)
		return []map[string]interface{}{{
			"type":      msg.Type,
			"uuid":      msg.ID,
			"timestamp": msg.Timestamp,
			"sessionId": sessionID,
			"message":   content,
		}}
	}
}

// geminiToolNameMap translates Gemini tool names to their Claude equivalents
// so the server and client can use a single set of tool name display logic.
var geminiToolNameMap = map[string]string{
	"read_file":         "Read",
	"write_file":        "Write",
	"replace":           "Edit",
	"run_shell_command": "Bash",
	"grep_search":       "Grep",
	"list_directory":    "Bash",
	"web_fetch":         "WebFetch",
	"google_web_search": "WebSearch",
	"get_internal_docs": "Read",
}

// normalizeGeminiToolName maps Gemini tool names to Claude equivalents.
func normalizeGeminiToolName(name string) string {
	if mapped, ok := geminiToolNameMap[name]; ok {
		return mapped
	}
	return name
}

// normalizeGeminiToolArgs adjusts Gemini tool args to match Claude's format.
func normalizeGeminiToolArgs(name string, args map[string]interface{}) map[string]interface{} {
	switch name {
	case "run_shell_command":
		// Gemini uses "command" + "description"; Claude uses "command"
		return map[string]interface{}{
			"command": args["command"],
		}
	case "list_directory":
		// Map to Bash with "ls <dir>" command
		dir := ""
		if d, ok := args["dir_path"]; ok {
			dir = fmt.Sprintf("%v", d)
		}
		return map[string]interface{}{
			"command": "ls " + dir,
		}
	default:
		return args
	}
}

// geminiToolResultContent extracts a display-friendly result string from a tool call.
// For file operations with resultDisplay containing diffs, returns the diff.
// Otherwise returns the function response output or error.
func geminiToolResultContent(tc geminiToolCall) string {
	// Try resultDisplay for file diffs
	if len(tc.ResultDisplay) > 0 {
		// resultDisplay can be a string or an object with fileDiff
		var displayObj struct {
			FileDiff string `json:"fileDiff"`
		}
		if json.Unmarshal(tc.ResultDisplay, &displayObj) == nil && displayObj.FileDiff != "" {
			return displayObj.FileDiff
		}
		// Try as plain string
		var displayStr string
		if json.Unmarshal(tc.ResultDisplay, &displayStr) == nil && displayStr != "" {
			return displayStr
		}
	}

	// Fall back to function response output/error
	var results []struct {
		FunctionResponse struct {
			Response struct {
				Output string `json:"output"`
				Error  string `json:"error"`
			} `json:"response"`
		} `json:"functionResponse"`
	}
	if json.Unmarshal(tc.Result, &results) == nil && len(results) > 0 {
		resp := results[0].FunctionResponse.Response
		if resp.Error != "" {
			return resp.Error
		}
		return resp.Output
	}

	return ""
}

// transformCopilotEvent converts a copilot events.jsonl line to Claude transcript format.
// Copilot events have the structure:
//
//	{"type":"event.type","data":{...},"id":"uuid","timestamp":"ISO-8601","parentId":"uuid"}
func transformCopilotEvent(line string) string {
	var event struct {
		Type      string          `json:"type"`
		Data      json.RawMessage `json:"data"`
		ID        string          `json:"id"`
		Timestamp string          `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return ""
	}

	var entry map[string]interface{}

	switch event.Type {
	case "user.message":
		var data struct {
			Content string `json:"content"`
		}
		json.Unmarshal(event.Data, &data)
		entry = map[string]interface{}{
			"type":      "user",
			"uuid":      event.ID,
			"timestamp": event.Timestamp,
			"message": map[string]interface{}{
				"role":    "user",
				"content": data.Content,
			},
		}

	case "assistant.message":
		var data struct {
			Content string `json:"content"`
			Model   string `json:"model"`
		}
		json.Unmarshal(event.Data, &data)
		entry = map[string]interface{}{
			"type":      "assistant",
			"uuid":      event.ID,
			"timestamp": event.Timestamp,
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": data.Content},
				},
				"model": data.Model,
			},
		}

	case "tool.execution_start":
		// Normalize to Claude tool_use format
		var data struct {
			ToolCallID string                 `json:"toolCallId"`
			ToolName   string                 `json:"toolName"`
			Arguments  map[string]interface{} `json:"arguments"`
		}
		json.Unmarshal(event.Data, &data)
		entry = map[string]interface{}{
			"type":      "assistant",
			"uuid":      event.ID,
			"timestamp": event.Timestamp,
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{
						"type":  "tool_use",
						"name":  normalizeCopilotToolName(data.ToolName),
						"id":    data.ToolCallID,
						"input": normalizeCopilotToolArgs(data.ToolName, data.Arguments),
					},
				},
			},
		}

	case "tool.execution_complete":
		// Normalize to Claude tool_result format (type "progress" with nested structure)
		var data struct {
			ToolCallID string `json:"toolCallId"`
			Success    bool   `json:"success"`
			Result     struct {
				Content         string `json:"content"`
				DetailedContent string `json:"detailedContent"`
			} `json:"result"`
		}
		json.Unmarshal(event.Data, &data)
		// Prefer detailedContent (has diffs for edits) over content (short summary)
		resultContent := data.Result.Content
		if data.Result.DetailedContent != "" {
			resultContent = data.Result.DetailedContent
		}
		entry = map[string]interface{}{
			"type":      "progress",
			"uuid":      event.ID,
			"timestamp": event.Timestamp,
			"data": map[string]interface{}{
				"message": map[string]interface{}{
					"type": "user",
					"message": map[string]interface{}{
						"role": "user",
						"content": []map[string]interface{}{
							{
								"type":        "tool_result",
								"tool_use_id": data.ToolCallID,
								"content":     resultContent,
							},
						},
					},
				},
			},
		}

	default:
		return ""
	}

	out, err := json.Marshal(entry)
	if err != nil {
		return ""
	}
	return string(out)
}

// normalizeCopilotToolName maps Copilot's lowercase tool names to Claude PascalCase.
func normalizeCopilotToolName(name string) string {
	switch name {
	case "bash":
		return "Bash"
	case "edit":
		return "Edit"
	case "view":
		return "Read"
	case "create":
		return "Write"
	default:
		return name
	}
}

// normalizeCopilotToolArgs translates Copilot arg keys to Claude equivalents.
func normalizeCopilotToolArgs(toolName string, args map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(args))
	for k, v := range args {
		switch k {
		case "path":
			out["file_path"] = v
		case "file_text":
			out["content"] = v
		case "old_str":
			out["old_string"] = v
		case "new_str":
			out["new_string"] = v
		default:
			out[k] = v
		}
	}
	return out
}

// envelopeTimestamp pulls the `ts` field out of a hearth/1 envelope at the
// start of `text`. Returns "" for non-enveloped text or any parse failure —
// callers treat absence as "no per-entry timestamp."
func envelopeTimestamp(text string) string {
	if !strings.HasPrefix(text, "hearth/1 ") {
		return ""
	}
	nl := strings.Index(text, "\n")
	if nl < 0 {
		return ""
	}
	var hdr struct {
		TS string `json:"ts"`
	}
	if err := json.Unmarshal([]byte(text[len("hearth/1 "):nl]), &hdr); err != nil {
		return ""
	}
	return hdr.TS
}

// transformCodexEvent converts a Codex session JSONL line to Claude transcript format.
// Codex JSONL events use a top-level "type" field (response_item, event_msg, etc.)
// with a "payload" containing the actual data.
// extractCodexPatch parses a Codex apply_patch and returns removed/added lines.
func extractCodexPatch(patch string) (string, string) {
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

// normalizeCodexTool translates Codex tool names and arguments to Claude equivalents.
// exec_command → Bash (extract cmd), apply_patch → Edit, etc.
func normalizeCodexTool(name, arguments string) (string, interface{}) {
	switch name {
	case "exec_command":
		var args struct {
			Cmd string `json:"cmd"`
		}
		json.Unmarshal([]byte(arguments), &args)
		return "Bash", map[string]string{"command": args.Cmd}
	case "apply_patch":
		var args struct {
			Patch string `json:"patch"`
		}
		json.Unmarshal([]byte(arguments), &args)
		// Extract file path from patch content
		filePath := "unknown"
		if idx := strings.Index(args.Patch, "*** Update File: "); idx >= 0 {
			line := args.Patch[idx+17:]
			if nl := strings.IndexByte(line, '\n'); nl >= 0 {
				filePath = line[:nl]
			}
		} else if idx := strings.Index(args.Patch, "*** Add File: "); idx >= 0 {
			line := args.Patch[idx+14:]
			if nl := strings.IndexByte(line, '\n'); nl >= 0 {
				filePath = line[:nl]
			}
		}
		oldStr, newStr := extractCodexPatch(args.Patch)
		return "Edit", map[string]interface{}{
			"file_path":  filePath,
			"old_string": oldStr,
			"new_string": newStr,
		}
	default:
		// Pass through unknown tools with raw arguments
		if arguments != "" {
			var parsed interface{}
			if err := json.Unmarshal([]byte(arguments), &parsed); err == nil {
				return name, parsed
			}
		}
		return name, map[string]string{}
	}
}

func transformCodexEvent(line string) string {
	var event struct {
		Timestamp string          `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return ""
	}

	var entry map[string]interface{}

	switch event.Type {
	case "response_item":
		// Contains messages, function calls, function call outputs, reasoning
		var item struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Name      string          `json:"name"`
			CallID    string          `json:"call_id"`
			Content   json.RawMessage `json:"content"`
			Arguments string          `json:"arguments"` // function_call: JSON-encoded args
			Output    string          `json:"output"`
			Summary   json.RawMessage `json:"summary"`
		}
		if err := json.Unmarshal(event.Payload, &item); err != nil {
			return ""
		}

		switch item.Type {
		case "message":
			if item.Role == "user" {
				// User message: content is an array of {type:"input_text", text:"..."}
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				json.Unmarshal(item.Content, &parts)
				text := ""
				for _, p := range parts {
					if p.Type == "input_text" && p.Text != "" {
						// Skip hearth instruction content and system context
						if strings.Contains(p.Text, "<!-- hearth -->") ||
							strings.HasPrefix(p.Text, "<permissions") ||
							strings.HasPrefix(p.Text, "<environment_context>") ||
							strings.HasPrefix(p.Text, "<collaboration_mode>") ||
							strings.Contains(p.Text, "<hearth-warmup>") {
							continue
						}
						if text != "" {
							text += "\n"
						}
						text += p.Text
					}
				}
				if text == "" {
					return ""
				}
				entry = map[string]interface{}{
					"type":      "user",
					"timestamp": event.Timestamp,
					"message": map[string]interface{}{
						"role":    "user",
						"content": text,
					},
				}
			} else if item.Role == "assistant" {
				// Assistant message: content is an array of {type:"output_text", text:"..."}
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				json.Unmarshal(item.Content, &parts)
				text := ""
				for _, p := range parts {
					if p.Type == "output_text" && p.Text != "" {
						if text != "" {
							text += "\n"
						}
						text += p.Text
					}
				}
				if text == "" {
					return ""
				}
				entry = map[string]interface{}{
					"type":      "assistant",
					"timestamp": event.Timestamp,
					"message": map[string]interface{}{
						"role": "assistant",
						"content": []map[string]interface{}{
							{"type": "text", "text": text},
						},
					},
				}
			} else {
				// Developer messages are system instructions — skip
				return ""
			}

		case "function_call":
			// Tool call: name + arguments (arguments is a JSON string)
			// Normalize Codex tool names to Claude equivalents
			toolName, toolInput := normalizeCodexTool(item.Name, item.Arguments)
			entry = map[string]interface{}{
				"type":      "assistant",
				"timestamp": event.Timestamp,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{
							"type":  "tool_use",
							"name":  toolName,
							"id":    item.CallID,
							"input": toolInput,
						},
					},
				},
			}

		case "function_call_output":
			// Tool result (matches Claude's "progress" format)
			entry = map[string]interface{}{
				"type":      "progress",
				"timestamp": event.Timestamp,
				"data": map[string]interface{}{
					"message": map[string]interface{}{
						"type": "user",
						"message": map[string]interface{}{
							"role": "user",
							"content": []map[string]interface{}{
								{
									"type":        "tool_result",
									"tool_use_id": item.CallID,
									"content":     item.Output,
								},
							},
						},
					},
				},
			}

		case "web_search_call":
			// Web search: has action.query
			var search struct {
				Action struct {
					Query string `json:"query"`
				} `json:"action"`
			}
			json.Unmarshal(event.Payload, &search)
			query := search.Action.Query
			if query == "" {
				return ""
			}
			entry = map[string]interface{}{
				"type":      "assistant",
				"timestamp": event.Timestamp,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{
							"type":  "tool_use",
							"name":  "WebSearch",
							"input": map[string]string{"query": query},
						},
					},
				},
			}

		case "reasoning":
			// Model reasoning summary
			var summaryParts []struct {
				Text string `json:"text"`
			}
			json.Unmarshal(item.Summary, &summaryParts)
			text := ""
			for _, p := range summaryParts {
				if text != "" {
					text += "\n"
				}
				text += p.Text
			}
			if text == "" {
				return ""
			}
			entry = map[string]interface{}{
				"type":      "assistant",
				"timestamp": event.Timestamp,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{"type": "thinking", "thinking": text},
					},
				},
			}

		default:
			return ""
		}

	case "event_msg":
		// Agent events: user_message, agent_reasoning, token_count, task_started
		var msg struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Text    string `json:"text"`
		}
		json.Unmarshal(event.Payload, &msg)

		switch msg.Type {
		case "user_message":
			// Codex 0.128 records every user message twice — once as a
			// response_item with role=user (handled above with full
			// content-block structure) and once as an event_msg of type
			// user_message with the body as a flat string. Emitting both
			// shows the user's input duplicated in the iOS transcript
			// view. response_item is the more complete record, so skip
			// the event_msg copy here.
			return ""
		case "agent_reasoning":
			entry = map[string]interface{}{
				"type":      "assistant",
				"timestamp": event.Timestamp,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{"type": "thinking", "thinking": msg.Text},
					},
				},
			}
		case "agent_message":
			// Mirror of the user_message skip above: codex 0.128 records
			// every assistant turn twice — once as a response_item with
			// role=assistant (handled in the response_item branch) and
			// once as an event_msg of type agent_message. Skip the
			// event_msg copy so the iOS transcript doesn't duplicate.
			return ""
		default:
			return ""
		}

	default:
		return ""
	}

	if entry == nil {
		return ""
	}
	out, err := json.Marshal(entry)
	if err != nil {
		return ""
	}
	return string(out)
}

// transformPiEvent converts a pi-coding-agent session JSONL line to Claude
// transcript format. Pi entries have:
//
//	{"type":"session",...} — session header (skip)
//	{"type":"message","id":"...","parentId":"...","timestamp":"...","message":{...}}
//
// Messages have role: "user", "assistant", or "toolResult".
// Assistant content can be a string or an array with text/toolCall blocks.
func transformPiEvent(line string) string {
	var event struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Timestamp string          `json:"timestamp"`
		Message   json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return ""
	}

	if event.Type != "message" {
		return "" // skip session headers, model changes, etc.
	}

	var msg struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Model      string          `json:"model"`
		ToolCallID string          `json:"toolCallId"`
		ToolName   string          `json:"toolName"`
		IsError    bool            `json:"isError"`
	}
	if err := json.Unmarshal(event.Message, &msg); err != nil {
		return ""
	}

	var entry map[string]interface{}

	switch msg.Role {
	case "user":
		// Content can be a string or an array of {type:"text", text:"..."}
		var text string
		if err := json.Unmarshal(msg.Content, &text); err != nil {
			// Try array format
			var blocks []struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(msg.Content, &blocks); err != nil {
				return ""
			}
			var parts []string
			for _, b := range blocks {
				if b.Text != "" {
					parts = append(parts, b.Text)
				}
			}
			text = strings.Join(parts, "\n")
		}
		entry = map[string]interface{}{
			"type":      "user",
			"uuid":      event.ID,
			"timestamp": event.Timestamp,
			"message": map[string]interface{}{
				"role":    "user",
				"content": text,
			},
		}

	case "assistant":
		// Content can be a string or an array of text/toolCall blocks
		var contentStr string
		if json.Unmarshal(msg.Content, &contentStr) == nil {
			// Simple string content
			entry = map[string]interface{}{
				"type":      "assistant",
				"uuid":      event.ID,
				"timestamp": event.Timestamp,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{"type": "text", "text": contentStr},
					},
					"model": msg.Model,
				},
			}
		} else {
			// Array content with text blocks and toolCall blocks
			var parts []json.RawMessage
			if err := json.Unmarshal(msg.Content, &parts); err != nil {
				return ""
			}
			entries := transformPiAssistantContent(parts, event.ID, event.Timestamp, msg.Model)
			if len(entries) == 0 {
				return ""
			}
			// Return multiple entries as separate lines
			var lines []string
			for _, e := range entries {
				out, err := json.Marshal(e)
				if err != nil {
					continue
				}
				lines = append(lines, string(out))
			}
			return strings.Join(lines, "\n")
		}

	case "toolResult":
		// Tool result: content is an array [{type:"text",text:"..."}]
		resultContent := ""
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(msg.Content, &parts) == nil {
			for _, p := range parts {
				if p.Text != "" {
					if resultContent != "" {
						resultContent += "\n"
					}
					resultContent += p.Text
				}
			}
		} else {
			// Try as plain string
			json.Unmarshal(msg.Content, &resultContent)
		}
		entry = map[string]interface{}{
			"type":      "progress",
			"uuid":      event.ID,
			"timestamp": event.Timestamp,
			"data": map[string]interface{}{
				"message": map[string]interface{}{
					"type": "user",
					"message": map[string]interface{}{
						"role": "user",
						"content": []map[string]interface{}{
							{
								"type":        "tool_result",
								"tool_use_id": msg.ToolCallID,
								"content":     resultContent,
							},
						},
					},
				},
			},
		}

	default:
		return ""
	}

	out, err := json.Marshal(entry)
	if err != nil {
		return ""
	}
	return string(out)
}

// transformPiAssistantContent processes a pi assistant message with mixed
// text and toolCall content blocks, returning Claude-format entries.
func transformPiAssistantContent(parts []json.RawMessage, id, timestamp, model string) []map[string]interface{} {
	var entries []map[string]interface{}
	var textParts []map[string]interface{}

	for _, raw := range parts {
		var block struct {
			Type      string                 `json:"type"`
			Text      string                 `json:"text"`
			ID        string                 `json:"id"`
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			continue
		}

		switch block.Type {
		case "text":
			if block.Text != "" {
				textParts = append(textParts, map[string]interface{}{
					"type": "text",
					"text": block.Text,
				})
			}
		case "toolCall":
			// Flush accumulated text before the tool call
			if len(textParts) > 0 {
				entries = append(entries, map[string]interface{}{
					"type":      "assistant",
					"uuid":      id,
					"timestamp": timestamp,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": textParts,
						"model":   model,
					},
				})
				textParts = nil
			}
			// Emit tool_use entry
			entries = append(entries, map[string]interface{}{
				"type":      "assistant",
				"uuid":      block.ID,
				"timestamp": timestamp,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{
							"type":  "tool_use",
							"id":    block.ID,
							"name":  normalizePiToolName(block.Name),
							"input": normalizePiToolArgs(block.Name, block.Arguments),
						},
					},
				},
			})
		}
	}

	// Flush remaining text
	if len(textParts) > 0 {
		entries = append(entries, map[string]interface{}{
			"type":      "assistant",
			"uuid":      id,
			"timestamp": timestamp,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": textParts,
				"model":   model,
			},
		})
	}

	return entries
}

// normalizePiToolName maps pi tool names to Claude equivalents.
func normalizePiToolName(name string) string {
	switch name {
	case "bash":
		return "Bash"
	case "edit":
		return "Edit"
	case "read":
		return "Read"
	case "write":
		return "Write"
	default:
		return name
	}
}

// normalizePiToolArgs translates pi tool argument keys to Claude equivalents.
func normalizePiToolArgs(toolName string, args map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(args))
	for k, v := range args {
		switch k {
		case "path":
			out["file_path"] = v
		case "old_str":
			out["old_string"] = v
		case "new_str":
			out["new_string"] = v
		default:
			out[k] = v
		}
	}
	return out
}

// streamTranscript tails a JSONL transcript file and POSTs each line to the server.
// seekToLastLines positions the reader near the last N lines of the file.
func seekToLastLines(f *os.File, n int) {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return
	}

	// Read from the end, looking for newlines
	buf := make([]byte, 1)
	count := 0
	pos := info.Size() - 1

	for pos > 0 {
		f.Seek(pos, io.SeekStart)
		f.Read(buf)
		if buf[0] == '\n' {
			count++
			if count > n {
				f.Seek(pos+1, io.SeekStart)
				return
			}
		}
		pos--
	}

	// File has fewer than n lines — read from start
	f.Seek(0, io.SeekStart)
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
