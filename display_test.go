//go:build darwin || linux

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEffectiveContent(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	ambient := screenAssignment{Kind: "url", Payload: "ambient"}
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	cases := []struct {
		name      string
		ambient   screenAssignment
		published screenAssignment
		want      string // Payload of the expected effective assignment ("" for empty)
	}{
		{"nothing published falls to ambient", ambient, screenAssignment{}, "ambient"},
		{"live published wins", ambient, screenAssignment{Kind: "url", Payload: "recipe", ExpiresAt: future}, "recipe"},
		{"expired published falls to ambient", ambient, screenAssignment{Kind: "url", Payload: "recipe", ExpiresAt: past}, "ambient"},
		{"published with no expiry wins", ambient, screenAssignment{Kind: "url", Payload: "pinned"}, "pinned"},
		{"both empty stays empty", screenAssignment{}, screenAssignment{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectiveContent(c.ambient, c.published, now)
			if got.Payload != c.want {
				t.Fatalf("effectiveContent(...).Payload = %q, want %q", got.Payload, c.want)
			}
		})
	}
}

// setPublished swaps the visible layer; current() reflects it immediately, and
// an expired publish falls back to ambient without an explicit clear.
func TestDisplayServerPublishAndExpiry(t *testing.T) {
	d := newDisplayServer()
	d.ambient = screenAssignment{Kind: "url", Payload: "art"}

	if got := d.current(); got.Payload != "art" {
		t.Fatalf("initial current = %q, want ambient 'art'", got.Payload)
	}

	d.setPublished(screenAssignment{Kind: "url", Payload: "timer", ExpiresAt: time.Now().Add(time.Hour)})
	if got := d.current(); got.Payload != "timer" {
		t.Fatalf("after publish current = %q, want 'timer'", got.Payload)
	}

	d.setPublished(screenAssignment{Kind: "url", Payload: "stale", ExpiresAt: time.Now().Add(-time.Minute)})
	if got := d.current(); got.Payload != "art" {
		t.Fatalf("after expired publish current = %q, want ambient 'art'", got.Payload)
	}
}

// applyControl drives the served content: show publishes, clear falls back to
// ambient, a bad command errors and changes nothing.
func TestApplyControl(t *testing.T) {
	d := newDisplayServer()
	d.ambient = screenAssignment{Kind: "url", Payload: "art"}

	if err := d.applyControl(controlCommand{Cmd: "show", URL: "recipe"}); err != nil {
		t.Fatal(err)
	}
	if got := d.current(); got.Payload != "recipe" {
		t.Fatalf("after show current = %q, want recipe", got.Payload)
	}

	if err := d.applyControl(controlCommand{Cmd: "clear"}); err != nil {
		t.Fatal(err)
	}
	if got := d.current(); got.Payload != "art" {
		t.Fatalf("after clear current = %q, want ambient art", got.Payload)
	}

	if err := d.applyControl(controlCommand{Cmd: "show"}); err == nil {
		t.Fatal("show with no url should error")
	}
	if err := d.applyControl(controlCommand{Cmd: "bogus"}); err == nil {
		t.Fatal("unknown command should error")
	}

	if err := d.applyControl(controlCommand{Cmd: "show", URL: "timer", TTLSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	if got := d.current(); got.Payload != "timer" || got.ExpiresAt.IsZero() {
		t.Fatalf("ttl show current = %+v, want timer with a non-zero expiry", got)
	}
}

// While unclaimed the screen shows its pairing code, overriding ambient/published;
// clearing it returns to normal content.
func TestCurrentShowsPairingCode(t *testing.T) {
	d := newDisplayServer()
	d.ambient = screenAssignment{Kind: "url", Payload: "art"}

	d.setPairing("654321")
	if got := d.current(); got.Kind != "pairing" || got.Payload != "654321" {
		t.Fatalf("current while pairing = %+v, want pairing 654321", got)
	}

	d.clearPairing()
	if got := d.current(); got.Payload != "art" {
		t.Fatalf("current after clearPairing = %q, want ambient art", got.Payload)
	}
}

func TestRandomSecret(t *testing.T) {
	a, b := randomSecret(), randomSecret()
	if len(a) != 64 {
		t.Fatalf("randomSecret len = %d, want 64 hex chars", len(a))
	}
	if a == b {
		t.Fatal("two randomSecrets should differ")
	}
}

// startDisplayPairing authenticates as the host (Bearer + ?host_id=) and sends
// form_factor=display, then returns the minted code.
func TestStartDisplayPairing(t *testing.T) {
	var gotHostID, gotAuth, gotFormFactor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHostID = r.URL.Query().Get("host_id")
		gotAuth = r.Header.Get("Authorization")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotFormFactor = body["form_factor"]
		_, _ = w.Write([]byte(`{"code":"123456"}`))
	}))
	defer srv.Close()

	code, err := startDisplayPairing(srv.URL, "host-1", "hsec", "screen-1", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if code != "123456" {
		t.Fatalf("code = %q, want 123456", code)
	}
	if gotHostID != "host-1" {
		t.Errorf("host_id query = %q, want host-1", gotHostID)
	}
	if gotAuth != "Bearer hsec" {
		t.Errorf("Authorization = %q, want Bearer hsec", gotAuth)
	}
	if gotFormFactor != "display" {
		t.Errorf("form_factor = %q, want display", gotFormFactor)
	}
}

