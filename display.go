package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// hearth display — the household-display server (docs/household-display-plan.md).
//
// Slice 3a is the standalone LAN-server skeleton that proves the resilience
// model (reconstruct-on-connect): a browser pointed at this box's HTTP endpoint
// gets the complete current content on every (re)connect, so a reload, a power
// blip, or a monitor that napped all resolve to the correct screen with no one
// standing in the kitchen. Persistence, TTLs, multi-screen addressing, host
// enrollment/relay routing, and screen claiming are later slices. Nothing here
// mints trust — it serves a page to whatever browser connects; identity and
// claiming (which screen is whose) come with the relay-backed pairing slice.

// screenAssignment is one content layer for a screen. ExpiresAt zero means no
// expiry: ambient never expires, a published layer always carries one.
type screenAssignment struct {
	Kind      string    `json:"kind"`       // url | image | video | markdown | pairing | "" (nothing)
	Payload   string    `json:"payload"`    // URL for url/image/video; rendered HTML for markdown
	ExpiresAt time.Time `json:"expires_at"` // zero = no expiry
}

func (a screenAssignment) empty() bool { return a.Kind == "" }

// viewportInfo is the browser window's reported dimensions for a screen
// (docs/display-viewport-plan.md): CSS pixels (w/h) plus devicePixelRatio. It's a
// tuning hint for a publishing agent, not a canvas contract — a window resizes and
// rotates. Reported by the kiosk over /ws/screen, cached here, and forwarded to the
// relay in the per-screen display_state report.
type viewportInfo struct {
	W   int     `json:"w"`
	H   int     `json:"h"`
	DPR float64 `json:"dpr"`
}

// viewportBounds clamps untrusted kiosk-reported dimensions. w/h in reasonable
// pixel range; dpr in a sane density range. An out-of-range value fails the parse
// so the frame is dropped and the screen keeps its last-known (or unknown) size.
func (v viewportInfo) valid() bool {
	return v.W >= 1 && v.W <= 32768 && v.H >= 1 && v.H <= 32768 && v.DPR >= 0.1 && v.DPR <= 10
}

func (a screenAssignment) expired(now time.Time) bool {
	return !a.ExpiresAt.IsZero() && now.After(a.ExpiresAt)
}

// effectiveContent resolves the two layers to what the screen should show right
// now: the published layer if present and unexpired, else ambient. This is the
// whole content model (§ The content model) — kept pure so it's unit-tested
// without a running server.
func effectiveContent(ambient, published screenAssignment, now time.Time) screenAssignment {
	if !published.empty() && !published.expired(now) {
		return published
	}
	return ambient
}

// displayTransport is the relay link the display subsystem reports over. It's an
// interface so the same displayServer works two ways: standalone (`hearth display`
// on a dedicated box owns its own *WSClient) and unified (the role-aware daemon
// drives the display subsystem over its single *DaemonWS — one host, one
// connection). Both satisfy it. Incoming display frames are pushed to
// handleRelayFrame by whoever owns the connection.
type displayTransport interface {
	SendText(data []byte)
	IsConnected() bool
}

// screenState is the content + live browsers for ONE screen. A display server
// keys these by screen io_device_id (§B1 of docs/display-browser-screens-plan.md)
// so a single host can drive many panels (kitchen, office) independently.
type screenState struct {
	ambient     screenAssignment
	published   screenAssignment
	pairingCode string        // non-empty while unclaimed: show this code on this screen
	viewport    *viewportInfo // last browser-reported window dims; nil = never reported
	subs        map[chan struct{}]struct{}
	// evicts are per-connection "your screen was revoked, close now" signals. Closed
	// (never sent on) by evictScreen when this screen drops from a display_screens
	// push, so the /ws/screen handler tears the socket down (§B3).
	evicts map[chan struct{}]struct{}
}

func newScreenState() *screenState {
	return &screenState{
		subs:   make(map[chan struct{}]struct{}),
		evicts: make(map[chan struct{}]struct{}),
	}
}

