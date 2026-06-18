//go:build darwin

package main

import (
	"net"
	"strconv"

	"golang.org/x/sys/unix"
)

// peerPID returns the connecting process's PID for a Unix-domain
// connection via the macOS-specific LOCAL_PEERPID socket option.
// Returns (0, false) if conn isn't *net.UnixConn or the syscall
// fails — caller treats as "couldn't identify."
func peerPID(conn net.Conn) (int, bool) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var pid int
	var sysErr error
	cerr := raw.Control(func(fd uintptr) {
		v, gerr := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if gerr != nil {
			sysErr = gerr
			return
		}
		pid = v
	})
	if cerr != nil || sysErr != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// getParentPID returns the parent PID of pid via the kern.proc.pid
// sysctl, parsed through x/sys/unix's KinfoProc wrapper. Returns
// (0, false) on any failure — caller ends the tree walk silently.
func getParentPID(pid int) (int, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return 0, false
	}
	ppid := int(kp.Eproc.Ppid)
	if ppid < 0 {
		return 0, false
	}
	return ppid, true
}

// scanProcForHearthAgents is a no-op on macOS — there's no /proc
// filesystem. The PID-file path in reRegisterExistingAgentPIDs is the
// sole restart-recovery mechanism on darwin.
func scanProcForHearthAgents() map[string]int { return nil }

// resolveAgentFromEnvironChain is a no-op on macOS — there's no /proc
// filesystem to read per-process environments from.
func resolveAgentFromEnvironChain(_ int) (string, int, bool) { return "", 0, false }

// processStartFingerprint returns an opaque string that uniquely
// identifies a particular instance of a PID across reuses. On macOS
// it's the formatted Proc.P_starttime Timeval — wall-clock at
// process creation, at microsecond resolution. Stable for the process
// lifetime; a future process reusing the same PID gets a different
// start time. Returns ("", false) on sysctl failure.
func processStartFingerprint(pid int) (string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return "", false
	}
	ts := kp.Proc.P_starttime
	return strconv.FormatInt(int64(ts.Sec), 10) + "." + strconv.FormatInt(int64(ts.Usec), 10), true
}
