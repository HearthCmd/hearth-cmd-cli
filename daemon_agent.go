//go:build darwin || linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// expandHome resolves a leading "~/" to $HOME. Leaves everything else
// (including bare "~", "~user", and absolute paths) untouched — we only
// care about the one shell-style shorthand the iOS wizard emits.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// localAgentForHarness translates a server-side harness name (the value
// in harnesses.name) to the CLI's local agent identifier used by
// buildAgentCommand. Registry lookup only; unknown harnesses (windsurf,
// openai-assistants, etc.) have no local CLI binary and return "".
// See harness_iface.go.
func localAgentForHarness(harness string) string {
	if h, ok := getHarnessByServerName(harness); ok {
		return h.Name()
	}
	return ""
}

// handleCreateAgentInstance forwards create_ai_agent_instance to the server.
// The server is now authoritative for spawning: after it writes the row, it
// pushes a wake frame to the daemon at the target host (which may or may not
// be this daemon), and the existing wake handler takes over. The CLI simply
// relays the server's response — caller sees spawn_error if the target
// daemon was offline.
func (d *Daemon) handleCreateAgentInstance(conn net.Conn, req ipcRequest) {
	resp, err := d.daemonWS.SendWSRequest(generateUUID(), "create_ai_agent_instance", req.WSData)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: err.Error()})
		return
	}
	sendControl(conn, ipcResponse{Type: "ws_response", Data: resp})
}

// spawnAgentInstance creates a fully-functioning agent instance with the full
// daemon machinery (interpose hook, transcript bridge, WS registration with
// the server). The PTY runs detached in the background; the phone and
// `hearth talk` drive it.
//
// The instance's id is the ai_agent_instance_id, so 'org agent stop' can find
// and terminate it without any extra bookkeeping.
func (d *Daemon) spawnAgentInstance(agentInstanceID, agentName, harnessName, cwd, modelProvider, modelName, jobTitle, jobMandate, organizationName, lastSessionID string) (int, error) {
	localAgent := localAgentForHarness(harnessName)
	if localAgent == "" {
		return 0, fmt.Errorf("no local CLI binary maps to harness %q", harnessName)
	}

	// Expand a leading "~/" to $HOME. The path comes from the server and
	// may have been entered on another device (e.g. the iOS wizard) that
	// doesn't know this host's real home. Expand exactly once here so
	// mkdir, cmd.Dir, the transcript-path derivation, and the interpose
	// setup all see the same absolute path.
	cwd = expandHome(cwd)

	// Ensure the agent's working directory exists. The default template is
	// $HOME/hearth_agents which most users won't have yet; without this
	// the exec.Cmd.Dir = cwd below would fail with ENOENT.
	if cwd != "" {
		if err := os.MkdirAll(cwd, 0755); err != nil {
			return 0, fmt.Errorf("create working directory %s: %w", cwd, err)
		}
	}

	req := ipcRequest{
		Agent:             localAgent,
		Project:           agentName, // surface the agent name as the "project" pill
		Cwd:               cwd,
		AIAgentInstanceID: agentInstanceID,
		Winsize:           &ipcWinsize{Rows: 40, Cols: 120},
		ModelProvider:     modelProvider,
		ModelName:         modelName,
		AgentName:         agentName,
		JobTitle:          jobTitle,
		JobMandate:        jobMandate,
		OrganizationName:  organizationName,
		LastSessionID:     lastSessionID,
	}
	s, err := d.newAgentInstance(req)
	if err != nil {
		return 0, err
	}

	d.mu.Lock()
	d.instances[s.aiAgentInstanceID] = s
	d.mu.Unlock()

	// Register the agent's OS PID for peer-cred-based identity
	// resolution. The PID isn't available until cmd.Start() fires
	// inside RunDaemon, so we use the onStarted callback rather than
	// reading cmd.Process.Pid here (which is nil at this point).
	// Cleaned up in the runRelay-done goroutine below.
	if s.relay != nil {
		instanceID := s.aiAgentInstanceID
		s.relay.onStarted = func(pid int) {
			d.registerAgentIdentity(instanceID, pid)
		}
	}

	// Report 'spawning' up front so the phone can show a spinner while
	// the child loads. The relay's onFirstOutput hook flips this to
	// 'running' as soon as the child writes anything — that's our
	// harness-agnostic "the process woke up" signal.
	d.reportPIDStatus(s.aiAgentInstanceID, "spawning")
	instanceID := s.aiAgentInstanceID
	s.relay.onFirstOutput = func() {
		d.reportPIDStatus(instanceID, "running")
	}

	// Run the relay PTY in the background. When the child exits, drop the
	// instance from the registry and report actual liveness via pid_status.
	// We leave status (intent) alone — that's set server-side on explicit
	// sleep/wake/retire commands. Tracked in d.agentWg so Shutdown can wait
	// for pid_status reports to land before closing the daemon WS.
	d.agentWg.Add(1)
	go func() {
		defer d.agentWg.Done()
		waitErr := s.runRelay()
		// Report pid_status BEFORE we do anything that might race with a
		// daemon shutdown closing the WS: the daemon-shutdown path waits
		// on each instance's Stop() (which waits on s.exited, closed by
		// runRelay), so by the time we're here Shutdown may already be
		// waiting to close the WS.
		d.reportPIDStatus(s.aiAgentInstanceID, classifyExit(waitErr))
		d.mu.Lock()
		delete(d.instances, s.aiAgentInstanceID)
		d.mu.Unlock()
		// Drop the PID → agent_id registry entry now that the process
		// is gone. Phase 0 of docs/agent-identity-plan.md.
		if s.relay != nil && s.relay.cmd != nil && s.relay.cmd.Process != nil {
			d.unregisterAgentIdentity(s.relay.cmd.Process.Pid)
		}
		removeAgentPIDFile(s.aiAgentInstanceID)
		s.Stop()
		log.Printf("daemon: spawned agent instance %s ended", s.aiAgentInstanceID)

		// Cycle: respawn if the cycle frame requested it.
		s.cycleMu.Lock()
		cycleCtx := s.cycleSpawnCtx
		s.cycleMu.Unlock()
		if cycleCtx != nil {
			log.Printf("daemon: cycling agent instance %s — respawning", s.aiAgentInstanceID)
			go d.handleWakeAgentInstance(s.aiAgentInstanceID, cycleCtx)
		}
	}()

	// PID is logged by registerAgentIdentity via the onStarted callback
	// once cmd.Start() fires inside RunDaemon. The 0 here is expected.
	log.Printf("daemon: spawning agent instance %s (cwd %s)", s.aiAgentInstanceID, cwd)
	return 0, nil
}