// displayServer holds per-screen content and the live browser connections to wake
// on a change. One host, many screens: the relay routes each display.publish to a
// screen_id (resolveDisplayScreen, relay display_publish.go) and this server keys
// content by it. The special key "" is THIS box's own/primary screen — a browser
// that connects to /ws/screen without an io_device_id, and any relay frame whose
// screen_id equals this box's own paired screen (primaryID), both resolve to it,
// so a single-screen box behaves exactly as before browser-as-screen (§B4) lands.
type displayServer struct {
	mu        sync.Mutex
	screens   map[string]*screenState
	primaryID string // this box's own paired screen io_device_id; collapses onto key ""
	// known is the relay-pushed set of screens bound to THIS host (display_screens
	// frame, §B3): screen io_device_id → its credential/metadata. /ws/screen viewers
	// are validated against it locally (no per-connect relay round trip); a screen
	// dropping out (revoked) evicts its live sockets. nil until the first push.
	known map[string]screenCred
	// screenPairLimiter throttles /screen/pair per LAN client (§B7) so a peer can't
	// burn the host's shared relay /pair/start budget. The pairing is otherwise
	// stateless — the browser mints + holds its own secret; this server holds nothing.
	screenPairLimiter *screenPairLimiter
	// reapTimers are the presence-grace timers for ephemeral (is_temp) screens whose
	// last browser has left (§B5): after reapGrace with no reconnect, we ask the relay
	// to remove the screen. reapGrace <= 0 means the default (defaultReapGrace).
	reapMu     sync.Mutex
	reapTimers map[string]*time.Timer
	reapGrace  time.Duration
	// relayTx is the link display_state reports go out on. In standalone mode it's
	// relayWS below; in unified mode the daemon sets it to its own DaemonWS.
	relayTx displayTransport
	relayWS *WSClient // standalone-owned /ws/daemon client (nil in unified mode)
}

// screenCred is one bound screen as the relay reports it — enough to validate a
// viewer (secret_hash) and render its identity/temp state.
type screenCred struct {
	SecretHash string
	Name       string
	IsTemp     bool
}

func newDisplayServer() *displayServer {
	return &displayServer{
		screens:           make(map[string]*screenState),
		known:             make(map[string]screenCred),
		reapTimers:        make(map[string]*time.Timer),
		screenPairLimiter: newScreenPairLimiter(),
	}
}

// screenKey resolves an addressed screen id to its map key. "" (a browser with no
// io_device_id) and this box's own screen (primaryID) both live at key "" — the
// primary screen — so single-screen behavior is unchanged; every other id is its
// own key. Caller need not hold d.mu (reads primaryID, set once at startup).
func (d *displayServer) screenKey(id string) string {
	if id == "" || (d.primaryID != "" && id == d.primaryID) {
		return ""
	}
	return id
}

// stateLocked returns the screenState for a key, creating it on first use. Caller
// holds d.mu.
func (d *displayServer) stateLocked(key string) *screenState {
	st := d.screens[key]
	if st == nil {
		st = newScreenState()
		d.screens[key] = st
	}
	return st
}

// current returns what THIS box's primary screen (key "") should show — the
// back-compat accessor used by local publish, display_state reporting, and tests.
func (d *displayServer) current() screenAssignment { return d.currentForScreen("") }

// currentForScreen resolves the effective content for one screen: its pairing code
// while unclaimed, else published-if-live else ambient.
func (d *displayServer) currentForScreen(id string) screenAssignment {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.screens[d.screenKey(id)]
	if st == nil {
		return screenAssignment{}
	}
	if st.pairingCode != "" {
		return screenAssignment{Kind: "pairing", Payload: st.pairingCode}
	}
	return effectiveContent(st.ambient, st.published, time.Now())
}

// setPublished swaps the primary screen's published layer. The local
// `hearth display show` path lands here; the relay route uses setPublishedForScreen.
func (d *displayServer) setPublished(a screenAssignment) { d.setPublishedForScreen("", a) }

