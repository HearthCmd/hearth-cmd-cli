package main

import "strings"

// screenSubprotocol is the WebSocket subprotocol a browser-as-screen offers on
// /ws/screen, followed by its screen id and secret: ["hearth.screen.v1", id, secret].
// The server echoes only the protocol token; the id/secret ride the offer (read
// from the handshake header, never echoed). Presenting a credential this way avoids
// putting the secret in the URL (which can be logged) and needs no blocking read.
const screenSubprotocol = "hearth.screen.v1"

// parseSubprotocols splits a Sec-WebSocket-Protocol header value into its tokens.
func parseSubprotocols(header string) []string {
	if header == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Browser-as-screen access control on the display server (§B3 of
// docs/display-browser-screens-plan.md). The relay is the source of truth for which
// screens are bound to this host; it pushes that set (display_screens, keyed by
// io_device_id, carrying each screen's secret_hash) over /ws/daemon. This server
// caches it and:
//   - validates a /ws/screen viewer's credential locally against it (no per-connect
//     relay round trip) — validScreenCredential; the /ws/screen ENFORCEMENT that
//     rejects an uncredentialed browser rides the kiosk that presents one (§B4);
//   - closes a screen's live sockets when it drops from a fresh push (revoked) —
//     applyDisplayScreens → evictScreen — the relay's forceCloseDeviceWS, one hop out.

// displayScreenInfo mirrors the relay's display_screens payload entry.
type displayScreenInfo struct {
	ScreenID   string `json:"screen_id"`
	SecretHash string `json:"secret_hash"`
	Name       string `json:"name"`
	IsTemp     bool   `json:"is_temp"`
}

// applyDisplayScreens replaces the known screen set with what the relay just pushed
// and evicts any screen that dropped out (revoked/unbound) — closing its live
// /ws/screen connections so the panel falls back to the pair page. Sending an empty
// set (the last screen was revoked) therefore evicts everything, by design.
func (d *displayServer) applyDisplayScreens(screens []displayScreenInfo) {
	next := make(map[string]screenCred, len(screens))
	for _, s := range screens {
		next[s.ScreenID] = screenCred{SecretHash: s.SecretHash, Name: s.Name, IsTemp: s.IsTemp}
	}

	d.mu.Lock()
	prev := d.known
	d.known = next
	d.mu.Unlock()

	// Evict screens present before but gone now.
	for id := range prev {
		if _, still := next[id]; !still {
			d.evictScreen(id)
		}
	}
}

// validScreenCredential reports whether (screenID, secret) matches a known bound
// screen — the check /ws/screen makes for a credentialed viewer. An unknown screen
// or a hash mismatch is false. A screen with a blank secret_hash (endpoint-style
// rows never mint one) can never be authenticated: a real secret can't hash to "".
func (d *displayServer) validScreenCredential(screenID, secret string) bool {
	if screenID == "" || secret == "" {
		return false
	}
	d.mu.Lock()
	cred, ok := d.known[screenID]
	d.mu.Unlock()
	if !ok || cred.SecretHash == "" {
		return false
	}
	return sha256Hex([]byte(secret)) == cred.SecretHash
}

// knownScreen returns the metadata for a bound screen id, if the relay has reported
// it. Used to resolve a credentialed connection's identity (§B4) and by tests.
func (d *displayServer) knownScreen(screenID string) (screenCred, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cred, ok := d.known[screenID]
	return cred, ok
}

// subscribeEvict registers an eviction signal for a live /ws/screen connection on a
// screen; the handler selects on the returned channel and tears down when it closes.
func (d *displayServer) subscribeEvict(id string) chan struct{} {
	ch := make(chan struct{})
	d.mu.Lock()
	d.stateLocked(d.screenKey(id)).evicts[ch] = struct{}{}
	d.mu.Unlock()
	return ch
}

func (d *displayServer) unsubscribeEvict(id string, ch chan struct{}) {
	d.mu.Lock()
	if st := d.screens[d.screenKey(id)]; st != nil {
		delete(st.evicts, ch)
	}
	d.mu.Unlock()
}

// evictScreen closes every live connection's eviction signal for a screen (and
// clears the registry), so the /ws/screen handlers for that screen return and close
// their sockets. Idempotent: closing then dropping the set means a late
// unsubscribeEvict is a no-op delete.
func (d *displayServer) evictScreen(id string) {
	d.mu.Lock()
	var chans []chan struct{}
	if st := d.screens[d.screenKey(id)]; st != nil {
		for ch := range st.evicts {
			chans = append(chans, ch)
		}
		st.evicts = make(map[chan struct{}]struct{})
	}
	d.mu.Unlock()
	for _, ch := range chans {
		close(ch)
	}
}
