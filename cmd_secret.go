//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// runSecret dispatches `hearth secret <subcommand> ...`. Operator
// surface for the host's secrets vault.
//
// Wire reshape: secrets are now labeled IAM-gated blobs. No more
// scope/connection/key — just a name + optional purpose. Who can use
// a secret is governed by the rules table (resource_kind='secret',
// action='secret.use'); grant/revoke insert/remove rows.
func runSecret(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printSecretUsage()
		if len(args) == 0 {
			os.Exit(1)
		}
		return
	}
	switch args[0] {
	case "list":
		runSecretList(args[1:])
	case "set":
		runSecretSet(args[1:])
	case "delete":
		runSecretDelete(args[1:])
	case "grant":
		runSecretGrant(args[1:])
	case "revoke":
		runSecretRevoke(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "hearth secret: unknown subcommand %q\n", args[0])
		printSecretUsage()
		os.Exit(1)
	}
}

func printSecretUsage() {
	fmt.Fprint(os.Stderr, `Usage: hearth secret <subcommand> [args]

Subcommands:
  list
      List secrets pinned to this host. Cleartext is never returned.

  set <name> [--value <v>] [--purpose <text>]
      Encrypt a credential value to this host's pubkey and store it
      server-side. Without --value, reads cleartext from stdin
      (prompted on a TTY; piped input otherwise). --purpose is
      optional FYI text the agent will see in its prompt.

  delete <id>
      Remove a secret by its server-issued id (from 'hearth secret list').
      Cascades the IAM rules that referenced it.

  grant <id> --to <kind>:<principal-id>
      Add an IAM rule allowing <principal> to use the secret.
      <kind> is 'human' or 'agent'.

  revoke <id> --from <kind>:<principal-id>
      Remove the IAM rule from grant. Refuses to revoke the secret's
      own creator (delete the secret instead).

Authorization to USE a secret is governed entirely by IAM rules:
  resource_kind='secret', resource_id=<id>, action='secret.use'
The creator gets an automatic allow rule at set time. Use grant to
share access with other principals.
`)
}

func runSecretList(args []string) {
	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "hearth secret list: unexpected args %v\n", args)
		os.Exit(1)
	}
	resp, err := sendSecretIPC(ipcRequest{Type: "secret_list"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth secret: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth secret: %s\n", resp.Message)
		os.Exit(1)
	}
	var inner struct {
		Secrets []map[string]interface{} `json:"secrets"`
	}
	if err := json.Unmarshal(resp.Data, &inner); err != nil {
		fmt.Fprintf(os.Stderr, "hearth secret: decode list: %v\n", err)
		os.Exit(1)
	}
	if len(inner.Secrets) == 0 {
		fmt.Fprintln(os.Stderr, "(no secrets configured)")
		return
	}
	for _, s := range inner.Secrets {
		setBy := ""
		if k := firstNonEmpty(s["set_by_kind"], ""); k != "" {
			setBy = k + ":" + firstNonEmpty(s["set_by_id"], "?")
		}
		fmt.Printf("%-40s %-20s purpose=%q set_by=%s set=%s\n",
			firstNonEmpty(s["id"], "?"),
			firstNonEmpty(s["name"], "?"),
			firstNonEmpty(s["purpose"], ""),
			firstNonEmpty(setBy, "unknown"),
			firstNonEmpty(s["set_at"], "unknown"),
		)
	}
}

func firstNonEmpty(v interface{}, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}

func runSecretSet(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: hearth secret set <name> [--value <v>] [--purpose <text>]")
		os.Exit(1)
	}
	name := args[0]
	value := ""
	purpose := ""
	hasInlineValue := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--value":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hearth secret set: --value requires a value")
				os.Exit(1)
			}
			value = args[i+1]
			hasInlineValue = true
			i++
		case "--purpose":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hearth secret set: --purpose requires text")
				os.Exit(1)
			}
			purpose = args[i+1]
			i++
		default:
			fmt.Fprintf(os.Stderr, "hearth secret set: unexpected arg %q\n", args[i])
			os.Exit(1)
		}
	}
	if !hasInlineValue {
		v, err := readSecretFromStdin()
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth secret: read: %v\n", err)
			os.Exit(1)
		}
		value = v
	}
	if value == "" {
		fmt.Fprintln(os.Stderr, "hearth secret: refusing to store empty value")
		os.Exit(1)
	}

	resp, err := sendSecretIPC(ipcRequest{
		Type:          "secret_set",
		SecretName:    name,
		SecretPurpose: purpose,
		SecretValue:   value,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth secret: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth secret: %s\n", resp.Message)
		os.Exit(1)
	}
	var inner struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.Unmarshal(resp.Data, &inner)
	fmt.Fprintf(os.Stderr, "set %s (%s)\n", inner.Name, inner.ID)
}

