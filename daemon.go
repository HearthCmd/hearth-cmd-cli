//go:build darwin || linux

package main

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ipcRequest is the JSON control message sent by CLI subcommands to the daemon.
// It also doubles as the internal parameter object that daemon_agent.go hands
// to newAgentInstance when spawning an agent via 'org agent create' — in that
// case it's not serialized over IPC, just passed in-process.
type ipcRequest struct {
	Type      string            `json:"type"` // status, stop, update_shutdown, ws_request
	Agent     string            `json:"agent,omitempty"`
	Project   string            `json:"project,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Winsize   *ipcWinsize       `json:"winsize,omitempty"`
	Force     bool              `json:"force,omitempty"`
	WSMsgType string            `json:"ws_msg_type,omitempty"` // for ws_request: the CRUD message type
	WSData    json.RawMessage   `json:"ws_data,omitempty"`     // for ws_request: the payload

	// resource_invoke fields. Carry the Resource Connection id, verb,
	// and args (JSON object) from the CLI through to
	// PluginSupervisor.Invoke. Daemon-side handler lands in commit 3
	// of the 1c plan; the fields ship here so the CLI's outgoing
	// frame is fully typed.
	ResourceConnectionID string          `json:"resource_connection_id,omitempty"`
	ResourceVerb         string          `json:"resource_verb,omitempty"`
	ResourceArgs         json.RawMessage `json:"resource_args,omitempty"`
	// SecretBindings maps an env-var name the verb's invoke needs to
	// the secret id that fulfills it: `{HA_TOKEN: 'sec-abc'}`. Set by
	// the CLI from `--secret HA_TOKEN=sec-abc` flags. Daemon resolves
	// each id via the server's authorized secrets_get, decrypts
	// locally, and hands cleartexts to the plugin via
	// InvokeParams.SecretBindings. Never holds cleartext.
	SecretBindings map[string]string `json:"secret_bindings,omitempty"`

	// Principal of the calling actor. Daemon-derived as of Phase 1 of
	// docs/agent-identity-plan.md (peer-cred + process-tree walk).
	// These IPC fields are retained for the legacy fallback path
	// inside derivePrincipal — used only when peerPID can't read the
	// connection's ucred (non-Unix conn, e.g. net.Pipe in tests).
	// CLI no longer populates them as of Phase 2. Production daemons
	// connect over Unix sockets, so peerPID always succeeds and these
	// fields are inert on the operator's box.
	ResourcePrincipalKind string `json:"resource_principal_kind,omitempty"`
	ResourcePrincipalID   string `json:"resource_principal_id,omitempty"`

	// `hearth secret` CLI plumbing. Cleartext crosses the local unix
	// socket; daemon encrypts to its own pubkey before relaying
	// ciphertext to the server. The set path takes a freeform label
	// (SecretName) + optional advisory purpose. Authorization is IAM-
	// rules driven server-side; this struct doesn't model it.
	SecretName    string `json:"secret_name,omitempty"`
	SecretPurpose string `json:"secret_purpose,omitempty"`
	SecretValue   string `json:"secret_value,omitempty"` // cleartext, write-only
	// grant/revoke add/remove (principal, secret:<id>, secret.use, allow)
	// rules via the server's IAM machinery.
	SecretGrantPrincipalKind string `json:"secret_grant_principal_kind,omitempty"`
	SecretGrantPrincipalID   string `json:"secret_grant_principal_id,omitempty"`
	// `hearth plugin` CLI plumbing. Install ships an absolute archive
	// path across the socket; the daemon opens it directly (same-host
	// FS, same user). Uninstall ships the slug + force flag.
	PluginArchivePath string `json:"plugin_archive_path,omitempty"`
	PluginSlug        string `json:"plugin_slug,omitempty"`
	PluginUpgrade     bool   `json:"plugin_upgrade,omitempty"`
	PluginForce       bool   `json:"plugin_force,omitempty"`

	// `hearth hh approve` CLI plumbing — approver-resolution phase 5b.
	// Forwarded to the server as tool_approve_permission_request when
	// the IPC caller derives to an agent principal; as a regular
	// permission_response when the caller is a human at the terminal.
	ApproveRequestID string `json:"approve_request_id,omitempty"`
	ApproveDecision  string `json:"approve_decision,omitempty"` // "allow" | "deny"
	ApproveReason    string `json:"approve_reason,omitempty"`

	// Chat reply fields — used by `hearth chat reply` to forward an agent's
	// chat message through the daemon to the server.
	ChatRoomID          string `json:"chat_room_id,omitempty"`
	ChatAgentInstanceID string `json:"chat_agent_instance_id,omitempty"`
	ChatText            string `json:"chat_text,omitempty"`

	// SecretID is the server-issued UUID. Operators obtain it from
	// `hearth secret list` output and pass it to delete/grant/revoke.
	SecretID string `json:"secret_id,omitempty"`
	// AIAgentInstanceID is the id of the ai_agent_instances row the caller
	// wants to spawn — set by spawnAgentInstance after create_ai_agent_instance.
	AIAgentInstanceID string `json:"-"`
	// ModelProvider + ModelName come from the spawn_context of
	// create_ai_agent_instance. They translate into per-agent env vars (e.g.
	// ANTHROPIC_MODEL for claude) so the child agent respects the user's
	// model choice instead of falling back to its own default.
	ModelProvider string `json:"-"`
	ModelName     string `json:"-"`
	// Identity metadata carried in spawn_context. Stamped into the agent's
	// system prompt (or instruction file, for harnesses that don't accept
	// system-prompt args) so the model knows who it is, what role it
	// fills, and which household it belongs to.
	AgentName        string `json:"-"`
	JobTitle         string `json:"-"`
	JobMandate       string `json:"-"`
	OrganizationName string `json:"-"`
	// LastSessionID is the harness-internal session id from the prior
	// spawn, threaded down from the wake spawn_context. Empty on first
	// wake. buildAgentCommand uses it to reattach the harness's prior
	// context window for harnesses that accept a session id flag
	// (claude, copilot, pi); validated against the on-disk transcript
	// existing before reuse, otherwise mints fresh.
	LastSessionID string `json:"-"`
}

// ipcResponse is the JSON control message sent by the daemon to the client.
type ipcResponse struct {
	Type      string          `json:"type"` // error, status_response, ok, ws_response, identity_response
	Message   string          `json:"message,omitempty"`
	Version   string          `json:"version,omitempty"`
	Instances []instanceInfo  `json:"instances,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"` // for ws_response: the server response payload

	// identity_response fields. Populated by handleIdentity from the
	// daemon's cache + process state. All omitempty so other response
	// types stay compact on the wire.
	Email         string           `json:"email,omitempty"`
	HumanUserID   string           `json:"human_user_id,omitempty"`
	Organizations []daemonOrgEntry `json:"organizations,omitempty"`
	HostID        string           `json:"host_id,omitempty"`
	Hostname      string           `json:"hostname,omitempty"`
	StartedAt     string           `json:"started_at,omitempty"`
	WSConnected   bool             `json:"ws_connected,omitempty"`
	ServerURL     string           `json:"server_url,omitempty"`
	AgentHomePath  string           `json:"agent_home_path,omitempty"`

	// harnesses_response: server-side harness names whose local binary the
	// daemon can resolve on PATH. Populated by handleHarnesses.
	Harnesses []string `json:"harnesses,omitempty"`

	// resource_invoke_response fields. ResourceStdout is the plugin's
	// stdout passthrough; ResourceExitCode is the plugin-reported
	// exit code (separate from CLI's process exit code).
	// ResourceErrCode carries the *PluginError code string on
	// failure paths (Type=="error"); empty on success.
	ResourceStdout   string `json:"resource_stdout,omitempty"`
	ResourceExitCode int    `json:"resource_exit_code,omitempty"`
	ResourceErrCode  string `json:"resource_err_code,omitempty"`

	// resource_refresh_response fields. EntityCount is how many
	// entities the snapshot pulled and persisted to the daemon-local
	// resource_entities table for this connection.
	EntityCount int `json:"entity_count,omitempty"`

	// secret_resolve_response: env-name → cleartext map. Cleartext on
	// the wire; same threat model as other secret IPC paths (local
	// unix socket, same user). The `hearth run` wrapper consumes
	// this and sets env on the child before exec.
	SecretCleartexts map[string]string `json:"secret_cleartexts,omitempty"`

	// harnesses_response: detected version + tested-set membership per
	// harness, keyed by server-side name. Empty/missing entry means the
	// probe hadn't run when the IPC came in (or the binary wasn't on
	// PATH). Used by `hh status` to render the VERSION column.
	HarnessVersions map[string]ipcHarnessVersion `json:"harness_versions,omitempty"`
}

// ipcHarnessVersion is the per-harness version metadata surfaced to
// `hh status`. Lives in the IPC layer rather than on the Harness
// interface because it's data, not behavior.
type ipcHarnessVersion struct {
	Installed string   `json:"installed,omitempty"`  // detected; "" if probe failed
	Minimum   string   `json:"minimum,omitempty"`    // adapter's MinimumVersion()
	Tested    []string `json:"tested,omitempty"`     // adapter's KnownTestedVersions()
}

type ipcWinsize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type instanceInfo struct {
	AIAgentInstanceID string `json:"ai_agent_instance_id"`
	Agent             string `json:"agent"`
	Project           string `json:"project"`
	Cwd               string `json:"cwd"`
	StartedAt         string `json:"started_at"`
}

// Daemon manages the lifecycle of agent instances and the IPC socket.
type Daemon struct {
	listener  net.Listener
	sockPath  string
	instances map[string]*AgentInstance
	mu        sync.RWMutex
	done      chan struct{}
	wg        sync.WaitGroup
	// agentWg tracks the per-instance monitoring goroutines spawned in
	// daemon_agent.go (the ones that block on runRelay and then report
	// pid_status). Shutdown drains this before closing the daemon WS so
	// terminal pid_status updates actually land on the server.
	agentWg sync.WaitGroup
	// singletonLock holds the exclusive flock on ~/.hearth/daemon.lock
	// that prevents a second `hearth daemon` from running. Kept open
	// for the daemon's lifetime; the kernel releases the lock when the
	// process exits. See daemon_singleton.go.
	singletonLock *os.File
	daemonWS      *DaemonWS // multiplexed WebSocket for all instances
	hostID        string    // daemon's registered host ID
	hostSecret  string    // daemon's bearer token for /ws/daemon
	humanUserID string    // resolved human_user ID (for logs and kept-for-compat IPC)
	startedAt   time.Time // process start; reported to `hearth status`

	// Identity cache populated by server pushes on /ws/daemon. Served to
	// `hearth status` via the "identity" IPC request so the CLI doesn't
	// have to round-trip the server for every status invocation.
	identityMu   sync.RWMutex
	email        string
	orgs         []daemonOrgEntry
	agentHomePath string // server-pushed; "" until first connect

	// plugins is the in-memory set of resource-plugin manifests
	// discovered from ~/.hearth/plugins/ at daemon boot. Populated by
	// runDaemonForeground via plugins.Load(); read by future IPC and
	// agent-spawn code (sub-phases 1b–1f). Nil-safe: never-set
	// daemons (e.g. some test paths) won't crash callers that
	// `if d.plugins != nil` first.
	plugins *PluginRegistry

	// pluginsDir is the on-disk root the plugin registry scans
	// (~/.hearth/plugins, or HEARTH_PLUGINS_DIR override). Cached so
	// the plugin install/uninstall IPC handlers can mutate disk +
	// re-trigger plugins.Load() without re-deriving the path.
	pluginsDir string

	// resourceConnections is the in-memory Resource Connection store fed by
	// fetchResourceConnectionsAtBoot at boot + reconnect + live-push
	// (the server's resource_connections table is the SOT). Nil-safe
	// like plugins.
	resourceConnections *ResourceConnectionStore

	// agentGrants caches the (agent_id → granted connection_ids)
	// view from the server. Fed by fetchAgentResourceGrantsAtBoot at
	// boot + reconnect + agent_resource_grants_changed live-push.
	// Consulted by buildResourcePluginPrompt to filter the per-agent
	// prompt to connections the agent has explicit grants for.
	agentGrants *AgentGrantsStore

	// pluginSupervisor manages the live plugin subprocesses, one per
	// active Resource Connection. Lazy launch on first Invoke, crash
	// respawn with backoff, ShutdownAll wired into Daemon.Shutdown.
	// Nil-safe: callers must `if d.pluginSupervisor != nil` first
	// (ShutdownAll itself is nil-safe).
	pluginSupervisor *PluginSupervisor

	// declarativeExecutor runs schema-v2 declarative verbs (manifest
	// http: blocks) in-daemon — no subprocess. Reachable from
	// handleResourceInvoke when the manifest's Source is
	// SourceDeclarative; binary manifests still route through
	// pluginSupervisor. Stateless beyond its http.Client.
	declarativeExecutor *DeclarativeExecutor

	// agentIdentities indexes spawned agent processes by PID for the
	// peer-cred-based identity resolver. Populated by spawnAgentInstance,
	// pruned when the relay goroutine reports the process ended. Phase 0
	// of docs/agent-identity-plan.md — used only for telemetry today;
	// becomes authoritative once Phase 1 lands.
	agentIdentities   map[int]agentIdentityRecord
	agentIdentitiesMu sync.RWMutex

	// localDB is the daemon's local sqlite handle for Tier 2 plugin
	// state (~/.hearth/daemon.db, see daemon_db.go). Phase 3 step 5.
	// Nil in test setups that don't open it; state RPC handlers check.
	localDB *DaemonDB

	// resourceAuthzWS is the daemon→server transport for the 1e
	// authorize_resource_invoke preflight. Defaults to d.daemonWS at
	// boot; left as a separate field so tests can inject a stub
	// without standing up a real DaemonWS. 1g removes this entirely
	// when evaluation moves daemon-local.
	resourceAuthzWS authzWS

	// secretsPrivKey is the daemon's X25519 private key, loaded or
	// generated at boot. The matching pubkey is uploaded to the
	// server (hosts.public_key) so phones can encrypt secrets to
	// this host. Held in memory for the daemon's lifetime; persists
	// at ~/.hearth/key. See secrets_crypto.go.
	secretsPrivKey *ecdh.PrivateKey
}

// daemonOrgEntry mirrors the server's MyOrganization wire shape so we
// can re-marshal it back to status callers without a second translation.
type daemonOrgEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Role      string `json:"role"`
	JoinedAt  string `json:"joined_at"`
	IsCurrent bool   `json:"is_current"`
	IsPro     bool   `json:"is_pro"`
	ProSource string `json:"pro_source,omitempty"`
}

// SetAccount caches the user's email + human_user_id. Called from
// DaemonWS when an "account" push arrives from the server.
func (d *Daemon) SetAccount(humanUserID, email string) {
	d.identityMu.Lock()
	if humanUserID != "" {
		d.humanUserID = humanUserID
	}
	d.email = email
	d.identityMu.Unlock()
}

// SetOrganizations caches the org list. Called from DaemonWS when an
// "organizations_list" push arrives.
func (d *Daemon) SetOrganizations(orgs []daemonOrgEntry) {
	d.identityMu.Lock()
	d.orgs = orgs
	d.identityMu.Unlock()
}

// SetAgentHomePath caches the host's server-side agent_home_path. Called
// from DaemonWS when an "agent_home_path" push arrives on connect or
// after the value is updated. Empty string is a valid push and means
// "no value set yet"; consumers fall back to a local default for
// display purposes.
func (d *Daemon) SetAgentHomePath(dir string) {
	d.identityMu.Lock()
	d.agentHomePath = dir
	d.identityMu.Unlock()
}

// runStart, runStop, runStatus manage the local daemon. Previously nested
// under `hearth host`; promoted to top-level so the common lifecycle
// verbs (login/start/stop/status/logout) live side-by-side.
func runStart(args []string) {
	foreground := false
	var deviceIDFlag string
	for i, a := range args {
		if a == "--foreground" {
			foreground = true
		}
		if a == "--device-id" && i+1 < len(args) {
			deviceIDFlag = args[i+1]
		}
	}
	if foreground {
		runDaemonForeground()
		return
	}
	if isDaemonRunning() {
		fmt.Fprintf(os.Stderr, "hearth: host is already started\n")
		return
	}
	if err := ensureDaemon(deviceIDFlag); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "hearth: host started")
}

func runStop(args []string) { stopDaemon() }

func runStatus(args []string) { daemonStatus() }


// hostList prints the caller's enrolled hosts via the daemon's WS. Auto-starts
// the daemon if it isn't already running so the user doesn't have to run a
// separate command first.
func hostList() {
	if !isDaemonRunning() {
		if err := ensureDaemon(""); err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
	}
	data, err := sendWSRequest("list_hosts", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	var resp struct {
		Hosts []struct {
			HostID     string `json:"host_id"`
			Hostname   string `json:"hostname"`
			LastSeenAt string `json:"last_seen_at"`
		} `json:"hosts"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: parse list_hosts: %v\n", err)
		os.Exit(1)
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "hearth: list_hosts: %s\n", resp.Error)
		os.Exit(1)
	}
	if len(resp.Hosts) == 0 {
		fmt.Println("(no hosts enrolled)")
		return
	}
	thisHost := readConfigValue("host_id")
	for _, h := range resp.Hosts {
		name := h.Hostname
		if name == "" {
			name = "(unnamed)"
		}
		marker := ""
		if h.HostID == thisHost {
			marker = " *"
		}
		seen := h.LastSeenAt
		if seen == "" {
			seen = "never"
		}
		fmt.Printf("%s  %s  last seen: %s%s\n", h.HostID, name, seen, marker)
	}
}

