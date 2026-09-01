//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestParseSubprotocols(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"hearth.screen.v1", []string{"hearth.screen.v1"}},
		{"hearth.screen.v1, kitchen , sek", []string{"hearth.screen.v1", "kitchen", "sek"}},
	}
	for _, c := range cases {
		got := parseSubprotocols(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("parseSubprotocols(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("parseSubprotocols(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// The per-IP limiter allows up to max in the window, blocks the next, gives each IP
// its own budget, and refills after the window.
func TestScreenPairLimiter(t *testing.T) {
	l := newScreenPairLimiter()
	now := time.Now()
	for i := 0; i < screenPairPerIPMax; i++ {
		if !l.allow("1.2.3.4", now) {
			t.Fatalf("request %d within the limit should be allowed", i)
		}
	}
	if l.allow("1.2.3.4", now) {
		t.Fatal("the request past the limit should be blocked")
	}
	if !l.allow("5.6.7.8", now) {
		t.Fatal("a different IP has its own budget")
	}
	if !l.allow("1.2.3.4", now.Add(2*screenPairPerIPWindow)) {
		t.Fatal("after the window the IP is allowed again")
	}
}

// A credentialed /ws/screen connect validates against the pushed set: correct
// (id, secret) via subprotocol streams; a wrong secret is closed with a policy
// violation.
func TestScreenWS_CredentialedConnect(t *testing.T) {
	d := newDisplayServer()
	d.applyDisplayScreens([]displayScreenInfo{{ScreenID: "kitchen", SecretHash: sha256Hex([]byte("sek"))}})
	srv := httptest.NewServer(http.HandlerFunc(d.handleScreenWS))
	defer srv.Close()
	wsURLStr := "ws" + strings.TrimPrefix(srv.URL, "http")

	// Correct credential → handshake completes and an assignment arrives.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	good, _, err := websocket.Dial(ctx, wsURLStr, &websocket.DialOptions{
		Subprotocols: []string{screenSubprotocol, "kitchen", "sek"},
	})
	if err != nil {
		t.Fatalf("valid credential should connect: %v", err)
	}
	if _, _, rerr := good.Read(ctx); rerr != nil {
		t.Fatalf("valid screen should receive an assignment, got %v", rerr)
	}
	good.Close(websocket.StatusNormalClosure, "done")

	// Wrong secret → handshake completes but the server closes it as a policy
	// violation on the first read.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	bad, _, err := websocket.Dial(ctx2, wsURLStr, &websocket.DialOptions{
		Subprotocols: []string{screenSubprotocol, "kitchen", "wrong"},
	})
	if err != nil {
		t.Fatalf("dial should complete the handshake even for a bad credential: %v", err)
	}
	_, _, rerr := bad.Read(ctx2)
	if websocket.CloseStatus(rerr) != websocket.StatusPolicyViolation {
		t.Fatalf("bad credential should be closed with policy violation, got %v", rerr)
	}
}

// The thin stateless forward (§B7): the BROWSER mints its own id + secret and posts
// them; /screen/pair hashes the secret for the relay's /pair/start (never storing it)
// and returns the code; /screen/pair/poll forwards (id, code) and returns only the
// status. The display server holds no pairing state and does NOT self-add to `known`
// (the relay's display_screens push is authoritative for that).
func TestScreenPair_StatelessForward(t *testing.T) {
	var gotStart struct {
		IODeviceID string `json:"io_device_id"`
		SecretHash string `json:"secret_hash"`
		IsTemp     bool   `json:"is_temp"`
	}
	var gotPollID, gotPollCode string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/pair/start":
			_ = json.NewDecoder(r.Body).Decode(&gotStart)
			_, _ = w.Write([]byte(`{"code":"424242"}`))
		case "/pair/poll":
			var pb struct {
				IODeviceID string `json:"io_device_id"`
				Code       string `json:"code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&pb)
			gotPollID, gotPollCode = pb.IODeviceID, pb.Code
			_, _ = w.Write([]byte(`{"status":"claimed"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer relay.Close()

	oldWS := wsURL
	wsURL = "ws" + strings.TrimPrefix(relay.URL, "http") + "/ws/relay"
	defer func() { wsURL = oldWS }()

	withFakeHome(t)
	if err := writeConfigValue("host_id", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigValue("host_secret", "s1"); err != nil {
		t.Fatal(err)
	}

	d := newDisplayServer()

	const screenID = "browser-screen-1"
	const secret = "browser-minted-secret"

	// POST /screen/pair with the browser-minted id + secret (temp display).
	body := `{"io_device_id":"` + screenID + `","secret":"` + secret + `","is_temp":true}`
	pw := httptest.NewRecorder()
	d.handleScreenPair(pw, httptest.NewRequest("POST", "/screen/pair", strings.NewReader(body)))
	var pairResp struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(pw.Body.Bytes(), &pairResp); err != nil {
		t.Fatal(err)
	}
	if pairResp.Error != "" || pairResp.Code != "424242" {
		t.Fatalf("pair resp = %+v, want code 424242", pairResp)
	}
	// The relay's /pair/start got the browser's id, the HASH of its secret (never the
	// secret itself), and is_temp.
	if gotStart.IODeviceID != screenID || gotStart.SecretHash != sha256Hex([]byte(secret)) || !gotStart.IsTemp {
		t.Fatalf("pair/start received %+v, want id=%s hash=sha256(secret) temp=true", gotStart, screenID)
	}

	// GET /screen/pair/poll forwards (id, code) and returns only the status.
	qw := httptest.NewRecorder()
	d.handleScreenPairPoll(qw, httptest.NewRequest("GET", "/screen/pair/poll?screen_id="+screenID+"&code=424242", nil))
	var pollResp map[string]interface{}
	if err := json.Unmarshal(qw.Body.Bytes(), &pollResp); err != nil {
		t.Fatal(err)
	}
	if pollResp["status"] != "claimed" {
		t.Fatalf("poll resp = %v, want claimed", pollResp)
	}
	if _, leaked := pollResp["secret"]; leaked {
		t.Fatal("a stateless poll must not return a secret")
	}
	if gotPollID != screenID || gotPollCode != "424242" {
		t.Fatalf("relay poll received (%s,%s), want (%s,424242)", gotPollID, gotPollCode, screenID)
	}

	// No pairing state, and no self-add — `known` is populated only by the relay push.
	if _, ok := d.knownScreen(screenID); ok {
		t.Fatal("stateless pairing must not self-add to known")
	}
}