func (d *displayServer) setPublishedForScreen(id string, a screenAssignment) {
	d.mu.Lock()
	d.stateLocked(d.screenKey(id)).published = a
	d.mu.Unlock()
	d.wakeScreen(id)
}

// setAmbient sets the primary screen's ambient (bottom) layer. Ambient authoring
// is not a v1 product surface yet; this exists for local defaults and tests.
func (d *displayServer) setAmbient(a screenAssignment) { d.setAmbientForScreen("", a) }

func (d *displayServer) setAmbientForScreen(id string, a screenAssignment) {
	d.mu.Lock()
	d.stateLocked(d.screenKey(id)).ambient = a
	d.mu.Unlock()
	d.wakeScreen(id)
}

// setPairing shows a server-pushed pairing code on a screen (non-empty) or returns
// to normal content (""). Vestigial since browser-as-screen (§B4): the kiosk now
// runs its own pairing UI from /screen/pair and the server no longer pushes a
// pairing assignment. Retained (with its test) in case a server-driven pairing
// display is wanted later; no startup caller today.
func (d *displayServer) setPairing(code string) { d.setPairingForScreen("", code) }

func (d *displayServer) setPairingForScreen(id, code string) {
	d.mu.Lock()
	d.stateLocked(d.screenKey(id)).pairingCode = code
	d.mu.Unlock()
	d.wakeScreen(id)
}

func (d *displayServer) clearPairing() { d.setPairing("") }

// setViewport records a screen's browser-reported window dimensions and reports the
// (now-enriched) state to the relay so display.query answers with the size. No
// wakeScreen — viewport doesn't change what the screen shows, only what the relay
// knows about it.
func (d *displayServer) setViewport(id string, vp viewportInfo) {
	d.mu.Lock()
	v := vp
	d.stateLocked(d.screenKey(id)).viewport = &v
	d.mu.Unlock()
	d.reportState()
}

// viewportForScreen returns a screen's last-known viewport, or nil if a browser has
// never reported one.
func (d *displayServer) viewportForScreen(id string) *viewportInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st := d.screens[d.screenKey(id)]; st != nil && st.viewport != nil {
		v := *st.viewport
		return &v
	}
	return nil
}

// handleScreenInbound parses a frame the kiosk sent up the /ws/screen socket. Today
// the only upstream frame is `viewport`; unknown types are ignored (forward-compat).
// The dimensions are untrusted LAN-browser input, so they're clamped (valid()) before
// storing — a bad frame is dropped, not fatal.
func (d *displayServer) handleScreenInbound(screenID string, data []byte) {
	var f struct {
		Type string  `json:"type"`
		W    int     `json:"w"`
		H    int     `json:"h"`
		DPR  float64 `json:"dpr"`
	}
	if json.Unmarshal(data, &f) != nil {
		return
	}
	switch f.Type {
	case "viewport":
		vp := viewportInfo{W: f.W, H: f.H, DPR: f.DPR}
		if vp.valid() {
			d.setViewport(screenID, vp)
		}
	}
}

// wakeScreen signals the live /ws/screen connections for ONE screen to re-send
// their current assignment, then reports state to the relay. Non-blocking and
// coalesced (a pending wake covers a new one).
func (d *displayServer) wakeScreen(id string) {
	d.mu.Lock()
	var subs []chan struct{}
	if st := d.screens[d.screenKey(id)]; st != nil {
		subs = make([]chan struct{}, 0, len(st.subs))
		for ch := range st.subs {
			subs = append(subs, ch)
		}
	}
	d.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default: // a pending wake already covers this one
		}
	}
	// Report to the relay so display.query answers from cache. NOTE (§B1): the relay
	// caches per serving_host_id today, so this reports only the primary screen;
	// per-screen reporting/caching for secondary screens arrives with B2.
	d.reportState()
}

