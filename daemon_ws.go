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
	"strings"
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
	// inboxResolvedFunc, when set, is called as each queued turn leaves an
	// instance's inbox, whatever the outcome. Wired by the owning Daemon so
	// producers that must report a delivery result (scheduled Routines) can,
	// long after their own call returned. See docs/agent-inbox-spec.md.
	inboxResolvedFunc func(e *inboxEntry, outcome string)

	sleepFunc func(aiAgentInstanceID string)
	wakeFunc  func(aiAgentInstanceID string, spawnContext json.RawMessage)
	cycleFunc func(aiAgentInstanceID string, spawnContext json.RawMessage)

	// scheduledTriggerFireFunc handles a server-pushed scheduled_trigger_fire
	// (Routines): spawn a temp agent or wake an existing one, inject the
	// kickoff, and ack. Wired to Daemon.handleScheduledTriggerFire.
	scheduledTriggerFireFunc func(raw json.RawMessage)

	// agentTaskFunc handles a server-pushed agent_task (onboarding): wake the
	// target agent if asleep and inject a reasoning prompt. Wired to
	// Daemon.handleAgentTask.
	agentTaskFunc func(raw json.RawMessage)

	// announceSatelliteFunc handles a server-pushed announce_satellite (voice V5b):
	// the relay asks this host — the one holding the HA connection — to speak a
	// message on a voice satellite on behalf of a (possibly parked) agent, by
	// invoking the HA announce verb. Wired to Daemon.handleAnnounceSatellite.
	announceSatelliteFunc func(aiAgentInstanceID, connection, entityID, message string, listen bool)

	// Identity-cache callbacks. Server pushes "account",
	// "organizations_list", and "agent_home_path" on connect (and on
	// changes); we hand the parsed payloads to the Daemon so
	// `hearth status` can read them out of process memory.
	accountFunc       func(humanUserID, email string)
	organizationsFunc func(orgs []daemonOrgEntry)
	agentHomePathFunc func(dir string)

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

	// installPluginFunc fires on a server-pushed install_plugin command
	// (app-initiated catalog install). The daemon wires it to the same
	// install path `hearth plugin install` uses and reports the outcome
	// back. A callback rather than a direct call for the reason stated
	// above: this layer does not know about plugin state, and should not
	// start now.
	installPluginFunc func(slug, version string, upgrade, force, allowBreaking bool)

	// displayFrameFunc, when set (role-aware daemon with the display role),
	// receives relay→host display_publish / display_clear frames and applies them
	// to the local display subsystem. Set by (*Daemon).startDisplaySubsystem to
	// displayServer.handleRelayFrame; nil on pure agent hosts. This is how the
	// unified daemon drives screens over its single /ws/daemon connection.
	displayFrameFunc func([]byte) bool
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
	injectFunc     func([]byte) error
	killFunc       func()
	// kickSubmitFunc, when set, is called immediately after writing the
	// submit byte during text injection. Used for harnesses (gemini-cli)
	// whose TextInput buffers pasted content past the submit byte and
	// only flushes when an external event (e.g. SIGWINCH from a winsize
	// change) re-enters their main loop.
	kickSubmitFunc func()

	// readiness/inbox/deliverer are the delivery substrate: every turn
	// injected into this agent is queued and drained on an availability
	// edge rather than written straight to the PTY. Wired by
	// StartInboxDelivery once the harness name is known. nil when the
	// daemon's local DB is unavailable, in which case injection falls back
	// to the direct write (see deliverTurn). See docs/agent-inbox-spec.md.
	readiness *agentReadiness
	inbox     *agentInbox
	deliverer *inboxDeliverer
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

// SendText sends an UNtagged text frame on the shared connection (no
// ai_agent_instance_id — unlike agentWS.SendText). The role-aware daemon uses it
// to report display state, which the relay routes by host, not instance. This
// makes *DaemonWS a displayTransport.
func (d *DaemonWS) SendText(data []byte) {
	d.ws.SendText(data)
}

