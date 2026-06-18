//go:build darwin || linux

package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// agent_identity.go — Phase 0 of docs/agent-identity-plan.md.
//
// The current "I am agent X" claim rides on the forgeable env var
// HEARTH_AGENT_INSTANCE_ID. The recommended fix (see plan) is to drop
// the env-var rail entirely and derive the calling agent's identity
// from the IPC socket's peer credentials plus a walk up the process
// tree, comparing each ancestor PID against a daemon-maintained
// registry of spawned agents.
//
// Phase 0 lands the plumbing — peer-cred lookup, PID registry,
// resolveCallerAgent — and wires WARN-only telemetry on every IPC
// handler that consumes an agent principal. No behavior changes; the
// env-var-derived claim still drives authz. We're collecting evidence
// that the tree-walk correctly identifies agents across the harness
// quirks before Phase 1 makes it authoritative.

// agentIdentityRecord is one row in the daemon's PID-keyed agent
// registry.
//
// StartFingerprint is the kernel-attested process-start identifier
// captured at registration (Linux: starttime ticks from
// /proc/<pid>/stat field 22; macOS: Proc.P_starttime Timeval). When
// the resolver finds a registered PID it re-reads the fingerprint
// from the kernel and refuses the match if it doesn't match the
// stored value — that detects PID reuse, in which case the registry
// still has an entry for the (now-dead) agent but the live process
// at that PID is unrelated. Without the fingerprint check we'd
// accidentally identify the unrelated process as the old agent.
//
// SpawnTime is wall-clock at registration; kept around for
// human-readable logging ("agent X registered N minutes ago") and
// is NOT used for the PID-recycle check.
type agentIdentityRecord struct {
	AgentID          string
	PID              int
	StartFingerprint string
	SpawnTime        time.Time
}

// registerAgentIdentity records a freshly-spawned agent's PID in the
// daemon's PID → agent_id map and snapshots its kernel-attested
// start-time fingerprint for the PID-recycle defense (see
// agentIdentityRecord doc). Called from spawnAgentInstance after
// cmd.Process.Pid is available.
//
// If the fingerprint read fails (rare — /proc not mounted, process
// already dead) we still register with an empty StartFingerprint;
// the resolver's behavior is documented in lookupAgentByPID.
func (d *Daemon) registerAgentIdentity(agentID string, pid int) {
	if d == nil || pid <= 0 || agentID == "" {
		log.Printf("[agent-identity] registerAgentIdentity skipped: pid=%d agentID=%q", pid, agentID)
		return
	}
	fp, _ := processStartFingerprint(pid)
	d.agentIdentitiesMu.Lock()
	defer d.agentIdentitiesMu.Unlock()
	if d.agentIdentities == nil {
		d.agentIdentities = map[int]agentIdentityRecord{}
	}
	d.agentIdentities[pid] = agentIdentityRecord{
		AgentID:          agentID,
		PID:              pid,
		StartFingerprint: fp,
		SpawnTime:        time.Now(),
	}
	// Persist PID to disk so it survives daemon restarts (see
	// reRegisterExistingAgentPIDs). File is tiny; write errors are non-fatal.
	writeAgentPIDFile(agentID, pid, fp)
	log.Printf("[agent-identity] registered agent %s at pid=%d fp=%q", agentID, pid, fp)
}

// agentPIDFilePath returns the path of the per-agent PID file.
func agentPIDFilePath(agentID string) string {
	return filepath.Join(os.TempDir(), "hearth-agent-"+agentID+".pid")
}

// writeAgentPIDFile persists the agent's PID and start-fingerprint so
// the registry can be restored after a daemon restart.
func writeAgentPIDFile(agentID string, pid int, fp string) {
	_ = os.WriteFile(agentPIDFilePath(agentID), []byte(fmt.Sprintf("%d\n%s\n", pid, fp)), 0600)
}

