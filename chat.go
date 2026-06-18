//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// runChat dispatches `hearth chat <subcommand>`.
func runChat(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hearth chat reply --room <room_id> \"<message>\"")
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

// runChatReply sends a message to an org chat room on behalf of the calling
// agent. Reads HEARTH_AGENT_INSTANCE_ID from env; the room_id is required.
//
// Usage: hearth chat reply --room <room_id> "<message>"
func runChatReply(args []string) {
	fs := flag.NewFlagSet("chat reply", flag.ExitOnError)
	room := fs.String("room", os.Getenv("HEARTH_CHAT_ROOM_ID"), "Chat room ID (or set HEARTH_CHAT_ROOM_ID)")
	_ = fs.Parse(args)

	agentInstanceID := os.Getenv("HEARTH_AGENT_INSTANCE_ID")
	if agentInstanceID == "" {
		fmt.Fprintln(os.Stderr, "hearth chat reply: HEARTH_AGENT_INSTANCE_ID not set")
		os.Exit(1)
	}
	if *room == "" {
		fmt.Fprintln(os.Stderr, "hearth chat reply: --room <room_id> is required")
		os.Exit(1)
	}

	text := strings.Join(fs.Args(), " ")
	if text == "" {
		fmt.Fprintln(os.Stderr, "hearth chat reply: message text is required")
		os.Exit(1)
	}

	req := ipcRequest{
		Type:                "chat_reply",
		ChatRoomID:          *room,
		ChatAgentInstanceID: agentInstanceID,
		ChatText:            text,
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

func sendChatReplyIPC(req ipcRequest) (*ipcResponse, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to daemon: %v\nRun 'hearth start' first", err)
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
