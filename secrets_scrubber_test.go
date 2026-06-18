//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/url"
	"strings"
	"testing"
)

func TestEncodingsOf_CoversAllForms(t *testing.T) {
	secret := []byte("ha-token-xyz")
	forms := encodingsOf(secret)

	// Each form must appear at least once in the result. We don't
	// assert exact count because some encodings happen to collide
	// (e.g. base64 std + url for inputs without +/_); dedupe via map.
	formStrings := map[string]bool{}
	for _, f := range forms {
		formStrings[string(f)] = true
	}

	want := []string{
		string(secret),                               // plaintext
		hex.EncodeToString(secret),                   // hex lower
		strings.ToUpper(hex.EncodeToString(secret)),  // hex UPPER
		base64.StdEncoding.EncodeToString(secret),    // b64 std padded
		base64.RawStdEncoding.EncodeToString(secret), // b64 std unpadded
		base64.URLEncoding.EncodeToString(secret),    // b64 url padded
		base64.RawURLEncoding.EncodeToString(secret), // b64 url unpadded
		url.QueryEscape(string(secret)),              // url-escaped
	}
	for _, w := range want {
		if !formStrings[w] {
			t.Errorf("encoding form %q missing from result", w)
		}
	}
}

func TestScrubBytes_ReplacesEveryForm(t *testing.T) {
	secret := []byte("supers3cret")
	forms := computeScrubForms([][]byte{secret})

	input := strings.Join([]string{
		"plain: " + string(secret),
		"hex: " + hex.EncodeToString(secret),
		"b64: " + base64.StdEncoding.EncodeToString(secret),
		"urlenc: " + url.QueryEscape(string(secret)),
		"safe value untouched",
	}, "\n")

	scrubbed := scrubBytes([]byte(input), forms)
	out := string(scrubbed)
	for _, lit := range []string{
		string(secret),
		hex.EncodeToString(secret),
		base64.StdEncoding.EncodeToString(secret),
		url.QueryEscape(string(secret)),
	} {
		if strings.Contains(out, lit) {
			t.Errorf("scrubbed output still contains %q\nfull: %s", lit, out)
		}
	}
	if !strings.Contains(out, "safe value untouched") {
		t.Errorf("scrub corrupted non-secret content: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("redaction marker missing from output: %s", out)
	}
}

func TestScrubBytes_EmptyForms_NoOp(t *testing.T) {
	input := []byte("no secrets here")
	got := scrubBytes(input, nil)
	if !bytes.Equal(got, input) {
		t.Errorf("nil forms should pass through: got %q", got)
	}
}

func TestComputeScrubForms_SortsLongestFirst(t *testing.T) {
	// Two secrets where one is a substring of the other. The longer
	// one MUST come first or the shorter form would replace a
	// prefix and leak the longer tail.
	short := []byte("abc")
	long := []byte("abcdef")
	forms := computeScrubForms([][]byte{short, long})
	if len(forms) == 0 {
		t.Fatal("expected forms")
	}
	if len(forms[0]) < len(forms[len(forms)-1]) {
		t.Errorf("forms not sorted longest-first")
	}
}

func TestScrubBytes_NestedSubstringSecrets(t *testing.T) {
	// "alpha" is a prefix of "alphabet". Both flagged as secrets.
	// Naive replace ("alpha" first) would turn "alphabet" into
	// "***bet" — leaking three letters. Longest-first prevents it.
	forms := computeScrubForms([][]byte{[]byte("alpha"), []byte("alphabet")})
	input := []byte("contains alpha and alphabet here")
	out := string(scrubBytes(input, forms))
	if strings.Contains(out, "bet") {
		t.Errorf("longest-first replace failed; got %q", out)
	}
	if strings.Contains(out, "alpha") {
		t.Errorf("residual alpha leak: %q", out)
	}
}

func TestScrubReader_StraddleAcrossReadBoundary(t *testing.T) {
	// Build a reader that returns the secret split across two
	// reads. The carry-buffer logic must catch it.
	secret := []byte("longish-secret-value")
	forms := computeScrubForms([][]byte{secret})

	prefix := []byte("prefix bytes ")
	suffix := []byte(" trailing bytes")
	wholeLine := append(append([]byte{}, prefix...), secret...)
	wholeLine = append(wholeLine, suffix...)

	// Reader emits two chunks split mid-secret.
	splitAt := len(prefix) + len(secret)/2
	r := &chunkReader{
		chunks: [][]byte{wholeLine[:splitAt], wholeLine[splitAt:]},
	}
	var buf bytes.Buffer
	if err := scrubReader(r, &buf, forms); err != nil {
		t.Fatalf("scrubReader: %v", err)
	}
	if bytes.Contains(buf.Bytes(), secret) {
		t.Errorf("split-across-reads secret leaked through: %q", buf.String())
	}
}

// chunkReader returns a fixed sequence of byte slices, one per
// Read call, then EOF. Tests the streaming scrub's carry-buffer.
type chunkReader struct {
	chunks [][]byte
	idx    int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.idx >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.idx])
	c.idx++
	return n, nil
}
