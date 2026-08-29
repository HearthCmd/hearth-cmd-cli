package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// kioskBundle is the built kiosk front-end served to screens at "/". In v1 it's a
// single hand-written index.html; the Vite kiosk app (web-kiosk/, slice P4-2)
// builds into this same directory and its output replaces the placeholder. The
// dir is committed (like the interpose blobs) so the public CLI repo builds
// without a JS toolchain. `all:` embeds bundler-emitted dot/underscore assets too.
//
//go:embed all:kioskdist
var kioskBundle embed.FS

// kioskFS returns the bundle rooted so index.html is at "/".
func kioskFS() fs.FS {
	sub, err := fs.Sub(kioskBundle, "kioskdist")
	if err != nil {
		panic("kiosk bundle embed: " + err.Error()) // compiled in — unreachable
	}
	return sub
}

// kioskHandler serves the embedded bundle. Registered as the catch-all "/"; the
// specific /ws/screen and /healthz routes take precedence in the mux.
func kioskHandler() http.Handler {
	return http.FileServerFS(kioskFS())
}