// subscribe registers a waiter on THIS box's primary screen (back-compat + tests).
func (d *displayServer) subscribe() chan struct{} { return d.subscribeToScreen("") }

func (d *displayServer) subscribeToScreen(id string) chan struct{} {
	ch := make(chan struct{}, 1)
	d.mu.Lock()
	d.stateLocked(d.screenKey(id)).subs[ch] = struct{}{}
	d.mu.Unlock()
	return ch
}

func (d *displayServer) unsubscribe(ch chan struct{}) { d.unsubscribeFromScreen("", ch) }

func (d *displayServer) unsubscribeFromScreen(id string, ch chan struct{}) {
	d.mu.Lock()
	if st := d.screens[d.screenKey(id)]; st != nil {
		delete(st.subs, ch)
	}
	d.mu.Unlock()
}

// handleScreenWS serves /ws/screen: on connect it sends the complete current
// assignment (reconstruct-on-connect), then re-sends on every content change and
// on a heartbeat (which also lets an expired TTL fall back to ambient).
func (d *displayServer) handleScreenWS(w http.ResponseWriter, r *http.Request) {
	// Which screen this connection is comes from its credential (§B4b): the browser
	// presents ["hearth.screen.v1", <screen_id>, <secret>] as the WS subprotocol and
	// the id is authenticated below. Every viewer must present a valid credential —
	// there is no credential-less path anymore.
	var screenID string

	// A credentialed browser (§B4) presents its screen id + secret via the WS
	// subprotocol: ["hearth.screen.v1", <screen_id>, <secret>]. Echo only the protocol
	// token so the handshake completes; the id/secret are read from the offer here.
	var credScreenID, credSecret string
	// CSWSH guard (§B3, display-access-security.md Option 3): AcceptOptions with no
	// OriginPatterns makes coder/websocket reject any handshake whose Origin doesn't
	// match the request Host, so a drive-by web page in a household browser can't
	// script a socket to /ws/screen and exfiltrate the screen. Kept explicit +
	// test-pinned so a refactor can't silently open it (never set InsecureSkipVerify
	// or a permissive OriginPatterns here). Direct navigation to IP:8090 has
	// Origin==Host and is NOT stopped by this — that is what the credential is for.
	acceptOpts := &websocket.AcceptOptions{}
	if offered := parseSubprotocols(r.Header.Get("Sec-WebSocket-Protocol")); len(offered) > 0 && offered[0] == screenSubprotocol {
		acceptOpts.Subprotocols = []string{screenSubprotocol}
		if len(offered) >= 3 {
			credScreenID, credSecret = offered[1], offered[2]
		}
	}
	conn, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusInternalError, "closing")

	// Require a valid per-screen credential (§B4b): an absent or invalid credential is
	// closed with a policy violation, so only a claimed screen bound to this host (in
	// the relay-pushed `known` set) can stream. A random LAN browser that reaches
	// IP:8090 gets the kiosk's pair page, never content.
	if credScreenID == "" {
		conn.Close(websocket.StatusPolicyViolation, "screen credential required")
		return
	}
	if !d.validScreenCredential(credScreenID, credSecret) {
		conn.Close(websocket.StatusPolicyViolation, "invalid screen credential")
		return
	}
	screenID = credScreenID

	// The kiosk now sends upstream too (a `viewport` frame — docs/display-viewport-plan.md),
	// so we can't use conn.CloseRead (which drains + discards). One reader goroutine
	// parses inbound frames and cancels ctx when the browser departs, preserving
	// CloseRead's teardown semantics; the main loop below only writes. One reader + one
	// writer is the correct coder/websocket pattern (concurrent read/write is allowed).
	ctx, cancelRead := context.WithCancel(r.Context())
	defer cancelRead()
	go func() {
		defer cancelRead() // read error = browser gone → tear the write loop down
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			d.handleScreenInbound(screenID, data)
		}
	}()

	// A revoked screen force-closes here: evictScreen closes this channel when the
	// screen drops from a display_screens push, so the loop below tears down.
	evict := d.subscribeEvict(screenID)
	defer d.unsubscribeEvict(screenID, evict)

	send := func() error {
		cur := d.currentForScreen(screenID)
		msg := map[string]interface{}{"type": "assignment", "kind": cur.Kind, "payload": cur.Payload}
		if !cur.ExpiresAt.IsZero() {
			msg["expires_at"] = cur.ExpiresAt.UTC().Format(time.RFC3339)
		}
		b, _ := json.Marshal(msg)
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}

	if err := send(); err != nil {
		return
	}

	// A live browser cancels any pending presence-grace reap for this screen (§B5).
	d.cancelReap(screenID)

	ch := d.subscribeToScreen(screenID)
	// Registered before the unsubscribe below, so it runs AFTER it (defers are LIFO)
	// — subs is already decremented when we check whether this was the last browser.
	defer func() {
		if d.liveConnCount(screenID) == 0 {
			d.scheduleReap(screenID)
		}
	}()
	defer d.unsubscribeFromScreen(screenID, ch)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "bye")
			return
		case <-evict:
			// The screen was revoked/unbound — drop the socket; the panel re-shows
			// the pair page (§B4). A distinct close code lets the kiosk tell "your
			// screen was removed" from an ordinary disconnect.
			conn.Close(websocket.StatusPolicyViolation, "screen revoked")
			return
		case <-ch:
			if err := send(); err != nil {
				return
			}
		case <-ticker.C:
			if err := send(); err != nil {
				return
			}
		}
	}
}

