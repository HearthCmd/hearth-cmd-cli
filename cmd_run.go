//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runRun is the entry point for `hearth run`. Parses --secret flags,
// asks the daemon to resolve each binding to cleartext (IAM-gated by
// the server-side Authorize chokepoint), execs the named command with
// the cleartexts injected as env vars, and streams stdout/stderr through
// a scrubber that masks every encoded form of every cleartext with `***`.
//
// Usage: hearth run [--secret ENV=<secret-id>]... [--] <cmd> [args...]
//
// Cleartext flows: daemon RAM → unix socket → wrapper RAM → child
// process env. Never enters the agent's transcript because the wrapper
// scrubs its own stdout/stderr before piping back to the caller's terminal.
// Decryption happens in the daemon (per-host secrets keypair holder),
// not in the CLI.
func runRun(args []string) {
	if len(args) == 0 {
		printRunUsage()
		os.Exit(1)
	}

	bindings := map[string]string{} // env_name → secret_id
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--secret":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hearth run: --secret requires ENV=<secret-id>")
				os.Exit(1)
			}
			pair := args[i+1]
			eq := strings.IndexByte(pair, '=')
			if eq < 1 || eq == len(pair)-1 {
				fmt.Fprintf(os.Stderr, "hearth run: --secret expects ENV=<secret-id>, got %q\n", pair)
				os.Exit(1)
			}
			env := pair[:eq]
			if env == "PATH" {
				fmt.Fprintln(os.Stderr, "hearth run: refusing to inject into PATH")
				os.Exit(1)
			}
			bindings[env] = pair[eq+1:]
			i += 2
		case a == "--":
			i++
			goto rest
		case a == "-h", a == "--help":
			printRunUsage()
			os.Exit(0)
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "hearth run: unknown flag %q\n", a)
			os.Exit(1)
		default:
			goto rest
		}
	}
rest:
	cmdArgs := args[i:]
	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "hearth run: missing command")
		os.Exit(1)
	}

	// Resolve secrets via daemon BEFORE forking the child. Any decrypt
	// failure aborts entirely — better to fail fast than run with some
	// env vars missing.
	cleartexts, perr := resolveSecretsViaDaemon(bindings)
	if perr != nil {
		exitForPluginErrorCode(string(perr.Code), perr.Message)
	}
	defer zeroSecretMap(cleartexts)

	// Refuse cleartexts < 8 bytes — scrubbing a short value would
	// false-positive-redact normal output.
	for env, v := range cleartexts {
		if len(v) < 8 {
			fmt.Fprintf(os.Stderr, "hearth run: secret %q is too short to safely scrub (need >= 8 bytes)\n", env)
			os.Exit(1)
		}
	}

	// Build scrub forms from every cleartext value.
	var vals [][]byte
	for _, v := range cleartexts {
		vals = append(vals, []byte(v))
	}
	forms := computeScrubForms(vals)

	// Build child env: pass through current env minus any names we're
	// injecting (so our binding wins on collision), then append the
	// secrets.
	env := os.Environ()
	skip := map[string]bool{}
	for k := range cleartexts {
		skip[k] = true
	}
	filtered := env[:0]
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			if skip[kv[:eq]] {
				continue
			}
		}
		filtered = append(filtered, kv)
	}
	for k, v := range cleartexts {
		filtered = append(filtered, k+"="+v)
	}

	bin, err := exec.LookPath(cmdArgs[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth run: not found: %s\n", cmdArgs[0])
		os.Exit(127)
	}

	cmd := exec.Command(bin, cmdArgs[1:]...)
	cmd.Env = filtered
	cmd.Stdin = os.Stdin

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth run: %v\n", err)
		os.Exit(1)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth run: %v\n", err)
		os.Exit(1)
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "hearth run: %v\n", err)
		os.Exit(1)
	}

	// Stream child stdout/stderr through the scrubber. scrubReader
	// uses a carry-buffer (= max form length - 1) so a cleartext
	// straddling two reads still gets caught.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if serr := scrubReader(stdoutPipe, os.Stdout, forms); serr != nil && serr != io.EOF {
			fmt.Fprintf(os.Stderr, "hearth run: stdout scrub error: %v\n", serr)
		}
	}()
	go func() {
		defer wg.Done()
		if serr := scrubReader(stderrPipe, os.Stderr, forms); serr != nil && serr != io.EOF {
			fmt.Fprintf(os.Stderr, "hearth run: stderr scrub error: %v\n", serr)
		}
	}()

	waitErr := cmd.Wait()
	wg.Wait()

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				os.Exit(status.ExitStatus())
			}
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "hearth run: %v\n", waitErr)
		os.Exit(1)
	}
}

func printRunUsage() {
	fmt.Fprint(os.Stderr, `Usage: hearth run [--secret ENV=<secret-id>]... [--] <cmd> [args...]

Decrypts named secrets (via the daemon, IAM-gated by the server's
secret.use rule) and injects them into the child process environment.
The child's stdout/stderr is scrubbed of all encoded forms of every
injected secret before being forwarded to the caller's terminal.

Cleartext lives in: daemon RAM (briefly), this wrapper's RAM (until
exec), child process env. Never enters the agent's transcript.

Refuses secrets < 8 bytes (scrub safety). Refuses --secret PATH=...

Examples:
  hearth run --secret GITHUB_TOKEN=sec-abc -- gh issue list
  hearth run --secret HA=sec-def -- curl -H "Authorization: Bearer $HA" https://...
`)
}

// resolveSecretsViaDaemon dials the daemon, sends a secret_resolve
// IPC, and returns the {env_name → cleartext} map. Returns a
// *PluginError on any path so the caller can map to an exit code.
//
// Empty bindings → returns an empty map without IPC round-trip
// (avoids the daemon's "no bindings to resolve" error for the common
// `hearth run -- <cmd>` no-secrets case).
func resolveSecretsViaDaemon(bindings map[string]string) (map[string]string, *PluginError) {
	if len(bindings) == 0 {
		return nil, nil
	}
	conn, err := net.DialTimeout("unix", daemonSockPath(), 5*time.Second)
	if err != nil {
		return nil, &PluginError{Code: ErrTransport,
			Message: fmt.Sprintf("cannot connect to daemon: %v\nRun 'hearth start' first", err)}
	}
	defer conn.Close()

	req := ipcRequest{
		Type:           "secret_resolve",
		SecretBindings: bindings,
	}
	// Principal identity is daemon-derived per Phase 2 of
	// docs/agent-identity-plan.md; the CLI no longer forwards
	// HEARTH_AGENT_INSTANCE_ID as a claim (was forgeable by
	// same-UID siblings).

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, &PluginError{Code: ErrInternal, Message: "marshal: " + err.Error()}
	}
	reqBytes = append(reqBytes, '\n')
	if _, err := conn.Write(reqBytes); err != nil {
		return nil, &PluginError{Code: ErrTransport, Message: "send: " + err.Error()}
	}
	// Generous read deadline — server-side ask can suspend
	// the response while a human approves on a phone (default
	// PendingRequest timeout is minutes).
	conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, &PluginError{Code: ErrTransport, Message: "read: " + err.Error()}
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, &PluginError{Code: ErrInternal, Message: "decode: " + err.Error()}
	}
	if resp.Type == "error" {
		code := ErrorCode(resp.ResourceErrCode)
		if code == "" {
			code = ErrInternal
		}
		return nil, &PluginError{Code: code, Message: resp.Message}
	}
	return resp.SecretCleartexts, nil
}