// classifyExit maps a cmd.Wait() error to a pid_status value. A signal
// exit is 'killed' regardless of who sent the signal — the SoT for
// "was this user intent?" is the status (intent) column, not pid_status.
func classifyExit(err error) string {
	if err == nil {
		return "exited"
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return "exited"
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return "killed"
		}
	}
	return "exited"
}

// reportLastSessionID stamps the harness-internal session id we used
// for this spawn onto the server's row. The next wake reads it back
// from spawn_context.last_session_id and resumes instead of starting
// fresh. Best-effort: failure (WS down, server says no) just means the
// next wake will mint a new id, same as today.
func (d *Daemon) reportLastSessionID(agentInstanceID, sessionID string) {
	reportLastSessionIDVia(d.daemonWS, agentInstanceID, sessionID)
}

// reportLastSessionIDVia is the WS-only variant — used by code paths
// (like the transcript streamer) that only hold a *DaemonWS, not a
// full *Daemon. Same best-effort contract.
func reportLastSessionIDVia(ws *DaemonWS, agentInstanceID, sessionID string) {
	if ws == nil || !ws.IsConnected() {
		return
	}
	data, _ := json.Marshal(map[string]interface{}{
		"id":              agentInstanceID,
		"last_session_id": sessionID,
	})
	resp, err := ws.SendWSRequest(generateUUID(), "update_ai_agent_instance", data)
	if err != nil {
		log.Printf("daemon: report last_session_id %s failed: %v", sessionID, err)
		return
	}
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(resp, &parsed) == nil && parsed.Error != "" {
		log.Printf("daemon: report last_session_id %s rejected: %s", sessionID, parsed.Error)
	}
}

