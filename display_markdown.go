package main

import (
	"bytes"

	"github.com/yuin/goldmark"
)

// mdRenderer is the display server's markdown→HTML renderer (Phase 3, P3-2).
// Default goldmark = CommonMark with raw HTML ESCAPED (WithUnsafe is deliberately
// NOT set): agent-authored markdown can't inject a <script> or an <iframe> into
// the kiosk, so the rendered fragment is safe to mount as innerHTML. The display
// server renders; the relay only forwards the raw markdown (dumb router).
var mdRenderer = goldmark.New()

// renderMarkdown converts markdown source to an HTML fragment. The kiosk wraps it
// in a styled, scrollable container.
func renderMarkdown(src string) (string, error) {
	var buf bytes.Buffer
	if err := mdRenderer.Convert([]byte(src), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}
