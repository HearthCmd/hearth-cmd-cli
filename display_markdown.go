package main

import (
	"bytes"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// mdRenderer is the display server's markdown→HTML renderer (Phase 3, P3-2).
// GitHub-Flavored Markdown (extension.GFM = tables, strikethrough, autolinks,
// task lists) so an agent's forecast/schedule TABLE renders as a real <table>
// instead of raw `| a | b |` pipe characters — matching what the mobile app's
// markdown renderer produces. Raw HTML is still ESCAPED (WithUnsafe is deliberately
// NOT set): agent-authored markdown can't inject a <script> or an <iframe> into the
// kiosk, so the rendered fragment is safe to mount as innerHTML. (For deliberate
// custom HTML, agents use `--type html`, which is sandboxed on the kiosk.) The
// display server renders; the relay only forwards the raw markdown (dumb router).
var mdRenderer = goldmark.New(goldmark.WithExtensions(extension.GFM))

// renderMarkdown converts markdown source to an HTML fragment. The kiosk wraps it
// in a styled, scrollable container.
func renderMarkdown(src string) (string, error) {
	var buf bytes.Buffer
	if err := mdRenderer.Convert([]byte(src), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}