// handleRelayFrame applies display_publish/clear frames routed from the relay and
// ignores everything else.
func TestHandleRelayFrame(t *testing.T) {
	d := newDisplayServer()
	d.ambient = screenAssignment{Kind: "url", Payload: "art"}

	if !d.handleRelayFrame([]byte(`{"type":"display_publish","cmd":"show","url":"recipe","ttl_seconds":3600}`)) {
		t.Fatal("display_publish should be consumed")
	}
	if got := d.current(); got.Payload != "recipe" {
		t.Fatalf("after publish current = %q, want recipe", got.Payload)
	}

	if !d.handleRelayFrame([]byte(`{"type":"display_clear","cmd":"clear"}`)) {
		t.Fatal("display_clear should be consumed")
	}
	if got := d.current(); got.Payload != "art" {
		t.Fatalf("after clear current = %q, want ambient art", got.Payload)
	}

	if d.handleRelayFrame([]byte(`{"type":"organizations_list"}`)) {
		t.Fatal("a non-display frame must not be consumed")
	}
	if d.handleRelayFrame([]byte(`not json`)) {
		t.Fatal("garbage must not be consumed")
	}
}

// applyControl tags the assignment with the content type (image/video), defaults
// a blank kind to url, and rejects an unknown kind.
func TestApplyControl_ContentTypes(t *testing.T) {
	d := newDisplayServer()

	if err := d.applyControl(controlCommand{Cmd: "show", Kind: "image", URL: "http://x/pic.jpg"}); err != nil {
		t.Fatal(err)
	}
	if got := d.current(); got.Kind != "image" || got.Payload != "http://x/pic.jpg" {
		t.Fatalf("image current = %+v, want image http://x/pic.jpg", got)
	}

	if err := d.applyControl(controlCommand{Cmd: "show", Kind: "video", URL: "http://x/clip.mp4"}); err != nil {
		t.Fatal(err)
	}
	if got := d.current(); got.Kind != "video" {
		t.Fatalf("video current kind = %q, want video", got.Kind)
	}

	// A blank kind defaults to url.
	if err := d.applyControl(controlCommand{Cmd: "show", URL: "http://x"}); err != nil {
		t.Fatal(err)
	}
	if got := d.current(); got.Kind != "url" {
		t.Fatalf("default kind = %q, want url", got.Kind)
	}

	if err := d.applyControl(controlCommand{Cmd: "show", Kind: "pdf", URL: "http://x"}); err == nil {
		t.Fatal("unknown content type should error")
	}
}

// A relay display_publish frame carrying a content type is applied with that kind.
func TestHandleRelayFrame_ContentType(t *testing.T) {
	d := newDisplayServer()
	if !d.handleRelayFrame([]byte(`{"type":"display_publish","cmd":"show","kind":"image","url":"pic.jpg"}`)) {
		t.Fatal("image display_publish should be consumed")
	}
	if got := d.current(); got.Kind != "image" || got.Payload != "pic.jpg" {
		t.Fatalf("image frame current = %+v, want image pic.jpg", got)
	}
}

// goldmark renders markdown to HTML with raw HTML escaped (no script injection
// from agent-authored content).
func TestRenderMarkdown(t *testing.T) {
	html, err := renderMarkdown("# Dinner\n\n- pasta\n- **sauce**\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h1", "Dinner", "<li>pasta", "<strong>sauce</strong>"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered markdown missing %q:\n%s", want, html)
		}
	}
	// Raw HTML in the source must be escaped, not passed through (WithUnsafe off).
	unsafe, err := renderMarkdown("<script>alert(1)</script>\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unsafe, "<script>") {
		t.Fatalf("raw <script> should be escaped, got:\n%s", unsafe)
	}
}

