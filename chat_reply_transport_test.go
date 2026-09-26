//go:build darwin || linux

package main

// Agent chat messages travel on stdin through a quoted heredoc instead of as a
// shell argument (docs/agent-chat-message-transport.md). The invariant these
// tests pin: text meant for the chat room is never treated as inert unless
// bash is guaranteed to treat it as inert too — so it can never be auto-allowed
// into running as a command. Wrongly treating a command as text only costs an
// approval prompt; the reverse must be impossible.

import (
	"regexp"
	"strings"
	"testing"
)

func TestReadChatReplyText(t *testing.T) {
	msg := "## VRAM\n| card | price |\n|---|---|\n| 3090 | $700 |\nUse `vLLM` > llama.cpp, it's faster.\n"
	cases := []struct {
		name  string
		args  []string
		stdin string
		tty   bool
		want  string
		err   string
	}{
		{name: "argument wins", args: []string{"hello", "there"}, stdin: "ignored", want: "hello there"},
		{name: `"-" means stdin, never a literal "-"`, args: []string{"-"}, stdin: "hi\n", want: "hi"},
		{name: "stdin, byte-for-byte, heredoc newline trimmed", stdin: msg, want: strings.TrimRight(msg, "\n")},
		{name: "a terminal is never read", stdin: "x", tty: true, err: "required"},
		{name: "empty stdin", stdin: "\n\n", err: "required"},
		{name: "too big", stdin: strings.Repeat("a", chatReplyMaxBytes+1), err: "over"},
	}
	for _, c := range cases {
		got, err := readChatReplyText(c.args, strings.NewReader(c.stdin), c.tty)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

// hostileBody is message text full of things a shell would run or act on if it
// ever read them as commands. Inside the allowlisted heredoc it's inert.
const hostileBody = "costs $700; `rm -rf ~` $(touch /tmp/pwned) > /etc/x | tee y && z\n" +
	"it's \"quoted\" \\ backslash # not a comment\n" +
	"cat <<'X'\nX\n$((1<<2)) ${HOME:-$(id)} $'\\'' case x in a) esac\n" +
	"HEARTH_MSG\n" + // a DIFFERENT marker than the one in use: just text
	"  HEARTH_MSG_AB12CD34\n" + // the real marker, but indented: not a terminator
	"HEARTH_MSG_AB12CD34 \n" // …or with a trailing space: not a terminator either

func TestIsChatReplyHeredoc_AcceptsTheTaughtShape(t *testing.T) {
	const m = "HEARTH_MSG_AB12CD34"
	for name, cmd := range map[string]string{
		"hostile body":         "hearth chat reply --room r1 <<'" + m + "'\n" + hostileBody + m,
		"thread":               "hearth chat reply --room r1 --thread t1 <<'" + m + "'\nhi\n" + m + "\n",
		"trailing blank lines": "hearth chat reply --room r1 <<'" + m + "'\nhi\n" + m + "\n \n\t\n",
		"tab separated":        "hearth\tchat\treply\t--room\tr1\t<<'" + m + "'\nhi\n" + m,
		"full path":            "/usr/local/bin/hearth chat reply --room=r1 <<'" + m + "'\nhi\n" + m,
		"dev binary":           "hearth-dev chat reply --channel --room r1 <<'" + m + "'\nhi\n" + m,
		"empty body":           "hearth chat reply --room r1 <<'" + m + "'\n" + m,
	} {
		if !isChatReplyHeredoc(cmd) || !isSafeCommand(cmd) {
			t.Errorf("%s: expected the taught shape to be auto-allowed:\n%s", name, cmd)
		}
	}
}

// Every deviation from the exact shape is refused — the message could then be
// read as commands (or the scanner can't prove it isn't), so it must prompt.
func TestIsChatReplyHeredoc_RefusesEverythingElse(t *testing.T) {
	for name, cmd := range map[string]string{
		"marker line inside the message ends it early": "hearth chat reply --room r <<'M'\nhi\nM\necho pwned > f\nM",
		"anything after the terminator":                "hearth chat reply --room r <<'M'\nhi\nM\nls",
		"a command after, on one line":                 "hearth chat reply --room r <<'M'\nhi\nM\n; ls",
		"operator on the first line":                   "hearth chat reply --room r <<'M'; rm x\nhi\nM",
		"&& on the first line":                         "hearth chat reply --room r && rm x <<'M'\nhi\nM",
		"pipe on the first line":                       "hearth chat reply --room r | sh <<'M'\nhi\nM",
		"redirect on the first line":                   "hearth chat reply --room r > f <<'M'\nhi\nM",
		"substitution in a word":                       "hearth chat reply --room $(id) <<'M'\nhi\nM",
		"backtick in a word":                           "hearth chat reply --room `id` <<'M'\nhi\nM",
		"variable in a word":                           "hearth chat reply --room $R <<'M'\nhi\nM",
		"quoted word":                                  "hearth chat reply --room \"r\" <<'M'\nhi\nM",
		"env assignment prefix":                        "X=$(id) hearth chat reply --room r <<'M'\nhi\nM",
		"unquoted marker (body would expand)":          "hearth chat reply --room r <<M\n$(touch f)\nM",
		"double-quoted marker":                         "hearth chat reply --room r <<\"M\"\nhi\nM",
		"backslash marker":                             "hearth chat reply --room r <<\\M\nhi\nM",
		"<<- form":                                     "hearth chat reply --room r <<-'M'\n\thi\n\tM",
		"space before marker":                          "hearth chat reply --room r << 'M'\nhi\nM",
		"marker with punctuation":                      "hearth chat reply --room r <<'M-1'\nhi\nM-1",
		"heredoc operator not last":                    "hearth chat reply <<'M' --room r\nhi\nM",
		"two heredocs":                                 "hearth chat reply --room r <<'M' <<'N'\nhi\nM\nx\nN",
		"no terminator":                                "hearth chat reply --room r <<'M'\nhi",
		"indented terminator":                          "hearth chat reply --room r <<'M'\nhi\n  M",
		"CRLF terminator":                              "hearth chat reply --room r <<'M'\r\nhi\r\nM\r\n",
		"carriage return in the first line":            "hearth chat reply --room r\r <<'M'\nhi\nM",
		"single line":                                  "hearth chat reply --room r <<'M'",
		"a different command":                          "cat <<'M'\nhi\nM",
		"a different hearth subcommand":                "hearth chat replyx --room r <<'M'\nhi\nM",
		"too few words":                                "hearth chat <<'M'\nhi\nM",
	} {
		if isChatReplyHeredoc(cmd) {
			t.Errorf("%s: matched the allowlist:\n%s", name, cmd)
		}
		if isSafeCommand(cmd) {
			t.Errorf("%s: auto-allowed:\n%s", name, cmd)
		}
	}
}

// A hearth command written inline is auto-allowed only when bash expands and
// interprets nothing in it — so no part of an inline message can run.
func TestIsSafeCommand_InlineHearthMustBePlain(t *testing.T) {
	for _, cmd := range []string{
		"hearth status",
		`hearth chat reply --room r "hello there"`,
		`hearth chat reply --room r "a > b; c | d && e #f"`,
		"hearth chat reply --room r 'costs $700 `x` $(y)'",
		`hearth chat reply --room r "say \"hi\" \\ ok"`,
		"hearth chat reply --room r a#b",
	} {
		if !isSafeCommand(cmd) {
			t.Errorf("expected safe: %s", cmd)
		}
	}
	for _, cmd := range []string{
		`hearth chat reply --room r "costs $(rm -rf ~)"`,
		"hearth chat reply --room r \"`id`\"",
		`hearth chat reply --room r "$HOME"`,
		`hearth chat reply --room r "a \$b"`,
		"hearth chat reply --room r hi; rm x",
		"hearth chat reply --room r hi\nrm x",
		"hearth chat reply --room r hi > f",
		"hearth chat reply --room r hi | sh",
		`hearth chat reply --room r "unterminated`,
		"hearth chat reply --room r 'unterminated",
		"hearth chat reply --room r $(id)",
		"hearth chat reply --room r \\$x",
		"hearth chat reply --room r # comment > f",
	} {
		if isSafeCommand(cmd) {
			t.Errorf("expected a prompt: %s", cmd)
		}
	}
}

// The general scanner must never consider text inert that bash would run.
// Each of these makes a real redirect look quoted, commented out, or part of a
// heredoc to a naive scanner; all must be refused.
func TestMayRedirectOutput_NoDesync(t *testing.T) {
	for name, cmd := range map[string]string{
		"$'…' with an escaped quote":               "echo $'\\''\necho pwned > f\necho '\n'",
		"quote inside $(…) inside double quotes":   `echo "$(echo ")" > f)"`,
		"quote inside ${…} inside double quotes":   `echo "${x/'"'/y}" > f`,
		"case inside $(…)":                         `echo "$(case x in a) echo x > f;; esac)"`,
		"apostrophe in a heredoc body":             "cat <<'X'\nit's\nX\necho pwned > f\n'",
		"'<<' inside a comment":                    "echo hi # <<'X'\necho pwned > ~/.bashrc\nX",
		"arithmetic shift":                         "echo $((1<<X))\necho pwned > ~/.bashrc\nX",
		"quotes are not quoting in $((…))":         "echo $(( ' $(echo x > f) ' ))",
		"quotes are not quoting in ((…))":          "(( ' $(echo x > f) ' ))",
		"quotes are not quoting in $[…]":           "echo $[ ' $(echo x > f) ' ]",
		"subscript is arithmetic":                  "echo ${a['$(echo x > f)']}",
		"a comment":                                "ls # comment > f",
		"backticks":                                "echo `echo x > f`",
		"heredoc in a subshell":                    "(cat <<'X'\nhi\nX\n)",
		"unterminated single quote":                "echo 'abc",
		"unterminated double quote":                `echo "abc`,
		"unterminated $(":                          "echo $(ls",
		"stray )":                                  "echo )",
		"unquoted heredoc (bodies expand)":         "cat <<EOF\n${x@P}\nEOF",
		"here-string word with a redirect":         "cat <<< x > f",
		"redirect after a quoted string":           `echo "a" > f`,
		"read-write redirect":                      "cat <> f",
		"process substitution writing":             "ls >(cat)",
		"redirect inside $(…) in double quotes":    `echo "$(echo x > f)"`,
		"redirect inside ${…} default (arbitrary)": "echo ${x:-$(echo x > f)}",
		"zsh glob qualifier runs code":             "ls *(e:'echo pwned > f':)",
		"zsh parameter flag evaluates the value":   "x='$(echo pwned > f)'; echo ${(e)x}",
		"array (not modelled)":                     "a=(1 2); echo x",
	} {
		if !mayRedirectOutput(cmd) {
			t.Errorf("%s: scanner missed it:\n%s", name, cmd)
		}
	}
}

// …while ordinary read-only commands stay auto-allowed.
func TestMayRedirectOutput_OrdinaryCommandsStillPass(t *testing.T) {
	for _, cmd := range []string{
		`ls "$(pwd)"`,
		`echo "${HOME}/x"`,
		"echo ${HOME:-/tmp}",
		"cat $(ls)",
		`grep -r "a > b" .`,
		"echo a#b ${#x} $#",
		"ls -la",
		"echo $'a\\'b'",
		`echo "it's" 'say "hi"'`,
		"cat <<< 'here string'",
		"ls\npwd",
	} {
		if mayRedirectOutput(cmd) {
			t.Errorf("expected no redirect: %s", cmd)
		}
	}
}

// A program started directly (not a shell running -c) gets its final argv: no
// shell reads it again, so nothing in it can run. hearth is safe with any
// arguments; other programs keep the '>' check that catches e.g. awk writing
// through its own syntax.
func TestIsSafeRequest_DirectExecVsShellScript(t *testing.T) {
	spawn := func(path string, args ...string) (interposeRequest, string) {
		req := interposeRequest{Type: "spawn", Path: path, Args: args}
		_, in := translateInterposeRequest(req)
		cmd, _ := in["command"].(string)
		return req, cmd
	}
	if req, cmd := spawn("/usr/local/bin/hearth", "hearth", "chat", "reply", "--room", "r", "it's a > b $(x) `y`"); !isSafeRequest(req, cmd) {
		t.Errorf("hearth started directly should be safe whatever its args: %s", cmd)
	}
	if req, cmd := spawn("/usr/bin/awk", "awk", `{print > "f"}`, "file"); isSafeRequest(req, cmd) {
		t.Errorf("awk writing through its own '>' must not be auto-allowed: %s", cmd)
	}
	if req, cmd := spawn("/usr/bin/cat", "cat", "notes.txt"); !isSafeRequest(req, cmd) {
		t.Errorf("plain cat should be safe: %s", cmd)
	}
	if req, cmd := spawn("/bin/bash", "/bin/bash", "-c", `hearth chat reply --room r "$(touch f)"`); isSafeRequest(req, cmd) {
		t.Errorf("a shell script whose chat message would run a command must prompt: %s", cmd)
	}
	if req, cmd := spawn("/bin/bash", "/bin/bash", "-c", "hearth chat reply --room r <<'M'\n$(touch f)\nM"); !isSafeRequest(req, cmd) {
		t.Errorf("the taught heredoc shape should be auto-allowed: %s", cmd)
	}
}

// Claude runs commands as eval '…' and Codex as -c '…'. Inside those single
// quotes every single quote is written as close-quote, escaped quote, reopen;
// the unwrappers must undo that, or the quoted heredoc marker looks unbalanced.
func TestUnwrap_SingleQuotedHeredoc(t *testing.T) {
	real := "hearth chat reply --room r1 <<'HEARTH_MSG_AB12CD34'\nit's a > b\nHEARTH_MSG_AB12CD34"
	escaped := strings.ReplaceAll(real, "'", `'\''`)

	claude := "source /tmp/snap.sh && setopt -m && eval '" + escaped + `' \< /dev/null && pwd -P >| /tmp/cwd`
	if got := unwrapEvalCommand(claude); got != real {
		t.Errorf("claude unwrap:\n got %q\nwant %q", got, real)
	}
	codex := "if . '/h/.codex/shell_snapshots/u.sh' >/dev/null 2>&1; then :; fi\n\nexec '/bin/zsh' -c '" + escaped + "'"
	if got := unwrapCodexCommand(codex); got != real {
		t.Errorf("codex unwrap:\n got %q\nwant %q", got, real)
	}

	// End to end through translate: the agent's post is auto-allowed.
	req := interposeRequest{Type: "spawn", Path: "/bin/zsh", Args: []string{"/bin/zsh", "-c", claude}}
	_, input := translateInterposeRequest(req)
	if cmd, _ := input["command"].(string); !isSafeRequest(req, cmd) {
		t.Errorf("claude-wrapped heredoc post not auto-allowed; command seen:\n%s", cmd)
	}
}

// The wrapper's own end marker is the LAST one: a command whose text contains
// the marker must not cut the extracted command short (and hide what follows).
func TestUnwrapEvalCommand_MarkerTextInsideCommand(t *testing.T) {
	real := `echo "' \< /dev/null" ; echo x > f`
	escaped := strings.ReplaceAll(real, "'", `'\''`)
	wrapped := "source /s && eval '" + escaped + `' \< /dev/null && pwd -P >| /tmp/cwd`
	got := unwrapEvalCommand(wrapped)
	if got != real {
		t.Fatalf("unwrap:\n got %q\nwant %q", got, real)
	}
	if isSafeCommand(got) {
		t.Fatal("the redirect after the marker text must still be seen")
	}
}

// What agents are taught is exactly what the hook allows: filling in any
// message (as long as it doesn't contain the marker line) yields the
// allowlisted shape. And each turn gets its own random marker.
func TestChatReplyHowTo_MatchesTheAllowlist(t *testing.T) {
	marker := newChatReplyMarker()
	if !regexp.MustCompile(`^HEARTH_MSG_[0-9A-F]{8}$`).MatchString(marker) {
		t.Fatalf("marker %q has the wrong shape", marker)
	}
	if other := newChatReplyMarker(); other == marker {
		t.Fatalf("two turns got the same marker %q", marker)
	}
	howTo := chatReplyHowTo("hearth chat reply --room r1", marker)
	example := "hearth chat reply --room r1 <<'" + marker + "'\nyour message\n" + marker + "\n"
	if !strings.Contains(howTo, example) {
		t.Fatalf("instructions don't contain the flush-left example:\n%s", howTo)
	}
	filled := strings.Replace(example, "your message", hostileBody, 1)
	if !isChatReplyHeredoc(filled) {
		t.Fatalf("the taught shape, filled in, isn't allowlisted:\n%s", filled)
	}
	if strings.Contains(howTo, `"your`) {
		t.Fatalf("still shows a quoted-argument message:\n%s", howTo)
	}
}