// daemonSockPath returns the path to the daemon's Unix socket.
// Per-uid by default (`/tmp/hearth-daemon-<uid>.sock`) so two users
// on the same host get distinct sockets — required for multi-user
// isolation. The socket itself is chmod'd to 0600 at creation time
// in runDaemonForeground so the OS enforces the boundary even though
// /tmp is world-readable.
//
// Uses /tmp (not ~/.hearth) to keep the path short — Unix sockets
// are limited to ~104 bytes on macOS. Long home directories would
// otherwise blow the limit.
//
// HEARTH_DAEMON_SOCK overrides for tests + advanced cases.
func daemonSockPath() string {
	if p := os.Getenv("HEARTH_DAEMON_SOCK"); p != "" {
		return p
	}
	return fmt.Sprintf("/tmp/hearth-daemon-%d.sock", os.Getuid())
}

// daemonPidPath returns the path to the daemon's PID file.
func daemonPidPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/hearth-daemon.pid"
	}
	return filepath.Join(home, ".hearth", "daemon.pid")
}

// isDaemonRunning checks if a daemon is already running by probing the socket.
func isDaemonRunning() bool {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// startDaemonBackground forks the daemon as a background process.
func startDaemonBackground() {
	if isDaemonRunning() {
		fmt.Fprintf(os.Stderr, "hearth: host is already started\n")
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: cannot resolve executable: %v\n", err)
		os.Exit(1)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	cmd := exec.Command(exePath, "start", "--foreground")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: failed to start host: %v\n", err)
		os.Exit(1)
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()

	// Wait for the socket to become available
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if isDaemonRunning() {
			fmt.Fprintf(os.Stderr, "hearth: host started (pid %d)\n", pid)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "hearth: host started but socket not ready\n")
}

// ensureDaemon starts the daemon if it's not already running.
// Returns an error if the daemon cannot be started.
func ensureDaemon(deviceIDFlag string) error {
	if isDaemonRunning() {
		return nil
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	// Resolve device ID and host ID, registering the host if this is the
	// first run on this machine.
	var hostID, deviceID string
	if wsURL != "" {
		// Resolve device ID: flag > env > config (same priority as connect)
		deviceID = deviceIDFlag
		if deviceID == "" {
			deviceID = os.Getenv("HEARTH_DEVICE_ID")
		}
		if deviceID == "" {
			deviceID = readConfigValue("io_device_id")
		}

		// Host enrollment is gated on an interactive email-OTP flow.
		// If creds are missing, drop straight into runRegister (which
		// prompts for email itself) so the user doesn't have to re-issue
		// a separate `hearth login` command.
		hostID = readConfigValue("host_id")
		if hostID == "" || readConfigValue("io_device_id") == "" || readConfigValue("io_device_secret") == "" {
			fmt.Fprintln(os.Stderr, "You're not logged in. Let's get you signed in first.")
			// runRegister exits the process on any error, so if it
			// returns we have fresh creds in config.
			runRegister(nil)
			hostID = readConfigValue("host_id")
			deviceID = readConfigValue("io_device_id")
		}
	}

	cmd := exec.Command(exePath, "start", "--foreground")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Pass host ID and device ID to the daemon via env
	if hostID != "" {
		cmd.Env = append(os.Environ(),
			"HEARTH_DAEMON_HOST_ID="+hostID,
			"HEARTH_DEVICE_ID="+deviceID,
		)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}
	cmd.Process.Release()

	// Wait for the socket to become available
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if isDaemonRunning() {
			return nil
		}
	}
	return fmt.Errorf("daemon started but socket not ready")
}

// runDaemonForeground runs the daemon in the foreground (used by background start).
func runDaemonForeground() {
	// Set up logging
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, ".hearth")
	os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "daemon.log")
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		log.SetOutput(f)
	}

	// Refuse to start if another daemon is already running on this
	// host. Two daemons sharing ~/.hearth/host_id flap each other off
	// the server's WS in a tight reconnect loop — silent breakage
	// that's only obvious in server logs. Acquire BEFORE opening the
	// socket so the user gets the clearer message ("daemon already
	// running, PID N") instead of a generic bind error.
	lock, err := acquireDaemonSingletonLock()
	if err != nil {
		log.Printf("daemon: %v", err)
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}

	sockPath := daemonSockPath()

	// Ensure parent directory exists
	os.MkdirAll(filepath.Dir(sockPath), 0755)

	// Remove stale socket
	os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Printf("daemon: failed to listen on %s: %v", sockPath, err)
		fmt.Fprintf(os.Stderr, "hearth: failed to listen on %s: %v\n", sockPath, err)
		os.Exit(1)
	}
	// Lock down to owner only. The OS-level rail backing the per-uid
	// socket path — another user on the box can see the socket inode
	// in /tmp but can't connect(). Belt-and-suspenders alongside the
	// per-uid naming.
	if err := os.Chmod(sockPath, 0600); err != nil {
		log.Printf("daemon: chmod %s 0600 failed: %v", sockPath, err)
	}

	d := &Daemon{
		listener:      listener,
		sockPath:      sockPath,
		instances:     make(map[string]*AgentInstance),
		done:          make(chan struct{}),
		startedAt:     time.Now(),
		singletonLock: lock,
	}

	// Write PID file
	pidPath := daemonPidPath()
	os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0644)
	defer os.Remove(pidPath)

	log.Printf("daemon: started (pid %d, socket %s)", os.Getpid(), sockPath)

	// Discover resource plugins. Local concern (reads disk only); not
	// gated on server-WS like ProbeAllHarnessVersions is. A missing
	// plugins dir is fine; any manifest present in the dir must be
	// valid — Load returns a hard error on malformed or invalid
	// manifests. Override path via HEARTH_PLUGINS_DIR for tests + dev
	// (e.g. symlink a repo's plugin dir into a fresh tempdir).
	d.plugins = NewPluginRegistry()
	d.pluginsDir = os.Getenv("HEARTH_PLUGINS_DIR")
	if d.pluginsDir == "" {
		d.pluginsDir = filepath.Join(home, ".hearth", "plugins")
	}
	if err := d.plugins.Load(d.pluginsDir); err != nil {
		log.Fatalf("daemon: plugin load error: %v", err)
	}

	// Connection store starts empty. fetchResourceConnectionsAtBoot
	// (kicked further down after d.daemonWS is wired) fills the
	// store from the server on boot + reconnect + live-push.
	d.resourceConnections = NewResourceConnectionStore()
	// Agent grants store starts empty too; fetchAgentResourceGrantsAtBoot
	// fills it on the same trigger points.
	d.agentGrants = NewAgentGrantsStore()
	if resourceAuthzBypass() {
		log.Printf("daemon: WARNING HEARTH_RESOURCE_AUTHZ_BYPASS is set; plugin invokes will skip the IAM authorize step (not for production)")
	}
	// Open the daemon's local sqlite for plugin Tier 2 state. Failures
	// are non-fatal — the daemon continues without plugin_state support
	// and State* RPCs from plugins error out cleanly. See
	// docs/resource-plugins-3-plan.md §3.5.
	if ldb, err := OpenDaemonDB(home); err != nil {
		log.Printf("daemon: local DB unavailable (%v) — plugin state RPCs will error", err)
	} else {
		d.localDB = ldb
	}
	d.pluginSupervisor = NewPluginSupervisor(d.plugins, d.resourceConnections, d.localDB)
	d.declarativeExecutor = NewDeclarativeExecutor()
	d.declarativeExecutor.SetOAuthExchanger(d)

	// X25519 keypair for the 1d secrets vault. Load if persisted at
	// ~/.hearth/key; otherwise generate + persist. A regenerated
	// key orphans every existing ciphertext (the matching private
	// half is gone), so saveSecretsPrivateKey refuses overwrite —
	// load-or-generate is one-shot per host.
	if priv, err := loadOrGenerateSecretsKey(); err != nil {
		log.Fatalf("secrets keystore: %v", err)
	} else {
		d.secretsPrivKey = priv
		log.Printf("daemon: secrets keypair ready (pubkey fingerprint %s)",
			secretsPubFingerprint(priv))
	}

	// d.resourceAuthzWS + the rule-seed goroutine BOTH need the daemon
	// WS to exist before they're useful. The daemonWS gets constructed
	// further down (around `d.daemonWS = NewDaemonWS(...)`); wiring +
	// kicking the seed happen there.

	// Resolve human_user ID and host ID (set by ensureDaemon after registration, or from config)
	d.humanUserID = readConfigValue("user_id")
	d.hostID = os.Getenv("HEARTH_DAEMON_HOST_ID")
	if d.hostID == "" && d.humanUserID != "" {
		d.hostID = readConfigValue("host_id")
	}
	d.hostSecret = readConfigValue("host_secret")
	if d.hostID != "" {
		log.Printf("daemon: using host %s", d.hostID)
	}

	// Start the multiplexed WebSocket — all instances share this connection
	// for transcript, permissions, and control messages.
	d.startDaemonWS()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("daemon: shutting down")
		d.Shutdown()
	}()

	d.Run()

	log.Printf("daemon: stopped")
}

