//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sendWSRequest connects to the daemon IPC socket and sends a ws_request,
// returning the raw JSON response from the server.
func sendWSRequest(msgType string, data map[string]interface{}) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to daemon: %v\nRun 'hearth start' first", err)
	}
	defer conn.Close()

	var payload json.RawMessage
	if len(data) > 0 {
		payload, err = json.Marshal(data)
		if err != nil {
			return nil, err
		}
	}

	req := ipcRequest{
		Type:      "ws_request",
		WSMsgType: msgType,
		WSData:    payload,
	}
	reqBytes, _ := json.Marshal(req)
	reqBytes = append(reqBytes, '\n')
	if _, err := conn.Write(reqBytes); err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}

	// 30s read deadline matches the daemon-side WS SendWSRequest
	// default — long enough for any normal WS round-trip, short
	// enough that a hung daemon surfaces as an error instead of
	// the CLI appearing to hang forever.
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %v", err)
	}

	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("invalid daemon response: %v", err)
	}
	if resp.Type == "error" {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return resp.Data, nil
}

// runArchivalWithCascadePrompt sends a ws_request that may be blocked by
// live dependents (eliminate/archive/abandon). On a has_live_dependents
// reply it prints the blocker tree, prompts [y/N], and retries once with
// cascade=true. Any other error is surfaced verbatim.
func runArchivalWithCascadePrompt(msgType, entityLabel, id string) {
	data, err := sendWSRequest(msgType, map[string]interface{}{"id": id})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	var envelope struct {
		Error    string          `json:"error"`
		Blockers json.RawMessage `json:"blockers"`
	}
	_ = json.Unmarshal(data, &envelope)
	if envelope.Error != "has_live_dependents" {
		printJSON(data)
		return
	}

	fmt.Fprintf(os.Stderr, "Cannot %s %s %s — live dependents:\n", msgType, entityLabel, id)
	printBlockerTree(envelope.Blockers)
	reader := bufio.NewReader(os.Stdin)
	ans := strings.ToLower(strings.TrimSpace(promptLine(reader, "Retire/eliminate them and continue? [y/N]: ")))
	if ans != "y" && ans != "yes" {
		fmt.Fprintln(os.Stderr, "aborted")
		os.Exit(1)
	}

	data, err = sendWSRequest(msgType, map[string]interface{}{"id": id, "cascade": true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	printJSON(data)
}

// runCreateOrUpdateWithCollisionPrompt sends a create/update ws_request that
// may be rejected by a name-uniqueness constraint. On a name_collision reply
// it prompts the user with four choices:
//
//  1. Use a different name for the new row
//  2. Soft-delete the colliding row and continue
//  3. Rename the colliding row and continue
//  4. Cancel
//
// Exactly one retry is sent. Any error that isn't name_collision is printed
// verbatim; the second-attempt error (if the remediation itself collides) is
// surfaced without a third prompt.
//
// nameField is the payload key carrying the name the user picked (e.g.
// "title" for job_description). entityLabel is a human-readable noun used in
// prompts. removeVerb is the verb used for option 2 ("remove", "archive",
// "abandon", "eliminate", "retire") matching the entity's soft-delete term.
func runCreateOrUpdateWithCollisionPrompt(msgType, entityLabel, removeVerb, nameField string, payload map[string]interface{}) {
	data := sendCreateOrUpdateResolvingCollision(msgType, entityLabel, removeVerb, nameField, payload)
	printJSON(data)
}

// sendCreateOrUpdateResolvingCollision is the same flow but returns the final
// response body instead of printing it, so callers with their own
// post-processing (e.g. agent create launching `talk` on the new row) can
// still use the collision prompt without losing control of stdout. Exits
// the process on transport errors or user cancellation.
func sendCreateOrUpdateResolvingCollision(msgType, entityLabel, removeVerb, nameField string, payload map[string]interface{}) json.RawMessage {
	reader := bufio.NewReader(os.Stdin)
	// pendingCascadeKey records which payload field most recently received a
	// "remove_conflicting" directive, so that if the server comes back with
	// has_live_dependents we know whether to upgrade remediate_collision or
	// remediate_slot_collision into the cascade-enabled form.
	pendingCascadeKey := ""

	for {
		data, err := sendWSRequest(msgType, payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		var env struct {
			Error     string `json:"error"`
			Collision struct {
				Kind            string `json:"kind"`
				ID              string `json:"id"`
				Name            string `json:"name"`
				SuggestedRename string `json:"suggested_rename"`
				// Zero value means "rename is supported" so old servers (which
				// always sent rename-enabled envelopes) still get the legacy UX.
				// New servers set this to false for entities whose identity IS
				// the user-visible name (working_directory).
				RenameSupported *bool `json:"rename_supported,omitempty"`
				// Populated for slot_collision responses.
				SlotKind string `json:"slot_kind"`
				SlotID   string `json:"slot_id"`
			} `json:"collision"`
			Blockers json.RawMessage `json:"blockers"`
		}
		_ = json.Unmarshal(data, &env)

		switch env.Error {
		case "name_collision":
			pendingCascadeKey = promptNameCollision(reader, env.Collision.ID, env.Collision.Name, env.Collision.SuggestedRename, env.Collision.RenameSupported, payload, entityLabel, removeVerb, nameField)
		case "slot_collision":
			promptSlotCollision(reader, env.Collision.ID, env.Collision.Name, env.Collision.SlotKind, payload, entityLabel, removeVerb)
			pendingCascadeKey = "remediate_slot_collision"
		case "has_live_dependents":
			// We only get here right after sending a remove_conflicting
			// remediation. pendingCascadeKey tells us which one.
			if pendingCascadeKey == "" {
				return data // unexpected shape; surface as-is
			}
			fmt.Fprintf(os.Stderr, "\n%s the existing %s would affect live children:\n",
				titleCase(removeVerb), entityLabel)
			printBlockerTree(env.Blockers)
			ans := strings.ToLower(strings.TrimSpace(
				promptLine(reader, "Retire/eliminate them and continue? [y/N]: ")))
			if ans != "y" && ans != "yes" {
				fmt.Fprintln(os.Stderr, "aborted")
				os.Exit(1)
			}
			payload[pendingCascadeKey] = map[string]interface{}{
				"remove_conflicting": true,
				"cascade_children":   true,
			}
			pendingCascadeKey = ""
		default:
			return data
		}
	}
}

// promptNameCollision prints the four-option name-collision menu, mutates
// payload with the user's choice, and returns the payload key that was set
// to a remove_conflicting directive (for cascade follow-up) — or "" if the
// choice (new name / rename) won't trigger has_live_dependents. Exits on
// cancel.
func promptNameCollision(reader *bufio.Reader, conflictID, conflictName, suggestedRename string, renameSupported *bool, payload map[string]interface{}, entityLabel, removeVerb, nameField string) string {
	renameOK := renameSupported == nil || *renameSupported

	attempted, _ := payload[nameField].(string)
	fmt.Fprintf(os.Stderr, "\n%q is already the %s of another %s (id %s).\n\n",
		attempted, nameField, entityLabel, conflictID)
	fmt.Fprintf(os.Stderr, "  1) Use a different %s for the new %s\n", nameField, entityLabel)
	fmt.Fprintf(os.Stderr, "  2) %s the existing %s and continue\n", titleCase(removeVerb), entityLabel)
	if renameOK {
		fmt.Fprintf(os.Stderr, "  3) Rename the existing %s and continue\n", entityLabel)
		fmt.Fprintf(os.Stderr, "  4) Cancel\n")
	} else {
		fmt.Fprintf(os.Stderr, "  3) Cancel\n")
	}

	promptRange := "1-4"
	if !renameOK {
		promptRange = "1-3"
	}
	choice := strings.TrimSpace(promptLine(reader, fmt.Sprintf("Select [%s]: ", promptRange)))

	switch {
	case choice == "1":
		newName := strings.TrimSpace(promptLine(reader, fmt.Sprintf("New %s: ", nameField)))
		if newName == "" {
			fmt.Fprintln(os.Stderr, "aborted")
			os.Exit(1)
		}
		payload[nameField] = newName
		// Previously-set remediation may no longer apply; clear it.
		delete(payload, "remediate_collision")
		return ""
	case choice == "2":
		payload["remediate_collision"] = "remove_conflicting"
		return "remediate_collision"
	case choice == "3" && renameOK:
		defaultRename := suggestedRename
		if defaultRename == "" {
			defaultRename = conflictName + " (old)"
		}
		got := strings.TrimSpace(promptLine(reader,
			fmt.Sprintf("New %s for existing %s [%s]: ", nameField, entityLabel, defaultRename)))
		if got == "" {
			got = defaultRename
		}
		payload["remediate_collision"] = map[string]interface{}{"rename_conflicting_to": got}
		return ""
	case choice == "3" && !renameOK, choice == "4" && renameOK, choice == "":
		fmt.Fprintln(os.Stderr, "aborted")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "aborted: unrecognized selection")
	os.Exit(1)
	return ""
}

// promptSlotCollision prints the two-option slot-collision menu and mutates
// payload with a remove_conflicting directive on remediate_slot_collision,
// or exits on cancel. Slot collisions don't support rename or "pick a
// different slot" in this flow — the slot FK came from the create/update
// payload itself; if the user wants a different one, they should re-run.
func promptSlotCollision(reader *bufio.Reader, conflictID, conflictName, slotKind string, payload map[string]interface{}, entityLabel, removeVerb string) {
	fmt.Fprintf(os.Stderr, "\nThat slot (%s) is already held by %s %q (id %s).\n\n",
		slotKind, entityLabel, conflictName, conflictID)
	fmt.Fprintf(os.Stderr, "  1) %s the existing %s and continue\n", titleCase(removeVerb), entityLabel)
	fmt.Fprintf(os.Stderr, "  2) Cancel\n")

	choice := strings.TrimSpace(promptLine(reader, "Select [1-2]: "))
	switch choice {
	case "1":
		payload["remediate_slot_collision"] = "remove_conflicting"
	case "2", "":
		fmt.Fprintln(os.Stderr, "aborted")
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "aborted: unrecognized selection")
		os.Exit(1)
	}
}

// titleCase uppercases the first rune of s for menu labels. Keeps us off
// strings.Title (deprecated) and avoids pulling in golang.org/x/text.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// printBlockerTree renders a blockers payload from the server in a
// human-readable tree. Accepts either {agents: [...]} (position case) or
// {positions: [{..., agents: [...]}]} (JD/WD case).
func printBlockerTree(raw json.RawMessage) {
	var b struct {
		Agents    []struct{ ID, Name string } `json:"agents"`
		Positions []struct {
			ID, Name string
			Agents   []struct{ ID, Name string } `json:"agents"`
		} `json:"positions"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		fmt.Fprintf(os.Stderr, "  (unparseable blockers: %s)\n", string(raw))
		return
	}
	for _, a := range b.Agents {
		fmt.Fprintf(os.Stderr, "  - agent %s (%s)\n", a.Name, a.ID)
	}
	for _, p := range b.Positions {
		fmt.Fprintf(os.Stderr, "  - position %s (%s)\n", p.Name, p.ID)
		for _, a := range p.Agents {
			fmt.Fprintf(os.Stderr, "      └─ agent %s (%s)\n", a.Name, a.ID)
		}
	}
}

// printJSON pretty-prints a JSON value to stdout.
func printJSON(v json.RawMessage) {
	var m interface{}
	if json.Unmarshal(v, &m) == nil {
		if pretty, err := json.MarshalIndent(m, "", "  "); err == nil {
			fmt.Println(string(pretty))
			return
		}
	}
	fmt.Println(string(v))
}

// workingOrgID returns the CLI device's current organization, as stored in
// io_devices.organization_id on the server. Source of truth is the server,
// not ~/.hearth/credentials — so a `hearth hh household switch` on one device
// doesn't need to hop through config to be visible here. Returns "" on any
// failure; the caller prints a generic "run 'hearth hh household switch'"
// hint so no error text is lost.
func workingOrgID() string {
	id, _ := workingOrgIDAndSlug()
	return id
}

// requireWorkingOrgID looks up the device's current org and exits
// with a friendly message when none is set. Distinguishes orphaned
// (zero memberships) from no-current-chosen (memberships exist but
// none flagged is_current) so the suggested fix is actually
// actionable — `hh household create` vs `hh household switch`. Used
// by every subcommand that needs an org context.
func requireWorkingOrgID() string {
	deviceID := readConfigValue("io_device_id")
	if deviceID == "" {
		fmt.Fprintln(os.Stderr, "hearth: not logged in (run 'hearth login')")
		os.Exit(1)
	}
	data, err := sendWSRequest("list_my_organizations", map[string]interface{}{
		"io_device_id": deviceID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	var resp struct {
		Organizations []struct {
			ID        string `json:"id"`
			IsCurrent bool   `json:"is_current"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	if len(resp.Organizations) == 0 {
		fmt.Fprintln(os.Stderr,
			"hearth: you're not a member of any household.\n"+
				"Create one with: hearth hh household create --name <name>")
		os.Exit(1)
	}
	for _, o := range resp.Organizations {
		if o.IsCurrent {
			return o.ID
		}
	}
	fmt.Fprintln(os.Stderr,
		"hearth: no current household set.\n"+
			"Pick one with: hearth hh household switch")
	os.Exit(1)
	return "" // unreachable
}

// workingOrgIDAndSlug returns both the id and the slug of the CLI device's
// current organization, in one round-trip. Slug is needed by call sites
// that build $HOME/hearth_agents/<slug>/... paths. Returns ("", "") on
// any failure.
func workingOrgIDAndSlug() (string, string) {
	deviceID := readConfigValue("io_device_id")
	if deviceID == "" {
		return "", ""
	}
	data, err := sendWSRequest("list_my_organizations", map[string]interface{}{
		"io_device_id": deviceID,
	})
	if err != nil {
		return "", ""
	}
	var resp struct {
		Organizations []struct {
			ID        string `json:"id"`
			Slug      string `json:"slug"`
			IsCurrent bool   `json:"is_current"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", ""
	}
	for _, o := range resp.Organizations {
		if o.IsCurrent {
			return o.ID, o.Slug
		}
	}
	return "", ""
}

// orgSlugForID returns the slug of the org with the given id, fetched via
// list_my_organizations. Returns "" if the caller isn't a member or the
// request fails — defaultAgentWorkingDir handles the empty case.
func orgSlugForID(orgID string) string {
	if orgID == "" {
		return ""
	}
	data, err := sendWSRequest("list_my_organizations", map[string]interface{}{})
	if err != nil {
		return ""
	}
	var resp struct {
		Organizations []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return ""
	}
	for _, o := range resp.Organizations {
		if o.ID == orgID {
			return o.Slug
		}
	}
	return ""
}

// =============================================================================
// runOrganization — entry point
// =============================================================================

func runOrganization(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printOrganizationUsage()
		os.Exit(0)
	}
	switch args[0] {
	case "household":
		runOrganizationOrg(args[1:])
	case "user":
		runOrganizationUser(args[1:])
	case "job_description":
		runOrganizationJob(args[1:])
	case "position":
		runOrganizationPos(args[1:])
	case "agent":
		runOrganizationAgent(args[1:])
	case "ai_model":
		runOrganizationModel(args[1:])
	case "host":
		runOrganizationHost(args[1:])
	case "device":
		runOrganizationDevice(args[1:])
	case "invite":
		runOrganizationInvite(args[1:])
	case "approve":
		runOrganizationApprove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "hearth hh: unknown entity %q\nRun 'hearth hh --help' for usage.\n", args[0])
		os.Exit(1)
	}
}

// runOrganizationApprove implements `hearth hh approve <request_id>
// <allow|deny> [--reason "..."]`. This is the wire shape for an
// agent's decision on a permission_request it's been designated to
// approve (approver-resolution phase 5b). The daemon derives the
// calling principal from the IPC connection's process tree; agent
// principals are forwarded as tool_approve_permission_request to the
// server. Operator-from-terminal use is currently rejected (the
// webview is the operator's approval surface).
func runOrganizationApprove(args []string) {
	fs := flag.NewFlagSet("hearth hh approve", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: hearth hh approve <request_id> <allow|deny> [--reason "..."]

Respond to a permission_request whose approver set includes the
calling agent. Membership in the approver set IS the authorization;
the server validates on the response.`)
	}
	var reason string
	fs.StringVar(&reason, "reason", "", "free-text rationale recorded in audit_log")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	positional := fs.Args()
	if len(positional) != 2 {
		fs.Usage()
		os.Exit(1)
	}
	requestID := positional[0]
	decision := positional[1]
	if decision != "allow" && decision != "deny" {
		fmt.Fprintf(os.Stderr, "hearth hh approve: decision must be 'allow' or 'deny', got %q\n", decision)
		os.Exit(1)
	}

	conn, err := net.DialTimeout("unix", daemonSockPath(), 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth hh approve: cannot connect to daemon: %v\nRun 'hearth start' first.\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	reqBytes, _ := json.Marshal(ipcRequest{
		Type:             "approve_permission_request",
		ApproveRequestID: requestID,
		ApproveDecision:  decision,
		ApproveReason:    reason,
	})
	reqBytes = append(reqBytes, '\n')
	if _, err := conn.Write(reqBytes); err != nil {
		fmt.Fprintf(os.Stderr, "hearth hh approve: send: %v\n", err)
		os.Exit(1)
	}
	// Server-side resolution is synchronous; 30s matches the IPC
	// default in sendWSRequest.
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth hh approve: read response: %v\n", err)
		os.Exit(1)
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "hearth hh approve: invalid response: %v\n", err)
		os.Exit(1)
	}
	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "hearth hh approve: %s\n", resp.Message)
		os.Exit(1)
	}
	// On success, daemon returns ws_response with the server's JSON.
	// Print it so the agent (or operator) can confirm what happened.
	if len(resp.Data) > 0 {
		printJSON(resp.Data)
	} else {
		fmt.Println(`{"ok":true}`)
	}
}

func printOrganizationUsage() {
	fmt.Fprintf(os.Stderr, `Usage: hearth hh <entity> <command> [flags]

Entities:
  household        Households
  user             Human users
  job_description  Agent job descriptions
  position         Household positions (org chart slots)
  agent            AI agent instances
  ai_model         AI brain models (read-only)
  host             Enrolled hosts (list, get, transfer, check)
  device           io_devices in the current household (read-only)

Actions:
  approve          Respond to a pending permission_request (agent-only)

Run 'hearth hh <entity> --help' for details.
`)
}

// =============================================================================
// org — organizations
// =============================================================================

func runOrganizationOrg(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh household <list|get|create|update|delete|switch>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		data, err := sendWSRequest("list_organizations", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("org get", flag.ExitOnError)
		id := fs.String("id", "", "Household ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_organization", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("org create", flag.ExitOnError)
		name := fs.String("name", "", "Household name")
		fs.Parse(args[1:])
		if *name == "" {
			reader := bufio.NewReader(os.Stdin)
			*name = promptLine(reader, "Name: ")
		}
		if *name == "" {
			fmt.Fprintf(os.Stderr, "hearth: name required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("create_organization", map[string]interface{}{"name": *name})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "update":
		fs := flag.NewFlagSet("org update", flag.ExitOnError)
		id := fs.String("id", "", "Household ID")
		name := fs.String("name", "", "New name")
		fs.Parse(args[1:])
		if *id == "" || *name == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id and --name required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("update_organization", map[string]interface{}{"id": *id, "name": *name})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "archive":
		fs := flag.NewFlagSet("org archive", flag.ExitOnError)
		id := fs.String("id", "", "Household ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("archive_organization", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "switch":
		// Interactive picker scoped to the calling user's memberships, with
		// a "create new" escape hatch. Source of truth for current org is
		// io_devices.organization_id on the server — we just send
		// set_current_organization and the phone + CLI both see the new scope.
		userID := readConfigValue("user_id")
		if userID == "" {
			fmt.Fprintf(os.Stderr, "hearth: not logged in (run 'hearth login <email>' first)\n")
			os.Exit(1)
		}
		deviceID := readConfigValue("io_device_id")
		if deviceID == "" {
			fmt.Fprintf(os.Stderr, "hearth: not enrolled (run 'hearth login <email>' first)\n")
			os.Exit(1)
		}
		chosen, err := selectUserOrganization(userID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		data, err := sendWSRequest("set_current_organization", map[string]interface{}{
			"io_device_id":    deviceID,
			"organization_id": chosen,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		var resp struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &resp); err == nil && resp.Error != "" {
			fmt.Fprintf(os.Stderr, "hearth: %s\n", resp.Error)
			os.Exit(1)
		}
		fmt.Printf("Current household set to %s.\n", chosen)
	default:
		fmt.Fprintf(os.Stderr, "hearth hh household: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// user — human users (scoped to working organization)
// =============================================================================

func runOrganizationUser(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh user <list|get|create>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		orgID := requireWorkingOrgID()
		data, err := sendWSRequest("list_human_users", map[string]interface{}{"organization_id": orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("user get", flag.ExitOnError)
		id := fs.String("id", "", "User ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		orgID := requireWorkingOrgID()
		data, err := sendWSRequest("get_human_user", map[string]interface{}{"id": *id, "organization_id": orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("user create", flag.ExitOnError)
		name := fs.String("name", "", "User name")
		role := fs.String("role", "", "Membership role (owner|member, default member)")
		fs.Parse(args[1:])

		orgID := requireWorkingOrgID()

		if *name == "" {
			reader := bufio.NewReader(os.Stdin)
			*name = promptLine(reader, "Name: ")
		}
		if *name == "" {
			fmt.Fprintf(os.Stderr, "hearth: name required\n")
			os.Exit(1)
		}

		payload := map[string]interface{}{
			"organization_id": orgID,
			"name":            *name,
		}
		if *role != "" {
			payload["role"] = *role
		}
		data, err := sendWSRequest("create_human_user", payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	default:
		fmt.Fprintf(os.Stderr, "hearth hh user: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// job — agent job descriptions
// =============================================================================

func runOrganizationJob(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh job_description <list|get|create|update|archive|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("job list", flag.ExitOnError)
		orgID := fs.String("household", "", "Household ID (defaults to current household from config)")
		fs.Parse(args[1:])
		if *orgID == "" {
			*orgID = requireWorkingOrgID()
		}
		data, err := sendWSRequest("list_agent_job_descriptions", map[string]interface{}{"organization_id": *orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("job get", flag.ExitOnError)
		id := fs.String("id", "", "Job description ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_agent_job_description", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("job create", flag.ExitOnError)
		title := fs.String("title", "", "Job title (e.g. \"Gardener\")")
		mandate := fs.String("mandate", "", "Mandate / responsibilities (markdown)")
		requiredSkills := fs.String("required-skills", "", "Required skills, tools, or knowledge (markdown)")
		priority := fs.Int("priority", 5, "Priority 1-10 (1=highest)")
		fs.Parse(args[1:])

		orgID := requireWorkingOrgID()

		reader := bufio.NewReader(os.Stdin)
		if *title == "" {
			*title = promptLine(reader, "Title: ")
		}
		if *title == "" {
			fmt.Fprintf(os.Stderr, "hearth: title required\n")
			os.Exit(1)
		}

		payload := map[string]interface{}{
			"organization_id": orgID,
			"title":           *title,
			"priority":        *priority,
		}
		if *mandate != "" {
			payload["mandate"] = *mandate
		}
		if *requiredSkills != "" {
			payload["required_skills"] = *requiredSkills
		}
		runCreateOrUpdateWithCollisionPrompt("create_agent_job_description", "job_description", "archive", "title", payload)
	case "update":
		fs := flag.NewFlagSet("job update", flag.ExitOnError)
		id := fs.String("id", "", "Job description ID")
		title := fs.String("title", "", "New title")
		mandate := fs.String("mandate", "", "New mandate")
		requiredSkills := fs.String("required-skills", "", "New required skills")
		priority := fs.Int("priority", 0, "New priority")
		fs.Parse(args[1:])
		if *id == "" || *title == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id and --title required\n")
			os.Exit(1)
		}
		payload := map[string]interface{}{"id": *id, "title": *title}
		if *mandate != "" {
			payload["mandate"] = *mandate
		}
		if *requiredSkills != "" {
			payload["required_skills"] = *requiredSkills
		}
		if *priority != 0 {
			payload["priority"] = *priority
		}
		runCreateOrUpdateWithCollisionPrompt("update_agent_job_description", "job_description", "archive", "title", payload)
	case "archive":
		fs := flag.NewFlagSet("job archive", flag.ExitOnError)
		id := fs.String("id", "", "Job description ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		runArchivalWithCascadePrompt("archive_agent_job_description", "job_description", *id)
	default:
		fmt.Fprintf(os.Stderr, "hearth hh job_description: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// pos — organization positions
// =============================================================================

func runOrganizationPos(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh position <list|get|create|eliminate|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("pos list", flag.ExitOnError)
		orgID := fs.String("household", "", "Household ID (defaults to current household from config)")
		fs.Parse(args[1:])
		if *orgID == "" {
			*orgID = requireWorkingOrgID()
		}
		data, err := sendWSRequest("list_organization_positions", map[string]interface{}{"organization_id": *orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("pos get", flag.ExitOnError)
		id := fs.String("id", "", "Position ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_organization_position", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("pos create", flag.ExitOnError)
		jobID := fs.String("job", "", "Agent job description ID (required; prompted if unset)")
		wdID := fs.String("wd", "", "Working directory ID (optional; if unset, you'll be prompted for a directory path)")
		parentID := fs.String("parent", "", "Parent position ID (optional)")
		hostID := fs.String("host-id", "", "Host on which to find-or-create the working directory (defaults to this host)")
		fs.Parse(args[1:])

		orgID := requireWorkingOrgID()

		// Positions no longer have a name of their own — their display
		// label is the title of the linked job description. Require a JD
		// (pick existing or create inline) before anything else so the
		// working-directory default can snake_case its title.
		if *jobID == "" {
			id, err := selectAgentJobDescription(orgID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
				os.Exit(1)
			}
			*jobID = id
		}

		jdTitle, err := fetchAgentJobDescriptionTitle(*jobID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		dirSuggestion := toSnakeCase(jdTitle)
		if dirSuggestion == "" {
			fmt.Fprintf(os.Stderr, "hearth: job description title must contain at least one alphanumeric character\n")
			os.Exit(1)
		}

		if *wdID == "" {
			id, err := findOrCreateWorkingDirectoryByPath(orgID, *hostID, dirSuggestion)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
				os.Exit(1)
			}
			*wdID = id
		}

		payload := map[string]interface{}{
			"organization_id":          orgID,
			"working_directory_id":     *wdID,
			"agent_job_description_id": *jobID,
		}
		if *parentID != "" {
			payload["parent_position_id"] = *parentID
		}
		runCreateOrUpdateWithCollisionPrompt("create_organization_position", "position", "eliminate", "name", payload)
	case "eliminate":
		fs := flag.NewFlagSet("pos eliminate", flag.ExitOnError)
		id := fs.String("id", "", "Position ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		runArchivalWithCascadePrompt("eliminate_organization_position", "position", *id)
	default:
		fmt.Fprintf(os.Stderr, "hearth hh position: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// agent — AI agent instances
// =============================================================================

func runOrganizationAgent(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh agent <list|get|create|sleep|wake|retire|attach>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("agent list", flag.ExitOnError)
		orgID := fs.String("household", "", "Household ID (defaults to current household from config)")
		includeRetired := fs.Bool("include-retired", false, "Include retired agents in the listing (default: hide them)")
		fs.Parse(args[1:])
		if *orgID == "" {
			*orgID = requireWorkingOrgID()
		}
		data, err := sendWSRequest("list_ai_agent_instances", map[string]interface{}{
			"organization_id": *orgID,
			"exclude_retired": !*includeRetired,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("agent get", flag.ExitOnError)
		id := fs.String("id", "", "Agent instance ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_ai_agent_instance", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("agent create", flag.ExitOnError)
		posID := fs.String("pos", "", "Position ID")
		modelID := fs.String("model", "", "AI brain model ID")
		harnessID := fs.String("harness", "", "Harness ID")
		name := fs.String("name", "", "Human-readable name")
		hostID := fs.String("host-id", "", "Host on which the agent should run (defaults to this host; picker only — new hosts must be enrolled via 'hearth start' on that machine)")
		temp := fs.Bool("temp", false, "Skip all prompts and spawn a disposable agent in the current directory (override with --wd). Flags still override individual defaults; unset ones fall back to: host=this host, harness=first listed, model=first listed, name='Temp <id>'. Temp agents auto-rename from their first user message and group separately in the iOS agent list.")
		wd := fs.String("wd", "", "Working directory for the temp agent (defaults to the current directory). Repeat invocations in the same directory reuse the wd row; if an agent is already active there you'll be asked whether to sleep it and replace.")
		fs.Parse(args[1:])

		orgID := requireWorkingOrgID()

		reader := bufio.NewReader(os.Stdin)

		// --temp takes a dedicated server endpoint that atomically creates
		// wd + position + instance and pushes the spawn to the daemon. All
		// we do locally is choose defaults for unset flags and shell out
		// to the create, then land the user in talk.
		if *temp {
			if err := runTempCreate(orgID, *name, *hostID, *harnessID, *modelID, *wd, reader); err != nil {
				fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
				os.Exit(1)
			}
			return
		}

		if *name == "" {
			*name = promptLine(reader, "Agent Name: ")
		}
		if *name == "" {
			fmt.Fprintf(os.Stderr, "hearth: name required\n")
			os.Exit(1)
		}

		// Host — cursor defaults to this daemon's host_id. No "create new"
		// entry; a new host has to be enrolled out-of-band by running
		// 'hearth start' on the target machine.
		if *hostID == "" {
			id, err := selectHost(readConfigValue("host_id"))
			if err != nil {
				fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
				os.Exit(1)
			}
			*hostID = id
		}

		var harnessName string
		if *harnessID == "" {
			id, name, err := selectHarness()
			if err != nil {
				fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
				os.Exit(1)
			}
			*harnessID = id
			harnessName = name
		} else {
			name, err := harnessNameByID(*harnessID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
				os.Exit(1)
			}
			harnessName = name
		}

		if *modelID == "" && harnessHonorsModelEnv(harnessName) {
			id, err := selectAIBrainModel(orgID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
				os.Exit(1)
			}
			*modelID = id
		}

		if *posID == "" {
			id, err := selectOrganizationPosition(orgID, *hostID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
				os.Exit(1)
			}
			*posID = id
		}

		// The daemon will spawn the harness with cwd set to the position's
		// working_directory. Make sure that directory exists on disk first —
		// prompt the user to create it if it doesn't, and bail otherwise.
		if err := ensureWorkingDirOnDisk(reader, *posID); err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}

		payload := map[string]interface{}{
			"organization_id":          orgID,
			"organization_position_id": *posID,
			"harness_id":               *harnessID,
			"name":                     *name,
		}
		if *modelID != "" {
			payload["ai_brain_model_id"] = *modelID
		}
		data := sendCreateOrUpdateResolvingCollision("create_ai_agent_instance", "agent", "retire", "name", payload)
		printJSON(data)
	case "sleep":
		fs := flag.NewFlagSet("agent sleep", flag.ExitOnError)
		id := fs.String("id", "", "Agent instance ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("sleep_ai_agent_instance", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "wake":
		fs := flag.NewFlagSet("agent wake", flag.ExitOnError)
		id := fs.String("id", "", "Agent instance ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		// wake can hit a slot_collision if another agent on the same
		// position is already awake. The shared collision loop handles
		// it by prompting the user to put the sitting occupant to sleep.
		runCreateOrUpdateWithCollisionPrompt("wake_ai_agent_instance", "agent", "sleep", "name", map[string]interface{}{"id": *id})
	case "retire":
		fs := flag.NewFlagSet("agent retire", flag.ExitOnError)
		id := fs.String("id", "", "Agent instance ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}

		// Resolve the working directory before retiring so we can offer to
		// delete it locally afterwards. Best-effort — lookup failure just
		// skips the prompt.
		wdPath, wdHostID := resolveAgentWorkingDir(*id)

		data, err := sendWSRequest("retire_ai_agent_instance", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)

		if wdPath == "" {
			return
		}
		thisHost := readConfigValue("host_id")
		if wdHostID != "" && thisHost != "" && wdHostID != thisHost {
			fmt.Fprintf(os.Stderr, "Working directory %s is on host %s; skipping local delete.\n", wdPath, wdHostID)
			return
		}
		reader := bufio.NewReader(os.Stdin)
		ans := strings.ToLower(strings.TrimSpace(promptLine(reader, fmt.Sprintf("Also delete working directory %s? [y/N]: ", wdPath))))
		if ans != "y" && ans != "yes" {
			return
		}
		if err := os.RemoveAll(wdPath); err != nil {
			fmt.Fprintf(os.Stderr, "hearth: failed to remove %s: %v\n", wdPath, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Removed %s\n", wdPath)
	case "attach":
		runAgentAttach(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "hearth hh agent: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// model — AI brain models (read-only)
// =============================================================================

func runOrganizationModel(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh ai_model <list|get>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("model list", flag.ExitOnError)
		orgID := fs.String("household", "", "Household ID (defaults to current household from config)")
		fs.Parse(args[1:])
		if *orgID == "" {
			*orgID = requireWorkingOrgID()
		}
		data, err := sendWSRequest("list_ai_brain_models", map[string]interface{}{"organization_id": *orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("model get", flag.ExitOnError)
		id := fs.String("id", "", "Model ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_ai_brain_model", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	default:
		fmt.Fprintf(os.Stderr, "hearth hh ai_model: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// resolveAgentWorkingDir looks up the working_directory path and host_id
// for an agent instance. Returns empty strings on any lookup error — callers
// should treat that as "skip wd-related behavior" rather than fatal.
func resolveAgentWorkingDir(agentInstanceID string) (string, string) {
	agentData, err := sendWSRequest("get_ai_agent_instance", map[string]interface{}{"id": agentInstanceID})
	if err != nil {
		return "", ""
	}
	var agentWrap struct {
		AIAgentInstance struct {
			OrganizationPositionID string `json:"organization_position_id"`
		} `json:"ai_agent_instance"`
	}
	if err := json.Unmarshal(agentData, &agentWrap); err != nil || agentWrap.AIAgentInstance.OrganizationPositionID == "" {
		return "", ""
	}
	posData, err := sendWSRequest("get_organization_position", map[string]interface{}{"id": agentWrap.AIAgentInstance.OrganizationPositionID})
	if err != nil {
		return "", ""
	}
	var posWrap struct {
		OrganizationPosition struct {
			WorkingDirectoryID string `json:"working_directory_id"`
		} `json:"organization_position"`
	}
	if err := json.Unmarshal(posData, &posWrap); err != nil || posWrap.OrganizationPosition.WorkingDirectoryID == "" {
		return "", ""
	}
	wdData, err := sendWSRequest("get_working_directory", map[string]interface{}{"id": posWrap.OrganizationPosition.WorkingDirectoryID})
	if err != nil {
		return "", ""
	}
	var wdWrap struct {
		WorkingDirectory struct {
			DirectoryPath string `json:"directory_path"`
			HostID        string `json:"host_id"`
		} `json:"working_directory"`
	}
	if err := json.Unmarshal(wdData, &wdWrap); err != nil {
		return "", ""
	}
	return wdWrap.WorkingDirectory.DirectoryPath, wdWrap.WorkingDirectory.HostID
}

// ensureWorkingDirOnDisk resolves the position's working_directory path,
// checks if it exists locally, and prompts the user to create it if missing.
// No-ops silently when the WD belongs to a different host (the daemon on that
// host owns its own disk). Returns an error if the user declines or the
// lookup/mkdir fails.
func ensureWorkingDirOnDisk(reader *bufio.Reader, positionID string) error {
	posData, err := sendWSRequest("get_organization_position", map[string]interface{}{"id": positionID})
	if err != nil {
		return fmt.Errorf("failed to fetch organization_position: %w", err)
	}
	var posWrap struct {
		OrganizationPosition struct {
			WorkingDirectoryID string `json:"working_directory_id"`
		} `json:"organization_position"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(posData, &posWrap); err != nil {
		return fmt.Errorf("failed to parse organization_position response: %w", err)
	}
	if posWrap.Error != "" {
		return fmt.Errorf("organization_position lookup: %s", posWrap.Error)
	}
	if posWrap.OrganizationPosition.WorkingDirectoryID == "" {
		return fmt.Errorf("organization_position has no working_directory")
	}

	wdData, err := sendWSRequest("get_working_directory", map[string]interface{}{"id": posWrap.OrganizationPosition.WorkingDirectoryID})
	if err != nil {
		return fmt.Errorf("failed to fetch working_directory: %w", err)
	}
	var wdWrap struct {
		WorkingDirectory struct {
			HostID        string `json:"host_id"`
			DirectoryPath string `json:"directory_path"`
		} `json:"working_directory"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(wdData, &wdWrap); err != nil {
		return fmt.Errorf("failed to parse working_directory response: %w", err)
	}
	if wdWrap.Error != "" {
		return fmt.Errorf("working_directory lookup: %s", wdWrap.Error)
	}

	dir := wdWrap.WorkingDirectory.DirectoryPath
	if dir == "" {
		return fmt.Errorf("working_directory has no directory_path")
	}

	// Skip local disk check when the WD belongs to a different host.
	thisHost := readConfigValue("host_id")
	wdHostID := wdWrap.WorkingDirectory.HostID
	if wdHostID != "" && thisHost != "" && wdHostID != thisHost {
		return nil
	}

	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists but is not a directory", dir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", dir, err)
	}

	answer := strings.ToLower(strings.TrimSpace(promptLine(reader, fmt.Sprintf("Directory %q doesn't exist. Create it? (Y/n): ", dir))))
	if answer == "n" || answer == "no" {
		return fmt.Errorf("aborted: working directory does not exist")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", dir, err)
	}
	fmt.Fprintf(os.Stderr, "Created %s\n", dir)
	return nil
}

// =============================================================================
// Temp spawn helpers
// =============================================================================

// runTempCreate fills unset flags with defaults, sends the single-shot
// create_temp_agent_instance ws_request (server creates wd + position +
// instance atomically and pushes the spawn to the daemon on host_id), and
// then execs `hearth talk --focus <id>`. Explicit flags win — any non-zero
// argument is passed through as-is.
//
// The wd defaults to pwd (or --wd if supplied). On a collision (an agent is
// already active in that directory) the server returns an
// "active_agent_in_directory" envelope; we prompt, sleep the occupant, and
// retry once.
func runTempCreate(orgID, name, hostID string, harnessID string, modelID, wd string, reader *bufio.Reader) error {
	shortID := strings.ReplaceAll(generateUUID(), "-", "")[:8]

	if name == "" {
		name = "Temp " + shortID
	}
	if hostID == "" {
		h := readConfigValue("host_id")
		if h == "" {
			return fmt.Errorf("no local host_id in config — run 'hearth start' first")
		}
		hostID = h
	}
	var harnessName string
	if harnessID == "" {
		id, err := firstHarnessID()
		if err != nil {
			return err
		}
		harnessID = id
	}
	{
		name, err := harnessNameByID(harnessID)
		if err != nil {
			return err
		}
		harnessName = name
	}
	if modelID == "" && harnessHonorsModelEnv(harnessName) {
		id, err := firstModelID(orgID)
		if err != nil {
			return err
		}
		modelID = id
	}

	// Resolve the wd path. Default to pwd; --wd overrides. Always send an
	// absolute path so the server's lookup against working_directories
	// matches consistently regardless of how the user typed it.
	if wd == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get cwd: %w", err)
		}
		wd = cwd
	}
	abs, err := filepath.Abs(wd)
	if err != nil {
		return fmt.Errorf("resolve --wd: %w", err)
	}
	wd = abs

	payload := map[string]interface{}{
		"organization_id": orgID,
		"host_id":         hostID,
		"harness_id":      harnessID,
		"directory_path":  wd,
		"name":            name,
	}
	if modelID != "" {
		payload["ai_brain_model_id"] = modelID
	}

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := sendWSRequest("create_temp_agent_instance", payload)
		if err != nil {
			return err
		}
		var wrap struct {
			AIAgentInstance struct {
				ID string `json:"id"`
			} `json:"ai_agent_instance"`
			SpawnError string `json:"spawn_error"`
			Error      string `json:"error"`
			Collision  struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"collision"`
		}
		if err := json.Unmarshal(resp, &wrap); err != nil {
			return fmt.Errorf("parse create_temp_agent_instance: %w", err)
		}
		if wrap.Error == "active_agent_in_directory" && attempt == 0 {
			occupantName := wrap.Collision.Name
			if occupantName == "" {
				occupantName = wrap.Collision.ID
			}
			fmt.Fprintf(os.Stderr, "An agent is already active in %s: %s\n", wd, occupantName)
			ans := strings.TrimSpace(strings.ToLower(promptLine(reader, "Sleep it and replace? [Y/n]: ")))
			if ans != "" && ans != "y" && ans != "yes" {
				return fmt.Errorf("aborted")
			}
			if _, err := sendWSRequest("sleep_ai_agent_instance", map[string]interface{}{"id": wrap.Collision.ID}); err != nil {
				return fmt.Errorf("sleep occupant: %w", err)
			}
			continue
		}
		if wrap.Error != "" {
			return fmt.Errorf("%s", wrap.Error)
		}
		if wrap.SpawnError != "" {
			return fmt.Errorf("agent row created but spawn failed: %s", wrap.SpawnError)
		}
		if wrap.AIAgentInstance.ID == "" {
			return fmt.Errorf("create_temp_agent_instance: empty id in response")
		}

		fmt.Fprintf(os.Stderr, "Spawned temp agent %s. Opening talk (agent may take a moment to warm up)…\n", wrap.AIAgentInstance.ID)
		runTalk([]string{"--focus", wrap.AIAgentInstance.ID})
		return nil
	}
	return fmt.Errorf("create_temp_agent_instance: still blocked after replacing occupant")
}

// firstHarnessID returns the first row from list_harnesses. The server's
// ordering happens to put claude-code first today, which is the right
// default for a one-off scratch agent on a dev machine.
func firstHarnessID() (string, error) {
	data, err := sendWSRequest("list_harnesses", map[string]interface{}{})
	if err != nil {
		return "", fmt.Errorf("list_harnesses: %w", err)
	}
	var resp struct {
		Harnesses []struct {
			ID string `json:"id"`
		} `json:"harnesses"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("list_harnesses parse: %w", err)
	}
	if len(resp.Harnesses) == 0 {
		return "", fmt.Errorf("no harnesses registered on the server")
	}
	return resp.Harnesses[0].ID, nil
}

// firstModelID returns the first ai_brain_model listed for the org.
func firstModelID(orgID string) (string, error) {
	data, err := sendWSRequest("list_ai_brain_models", map[string]interface{}{"organization_id": orgID})
	if err != nil {
		return "", fmt.Errorf("list_ai_brain_models: %w", err)
	}
	var resp struct {
		AIBrainModels []struct {
			ID string `json:"id"`
		} `json:"ai_brain_models"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("list_ai_brain_models parse: %w", err)
	}
	if len(resp.AIBrainModels) == 0 {
		return "", fmt.Errorf("no ai_brain_models in this household — add one with 'hearth hh ai_model create'")
	}
	return resp.AIBrainModels[0].ID, nil
}

// =============================================================================
// host — enrolled hosts (read-only)
// =============================================================================

func runOrganizationHost(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh host <list|get|rename|forget|check>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "check":
		runHostCheck(args[1:])
		return
	case "list":
		// Formatted output (host_id + hostname + last_seen, marks the
		// current host with *). Defined in daemon.go since it was also
		// used by the old top-level `hearth host ls`.
		hostList()
	case "get":
		fs := flag.NewFlagSet("host get", flag.ExitOnError)
		id := fs.String("id", "", "Host ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_host", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "rename":
		fs := flag.NewFlagSet("host rename", flag.ExitOnError)
		id := fs.String("id", "", "Host ID")
		name := fs.String("name", "", "New hostname")
		fs.Parse(args[1:])
		if *id == "" || *name == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id and --name required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("host_rename", map[string]interface{}{
			"host_id":      *id,
			"new_hostname": *name,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "forget":
		fs := flag.NewFlagSet("host forget", flag.ExitOnError)
		id := fs.String("id", "", "Host ID")
		yes := fs.Bool("yes", false, "Skip the confirmation prompt")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		// Loud warning when forgetting the host we're calling from.
		// `forget` soft-revokes the row and force-closes the daemon
		// WS — for the current host that means this very CLI session
		// loses its daemon link. Re-enroll would mint a new host_id.
		selfHostID := readConfigValue("host_id")
		isSelf := selfHostID != "" && selfHostID == *id
		if !*yes {
			if isSelf {
				fmt.Fprintln(os.Stderr,
					"WARNING: this is the current host. Forgetting it will "+
						"disconnect the local daemon immediately; re-enrolling "+
						"will mint a new host_id.")
			}
			reader := bufio.NewReader(os.Stdin)
			ans := strings.ToLower(strings.TrimSpace(
				promptLine(reader, fmt.Sprintf("Forget host %s? [y/N]: ", *id))))
			if ans != "y" && ans != "yes" {
				fmt.Fprintln(os.Stderr, "aborted")
				os.Exit(1)
			}
		}
		data, err := sendWSRequest("host_forget", map[string]interface{}{"host_id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	default:
		fmt.Fprintf(os.Stderr, "hearth hh host: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// device — io_devices (read-only; mirrors org host)
// =============================================================================

func runOrganizationDevice(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh device <list|get>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("device list", flag.ExitOnError)
		orgID := fs.String("household", "", "Household ID (defaults to current household from config)")
		fs.Parse(args[1:])
		if *orgID == "" {
			*orgID = requireWorkingOrgID()
		}
		data, err := sendWSRequest("list_io_devices", map[string]interface{}{"organization_id": *orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("device get", flag.ExitOnError)
		id := fs.String("id", "", "io_device ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_io_device", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	default:
		fmt.Fprintf(os.Stderr, "hearth hh device: unknown command %q\n", args[0])
		os.Exit(1)
	}
}