// readAgentPIDFile reads back a previously persisted (pid, fingerprint)
// pair. Returns (0, "") if the file is absent or malformed.
func readAgentPIDFile(agentID string) (pid int, fp string) {
	data, err := os.ReadFile(agentPIDFilePath(agentID))
	if err != nil {
		return 0, ""
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(parts) < 1 {
		return 0, ""
	}
	pid, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || pid <= 0 {
		return 0, ""
	}
	if len(parts) == 2 {
		fp = strings.TrimSpace(parts[1])
	}
	return pid, fp
}

// removeAgentPIDFile deletes the persisted PID file when the agent exits.
func removeAgentPIDFile(agentID string) {
	_ = os.Remove(agentPIDFilePath(agentID))
}

// reRegisterExistingAgentPIDs restores the in-memory PID → agent_id
// registry from two sources after a daemon restart:
//
//  1. PID files written by previous daemon runs
//     (/tmp/hearth-agent-<agentID>.pid).
//  2. /proc environ scan (Linux only) — catches agents that were started
//     before the PID-file scheme existed, or whose PID files were lost.
//
// Both sources apply the same fingerprint check to reject stale entries
// from PID reuse.
func (d *Daemon) reRegisterExistingAgentPIDs() {
	// Source 1: PID files from previous runs.
	pattern := filepath.Join(os.TempDir(), "hearth-agent-*.pid")
	files, _ := filepath.Glob(pattern)
	for _, f := range files {
		base := filepath.Base(f)
		// base = "hearth-agent-<agentID>.pid"
		agentID := strings.TrimSuffix(strings.TrimPrefix(base, "hearth-agent-"), ".pid")
		if agentID == "" {
			continue
		}
		pid, storedFP := readAgentPIDFile(agentID)
		if pid <= 0 {
			continue
		}
		liveFP, ok := processStartFingerprint(pid)
		if !ok {
			removeAgentPIDFile(agentID)
			continue
		}
		if storedFP != "" && liveFP != "" && storedFP != liveFP {
			log.Printf("[agent-identity] re-register skip: PID %d for agent %s fingerprint mismatch (stale)", pid, agentID)
			removeAgentPIDFile(agentID)
			continue
		}
		d.agentIdentitiesMu.Lock()
		if d.agentIdentities == nil {
			d.agentIdentities = map[int]agentIdentityRecord{}
		}
		d.agentIdentities[pid] = agentIdentityRecord{
			AgentID:          agentID,
			PID:              pid,
			StartFingerprint: liveFP,
			SpawnTime:        time.Now(),
		}
		d.agentIdentitiesMu.Unlock()
		log.Printf("[agent-identity] re-registered agent %s at PID %d (pid file)", agentID, pid)
	}

	// Source 2: /proc environ scan (no-op on darwin). Catches agents
	// whose PID files are missing — e.g. first run after upgrade.
	procAgents := scanProcForHearthAgents()
	for agentID, pid := range procAgents {
		d.agentIdentitiesMu.RLock()
		_, alreadyRegistered := d.agentIdentities[pid]
		d.agentIdentitiesMu.RUnlock()
		if alreadyRegistered {
			continue
		}
		liveFP, ok := processStartFingerprint(pid)
		if !ok {
			continue
		}
		d.agentIdentitiesMu.Lock()
		if d.agentIdentities == nil {
			d.agentIdentities = map[int]agentIdentityRecord{}
		}
		d.agentIdentities[pid] = agentIdentityRecord{
			AgentID:          agentID,
			PID:              pid,
			StartFingerprint: liveFP,
			SpawnTime:        time.Now(),
		}
		d.agentIdentitiesMu.Unlock()
		// Persist so future restarts can use the faster PID-file path.
		writeAgentPIDFile(agentID, pid, liveFP)
		log.Printf("[agent-identity] re-registered agent %s at PID %d (proc scan)", agentID, pid)
	}
}

// unregisterAgentIdentity drops the registration for a PID. Called
// from the relay-wait goroutine when the agent process exits. Safe to
// call with a PID that was never registered.
func (d *Daemon) unregisterAgentIdentity(pid int) {
	if d == nil || pid <= 0 {
		return
	}
	d.agentIdentitiesMu.Lock()
	defer d.agentIdentitiesMu.Unlock()
	delete(d.agentIdentities, pid)
}

// lookupAgentByPID returns the registered agent_id for a PID if one
// exists AND the live process at that PID has the same start-time
// fingerprint as when we registered it. The fingerprint check
// catches PID reuse: if the original agent died and the kernel
// later handed the same PID to an unrelated process, the registry
// entry survives (the cleanup goroutine may not have run yet) but
// the new process's fingerprint differs.
//
// Behavior on fingerprint trouble:
//   - Registered fingerprint empty (registration couldn't read it):
//     accept the PID match. Mostly a no-op for the recycle defense
//     in this rare path, but better than refusing all matches.
//   - Live fingerprint empty (current read failed): also accept.
//     Same rationale — being strict here would reject legitimate
//     callers in transient /proc / sysctl glitches.
//   - Both present and unequal: REFUSE the match. This is the
//     PID-recycle case we care about.
func (d *Daemon) lookupAgentByPID(pid int) (string, bool) {
	if d == nil {
		return "", false
	}
	d.agentIdentitiesMu.RLock()
	r, ok := d.agentIdentities[pid]
	d.agentIdentitiesMu.RUnlock()
	if !ok {
		return "", false
	}
	liveFP, fpOK := processStartFingerprint(pid)
	if fpOK && r.StartFingerprint != "" && liveFP != r.StartFingerprint {
		log.Printf("[agent-identity] PID recycle suspected for pid=%d (registered fp=%q live fp=%q) — refusing match",
			pid, r.StartFingerprint, liveFP)
		return "", false
	}
	return r.AgentID, true
}

// maxTreeWalkHops caps the parent-walk depth so a pathological
// process tree (cycles, runaway depth) can't hang the resolver.
// Real shells / agents / harnesses rarely nest beyond a handful.
const maxTreeWalkHops = 32

// resolveCallerAgent walks the process tree upward from callerPID
// looking for a PID in the agent registry. On a registry miss it falls
// back to resolveAgentFromEnvironChain (Linux only) which reads
// HEARTH_AGENT_INSTANCE_ID from /proc environ — useful when the registry
// is stale after a daemon restart. Returns (agent_id, true) on hit;
// ("", false) when the walk reaches PID 1 / the daemon's own PID / the
// hop cap without finding one.
func (d *Daemon) resolveCallerAgent(callerPID int) (string, bool) {
	if d == nil || callerPID <= 0 {
		return "", false
	}
	selfPID := os.Getpid()

	// Phase 1: registry walk.
	cur := callerPID
	for i := 0; i < maxTreeWalkHops; i++ {
		if cur <= 1 || cur == selfPID {
			break
		}
		if agentID, ok := d.lookupAgentByPID(cur); ok {
			log.Printf("[agent-identity] registry hit: pid=%d → agent=%s (walk_start=%d)", cur, agentID, callerPID)
			return agentID, true
		}
		ppid, ok := getParentPID(cur)
		if !ok || ppid == cur {
			break
		}
		cur = ppid
	}

	// Phase 2: environ-chain fallback (Linux only). Useful when the
	// registry is stale — e.g. agent was running before daemon restart
	// and the PID file / proc-scan didn't restore the entry.
	// DEBUG-ONLY: log the miss and fallback result for diagnostics.
	d.agentIdentitiesMu.RLock()
	registrySize := len(d.agentIdentities)
	d.agentIdentitiesMu.RUnlock()
	if agentID, foundPID, ok := resolveAgentFromEnvironChain(callerPID); ok {
		log.Printf("[agent-identity] environ fallback: pid=%d has HEARTH_AGENT_INSTANCE_ID=%s (caller_pid=%d registry_size=%d)",
			foundPID, agentID, callerPID, registrySize)
		return agentID, true
	}
	log.Printf("[agent-identity] no agent found: caller_pid=%d registry_size=%d", callerPID, registrySize)
	return "", false
}

// derivePrincipal is the Phase-1 authoritative resolver. Tree walk
// from the IPC caller's PID is the source of truth for which agent
// (if any) is making the call; the CLI's claim is treated as a hint
// or refused outright per the fallback policy in
// docs/agent-identity-plan.md.
//
// Returns:
//   - (kind, id, nil) — caller authorized as that principal.
//   - ("", "", *PluginError) — call refused with ErrUnauthorized
//     because the claim mentioned an agent but the tree walk
//     couldn't find one ("soft-promotion" guard).
//
// Fallback when peer-cred lookup itself fails (non-Unix conn — e.g.
// net.Pipe in tests, or any wrapping that strips ucred): falls back
// to the legacy claim-based behavior with the human-user default.
// This keeps existing test scaffolds working; a real production
// daemon's clients all flow through the Unix listener.
func (d *Daemon) derivePrincipal(conn net.Conn, claimedKind, claimedID, surface string) (kind, id string, perr *PluginError) {
	pid, ok := peerPID(conn)
	if !ok {
		// Best-effort: fall through to the legacy semantics. Logged
		// once at TRACE-equivalent verbosity — not WARN, because
		// hitting this on every test-daemon IPC would drown the log.
		return legacyClaimPrincipal(claimedKind), legacyClaimID(claimedID, d.humanUserID), nil
	}
	derivedAgent, derivedOK := d.resolveCallerAgent(pid)

	switch {
	case derivedOK:
		// Tree walk found an agent. That's authoritative.
		if claimedKind == "agent" && claimedID != "" && claimedID != derivedAgent {
			log.Printf("[agent-identity] %s: mismatch — using derived (caller_pid=%d derived_agent=%s claimed_agent=%s)",
				surface, pid, derivedAgent, claimedID)
		}
		return "agent", derivedAgent, nil
	case claimedKind == "agent" && claimedID != "":
		// Tree walk found no agent, but the caller claimed one.
		// Refuse — silently falling back to the operator's human
		// principal would be a soft promotion (the operator usually
		// has BROADER rules than the agent claims).
		log.Printf("[agent-identity] %s: refusing claim-without-tree (caller_pid=%d claimed_agent=%s)",
			surface, pid, claimedID)
		return "", "", &PluginError{
			Code:    ErrUnauthorized,
			Message: "agent principal claimed but caller's process tree contains no registered agent",
		}
	default:
		// No tree, no claim → genuine operator at the terminal.
		return "human", d.humanUserID, nil
	}
}

// legacyClaimPrincipal / legacyClaimID reproduce the pre-Phase-1
// "claim-wins with human default" semantics for the rare path where
// peer-cred lookup fails (non-Unix conn). Same shape as the inline
// blocks that lived in each handler before this refactor.
func legacyClaimPrincipal(claimedKind string) string {
	if claimedKind != "" {
		return claimedKind
	}
	return "human"
}

func legacyClaimID(claimedID, defaultHumanID string) string {
	if claimedID != "" {
		return claimedID
	}
	return defaultHumanID
}
