//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// agentStopGrace is how long Stop() waits after SIGTERM for the child
// to exit on its own before escalating to SIGKILL. See
// docs/daemon-agent-lifecycle.md for the rationale (claude can be slow
// to finish a turn).
const agentStopGrace = 10 * time.Second

// AgentInstance represents a single running agent owned by the daemon.
// Each AgentInstance corresponds exactly to one ai_agent_instance_id (one
// conversation) on the server. All instances are detached: the PTY runs
// in the background and the user interacts via the iOS app or
// `hearth talk`.
type AgentInstance struct {
	aiAgentInstanceID string
	agent             string
	project           string
	cwd               string
	deviceID          string
	startedAt         time.Time
	daemon            *Daemon

	relay *Relay

	interposeSock  string
	interposeClean func()
	interposeRelay *interposeRelay

	bridgePath     string
	bridgeDone     chan struct{}
	bridgeFinished chan struct{}

	transcriptCancel context.CancelFunc

	// attachCancel closes the per-instance attach socket and tears
	// down the attachHub. nil for harnesses where attach isn't
	// supported (see attachSupportedForAgent).
	attachCancel func()

	libPath      string
	libExtracted bool

	// exited is closed once runRelay() returns (child is fully reaped).
	// Stop() waits on this to know when the grace window has paid off.
	exited chan struct{}
	// stopOnce guards Stop() so the daemon-shutdown path and the
	// child-exited-naturally path (daemon_agent.go) don't double-clean.
	stopOnce sync.Once

	// cycleSpawnCtx, when non-nil, tells the runRelay cleanup goroutine
	// to respawn this instance after it exits. Set by the cycle frame
	// handler before the kill signal is sent; read once after exit.
	cycleMu       sync.Mutex
	cycleSpawnCtx []byte
}