// Run accepts connections and handles them until the daemon is shut down.
func (d *Daemon) Run() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			select {
			case <-d.done:
				d.wg.Wait()
				return
			default:
				log.Printf("daemon: accept error: %v", err)
				continue
			}
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.handleConn(conn)
		}()
	}
}

// Shutdown gracefully stops the daemon: record user intent to be offline,
// signal each running agent to wind down (SIGTERM → 10s grace → SIGKILL),
// wait for their pid_status reports to land on the server, then tear down
// the WebSocket and IPC listener. A crash or SIGKILL skips this entirely,
// leaving hosts.desired_status='connected' — which is the whole point of
// that field.
//
// Order matters and is load-bearing:
//   1. write desired_status='disconnected' (WS still open)
//   2. stop each instance, blocking until its child is reaped
//   3. wait on agentWg so the per-instance monitoring goroutines finish
//      their reportPIDStatus calls (WS still open)
//   4. close the WS
//   5. close the IPC listener / drop the unix socket
// Doing (4) before (2) or (3) would silently drop every terminal
// pid_status update and leave the server believing those agents are still
// running, pending a later host_disconnected reconciliation.
func (d *Daemon) Shutdown() {
	d.setHostDesiredStatus("disconnected")

	// Snapshot the current instance map so we don't hold the lock while
	// each Stop() blocks for up to agentStopGrace. The per-instance
	// monitoring goroutines take the same lock to delete themselves
	// from d.instances after runRelay returns; if we held it throughout
	// they'd deadlock behind this loop.
	d.mu.RLock()
	snapshot := make([]*AgentInstance, 0, len(d.instances))
	for _, s := range d.instances {
		snapshot = append(snapshot, s)
	}
	d.mu.RUnlock()

	for _, s := range snapshot {
		log.Printf("daemon: stopping agent instance %s", s.aiAgentInstanceID)
		s.Stop()
	}

	// Tear down plugin subprocesses. Ordered after instance Stop so a
	// hypothetical future agent-side audit hook can still observe its
	// plugin during the stop path, but before agentWg.Wait so we don't
	// hold the WS open longer than needed. Safe on nil receiver.
	if err := d.pluginSupervisor.ShutdownAll(); err != nil {
		log.Printf("daemon: plugin shutdown: %v", err)
	}

	// Let the monitoring goroutines flush their pid_status reports over
	// the still-open WS before we close it.
	d.agentWg.Wait()

	if d.daemonWS != nil {
		d.daemonWS.Close()
	}

	close(d.done)
	d.listener.Close()
	os.Remove(d.sockPath)
}

