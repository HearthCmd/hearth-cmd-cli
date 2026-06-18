//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// WSConn is the interface used by agent instances for WebSocket communication.
// Both *WSClient (direct) and *agentWS (multiplexed via daemon) implement it.
type WSConn interface {
	SendText(data []byte)
	Send(data []byte)
	RegisterPending(requestID string) <-chan []byte
	RemovePending(requestID string)
	Close()
}

// DaemonWS is a multiplexed WebSocket owned by the daemon. All agent instances
// share this single connection. Outgoing text frames are tagged with
// ai_agent_instance_id so the server can route them. Incoming messages are
// dispatched to the correct agent instance by ai_agent_instance_id.
type DaemonWS struct {
	ws *WSClient

	mu        sync.RWMutex
	instances map[string]*agentWS // ai_agent_instance_id → agent instance handle

	// Callbacks wired up by the owning Daemon for server-initiated
	// intent-change commands. Both are idempotent.
	sleepFunc  func(aiAgentInstanceID string)
	wakeFunc   func(aiAgentInstanceID string, spawnContext json.RawMessage)
	cycleFunc  func(aiAgentInstanceID string, spawnContext json.RawMessage)

	// Identity-cache callbacks. Server pushes "account",
	// "organizations_list", and "agent_home_path" on connect (and on
	// changes); we hand the parsed payloads to the Daemon so
	// `hearth status` can read them out of process memory.
	accountFunc       func(humanUserID, email string)
	organizationsFunc func(orgs []daemonOrgEntry)
	agentHomePathFunc  func(dir string)

	// afterReconnectFunc, when set, runs after the post-reconnect
	// agent re-registration. Lets the owning Daemon hook additional
	// reconnect-time work (2a re-reports plugin installs here) without
	// the WS layer needing to know about plugin / secret / rule state.
	afterReconnectFunc func()

	// resourceConnectionsChangedFunc fires when the server sends a
	// resource_connections_changed nudge (2b live-push). The daemon
	// wires it to refetch the connection list. Body of the frame is
	// informational (change_kind, connection_id) — the daemon
	// refetches the full list regardless, so this callback takes no
	// args.
	resourceConnectionsChangedFunc func()

	// agentResourceGrantsChangedFunc fires when the server sends an
	// agent_resource_grants_changed nudge (phase 4 live-push). The
	// daemon wires it to refetch the (agent → connection) grant view.
	// Like resourceConnectionsChangedFunc, the frame body is
	// informational; the daemon refetches the full list.
	agentResourceGrantsChangedFunc func()
}

// agentWS is a per-agent-instance handle to the shared daemon WebSocket.
type agentWS struct {
	daemon            *DaemonWS
	aiAgentInstanceID string
	project           string
	agent             string
	cwd               string
	version           string
	// agentSessionID is the harness-internal session id this spawn is
	// associated with (UUID for SessionIDMint harnesses; the codex
	// UUIDv7 for SessionIDHarnessAssigned once discovered). Empty
	// until known. Used by replayTranscriptHistory so the history
	// lookup goes through the deterministic by-id path instead of
	// the "newest on disk" fallback — without this, history replay
	// could surface a different agent's transcript when multiple
	// hearth or non-hearth sessions share a cwd or session-state dir.
	agentSessionID string
	injectFunc        func([]byte) error
	killFunc          func()
	// kickSubmitFunc, when set, is called immediately after writing the
	// submit byte during text injection. Used for harnesses (gemini-cli)
	// whose TextInput buffers pasted content past the submit byte and
	// only flushes when an external event (e.g. SIGWINCH from a winsize
	// change) re-enters their main loop.
	kickSubmitFunc func()
}

// NewDaemonWS creates a multiplexed WebSocket connection.
// bearer is the host_secret minted at enroll; the server validates it
// against hosts.secret_hash keyed by the host_id query param.
func NewDaemonWS(url, bearer string) *DaemonWS {
	d := &DaemonWS{
		instances: make(map[string]*agentWS),
	}

	// The inject callback receives text frames from the server (input injection).
	// We override the normal inject path since there's no single PTY to write to.
	d.ws = NewWSClient(url, bearer, WSModeRW, nil)

	d.ws.controlFunc = func(data []byte) {
		d.routeControlFrame(data)
	}

	// Catch any text frame not matched by routePermissionResponse.
	// The server tags phone input with ai_agent_instance_id so we can route
	// to the correct agent instance's PTY.
	d.ws.textFrameFunc = func(data []byte) bool {
		return d.handleTextFrame(data)
	}

	// On reconnect, re-register all active agent instances with the server.
	// Daemon-level reconnect work (e.g. 2a's plugin-install re-report)
	// hooks via afterReconnectFunc so it stays decoupled from the
	// agent-registration code path.
	d.ws.reconnectFunc = func() {
		d.reregisterAgentInstances()
		if d.afterReconnectFunc != nil {
			d.afterReconnectFunc()
		}
	}

	return d
}