// reportPIDStatus sends the daemon's actual-liveness update to the
// server. Best-effort: if the WS isn't connected, we skip — the server
// will eventually flip pid_status='host_disconnected' when it notices
// the daemon drop.
func (d *Daemon) reportPIDStatus(agentInstanceID, pidStatus string) {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		return
	}
	data, _ := json.Marshal(map[string]string{
		"id":         agentInstanceID,
		"pid_status": pidStatus,
	})
	if _, err := d.daemonWS.SendWSRequest(generateUUID(), "set_ai_agent_instance_pid_status", data); err != nil {
		log.Printf("daemon: failed to report pid_status=%s for %s: %v", pidStatus, agentInstanceID, err)
	}
}

// handleSleepAgentInstance is called by the daemon WS when the server
// forwards a sleep command. The status flip is authoritative on the
// server; our job is just to tear down the local child process. No-op
// if the instance isn't currently running on this daemon.
func (d *Daemon) handleSleepAgentInstance(agentInstanceID string) {
	d.mu.Lock()
	s := d.instances[agentInstanceID]
	delete(d.instances, agentInstanceID)
	d.mu.Unlock()
	if s == nil {
		return
	}
	s.Stop()
}

// handleWakeAgentInstance is called by the daemon WS when the server
// forwards a wake command with spawn context. Spawns a fresh child
// process for the instance. If one is already running locally we
// short-circuit — wake is idempotent.
func (d *Daemon) handleWakeAgentInstance(agentInstanceID string, spawnCtx json.RawMessage) {
	d.mu.RLock()
	_, exists := d.instances[agentInstanceID]
	d.mu.RUnlock()
	if exists {
		log.Printf("daemon: wake %s: already running locally, ignoring", agentInstanceID)
		return
	}

	var ctx struct {
		HarnessName      string `json:"harness_name"`
		HostID           string `json:"host_id"`
		DirectoryPath    string `json:"directory_path"`
		ModelProvider    string `json:"model_provider"`
		ModelName        string `json:"model_name"`
		AgentName        string `json:"agent_name"`
		JobTitle         string `json:"job_title"`
		JobMandate       string `json:"job_mandate"`
		OrganizationName string `json:"organization_name"`
		LastSessionID    string `json:"last_session_id"`
	}
	if err := json.Unmarshal(spawnCtx, &ctx); err != nil {
		log.Printf("daemon: wake %s: invalid spawn_context: %v", agentInstanceID, err)
		return
	}
	if ctx.HostID != "" && d.hostID != "" && ctx.HostID != d.hostID {
		// Shouldn't happen — the server looked up our daemon via hostID
		// before forwarding — but guard anyway.
		log.Printf("daemon: wake %s: spawn_context host %s doesn't match this daemon %s", agentInstanceID, ctx.HostID, d.hostID)
		return
	}
	if _, err := d.spawnAgentInstance(agentInstanceID, ctx.AgentName, ctx.HarnessName, ctx.DirectoryPath, ctx.ModelProvider, ctx.ModelName, ctx.JobTitle, ctx.JobMandate, ctx.OrganizationName, ctx.LastSessionID); err != nil {
		log.Printf("daemon: wake %s: spawn failed: %v", agentInstanceID, err)
	}
}

// handleCycleAgentInstance kills the running instance (if any) and respawns
// it once it exits. If no process is running, it spawns immediately (same as
// a wake). Called by the daemon WS when a "cycle" control frame arrives from
// the server.
func (d *Daemon) handleCycleAgentInstance(agentInstanceID string, spawnCtx json.RawMessage) {
	d.mu.Lock()
	inst := d.instances[agentInstanceID]
	if inst != nil {
		inst.cycleMu.Lock()
		inst.cycleSpawnCtx = spawnCtx
		inst.cycleMu.Unlock()
	}
	d.mu.Unlock()

	if inst != nil {
		// Agent is live: SIGKILL the process group. The runRelay cleanup
		// goroutine will see cycleSpawnCtx and respawn after the exit.
		if inst.relay != nil && inst.relay.cmd != nil && inst.relay.cmd.Process != nil {
			log.Printf("daemon: cycle %s: killing running process", agentInstanceID)
			_ = syscall.Kill(-inst.relay.cmd.Process.Pid, syscall.SIGKILL)
		}
		return
	}

	// Agent not running: respawn immediately.
	log.Printf("daemon: cycle %s: not running, spawning directly", agentInstanceID)
	go d.handleWakeAgentInstance(agentInstanceID, spawnCtx)
}