// handleUpdateShutdown handles the update_shutdown IPC request.
// If there are active instances and force is false, it reports them back.
// Otherwise it shuts down.
func (d *Daemon) handleUpdateShutdown(conn net.Conn, req ipcRequest) {
	d.mu.RLock()
	count := len(d.instances)
	d.mu.RUnlock()

	if count > 0 && !req.Force {
		// Report active instances so the update command can prompt the user
		d.mu.RLock()
		var instances []instanceInfo
		for _, s := range d.instances {
			instances = append(instances, instanceInfo{
				AIAgentInstanceID: s.aiAgentInstanceID,
				Agent:             s.agent,
				Project:           s.project,
				Cwd:               s.cwd,
				StartedAt:         s.startedAt.Format(time.RFC3339),
			})
		}
		d.mu.RUnlock()
		sendControl(conn, ipcResponse{Type: "active_instances", Instances: instances})
		return
	}

	sendControl(conn, ipcResponse{Type: "ok"})
	go d.Shutdown()
}

// buildDaemonWSURL constructs the /ws/daemon dial URL with the current
// host_id, hostname, version handshake, and io_device_id from creds.
// Extracted from startDaemonWS so the reload_credentials IPC can rebuild
// the URL on the fly when host_id changes between logout/login.
func (d *Daemon) buildDaemonWSURL() (string, error) {
	if wsURL == "" {
		return "", fmt.Errorf("no relay URL configured")
	}
	dialURL, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("bad relay URL: %w", err)
	}
	dialURL.Path = strings.TrimSuffix(dialURL.Path, "/relay") + "/daemon"
	q := dialURL.Query()
	q.Set("host_id", d.hostID)
	if hostname, err := os.Hostname(); err == nil {
		q.Set("hostname", hostname)
	}
	if version != "" {
		q.Set("version", version)
	}
	// Forward-compat version handshake: server gates the upgrade on
	// these. See client_header.go and
	// hearth-cmd/docs/forward-compat-version-handshake.md.
	addClientQuery(q)
	// Pass the daemon's io_device_id so server-side pushes that depend on
	// "current org" (organizations_list, …) can compute IsCurrent the same
	// way they do for the phone /ws path. Optional — the server falls
	// back to "" (no current marker) when this isn't supplied.
	if devID := readConfigValue("io_device_id"); devID != "" {
		q.Set("io_device_id", devID)
	}
	dialURL.RawQuery = q.Encode()
	return dialURL.String(), nil
}

// handleReloadCredentials re-reads the credentials file and, if anything
// changed, swaps the daemon's auth on the live WebSocket. Sent by the
// CLI from `hearth login` (so a daemon that systemd has kept alive
// across the logout/login round-trip picks up the new identity) and
// from `hearth logout` (so the daemon drops its WS instead of looping
// reconnects against a revoked io_device).
//
// Behavior matrix:
//
//   - creds unchanged → no-op.
//   - creds cleared (logout side): close WS, leave daemon process
//     running. systemd-managed daemons stay alive; the next reload
//     (post-login) will reopen the WS.
//   - creds populated and changed: swap d.hostID/d.hostSecret, rebuild
//     the dial URL, hand it to DaemonWS.UpdateAuth. The reconnect loop
//     in ws.Run picks up the new auth on its next dial; agent-instance
//     registrations on DaemonWS.instances survive the swap and get
//     re-registered via the existing reconnectFunc.
//   - creds populated and no live WS (e.g. earlier reload cleared it):
//     start a fresh WS via startDaemonWS.
func (d *Daemon) handleReloadCredentials(conn net.Conn) {
	newUserID := readConfigValue("user_id")
	newHostID := readConfigValue("host_id")
	newHostSecret := readConfigValue("host_secret")

	if newUserID == d.humanUserID && newHostID == d.hostID && newHostSecret == d.hostSecret {
		sendControl(conn, ipcResponse{Type: "ok", Message: "credentials unchanged"})
		return
	}

	log.Printf("daemon: reload_credentials — host %q→%q, user %q→%q",
		d.hostID, newHostID, d.humanUserID, newUserID)

	d.humanUserID = newUserID
	d.hostID = newHostID
	d.hostSecret = newHostSecret

	// Cleared creds: drop the WS but keep the process alive. systemd
	// will not restart us, since the process is still up.
	if newHostID == "" || newHostSecret == "" {
		if d.daemonWS != nil {
			d.daemonWS.Close()
			d.daemonWS = nil
		}
		sendControl(conn, ipcResponse{Type: "ok", Message: "credentials cleared, WebSocket dropped"})
		return
	}

	newURL, err := d.buildDaemonWSURL()
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: err.Error()})
		return
	}

	if d.daemonWS != nil {
		d.daemonWS.UpdateAuth(newURL, d.hostSecret)
		sendControl(conn, ipcResponse{Type: "ok", Message: "credentials reloaded, WebSocket reconnecting"})
		return
	}

	// No existing WS — first-time spin-up after a cleared-creds reload.
	// startDaemonWS reads d.hostID/d.hostSecret directly, so we can
	// invoke it as-is.
	go d.startDaemonWS()
	sendControl(conn, ipcResponse{Type: "ok", Message: "credentials loaded, WebSocket starting"})
}