func runDisplay(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "publish":
			runDisplayPublish(args[1:])
			return
		case "show":
			runDisplayShow(args[1:])
			return
		case "clear":
			runDisplayClear(args[1:])
			return
		case "query":
			runDisplayQuery(args[1:])
			return
		case "--help", "-h":
			fmt.Fprintln(os.Stderr, "Usage: hearth display [--bind <addr>]\n"+
				"       hearth display publish <url> --target <screen> [--type url|image|video] [--ttl <seconds>]\n"+
				"       hearth display publish --target <screen> --type markdown --file <path> [--ttl <seconds>]\n"+
				"       hearth display query --target <screen>\n"+
				"       hearth display clear --target <screen>\n"+
				"       hearth display show <url> [--ttl <seconds>]   (local: this box's screen)\n"+
				"       hearth display clear                          (local: this box's screen)")
			return
		}
	}
	runDisplayServe(args)
}

func runDisplayServe(args []string) {
	fs := flag.NewFlagSet("display", flag.ExitOnError)
	bind := fs.String("bind", "0.0.0.0:8090", "LAN address to serve the display on")
	fs.Parse(args)

	// Footgun guard (§2): a standalone display server opens its OWN /ws/daemon
	// connection for this host. If the agent daemon is already running here, that's
	// a SECOND connection for the same host_id and the relay drops one. On a
	// combined box the display subsystem belongs INSIDE the daemon (role-aware,
	// one connection) — point the user at the role path instead of silently
	// clobbering. A dedicated display-only box (no daemon) is unaffected.
	if isDaemonRunning() {
		fmt.Fprintln(os.Stderr,
			"hearth display: a daemon is already running on this host — a standalone display\n"+
				"server would open a second connection for the same host and the relay would drop one.\n"+
				"On a combined box the daemon drives the screen once it has the display role:\n"+
				"    hearth hh host role add display\n"+
				"    systemctl --user restart hearth   # or: hearth stop && hearth start")
		os.Exit(1)
	}

	// A display box is a host. If this machine isn't enrolled, drop into
	// enrollment as a DISPLAY server (roles {display}) — mirroring how `hearth
	// start` auto-enrolls an agent host. runRegister prompts for email/OTP and
	// returns on success (it exits the process on failure), as in runStart.
	if readConfigValue("host_id") == "" {
		runRegister([]string{"--display"})
		if readConfigValue("host_id") == "" {
			fmt.Fprintln(os.Stderr, "hearth display: not enrolled; run `hearth login --display` first")
			os.Exit(1)
		}
	}

	d := newDisplayServer()

	// Screens pair themselves now (browser-as-screen, §B4): each browser that loads
	// the kiosk claims itself via /screen/pair and holds its own credential, so the
	// daemon no longer auto-pairs one fixed screen at startup.

	// Standalone: own the relay client (dial + reconnect + attach as transport).
	// The unified daemon attaches its OWN /ws/daemon connection instead — see
	// (*Daemon).startDisplaySubsystem — so serve() below stays transport-agnostic.
	go d.connectRelay()

	stop, err := d.serve(*bind)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth display: %v\n", err)
		os.Exit(1)
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	<-sigc
	stop()
	d.closeRelay()
}

