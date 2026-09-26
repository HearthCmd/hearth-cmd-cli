//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// runChat dispatches `hearth chat <subcommand>`.
func runChat(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, chatReplyUsage)
		os.Exit(1)
	}
	switch args[0] {
	case "reply":
		runChatReply(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "hearth chat: unknown subcommand %q\n", args[0])
		os.Exit(1)
	}
}

const chatReplyUsage = `usage: hearth chat reply --room <room_id> [--thread <message_id> | --channel] ["<message>"]
  With no message argument, the message is read from stdin. Prefer a quoted heredoc,
  which passes any text through untouched (no quoting, no $ or backtick expansion):
    hearth chat reply --room <room_id> <<'HEARTH_MSG'
    your message
    HEARTH_MSG`

// daemonCapabilities is what this daemon advertises to the relay on dial (the
// `caps` query param). chat_reply_stdin: `hearth chat reply` reads the message
// from stdin, so the relay may teach agents here the heredoc form
// (docs/agent-chat-message-transport.md).
const daemonCapabilities = "chat_reply_stdin"

// chatReplyMaxBytes caps a message read from stdin. Far above any chat
// message; it only stops a runaway pipe.
const chatReplyMaxBytes = 64 * 1024

// runChatReply sends a message to an org chat room on behalf of the calling
// agent. Reads HEARTH_AGENT_INSTANCE_ID from env; the room_id is required.
//
// Rooms have Slack-style threads. --thread <id> replies in the thread that
// message belongs to; --channel posts in the main room. With neither, the
// server puts the reply where the agent was asked: in the thread if the agent
// was woken from one (recently), otherwise the main room.
//
// The message is the remaining arguments, or — with none — stdin. Stdin is the
// robust form: an inline argument goes through shell quoting and expansion, so
// long or rich messages get mangled (docs/agent-chat-message-transport.md).
//
// Usage: hearth chat reply --room <room_id> [--thread <message_id> | --channel] ["<message>"]
func runChatReply(args []string) {
	fs := flag.NewFlagSet("chat reply", flag.ExitOnError)
	room := fs.String("room", os.Getenv("HEARTH_CHAT_ROOM_ID"), "Chat room ID (or set HEARTH_CHAT_ROOM_ID)")
	thread := fs.String("thread", "", "Reply in this thread (the id of any message in it)")
	channel := fs.Bool("channel", false, "Post in the main room, not a thread")
	_ = fs.Parse(args)

	if *thread != "" && *channel {
		fmt.Fprintln(os.Stderr, "hearth chat reply: use --thread or --channel, not both")
		os.Exit(1)
	}

	agentInstanceID := os.Getenv("HEARTH_AGENT_INSTANCE_ID")
	if agentInstanceID == "" {
		fmt.Fprintln(os.Stderr, "hearth chat reply: HEARTH_AGENT_INSTANCE_ID not set")
		os.Exit(1)
	}
	if *room == "" {
		fmt.Fprintln(os.Stderr, "hearth chat reply: --room <room_id> is required")
		os.Exit(1)
	}

	text, err := readChatReplyText(fs.Args(), os.Stdin, stdinIsTerminal())
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth chat reply: %v\n", err)
		os.Exit(1)
	}

	req := ipcRequest{
		Type:                "chat_reply",
		ChatRoomID:          *room,
		ChatAgentInstanceID: agentInstanceID,
		ChatText:            text,
		ChatThreadRootID:    *thread,
		ChatChannel:         *channel,
	}
	resp, err := sendChatReplyIPC(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth chat reply: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth chat reply: server error: %s\n", resp.Message)
		os.Exit(1)
	}
	// Silent on success — agent gets a clean exit code 0.
}

// readChatReplyText returns the message: the arguments joined, or — when there
// are none — stdin, read up to chatReplyMaxBytes with the heredoc's trailing
// newline trimmed. A terminal on stdin is never read (it would just hang
// waiting for a person), and an empty result is an error.
func readChatReplyText(args []string, stdin io.Reader, stdinIsTTY bool) (string, error) {
	// "-" is the usual "read from stdin" spelling; don't post it literally.
	if len(args) > 0 && !(len(args) == 1 && args[0] == "-") {
		if text := strings.Join(args, " "); text != "" {
			return text, nil
		}
	}
	errRequired := errors.New("message text is required (as an argument, or on stdin)")
	if stdin == nil || stdinIsTTY {
		return "", errRequired
	}
	b, err := io.ReadAll(io.LimitReader(stdin, chatReplyMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading message from stdin: %v", err)
	}
	if len(b) > chatReplyMaxBytes {
		return "", fmt.Errorf("message on stdin is over %d KiB", chatReplyMaxBytes/1024)
	}
	text := strings.TrimRight(string(b), "\r\n")
	if strings.TrimSpace(text) == "" {
		return "", errRequired
	}
	return text, nil
}

// stdinIsTerminal reports whether stdin is a character device: an interactive
// terminal (reading would hang waiting for a person) or /dev/null, which is what
// agent runtimes attach when no message was piped in. Either way there's no
// message on it. A heredoc or pipe is not a character device.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func sendChatReplyIPC(req ipcRequest) (*ipcResponse, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 5*time.Second)
	if err != nil {
		return nil, daemonDialError(err)
	}
	defer conn.Close()

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %v", err)
	}
	reqBytes = append(reqBytes, '\n')
	if _, err := conn.Write(reqBytes); err != nil {
		return nil, fmt.Errorf("send: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read: %v", err)
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode: %v", err)
	}
	return &resp, nil
}