// startDaemonWS starts the multiplexed WebSocket connection. All instances
// share this single connection for transcript, permissions, and control messages.
// The connection is maintained with auto-reconnect for the lifetime of the daemon.
func (d *Daemon) startDaemonWS() {
	if wsURL == "" {
		log.Printf("daemon: no relay URL configured, WebSocket disabled")
		return
	}

	if d.hostID == "" {
		log.Printf("daemon: no host ID configured, WebSocket disabled")
		return
	}

	if d.hostSecret == "" {
		log.Printf("daemon: no host_secret configured, WebSocket disabled — re-run 'hearth login'")
		return
	}

	dialStr, err := d.buildDaemonWSURL()
	if err != nil {
		log.Printf("daemon: %v", err)
		return
	}

	d.daemonWS = NewDaemonWS(dialStr, d.hostSecret)
	d.daemonWS.sleepFunc = d.handleSleepAgentInstance
	d.daemonWS.wakeFunc = d.handleWakeAgentInstance
	d.daemonWS.cycleFunc = d.handleCycleAgentInstance
	d.daemonWS.accountFunc = d.SetAccount
	d.daemonWS.organizationsFunc = d.SetOrganizations
	d.daemonWS.agentHomePathFunc = d.SetAgentHomePath

	// resource-plugin substrate (1e/1f) needs daemonWS to be set
	// before it's useful. Wire the authz preflight transport here.
	// Default-rules seeding used to be kicked here as a per-connection
	// boot goroutine; that path was removed when explicit per-agent
	// grants (agent_resource_grant_create) replaced the automatic
	// seed-to-all-agents behavior.
	d.resourceAuthzWS = d.daemonWS
	// Restore the PID → agent_id registry for agents that survived a
	// daemon restart. Must run before any IPC connections land so that
	// hearth resource invokes from still-running agents are identified
	// correctly and don't fall back to the human principal.
	d.reRegisterExistingAgentPIDs()
	// 1d: upload the secrets pubkey to the server so phones (and
	// the daemon's own yaml-bootstrap) can encrypt to it. Same
	// "wait for WS up" pattern as the seed goroutine. Server
	// refuses different-pubkey overwrites, so re-runs are no-ops.
	go d.enrollHostPubkeyAtBoot()
	// 2a: push the discovered-plugin list to the server so the
	// org-wide registry (consumed by phase 2's connection
	// create-form picker) reflects this host. Re-report on every
	// reconnect (afterReconnectFunc below) so a server restart
	// or daemon WS flap doesn't leave the org's view stale. See
	// plugin_registry_report.go + docs/resource-plugins-2a-plan.md.
	go d.reportPluginInstallsAtBoot()
	// Phase 2: server is SOT for the connection list. Fetch fires
	// in parallel with the secrets-bootstrap goroutine above; the
	// bootstrap snapshots resourceConnections.List() before this swap
	// lands. Yaml-loaded credentials carry over by id so the
	// resolver's secret:false path keeps working until phase 2 UX
	// supersedes that. See resource_connections_fetch.go.
	go d.fetchResourceConnectionsAtBoot()
	go d.fetchAgentResourceGrantsAtBoot()
	d.daemonWS.afterReconnectFunc = func() {
		go d.reportPluginInstallsAtBoot()
		go d.fetchResourceConnectionsAtBoot()
		go d.fetchAgentResourceGrantsAtBoot()
		// Re-report running pid_status for any agents that survived the
		// disconnect. The server stamped them host_disconnected when the
		// WS dropped; without this they stay stale until re-spawned.
		d.mu.RLock()
		for id := range d.instances {
			go d.reportPIDStatus(id, "running")
		}
		d.mu.RUnlock()
	}
	// 2b live-push: server pings every in-org daemon on connection
	// create / delete; we refetch in response. waitForDaemonWS inside
	// fetchResourceConnectionsAtBoot is a no-op when the WS is up,
	// so it returns quickly under nominal conditions.
	d.daemonWS.resourceConnectionsChangedFunc = func() {
		log.Printf("daemon: resource_connections_changed received, refetching")
		go d.fetchResourceConnectionsAtBoot()
	}
	// Phase-4 live-push: server pings the agent's host on grant
	// create / delete; we refetch in response. Same shape as the
	// connection-change refetch.
	d.daemonWS.agentResourceGrantsChangedFunc = func() {
		log.Printf("daemon: agent_resource_grants_changed received, refetching")
		go d.fetchAgentResourceGrantsAtBoot()
	}

	go d.daemonWS.Run()

	// Wait for the WebSocket to connect before accepting IPC connections,
	// so the first agent_instance_connect message is sent over a live
	// connection rather than queued and drained later.
	for i := 0; i < 100; i++ {
		if d.daemonWS.IsConnected() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !d.daemonWS.IsConnected() {
		log.Printf("daemon: WARNING WebSocket not connected after 5s, proceeding anyway")
	}
	log.Printf("daemon: WebSocket started (human_user %s, host %s)", d.humanUserID, d.hostID)

	// Record the user's intent to be online. The column is strictly
	// intent-based: only graceful start/stop mutate it.
	d.setHostDesiredStatus("connected")

	// Probe each harness's installed version once, populating the
	// version cache. Logs an INFO line per detected version and a
	// WARN per "installed but not in KnownTestedVersions" — the
	// signal we want for catching silent version creep. Refusals
	// for below-MinimumVersion are enforced later at spawn time.
	ProbeAllHarnessVersions()

	// Probe which agent binaries this daemon can see on PATH and tell the
	// server. Reported set is used by the agent-create wizard so the
	// harness picker only shows harnesses this host can actually spawn.
	d.reportAvailableHarnesses()

	// Clean up any leftover /tmp markers from a prior daemon life, then
	// query the server for agents that should be running on this host
	// (status='active', not retired, position + WD still alive) and
	// spawn them. See docs/daemon-agent-lifecycle.md for the full contract.
	// Runs in a goroutine so a slow spawn chain doesn't delay IPC accept.
	go d.reconcileAndWakeAgents()
}

// setHostDesiredStatus sends hosts.desired_status to the server. Best-effort:
// if the WebSocket isn't connected, we skip rather than buffer. The caller's
// intent is recorded only when a live connection exists.
func (d *Daemon) setHostDesiredStatus(status string) {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() || d.hostID == "" {
		return
	}
	data, _ := json.Marshal(map[string]string{
		"host_id":        d.hostID,
		"desired_status": status,
	})
	resp, err := d.daemonWS.SendWSRequest(generateUUID(), "update_host_desired_status", data)
	if err != nil {
		log.Printf("daemon: update host desired_status=%s failed: %v", status, err)
		return
	}
	// The server returns {"error": "..."} for anything it rejects (unknown
	// message type, host scoping failures, etc.). Log so it isn't swallowed.
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(resp, &parsed) == nil && parsed.Error != "" {
		log.Printf("daemon: update host desired_status=%s rejected: %s", status, parsed.Error)
		return
	}
	log.Printf("daemon: update host desired_status=%s ok", status)
}

// harnessProbes maps each server-side harness name to the local binary we'd
// exec to spawn it. Harnesses without a local binary (e.g. windsurf,
// openai-assistants) are absent by construction.
// Keep this list in sync with agentBinary() in agent.go and
// localAgentForHarness() in daemon_agent.go — reporting a harness the
// spawn path doesn't recognize would give the user a dead-end option.
var harnessProbes = []struct{ Harness, Binary string }{
	{"claude-code", "claude"},
	{"codex", "codex"},
	{"gemini", "gemini"},
	{"copilot", "copilot"},
	{"pi", "pi"},
}

// probeAvailableHarnesses returns the server-side harness names whose local
// binary is on the daemon's own PATH. Deliberately uses the daemon's PATH
// (not a shell-sourced one) so the reported set is exactly what's reachable
// at spawn time — no false positives where we advertise `claude` but then
// fail to exec it.
func probeAvailableHarnesses() []string {
	available := []string{}
	for _, p := range harnessProbes {
		if _, err := exec.LookPath(p.Binary); err == nil {
			available = append(available, p.Harness)
		}
	}
	return available
}

// collectHarnessVersionsForIPC builds the per-harness version map for
// `hh status` consumption. Keyed by server-side name (same convention
// as the Harnesses list); pulls Installed from the version cache and
// Minimum/Tested from the registered adapter.
func collectHarnessVersionsForIPC() map[string]ipcHarnessVersion {
	out := map[string]ipcHarnessVersion{}
	for _, p := range harnessProbes {
		h, ok := getHarnessByServerName(p.Harness)
		if !ok {
			continue
		}
		out[p.Harness] = ipcHarnessVersion{
			Installed: getCachedVersion(h.Name()),
			Minimum:   h.MinimumVersion(),
			Tested:    h.KnownTestedVersions(),
		}
	}
	return out
}

// reportAvailableHarnesses probes PATH and tells the server which harnesses
// this daemon can actually spawn. Run once at connect time; the server
// overwrites the prior set for this host.
func (d *Daemon) reportAvailableHarnesses() {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() || d.hostID == "" {
		return
	}
	available := probeAvailableHarnesses()

	// Tell the server this host's home directory too. The iOS wizard uses
	// it to build absolute default working-directory paths instead of
	// suggesting "~/..." — the phone doesn't know the host's home, and
	// nothing server-side was expanding "~" before spawn.
	homeDir, _ := os.UserHomeDir()
	data, _ := json.Marshal(map[string]interface{}{
		"host_id":   d.hostID,
		"harnesses": available,
		"home_dir":  homeDir,
	})
	resp, err := d.daemonWS.SendWSRequest(generateUUID(), "report_host_harnesses", data)
	if err != nil {
		log.Printf("daemon: report_host_harnesses failed: %v", err)
		return
	}
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(resp, &parsed) == nil && parsed.Error != "" {
		log.Printf("daemon: report_host_harnesses rejected: %s", parsed.Error)
		return
	}
	log.Printf("daemon: reported %d available harnesses: %v", len(available), available)
}

// handleConn processes a single client connection.
func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()

	// Read control message (JSON, newline-delimited)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		log.Printf("daemon: read control message error: %v", err)
		return
	}
	conn.SetReadDeadline(time.Time{}) // clear deadline for long-lived I/O

	var req ipcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		log.Printf("daemon: invalid control message: %v", err)
		sendControl(conn, ipcResponse{Type: "error", Message: "invalid request"})
		return
	}

	switch req.Type {
	case "status":
		d.handleStatus(conn)
	case "identity":
		d.handleIdentity(conn)
	case "harnesses":
		sendControl(conn, ipcResponse{
			Type:            "harnesses_response",
			Harnesses:       probeAvailableHarnesses(),
			HarnessVersions: collectHarnessVersionsForIPC(),
		})
	case "stop":
		sendControl(conn, ipcResponse{Type: "ok"})
		// Run Shutdown synchronously so it completes under d.wg before the
		// daemon main loop exits. Otherwise Run() can return (listener closed,
		// done closed, wg empty because handleConn already returned) and the
		// process dies before Stop → killStreamer runs, orphaning streamers.
		d.Shutdown()
	case "reload_credentials":
		d.handleReloadCredentials(conn)
	case "update_shutdown":
		d.handleUpdateShutdown(conn, req)
	case "ws_request":
		d.handleWSRequest(conn, req)
	case "resource_invoke":
		d.handleResourceInvoke(conn, req)
	case "resource_refresh":
		d.handleResourceRefresh(conn, req)
	case "resource_list":
		d.handleResourceList(conn, req)
	case "secret_list":
		d.handleSecretList(conn, req)
	case "secret_set":
		d.handleSecretSet(conn, req)
	case "secret_delete":
		d.handleSecretDelete(conn, req)
	case "secret_grant":
		d.handleSecretGrant(conn, req)
	case "secret_revoke":
		d.handleSecretRevoke(conn, req)
	case "secret_resolve":
		d.handleSecretResolve(conn, req)
	case "plugin_list":
		d.handlePluginList(conn, req)
	case "plugin_install":
		d.handlePluginInstall(conn, req)
	case "plugin_uninstall":
		d.handlePluginUninstall(conn, req)
	case "approve_permission_request":
		d.handleApprovePermissionRequest(conn, req)
	case "chat_reply":
		d.handleChatReply(conn, req)
	default:
		sendControl(conn, ipcResponse{Type: "error", Message: "unknown request type"})
	}
}