func runSecretDelete(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: hearth secret delete <id>")
		fmt.Fprintln(os.Stderr, "  Find the id with: hearth secret list")
		os.Exit(1)
	}
	resp, err := sendSecretIPC(ipcRequest{
		Type:     "secret_delete",
		SecretID: args[0],
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth secret: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth secret: %s\n", resp.Message)
		os.Exit(1)
	}
	var inner struct {
		Deleted int `json:"deleted"`
	}
	_ = json.Unmarshal(resp.Data, &inner)
	fmt.Fprintf(os.Stderr, "deleted %d row(s)\n", inner.Deleted)
}

func runSecretGrant(args []string) {
	id, kind, principal, err := parseGrantRevokeArgs(args, "--to")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth secret grant: %v\n", err)
		fmt.Fprintln(os.Stderr, "Usage: hearth secret grant <id> --to <kind>:<principal-id>")
		os.Exit(1)
	}
	resp, err := sendSecretIPC(ipcRequest{
		Type:                     "secret_grant",
		SecretID:                 id,
		SecretGrantPrincipalKind: kind,
		SecretGrantPrincipalID:   principal,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth secret: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth secret: %s\n", resp.Message)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "granted %s to %s:%s\n", id, kind, principal)
}

func runSecretRevoke(args []string) {
	id, kind, principal, err := parseGrantRevokeArgs(args, "--from")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth secret revoke: %v\n", err)
		fmt.Fprintln(os.Stderr, "Usage: hearth secret revoke <id> --from <kind>:<principal-id>")
		os.Exit(1)
	}
	resp, err := sendSecretIPC(ipcRequest{
		Type:                     "secret_revoke",
		SecretID:                 id,
		SecretGrantPrincipalKind: kind,
		SecretGrantPrincipalID:   principal,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth secret: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth secret: %s\n", resp.Message)
		os.Exit(1)
	}
	var inner struct {
		Revoked int `json:"revoked"`
	}
	_ = json.Unmarshal(resp.Data, &inner)
	fmt.Fprintf(os.Stderr, "revoked %d rule(s)\n", inner.Revoked)
}

// parseGrantRevokeArgs handles the shared `<id> --to <kind>:<id>` /
// `<id> --from <kind>:<id>` arg shape. flagName is "--to" or "--from".
// Returns (secretID, kind, principalID).
func parseGrantRevokeArgs(args []string, flagName string) (string, string, string, error) {
	if len(args) < 3 {
		return "", "", "", fmt.Errorf("expected <id> %s <kind>:<principal-id>", flagName)
	}
	id := args[0]
	if args[1] != flagName {
		return "", "", "", fmt.Errorf("expected %s flag, got %q", flagName, args[1])
	}
	principalSpec := args[2]
	colon := strings.IndexByte(principalSpec, ':')
	if colon < 1 || colon == len(principalSpec)-1 {
		return "", "", "", fmt.Errorf("principal spec must be <kind>:<id>, got %q", principalSpec)
	}
	kind := principalSpec[:colon]
	principalID := principalSpec[colon+1:]
	if kind != "human" && kind != "agent" {
		return "", "", "", fmt.Errorf("principal kind must be 'human' or 'agent', got %q", kind)
	}
	return id, kind, principalID, nil
}

// readSecretFromStdin prompts the operator on a TTY (without echoing
// the input) or reads piped input verbatim. Trims a trailing newline
// so `echo foo | hearth secret set ...` works cleanly.
func readSecretFromStdin() (string, error) {
	stat, _ := os.Stdin.Stat()
	if stat.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "Enter secret value: ")
	}
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// sendSecretIPC is the standard dial-and-read pattern from
// cmd_resource.go's helper, lifted to avoid cross-file dep.
func sendSecretIPC(req ipcRequest) (*ipcResponse, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to daemon: %v\nRun 'hearth start' first", err)
	}
	defer conn.Close()

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %v", err)
	}
	reqBytes = append(reqBytes, '\n')
	if _, err := conn.Write(reqBytes); err != nil {
		return nil, fmt.Errorf("send: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read: %v", err)
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode: %v", err)
	}
	return &resp, nil
}