// newAgentInstance creates and starts a new agent instance. It sets up the
// PTY, WebSocket,
// interpose, bridge, and transcript streamer, but does NOT touch the terminal
// (that's the client's job).
func (d *Daemon) newAgentInstance(req ipcRequest) (*AgentInstance, error) {
	if wsURL == "" {
		return nil, fmt.Errorf("no relay server URL configured")
	}

	agent := resolveAgent(req.Agent)
	if !knownAgents[agent] {
		return nil, fmt.Errorf("unknown agent %q", agent)
	}

	cwd := req.Cwd
	if cwd == "" {
		return nil, fmt.Errorf("working directory is required")
	}

	// The agent's "device_id" is the daemon's human_user_id. Fall back to
	// env/config only if the daemon hasn't resolved one yet (shouldn't happen
	// once the daemon is registered).
	devID := d.humanUserID
	if devID == "" {
		devID = os.Getenv("HEARTH_DEVICE_ID")
	}
	if devID == "" {
		devID = readConfigValue("io_device_id")
	}
	if devID == "" {
		return nil, fmt.Errorf("not logged in — run 'hearth login <email>' first")
	}

	proj := req.Project
	if proj == "" {
		proj = os.Getenv("HEARTH_PROJECT")
	}
	if proj == "" {
		proj = readConfigValue("project")
	}
	if proj == "" && cwd != "" {
		proj = filepath.Base(cwd)
	}

	// Refuse spawn if the harness binary is below its declared
	// MinimumVersion. Probed once at daemon startup; the cached
	// version is what CheckSpawnPreconditions reads. Failure here
	// surfaces as a clear "please upgrade" error rather than the
	// silent misbehavior of running mismatched code against an old
	// binary.
	if h, ok := getHarness(agent); ok {
		if err := CheckSpawnPreconditions(h); err != nil {
			return nil, err
		}
	}

	// Build agent command with its agent-internal session ID and flags.
	// identityPrompt stamps name/role/mandate/org into the agent's system
	// prompt; empty for talk-style sessions that have no JD context.
	identityPrompt := buildIdentityPrompt(req.AgentName, req.JobTitle, req.JobMandate, req.OrganizationName)
	// Pre-fetch entity caches per connection so the prompt builder
	// stays pure (no DB handle in its signature). Nil/empty on hosts
	// where localDB couldn't be opened — prompt still renders, just
	// without entity lists.
	entitiesByConn := map[string][]Entity{}
	if d.localDB != nil {
		for _, c := range d.resourceConnections.List() {
			if ents, err := d.localDB.ListEntities(c.ConnectionID); err == nil && len(ents) > 0 {
				entitiesByConn[c.ConnectionID] = ents
			}
		}
	}
	resourcePrompt := buildResourcePluginPrompt(req.AIAgentInstanceID, d.agentGrants, d.resourceConnections, d.plugins, entitiesByConn)
	setup, err := buildAgentCommand(agent, identityPrompt, cwd, req.LastSessionID, resourcePrompt)
	if err != nil {
		return nil, err
	}
	command := setup.Command
	cmdArgs := setup.Args
	aiAgentInstanceID := setup.AIAgentInstanceID
	// Honor an explicit ai_agent_instance_id override (used by spawnAgentInstance
	// so the instance is keyed by the supplied ID rather than a fresh UUID).
	if req.AIAgentInstanceID != "" {
		aiAgentInstanceID = req.AIAgentInstanceID
		setup.AIAgentInstanceID = req.AIAgentInstanceID
	}

	// Register instance with the server via daemon WS (no phone approval needed)
	if d.daemonWS == nil {
		return nil, fmt.Errorf("daemon WebSocket not connected")
	}
	if err := d.daemonWS.ConnectAgentInstance(aiAgentInstanceID, proj, agentServerName(agent), cwd, version); err != nil {
		return nil, fmt.Errorf("agent_instance_connect failed: %w", err)
	}

	// Report the session id we ended up using so the next wake gets it
	// back via spawn_context.last_session_id and the harness reattaches
	// its prior context window. Gated on the harness opting in via
	// ReportsResumeID — codex/gemini return false because their wake
	// path resolves sessions differently (codex via cwd, gemini via
	// JSONL header). Best-effort and async — failure here just means
	// next wake mints fresh, which is the same behavior we have today.
	if setup.AgentSessionID != "" {
		if h, ok := getHarness(agent); ok && h.ReportsResumeID() {
			go d.reportLastSessionID(aiAgentInstanceID, setup.AgentSessionID)
		}
	}

	// PreSpawn absorbs trust pre-accepts, project-local settings seeds,
	// and instruction-file installs. Errors are logged, never fatal —
	// matches the pre-SPI helpers' best-effort contract.
	if h, ok := getHarness(agent); ok {
		hctx := HarnessCtx{
			AIAgentInstanceID: aiAgentInstanceID,
			IdentityPrompt:    identityPrompt,
			Cwd:               cwd,
		}
		if err := h.PreSpawn(hctx); err != nil {
			log.Printf("PreSpawn(%s) failed: %v", agent, err)
		}
		// RemoveSkill reconcile: strip skill files for connections that are
		// no longer granted to this agent (e.g. grant was deleted while the
		// agent was sleeping). Idempotent — RemoveSkill is a no-op for
		// connections that were never installed.
		grantedSet := make(map[string]bool)
		for _, id := range d.agentGrants.GrantedConnectionIDs(aiAgentInstanceID) {
			grantedSet[id] = true
		}
		for _, conn := range d.resourceConnections.List() {
			if !grantedSet[conn.ConnectionID] {
				skillKey := conn.Slug
				if skillKey == "" {
					skillKey = conn.ConnectionID
				}
				if err := h.RemoveSkill(hctx, skillKey, conn.PluginSlug); err != nil {
					log.Printf("RemoveSkill(%s, conn=%s): %v", agent, skillKey, err)
				}
			}
		}

		// InstallSkill: for each resource connection this agent has been
		// granted, install the plugin's skill.md via the harness-native
		// mechanism (Claude Code .claude/skills/; others inline into their
		// instruction file). Best-effort — a missing or unreadable skill.md
		// is silently skipped; the agent still runs, just without the skill.
		for _, connID := range d.agentGrants.GrantedConnectionIDs(aiAgentInstanceID) {
			conn, ok := d.resourceConnections.Get(connID)
			if !ok {
				continue
			}
			skillPath := filepath.Join(d.pluginsDir, conn.PluginSlug, "skill.md")
			content, err := os.ReadFile(skillPath)
			if err != nil {
				continue // no skill.md for this plugin
			}
			// Use slug (not UUID) as the skill directory key so directory
			// names stay human-readable (e.g. github-github_work/).
			skillKey := conn.Slug
			if skillKey == "" {
				skillKey = connID
			}
			if err := h.InstallSkill(hctx, skillKey, conn.PluginSlug, content); err != nil {
				log.Printf("InstallSkill(%s, conn=%s): %v", agent, skillKey, err)
			}
		}
	}

	s := &AgentInstance{
		aiAgentInstanceID: aiAgentInstanceID,
		agent:             agent,
		project:           proj,
		cwd:               cwd,
		deviceID:          devID,
		startedAt:         time.Now(),
		daemon:            d,
		exited:            make(chan struct{}),
	}

	// Create bridge file
	s.bridgePath = filepath.Join(os.TempDir(), "hearth-bridge-"+aiAgentInstanceID)
	if f, err := os.Create(s.bridgePath); err == nil {
		f.Close()
	}

	exportEnvs := buildExportEnvs(devID, aiAgentInstanceID, proj, s.bridgePath, agent, req.ModelName)
	command, cmdArgs, interpose, err := setupInterpose(agent, command, cmdArgs, aiAgentInstanceID, cwd, exportEnvs)
	if err != nil {
		s.cleanup()
		return nil, fmt.Errorf("interpose setup failed: %w", err)
	}
	s.libPath = interpose.LibPath
	s.libExtracted = interpose.LibExtracted
	s.interposeSock = interpose.SockPath
	s.interposeClean = interpose.SockCleanup
	s.interposeRelay = interpose.Relay

	// Create the relay (PTY only, no per-instance WebSocket) in daemon mode
	r, err := NewDaemon(command, cmdArgs, exportEnvs, cwd, req.Winsize)
	if err != nil {
		s.cleanup()
		return nil, fmt.Errorf("failed to create relay: %w", err)
	}
	s.relay = r

	// Per-harness relay configuration. NeedsInjectGate covers the
	// "first user message gets eaten because the TUI hadn't enabled
	// bracketed paste yet" failure mode (codex + gemini). WarmupPayload
	// primes the child with bytes the moment the gate opens — codex
	// uses this to flush its rollout JSONL by the time the user's real
	// first turn lands; AGENTS.md tells codex to ignore the warmup and
	// the transcript transformer hides it from iOS.
	if h, ok := getHarness(agent); ok {
		if h.NeedsInjectGate() {
			r.EnableInjectGate()
		}
		if payload := h.WarmupPayload(); payload != nil {
			r.SetWarmupPayload(payload)
		}
	}

	// Register this instance with the daemon's shared WebSocket
	killFunc := func() {
		if r.cmd.Process != nil {
			r.killed = true
			syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	r.killFunc = killFunc
	if d.daemonWS != nil {
		aw := d.daemonWS.RegisterAgentInstance(aiAgentInstanceID, r.Inject, killFunc)
		aw.project = proj
		aw.agent = agentServerName(agent)
		aw.cwd = cwd
		aw.version = version
		aw.agentSessionID = setup.AgentSessionID
		// Hook the harness's PostSubmit (e.g. gemini's SIGWINCH-after-\r)
		// to the daemon-ws submit path. For ported harnesses with a
		// no-op PostSubmit (claude, pi, today) this still attaches a
		// closure but the call is harmless. Un-ported harnesses fall
		// through with kickSubmitFunc nil — same as before. See
		// harness_iface.go.
		if h, ok := getHarness(agent); ok {
			aw.kickSubmitFunc = func() {
				if r.cmd != nil && r.cmd.Process != nil {
					_ = h.PostSubmit(r.cmd.Process)
				}
			}
		}
		r.wsConn = aw

		// Start bridge tailer using the instance's WS handle
		s.bridgeDone = make(chan struct{})
		s.bridgeFinished = make(chan struct{})
		go func() {
			tailBridge(s.bridgePath, aw, s.bridgeDone, agentServerName(agent))
			close(s.bridgeFinished)
		}()
	}

	// Per-instance attach socket (`hearth agent attach`). Only stood
	// up for harnesses we've validated — see attachSupportedForAgent.
	// The hub is hung off the relay so the PTY drain in RunDaemon
	// can fan-out without per-frame branching.
	if attachSupportedForAgent(agent) {
		r.attachHub = newAttachHub()
		if cancel, err := startAttachListener(s); err != nil {
			log.Printf("daemon: attach listener failed: %v", err)
			// Non-fatal — agent still runs, attach just won't work
			// for this instance. Operator can re-spawn if they want it.
			r.attachHub = nil
		} else {
			s.attachCancel = cancel
		}
	}

	// Set up prompt relay so interpose can show prompts
	s.interposeRelay.SetRelay(r)

	// Start transcript streamer
	startTime := time.Now()
	var transcriptCtx context.Context
	transcriptCtx, s.transcriptCancel = context.WithCancel(context.Background())
	go startTranscriptStreamer(transcriptCtx, agent, aiAgentInstanceID, setup.AgentSessionID, s.bridgePath, cwd, startTime, d.daemonWS)

	// The caller is responsible for starting the PTY via s.runRelay() in a
	// goroutine so it can attach its own lifecycle cleanup (see daemon_agent.go's
	// spawnAgentInstance for the canonical pattern).

	return s, nil
}

// runRelay runs the PTY child process. When it exits, the instance is
// done; the returned error is the result of cmd.Wait() — nil for a clean
// exit, *exec.ExitError for any nonzero rc or signal. Callers classify
// this into a pid_status value.
func (s *AgentInstance) runRelay() error {
	err := s.relay.RunDaemon()

	s.transcriptCancel()
	killStreamer(s.aiAgentInstanceID)

	if s.bridgeDone != nil {
		close(s.bridgeDone)
		<-s.bridgeFinished
	}

	// Signal Stop() (and any other waiter) that the child is fully reaped.
	// cmd.Wait() above means the OS has released the PID, so Stop() no
	// longer needs to worry about sending SIGKILL at this point.
	close(s.exited)

	log.Printf("daemon: agent instance %s relay exited (err=%v)", s.aiAgentInstanceID, err)
	return err
}

// Stop terminates the instance, waits for the child process to exit
// (SIGTERM, grace window, SIGKILL if still alive), and cleans up.
//
// Idempotent: Stop is called from both (a) the runRelay-completion path
// in daemon_agent.go once the child exits on its own, and (b) the
// daemon-shutdown path in daemon.go. Only the first caller does the
// signalling + cleanup; subsequent callers return immediately. In case
// (a) the child is already dead by the time Stop runs — s.exited is
// already closed — so the signalling is effectively a no-op and Stop
// proceeds straight to cleanup.
func (s *AgentInstance) Stop() {
	s.stopOnce.Do(func() {
		// The transcript streamer is a detached process (its own session) and
		// will outlive the daemon unless we reap it explicitly. Do this
		// synchronously — the relay-exit path also calls killStreamer, but that
		// goroutine isn't guaranteed to run before the daemon process exits.
		killStreamer(s.aiAgentInstanceID)

		alreadyDead := false
		if s.exited != nil {
			select {
			case <-s.exited:
				alreadyDead = true
			default:
			}
		}

		if !alreadyDead && s.relay != nil && s.relay.cmd != nil && s.relay.cmd.Process != nil {
			// Try a graceful shutdown first. If the child is claude
			// mid-turn, it may take several seconds to wrap up — hence
			// the 10s grace (see docs/daemon-agent-lifecycle.md).
			_ = s.relay.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-s.exited:
				// Child exited within the grace window.
			case <-time.After(agentStopGrace):
				log.Printf("daemon: agent instance %s didn't exit within %s, sending SIGKILL",
					s.aiAgentInstanceID, agentStopGrace)
				// Kill the whole process group — claude forks children
				// (mcp servers, etc.) that need to go down with it.
				if s.relay.cmd.Process != nil {
					s.relay.killed = true
					_ = syscall.Kill(-s.relay.cmd.Process.Pid, syscall.SIGKILL)
				}
				// Wait for runRelay to finish reaping so cleanup
				// doesn't race with file descriptor teardown.
				if s.exited != nil {
					<-s.exited
				}
			}
		}

		s.cleanup()
	})
}

// cleanup removes instance-specific files and resources.
func (s *AgentInstance) cleanup() {
	if s.attachCancel != nil {
		s.attachCancel()
		s.attachCancel = nil
	}
	if s.interposeClean != nil {
		s.interposeClean()
	}
	if s.libPath != "" && s.libExtracted {
		os.Remove(s.libPath)
	}
	if s.bridgePath != "" {
		os.Remove(s.bridgePath)
	}

	if s.interposeRelay != nil {
		s.interposeRelay.ClearRelay(s.relay)
	}

	// Notify server and unregister from daemon's shared WebSocket
	if s.daemon != nil && s.daemon.daemonWS != nil {
		s.daemon.daemonWS.DisconnectAgentInstance(s.aiAgentInstanceID)
		s.daemon.daemonWS.UnregisterAgentInstance(s.aiAgentInstanceID)
	}
}

// NewDaemon creates a Relay configured for a detached agent instance.
// The PTY is created and its window size set from `winsize`; the child
// process's working directory is set to cwd.
func NewDaemon(command string, args []string, exportEnvs map[string]string, cwd string, winsize *ipcWinsize) (*Relay, error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("openPTY: %w", err)
	}

	// Set initial window size. If the caller (iOS spawn path) doesn't
	// supply one, fall back to a conventional 24x80 — Ink/React TUIs
	// (gemini-cli in particular) silently refuse to render into a 0x0
	// PTY, which is the kernel default. Claude/codex tolerate it; gemini
	// produces zero PTY output until something resizes the terminal.
	ws := &Winsize{Row: 24, Col: 80}
	if winsize != nil && winsize.Rows > 0 && winsize.Cols > 0 {
		ws = &Winsize{Row: winsize.Rows, Col: winsize.Cols}
	}
	setWinsize(master.Fd(), ws)

	// Build the child's environment: daemon's env + hearth-specific vars.
	childEnv := os.Environ()
	for k, v := range exportEnvs {
		childEnv = append(childEnv, k+"="+v)
	}

	// Resolve the command binary using the child's PATH (not the daemon's).
	// exec.Command uses LookPath with the current process's PATH, which may
	// point to a different binary. Temporarily swap PATH for resolution.
	resolvedCmd := command
	for _, e := range childEnv {
		if k, v, ok := strings.Cut(e, "="); ok && k == "PATH" {
			origPath := os.Getenv("PATH")
			os.Setenv("PATH", v)
			if p, err := exec.LookPath(command); err == nil {
				resolvedCmd = p
			}
			os.Setenv("PATH", origPath)
			break
		}
	}

	cmd := newAgentCmd(resolvedCmd, args)
	cmd.Dir = cwd
	cmd.Env = childEnv

	r := &Relay{
		cmd:    cmd,
		master: master,
		slave:  slave,
	}

	return r, nil
}

