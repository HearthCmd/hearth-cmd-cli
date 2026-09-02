//go:build darwin || linux

package main

// Pure-function coverage for interpose.go: isSafeCommand (the
// auto-allow gate for Bash), translateInterposeRequest (the protocol
// shim that converts interpose's spawn/open/read/connect into the
// server's tool_name + tool_input shape), the three command-unwrappers
// for harness shell wrappers, the apply_patch parser, and the small
// helpers (formatToolDetail, interposeRequestSummary).
//
// These functions are the seams the daemon uses to decide what to
// auto-allow vs. surface to the user. Wrong answers here either spam
// the user with prompts on read-only commands or silently auto-allow
// a write — both are user-visible bugs.

import (
	"bytes"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
)

// ---------- isSafeCommand ----------

func TestIsSafeCommand_AllowsKnownReadOnlyTools(t *testing.T) {
	for _, cmd := range []string{
		"ls",
		"ls -la",
		"pwd",
		"cat /etc/hosts",
		"grep foo bar.txt",
		"echo hello",
		"hearth status",
		"VAR=val ls",              // env-var prefix skipped
		"FOO=bar BAZ=qux pwd",     // multiple env-var prefixes
		"/usr/local/bin/cat file", // basename extracted
	} {
		if !isSafeCommand(cmd) {
			t.Errorf("expected %q to be safe", cmd)
		}
	}
}

func TestIsSafeCommand_RejectsRedirects(t *testing.T) {
	for _, cmd := range []string{
		"ls > out.txt",
		"echo hi >> log",
		"cat foo > bar",
	} {
		if isSafeCommand(cmd) {
			t.Errorf("expected %q to be unsafe (output redirect)", cmd)
		}
	}
}

func TestIsSafeCommand_QuotedRedirectsAreFine(t *testing.T) {
	// '>' inside quotes is part of the argument, not a redirect — must
	// not flip the safety verdict.
	for _, cmd := range []string{
		`echo "a > b"`,
		`echo 'a > b'`,
		`grep ">" file`,
	} {
		if !isSafeCommand(cmd) {
			t.Errorf("expected %q to be safe (quoted '>')", cmd)
		}
	}
}

func TestIsSafeCommand_RejectsWriteCommands(t *testing.T) {
	for _, cmd := range []string{
		"rm -rf foo",
		"mv a b",
		"cp a b",
		"touch new",
		"git commit",
		"npm install",
		"", // empty
	} {
		if isSafeCommand(cmd) {
			t.Errorf("expected %q to be unsafe", cmd)
		}
	}
}

// ---------- translateInterposeRequest ----------

func TestTranslate_Read(t *testing.T) {
	tool, input := translateInterposeRequest(interposeRequest{Type: "read", Path: "/etc/hosts"})
	if tool != "Read" || input["file_path"] != "/etc/hosts" {
		t.Errorf("read translation: tool=%q input=%v", tool, input)
	}
}

func TestTranslate_OpenWriteOnNonexistentIsWrite(t *testing.T) {
	tool, _ := translateInterposeRequest(interposeRequest{
		Type: "open", Path: "/nonexistent/totally-fake-path", Flags: "w",
	})
	if tool != "Write" {
		t.Errorf("nonexistent open should be Write, got %q", tool)
	}
}

func TestTranslate_ConnectMapsToWebFetch(t *testing.T) {
	tool, input := translateInterposeRequest(interposeRequest{
		Type: "connect", Host: "api.example.com", Port: 443,
	})
	if tool != "WebFetch" {
		t.Errorf("connect should map to WebFetch, got %q", tool)
	}
	if u, _ := input["url"].(string); !strings.HasPrefix(u, "https://") || !strings.Contains(u, "api.example.com") {
		t.Errorf("expected https URL with host, got %v", input["url"])
	}
}