// handleChatReply forwards an agent's chat reply to the server via ws_request.
// Called when `hearth chat reply` sends a chat_reply IPC message.
func (d *Daemon) handleChatReply(conn net.Conn, req ipcRequest) {
	if req.ChatRoomID == "" || req.ChatAgentInstanceID == "" || req.ChatText == "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "chat_room_id, chat_agent_instance_id, and text required"})
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"room_id":              req.ChatRoomID,
		"ai_agent_instance_id": req.ChatAgentInstanceID,
		"text":                 req.ChatText,
	})
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: err.Error()})
		return
	}
	innerReq := ipcRequest{
		Type:      "ws_request",
		WSMsgType: "send_chat_message",
		WSData:    json.RawMessage(payload),
	}
	d.handleWSRequest(conn, innerReq)
}

// resourceInvokeTimeout returns the per-call deadline for plugin
// invokes. HEARTH_RESOURCE_INVOKE_TIMEOUT, if set and parseable,
// wins; otherwise 30s.
func resourceInvokeTimeout() time.Duration {
	const fallback = 30 * time.Second
	raw := os.Getenv("HEARTH_RESOURCE_INVOKE_TIMEOUT")
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("daemon: HEARTH_RESOURCE_INVOKE_TIMEOUT %q is invalid; using %s", raw, fallback)
		return fallback
	}
	return d
}

// handleResourceInvoke dispatches a `hearth resource invoke` IPC to
// PluginSupervisor.Invoke and marshals the result back to the
// client. Local-only path: the supervisor talks to a subprocess on
// this host, not the server. IAM/authorize lands in 1e — for now
// every well-formed call succeeds modulo plugin- or transport-level
// failures. Per docs/resource-plugins-1c-plan.md commit 3.
func (d *Daemon) handleResourceInvoke(conn net.Conn, req ipcRequest) {
	if d.pluginSupervisor == nil && d.declarativeExecutor == nil {
		// Defensive: the production boot path wires at least one of
		// these. Bare-daemon test/scaffold paths fail closed with a
		// clear message rather than panicking downstream on nil plugin
		// registry / resource-connection-store derefs. Either backend
		// satisfies the guard; the actual per-branch nil-check happens
		// after manifest classification.
		sendControl(conn, ipcResponse{Type: "error", Message: "plugin supervisor not initialized"})
		return
	}
	if req.ResourceConnectionID == "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "missing resource_connection_id"})
		return
	}
	if req.ResourceVerb == "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "missing resource_verb"})
		return
	}

	// Resolve connection + manifest up front so the preflight authorize
	// call can pass plugin_slug to the server. These lookups happen
	// again inside PluginSupervisor.Invoke; duplicate work is fine for
	// the 1e stopgap (1g restructures the eval path and the duplication
	// goes away).
	// req.ResourceConnectionID is the slug typed by the user / agent.
	// Resolve it to the UUID-keyed entry so all downstream server calls
	// and local-DB keys use the stable UUID.
	rc, ok := d.resourceConnections.GetBySlug(req.ResourceConnectionID)
	if !ok {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         "unknown connection: " + req.ResourceConnectionID,
			ResourceErrCode: string(ErrBadArgs),
		})
		return
	}
	manifest, ok := d.plugins.GetPluginBySlug(rc.PluginSlug)
	if !ok {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         "plugin install not registered: " + rc.PluginSlug,
			ResourceErrCode: string(ErrUnavailable),
		})
		return
	}

	// Principal identity is derived from the IPC caller's process tree
	// (peer-cred → tree walk → agent registry); see
	// docs/agent-identity-plan.md. The CLI's claim is treated as a
	// hint, ignored when it disagrees with the tree, and refused
	// outright when it claims an agent that's not in the tree.
	principalKind, principalID, identityErr := d.derivePrincipal(conn,
		req.ResourcePrincipalKind, req.ResourcePrincipalID, "resource_invoke")
	if identityErr != nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         identityErr.Message,
			ResourceErrCode: string(identityErr.Code),
		})
		return
	}
	// Pass principalID as the agent instance ID hint when the principal
	// resolved as an agent — the server uses it to populate
	// ai_agent_instance_id in the WS frame for overlay routing. When the
	// principal is human (stale registry fallback), principalID is the
	// human user ID and the hint is empty; the environ-chain fallback in
	// resolveCallerAgent should prevent hitting this path in practice.
	agentHint := ""
	if principalKind == "agent" {
		agentHint = principalID
	}
	bindingID, pe := d.preflightAuthorizeResourceInvoke(d.resourceAuthzWS, principalKind, principalID,
		rc.ConnectionID, manifest.PluginSlug, req.ResourceVerb, req.ResourceArgs, agentHint)
	if pe != nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         pe.Message,
			ResourceErrCode: string(pe.Code),
		})
		return
	}

	// Resolve --secret bindings before launching the invoke. Each
	// secret_id goes through the server's Authorize() check; deny or
	// ask-timeout fails the whole invoke (no partial-cleartext
	// hand-off). Cleartext lives in the bindings map during the call
	// and gets zeroed after.
	secretCleartexts, secretErr := d.resolveSecretBindings(req.SecretBindings, principalKind, principalID)
	if secretErr != nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         secretErr.Message,
			ResourceErrCode: string(secretErr.Code),
		})
		return
	}
	defer zeroSecretMap(secretCleartexts)

	// Per-call deadline. HEARTH_RESOURCE_INVOKE_TIMEOUT overrides
	// the 30s default with any time.ParseDuration-compatible string;
	// malformed values log + fall back. Matches the DaemonWS
	// SendWSRequest default. ctx is rooted at Background rather than
	// the IPC conn — see plan §4 for the open question.
	ctx, cancel := context.WithTimeout(context.Background(), resourceInvokeTimeout())
	defer cancel()

	// Auto-refresh: declarative adapters with a snapshot block and an
	// empty entity cache get a one-shot snapshot inline using the same
	// credentials the caller provided. Best-effort — any failure logs
	// and the invoke proceeds (the verb itself doesn't depend on the
	// cache; only the NEXT invoke's predicate matching does). Authorize
	// already ran above and saw no entity context this time; subsequent
	// invokes benefit from cached kind/labels.
	if manifest.Source == SourceDeclarative && manifest.Snapshot != nil && d.localDB != nil {
		if existing, err := d.localDB.ListEntities(rc.ConnectionID); err == nil && len(existing) == 0 {
			d.autoRefreshEntitiesOnFirstInvoke(ctx, rc, manifest, secretCleartexts)
		} else if found, lastFetched, lerr := d.localDB.LatestEntityFetchedAt(rc.ConnectionID); lerr == nil && found {
			if age := time.Since(lastFetched); age > snapshotStaleAfter {
				log.Printf("daemon: entity snapshot for %s is stale (last refreshed %s ago); run `hearth resource refresh %s --secret ...`",
					rc.Slug, age.Round(time.Hour), rc.Slug)
			}
		}
	}

	var (
		result    InvokeResult
		invokeErr error
	)
	switch manifest.Source {
	case SourceDeclarative:
		// In-daemon HTTP execution. bindingID is unused for declarative
		// (no subprocess State* RPCs to scope), and secretCleartexts
		// becomes the executor's Credentials map.
		if d.declarativeExecutor == nil {
			invokeErr = &PluginError{Code: ErrInternal, Message: "declarative executor not initialized"}
		} else {
			result, invokeErr = d.invokeDeclarativeVerb(ctx, rc, manifest, req.ResourceVerb, req.ResourceArgs, secretCleartexts)
		}
	default:
		// SourceBinary (and the historic empty == binary): subprocess.
		if d.pluginSupervisor == nil {
			// Defensive — the production boot path wires this
			// unconditionally; some test scaffolds skip it. Returning
			// the standard PluginError shape rather than the bare
			// pre-router guard so the err code field is populated.
			invokeErr = &PluginError{Code: ErrInternal, Message: "plugin supervisor not initialized"}
		} else {
			result, invokeErr = d.pluginSupervisor.Invoke(ctx, rc.ConnectionID, req.ResourceVerb, req.ResourceArgs, secretCleartexts, bindingID)
		}
	}
	if err := invokeErr; err != nil {
		var code string
		msg := err.Error()
		if pe, ok := err.(*PluginError); ok {
			code = string(pe.Code)
			msg = pe.Message
		}
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         msg,
			ResourceErrCode: code,
		})
		return
	}
	sendControl(conn, ipcResponse{
		Type:             "resource_invoke_response",
		ResourceStdout:   result.Stdout,
		ResourceExitCode: result.ExitCode,
	})
}

