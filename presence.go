package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// runPresence implements `hearth presence assert` (local/presence rung LP2,
// docs/local-presence-rung-plan.md). Agent-invoked from a voice turn: when a
// caller identifies themselves at a shared puck ("I'm Matt"), the agent resolves
// the spoken name to a household member id (browsing members — discovery) and
// asserts presence. The relay then fast-lanes that person's phone to the front of
// any approval this session raises. The claim is cheap and unauthenticated by
// design; the real gate is the person's own phone biometric.
//
// Attributed to the calling agent by the daemon's principal derivation, so the
// relay refuses it for a human at the terminal ("assert_presence is for agents").
func runPresence(args []string) {
	if len(args) == 0 || args[0] != "assert" {
		fmt.Fprintln(os.Stderr, "usage: hearth presence assert --human <human_user_id>")
		os.Exit(1)
	}
	fs := flag.NewFlagSet("hearth presence assert", flag.ExitOnError)
	human := fs.String("human", "", "human_user_id of the person physically present (resolve their spoken name via `hearth hh user list`).")
	fs.Parse(args[1:])

	if *human == "" {
		fmt.Fprintln(os.Stderr, "hearth presence assert: provide --human <human_user_id>")
		os.Exit(1)
	}

	data, err := sendWSRequest("assert_presence", map[string]interface{}{"human_user_id": *human})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth presence assert: %v\n", err)
		os.Exit(1)
	}
	var resp struct {
		OK                  bool   `json:"ok"`
		IsPotentialApprover bool   `json:"is_potential_approver"`
		Error               string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "hearth presence assert: could not read response: %v\n", err)
		os.Exit(1)
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "hearth presence assert: %s\n", resp.Error)
		os.Exit(1)
	}
	// Inline echo so the agent's turn proceeds knowing what to say to the caller.
	if resp.IsPotentialApprover {
		fmt.Println("Presence noted. If anything this session needs approval, I'll ask you first — on your phone.")
	} else {
		fmt.Println("Presence noted — but you're not set up to approve actions in this household, so if something needs sign-off I'll ask an authorized approver instead.")
	}
}
