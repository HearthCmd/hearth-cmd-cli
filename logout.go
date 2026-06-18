//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

func runLogout(args []string) {
	force := false
	for _, a := range args {
		switch a {
		case "--force", "-f":
			force = true
		case "--help", "-h":
			fmt.Fprintf(os.Stderr, `Usage: hearth logout [--force]

Signs out of Hearth on this machine. Stops the host daemon, revokes
this machine's credentials on the server, and clears local config.

  --force  Proceed even if agents are currently running.
`)
			os.Exit(0)
		}
	}

	// Step 1: if the daemon is up, refuse to log out while agents are
	// still running unless --force.
	if isDaemonRunning() {
		instances, err := queryActiveInstances()
		if err == nil && len(instances) > 0 && !force {
			fmt.Fprintf(os.Stderr, "hearth: %d agent(s) still running:\n", len(instances))
			for _, s := range instances {
				id := s.AIAgentInstanceID
				if len(id) > 8 {
					id = id[:8]
				}
				fmt.Fprintf(os.Stderr, "  %s  %s  %s\n", id, s.Agent, s.Project)
			}
			fmt.Fprintf(os.Stderr, "Retire them first or re-run with --force.\n")
			os.Exit(1)
		}
		// Step 2: stop the daemon gracefully. stopDaemon also writes
		// desired_status=disconnected on the server.
		stopDaemon()
	}

	// Steps 3 + 4: self-revoke on the server (best-effort). If the
	// network's down or the server's unreachable, still wipe locally —
	// the alternative leaves the user stuck.
	ioDeviceID := readConfigValue("io_device_id")
	ioDeviceSecret := readConfigValue("io_device_secret")
	hostID := readConfigValue("host_id")
	if ioDeviceID != "" && ioDeviceSecret != "" {
		if baseURL, err := serverBaseURL(); err == nil {
			payload := map[string]string{}
			if hostID != "" {
				payload["host_id"] = hostID
			}
			if _, err := deviceAuthedPost(baseURL, "/logout", ioDeviceID, ioDeviceSecret,
				ActionTuple{Kind: "io_device", ID: ioDeviceID, Action: "logout"},
				payload); err != nil {
				fmt.Fprintf(os.Stderr, "hearth: server logout failed (%v); clearing local credentials anyway\n", err)
			}
		}
	}

	// Step 5: wipe credentials from ~/.hearth/credentials. host_id is
	// deliberately preserved so a subsequent same-user login on this
	// machine reclaims the same identity: the server leaves the host
	// row active (revoked_at IS NULL) but flips desired_status to
	// 'disconnected', so /auth/session/reclaimable-hosts will also
	// suggest it during login if the user happens to nuke their
	// credentials file. A different user logging in here lands on
	// the existing row's "different owner — refuse" branch and falls
	// through to a fresh-host enroll. host_secret was just rotated
	// out server-side and would be useless to keep; the new login
	// mints a fresh one. Leave the file in place — non-credential
	// keys (if any) stay intact.
	for _, k := range []string{"user_id", "host_secret", "io_device_id", "io_device_secret", "email"} {
		_ = writeConfigValue(k, "")
	}

	// Under systemd (or anything else that respawns the daemon after
	// stopDaemon exits in step 2), a fresh daemon process may already
	// have come back up reading the now-wiped credentials file. Nudge
	// it: handleReloadCredentials sees cleared creds and drops the WS,
	// avoiding a reconnect loop against a revoked io_device until the
	// user logs back in. No-op when no daemon is listening.
	reloadDaemonCredentials()

	// Step 6: best-effort cleanup of ephemeral runtime state. The
	// daemon's graceful shutdown should already have removed most of
	// these; belt-and-suspenders for the crashed-daemon case.
	cleanupTmpArtifacts()

	// Step 7.
	fmt.Fprintf(os.Stderr, "Logged out. Run 'hearth login <email>' to sign back in.\n")
}

// queryActiveInstances asks the running daemon for its active agent
// instances via the IPC status message.
func queryActiveInstances() ([]instanceInfo, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	msg, _ := json.Marshal(ipcRequest{Type: "status"})
	msg = append(msg, '\n')
	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	return resp.Instances, nil
}

// cleanupTmpArtifacts removes /tmp files the daemon and spawned agents
// leave behind. Silently ignores anything that isn't there.
func cleanupTmpArtifacts() {
	paths := []string{daemonSockPath()}
	globs := []string{
		"/tmp/hearth-bridge-*",
		"/tmp/hearth-stream-*.pid",
		"/tmp/hearth-interpose-*.log",
		"/tmp/.gl-*",
		"/tmp/gl-*.sock",
	}
	for _, g := range globs {
		matches, _ := filepath.Glob(g)
		paths = append(paths, matches...)
	}
	for _, p := range paths {
		_ = os.Remove(p)
	}
}