// invokeDeclarativeVerb runs a single verb call through the in-daemon
// DeclarativeExecutor — no subprocess. Resolves the verb's http: block
// off the manifest, parses the connection's config blob, deserializes
// the caller's args, and hands everything to the executor. Errors all
// route back as *PluginError so the IPC-response code path stays
// uniform with the binary branch.
func (d *Daemon) invokeDeclarativeVerb(
	ctx context.Context,
	rc ResourceConnection,
	manifest PluginManifest,
	verb string,
	rawArgs json.RawMessage,
	credentials map[string]string,
) (InvokeResult, error) {
	if d.declarativeExecutor == nil {
		return InvokeResult{}, &PluginError{Code: ErrInternal, Message: "declarative executor not initialized"}
	}
	var spec *VerbHTTPSpec
	for i := range manifest.Verbs {
		if manifest.Verbs[i].Name == verb {
			spec = manifest.Verbs[i].HTTP
			break
		}
	}
	if spec == nil {
		return InvokeResult{}, &PluginError{
			Code:    ErrBadArgs,
			Message: fmt.Sprintf("verb %q has no http: block in declarative manifest %q", verb, manifest.PluginSlug),
		}
	}

	config := map[string]any{}
	if raw := strings.TrimSpace(rc.Config); raw != "" && raw != "{}" {
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			return InvokeResult{}, &PluginError{
				Code:    ErrInternal,
				Message: "connection config is not a JSON object: " + err.Error(),
			}
		}
	}

	args := map[string]any{}
	if len(rawArgs) > 0 {
		// json.Unmarshal into a fresh map silently ignores nulls; a
		// non-object payload (array, scalar) surfaces as bad_args.
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return InvokeResult{}, &PluginError{
				Code:    ErrBadArgs,
				Message: "verb args are not a JSON object: " + err.Error(),
			}
		}
	}

	// Expand typed credentials (e.g. service_account_json → access_token)
	// before handing the scope to the template engine.
	expandedCreds, expandErr := d.declarativeExecutor.expandCredentials(ctx, manifest.Credentials, credentials, config)
	if expandErr != nil {
		return InvokeResult{}, &PluginError{Code: ErrInternal, Message: "declarative invoke: expand credentials: " + expandErr.Error()}
	}

	// Explicit nil-check on the *PluginError before returning through
	// the (InvokeResult, error) signature. A typed-nil *PluginError
	// wrapped into an error interface reads as non-nil and (*PluginError)(nil).Error()
	// segfaults on the caller's `err.Error()` line.
	result, perr := d.declarativeExecutor.Invoke(ctx, DeclarativeInvokeInput{
		Spec:        spec,
		Config:      config,
		Credentials: expandedCreds,
		Args:        args,
	})
	if perr != nil {
		return result, perr
	}
	return result, nil
}

// autoRefreshEntitiesOnFirstInvoke runs the connection's snapshot once
// when the entity cache is empty, populating resource_entities for
// future IAM predicate matching. Best-effort: every failure logs and
// returns; the in-flight invoke proceeds regardless because its own
// authorize call already ran (with no entity context this time).
//
// Reuses the caller's resolved credentials — same map the verb is
// about to use — so this is "free" from an auth surface perspective.
// Manual `hearth resource refresh` still exists for explicit refresh;
// this just removes the requirement to run it before the first verb.
func (d *Daemon) autoRefreshEntitiesOnFirstInvoke(
	ctx context.Context,
	rc ResourceConnection,
	manifest PluginManifest,
	credentials map[string]string,
) {
	config := map[string]any{}
	if raw := strings.TrimSpace(rc.Config); raw != "" && raw != "{}" {
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			log.Printf("daemon: auto-refresh %s: connection config invalid: %v", rc.ConnectionID, err)
			return
		}
	}
	expandedCreds, expandErr := d.declarativeExecutor.expandCredentials(ctx, manifest.Credentials, credentials, config)
	if expandErr != nil {
		log.Printf("daemon: auto-refresh %s: credential expansion failed: %v", rc.ConnectionID, expandErr)
		return
	}
	entities, perr := d.declarativeExecutor.RunSnapshot(ctx, DeclarativeSnapshotInput{
		Spec:        manifest.Snapshot,
		Config:      config,
		Credentials: expandedCreds,
	})
	if perr != nil {
		log.Printf("daemon: auto-refresh %s: snapshot failed: %s (%s)", rc.ConnectionID, perr.Message, perr.Code)
		return
	}
	if err := d.localDB.ReplaceEntities(rc.ConnectionID, entities); err != nil {
		log.Printf("daemon: auto-refresh %s: persist failed: %v", rc.ConnectionID, err)
		return
	}
	log.Printf("daemon: auto-refreshed entities for %s on first invoke (%d entities)", rc.ConnectionID, len(entities))
}

// handleResourceRefresh runs the connection's declarative snapshot
// (manifest.snapshot block) and replaces the daemon-local entity
// cache. CLI-triggered via `hearth resource refresh <conn>`. No-op
// (success with EntityCount=0) for binary plugins — they're expected
// to emit entities through their own Onboard path, which isn't part
// of this surface.
//
// Authorization: not yet IAM-gated (entities are read-only metadata
// the daemon caches for its own evaluator). Snapshots may surface
// PII (entity names, room names); restrict by the same secret-binding
// path the caller would use for any verb.
func (d *Daemon) handleResourceRefresh(conn net.Conn, req ipcRequest) {
	if req.ResourceConnectionID == "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "missing resource_connection_id"})
		return
	}
	// req.ResourceConnectionID is the slug typed by the user / agent.
	rc, ok := d.resourceConnections.GetBySlug(req.ResourceConnectionID)
	if !ok {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         "unknown connection: " + req.ResourceConnectionID,
			ResourceErrCode: string(ErrBadArgs),
		})
		return
	}
	manifest, ok := d.plugins.GetPluginBySlug(rc.PluginSlug)
	if !ok {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         "plugin install not registered: " + rc.PluginSlug,
			ResourceErrCode: string(ErrUnavailable),
		})
		return
	}
	if manifest.Source != SourceDeclarative {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         "refresh only supported for declarative adapters; " + rc.PluginSlug + " is " + manifest.Source,
			ResourceErrCode: string(ErrBadArgs),
		})
		return
	}
	if manifest.Snapshot == nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         "declarative adapter " + rc.PluginSlug + " declares no snapshot block",
			ResourceErrCode: string(ErrBadArgs),
		})
		return
	}
	if d.localDB == nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         "daemon-local sqlite not available; entities cannot be persisted",
			ResourceErrCode: string(ErrUnavailable),
		})
		return
	}

	principalKind, principalID, identityErr := d.derivePrincipal(conn,
		req.ResourcePrincipalKind, req.ResourcePrincipalID, "resource_refresh")
	if identityErr != nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         identityErr.Message,
			ResourceErrCode: string(identityErr.Code),
		})
		return
	}
	secretCleartexts, secretErr := d.resolveSecretBindings(req.SecretBindings, principalKind, principalID)
	if secretErr != nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         secretErr.Message,
			ResourceErrCode: string(secretErr.Code),
		})
		return
	}
	defer zeroSecretMap(secretCleartexts)

	cfg := map[string]any{}
	if raw := strings.TrimSpace(rc.Config); raw != "" && raw != "{}" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			sendControl(conn, ipcResponse{
				Type:            "error",
				Message:         "connection config is not a JSON object: " + err.Error(),
				ResourceErrCode: string(ErrInternal),
			})
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), resourceInvokeTimeout())
	defer cancel()
	expandedCreds, expandErr := d.declarativeExecutor.expandCredentials(ctx, manifest.Credentials, secretCleartexts, cfg)
	if expandErr != nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         "expand credentials: " + expandErr.Error(),
			ResourceErrCode: string(ErrInternal),
		})
		return
	}
	entities, perr := d.declarativeExecutor.RunSnapshot(ctx, DeclarativeSnapshotInput{
		Spec:        manifest.Snapshot,
		Config:      cfg,
		Credentials: expandedCreds,
	})
	if perr != nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         perr.Message,
			ResourceErrCode: string(perr.Code),
		})
		return
	}
	if err := d.localDB.ReplaceEntities(rc.ConnectionID, entities); err != nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         "persist entities: " + err.Error(),
			ResourceErrCode: string(ErrInternal),
		})
		return
	}
	log.Printf("daemon: resource_refresh conn=%s slug=%s entities=%d",
		rc.ConnectionID, rc.PluginSlug, len(entities))
	sendControl(conn, ipcResponse{
		Type:        "resource_refresh_response",
		EntityCount: len(entities),
	})
}