// serve starts the display subsystem's LAN HTTP server (kiosk bundle + /ws/screen),
// the local control socket (`hearth display show`/`clear`), and the relay
// heartbeat, and returns a stop func. It is non-blocking and transport-agnostic:
// the relay link is attached separately (connectRelay for standalone, attachTransport
// for the unified daemon), so both entry points share this serving core.
func (d *displayServer) serve(bind string) (func(), error) {
	mux := http.NewServeMux()
	mux.Handle("/", kioskHandler())
	mux.HandleFunc("/ws/screen", d.handleScreenWS)
	// Browser-driven screen pairing (§B4): an unclaimed browser asks this server for
	// a screen and polls until a phone claims it. LAN HTTP (not the local control
	// socket) because the browser may be on another host.
	mux.HandleFunc("/screen/pair", d.handleScreenPair)
	mux.HandleFunc("/screen/pair/poll", d.handleScreenPairPoll)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	srv := &http.Server{Addr: bind, Handler: mux}

	// Local control socket — a unix socket, not an HTTP route: the HTTP server
	// binds the LAN, and publish control must be local-only, never "publish this"
	// from anyone on the network.
	sockPath := displayControlSockPath()
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	go d.serveControl(ln)

	// Heartbeat the current content to the relay so display.query answers from
	// cache and a relay restart re-warms within one interval. Change-driven reports
	// (via wakeScreen) cover immediacy; this covers reconnect + TTL expiry.
	hbStop := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-t.C:
				d.reportState()
			}
		}
	}()

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "hearth display: %v\n", err)
		}
	}()
	fmt.Fprintf(os.Stderr, "hearth display: serving on http://%s  (kiosk at /, screen socket at /ws/screen)\n", bind)

	return func() {
		close(hbStop)
		ln.Close()
		_ = os.Remove(sockPath)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}

// ---- local control channel (hearth display show / clear) -----------------

type controlCommand struct {
	Cmd        string `json:"cmd"`         // "show" | "clear"
	Kind       string `json:"kind"`        // content type: "url" (default) | "image" | "video" | "markdown"
	URL        string `json:"url"`         // the content URL, for kind url|image|video
	Markdown   string `json:"markdown"`    // raw markdown source, for kind=="markdown"
	TTLSeconds int    `json:"ttl_seconds"` // 0 = no expiry (until cleared)
}

type controlReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// displayControlSockPath is the local-only socket the running display server
// listens on and `hearth display show`/`clear` dial.
func displayControlSockPath() string {
	return filepath.Join(os.TempDir(), "hearth-display.sock")
}

// applyControl mutates THIS box's primary screen per a control command (the local
// `hearth display show`/`clear` socket path). The relay route uses
// applyControlForScreen with the target screen id.
func (d *displayServer) applyControl(cmd controlCommand) error {
	return d.applyControlForScreen("", cmd)
}