// applyControl renders markdown to HTML on the display server (relay forwards the
// raw source), tags the assignment markdown, and rejects empty content.
func TestApplyControl_Markdown(t *testing.T) {
	d := newDisplayServer()

	if err := d.applyControl(controlCommand{Cmd: "show", Kind: "markdown", Markdown: "# Hi\n"}); err != nil {
		t.Fatal(err)
	}
	got := d.current()
	if got.Kind != "markdown" {
		t.Fatalf("kind = %q, want markdown", got.Kind)
	}
	if !strings.Contains(got.Payload, "<h1") {
		t.Fatalf("payload should be rendered HTML, got %q", got.Payload)
	}

	if err := d.applyControl(controlCommand{Cmd: "show", Kind: "markdown"}); err == nil {
		t.Fatal("empty markdown should error")
	}
}

// A relay markdown frame is rendered to HTML by the display server.
func TestHandleRelayFrame_Markdown(t *testing.T) {
	d := newDisplayServer()
	if !d.handleRelayFrame([]byte(`{"type":"display_publish","cmd":"show","kind":"markdown","markdown":"## Note"}`)) {
		t.Fatal("markdown display_publish should be consumed")
	}
	if got := d.current(); got.Kind != "markdown" || !strings.Contains(got.Payload, "<h2") {
		t.Fatalf("markdown frame current = %+v, want rendered h2", got)
	}
}

// displayStateFrame nests kind/payload under `data` — the relay hands
// handleDisplayState the frame's `data` field, so a top-level payload is silently
// dropped (the bug this asserts against). expires_at is included only when set.
func TestDisplayStateFrame(t *testing.T) {
	f := displayStateFrame(screenAssignment{Kind: "markdown", Payload: "<h1>x</h1>"})
	if f["type"] != "display_state" {
		t.Fatalf("frame type = %v, want display_state", f["type"])
	}
	data, ok := f["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("frame payload must be under `data`, got %v", f)
	}
	if data["kind"] != "markdown" || data["payload"] != "<h1>x</h1>" {
		t.Fatalf("data = %v, want kind=markdown payload=<h1>x</h1>", data)
	}
	if _, ok := data["expires_at"]; ok {
		t.Fatal("no-expiry assignment should omit expires_at")
	}
	f2 := displayStateFrame(screenAssignment{Kind: "url", Payload: "u", ExpiresAt: time.Now().Add(time.Hour)})
	d2, _ := f2["data"].(map[string]interface{})
	if _, ok := d2["expires_at"]; !ok {
		t.Fatal("assignment with a TTL should set expires_at under data")
	}
}

// fakeTransport is a displayTransport for tests — records what was sent.
type fakeTransport struct {
	connected bool
	sent      [][]byte
}

func (f *fakeTransport) SendText(b []byte) { f.sent = append(f.sent, b) }
func (f *fakeTransport) IsConnected() bool { return f.connected }

// attachTransport reports immediately, and every content change reports again
// through the attached transport — the path the unified daemon drives.
func TestReportStateViaTransport(t *testing.T) {
	d := newDisplayServer()
	tx := &fakeTransport{connected: true}
	d.attachTransport(tx)
	if len(tx.sent) == 0 {
		t.Fatal("attachTransport should send an initial report")
	}

	before := len(tx.sent)
	d.setPublished(screenAssignment{Kind: "url", Payload: "x", ExpiresAt: time.Now().Add(time.Hour)})
	if len(tx.sent) <= before {
		t.Fatal("a content change should trigger a report")
	}
	var frame map[string]interface{}
	_ = json.Unmarshal(tx.sent[len(tx.sent)-1], &frame)
	data, _ := frame["data"].(map[string]interface{})
	if frame["type"] != "display_state" || data["payload"] != "x" {
		t.Fatalf("last report = %v, want display_state with data.payload x", frame)
	}
}

// A disconnected transport drops reports rather than queueing while offline.
func TestReportStateSkipsWhenDisconnected(t *testing.T) {
	d := newDisplayServer()
	tx := &fakeTransport{connected: false}
	d.attachTransport(tx)
	d.setPublished(screenAssignment{Kind: "url", Payload: "x"})
	if len(tx.sent) != 0 {
		t.Fatalf("disconnected transport should send nothing, got %d frames", len(tx.sent))
	}
}

// A subscriber is woken on a content change (non-blocking, coalesced).
func TestDisplayServerNotifiesSubscribers(t *testing.T) {
	d := newDisplayServer()
	ch := d.subscribe()
	defer d.unsubscribe(ch)

	d.setPublished(screenAssignment{Kind: "url", Payload: "x", ExpiresAt: time.Now().Add(time.Hour)})
	select {
	case <-ch:
		// woken as expected
	case <-time.After(time.Second):
		t.Fatal("subscriber was not notified of a content change")
	}
}
