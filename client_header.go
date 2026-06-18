//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// X-Hearth-Client wiring for the version handshake.
// Spec: hearth-cmd/docs/forward-compat-version-handshake.md.
//
// Every HTTP call we make to the server carries `X-Hearth-Client: cli/<ver>`.
// The daemon WS dial URL adds `client_kind=cli&client_version=<ver>` query
// params. If the server returns HTTP 426 or closes the WS with custom
// code 4426, we print a clear message pointing the user at `hearth update`
// and exit.

const clientKindCLI = "cli"

// clientHeaderValue returns the value to send in `X-Hearth-Client`. The
// build-time `version` global may be empty in dev builds — fall back to
// "0.0.0" so the server has a parseable semver to compare against.
func clientHeaderValue() string {
	v := strings.TrimSpace(version)
	if v == "" {
		v = "0.0.0"
	}
	return clientKindCLI + "/" + v
}

func clientVersionValue() string {
	v := strings.TrimSpace(version)
	if v == "" {
		v = "0.0.0"
	}
	return v
}

// addClientHeader sets X-Hearth-Client on req. Use on every outbound
// HTTP request.
func addClientHeader(req *http.Request) {
	req.Header.Set("X-Hearth-Client", clientHeaderValue())
}

// addClientQuery adds client_kind + client_version to a URL's query
// string. Use on every outbound WS dial URL.
func addClientQuery(q url.Values) {
	q.Set("client_kind", clientKindCLI)
	q.Set("client_version", clientVersionValue())
}

// outdatedBody is the JSON shape the server returns on 426 and sends as
// a text frame before closing a WS with code 4426.
type outdatedBody struct {
	Error       string `json:"error"`
	MinVersion  string `json:"min_version"`
	YourVersion string `json:"your_version"`
	UpdateURL   string `json:"update_url"`
}

// printOutdatedAndExit prints a friendly message and terminates the
// process. Called from any code path that observes a 426 response or a
// WS close with code 4426. Never returns.
func printOutdatedAndExit(b outdatedBody) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "hearth-cmd-cli is too old to talk to this server.")
	if b.YourVersion != "" {
		fmt.Fprintf(os.Stderr, "  this version: %s\n", b.YourVersion)
	}
	if b.MinVersion != "" {
		fmt.Fprintf(os.Stderr, "  required min: %s\n", b.MinVersion)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Run `hearth update` to install the latest release.")
	if b.UpdateURL != "" {
		fmt.Fprintf(os.Stderr, "Or download manually: %s\n", b.UpdateURL)
	}
	os.Exit(2)
}

// checkOutdated inspects an HTTP response. If it's a 426 it decodes the
// body, prints, and exits the process. Otherwise it's a no-op. Caller
// must not have read the body yet.
func checkOutdated(resp *http.Response) {
	if resp.StatusCode != http.StatusUpgradeRequired {
		return
	}
	var body outdatedBody
	_ = json.NewDecoder(resp.Body).Decode(&body)
	printOutdatedAndExit(body)
}

// dialErrorIs426 is a best-effort check for nhooyr.io/websocket dial
// failures caused by the server returning 426 during the upgrade. The
// library's error text includes the HTTP status code.
func dialErrorIs426(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "426")
}

// printAuthBrokenAndExit drops the terminal back out of the alt screen
// (bubble tea is mid-Program when we hit this from talk), prints a
// short friendly message naming the fix, and exits with status 1. The
// exit-from-Program path doesn't get to run cleanly — we're called
// from a background goroutine — so we issue the alt-screen-leave
// escape directly to stderr before printing.
func printAuthBrokenAndExit() {
	// CSI ?1049l = leave alt screen. Restores the normal scrollback
	// so the user actually SEES the message rather than having it
	// painted into the to-be-discarded alt buffer.
	fmt.Fprint(os.Stderr, "\x1b[?1049l")
	fmt.Fprintln(os.Stderr, "hearth: this device's credentials are no longer accepted by the server.")
	fmt.Fprintln(os.Stderr, "        run `hearth login` to re-enroll, then try again.")
	os.Exit(1)
}

// dialErrorIs403 is the same best-effort check for a 403 Forbidden on
// the upgrade — the server's response when validateIODeviceSecret
// rejects the io_device_id/secret pair. Treated as a hard auth-broken
// signal rather than retried indefinitely (no amount of backoff fixes
// a stale credential).
func dialErrorIs403(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "403")
}
