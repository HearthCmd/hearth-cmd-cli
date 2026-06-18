//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/url"
	"sort"
	"strings"
)

// Output scrubber for plugin stdout/stderr. Backstop, not a
// guarantee — plugin authors are still responsible for not echoing
// credentials. But a single bug in a plugin's logging shouldn't
// dump a token into the agent's terminal or the daemon's log.
//
// Primitives lifted verbatim from greenlight-cli/run.go
// (encodingsOf, dedupeForms, scrub). Eight encoding forms cover
// what's practically observed: plaintext, hex (both cases), base64
// (std + url, padded + unpadded), and URL-encoded. If a plugin
// SHA-256s a credential and prints the digest, we won't catch it —
// document as a known limit.

// encodingsOf returns deduplicated byte forms of a secret used by
// scrub. Lifted verbatim from greenlight-cli.
func encodingsOf(secret []byte) [][]byte {
	formsSet := map[string]struct{}{
		string(secret):                               {},
		hex.EncodeToString(secret):                   {},
		strings.ToUpper(hex.EncodeToString(secret)):  {},
		base64.StdEncoding.EncodeToString(secret):    {},
		base64.RawStdEncoding.EncodeToString(secret): {},
		base64.URLEncoding.EncodeToString(secret):    {},
		base64.RawURLEncoding.EncodeToString(secret): {},
		url.QueryEscape(string(secret)):              {},
	}
	out := make([][]byte, 0, len(formsSet))
	for s := range formsSet {
		out = append(out, []byte(s))
	}
	return out
}

func dedupeForms(forms [][]byte) [][]byte {
	seen := map[string]struct{}{}
	out := make([][]byte, 0, len(forms))
	for _, f := range forms {
		if len(f) == 0 {
			continue
		}
		k := string(f)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, f)
	}
	return out
}

// computeScrubForms expands a set of secret values into all their
// encoding forms, sorted longest-first. Sort matters: scrub
// replaces in order, and a longer form whose prefix matches a
// shorter form must be replaced first or the longer form's tail
// leaks. Cached once per process; the result is plumbed into
// p.scrubForms.
func computeScrubForms(secrets [][]byte) [][]byte {
	var all [][]byte
	for _, s := range secrets {
		if len(s) == 0 {
			continue
		}
		all = append(all, encodingsOf(s)...)
	}
	all = dedupeForms(all)
	sort.Slice(all, func(i, j int) bool { return len(all[i]) > len(all[j]) })
	return all
}

// scrubBytes returns src with every form in forms replaced by "***".
// Single-shot — no streaming carry. Used for InvokeResult.Stdout
// (one complete frame per call) and per-line stderr (bufio.Scanner
// already produces whole lines, so a secret can't straddle two of
// them within one log emission).
func scrubBytes(src []byte, forms [][]byte) []byte {
	if len(forms) == 0 || len(src) == 0 {
		return src
	}
	redaction := []byte("***")
	for _, f := range forms {
		src = bytes.ReplaceAll(src, f, redaction)
	}
	return src
}

// scrubReader is the streaming variant lifted from greenlight-cli.
// Not used by the current plugin_process.go paths (stderr is
// line-bufio'd and Invoke results are single-shot), but kept
// because (a) it's a verbatim port we don't want to lose, and
// (b) future streaming-output verbs will need it. The carry-buffer
// pattern prevents a form straddling two reads from slipping
// through.
func scrubReader(src io.Reader, dst io.Writer, forms [][]byte) error {
	if len(forms) == 0 {
		_, err := io.Copy(dst, src)
		return err
	}
	overlap := len(forms[0]) - 1
	buf := make([]byte, 64*1024)
	var carry []byte
	redaction := []byte("***")

	for {
		n, err := src.Read(buf)
		if n > 0 {
			window := append(carry, buf[:n]...)
			for _, f := range forms {
				window = bytes.ReplaceAll(window, f, redaction)
			}
			if len(window) > overlap {
				if _, werr := dst.Write(window[:len(window)-overlap]); werr != nil {
					return werr
				}
				carry = append(carry[:0], window[len(window)-overlap:]...)
			} else {
				carry = append(carry[:0], window...)
			}
		}
		if err == io.EOF {
			for _, f := range forms {
				carry = bytes.ReplaceAll(carry, f, redaction)
			}
			_, werr := dst.Write(carry)
			return werr
		}
		if err != nil {
			return err
		}
	}
}