// applyControlForScreen mutates one screen's content per a control command. Kept
// separate from the socket/relay transports so it's unit-tested directly.
func (d *displayServer) applyControlForScreen(screenID string, cmd controlCommand) error {
	switch cmd.Cmd {
	case "show":
		kind := cmd.Kind
		if kind == "" {
			kind = "url"
		}
		// The display server renders markdown to HTML here; url/image/video carry a
		// bare URL the kiosk mounts directly.
		var payload string
		switch kind {
		case "url", "image", "video":
			if cmd.URL == "" {
				return fmt.Errorf("show requires a url")
			}
			payload = cmd.URL
		case "markdown":
			if cmd.Markdown == "" {
				return fmt.Errorf("markdown show requires markdown content")
			}
			html, err := renderMarkdown(cmd.Markdown)
			if err != nil {
				return fmt.Errorf("render markdown: %w", err)
			}
			payload = html
		default:
			return fmt.Errorf("unknown content type %q (url|image|video|markdown)", kind)
		}
		a := screenAssignment{Kind: kind, Payload: payload}
		if cmd.TTLSeconds > 0 {
			a.ExpiresAt = time.Now().Add(time.Duration(cmd.TTLSeconds) * time.Second)
		}
		d.setPublishedForScreen(screenID, a)
		return nil
	case "clear":
		d.setPublishedForScreen(screenID, screenAssignment{}) // empty published → falls to ambient
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd.Cmd)
	}
}

func (d *displayServer) serveControl(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed on shutdown
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			line, err := bufio.NewReader(c).ReadBytes('\n')
			if err != nil && len(line) == 0 {
				return
			}
			reply := controlReply{OK: true}
			var cmd controlCommand
			if jerr := json.Unmarshal(line, &cmd); jerr != nil {
				reply = controlReply{Error: "invalid command"}
			} else if aerr := d.applyControl(cmd); aerr != nil {
				reply = controlReply{Error: aerr.Error()}
			}
			b, _ := json.Marshal(reply)
			_, _ = c.Write(append(b, '\n'))
		}(conn)
	}
}

func runDisplayShow(args []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "Usage: hearth display show <url> [--ttl <seconds>]")
		os.Exit(1)
	}
	url := args[0]
	fs := flag.NewFlagSet("display show", flag.ExitOnError)
	ttl := fs.Int("ttl", 0, "seconds until the content expires (0 = until cleared)")
	fs.Parse(args[1:])
	sendDisplayControl(controlCommand{Cmd: "show", URL: url, TTLSeconds: *ttl})
}

func runDisplayClear(args []string) {
	fs := flag.NewFlagSet("display clear", flag.ExitOnError)
	target := fs.String("target", "", "screen name or id to clear via the relay; omit to clear this box's local screen")
	fs.Parse(args)
	if *target != "" {
		sendDisplayRequest("display_clear", map[string]interface{}{"target": *target}, "cleared "+*target)
		return
	}
	sendDisplayControl(controlCommand{Cmd: "clear"})
}

// runDisplayPublish publishes content to a NAMED screen through the relay — the
// authorized path (display.publish is gated by authorize()). Distinct from
// `show`, which drives THIS box's own screen over the local socket and skips the
// relay/authorize. This is the verb an agent runs to put content on a screen.
func runDisplayPublish(args []string) {
	const usage = "Usage: hearth display publish <url> --target <screen> [--type url|image|video] [--ttl <seconds>]\n" +
		"       hearth display publish --target <screen> --type markdown --file <path> [--ttl <seconds>]"
	// An optional leading positional: the content URL for url/image/video.
	// Markdown carries no URL — it comes from --file instead.
	var content string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		content = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("display publish", flag.ExitOnError)
	target := fs.String("target", "", "screen name or id to publish to")
	ctype := fs.String("type", "url", "content type: url | image | video | markdown")
	file := fs.String("file", "", "path to a markdown file (required for --type markdown)")
	ttl := fs.Int("ttl", 0, "seconds until the content expires (0 = until cleared)")
	fs.Parse(args)
	if *target == "" {
		fmt.Fprintln(os.Stderr, "hearth: --target <screen> required\n"+usage)
		os.Exit(1)
	}

	payload := map[string]interface{}{"target": *target, "content_type": *ctype, "ttl_seconds": *ttl}
	switch *ctype {
	case "url", "image", "video":
		if content == "" {
			fmt.Fprintln(os.Stderr, "hearth: a content URL is required\n"+usage)
			os.Exit(1)
		}
		payload["url"] = content
	case "markdown":
		if *file == "" {
			fmt.Fprintln(os.Stderr, "hearth: --file <path> is required for --type markdown")
			os.Exit(1)
		}
		md, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: read %s: %v\n", *file, err)
			os.Exit(1)
		}
		payload["markdown"] = string(md)
	default:
		fmt.Fprintln(os.Stderr, "hearth: --type must be url, image, video, or markdown")
		os.Exit(1)
	}
	sendDisplayRequest("display_publish", payload, "published to "+*target)
}

