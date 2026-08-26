//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"log"
)

// handleAnnounceSatellite speaks a message out loud on a Home Assistant voice
// satellite (voice V5b), on behalf of the agent the relay named. The relay pushes
// this to the host holding the HA connection when a voice-driven agent's action
// parked on approval — the agent is frozen on the parked tool call and can't speak
// for itself, and the relay can't reach HA. We invoke the HA announce verb AS THAT
// AGENT: the server-side authorize gates it on the agent's existing HA grant (the
// same grant it uses to speak answers), autofill supplies ha_token, and this host
// decrypts it locally (the credential is pinned here). The verb is hardcoded to
// "announce", so a relay push can never drive any other HA verb.
//
// Best-effort and fire-and-forget: any failure just logs and the puck stays quiet
// (the phone approval path is untouched). Runs off the WS read loop.
func (d *Daemon) handleAnnounceSatellite(aiAgentInstanceID, connection, entityID, message string) {
	if aiAgentInstanceID == "" || connection == "" || entityID == "" || message == "" {
		log.Printf("daemon: announce_satellite: missing field (agent=%q conn=%q entity=%q msg_empty=%v)",
			aiAgentInstanceID, connection, entityID, message == "")
		return
	}
	rc, ok := d.resourceConnections.Resolve(connection)
	if !ok {
		log.Printf("daemon: announce_satellite: unknown connection %q", connection)
		return
	}
	manifest, ok := d.plugins.GetPluginBySlug(rc.PluginSlug)
	if !ok {
		log.Printf("daemon: announce_satellite: plugin not registered for %q", rc.PluginSlug)
		return
	}
	if manifest.Source != SourceDeclarative || d.declarativeExecutor == nil {
		log.Printf("daemon: announce_satellite: %q is not a declarative adapter; cannot announce", connection)
		return
	}

	args, err := json.Marshal(map[string]string{"entity_id": entityID, "message": message})
	if err != nil {
		log.Printf("daemon: announce_satellite: marshal args: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), resourceInvokeTimeout())
	defer cancel()

	// Authorize as the named agent (principal + its grant), fixed verb "announce".
	_, autofillCreds, pe := d.preflightAuthorizeResourceInvoke(
		d.resourceAuthzWS, "agent", aiAgentInstanceID,
		rc.ConnectionID, manifest.PluginSlug, "announce", args, aiAgentInstanceID)
	if pe != nil {
		log.Printf("daemon: announce_satellite: authorize failed for agent %s on %s: %s",
			aiAgentInstanceID, connection, pe.Message)
		return
	}

	// Autofill supplies the connection's credential (ha_token); no --secret here.
	secretCleartexts, secretErr := d.resolveSecretBindings(mergeSecretBindings(autofillCreds, nil), "agent", aiAgentInstanceID)
	if secretErr != nil {
		log.Printf("daemon: announce_satellite: resolve credential failed: %s", secretErr.Message)
		return
	}
	defer zeroSecretMap(secretCleartexts)

	if _, err := d.invokeDeclarativeVerb(ctx, rc, manifest, "announce", args, secretCleartexts); err != nil {
		log.Printf("daemon: announce_satellite: invoke failed: %v", err)
		return
	}
	log.Printf("daemon: announce_satellite: spoke on %s via %s (for agent %s)", entityID, connection, aiAgentInstanceID)
}
