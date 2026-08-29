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

// displayServer holds the LAN-served content for the (single, for 3a) screen on
// this box and the set of live browser connections to wake on a change.
type displayServer struct {
	mu          sync.Mutex
	ambient     screenAssignment
	published   screenAssignment
	pairingCode string // non-empty while the screen is unclaimed: show this code
	subs        map[chan struct{}]struct{}
	// relayTx is the link display_state reports go out on. In standalone mode it's
	// relayWS below; in unified mode the daemon sets it to its own DaemonWS.
	relayTx displayTransport
	relayWS *WSClient // standalone-owned /ws/daemon client (nil in unified mode)
}

func newDisplayServer() *displayServer {
	return &displayServer{subs: make(map[chan struct{}]struct{})}
}

func (d *displayServer) current() screenAssignment {
	d.mu.Lock()
	defer d.mu.Unlock()
	// While unclaimed, the screen shows its pairing code and nothing else.
	if d.pairingCode != "" {
		return screenAssignment{Kind: "pairing", Payload: d.pairingCode}
	}
	return effectiveContent(d.ambient, d.published, time.Now())
}

// setPublished swaps the published layer and wakes every live screen. The local
// `hearth display show` path and, later, the relay display.publish route both
// land here.
func (d *displayServer) setPublished(a screenAssignment) {
	d.mu.Lock()
	d.published = a
	d.mu.Unlock()
	d.wakeAll()
}

// setPairing shows a pairing code on the screen (non-empty) or returns to normal
// content (""). Set while the screen is unclaimed; cleared once a phone claims it.
func (d *displayServer) setPairing(code string) {
	d.mu.Lock()
	d.pairingCode = code
	d.mu.Unlock()
	d.wakeAll()
}

func (d *displayServer) clearPairing() { d.setPairing("") }

// wakeAll signals every live /ws/screen connection to re-send the current
// assignment. Non-blocking and coalesced (a pending wake covers a new one).
func (d *displayServer) wakeAll() {
	d.mu.Lock()
	subs := make([]chan struct{}, 0, len(d.subs))
	for ch := range d.subs {
		subs = append(subs, ch)
	}
	d.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default: // a pending wake already covers this one
		}
	}
	// Report the new content to the relay so display.query can answer from cache.
	d.reportState()
}

func (d *displayServer) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	d.mu.Lock()
	d.subs[ch] = struct{}{}
	d.mu.Unlock()
	return ch
}

func (d *displayServer) unsubscribe(ch chan struct{}) {
	d.mu.Lock()
	delete(d.subs, ch)
	d.mu.Unlock()
}

// handleScreenWS serves /ws/screen: on connect it sends the complete current
// assignment (reconstruct-on-connect), then re-sends on every content change and
// on a heartbeat (which also lets an expired TTL fall back to ambient).
func (d *displayServer) handleScreenWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusInternalError, "closing")
	// CloseRead drains incoming frames (handling pings/close) and gives us a
	// context cancelled when the browser goes away — this server only writes.
	ctx := conn.CloseRead(r.Context())

	send := func() error {
		cur := d.current()
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

	ch := d.subscribe()
	defer d.unsubscribe(ch)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "bye")
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

	// Pair the screen if it hasn't been claimed yet: show a code on the panel and
	// poll until a phone claims it. Non-fatal — if pairing can't start we still
	// serve (the screen just isn't a household entity yet).
	if readConfigValue("display_claimed") != "1" {
		provisionAndPairScreen(d)
	}

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
	// (via wakeAll) cover immediacy; this covers reconnect + TTL expiry.
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

// applyControl mutates the served content per a control command. Kept separate
// from the socket transport so it can be unit-tested directly.
func (d *displayServer) applyControl(cmd controlCommand) error {
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
		d.setPublished(a)
		return nil
	case "clear":
		d.setPublished(screenAssignment{}) // empty published → falls to ambient
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