// RunDaemon starts the child process, drains PTY output (no attached terminal),
// and returns when the child exits. Transcripts flow to the phone via the
// streamer + bridge file, not through the PTY.
func (r *Relay) RunDaemon() error {
	defer r.cleanupDaemon()

	// Start child process on the slave PTY
	r.cmd.Stdin = r.slave
	r.cmd.Stdout = r.slave
	r.cmd.Stderr = r.slave
	r.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    3,
	}
	r.cmd.ExtraFiles = []*os.File{r.slave}

	if err := r.cmd.Start(); err != nil {
		return fmt.Errorf("start child: %w", err)
	}
	log.Printf("daemon: child started (pid %d, cmd %s)", r.cmd.Process.Pid, r.cmd.Path)
	if r.onStarted != nil {
		r.onStarted(r.cmd.Process.Pid)
	}

	r.slave.Close()
	r.slave = nil

	// Drain PTY output into the void — nothing consumes it directly. The
	// agent's own transcript JSONL is the canonical source of user-visible
	// activity, read by the streamer and forwarded via WebSocket.
	//
	// The very first non-empty read fires onFirstOutput so the daemon can
	// promote pid_status from 'spawning' to 'running'. Most harnesses stay
	// silent until their UI is drawn, so this is a reasonable "process is
	// alive" heartbeat without per-agent heuristics.
	// DEBUG-ONLY: optional PTY tee. If HEARTH_DEBUG_PTY_DIR is set, mirror
	// raw PTY output to <dir>/pty-<pid>.bin so we can post-mortem what a
	// TUI rendered (or didn't). Inert when the env var is unset. Earned
	// its keep diagnosing gemini-cli's silent paste-buffering bug —
	// invaluable when the bridge file is empty and you can't otherwise
	// tell whether the TUI rendered at all. Grep `DEBUG-ONLY` to find
	// every diagnostic-only block in the codebase.
	var dbgF *os.File
	if dir := os.Getenv("HEARTH_DEBUG_PTY_DIR"); dir != "" {
		path := fmt.Sprintf("%s/pty-%d.bin", dir, r.cmd.Process.Pid)
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
			log.Printf("daemon: PTY debug tee → %s", path)
			dbgF = f
		} else {
			log.Printf("daemon: PTY debug tee open failed: %v", err)
		}
	}

	done := make(chan error, 1)
	go func() {
		if dbgF != nil {
			defer dbgF.Close()
		}
		buf := make([]byte, 4096)
		sawOutput := false
		for {
			n, err := r.master.Read(buf)
			if n > 0 {
				if dbgF != nil {
					dbgF.Write(buf[:n])
				}
				if r.attachHub != nil {
					r.attachHub.Feed(buf[:n])
				}
				if !sawOutput {
					sawOutput = true
					if r.onFirstOutput != nil {
						r.onFirstOutput()
					}
				}
				r.noteOutput()
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()

	// Wait for child to exit
	waitErr := r.cmd.Wait()

	r.master.Close()
	r.master = nil

	<-done

	return waitErr
}

func (r *Relay) cleanupDaemon() {
	if r.master != nil {
		r.master.Close()
	}
	if r.slave != nil {
		r.slave.Close()
	}
}

// newAgentCmd wraps exec.Command — kept as an indirection point for any
// harness that needs to splice in workarounds before exec.
func newAgentCmd(command string, args []string) *exec.Cmd {
	return exec.Command(command, args...)
}