func TestTranslate_ConnectNonHTTPSPort(t *testing.T) {
	// Port != 443 falls through to IP rather than the host URL — the
	// interpose layer can't probe TLS, so it surfaces the raw IP.
	tool, input := translateInterposeRequest(interposeRequest{
		Type: "connect", Host: "api.example.com", IP: "203.0.113.5", Port: 80,
	})
	if tool != "WebFetch" {
		t.Errorf("connect should map to WebFetch, got %q", tool)
	}
	if input["url"] != "203.0.113.5" {
		t.Errorf("non-443 port should surface the IP, got %v", input["url"])
	}
}

func TestTranslate_SpawnExtractsShellCommand(t *testing.T) {
	tool, input := translateInterposeRequest(interposeRequest{
		Type: "spawn",
		Path: "/bin/bash",
		Args: []string{"bash", "-c", "ls -la"},
	})
	if tool != "Bash" {
		t.Errorf("spawn should map to Bash, got %q", tool)
	}
	if input["command"] != "ls -la" {
		t.Errorf("expected 'ls -la', got %v", input["command"])
	}
}

func TestTranslate_SpawnNoArgsJoins(t *testing.T) {
	// No -c, no --, just argv → joined.
	tool, input := translateInterposeRequest(interposeRequest{
		Type: "spawn",
		Path: "/usr/bin/git",
		Args: []string{"git", "status"},
	})
	if tool != "Bash" {
		t.Errorf("expected Bash, got %q", tool)
	}
	if cmd, _ := input["command"].(string); !strings.Contains(cmd, "git status") {
		t.Errorf("expected joined argv, got %v", input["command"])
	}
}

func TestTranslate_SpawnCodexApplyPatch(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: foo/bar.go\n-old line\n+new line\n*** End Patch"
	tool, input := translateInterposeRequest(interposeRequest{
		Type: "spawn",
		Path: "/usr/local/bin/codex",
		Args: []string{"codex", "apply_patch", patch},
	})
	if tool != "Edit" {
		t.Errorf("codex apply_patch should map to Edit, got %q", tool)
	}
	if input["file_path"] != "foo/bar.go" {
		t.Errorf("expected file_path 'foo/bar.go', got %v", input["file_path"])
	}
	if !strings.Contains(input["old_string"].(string), "old line") {
		t.Errorf("old_string lost: %v", input["old_string"])
	}
	if !strings.Contains(input["new_string"].(string), "new line") {
		t.Errorf("new_string lost: %v", input["new_string"])
	}
}

func TestTranslate_DefaultGenericShape(t *testing.T) {
	tool, input := translateInterposeRequest(interposeRequest{
		Type: "weird-unhandled-type", Path: "/x",
	})
	if tool != "Generic" {
		t.Errorf("unknown type should fall through to Generic, got %q", tool)
	}
	if input["type"] != "weird-unhandled-type" || input["path"] != "/x" {
		t.Errorf("generic shape lost fields: %v", input)
	}
}

// ---------- unwrapEvalCommand ----------

func TestUnwrapEvalCommand_ClaudeWrapper(t *testing.T) {
	cmd := `source /tmp/snap.sh && setopt -m && eval 'ls -la' \< /dev/null && pwd`
	got := unwrapEvalCommand(cmd)
	if got != "ls -la" {
		t.Errorf("expected 'ls -la', got %q", got)
	}
}

func TestUnwrapEvalCommand_NoWrapperPassThrough(t *testing.T) {
	cmd := "echo hello"
	if got := unwrapEvalCommand(cmd); got != cmd {
		t.Errorf("non-wrapped should pass through, got %q", got)
	}
}

func TestUnwrapEvalCommand_DoubleQuotedWithEscapes(t *testing.T) {
	cmd := `source /x && eval "echo \"hi\"" \< /dev/null && pwd`
	got := unwrapEvalCommand(cmd)
	// \" sequences get unescaped to "
	if !strings.Contains(got, `echo "hi"`) {
		t.Errorf("expected unescaped echo, got %q", got)
	}
}

