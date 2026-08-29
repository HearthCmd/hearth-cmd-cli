package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// runAcquire implements `hearth acquire` (CP2, docs/capability-provisioning.md).
// Agent-invoked: request a capability the agent can't currently reach. The call
// blocks while a human approves (the relay parks the ask up to its household-ask
// window), then prints the how-to inline so the agent's turn resumes competent.
func runAcquire(args []string) {
	fs := flag.NewFlagSet("hearth acquire", flag.ExitOnError)
	kind := fs.String("kind", "resource", "What to acquire: 'resource' = a connected service (Home Assistant, a calendar, …); 'talk' = permission to message another household agent.")
	id := fs.String("id", "", "Exact id of the target (from browsing, if you have discovery access).")
	name := fs.String("name", "", "Exact name of the target — e.g. --name \"Family Calendar\" (alternative to --id).")
	reason := fs.String("reason", "", "Why you need it. Shown to the person who approves.")
	fs.Parse(args)

	if *id == "" && *name == "" {
		fmt.Fprintln(os.Stderr, "hearth acquire: provide --id or --name of the thing to acquire")
		os.Exit(1)
	}

	payload := map[string]interface{}{"kind": *kind}
	if *id != "" {
		payload["id"] = *id
	}
	if *name != "" {
		payload["name"] = *name
	}
	if *reason != "" {
		payload["reason"] = *reason
	}

	// 13 min > the daemon/relay household-ask window (12 min), so the CLI outlives
	// the server-side wait rather than timing out mid-approval.
	data, err := sendWSRequestDeadline("acquire", payload, 13*time.Minute)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth acquire: %v\n", err)
		os.Exit(1)
	}

	var resp struct {
		Granted        bool   `json:"granted"`
		AlreadyGranted bool   `json:"already_granted"`
		Kind           string `json:"kind"`
		// resource kind
		ConnectionName string   `json:"connection_name"`
		ConnectionID   string   `json:"connection_id"`
		Verbs          []string `json:"verbs"`
		// talk kind
		TargetName string `json:"target_name"`
		TargetID   string `json:"target_id"`
		Mention    string `json:"mention"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "hearth acquire: could not read response: %v\n", err)
		os.Exit(1)
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "hearth acquire: %s\n", resp.Error)
		os.Exit(1)
	}
	if !resp.Granted {
		fmt.Fprintln(os.Stderr, "hearth acquire: not granted")
		os.Exit(1)
	}

	// Success — the inline competence echo (§3.3): tell the agent it now has the
	// capability and exactly how to use it, so the current turn proceeds.
	if resp.Kind == "talk" {
		name := resp.TargetName
		if name == "" {
			name = resp.TargetID
		}
		if resp.AlreadyGranted {
			fmt.Printf("You can already message %q.\n", name)
		} else {
			fmt.Printf("You can now message %q.\n", name)
		}
		fmt.Printf("Reach them by @mentioning %s in a chat room reply.\n", resp.Mention)
		return
	}

	target := resp.ConnectionName
	if target == "" {
		target = resp.ConnectionID
	}
	if resp.AlreadyGranted {
		fmt.Printf("You already have access to %q.\n", target)
	} else {
		fmt.Printf("Access granted to %q.\n", target)
	}
	fmt.Printf("Use it with:\n  hearth resource invoke %q <verb> '<json-args>'\n", target)
	if len(resp.Verbs) > 0 {
		fmt.Printf("Available verbs: %s\n", strings.Join(resp.Verbs, ", "))
	}
}