// Run starts the WebSocket connection. Blocks until closed.
func (d *DaemonWS) Run() {
	d.ws.Run()
}

// Close shuts down the WebSocket.
func (d *DaemonWS) Close() {
	d.ws.Close()
}

// UpdateAuth swaps the dial URL + bearer on the underlying ws client
// and force-closes the current connection. Registered agent instances
// stay in d.instances; the reconnectFunc re-runs reregisterAgentInstances
// against the new auth, so transcripts/permission paths stay live across
// a `hearth login`-triggered credential reload.
func (d *DaemonWS) UpdateAuth(url, bearer string) {
	d.ws.UpdateAuth(url, bearer)
}

// IsConnected returns whether the WebSocket is connected.
func (d *DaemonWS) IsConnected() bool {
	return d.ws.IsConnected()
}

// RegisterAgentInstance creates a per-instance handle for the given ID.
func (d *DaemonWS) RegisterAgentInstance(id string, injectFunc func([]byte) error, killFunc func()) *agentWS {
	aw := &agentWS{
		daemon:            d,
		aiAgentInstanceID: id,
		injectFunc:        injectFunc,
		killFunc:          killFunc,
	}
	d.mu.Lock()
	d.instances[id] = aw
	d.mu.Unlock()
	log.Printf("daemon-ws: registered agent instance %s", id)
	return aw
}

// SetAgentSessionID backfills the harness-internal session id onto a
// registered agent instance. Used by the transcript streamer after
// discovering codex's harness-assigned UUID (SessionIDHarnessAssigned
// harnesses don't know their id at register time). No-op if the
// instance isn't registered or already has a value — first writer
// wins, which matches the streamer's "found once" semantics.
func (d *DaemonWS) SetAgentSessionID(id, sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if aw, ok := d.instances[id]; ok && aw.agentSessionID == "" {
		aw.agentSessionID = sessionID
	}
}

// UnregisterAgentInstance removes an agent instance handle.
func (d *DaemonWS) UnregisterAgentInstance(id string) {
	d.mu.Lock()
	delete(d.instances, id)
	d.mu.Unlock()
	log.Printf("daemon-ws: unregistered agent instance %s", id)
}

// ConnectAgentInstance sends an agent_instance_connect message over the daemon
// WS and waits for the server to acknowledge it. This replaces HTTP enrollment
// for agent instances within an already-enrolled daemon.
func (d *DaemonWS) ConnectAgentInstance(id, project, agent, cwd, version string) error {
	// Store metadata on the instance handle so we can re-register on reconnect
	d.mu.RLock()
	aw := d.instances[id]
	d.mu.RUnlock()
	if aw != nil {
		aw.project = project
		aw.agent = agent
		aw.cwd = cwd
		aw.version = version
	}

	return d.sendAgentInstanceConnect(id, agent, version)
}