// ---------- unwrapGeminiCommand ----------

func TestUnwrapGeminiCommand_HappyPath(t *testing.T) {
	cmd := "shopt -u inherit_errexit; { ls -la }; __code=$?; echo $__code"
	got := unwrapGeminiCommand(cmd)
	if got != "ls -la" {
		t.Errorf("expected 'ls -la', got %q", got)
	}
}

func TestUnwrapGeminiCommand_NotShoptPrefixedPassThrough(t *testing.T) {
	cmd := "echo hello"
	if got := unwrapGeminiCommand(cmd); got != cmd {
		t.Errorf("non-shopt should pass through, got %q", got)
	}
}

// ---------- unwrapCodexCommand ----------

func TestUnwrapCodexCommand_HappyPath(t *testing.T) {
	cmd := "if . '/home/u/.codex/shell_snapshots/abc.sh' >/dev/null 2>&1; then :; fi\n\nexec '/bin/zsh' -c 'ls -la'"
	got := unwrapCodexCommand(cmd)
	if got != "ls -la" {
		t.Errorf("expected 'ls -la', got %q", got)
	}
}

func TestUnwrapCodexCommand_NoExecPassThrough(t *testing.T) {
	cmd := "echo hello"
	if got := unwrapCodexCommand(cmd); got != cmd {
		t.Errorf("non-codex should pass through, got %q", got)
	}
}

// ---------- extractPatchStrings ----------

func TestExtractPatchStrings(t *testing.T) {
	patch := "*** Begin Patch\n-foo\n-bar\n+baz\n+qux\n*** End Patch"
	old, new := extractPatchStrings(patch)
	if !strings.Contains(old, "foo") || !strings.Contains(old, "bar") {
		t.Errorf("old_string lost lines: %q", old)
	}
	if !strings.Contains(new, "baz") || !strings.Contains(new, "qux") {
		t.Errorf("new_string lost lines: %q", new)
	}
}

func TestExtractPatchStrings_EmptyPatch(t *testing.T) {
	old, new := extractPatchStrings("")
	if old != "" || new != "" {
		t.Errorf("empty patch should yield empty strings, got %q / %q", old, new)
	}
}

// ---------- formatToolDetail ----------

