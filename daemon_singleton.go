//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// acquireDaemonSingletonLock takes an exclusive, non-blocking advisory
// lock on ~/.hearth/daemon.lock so a second `hearth daemon` foreground
// process can't run alongside an existing one. The two-daemon case
// produced silent breakage in dogfood: both processes read the same
// host_id, the server's daemon map keys on it, and the connections
// kicked each other off the WS in a tight reconnect loop.
//
// flock semantics:
//   - LOCK_EX | LOCK_NB: exclusive, fail fast if already held.
//   - The lock is released by the kernel when the holding process
//     exits, crashes, or is killed — no stale-file cleanup needed.
//   - The returned *os.File MUST be kept open for the daemon's
//     lifetime (closing it releases the lock). The caller stores
//     it on the Daemon struct.
//
// On lock contention the existing PID file is consulted to surface
// the running daemon's PID in the error message; if the PID file is
// missing or unreadable, the message just names the lockfile path.
func acquireDaemonSingletonLock() (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}
	lockDir := filepath.Join(home, ".hearth")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, "daemon.lock")

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lockfile %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		// EWOULDBLOCK / EAGAIN: another process holds the lock.
		other := readExistingDaemonPID()
		if other != "" {
			return nil, fmt.Errorf(
				"another hearth daemon is already running (PID %s, lock held on %s); refusing to start a second one",
				other, lockPath)
		}
		return nil, fmt.Errorf(
			"another hearth daemon is already running (lock held on %s); refusing to start a second one",
			lockPath)
	}
	return f, nil
}

// readExistingDaemonPID best-effort returns the PID written by the
// running daemon. Empty string on any failure — the caller surfaces
// a slightly less specific error in that case.
func readExistingDaemonPID() string {
	data, err := os.ReadFile(daemonPidPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