// sendAgentInstanceConnect sends an agent_instance_connect message and waits
// for ack. Server resolves project/cwd from the DB row — we just send the agent
// harness name and client version.
func (d *DaemonWS) sendAgentInstanceConnect(id, agent, version string) error {
	data := map[string]string{
		"agent":   agent,
		"version": version,
	}
	dataBytes, _ := json.Marshal(data)

	msg := map[string]interface{}{
		"type":                 "agent_instance_connect",
		"ai_agent_instance_id": id,
		"data":                 json.RawMessage(dataBytes),
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	// Register a pending response keyed by instance ID so we can wait for the ack
	ch := d.ws.RegisterPending("agent_instance_connect:" + id)
	defer d.ws.RemovePending("agent_instance_connect:" + id)

	d.ws.SendText(msgBytes)

	select {
	case resp := <-ch:
		var ack struct {
			Type  string `json:"type"`
			Error string `json:"error,omitempty"`
		}
		if json.Unmarshal(resp, &ack) == nil && ack.Error != "" {
			return fmt.Errorf("agent_instance_connect failed: %s", ack.Error)
		}
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("agent_instance_connect timed out")
	}
}

// reregisterAgentInstances re-sends agent_instance_connect for all active
// instances after a reconnect.
func (d *DaemonWS) reregisterAgentInstances() {
	d.mu.RLock()
	instances := make([]*agentWS, 0, len(d.instances))
	for _, aw := range d.instances {
		instances = append(instances, aw)
	}
	d.mu.RUnlock()

	if len(instances) == 0 {
		return
	}

	log.Printf("daemon-ws: reconnected, re-registering %d agent instance(s)", len(instances))
	for _, aw := range instances {
		if err := d.sendAgentInstanceConnect(aw.aiAgentInstanceID, aw.agent, aw.version); err != nil {
			log.Printf("daemon-ws: failed to re-register agent instance %s: %v", aw.aiAgentInstanceID, err)
		} else {
			log.Printf("daemon-ws: re-registered agent instance %s", aw.aiAgentInstanceID)
		}
	}
}

// DisconnectAgentInstance sends an agent_instance_disconnect message over the
// daemon WS.
func (d *DaemonWS) DisconnectAgentInstance(id string) {
	msg := map[string]string{
		"type":                 "agent_instance_disconnect",
		"ai_agent_instance_id": id,
	}
	msgBytes, _ := json.Marshal(msg)
	d.ws.SendText(msgBytes)
}

// routeControlFrame handles binary control frames from the server.
func (d *DaemonWS) routeControlFrame(data []byte) {
	var msg struct {
		Type              string          `json:"type"`
		AIAgentInstanceID string          `json:"ai_agent_instance_id"`
		SpawnContext      json.RawMessage `json:"spawn_context"`
		Limit             int             `json:"limit"`
		WorkingDir        string          `json:"working_dir"`
		// relay_file fields
		DestPath string `json:"dest_path"`
		Filename string `json:"filename"`
		DataB64  string `json:"data_b64"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return
	}

	switch msg.Type {
	case "kill":
		d.mu.RLock()
		aw := d.instances[msg.AIAgentInstanceID]
		d.mu.RUnlock()
		if aw != nil && aw.killFunc != nil {
			log.Printf("daemon-ws: kill agent instance %s", msg.AIAgentInstanceID)
			aw.killFunc()
		}
	case "retire_agent_instance":
		// Drop any local state for this instance — the server has retired the row.
		d.mu.Lock()
		delete(d.instances, msg.AIAgentInstanceID)
		d.mu.Unlock()
		log.Printf("daemon-ws: retired agent instance %s", msg.AIAgentInstanceID)
	case "destroy_agent_instance":
		// Temp-only counterpart to retire: kill, drop, and rm -rf the
		// working directory. Server passes the path explicitly so we don't
		// have to depend on aw being in-memory (daemon may have restarted
		// since spawn) and so the daemon never decides on its own which
		// path to wipe — server-authoritative.
		d.mu.Lock()
		aw := d.instances[msg.AIAgentInstanceID]
		delete(d.instances, msg.AIAgentInstanceID)
		d.mu.Unlock()
		if aw != nil && aw.killFunc != nil {
			aw.killFunc()
		}
		if path := msg.WorkingDir; path != "" && filepath.IsAbs(path) {
			if err := os.RemoveAll(path); err != nil {
				log.Printf("daemon-ws: destroy_agent_instance %s: RemoveAll(%s) failed: %v", msg.AIAgentInstanceID, path, err)
			} else {
				log.Printf("daemon-ws: destroyed agent instance %s, removed %s", msg.AIAgentInstanceID, path)
			}
		} else {
			log.Printf("daemon-ws: destroy_agent_instance %s: missing/invalid working_dir %q; skipping rm", msg.AIAgentInstanceID, path)
		}
	case "sleep":
		if d.sleepFunc != nil {
			log.Printf("daemon-ws: sleep agent instance %s", msg.AIAgentInstanceID)
			d.sleepFunc(msg.AIAgentInstanceID)
		}
	case "wake":
		if d.wakeFunc != nil {
			log.Printf("daemon-ws: wake agent instance %s", msg.AIAgentInstanceID)
			// Spawn off the read loop — the wake handler blocks on
			// agent_instance_connect's ack, and that ack travels back
			// through this same loop. Running inline would deadlock.
			go d.wakeFunc(msg.AIAgentInstanceID, msg.SpawnContext)
		}
	case "cycle":
		if d.cycleFunc != nil {
			log.Printf("daemon-ws: cycle agent instance %s", msg.AIAgentInstanceID)
			go d.cycleFunc(msg.AIAgentInstanceID, msg.SpawnContext)
		}
	case "relay_file":
		// Server-relayed file from the user's phone. Decode and write to the
		// agent's working directory; path is server-authoritative.
		if !filepath.IsAbs(msg.DestPath) {
			log.Printf("daemon-ws: relay_file: non-absolute dest path %q; ignoring", msg.DestPath)
			break
		}
		data, err := base64.StdEncoding.DecodeString(msg.DataB64)
		if err != nil {
			log.Printf("daemon-ws: relay_file: base64 decode error for %s: %v", msg.Filename, err)
			break
		}
		if err := os.MkdirAll(filepath.Dir(msg.DestPath), 0755); err != nil {
			log.Printf("daemon-ws: relay_file: mkdir error for %s: %v", msg.DestPath, err)
			break
		}
		if err := os.WriteFile(msg.DestPath, data, 0644); err != nil {
			log.Printf("daemon-ws: relay_file: write error for %s: %v", msg.DestPath, err)
		} else {
			log.Printf("daemon-ws: relay_file: wrote %d bytes to %s", len(data), msg.DestPath)
		}
	case "transcript_history_request":
		// Replay the agent's on-disk JSONL through the same `transcript`
		// frames the live tail uses, so the server's existing transcript
		// processing path can fan the entries out to the requesting
		// device. Read off the read loop — file IO can take a moment on
		// long transcripts.
		go d.replayTranscriptHistory(msg.AIAgentInstanceID, msg.Limit)
	default:
		log.Printf("daemon-ws: unknown control message: %s", msg.Type)
	}
}

// handleTextFrame handles text frames not matched by routePermissionResponse.
// It tries to parse JSON with an ai_agent_instance_id and route the content as
// PTY input to the matching instance. Returns true if the frame was consumed.
func (d *DaemonWS) handleTextFrame(data []byte) bool {
	if len(data) == 0 || data[0] != '{' {
		return false
	}

	var msg struct {
		Type              string `json:"type"`
		AIAgentInstanceID string `json:"ai_agent_instance_id"`
		Text              string `json:"text"`
		Data              string `json:"data"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return false
	}

	// Identity pushes carry no ai_agent_instance_id. Route them before
	// the per-instance check below so the cache populates on connect.
	switch msg.Type {
	case "account":
		var acc struct {
			HumanUserID string `json:"human_user_id"`
			Email       string `json:"email"`
		}
		if json.Unmarshal(data, &acc) == nil && d.accountFunc != nil {
			d.accountFunc(acc.HumanUserID, acc.Email)
		}
		return true
	case "organizations_list":
		var orgs struct {
			Organizations []daemonOrgEntry `json:"organizations"`
		}
		if json.Unmarshal(data, &orgs) == nil && d.organizationsFunc != nil {
			d.organizationsFunc(orgs.Organizations)
		}
		return true
	case "agent_home_path":
		var ahd struct {
			AgentHomePath string `json:"agent_home_path"`
		}
		if json.Unmarshal(data, &ahd) == nil && d.agentHomePathFunc != nil {
			d.agentHomePathFunc(ahd.AgentHomePath)
		}
		return true
	case "resource_connections_changed":
		// 2b live-push: server tells us a connection was created or
		// deleted in our org. Refetch the full list — frame body is
		// informational only.
		if d.resourceConnectionsChangedFunc != nil {
			d.resourceConnectionsChangedFunc()
		}
		return true
	case "agent_resource_grants_changed":
		// Phase-4 live-push: server tells us an agent's grant set
		// changed (create or delete on this host's agent). Refetch
		// the full (agent → connection) view — frame body is
		// informational only.
		if d.agentResourceGrantsChangedFunc != nil {
			d.agentResourceGrantsChangedFunc()
		}
		return true
	case "relay_file":
		// File relayed from the user's phone. ai_agent_instance_id may be
		// empty (chat room path) — routeControlFrame doesn't need it for
		// this type; it only reads dest_path, filename, data_b64.
		d.routeControlFrame(data)
		return true
	}

	if msg.AIAgentInstanceID == "" {
		return false
	}

	// Intent-change control frames (sleep/wake/retire/destroy) may arrive
	// as plain text frames with a tagged type field. Route them before
	// falling through to the PTY inject path — wake in particular targets
	// instances that aren't yet in d.instances, and destroy must reach
	// routeControlFrame so the on-disk rm-rf fires. (Omitting destroy from
	// this list silently swallowed Fire-from-mobile frames on temp agents:
	// the server-side cascade landed but the workdir survived because the
	// frame fell through to the PTY-inject path and was dropped at the
	// "no text/data field" check.)
	switch msg.Type {
	case "sleep", "wake", "cycle", "transcript_history_request", "retire_agent_instance", "destroy_agent_instance":
		d.routeControlFrame(data)
		return true
	case "agent_approval_request":
		// Approver-resolution phase 5b: server dispatched a permission
		// request whose approver set names this agent. Build a
		// structured prompt and inject it as a pseudo-turn so the
		// agent reads it as fresh input and can respond via
		// `hearth hh approve`.
		return d.routeAgentApprovalRequest(data, msg.AIAgentInstanceID)
	case "chat_mention":
		return d.routeChatMention(data, msg.AIAgentInstanceID)
	}

	d.mu.RLock()
	aw := d.instances[msg.AIAgentInstanceID]
	d.mu.RUnlock()
	if aw == nil || aw.injectFunc == nil {
		log.Printf("daemon-ws: text frame for unknown agent instance %s", msg.AIAgentInstanceID)
		return false
	}

	// Extract text content — server may use "text" or "data" field.
	content := msg.Text
	if content == "" {
		content = msg.Data
	}
	if content == "" {
		log.Printf("daemon-ws: text frame for %s has no text/data field", msg.AIAgentInstanceID)
		return true // consumed but nothing to inject
	}

	// The server base64-encodes the text for the daemon WebSocket.
	// Decode it before injecting into the PTY.
	decoded, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		// Not base64 — use as-is (plain text fallback).
		decoded = []byte(content)
	}

	// The server wraps both input and control messages as type "binary".
	// After decoding, check if the payload is a known control message
	// (e.g. {"type":"kill"}) and route it instead of injecting as text.
	if len(decoded) > 0 && decoded[0] == '{' {
		var ctrl struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(decoded, &ctrl) == nil {
			switch ctrl.Type {
			case "kill", "retire_agent_instance":
				var full map[string]interface{}
				if json.Unmarshal(decoded, &full) == nil {
					if _, ok := full["ai_agent_instance_id"]; !ok {
						full["ai_agent_instance_id"] = msg.AIAgentInstanceID
					}
					if tagged, err := json.Marshal(full); err == nil {
						d.routeControlFrame(tagged)
						return true
					}
				}
			}
		}
	}

	// Wrap the body in bracketed-paste markers so TUI agents that auto-
	// submit on every internal \n (codex, pi — both ratatui-based) treat
	// the whole envelope as a single paste into the input field. claude-
	// code's Ink-based TUI also honors bracketed paste, so the wrapping
	// is uniform across harnesses. After the end marker, a single \r
	// submits the input as one prompt — and the system-prompt instruction
	// to extract "the body after the blank line" finally applies because
	// the agent sees the whole hearth/1 envelope as one user turn instead
	// of the JSON header and body arriving as separate, partial submits.
	text := bytes.TrimRight(decoded, "\r\n")
	needsSubmit := len(text) > 0 || len(decoded) > 0

	if len(text) > 0 {
		log.Printf("daemon-ws: inject %d bytes to %s", len(text), msg.AIAgentInstanceID)
		payload := make([]byte, 0, len(text)+12)
		payload = append(payload, []byte("\x1b[200~")...)
		payload = append(payload, text...)
		payload = append(payload, []byte("\x1b[201~")...)
		if err := aw.injectFunc(payload); err != nil {
			log.Printf("daemon-ws: inject error for %s: %v", msg.AIAgentInstanceID, err)
		}
	}

	if needsSubmit {
		// Per-harness pause between paste payload and the \r submit
		// byte. Most are happy with ~50ms; gemini-cli's TextInput needs
		// ~300ms to settle and is paired with a SIGWINCH kick after \r
		// (see kickSubmitFunc / Harness.PostSubmit). Un-ported harnesses
		// fall through to the 50ms default. See harness_iface.go.
		delay := 50 * time.Millisecond
		if h, ok := getHarnessByServerName(aw.agent); ok {
			delay = h.SubmitDelay()
		}
		time.Sleep(delay)
		if err := aw.injectFunc([]byte{'\r'}); err != nil {
			log.Printf("daemon-ws: inject error for %s: %v", msg.AIAgentInstanceID, err)
		}
		if aw.kickSubmitFunc != nil {
			time.Sleep(20 * time.Millisecond)
			aw.kickSubmitFunc()
		}
	}

	return true
}

