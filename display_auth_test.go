//go:build darwin || linux

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A display_screens push caches the bound set and drives per-screen credential
// validation: a matching (id, secret) is accepted, a wrong secret / unknown screen /
// blank-hash row is not.
func TestApplyDisplayScreens_CacheAndValidate(t *testing.T) {
	d := newDisplayServer()
	d.applyDisplayScreens([]displayScreenInfo{
		{ScreenID: "kitchen", SecretHash: sha256Hex([]byte("k-secret")), Name: "Kitchen", IsTemp: false},
		{ScreenID: "laptop", SecretHash: sha256Hex([]byte("l-secret")), Name: "Laptop", IsTemp: true},
		{ScreenID: "endpoint", SecretHash: "", Name: "Endpoint"}, // secret-less row: unauthenticable
	})

	if cred, ok := d.knownScreen("laptop"); !ok || !cred.IsTemp || cred.Name != "Laptop" {
		t.Fatalf("knownScreen(laptop) = %+v ok=%v, want temp Laptop", cred, ok)
	}
	if !d.validScreenCredential("kitchen", "k-secret") {
		t.Fatal("correct kitchen secret should validate")
	}
	if d.validScreenCredential("kitchen", "wrong") {
		t.Fatal("wrong secret must not validate")
	}
	if d.validScreenCredential("unknown", "whatever") {
		t.Fatal("unknown screen must not validate")
	}
	if d.validScreenCredential("endpoint", "") || d.validScreenCredential("endpoint", "anything") {
		t.Fatal("a blank-secret_hash screen can never authenticate")
	}
	if d.validScreenCredential("kitchen", "") {
		t.Fatal("empty secret must not validate")
	}
}

// A screen that drops out of a fresh push is evicted (its eviction channel closes),
// while a screen still present is not; an empty push evicts everything.
func TestApplyDisplayScreens_EvictsDroppedScreen(t *testing.T) {
	d := newDisplayServer()
	evK := d.subscribeEvict("kitchen")
	evO := d.subscribeEvict("office")

	// Both present — neither evicted.
	d.applyDisplayScreens([]displayScreenInfo{
		{ScreenID: "kitchen", SecretHash: "h1"},
		{ScreenID: "office", SecretHash: "h2"},
	})
	assertOpen(t, evK, "kitchen while present")
	assertOpen(t, evO, "office while present")

	// Drop office only.
	d.applyDisplayScreens([]displayScreenInfo{{ScreenID: "kitchen", SecretHash: "h1"}})
	assertOpen(t, evK, "kitchen still present")
	assertClosed(t, evO, "office dropped")

	// Empty push evicts the rest.
	d.applyDisplayScreens(nil)
	assertClosed(t, evK, "kitchen dropped by empty push")
}

// The relay's display_screens frame is consumed by handleRelayFrame and populates
// the cache.
func TestHandleRelayFrame_DisplayScreens(t *testing.T) {
	d := newDisplayServer()
	frame := `{"type":"display_screens","screens":[{"screen_id":"s1","secret_hash":"` + sha256Hex([]byte("x")) + `","name":"S1","is_temp":true}]}`
	if !d.handleRelayFrame([]byte(frame)) {
		t.Fatal("display_screens frame should be consumed")
	}
	if cred, ok := d.knownScreen("s1"); !ok || !cred.IsTemp {
		t.Fatalf("s1 not cached from frame: %+v ok=%v", cred, ok)
	}
	if !d.validScreenCredential("s1", "x") {
		t.Fatal("credential from the pushed frame should validate")
	}
}

// The /ws/screen CSWSH guard (§B3): a handshake from a foreign Origin is rejected
// at the handshake (a same-origin one completes — the credential is what gates it).
func TestScreenWS_RejectsCrossOrigin(t *testing.T) {
	d := newDisplayServer()
	srv := httptest.NewServer(http.HandlerFunc(d.handleScreenWS))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	// Foreign Origin → rejected at the handshake.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	badConn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://evil.example"}},
	})
	if err == nil {
		badConn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("a cross-origin /ws/screen handshake must be rejected")
	}

	// No Origin header (a Go client / same-host page) → the handshake itself completes;
	// the credential check (below) is the gate, not the origin.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	okConn, _, err := websocket.Dial(ctx2, wsURL, nil)
	if err != nil {
		t.Fatalf("same-origin handshake should complete, got %v", err)
	}
	okConn.Close(websocket.StatusNormalClosure, "done")
}

// §B4b: every /ws/screen viewer must present a valid credential. An uncredentialed
// connection completes the handshake but is closed with a policy violation.
func TestScreenWS_RejectsUncredentialed(t *testing.T) {
	d := newDisplayServer()
	srv := httptest.NewServer(http.HandlerFunc(d.handleScreenWS))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil) // no subprotocol → no credential
	if err != nil {
		t.Fatalf("handshake should complete, got %v", err)
	}
	if _, _, rerr := conn.Read(ctx); websocket.CloseStatus(rerr) != websocket.StatusPolicyViolation {
		t.Fatalf("an uncredentialed viewer must be closed with a policy violation, got %v", rerr)
	}
}

func assertClosed(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch: // a closed channel receives immediately
	case <-time.After(time.Second):
		t.Fatalf("%s: expected eviction (channel closed), got none", what)
	}
}

func assertOpen(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s: channel was closed/evicted unexpectedly", what)
	default:
	}
}
