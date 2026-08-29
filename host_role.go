package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// runHostRole implements `hearth hh host role <list|add|remove> [role] [--id <host>]`,
// the durable surface for a host's agent/display roles (docs/household-display-plan.md
// §1–2). It reads the current set via get_host, applies the change locally, and
// writes the whole set back via host_set_roles (host-owner gated server-side).
// --id defaults to this box's own host_id, so the common case is
// `hearth hh host role add display` run on the box itself.
func runHostRole(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: hearth hh host role <list|add|remove> [agent|display] [--id <host_id>]")
		os.Exit(1)
	}
	sub := args[0]

	var role string
	var flagArgs []string
	switch sub {
	case "add", "remove":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "hearth: a role is required (agent|display)")
			os.Exit(1)
		}
		role = args[1]
		flagArgs = args[2:]
	case "list":
		flagArgs = args[1:]
	default:
		fmt.Fprintf(os.Stderr, "hearth hh host role: unknown subcommand %q (list|add|remove)\n", sub)
		os.Exit(1)
	}

	fs := flag.NewFlagSet("host role", flag.ExitOnError)
	id := fs.String("id", "", "Host ID (defaults to this host)")
	fs.Parse(flagArgs)

	hostID := *id
	if hostID == "" {
		hostID = readConfigValue("host_id")
	}
	if hostID == "" {
		fmt.Fprintln(os.Stderr, "hearth: no --id given and no local host — run this on an enrolled host or pass --id")
		os.Exit(1)
	}

	current, err := hostRolesViaGet(hostID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	// A blank/legacy column comes back empty over the wire; treat it as agent
	// so `list` and remove-math match the server's own normalization.
	if len(current) == 0 {
		current = []string{"agent"}
	}

	if sub == "list" {
		fmt.Println(strings.Join(current, ", "))
		return
	}

	if role != "agent" && role != "display" {
		fmt.Fprintf(os.Stderr, "hearth: unknown role %q (agent|display)\n", role)
		os.Exit(1)
	}

	next := applyHostRoleChange(current, sub, role)
	if len(next) == 0 {
		fmt.Fprintln(os.Stderr, "hearth: refusing to remove the last role — a host must keep at least one (agent|display)")
		os.Exit(1)
	}

	data, err := sendWSRequest("host_set_roles", map[string]interface{}{
		"host_id": hostID,
		"roles":   next,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	// host_set_roles surfaces failures (not-yours, empty) as an "error" field in
	// the data, not as a transport error — check it like every other CRUD reply.
	var resp struct {
		Error string   `json:"error"`
		Roles []string `json:"roles"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "hearth: %s\n", resp.Error)
		os.Exit(1)
	}
	fmt.Printf("host %s roles: %s\n", hostID, strings.Join(resp.Roles, ", "))

	// When the change targets THIS box, keep the local roles hint in sync so the
	// daemon activates the right subsystems on its next start, and tell the user a
	// restart is needed to apply (the running daemon reads the hint only at boot).
	if hostID == readConfigValue("host_id") {
		writeConfigValue("roles", strings.Join(resp.Roles, ","))
		fmt.Fprintln(os.Stderr,
			"Restart the daemon to apply: `systemctl --user restart hearth` (or `hearth stop && hearth start`).")
	}
}

// hostRolesViaGet fetches a host's current role set through get_host.
func hostRolesViaGet(hostID string) ([]string, error) {
	data, err := sendWSRequest("get_host", map[string]interface{}{"id": hostID})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Error string `json:"error"`
		Host  struct {
			Roles []string `json:"roles"`
		} `json:"host"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse host: %v", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Host.Roles, nil
}

// applyHostRoleChange returns the role set after an add or remove, de-duplicated
// and order-preserving. It does not enforce non-emptiness — the caller refuses
// removing the last role, and the server independently refuses an empty set.
func applyHostRoleChange(current []string, op, role string) []string {
	out := make([]string, 0, len(current)+1)
	seen := map[string]bool{}
	for _, r := range current {
		if op == "remove" && r == role {
			continue
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	if op == "add" && !seen[role] {
		out = append(out, role)
	}
	return out
}
