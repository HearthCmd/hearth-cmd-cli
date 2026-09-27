//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Household teardown: the relay is about to permanently delete the household
// this host belongs to, and asks the host to clean up after it first. The host
// stops its agents, erases what Hearth created for the household on this
// computer, signs out, and says so (household_teardown_done). The daemon
// process stays up, signed out, exactly as after `hearth logout`, so a
// service manager doesn't restart it into a loop; `hearth login` brings it
// back for another household.
//
// What is erased:
//   - <agent home>/<household slug>/: every folder Hearth made for the
//     household's agents, chat uploads included;
//   - what Claude Code kept about each of those folders outside them
//     (eraseClaudeSessionState), only for folders inside that root. A folder
//     someone pointed an agent at themselves is theirs, and so is any Claude
//     history about it;
//   - installed plugins and the daemon's local plugin state, resource cache
//     and message queue, all of which belonged to the household;
//   - the credentials.
//
// The host's secrets key stays: everything encrypted to it lived on the relay
// and is deleted there, and a signed-out daemon still holds it in memory.

// householdSlugRe is what a household slug may look like before it becomes a
// path component. Anything else (a separator, "..", empty) is refused.
var householdSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

type householdTeardownFrame struct {
	OrganizationID     string   `json:"organization_id"`
	HouseholdSlug      string   `json:"household_slug"`
	WorkingDirectories []string `json:"working_directories"`
}

// householdTeardownFlushWait bounds the wait for stopped agents to report.
// Shorter than Shutdown's: the relay waits 30s in all for the acknowledgement,
// and stopping agents can already take agentStopGrace (10s).
const householdTeardownFlushWait = 5 * time.Second

// householdTeardownOnce keeps a repeated frame from running the teardown
// twice at once.
var householdTeardownOnce sync.Mutex

// handleHouseholdTeardown runs the teardown off the WebSocket read loop: it
// ends by closing that WebSocket, which waits for the read loop to finish.
func (d *Daemon) handleHouseholdTeardown(data []byte) {
	var f householdTeardownFrame
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("household_teardown: bad frame: %v", err)
		return
	}
	go d.runHouseholdTeardown(f)
}

func (d *Daemon) runHouseholdTeardown(f householdTeardownFrame) {
	if !householdTeardownOnce.TryLock() {
		log.Printf("household_teardown: already running")
		return
	}
	defer householdTeardownOnce.Unlock()
	log.Printf("household_teardown: household %s (%s) is being deleted; cleaning up", f.OrganizationID, f.HouseholdSlug)

	d.stopAllAgents()
	if d.pluginSupervisor != nil {
		if err := d.pluginSupervisor.ShutdownAll(); err != nil {
			log.Printf("household_teardown: plugin shutdown: %v", err)
		}
	}

	d.identityMu.RLock()
	agentHome := d.agentHomePath
	d.identityMu.RUnlock()
	if agentHome == "" {
		agentHome = localAgentHomeBase()
	}
	errs := eraseHouseholdFiles(agentHome, f.HouseholdSlug, f.WorkingDirectories, d.pluginsDir)
	// Let the stopped agents report their exit over the still-open WebSocket
	// (as Shutdown does) before it is dropped below.
	flushed := make(chan struct{})
	go func() { d.agentWg.Wait(); close(flushed) }()
	select {
	case <-flushed:
	case <-time.After(householdTeardownFlushWait):
	}
	if d.localDB != nil {
		for _, table := range []string{"plugin_state", "resource_entities", "agent_inbox"} {
			if _, err := d.localDB.db.Exec(`DELETE FROM ` + table); err != nil {
				errs = append(errs, fmt.Sprintf("clear %s: %v", table, err))
			}
		}
	}

	ack, _ := json.Marshal(map[string]interface{}{
		"type":            "household_teardown_done",
		"organization_id": f.OrganizationID,
		"ok":              len(errs) == 0,
		"error":           strings.Join(errs, "; "),
	})
	if d.daemonWS != nil {
		d.daemonWS.SendText(ack)
	}
	if len(errs) > 0 {
		log.Printf("household_teardown: finished with errors: %s", strings.Join(errs, "; "))
	}

	// Sign out, as `hearth logout` does, but completely: the host itself is
	// being deleted, so its id goes too.
	for _, k := range []string{"user_id", "host_id", "host_secret", "io_device_id", "io_device_secret", "email"} {
		_ = writeConfigValue(k, "")
	}
	d.humanUserID = ""
	d.hostID = ""
	d.hostSecret = ""
	if d.daemonWS != nil {
		d.daemonWS.Close()
		d.daemonWS = nil
	}
	log.Printf("household_teardown: done; signed out. Run 'hearth login' to use this computer with another household.")
}

// stopAllAgents stops every running agent concurrently (each Stop can take up
// to agentStopGrace).
func (d *Daemon) stopAllAgents() {
	d.mu.RLock()
	snapshot := make([]*AgentInstance, 0, len(d.instances))
	for _, s := range d.instances {
		snapshot = append(snapshot, s)
	}
	d.mu.RUnlock()
	var wg sync.WaitGroup
	for _, s := range snapshot {
		wg.Add(1)
		go func(inst *AgentInstance) {
			defer wg.Done()
			log.Printf("household_teardown: stopping agent instance %s", inst.aiAgentInstanceID)
			inst.Stop()
		}(s)
	}
	wg.Wait()
}

// eraseHouseholdFiles removes what Hearth created on this computer for one
// household: the household's folder under agentHome, Claude Code's state
// about the agent folders inside it, and the plugins directory. Returns the
// problems it hit; a refused slug erases nothing.
func eraseHouseholdFiles(agentHome, slug string, workingDirs []string, pluginsDir string) []string {
	var errs []string
	if !householdSlugRe.MatchString(slug) {
		return []string{fmt.Sprintf("refusing household slug %q", slug)}
	}
	agentHome = expandHome(agentHome)
	if !filepath.IsAbs(agentHome) || filepath.Clean(agentHome) == "/" {
		return []string{fmt.Sprintf("refusing agent home %q", agentHome)}
	}
	root := filepath.Join(filepath.Clean(agentHome), slug)

	for _, wd := range workingDirs {
		// A path recorded with the app's "~/" shorthand names the same
		// folder; Claude Code knows it by its absolute path.
		if wd = expandHome(wd); pathInside(root, wd) {
			eraseClaudeSessionState(filepath.Clean(wd))
		}
	}
	if err := os.RemoveAll(root); err != nil {
		errs = append(errs, fmt.Sprintf("remove %s: %v", root, err))
	}
	if pluginsDir != "" && filepath.IsAbs(pluginsDir) && filepath.Clean(pluginsDir) != "/" {
		if err := os.RemoveAll(pluginsDir); err != nil {
			errs = append(errs, fmt.Sprintf("remove %s: %v", pluginsDir, err))
		}
	}
	return errs
}

// pathInside reports whether p is root or somewhere below it, lexically.
func pathInside(root, p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
