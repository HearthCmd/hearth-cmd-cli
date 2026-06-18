//go:build linux

package main

import (
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// peerPID returns the connecting process's PID for a Unix-domain
// connection via SO_PEERCRED. Returns (0, false) if conn isn't a
// *net.UnixConn or the syscall fails — caller treats that as
// "couldn't identify, fall through to env-var-claim handling."
func peerPID(conn net.Conn) (int, bool) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var ucred *syscall.Ucred
	var sysErr error
	cerr := raw.Control(func(fd uintptr) {
		ucred, sysErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if cerr != nil || sysErr != nil || ucred == nil {
		return 0, false
	}
	return int(ucred.Pid), true
}

// readStatPostComm reads /proc/<pid>/stat and returns the
// whitespace-split fields AFTER the comm field (field 2). The post-
// comm slice has field 3 (state) at index 0, field 4 (ppid) at
// index 1, ..., field 22 (starttime) at index 19. The function exists
// because comm is wrapped in parens and can itself contain
// spaces / parens that the kernel doesn't escape — splitting on the
// LAST `)` is the standard parse robustness trick.
func readStatPostComm(pid int) ([]string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil, false
	}
	s := string(data)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen >= len(s)-1 {
		return nil, false
	}
	return strings.Fields(s[rparen+1:]), true
}

// getParentPID reads /proc/<pid>/stat to extract the parent PID
// (field 4 in the stat line, 1-indexed). Returns (0, false) on any
// read or parse failure — caller ends the tree walk silently.
func getParentPID(pid int) (int, bool) {
	fields, ok := readStatPostComm(pid)
	if !ok || len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return 0, false
	}
	return ppid, true
}

// resolveAgentFromEnvironChain walks the process tree upward from pid,
// reading /proc/<n>/environ at each hop to check for
// HEARTH_AGENT_INSTANCE_ID. Returns (agentID, foundPID, true) on the
// first hit; ("", 0, false) if the walk reaches PID 1 / the hop cap
// without finding one.
//
// This is the fallback path for resolveCallerAgent when the in-memory
// PID registry is empty or stale (e.g. after a daemon restart). Reading
// from the kernel-attested /proc/<peerPID>/environ chain is more trusted
// than an IPC message claim: the peer PID comes from SO_PEERCRED, which
// the kernel sets and the caller can't forge. A same-UID adversary could
// still set HEARTH_AGENT_INSTANCE_ID in their own environment — the daemon
// does NOT auto-register environ-sourced identities into the permanent
// registry for this reason. The returned ID is accepted as best-effort
// for display routing; future registry entries overwrite it if a
// correctly-registered agent with the same ID reconnects.
func resolveAgentFromEnvironChain(startPID int) (agentID string, foundPID int, ok bool) {
	cur := startPID
	for i := 0; i < maxTreeWalkHopsLinux; i++ {
		if cur <= 1 {
			return "", 0, false
		}
		data, err := os.ReadFile("/proc/" + strconv.Itoa(cur) + "/environ")
		if err == nil {
			for _, kv := range strings.Split(string(data), "\x00") {
				if strings.HasPrefix(kv, "HEARTH_AGENT_INSTANCE_ID=") {
					id := strings.TrimPrefix(kv, "HEARTH_AGENT_INSTANCE_ID=")
					if id != "" {
						return id, cur, true
					}
				}
			}
		}
		ppid, pok := getParentPID(cur)
		if !pok || ppid == cur {
			return "", 0, false
		}
		cur = ppid
	}
	return "", 0, false
}

// maxTreeWalkHopsLinux is the per-request depth cap for the Linux
// environ-chain fallback walk (same rationale as maxTreeWalkHops in
// agent_identity.go).
const maxTreeWalkHopsLinux = 32

// scanProcForHearthAgents reads /proc/*/environ for all processes
// owned by the current user and returns a map of agentID → PID for
// every process that has HEARTH_AGENT_INSTANCE_ID set. Used at daemon
// startup to re-register agents that were running before the daemon
// restarted. Best-effort: unreadable environ entries are skipped.
func scanProcForHearthAgents() map[string]int {
	uid := os.Getuid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	result := make(map[string]int)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		// Only inspect processes owned by this user.
		info, err := e.Info()
		if err != nil {
			continue
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			if int(stat.Uid) != uid {
				continue
			}
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		// environ is NUL-delimited key=value pairs.
		for _, kv := range strings.Split(string(data), "\x00") {
			if strings.HasPrefix(kv, "HEARTH_AGENT_INSTANCE_ID=") {
				agentID := strings.TrimPrefix(kv, "HEARTH_AGENT_INSTANCE_ID=")
				if agentID != "" {
					result[agentID] = pid
				}
				break
			}
		}
	}
	return result
}

// processStartFingerprint returns an opaque string that uniquely
// identifies a particular instance of a PID across reuses. The string
// is the kernel-reported starttime (clock ticks since boot, field 22
// of /proc/<pid>/stat) — stable for the lifetime of the process,
// guaranteed to differ for any future process that reuses the same
// PID after a reboot or even within the same boot (the kernel
// increments the boot-tick counter monotonically). Returns ("", false)
// on any read/parse failure; caller treats that as "can't verify."
func processStartFingerprint(pid int) (string, bool) {
	fields, ok := readStatPostComm(pid)
	if !ok || len(fields) < 20 {
		return "", false
	}
	// Field 22 (1-indexed) = index 19 of the post-comm slice.
	return fields[19], true
}