// handleApprovePermissionRequest is the `hearth hh approve` IPC handler
// (approver-resolution phase 5b). Derives the calling principal (agent
// or human) via the same peer-cred + tree-walk pipeline the resource
// invoke path uses, then forwards the decision to the server as
// tool_approve_permission_request (agent path).
//
// Human-principal callers — operator at a terminal running
// `hearth hh approve` — get a "phase 5b is agent-only" error today.
// Operator-terminal approval routes through the webview's
// permission_response path; a future phase can wire a CLI parallel
// if the operator-from-terminal flow turns out to matter.
func (d *Daemon) handleApprovePermissionRequest(conn net.Conn, req ipcRequest) {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		sendControl(conn, ipcResponse{Type: "error", Message: "daemon WebSocket not connected"})
		return
	}
	if req.ApproveRequestID == "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "approve_request_id required"})
		return
	}
	if req.ApproveDecision != "allow" && req.ApproveDecision != "deny" {
		sendControl(conn, ipcResponse{Type: "error", Message: "approve_decision must be 'allow' or 'deny'"})
		return
	}

	principalKind, principalID, identityErr := d.derivePrincipal(conn, "", "", "approve_permission_request")
	if identityErr != nil {
		sendControl(conn, ipcResponse{
			Type:            "error",
			Message:         identityErr.Message,
			ResourceErrCode: string(identityErr.Code),
		})
		return
	}
	if principalKind != "agent" {
		sendControl(conn, ipcResponse{
			Type:    "error",
			Message: "hearth hh approve is agent-only today (phase 5b); operator approvals go through the webview",
		})
		return
	}

	payload := map[string]interface{}{
		"request_id":            req.ApproveRequestID,
		"ai_agent_instance_id":  principalID,
		"decision":              req.ApproveDecision,
		"reason":                req.ApproveReason,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "marshal payload: " + err.Error()})
		return
	}
	resp, err := d.daemonWS.SendWSRequest(generateUUID(), "tool_approve_permission_request", data)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: err.Error()})
		return
	}
	sendControl(conn, ipcResponse{Type: "ws_response", Data: json.RawMessage(resp)})
}

// handleWSRequest forwards a CRUD message to the server over the daemon WebSocket
// and returns the response to the IPC client.
func (d *Daemon) handleWSRequest(conn net.Conn, req ipcRequest) {
	if d.daemonWS == nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "daemon WebSocket not available"})
		return
	}
	if !d.daemonWS.IsConnected() {
		sendControl(conn, ipcResponse{Type: "error", Message: "daemon WebSocket not connected"})
		return
	}

	// create_ai_agent_instance gets special treatment: the daemon pre-checks
	// host locality, forwards the create, then spawns the agent process.
	if req.WSMsgType == "create_ai_agent_instance" {
		d.handleCreateAgentInstance(conn, req)
		return
	}

	// update_ai_agent_instance with a non-empty retired_at is the "stop"
	// path: terminate the local instance (if any) before forwarding the row
	// update. Instances from spawnAgentInstance are keyed by
	// ai_agent_instance_id in d.instances, so the lookup is direct.
	if req.WSMsgType == "update_ai_agent_instance" {
		var probe struct {
			ID        string `json:"id"`
			RetiredAt string `json:"retired_at"`
		}
		if json.Unmarshal(req.WSData, &probe) == nil && probe.RetiredAt != "" && probe.ID != "" {
			d.mu.Lock()
			s := d.instances[probe.ID]
			delete(d.instances, probe.ID)
			d.mu.Unlock()
			if s != nil {
				log.Printf("daemon: stopping spawned agent instance %s on retire", probe.ID)
				s.Stop()
			}
		}
	}

	correlationID := generateUUID()
	resp, err := d.daemonWS.SendWSRequest(correlationID, req.WSMsgType, req.WSData)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: err.Error()})
		return
	}
	sendControl(conn, ipcResponse{Type: "ws_response", Data: json.RawMessage(resp)})
}

// handleIdentity returns the daemon's cached account/org state along with
// this-host information used by `hearth status`. Cache is populated by
// /ws/daemon pushes; if the WebSocket is connected but the cache hasn't
// landed yet, block briefly so a fast `status` invocation right after
// startup doesn't see empty fields.
func (d *Daemon) handleIdentity(conn net.Conn) {
	if d.daemonWS != nil && d.daemonWS.IsConnected() {
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			d.identityMu.RLock()
			ready := d.email != "" || len(d.orgs) > 0
			d.identityMu.RUnlock()
			if ready {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	d.identityMu.RLock()
	email := d.email
	humanUserID := d.humanUserID
	orgs := append([]daemonOrgEntry(nil), d.orgs...)
	agentHome := d.agentHomePath
	d.identityMu.RUnlock()

	hostname, _ := os.Hostname()
	// Server is SOT for agent_home_path; if we haven't received the push
	// yet (very first connect, or WS still down), fall back to the local
	// default so `hearth status` can render something sensible.
	if agentHome == "" {
		agentHome = localAgentHomeBase()
	}
	wsConnected := d.daemonWS != nil && d.daemonWS.IsConnected()

	sendControl(conn, ipcResponse{
		Type:          "identity_response",
		Email:         email,
		HumanUserID:   humanUserID,
		Organizations: orgs,
		HostID:        d.hostID,
		Hostname:      hostname,
		StartedAt:     d.startedAt.Format(time.RFC3339),
		WSConnected:   wsConnected,
		ServerURL:     wsURL,
		AgentHomePath:  agentHome,
	})
}

// handleStatus sends instance information back to the client.
func (d *Daemon) handleStatus(conn net.Conn) {
	d.mu.RLock()
	var instances []instanceInfo
	for _, s := range d.instances {
		instances = append(instances, instanceInfo{
			AIAgentInstanceID: s.aiAgentInstanceID,
			Agent:             s.agent,
			Project:           s.project,
			Cwd:               s.cwd,
			StartedAt:         s.startedAt.Format(time.RFC3339),
		})
	}
	d.mu.RUnlock()

	sendControl(conn, ipcResponse{
		Type:      "status_response",
		Instances: instances,
	})
}

// sendControl writes a JSON control message followed by a newline.
func sendControl(conn net.Conn, resp ipcResponse) {
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	conn.Write(data)
}

// stopDaemon asks the running daemon to shut down. Any currently-spawned
// agent instances are gracefully terminated as part of the shutdown (see
// AgentInstance.Stop and docs/daemon-agent-lifecycle.md) — we no longer
// prompt the user about them. Their intent (status=active) is preserved,
// so the next `daemon start` will wake them back up.
func stopDaemon() {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: host is already stopped\n")
		return
	}
	defer conn.Close()

	msg, _ := json.Marshal(ipcRequest{Type: "stop"})
	msg = append(msg, '\n')
	conn.Write(msg)

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: host stopped\n")
		return
	}
	var resp ipcResponse
	json.Unmarshal(line, &resp)
	if resp.Type != "ok" {
		fmt.Fprintf(os.Stderr, "hearth: %s\n", resp.Message)
		return
	}

	// The daemon writes "ok" before its listener actually unbinds, so a
	// rapid follow-up `start` would otherwise see the socket still
	// accepting connections and report "already running". Poll until the
	// socket is genuinely gone before declaring success. Budget has to
	// cover Shutdown's worst case: per-agent SIGTERM grace (agentStopGrace,
	// 10s) iterated serially across running instances, plus WS round-trips.
	deadline := time.Now().Add(60 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		if !isDaemonRunning() {
			stopped = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !stopped {
		fmt.Fprintf(os.Stderr, "hearth: stop requested but host is still shutting down (agents may be draining); re-run `hearth status` in a few seconds\n")
		return
	}
	fmt.Fprintf(os.Stderr, "hearth: host stopped\n")
}

// reloadDaemonCredentials sends the daemon a reload_credentials IPC
// so it picks up freshly-written credentials (or notices they've been
// wiped) without restarting the process. Best-effort: a stopped or
// unreachable daemon is fine — the next start reads creds from disk
// anyway. No-op if the socket isn't listening.
//
// Used by `hearth login` (after writing fresh creds) and `hearth
// logout` (after wiping them) so systemd-managed daemons that survive
// the logout/login round-trip pick up the new identity instead of
// looping reconnects against stale auth.
func reloadDaemonCredentials() {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		return // daemon not running; nothing to reload.
	}
	defer conn.Close()

	msg, _ := json.Marshal(ipcRequest{Type: "reload_credentials"})
	msg = append(msg, '\n')
	if _, err := conn.Write(msg); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: failed to signal daemon credential reload (%v); a daemon restart will pick them up\n", err)
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		// Best-effort — daemon may have closed the socket as part of
		// reloading. Not worth surfacing to the user.
		return
	}
	var resp ipcResponse
	_ = json.Unmarshal(line, &resp)
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth: daemon credential reload failed (%s); a daemon restart will pick them up\n", resp.Message)
	}
}

// daemonStatus is implemented in status.go.
