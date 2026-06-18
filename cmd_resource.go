//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// runResource dispatches `hearth resource <subcommand> ...`.
func runResource(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printResourceUsage()
		if len(args) == 0 {
			os.Exit(1)
		}
		return
	}
	switch args[0] {
	case "list":
		runResourceList(args[1:])
	case "invoke":
		runResourceInvoke(args[1:])
	case "refresh":
		runResourceRefresh(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "hearth resource: unknown subcommand %q\n", args[0])
		printResourceUsage()
		os.Exit(1)
	}
}

func printResourceUsage() {
	fmt.Fprint(os.Stderr, `Usage: hearth resource <subcommand> [args]

Subcommands:
  list
      List Resource Connections backed by plugins installed on this
      host. Server-filtered by host_id — other hosts' connections in
      the same organization aren't shown. Use the webview (Settings →
      Debug → Resource Connections) for the org-wide view.

  invoke <connection-id> <verb> [args-json] [--secret ENV=<secret-id> ...]
      Dispatch a verb call to the plugin backing a Resource Connection
      and print the plugin's response. args-json is an optional JSON
      object passed verbatim as the verb's args; omit for null.

      --secret ENV=<secret-id> may repeat. The daemon authorizes each
      secret via IAM (resource_kind='secret', action='secret.use'),
      decrypts locally, and passes the cleartext to the plugin keyed
      by ENV. Plugin author reads it as if it were an env var. The
      cleartext never enters the agent's transcript; the daemon
      scrubs it (and encoded variants) from the plugin's response.

  refresh <connection-id> [--secret ENV=<secret-id> ...]
      Run the connection's declarative snapshot (manifest.snapshot
      block), replace the daemon-local entity cache, and print the
      number of entities pulled. Declarative adapters only; binary
      plugins surface entities through their own Onboard step.
      Credentials follow the same --secret ENV=<id> convention as
      'invoke' (the snapshot HTTP usually needs the upstream token).

Resource Connections live server-side; manage them in the webview
(Settings → Debug → Resource Connections). Credentials are set
via 'hearth secret set' and granted to principals via
'hearth secret grant'.
`)
}

// runResourceInvoke parses positional args + optional --secret flags,
// dials the daemon, sends a resource_invoke IPC, and prints the
// result. Exit-code mapping for the *PluginError vocabulary is in
// exitForPluginErrorCode.
func runResourceInvoke(args []string) {
	// Split positional args from --secret flag pairs. Order of
	// positional and flag args is flexible.
	positional := []string{}
	bindings := map[string]string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "--secret" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hearth resource invoke: --secret requires ENV=<secret-id>")
				os.Exit(1)
			}
			pair := args[i+1]
			eq := strings.IndexByte(pair, '=')
			if eq < 1 || eq == len(pair)-1 {
				fmt.Fprintf(os.Stderr, "hearth resource invoke: --secret expects ENV=<secret-id>, got %q\n", pair)
				os.Exit(1)
			}
			bindings[pair[:eq]] = pair[eq+1:]
			i++
			continue
		}
		positional = append(positional, args[i])
	}
	if len(positional) < 2 || len(positional) > 3 {
		fmt.Fprintln(os.Stderr, "Usage: hearth resource invoke <connection-id> <verb> [args-json] [--secret ENV=<id> ...]")
		os.Exit(1)
	}
	connID := positional[0]
	verb := positional[1]
	var argsJSON json.RawMessage
	if len(positional) == 3 {
		raw := json.RawMessage(positional[2])
		var probe interface{}
		if err := json.Unmarshal(raw, &probe); err != nil {
			fmt.Fprintf(os.Stderr, "hearth resource: args-json is not valid JSON: %v\n", err)
			os.Exit(1)
		}
		argsJSON = raw
	}

	req := ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: connID,
		ResourceVerb:         verb,
		ResourceArgs:         argsJSON,
		SecretBindings:       bindings,
	}
	// Phase 2 of docs/agent-identity-plan.md: the CLI no longer
	// forwards HEARTH_AGENT_INSTANCE_ID as a principal claim. The
	// daemon derives the calling agent's identity from the IPC
	// socket's peer credentials + a process-tree walk; an env-var
	// claim was forgeable by same-UID siblings and is no longer
	// authoritative. The env var still rides in the agent's
	// environment as an informational signal (the agent can log
	// "I am instance X"), but it doesn't drive authz.
	resp, err := sendResourceInvokeIPC(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth resource: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		exitForPluginErrorCode(resp.ResourceErrCode, resp.Message)
	}

	// Plugin stdout passes through verbatim — no trailing-newline
	// normalization. Callers piping this into jq / scripts get exactly
	// what the plugin emitted.
	fmt.Print(resp.ResourceStdout)
	os.Exit(resp.ResourceExitCode)
}

