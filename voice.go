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

// runVoice dispatches `hearth voice <subcommand>`.
func runVoice(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hearth voice handoff --to \"<agent name>\"")
		os.Exit(1)
	}
	switch args[0] {
	case "handoff":
		runVoiceHandoff(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "hearth voice: unknown subcommand %q\n", args[0])
		os.Exit(1)
	}
}

// runVoiceHandoff transfers the voice conversation the calling agent is handling
// to another household agent (voice track V4). The calling agent is identified by
// HEARTH_AGENT_INSTANCE_ID (as `hearth chat reply` is); the relay resolves which
// conversation to hand off from that instance's most-recent voice session.
//
// Usage: hearth voice handoff --to "<agent name or id>"
func runVoiceHandoff(args []string) {
	fs := flag.NewFlagSet("voice handoff", flag.ExitOnError)
	to := fs.String("to", "", "Target agent name or id")
	_ = fs.Parse(args)

	agentInstanceID := os.Getenv("HEARTH_AGENT_INSTANCE_ID")
	if agentInstanceID == "" {
		fmt.Fprintln(os.Stderr, "hearth voice handoff: HEARTH_AGENT_INSTANCE_ID not set")
		os.Exit(1)
	}
	// Accept the target either via --to or as trailing args (`handoff --to X`, or
	// `handoff Alice`), so the agent can invoke it naturally.
	target := strings.TrimSpace(*to)
	if target == "" {
		target = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "hearth voice handoff: --to \"<agent name>\" is required")
		os.Exit(1)
	}

	req := ipcRequest{
		Type:                 "voice_handoff",
		VoiceAgentInstanceID: agentInstanceID,
		VoiceHandoffTo:       target,
	}
	resp, err := sendVoiceHandoffIPC(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth voice handoff: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth voice handoff: server error: %s\n", resp.Message)
		os.Exit(1)
	}
	// Silent on success — the agent gets a clean exit code 0.
}

func sendVoiceHandoffIPC(req ipcRequest) (*ipcResponse, error) {
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
