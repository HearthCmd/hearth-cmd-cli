package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// runProposeOnboarding implements `hearth propose-onboarding` (onboarding P2,
// docs/onboarding-plan.md). Invoked by the Onboarding Facilitator agent: emit a
// reasoned provisioning proposal for a new hire. The agent PROPOSES; a human
// reviews and applies it from the app. The proposal is inert until then — the
// relay refuses `apply` from any agent principal, so this can never mint on its
// own. The relay also validates every proposed item against the real household
// catalog before persisting, so an item can only target an entity that exists.
func runProposeOnboarding(args []string) {
	fs := flag.NewFlagSet("hearth propose-onboarding", flag.ExitOnError)
	itemsJSON := fs.String("items", "", "JSON array of proposed items (required). Each item: {op, primitive, risk, why, fields{...}}.")
	rationale := fs.String("rationale", "", "One-paragraph rationale for the bundle, shown to the person who reviews it.")
	intentTargetID := fs.String("intent-target-id", "", "The entity being onboarded (e.g. the new agent's position id).")
	fs.Parse(args)

	payload, err := buildProposeOnboardingPayload(*itemsJSON, *rationale, *intentTargetID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth propose-onboarding: %v\n", err)
		os.Exit(1)
	}

	data, err := sendWSRequest("provisioning_agent_propose", payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth propose-onboarding: %v\n", err)
		os.Exit(1)
	}

	var resp struct {
		Proposal struct {
			ID    string            `json:"id"`
			Items []json.RawMessage `json:"items"`
		} `json:"provisioning_proposal"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "hearth propose-onboarding: could not read response: %v\n", err)
		os.Exit(1)
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "hearth propose-onboarding: %s\n", resp.Error)
		os.Exit(1)
	}

	fmt.Printf("Proposed %d item(s) for review (proposal %s).\n", len(resp.Proposal.Items), resp.Proposal.ID)
	fmt.Println("A person will review and apply them from the app — you can't apply them yourself.")
}

// buildProposeOnboardingPayload validates --items is a non-empty JSON array and
// assembles the ws_request payload. proposer_kind and the proposing agent's id
// are stamped SERVER-SIDE from the caller principal, never trusted from the
// client — this verb only carries the reasoned items.
func buildProposeOnboardingPayload(itemsJSON, rationale, intentTargetID string) (map[string]interface{}, error) {
	if itemsJSON == "" {
		return nil, fmt.Errorf("--items is required (a JSON array of proposed items)")
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(itemsJSON), &items); err != nil {
		return nil, fmt.Errorf("--items must be a JSON array: %v", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("--items is empty; propose at least one item")
	}
	payload := map[string]interface{}{
		"intent_kind": "entity",
		"items":       items,
	}
	if rationale != "" {
		payload["rationale"] = rationale
	}
	if intentTargetID != "" {
		payload["intent_target_id"] = intentTargetID
	}
	return payload, nil
}