// setDisplayFrameFunc wires the display subsystem's frame handler so relay→host
// display_publish / display_clear frames reach it. Set once when a role-display
// daemon activates the display subsystem.
func (d *DaemonWS) setDisplayFrameFunc(fn func([]byte) bool) {
	d.mu.Lock()
	d.displayFrameFunc = fn
	d.mu.Unlock()
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
	aw := d.instances[id]
	delete(d.instances, id)
	d.mu.Unlock()
	if aw != nil {
		// Stop draining, but leave the queue on disk: anything still
		// pending is delivered when this instance next spawns.
		aw.StopInboxDelivery()
	}
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
		// install_plugin fields — app-initiated catalog install.
		PluginCatalogSlug    string `json:"plugin_catalog_slug"`
		PluginCatalogVersion string `json:"plugin_catalog_version"`
		PluginUpgrade        bool   `json:"plugin_upgrade"`
		PluginForce          bool   `json:"plugin_force"`
		PluginAllowBreaking  bool   `json:"plugin_allow_breaking"`
		// announce_satellite fields (voice V5b).
		Connection string `json:"connection"`
		EntityID   string `json:"entity_id"`
		Message    string `json:"message"`
		// Listen asks the satellite to re-open its microphone after speaking, so
		// the household can answer without the wake word. The relay sends a
		// boolean, never a verb name — see handleAnnounceSatellite.
		Listen bool `json:"listen"`
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
		aw := d.instances[msg.AIAgentInstanceID]
		delete(d.instances, msg.AIAgentInstanceID)
		d.mu.Unlock()
		if aw != nil {
			aw.StopInboxDelivery()
			// A retired agent's queue will never drain, and keeping it
			// would mean a future same-id instance replaying dead
			// messages. This is the one place we discard on purpose.
			if err := aw.inbox.DropInstance(); err != nil {
				log.Printf("WARN daemon-ws: could not clear inbox for retired %s: %v", msg.AIAgentInstanceID, err)
			}
		}
		log.Printf("daemon-ws: retired agent instance %s", msg.AIAgentInstanceID)
	case "install_plugin":
		// App-initiated install. The server has already authorized the
		// request; the daemon still does its own fetching and verifying,
		// because the trust root is the key compiled into this binary and
		// nothing the server says can substitute for it.
		//
		// Runs on its own goroutine: the install makes a network round trip
		// to GitHub, and this is the WS read loop. Blocking here would stall
		// every other frame from the server for the duration.
		if d.installPluginFunc == nil {
			log.Printf("daemon-ws: install_plugin received but no handler wired; ignoring")
			return
		}
		go d.installPluginFunc(msg.PluginCatalogSlug, msg.PluginCatalogVersion,
			msg.PluginUpgrade, msg.PluginForce, msg.PluginAllowBreaking)
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
		if aw != nil {
			aw.StopInboxDelivery()
			if err := aw.inbox.DropInstance(); err != nil {
				log.Printf("WARN daemon-ws: could not clear inbox for destroyed %s: %v", msg.AIAgentInstanceID, err)
			}
		}
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
	case "announce_satellite":
		// Voice V5b — speak a message on a satellite via the HA announce verb, on
		// behalf of the named agent. Off the read loop: it does a server authorize
		// round-trip plus the HA call, and blocking here would stall every other
		// frame.
		if d.announceSatelliteFunc != nil {
			go d.announceSatelliteFunc(msg.AIAgentInstanceID, msg.Connection, msg.EntityID, msg.Message, msg.Listen)
		}
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

	// Display frames (relay→host publish/clear) carry no ai_agent_instance_id and
	// are routed to the display subsystem on a role-display daemon. handleRelayFrame
	// only consumes display_publish/display_clear, so this is inert on agent hosts.
	d.mu.RLock()
	displayFn := d.displayFrameFunc
	d.mu.RUnlock()
	if displayFn != nil && displayFn(data) {
		return true
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
	case "install_plugin":
		// App-initiated catalog install. Host-scoped: a plugin belongs to
		// the host, not to any agent, so this frame carries no
		// ai_agent_instance_id and MUST be routed here — above the
		// agent-id guard below, which drops agent-less frames on the
		// floor without logging. Same trap that swallowed
		// destroy_agent_instance; see the note on that switch.
		d.routeControlFrame(data)
		return true
	case "scheduled_trigger_fire":
		// Routines. Routed here (above the agent-id guard) because temp-mode
		// fires carry NO ai_agent_instance_id — the daemon mints the instance.
		// Existing-mode fires do carry one; both land here and the handler
		// branches on target_mode. Runs in its own goroutine: it spawns/waits
		// and blocks, which must not stall the WS read loop.
		if d.scheduledTriggerFireFunc != nil {
			go d.scheduledTriggerFireFunc(data)
		}
		return true
	case "agent_task":
		// Onboarding: a system-initiated reasoning task for a specific agent (the
		// Facilitator). Carries ai_agent_instance_id but is handled here, above the
		// agent-id routing guard, because wake+wait+inject blocks and must run in
		// its own goroutine off the WS read loop.
		if d.agentTaskFunc != nil {
			go d.agentTaskFunc(data)
		}
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

	// Hand the body to the inbox, which wraps it in bracketed paste and
	// submits it when the harness is actually accepting turns. The wrapping
	// matters because TUI agents that auto-submit on every internal \n
	// (codex, pi — both ratatui-based) would otherwise see the JSON header
	// and the body arrive as two partial submits; the timing matters because
	// a write into a mid-turn harness is silently absorbed rather than
	// becoming a turn (docs/agent-inbox-spec.md §1).
	text := bytes.TrimRight(decoded, "\r\n")
	if len(text) == 0 {
		return true
	}

	// System events (permission_resolved and friends) are observability, and
	// a stale one is noise — give them a much shorter shelf life than a
	// person's message.
	source, ttl := "relay_input", inboxTTLChat
	if isSystemEventEnvelope(text) {
		source, ttl = "system_event", inboxTTLSystemEvent
	}
	d.deliverTurn(msg.AIAgentInstanceID, text, source, ttl)
	return true
}

// isSystemEventEnvelope reports whether a payload is a server-emitted
// system_event envelope rather than something a person or a Routine sent.
func isSystemEventEnvelope(payload []byte) bool {
	s := string(payload)
	if !strings.HasPrefix(s, "hearth/") {
		return false
	}
	line, _, found := strings.Cut(s, "\n")
	if !found {
		return false
	}
	_, header, found := strings.Cut(line, " ")
	if !found {
		return false
	}
	var env struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal([]byte(header), &env) != nil {
		return false
	}
	return env.Kind == "system_event"
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

	d.deliverTurn(agentInstanceID, prompt, "agent_approval_request", inboxTTLApproval)
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

	d.deliverTurn(agentInstanceID, prompt, "chat_mention", inboxTTLChat)
	return true
}

// lookupAgentWS returns the live per-instance handle, or nil if the instance
// isn't registered on this daemon.
func (d *DaemonWS) lookupAgentWS(id string) *agentWS {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.instances[id]
}

// waitForLiveInstance polls until the instance is registered with an inject
// hook (i.e. a spawn has wired it up) or the timeout elapses. Used after a
// wake/temp-spawn, where registration happens asynchronously.
func (d *DaemonWS) waitForLiveInstance(id string, timeout time.Duration) *agentWS {
	deadline := time.Now().Add(timeout)
	for {
		if aw := d.lookupAgentWS(id); aw != nil && aw.injectFunc != nil {
			return aw
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// writeTurnToPTY writes one paste-wrapped prompt to the agent's PTY and submits
// it. This is the raw mechanism, with no readiness check and no confirmation —
// which is precisely why nothing outside the inbox's delivery loop should call
// it. A bare write here is what silently loses messages when the harness is
// mid-turn (docs/agent-inbox-spec.md §1).
//
// For harnesses with an inject gate (codex/gemini) injectFunc blocks until the
// child is ready; others write immediately.
func writeTurnToPTY(aw *agentWS, prompt []byte) error {
	payload := make([]byte, 0, len(prompt)+12)
	payload = append(payload, []byte("\x1b[200~")...)
	payload = append(payload, prompt...)
	payload = append(payload, []byte("\x1b[201~")...)
	if err := aw.injectFunc(payload); err != nil {
		return err
	}

	// Per-harness pause between paste payload and the \r submit byte. Most
	// are happy with ~50ms; gemini-cli's TextInput needs ~300ms to settle
	// and is paired with a SIGWINCH kick after \r (see kickSubmitFunc /
	// Harness.PostSubmit). Un-ported harnesses fall through to the 50ms
	// default. See harness_iface.go.
	delay := 50 * time.Millisecond
	if h, ok := getHarnessByServerName(aw.agent); ok {
		delay = h.SubmitDelay()
	}
	time.Sleep(delay)
	if err := aw.injectFunc([]byte{'\r'}); err != nil {
		return err
	}
	if aw.kickSubmitFunc != nil {
		time.Sleep(20 * time.Millisecond)
		aw.kickSubmitFunc()
	}
	return nil
}

// deliverTurn is the single entry point for putting a turn into an agent's
// context. Every producer goes through it — phone chat, @mentions,
// agent-as-approver pages, system events, scheduled Routine kickoffs — so there
// is no second path that can quietly regress to an unconditional write.
//
// It queues rather than writes: the inbox's drain loop picks the moment, and
// confirms against the transcript that the payload actually became a turn.
// Returns false only when the instance isn't live on this daemon, which is the
// caller's cue to fall back to its existing "no live agent" handling. The inbox
// covers "agent present but unavailable" and deliberately not "host offline" —
// that boundary stays with the relay (docs/agent-inbox-spec.md §6).
func (d *DaemonWS) deliverTurn(id string, prompt []byte, source string, ttl time.Duration) bool {
	aw := d.lookupAgentWS(id)
	if aw == nil || aw.injectFunc == nil {
		return false
	}

	if aw.inbox == nil {
		// No local DB — the daemon logged that loudly at boot. Degrade to
		// the pre-inbox behavior rather than dropping the message entirely.
		log.Printf("WARN daemon-ws: no inbox for %s; writing %s turn straight to the PTY (may be swallowed if the agent is mid-turn)", id, source)
		if err := writeTurnToPTY(aw, prompt); err != nil {
			log.Printf("daemon-ws: %s inject error for %s: %v", source, id, err)
			return false
		}
		return true
	}

	key, probe := inboxKeyAndProbe(prompt)
	if err := aw.inbox.Enqueue(key, prompt, probe, source, ttl); err != nil {
		log.Printf("WARN daemon-ws: could not queue %s turn for %s: %v", source, id, err)
		return false
	}
	log.Printf("daemon-ws: queued %d-byte %s turn for %s (key=%s)", len(prompt), source, id, key)
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
//
// It also marks the agent busy for readiness purposes: an interpose permission
// request in flight means the harness is blocked waiting on a human, which is
// both unambiguously "not accepting turns" and — before the inbox existed — the
// single most common moment for an injected message to be silently swallowed.
func (aw *agentWS) RegisterPending(requestID string) <-chan []byte {
	if aw.readiness != nil {
		aw.readiness.AskStarted(requestID)
	}
	return aw.daemon.ws.RegisterPending(requestID)
}

// RemovePending removes a pending request channel. Idempotent, and so is the
// readiness side (it tracks a set of request ids, not a count).
func (aw *agentWS) RemovePending(requestID string) {
	if aw.readiness != nil {
		aw.readiness.AskEnded(requestID)
	}
	aw.daemon.ws.RemovePending(requestID)
}

// StartInboxDelivery stands up this instance's readiness tracker, queue, and
// drain loop. Called from the spawn path once aw.agent is known (the harness
// name selects the transcript classifier). Safe to call with a nil db — the
// instance then falls back to direct PTY writes, loudly.
func (aw *agentWS) StartInboxDelivery(db *DaemonDB) {
	aw.readiness = newAgentReadiness(aw.aiAgentInstanceID, aw.agent)
	if db == nil || db.db == nil {
		log.Printf("WARN daemon-ws: no local DB — agent %s has no message inbox; turns injected while it is busy may be lost", aw.aiAgentInstanceID)
		return
	}
	aw.inbox = newAgentInbox(db, aw.aiAgentInstanceID)
	aw.deliverer = newInboxDeliverer(aw.aiAgentInstanceID, aw.inbox, aw.readiness, func(payload []byte) error {
		return writeTurnToPTY(aw, payload)
	})
	// Read the hook at call time, not now: the daemon wires it at boot, and
	// binding it here would make the ordering of two unrelated setup steps
	// load-bearing.
	aw.deliverer.onResolved = func(e *inboxEntry, outcome string) {
		if f := aw.daemon.inboxResolvedFunc; f != nil {
			f(e, outcome)
		}
	}
	go aw.deliverer.run()

	if depth, err := aw.inbox.Depth(); err == nil && depth > 0 {
		// Messages queued before a daemon restart or an agent sleep. This
		// surviving is the entire point of persisting the queue.
		log.Printf("daemon-ws: agent %s has %d message(s) waiting from a previous session", aw.aiAgentInstanceID, depth)
	}
}

// StopInboxDelivery ends the drain loop. The queue itself persists — an agent
// that stops mid-queue drains on its next spawn.
func (aw *agentWS) StopInboxDelivery() {
	if aw.deliverer != nil {
		aw.deliverer.Stop()
	}
}

// ObserveTranscriptLine feeds one bridge-shape line to the readiness tracker.
// Called from the bridge tail, which reads every line anyway.
func (aw *agentWS) ObserveTranscriptLine(line []byte) {
	if aw.readiness != nil {
		aw.readiness.ObserveLine(line)
	}
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

// SendWSRequestAs is SendWSRequest with an explicit caller principal, attached
// to the frame as principal_kind/principal_id so the relay authorizes the
// action against that principal. Used by the CLI-relay path (handleWSRequest),
// which resolves the calling agent from the IPC caller's process tree
// (derivePrincipal). Empty principalKind sends no principal fields — the relay
// then defaults the caller to the host-owner human (daemon-self calls, and
// human-operator CLI calls).
func (d *DaemonWS) SendWSRequestAs(correlationID, msgType string, data json.RawMessage, principalKind, principalID string) ([]byte, error) {
	return d.SendWSRequestTimeoutAs(correlationID, msgType, data, principalKind, principalID, defaultWSRequestTimeout)
}

// SendWSRequestTimeout is the per-call timeout variant. Used by the
// resource-plugin Ask path (preflightAuthorizeResourceInvoke),
// which blocks server-side on a human response and needs longer than
// the 30s CRUD default. The server's defaultTimeout is ~10 min; pass
// something slightly longer here so the daemon-side deadline doesn't
// fire before the server has a chance to return human_timeout itself.
func (d *DaemonWS) SendWSRequestTimeout(correlationID, msgType string, data json.RawMessage, timeout time.Duration) ([]byte, error) {
	return d.SendWSRequestTimeoutAs(correlationID, msgType, data, "", "", timeout)
}

// SendWSRequestTimeoutAs is SendWSRequestTimeout carrying an explicit caller
// principal (see SendWSRequestAs). Empty principalKind omits the principal
// fields entirely.
func (d *DaemonWS) SendWSRequestTimeoutAs(correlationID, msgType string, data json.RawMessage, principalKind, principalID string, timeout time.Duration) ([]byte, error) {
	msg := map[string]interface{}{
		"type":           "ws_request",
		"correlation_id": correlationID,
		"msg_type":       msgType,
	}
	if len(data) > 0 {
		msg["data"] = json.RawMessage(data)
	}
	if principalKind != "" {
		msg["principal_kind"] = principalKind
		msg["principal_id"] = principalID
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