// exitForPluginErrorCode maps the daemon-reported PluginError code
// string to a CLI exit code and stderr message. Keeps exit 1
// reserved for client-side argument errors (unknown subcommand,
// malformed args-json) so callers can distinguish "user typo" from
// "plugin said no" from "plugin process dead." See
// docs/resource-plugins-1c-plan.md §5.
//
// Does not return.
func exitForPluginErrorCode(code, message string) {
	exit := 2
	prefix := "error"
	switch ErrorCode(code) {
	case ErrBadArgs:
		prefix = "bad_args"
	case ErrUnauthorized:
		prefix = "unauthorized"
	case ErrUnavailable:
		prefix = "unavailable"
	case ErrForbidden:
		prefix = "forbidden"
	case ErrInternal:
		prefix = "internal"
	case ErrTransport:
		prefix = "transport"
		exit = 3
	}
	if message == "" {
		fmt.Fprintf(os.Stderr, "hearth resource: %s\n", prefix)
	} else {
		fmt.Fprintf(os.Stderr, "hearth resource: %s: %s\n", prefix, message)
	}
	os.Exit(exit)
}

// runResourceRefresh fires the manifest's snapshot HTTP for the
// named declarative connection, replacing the daemon-local entity
// cache. Reuses the --secret ENV=<id> shape from invoke so the
// snapshot's upstream token is wired the same way as verb credentials.
// Exit codes match exitForPluginErrorCode's mapping.
func runResourceRefresh(args []string) {
	positional := []string{}
	bindings := map[string]string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "--secret" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hearth resource refresh: --secret requires ENV=<secret-id>")
				os.Exit(1)
			}
			pair := args[i+1]
			eq := strings.IndexByte(pair, '=')
			if eq < 1 || eq == len(pair)-1 {
				fmt.Fprintf(os.Stderr, "hearth resource refresh: --secret expects ENV=<secret-id>, got %q\n", pair)
				os.Exit(1)
			}
			bindings[pair[:eq]] = pair[eq+1:]
			i++
			continue
		}
		positional = append(positional, args[i])
	}
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: hearth resource refresh <connection-id> [--secret ENV=<id> ...]")
		os.Exit(1)
	}
	req := ipcRequest{
		Type:                 "resource_refresh",
		ResourceConnectionID: positional[0],
		SecretBindings:       bindings,
	}
	// Principal identity is daemon-derived per Phase 2 of
	// docs/agent-identity-plan.md; CLI no longer forwards
	// HEARTH_AGENT_INSTANCE_ID as a claim.
	resp, err := sendResourceInvokeIPC(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth resource: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		exitForPluginErrorCode(resp.ResourceErrCode, resp.Message)
	}
	fmt.Printf("refreshed %s: %d entities\n", positional[0], resp.EntityCount)
}

// runResourceList prints connections backed by plugins on this host.
// The daemon round-trips to the server with its own host_id as the
// filter so the result is always fresh — no reliance on the daemon's
// in-memory connection cache. Output is the raw server envelope,
// pretty-printed JSON, matching the `hearth hh <entity> list`
// convention.
func runResourceList(args []string) {
	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "hearth resource list: unexpected args %v\n", args)
		os.Exit(1)
	}
	resp, err := sendResourceInvokeIPC(ipcRequest{Type: "resource_list"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth resource: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth resource: %s\n", resp.Message)
		os.Exit(1)
	}
	printJSON(resp.Data)
}

// sendResourceInvokeIPC dials the daemon's unix socket, sends one
// ipcRequest line, and reads one ipcResponse line. Mirrors the
// pattern in organization.go's sendWSRequest but doesn't wrap a
// ws_request — we're talking directly to the daemon, not through
// it to the server.
func sendResourceInvokeIPC(req ipcRequest) (*ipcResponse, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to daemon: %v\nRun 'hearth start' first", err)
	}
	defer conn.Close()

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %v", err)
	}
	reqBytes = append(reqBytes, '\n')
	if _, err := conn.Write(reqBytes); err != nil {
		return nil, fmt.Errorf("send request: %v", err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read response: %v", err)
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %v", err)
	}
	return &resp, nil
}
