package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
)

// maxDisplayPortProbes bounds the upward port scan when the preferred display bind
// port is already taken (another display daemon sharing this box). Enough headroom
// for a handful of households on one machine without turning a real misconfiguration
// (nothing can bind at all) into an unbounded scan.
const maxDisplayPortProbes = 20

// listenDisplayLAN binds the display server's LAN socket, hunting upward from the
// requested port when it is already in use — so several display daemons can share one
// box without hand-assigned ports (docs/household-display-plan.md §1). It returns the
// bound listener and the address actually bound; the daemon persists that back into
// `display_bind` so the port stays stable across restarts (a moved port would break
// the URL already-paired browsers point at).
//
// Binding synchronously here (rather than letting http.ListenAndServe bind inside a
// goroutine) is also what makes a collision catchable: previously the LAN bind error
// surfaced only as an async stderr line and never reached the daemon's displayError.
// A non-"address in use" failure (bad host, permission) is returned as-is — we hunt
// past a busy port, not past a genuine misconfiguration.
func listenDisplayLAN(bind string) (net.Listener, string, error) {
	host, portStr, err := net.SplitHostPort(bind)
	if err != nil {
		return nil, "", fmt.Errorf("invalid display_bind %q: %w", bind, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, "", fmt.Errorf("invalid display_bind port %q in %q: %w", portStr, bind, err)
	}

	probes := maxDisplayPortProbes
	if port == 0 {
		probes = 1 // port 0 = OS-assigned; a single bind, nothing to hunt
	}

	var lastErr error
	for i := 0; i < probes; i++ {
		p := port + i
		addr := net.JoinHostPort(host, strconv.Itoa(p))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			bound := addr
			if p == 0 {
				// Recover the OS-assigned port so the advertised address is concrete.
				if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
					bound = net.JoinHostPort(host, strconv.Itoa(tcp.Port))
				}
			}
			return ln, bound, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, "", err // real failure (perms, bad host) — don't paper over it
		}
		lastErr = err
	}
	return nil, "", fmt.Errorf("no free display port in %d..%d (all in use): %w", port, port+probes-1, lastErr)
}

// displayPairingURLs returns browser-loadable http URLs for reaching the display
// server bound at bind — one per detected private LAN IPv4 address. The configured
// bind host is usually 0.0.0.0 (all interfaces), which a browser cannot load; these
// are the concrete addresses a phone or wall tablet on the same network types in to
// pair a new screen. An explicit, non-wildcard bind host is already reachable and is
// used verbatim.
func displayPairingURLs(bind string) []string {
	host, portStr, err := net.SplitHostPort(bind)
	if err != nil {
		return nil
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{fmt.Sprintf("http://%s/", net.JoinHostPort(host, portStr))}
	}
	var urls []string
	for _, ip := range privateLANIPv4s() {
		urls = append(urls, fmt.Sprintf("http://%s/", net.JoinHostPort(ip, portStr)))
	}
	return urls
}

// privateLANIPv4s returns this host's private (RFC1918) IPv4 addresses — the ones a
// browser on the same LAN can reach. Loopback and link-local are excluded. Order
// follows the OS's interface enumeration.
func privateLANIPv4s() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		ip4 := ip.To4()
		if ip4 == nil || !ip4.IsPrivate() || ip4.IsLoopback() {
			continue
		}
		out = append(out, ip4.String())
	}
	return out
}
