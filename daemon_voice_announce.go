//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"log"
)

// satelliteVerb picks the Home Assistant service to speak with. Two verbs, chosen
// by a boolean the relay sends — never by a verb name on the wire, so a relay push
// still cannot drive an arbitrary HA service from this host.
//
//   - announce: speak and stop. The exchange is over.
//   - start_conversation: speak, then re-open the satellite's microphone so the
//     household can answer with no wake word. Used when the agent marked its reply
//     <hearth-voice listen> because it asked a question.
func satelliteVerb(listen bool) string {
	if listen {
		return "start_conversation"
	}
	return "announce"
}

// handleAnnounceSatellite speaks a message out loud on a Home Assistant voice
// satellite (voice V5b), on behalf of the agent the relay named. The relay pushes
// this to the host holding the HA connection — the agent may be frozen on a parked
// tool call and unable to speak for itself, and the relay can't reach HA at all.
// We invoke the HA verb AS THAT AGENT: the server-side authorize gates it on the
// agent's existing HA grant (the same grant it uses to speak answers), autofill
// supplies ha_token, and this host decrypts it locally (the credential is pinned
// here). The verb comes from satelliteVerb's fixed pair, so a relay push can never
// drive any other HA verb.
//
// Best-effort and fire-and-forget: any failure just logs and the puck stays quiet
// (the phone approval path is untouched). Runs off the WS read loop.
func (d *Daemon) handleAnnounceSatellite(aiAgentInstanceID, connection, entityID, message string, listen bool) {
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

	verb := satelliteVerb(listen)

	args, err := json.Marshal(map[string]string{"entity_id": entityID, "message": message})
	if err != nil {
		log.Printf("daemon: announce_satellite: marshal args: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), resourceInvokeTimeout())
	defer cancel()

	// Authorize as the named agent (principal + its grant), fixed verb from the pair.
	_, autofillCreds, pe := d.preflightAuthorizeResourceInvoke(
		d.resourceAuthzWS, "agent", aiAgentInstanceID,
		rc.ConnectionID, manifest.PluginSlug, verb, args, aiAgentInstanceID)
	if pe != nil {
		log.Printf("daemon: announce_satellite: authorize failed for agent %s on %s (%s): %s",
			aiAgentInstanceID, connection, verb, pe.Message)
		return
	}

	// Autofill supplies the connection's credential (ha_token); no --secret here.
	secretCleartexts, secretErr := d.resolveSecretBindings(mergeSecretBindings(autofillCreds, nil), "agent", aiAgentInstanceID)
	if secretErr != nil {
		log.Printf("daemon: announce_satellite: resolve credential failed: %s", secretErr.Message)
		return
	}
	defer zeroSecretMap(secretCleartexts)

	if _, err := d.invokeDeclarativeVerb(ctx, rc, manifest, verb, args, secretCleartexts); err != nil {
		log.Printf("daemon: announce_satellite: %s failed: %v", verb, err)
		return
	}
	log.Printf("daemon: announce_satellite: %s on %s via %s (for agent %s)", verb, entityID, connection, aiAgentInstanceID)
}
