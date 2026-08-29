package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The embedded kiosk bundle serves index.html at "/" — the SPA mount point the
// kiosk app renders into. (The Vite build moves the socket logic into the JS
// bundle, so the mount div is the stable marker, not any runtime string.)
func TestKioskHandlerServesIndex(t *testing.T) {
	srv := httptest.NewServer(kioskHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `id="root"`) {
		t.Fatalf("kiosk index should carry the SPA mount div, got %d bytes without it", len(body))
	}
}

// The catch-all kiosk handler at "/" must not shadow the specific API routes —
// Go 1.22 mux precedence gives the more specific pattern the win.
func TestKioskMuxRoutePrecedence(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/", kioskHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("/healthz body = %q, want ok (specific route must beat the kiosk catch-all)", body)
	}

	root, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	root.Body.Close()
	if root.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200 (kiosk still served at root)", root.StatusCode)
	}
}