// runDisplayQuery asks the relay what's currently on a named screen — the read
// side of the publish/clear surface. The relay answers from the push-cache the
// screen's display server reports into, so this needs no round trip to the box.
func runDisplayQuery(args []string) {
	fs := flag.NewFlagSet("display query", flag.ExitOnError)
	target := fs.String("target", "", "screen name or id to query")
	fs.Parse(args)
	if *target == "" {
		fmt.Fprintln(os.Stderr, "Usage: hearth display query --target <screen>")
		os.Exit(1)
	}
	data, err := sendWSRequest("display_query", map[string]interface{}{"target": *target})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	var resp struct {
		Error     string `json:"error"`
		Online    bool   `json:"online"`
		Kind      string `json:"kind"`
		Payload   string `json:"payload"`
		ExpiresAt string `json:"expires_at"`
		Expired   bool   `json:"expired"`
		Viewport  *struct {
			W           int     `json:"w"`
			H           int     `json:"h"`
			DPR         float64 `json:"dpr"`
			Orientation string  `json:"orientation"`
		} `json:"viewport"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "hearth: %s\n", resp.Error)
		os.Exit(1)
	}
	status := "offline"
	if resp.Online {
		status = "online"
	}
	switch {
	case resp.Expired:
		fmt.Printf("%s (%s): nothing showing (last content expired)\n", *target, status)
	case resp.Kind == "" || resp.Kind == "unknown":
		fmt.Printf("%s (%s): nothing showing\n", *target, status)
	default:
		line := fmt.Sprintf("%s (%s): %s %s", *target, status, resp.Kind, resp.Payload)
		if resp.ExpiresAt != "" {
			line += " (expires " + resp.ExpiresAt + ")"
		}
		fmt.Println(line)
	}
	// The screen's window size, when a kiosk has reported it — tune published content
	// (font scale, layout density, image resolution, aspect) to it. Absent = unknown
	// (no browser has connected yet); render responsively rather than assuming.
	if resp.Viewport != nil {
		fmt.Printf("  screen size: %d×%d px, %gx density (%s)\n",
			resp.Viewport.W, resp.Viewport.H, resp.Viewport.DPR, resp.Viewport.Orientation)
	}
}

// sendDisplayRequest sends a display ws_request through the daemon (as the calling
// agent/human principal); the relay authorizes and routes it to the screen's
// serving display server. App-level failures ride the reply's "error" field.
func sendDisplayRequest(msgType string, payload map[string]interface{}, okMsg string) {
	data, err := sendWSRequest(msgType, payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "hearth: %s\n", resp.Error)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, okMsg)
}

func sendDisplayControl(cmd controlCommand) {
	conn, err := net.DialTimeout("unix", displayControlSockPath(), 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth display: no local display server running (%v)\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	b, _ := json.Marshal(cmd)
	_, _ = conn.Write(append(b, '\n'))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, _ := bufio.NewReader(conn).ReadBytes('\n')
	var reply controlReply
	_ = json.Unmarshal(line, &reply)
	if reply.Error != "" {
		fmt.Fprintf(os.Stderr, "hearth display: %s\n", reply.Error)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "ok")
}
