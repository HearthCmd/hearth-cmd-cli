package main

import (
	"net"
	"strings"
	"testing"
)

// listenDisplayLAN hunts past a busy port so a second display daemon on the same box
// starts on the next free port instead of failing — the core of the multi-host
// graceful fallback.
func TestListenDisplayLAN_HuntsPastBusyPort(t *testing.T) {
	// Occupy a concrete port, then ask listenDisplayLAN to bind that exact address.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	defer busy.Close()
	occupied := busy.Addr().String()

	ln, actual, err := listenDisplayLAN(occupied)
	if err != nil {
		t.Fatalf("listenDisplayLAN(%s): %v", occupied, err)
	}
	defer ln.Close()
	if actual == occupied {
		t.Fatalf("bound the occupied port %s; expected to hunt to a free one", occupied)
	}
	if ln.Addr().String() != actual {
		t.Fatalf("returned addr %s != listener addr %s", actual, ln.Addr().String())
	}
}

// A port-0 bind lets the OS assign a port; the returned address is concrete (never
// ":0"), so it can be advertised for pairing.
func TestListenDisplayLAN_PortZeroResolvesConcrete(t *testing.T) {
	ln, actual, err := listenDisplayLAN("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenDisplayLAN: %v", err)
	}
	defer ln.Close()
	if strings.HasSuffix(actual, ":0") || !strings.HasPrefix(actual, "127.0.0.1:") {
		t.Fatalf("actual = %q, want a concrete 127.0.0.1:<port>", actual)
	}
}

func TestListenDisplayLAN_BadAddr(t *testing.T) {
	if _, _, err := listenDisplayLAN("not-an-address"); err == nil {
		t.Fatal("expected an error for a malformed bind address")
	}
}

// An explicit (non-wildcard) bind host is already reachable, so it's used verbatim.
func TestDisplayPairingURLs_ExplicitHostVerbatim(t *testing.T) {
	got := displayPairingURLs("192.168.7.5:8090")
	if len(got) != 1 || got[0] != "http://192.168.7.5:8090/" {
		t.Fatalf("got %v, want [http://192.168.7.5:8090/]", got)
	}
}

// A wildcard bind can't be loaded in a browser, so it expands to concrete per-
// interface URLs — each well-formed, carrying the port, never the wildcard host.
func TestDisplayPairingURLs_WildcardExpandsToConcrete(t *testing.T) {
	for _, u := range displayPairingURLs("0.0.0.0:8091") {
		if !strings.HasPrefix(u, "http://") || !strings.HasSuffix(u, ":8091/") {
			t.Fatalf("malformed pairing URL %q", u)
		}
		if strings.Contains(u, "0.0.0.0") {
			t.Fatalf("pairing URL still carries the wildcard host: %q", u)
		}
	}
}

// The host-level pairing block rides in display_state only once the server has bound
// (bindAddr/pairingURLs set), so the app can show which URL to open to add a screen.
func TestDisplayStateReport_IncludesPairingWhenBound(t *testing.T) {
	d := newDisplayServer()
	d.bindAddr = "0.0.0.0:8091"
	d.pairingURLs = []string{"http://192.168.7.5:8091/"}

	data := d.displayStateReport()["data"].(map[string]interface{})
	pairing, ok := data["pairing"].(map[string]interface{})
	if !ok {
		t.Fatalf("no pairing block in report: %v", data)
	}
	if pairing["bind"] != "0.0.0.0:8091" {
		t.Fatalf("pairing bind = %v, want 0.0.0.0:8091", pairing["bind"])
	}
	urls, _ := pairing["urls"].([]string)
	if len(urls) != 1 || urls[0] != "http://192.168.7.5:8091/" {
		t.Fatalf("pairing urls = %v", pairing["urls"])
	}
}

// A server that hasn't bound (no display role / never served) reports no pairing block
// — keeping the report a pure function of state and not advertising a phantom endpoint.
func TestDisplayStateReport_NoPairingWhenUnbound(t *testing.T) {
	d := newDisplayServer()
	data := d.displayStateReport()["data"].(map[string]interface{})
	if _, ok := data["pairing"]; ok {
		t.Fatalf("unexpected pairing block for an unbound server: %v", data)
	}
}