// routeAgentApprovalRequest handles an inbound agent_approval_request
// frame from the server. The frame names this agent as an approver
// for a permission_request the server is currently waiting on; the
// daemon's job is to build a structured prompt describing the
// pending request and inject it as a pseudo-turn so the agent
// reads it as fresh user input. The agent decides by running
// `hearth hh approve <request_id> <allow|deny> [--reason "..."]`
// from its own shell tool. See docs/approver-resolution.md
// §"Agent-as-approver".
//
// Returns true (frame consumed) regardless of whether injection
// succeeds — the agent's host may be offline or the agent may not
// be in d.instances if the daemon restarted since spawn. Both
// cases log and drop. Server's request stays open; other approvers
// continue racing.
func (d *DaemonWS) routeAgentApprovalRequest(raw []byte, agentInstanceID string) bool {
	if agentInstanceID == "" {
		log.Printf("daemon-ws: agent_approval_request missing ai_agent_instance_id")
		return true
	}
	var frame struct {
		RequestID     string          `json:"request_id"`
		InitiatorID   string          `json:"initiator_id"`
		InitiatorKind string          `json:"initiator_kind"`
		ResourceKind  string          `json:"resource_kind"`
		ResourceID    string          `json:"resource_id"`
		Action        string          `json:"action"`
		SubjectKind   string          `json:"subject_kind"`
		Subject       json.RawMessage `json:"subject"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		log.Printf("daemon-ws: agent_approval_request unmarshal failed: %v", err)
		return true
	}
	if frame.RequestID == "" {
		log.Printf("daemon-ws: agent_approval_request missing request_id")
		return true
	}

	d.mu.RLock()
	aw := d.instances[agentInstanceID]
	d.mu.RUnlock()
	if aw == nil || aw.injectFunc == nil {
		log.Printf("daemon-ws: agent_approval_request: no live instance for %s (request_id=%s); dropping", agentInstanceID, frame.RequestID)
		return true
	}

	prompt := buildApprovalPrompt(frame.RequestID, frame.InitiatorID, frame.InitiatorKind,
		frame.ResourceKind, frame.ResourceID, frame.Action, frame.SubjectKind, frame.Subject)

	log.Printf("daemon-ws: agent_approval_request inject %d bytes to %s (request_id=%s)", len(prompt), agentInstanceID, frame.RequestID)
	payload := make([]byte, 0, len(prompt)+12)
	payload = append(payload, []byte("\x1b[200~")...)
	payload = append(payload, prompt...)
	payload = append(payload, []byte("\x1b[201~")...)
	if err := aw.injectFunc(payload); err != nil {
		log.Printf("daemon-ws: agent_approval_request inject error for %s: %v", agentInstanceID, err)
		return true
	}
	delay := 50 * time.Millisecond
	if h, ok := getHarnessByServerName(aw.agent); ok {
		delay = h.SubmitDelay()
	}
	time.Sleep(delay)
	if err := aw.injectFunc([]byte{'\r'}); err != nil {
		log.Printf("daemon-ws: agent_approval_request submit error for %s: %v", agentInstanceID, err)
	}
	if aw.kickSubmitFunc != nil {
		time.Sleep(20 * time.Millisecond)
		aw.kickSubmitFunc()
	}
	return true
}

// routeChatMention delivers an org-chat @mention to the named agent instance.
// The agent is expected to reply using `hearth chat reply --room <id> "..."`.
func (d *DaemonWS) routeChatMention(raw []byte, agentInstanceID string) bool {
	if agentInstanceID == "" {
		log.Printf("daemon-ws: chat_mention missing ai_agent_instance_id")
		return true
	}
	var frame struct {
		RoomID  string `json:"room_id"`
		Message struct {
			SenderName string `json:"sender_name"`
			Text       string `json:"text"`
		} `json:"message"`
		Context []struct {
			SenderName string `json:"sender_name"`
			Text       string `json:"text"`
			CreatedAt  string `json:"created_at"`
		} `json:"context"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil || frame.RoomID == "" {
		log.Printf("daemon-ws: chat_mention unmarshal failed or missing room_id")
		return true
	}

	d.mu.RLock()
	aw := d.instances[agentInstanceID]
	d.mu.RUnlock()
	if aw == nil || aw.injectFunc == nil {
		log.Printf("daemon-ws: chat_mention: no live instance for %s; dropping", agentInstanceID)
		return true
	}

	prompt := buildChatMentionPrompt(frame.RoomID, frame.Message.SenderName, frame.Message.Text, func() []string {
		lines := make([]string, 0, len(frame.Context))
		for _, c := range frame.Context {
			lines = append(lines, fmt.Sprintf("[%s]: %s", c.SenderName, c.Text))
		}
		return lines
	}())

	log.Printf("daemon-ws: chat_mention inject %d bytes to %s (room=%s)", len(prompt), agentInstanceID, frame.RoomID)
	payload := make([]byte, 0, len(prompt)+12)
	payload = append(payload, []byte("\x1b[200~")...)
	payload = append(payload, prompt...)
	payload = append(payload, []byte("\x1b[201~")...)
	if err := aw.injectFunc(payload); err != nil {
		log.Printf("daemon-ws: chat_mention inject error for %s: %v", agentInstanceID, err)
		return true
	}
	delay := 50 * time.Millisecond
	if h, ok := getHarnessByServerName(aw.agent); ok {
		delay = h.SubmitDelay()
	}
	time.Sleep(delay)
	if err := aw.injectFunc([]byte{'\r'}); err != nil {
		log.Printf("daemon-ws: chat_mention submit error for %s: %v", agentInstanceID, err)
	}
	if aw.kickSubmitFunc != nil {
		time.Sleep(20 * time.Millisecond)
		aw.kickSubmitFunc()
	}
	return true
}

func buildChatMentionPrompt(roomID, senderName, text string, contextLines []string) []byte {
	// Wrap in a hearth/1 envelope so the phone's transcript renderer can
	// suppress this injected context — it's agent scaffolding, not a real
	// user message. The agent still receives the full text unchanged.
	var body bytes.Buffer
	if len(contextLines) > 0 {
		body.WriteString("--- Recent org chat ---\n")
		for _, l := range contextLines {
			body.WriteString(l)
			body.WriteByte('\n')
		}
		body.WriteString("--- End context ---\n\n")
	}
	fmt.Fprintf(&body, "[Org Chat from %s]: %s\n\n", senderName, text)
	fmt.Fprintf(&body, "To reply to the chat room, run:\n  hearth chat reply --room %s \"your response\"\n", roomID)
	body.WriteString("You may send multiple replies. Keep responses concise.")

	var out bytes.Buffer
	out.WriteString("hearth/1 {\"kind\":\"chat_context\"}\n\n")
	out.Write(body.Bytes())
	return out.Bytes()
}

// buildApprovalPrompt formats the structured prompt the agent reads
// on its forced turn. Kept separate so it's testable without the
// PTY plumbing. The prompt instructs the agent to call the
// `hearth hh approve` CLI subcommand from its shell tool — this is
// the wire shape for the agent's decision (see
// docs/approver-resolution.md §"Agent-as-approver" — we chose the
// CLI subcommand over per-harness tool registration for uniformity
// across harnesses).
func buildApprovalPrompt(requestID, initiatorID, initiatorKind, resourceKind, resourceID, action, subjectKind string, subject json.RawMessage) []byte {
	var sb bytes.Buffer
	sb.WriteString("hearth: permission_request awaiting your approval.\n\n")
	sb.WriteString("You have been designated as an approver for this request. Use your own\n")
	sb.WriteString("judgment, guided by your system prompt, then respond by running\n")
	sb.WriteString("`hearth hh approve <request_id> <allow|deny> [--reason \"...\"]` from your\n")
	sb.WriteString("shell tool. Membership in the approver set IS your authorization; the\n")
	sb.WriteString("server validates that on the response.\n\n")
	sb.WriteString("Request details:\n")
	fmt.Fprintf(&sb, "  request_id:    %s\n", requestID)
	fmt.Fprintf(&sb, "  initiator:     %s (%s)\n", initiatorID, initiatorKind)
	fmt.Fprintf(&sb, "  resource:      %s:%s\n", resourceKind, resourceID)
	fmt.Fprintf(&sb, "  action:        %s\n", action)
	if subjectKind != "" {
		fmt.Fprintf(&sb, "  subject_kind:  %s\n", subjectKind)
	}
	if len(subject) > 0 && string(subject) != "null" {
		fmt.Fprintf(&sb, "  subject:       %s\n", string(subject))
	}
	sb.WriteString("\nExample responses:\n")
	fmt.Fprintf(&sb, "  hearth hh approve %s allow --reason \"matches policy\"\n", requestID)
	fmt.Fprintf(&sb, "  hearth hh approve %s deny  --reason \"out of scope\"\n", requestID)
	return sb.Bytes()
}

// SendText sends a text frame tagged with the instance's ai_agent_instance_id.
func (aw *agentWS) SendText(data []byte) {
	var msg map[string]interface{}
	if json.Unmarshal(data, &msg) != nil {
		return
	}

	// For permission requests, the ID goes inside "data" (server expects it there).
	// For everything else (transcript, cancel), it goes at the top level.
	msgType, _ := msg["type"].(string)
	if msgType == "permission_request" {
		if dataField, ok := msg["data"].(map[string]interface{}); ok {
			dataField["ai_agent_instance_id"] = aw.aiAgentInstanceID
		}
	}
	msg["ai_agent_instance_id"] = aw.aiAgentInstanceID

	tagged, err := json.Marshal(msg)
	if err != nil {
		return
	}
	aw.daemon.ws.SendText(tagged)
}

// RegisterPending creates a channel for receiving a permission response.
func (aw *agentWS) RegisterPending(requestID string) <-chan []byte {
	return aw.daemon.ws.RegisterPending(requestID)
}

// RemovePending removes a pending request channel.
func (aw *agentWS) RemovePending(requestID string) {
	aw.daemon.ws.RemovePending(requestID)
}

// defaultWSRequestTimeout caps a server round-trip the daemon waits on
// for a CRUD-shaped call. Most paths (rules list, host status, etc.)
// resolve in well under a second; 30s leaves headroom for cold relays
// without hanging the daemon goroutine indefinitely. Ask paths
// override via SendWSRequestTimeout — they wait on a human.
const defaultWSRequestTimeout = 30 * time.Second

// SendWSRequest sends an organization CRUD request to the server over the daemon
// WebSocket and waits for the response. correlationID must be unique per call.
func (d *DaemonWS) SendWSRequest(correlationID, msgType string, data json.RawMessage) ([]byte, error) {
	return d.SendWSRequestTimeout(correlationID, msgType, data, defaultWSRequestTimeout)
}

// SendWSRequestTimeout is the per-call timeout variant. Used by the
// resource-plugin Ask path (preflightAuthorizeResourceInvoke),
// which blocks server-side on a human response and needs longer than
// the 30s CRUD default. The server's defaultTimeout is ~10 min; pass
// something slightly longer here so the daemon-side deadline doesn't
// fire before the server has a chance to return human_timeout itself.
func (d *DaemonWS) SendWSRequestTimeout(correlationID, msgType string, data json.RawMessage, timeout time.Duration) ([]byte, error) {
	msg := map[string]interface{}{
		"type":           "ws_request",
		"correlation_id": correlationID,
		"msg_type":       msgType,
	}
	if len(data) > 0 {
		msg["data"] = json.RawMessage(data)
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	ch := d.ws.RegisterPending(correlationID)
	defer d.ws.RemovePending(correlationID)

	d.ws.SendText(msgBytes)

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("ws_request timed out")
	}
}

// Send is a no-op — PTY output is not sent to the server.
func (aw *agentWS) Send(data []byte) {}

// Close is a no-op — the shared connection is owned by the daemon.
func (aw *agentWS) Close() {}