func TestFormatToolDetail(t *testing.T) {
	cases := []struct {
		tool  string
		input map[string]interface{}
		want  string
	}{
		{"Bash", map[string]interface{}{"command": "ls -la"}, "ls -la"},
		{"Read", map[string]interface{}{"file_path": "/etc/hosts"}, "/etc/hosts"},
		{"Write", map[string]interface{}{"file_path": "/tmp/x"}, "/tmp/x"},
		{"Edit", map[string]interface{}{"file_path": "/tmp/y"}, "/tmp/y"},
		{"WebFetch", map[string]interface{}{"url": "https://x.com"}, "https://x.com"},
	}
	for _, c := range cases {
		if got := formatToolDetail(c.tool, c.input); got != c.want {
			t.Errorf("formatToolDetail(%q, %v) = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}

func TestFormatToolDetail_FallsBackToFmt(t *testing.T) {
	got := formatToolDetail("UnknownTool", map[string]interface{}{"foo": "bar"})
	if !strings.Contains(got, "foo") || !strings.Contains(got, "bar") {
		t.Errorf("expected fallback to surface raw input, got %q", got)
	}
}

// ---------- interposeRequestSummary ----------

func TestInterposeRequestSummary(t *testing.T) {
	cases := []struct {
		req  interposeRequest
		want string
	}{
		{interposeRequest{Type: "open", Path: "/x", Flags: "w"}, "/x (w)"},
		{interposeRequest{Type: "connect", Host: "api.example.com"}, "api.example.com"},
		{interposeRequest{Type: "connect", IP: "1.2.3.4"}, "1.2.3.4"},
		{interposeRequest{Type: "read", Path: "/etc/hosts"}, "/etc/hosts"},
		{interposeRequest{Type: "spawn", Path: "/bin/sh", Args: []string{"sh", "-c", "ls"}}, "/bin/sh ls"},
	}
	for _, c := range cases {
		if got := interposeRequestSummary(c.req); got != c.want {
			t.Errorf("summary(%+v) = %q, want %q", c.req, got, c.want)
		}
	}
}

// ---------- sanity: round-trip a few real shapes ----------

func TestTranslate_RoundTripBashExtractedCommand(t *testing.T) {
	// claude-style wrapper → translate → unwrap → original cmd
	wrapped := `source /tmp/snap.sh && setopt -m && eval 'echo hi' \< /dev/null && pwd`
	tool, input := translateInterposeRequest(interposeRequest{
		Type: "spawn", Path: "/bin/zsh",
		Args: []string{"zsh", "-c", wrapped},
	})
	if tool != "Bash" {
		t.Fatalf("expected Bash, got %q", tool)
	}
	if input["command"] != "echo hi" {
		t.Errorf("expected unwrapped 'echo hi', got %v", input["command"])
	}
}

func TestExtractPatchStrings_OnlyAdditions(t *testing.T) {
	patch := "*** Begin Patch\n+only added\n*** End Patch"
	old, new := extractPatchStrings(patch)
	if old != "" {
		t.Errorf("expected empty old, got %q", old)
	}
	if !strings.Contains(new, "only added") {
		t.Errorf("expected 'only added', got %q", new)
	}
}

// Sanity: structural assertion that a Read translation never tries
// to reach the filesystem (regression: an earlier version did a
// filesystem stat in the Read branch by mistake).
func TestTranslate_ReadDoesNotTouchFilesystem(t *testing.T) {
	got1, _ := translateInterposeRequest(interposeRequest{Type: "read", Path: "/this/does/not/exist/x"})
	got2, _ := translateInterposeRequest(interposeRequest{Type: "read", Path: "/this/does/not/exist/y"})
	if got1 != "Read" || got2 != "Read" {
		t.Errorf("Read translation must be deterministic regardless of fs state, got %q / %q", got1, got2)
	}
}

// Sanity: argv with no -c and no -- but multiple args — the join
// path must include them all in order.
func TestTranslate_SpawnJoinsAllArgs(t *testing.T) {
	tool, input := translateInterposeRequest(interposeRequest{
		Type: "spawn", Path: "/usr/bin/grep",
		Args: []string{"grep", "-r", "foo", "src/"},
	})
	if tool != "Bash" {
		t.Errorf("expected Bash, got %q", tool)
	}
	wantParts := []string{"grep", "-r", "foo", "src/"}
	cmd, _ := input["command"].(string)
	parts := strings.Fields(cmd)
	if !reflect.DeepEqual(parts, wantParts) {
		t.Errorf("expected joined %v, got %v", wantParts, parts)
	}
}

// The SNI allowlist bypass. The hook builds the connect request with the raw
// hostname (`"host":"%s"` — the one request builder that does not pre-escape),
// and the hostname is memcpy'd straight out of a ClientHello, so a gated process
// controls it. encoding/json keeps the LAST duplicate key, so without this check
// the daemon gates api.anthropic.com while the socket goes to evil.com — and an
// existing WebFetch auto-approve rule for the allowlisted host means nobody is
// even asked.
func TestRejectDuplicateJSONKeys_BlocksSNIHostInjection(t *testing.T) {
	// What the hook emits for SNI `evil.com","host":"api.anthropic.com`.
	line := []byte(`{"type":"connect","host":"evil.com","host":"api.anthropic.com","ip":"1.2.3.4","port":443,"pid":42}`)

	// Establish the danger is real before asserting we stop it: stock unmarshal
	// silently prefers the attacker's second value.
	var req interposeRequest
	if err := json.Unmarshal(line, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Host != "api.anthropic.com" {
		t.Fatalf("precondition failed: encoding/json gave host=%q, expected last-wins to yield the allowlisted host", req.Host)
	}

	if err := rejectDuplicateJSONKeys(line); err == nil {
		t.Fatal("duplicate host key accepted — the allowlist bypass is open")
	}
}

func TestRejectDuplicateJSONKeys_AllowsOrdinaryRequests(t *testing.T) {
	for _, line := range []string{
		`{"type":"spawn","path":"/bin/echo","args":["echo","hi"],"pid":1}`,
		`{"type":"connect","host":"api.anthropic.com","ip":"1.2.3.4","port":443,"pid":2}`,
		`{"type":"open","path":"/tmp/x","flags":"w","pid":3,"project":true}`,
		// A repeated key in DIFFERENT objects is not a duplicate.
		`{"type":"spawn","args":["a","a"],"path":"/bin/x","pid":4}`,
	} {
		if err := rejectDuplicateJSONKeys([]byte(line)); err != nil {
			t.Errorf("rejected a legitimate request %s: %v", line, err)
		}
	}
}

// Syntax errors are json.Unmarshal's to report — it has the better message, and
// this check running first must not swallow or reword them.
func TestRejectDuplicateJSONKeys_DefersToUnmarshalOnGarbage(t *testing.T) {
	for _, line := range []string{
		`{"type":"spawn","args":["a",`, // truncated
		`not json at all`,
		``,
	} {
		if err := rejectDuplicateJSONKeys([]byte(line)); err != nil {
			t.Errorf("reported a syntax error that belongs to Unmarshal (%s): %v", line, err)
		}
	}
}

// A request larger than the old 4 KiB recvmsg buffer used to reach Unmarshal
// truncated and be denied fail-closed, with size never named as the reason.
func TestReadToNewline_ReassemblesAcrossChunks(t *testing.T) {
	bigArg := strings.Repeat("x", 12000)
	payload := `{"type":"spawn","path":"/bin/echo","args":["echo","` + bigArg + `"],"pid":1}`

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		// Deliberately split mid-payload, and send the newline separately —
		// which is exactly what the hook's two writes do.
		client.Write([]byte(payload[:1000]))
		client.Write([]byte(payload[1000:]))
		client.Write([]byte("\n"))
	}()

	first := make([]byte, 512) // stand-in for the 4 KiB recvmsg first chunk
	n, err := server.Read(first)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}

	line, err := readToNewline(server, append([]byte(nil), first[:n]...))
	if err != nil {
		t.Fatalf("readToNewline: %v", err)
	}

	var req interposeRequest
	if err := json.Unmarshal(bytes.TrimRight(line, "\n"), &req); err != nil {
		t.Fatalf("reassembled request did not parse: %v", err)
	}
	if len(req.Args) != 2 || req.Args[1] != bigArg {
		t.Fatalf("argv did not survive reassembly: got %d args, second is %d bytes",
			len(req.Args), len(req.Args[1]))
	}
}

func TestInterposeRequestPrefix_BoundedAndSanitized(t *testing.T) {
	got := interposeRequestPrefix([]byte("{\"a\":\"b\"}\n\x00\x1b[31m\xff"))
	for _, bad := range []string{"\n", "\x00", "\x1b", "\xff"} {
		if strings.Contains(got, bad) {
			t.Errorf("prefix leaked a raw control/non-ASCII byte %q: %s", bad, got)
		}
	}

	long := interposeRequestPrefix(bytes.Repeat([]byte("a"), interposeRequestPrefixBytes*3))
	if !strings.HasSuffix(long, `"...`) {
		t.Errorf("an over-long request should be marked truncated, got tail %q", long[len(long)-8:])
	}
	// Cap plus quotes and the ellipsis — never the whole argument.
	if len(long) > interposeRequestPrefixBytes+8 {
		t.Errorf("prefix ran to %d bytes, cap is %d", len(long), interposeRequestPrefixBytes)
	}
}
