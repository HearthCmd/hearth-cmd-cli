//go:build darwin || linux

package main

import (
	"encoding/json"
	"log"
)

// agentResourceGrantsFetchResp mirrors the server-side response in
// hearth-cmd/cmd/hearth-cloud/agent_resource_grants_ws.go. Kept in
// sync by hand.
type agentResourceGrantsFetchResp struct {
	Type   string                       `json:"type"`
	Error  string                       `json:"error"`
	Grants []agentResourceGrantFetchRow `json:"grants"`
}

type agentResourceGrantFetchRow struct {
	AIAgentInstanceID    string `json:"ai_agent_instance_id"`
	ResourceConnectionID string `json:"resource_connection_id"`
}

// fetchAgentResourceGrantsAtBoot pulls the server's (agent → granted
// connection) view scoped to agents on this host, and swaps it into
// d.agentGrants. Called at boot, on reconnect, and on the
// agent_resource_grants_changed live-push frame.
//
// Best-effort: a missing WS / failed call logs and moves on. The
// store stays at its previous state — daemon keeps using the last
// known good view until the next refetch lands.
func (d *Daemon) fetchAgentResourceGrantsAtBoot() {
	if d.agentGrants == nil || d.daemonWS == nil {
		return
	}
	if !d.waitForDaemonWS(seedWSConnectTimeout) {
		log.Printf("daemon: fetchAgentResourceGrants skipped: WS did not connect within %s", seedWSConnectTimeout)
		return
	}

	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "agent_resource_grants_fetch", nil)
	if err != nil {
		log.Printf("daemon: agent_resource_grants_fetch ws_request: %v", err)
		return
	}
	var resp agentResourceGrantsFetchResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		log.Printf("daemon: agent_resource_grants_fetch decode: %v", err)
		return
	}
	if resp.Type == "error" {
		log.Printf("daemon: agent_resource_grants_fetch server error: %s", resp.Error)
		return
	}

	next := map[string]map[string]struct{}{}
	for _, g := range resp.Grants {
		set, ok := next[g.AIAgentInstanceID]
		if !ok {
			set = map[string]struct{}{}
			next[g.AIAgentInstanceID] = set
		}
		set[g.ResourceConnectionID] = struct{}{}
	}
	d.agentGrants.swap(next)
	log.Printf("daemon: agent_resource_grants fetched — %d agent(s) with grants, %d total rows",
		len(next), len(resp.Grants))
}
