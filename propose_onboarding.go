package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

// runProposeOnboarding implements `hearth propose-onboarding` (onboarding P2,
// docs/onboarding-plan.md). Invoked by the Onboarding Facilitator agent: emit a
// reasoned provisioning proposal for a new hire. The agent PROPOSES; a human
// reviews and applies it from the app. The proposal is inert until then — the
// relay refuses `apply` from any agent principal, so this can never mint on its
// own. The relay also validates every proposed item against the real household
// catalog before persisting, so an item can only target an entity that exists.
//
// Items are read from a FILE (--items-file) or STDIN by preference, NOT inlined
// as --items. That is load-bearing, not cosmetic: the items are a JSON array
// full of double-quotes, and the interpose hook that gates an agent's exec does
// not JSON-escape argv — a quote-bearing argument corrupts the hook's request,
// the daemon can't parse it, and the exec is DENIED (exit 126) before any rule
// or ask. Passing a path (no embedded quotes) sidesteps that entirely. --items
// is kept for humans and back-compat, but the Facilitator prompt teaches
// --items-file.
func runProposeOnboarding(args []string) {
	fs := flag.NewFlagSet("hearth propose-onboarding", flag.ExitOnError)
	itemsJSON := fs.String("items", "", "JSON array of proposed items inline. Prefer --items-file or stdin — an inline array of quoted JSON can be mangled by the agent exec gate.")
	itemsFile := fs.String("items-file", "", "Path to a file holding the JSON array of proposed items. Preferred over --items. Each item: {op, primitive, risk, why, fields{...}}.")
	rationale := fs.String("rationale", "", "One-paragraph rationale for the bundle, shown to the person who reviews it.")
	intentTargetID := fs.String("intent-target-id", "", "The entity being onboarded (e.g. the new agent's position id).")
	// SHIPPING THESE? The Facilitator is not yet told to use them. The prompt
	// line lives in relay/cmd/hearth-cloud/provisioning_prompts.go
	// (facilitatorHowTo) and was held back because this parses with
	// flag.ExitOnError — on a daemon predating these flags, an unknown flag does
	// not degrade, it exits(2) and takes the whole proposal down. Add the prompt
	// line back in the release that puts this binary on hosts, not before.
	sourceBlueprint := fs.String("source-blueprint", "", "The published blueprint this bundle was drawn from, e.g. verge_labs/dj. Provenance only — it records where the household's shape came from and binds nothing.")
	sourceBlueprintVersion := fs.String("source-blueprint-version", "", "The version of that blueprint, when known.")
	fs.Parse(args)

	items, err := readProposeItems(*itemsFile, *itemsJSON, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth propose-onboarding: %v\n", err)
		os.Exit(1)
	}

	payload, err := buildProposeOnboardingPayload(items, *rationale, *intentTargetID, *sourceBlueprint, *sourceBlueprintVersion)
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

// readProposeItems resolves the items JSON from, in order of preference:
// --items-file (a path), then --items (inline), then stdin when it is piped
// (not a terminal). Returns the raw JSON text; validation happens in
// buildProposeOnboardingPayload. stdin is the belt-and-suspenders path so a
// piped `... < items.json` also works; a bare invocation with a terminal stdin
// errors rather than blocking.
func readProposeItems(itemsFile, itemsInline string, stdin *os.File) (string, error) {
	if itemsFile != "" {
		b, err := os.ReadFile(itemsFile)
		if err != nil {
			return "", fmt.Errorf("--items-file: %v", err)
		}
		return string(b), nil
	}
	if itemsInline != "" {
		return itemsInline, nil
	}
	// Fall back to stdin only when it is piped/redirected, never a live
	// terminal (which would hang waiting for input).
	if fi, err := stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("reading items from stdin: %v", err)
		}
		return string(b), nil
	}
	return "", fmt.Errorf("no items given; pass --items-file <path>, pipe the JSON on stdin, or use --items")
}

// buildProposeOnboardingPayload validates the items text is a non-empty JSON
// array and assembles the ws_request payload. proposer_kind and the proposing
// agent's id are stamped SERVER-SIDE from the caller principal, never trusted
// from the client — this verb only carries the reasoned items.
func buildProposeOnboardingPayload(itemsJSON, rationale, intentTargetID, sourceBlueprint, sourceBlueprintVersion string) (map[string]interface{}, error) {
	if itemsJSON == "" {
		return nil, fmt.Errorf("items are empty (a JSON array is required)")
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(itemsJSON), &items); err != nil {
		return nil, fmt.Errorf("items must be a JSON array: %v", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("items array is empty; propose at least one item")
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
	// Provenance (docs/blueprints.md §9). Recorded rather than derived because it
	// cannot be reconstructed later: once a household is built there is nothing
	// in the graph that says which pattern it came from. A version with no slug
	// would say nothing, so it only rides along with one.
	if sourceBlueprint != "" {
		payload["source_blueprint"] = sourceBlueprint
		if sourceBlueprintVersion != "" {
			payload["source_blueprint_version"] = sourceBlueprintVersion
		}
	}
	return payload, nil
}
