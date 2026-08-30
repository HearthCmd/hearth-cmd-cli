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

// withVersion overrides the build-stamp `version` global for a test.
func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}

// pointWSAt sets wsURL so serverBaseURL() resolves to the httptest server's
// origin (ws://host → http://host in serverBaseURL).
func pointWSAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	withWSURL(t, "ws://"+host+"/ws/relay")
}

func TestCheckServerVersion_NudgesWhenBehind(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Hearth-Client")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          true,
			"recommended": "2.4.0",
			"update_url":  "https://example.test/dl",
		})
	}))
	defer srv.Close()
	withVersion(t, "2.3.0")
	pointWSAt(t, srv)

	n := checkServerVersion(3 * time.Second)
	if n == nil {
		t.Fatal("expected a nudge when the server recommends a newer version")
	}
	if n.Recommended != "2.4.0" || n.UpdateURL != "https://example.test/dl" {
		t.Fatalf("unexpected nudge: %+v", n)
	}
	if gotHeader != "cli/2.3.0" {
		t.Fatalf("X-Hearth-Client = %q, want cli/2.3.0", gotHeader)
	}
	if !strings.Contains(n.summary(), "2.3.0 → 2.4.0") {
		t.Fatalf("summary missing version transition: %q", n.summary())
	}
}

func TestCheckServerVersion_SilentWhenUpToDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()
	withVersion(t, "2.4.0")
	pointWSAt(t, srv)

	if n := checkServerVersion(3 * time.Second); n != nil {
		t.Fatalf("expected no nudge when up to date, got %+v", n)
	}
}

func TestCheckServerVersion_IgnoresHardGate426(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The MIN gate. The soft nudge must NOT act on it — the blocking
		// paths (WS dial / authed calls) own that.
		w.WriteHeader(http.StatusUpgradeRequired)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "client_outdated", "min_version": "3.0.0", "your_version": "2.3.0",
		})
	}))
	defer srv.Close()
	withVersion(t, "2.3.0")
	pointWSAt(t, srv)

	if n := checkServerVersion(3 * time.Second); n != nil {
		t.Fatalf("expected 426 to be ignored by the soft nudge, got %+v", n)
	}
}

func TestCheckServerVersion_SkipsDevBuild(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "recommended": "2.4.0"})
	}))
	defer srv.Close()
	withVersion(t, "dev")
	pointWSAt(t, srv)

	if n := checkServerVersion(3 * time.Second); n != nil {
		t.Fatalf("expected dev build to skip the nudge, got %+v", n)
	}
	if hit {
		t.Fatal("dev build must not even hit the server")
	}
}
