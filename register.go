//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// promptAccountNames asks the user for their display name and organization
// name, showing a default derived from the email local-part. Empty input
// accepts the default. Returns the chosen values.
//
// First-time registration only — the server's maybeApplyEnrollmentNaming
// only writes when the current values are still placeholders (name ==
// email for the user, UUID for the org), so passing non-empty values on
// reclaim is harmless but noisy, hence the caller's mode == "fresh" guard.
// promptApprovalPolicy asks who is allowed to approve permission requests
// for this host. Default (bare Enter) is owner_only — narrower blast
// radius, matches the common case of a personal dev box. Returns either
// "owner_only" or "org_members" (never "" when called).
func promptApprovalPolicy(reader *bufio.Reader) string {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Who is allowed to approve permission requests on this host?")
	fmt.Fprintln(os.Stderr, "  [1] only me (recommended)")
	fmt.Fprintln(os.Stderr, "  [2] anyone in my household")
	for {
		fmt.Fprint(os.Stderr, "Choice [1]: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			// Non-interactive / EOF: default.
			return "owner_only"
		}
		switch strings.TrimSpace(line) {
		case "", "1", "owner", "owner_only":
			return "owner_only"
		case "2", "org", "org_members":
			return "org_members"
		default:
			fmt.Fprintln(os.Stderr, "  Please enter 1 or 2.")
		}
	}
}

func promptAccountNames(reader *bufio.Reader, email string) (userName, orgName string) {
	defaultUser := defaultUserNameFromEmail(email)

	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Finishing account setup (press Enter to accept the suggested):")
	userName = promptWithDefault(reader, "  Your name", defaultUser)
	// Derive the household-name suggestion from whatever the user
	// actually entered for their name, not from the email — typing your
	// real name should produce a matching household suggestion.
	orgName = promptWithDefault(reader, "  Household name", defaultOrgNameFromUserName(userName))
	return
}

// defaultAgentHomeBase returns $HOME/hearth_agents — the org-agnostic
// base path suggested at first-host enrollment. Falls back to
// ./hearth_agents if the home directory can't be resolved.
func defaultAgentHomeBase() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, "hearth_agents")
}

// promptAgentHomePath asks the user where agent working directories
// should live on this host. Run only on first-host enrollment
// (is_new_host=true from the server). The default is the org-agnostic
// base ($HOME/hearth_agents); the prompt previews how each of the
// user's existing orgs will namespace into per-slug subdirectories so
// the choice is self-documenting.
func promptAgentHomePath(reader *bufio.Reader, orgs []daemonOrgEntry) string {
	base := defaultAgentHomeBase()
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Where should agents on this host live?")
	fmt.Fprintln(os.Stderr, "  Each household gets its own subdirectory under this base. For example:")
	if len(orgs) == 0 {
		// No orgs yet — shouldn't happen post-enroll, but handle gracefully.
		fmt.Fprintf(os.Stderr, "    %s/<household_slug>/...\n", base)
	} else {
		for _, o := range orgs {
			slug := o.Slug
			if slug == "" {
				slug = "<household>"
			}
			fmt.Fprintf(os.Stderr, "    %s\n", filepath.Join(base, slug))
		}
	}
	return promptWithDefault(reader, "  Agent home directory", base)
}

// defaultUserNameFromEmail turns "matt.beller@example.com" into
// "Matt Beller": split local-part on . _ -, title-case each, join with
// spaces. Good enough for a default; users who hate it hit a key.
func defaultUserNameFromEmail(email string) string {
	local := email
	if i := strings.Index(email, "@"); i > 0 {
		local = email[:i]
	}
	parts := strings.FieldsFunc(local, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '+'
	})
	for i, p := range parts {
		if len(p) == 0 {
			continue
		}
		runes := []rune(p)
		runes[0] = unicode.ToUpper(runes[0])
		parts[i] = string(runes)
	}
	joined := strings.Join(parts, " ")
	if joined == "" {
		return "User"
	}
	return joined
}

func defaultOrgNameFromUserName(userName string) string {
	trimmed := strings.TrimSpace(userName)
	if trimmed == "" {
		return "Home"
	}
	// Use the first word so "Matt Beller" → "Matt's Household".
	first := strings.Fields(trimmed)[0]
	return first + "'s Household"
}

// pickReclaimableHost prompts the user to reclaim a previously-enrolled
// host when the local credentials file is empty or missing. Server side
// returns the user's hosts not currently held by a live daemon; we sort
// hostname matches first so the most likely "this is the same machine"
// candidate is the default.
//
// Returns the chosen host_id, or "" to fall through to fresh enroll.
// Best-effort: any peek failure returns "" (user lands on fresh enroll).
func pickReclaimableHost(reader *bufio.Reader, baseURL, sessionToken, localHostname string) string {
	candidates, err := fetchReclaimableHosts(baseURL, sessionToken)
	if err != nil || len(candidates) == 0 {
		return ""
	}
	// Hostname matches first, then by last-seen (server already orders
	// by last_seen DESC, so we only need to pull matches to the top).
	sorted := make([]reclaimableHost, 0, len(candidates))
	for _, c := range candidates {
		if c.Hostname == localHostname {
			sorted = append(sorted, c)
		}
	}
	for _, c := range candidates {
		if c.Hostname != localHostname {
			sorted = append(sorted, c)
		}
	}

	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Found previously-enrolled hosts on your account that aren't currently online:")
	for i, c := range sorted {
		marker := ""
		if c.Hostname == localHostname {
			marker = "  ← matches this machine's hostname"
		}
		hn := c.Hostname
		if hn == "" {
			hn = "(no hostname)"
		}
		seen := c.LastSeenAt
		if seen == "" {
			seen = "never"
		}
		fmt.Fprintf(os.Stderr, "  [%d] %s in %s — last seen %s%s\n", i+1, hn, c.OrganizationName, seen, marker)
	}
	fmt.Fprintf(os.Stderr, "  [%d] Enroll as a new host\n", len(sorted)+1)

	defaultChoice := "1"
	fmt.Fprintf(os.Stderr, "\nReclaim which? [%s]: ", defaultChoice)
	line, err := reader.ReadString('\n')
	if err != nil {
		return ""
	}
	choice := strings.TrimSpace(line)
	if choice == "" {
		choice = defaultChoice
	}
	// "Enroll as a new host" sentinel — falls through to fresh.
	if choice == fmt.Sprintf("%d", len(sorted)+1) {
		return ""
	}
	for i, c := range sorted {
		if choice == fmt.Sprintf("%d", i+1) {
			return c.HostID
		}
	}
	// Unrecognized input — be safe and fall through to fresh rather
	// than guessing.
	fmt.Fprintf(os.Stderr, "Unrecognized choice %q; falling through to a fresh enrollment.\n", choice)
	return ""
}

// resolveTargetOrgID picks which organization the freshly enrolled host
// should be bound to. Logic:
//   - --org <slug-or-id> supplied: match it against the user's
//     memberships; bail with a helpful error if it doesn't resolve.
//   - User has zero or one membership: nothing to ask; return "" so the
//     server falls back to its default (only-or-earliest org).
//   - User has multiple memberships and didn't pass --org: prompt.
//
// On any peek-API error we surface a warning and return "" so the
// server's default takes over. Host-to-org binding is immutable
// post-enroll (transfer_host was removed in phase 3 step 8); if the
// default lands the host in the wrong household the operator's
// recourse is to revoke and re-enroll.
// Returns the target organization_id (empty string means "let server
// pick").
func resolveTargetOrgID(reader *bufio.Reader, baseURL, sessionToken, orgArg string) string {
	orgs, err := fetchSessionOrgs(baseURL, sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not look up your organizations (%v); proceeding with the server default.\n", err)
		return ""
	}
	if len(orgs) == 0 {
		// Brand-new user; bootstrap path will mint an org server-side.
		return ""
	}
	if orgArg != "" {
		for _, o := range orgs {
			if o.ID == orgArg || o.Slug == orgArg {
				return o.ID
			}
		}
		fmt.Fprintf(os.Stderr, "Error: --org %q didn't match any of your households:\n", orgArg)
		for _, o := range orgs {
			fmt.Fprintf(os.Stderr, "  - %s (slug=%s)\n", o.Name, o.Slug)
		}
		os.Exit(1)
	}
	if len(orgs) == 1 {
		return orgs[0].ID
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "This account belongs to multiple households. Pick one to bind this host to:")
	for i, o := range orgs {
		fmt.Fprintf(os.Stderr, "  [%d] %s (slug=%s)\n", i+1, o.Name, o.Slug)
	}
	fmt.Fprintf(os.Stderr, "Enter a number (or slug): ")
	line, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	choice := strings.TrimSpace(line)
	if choice == "" {
		fmt.Fprintf(os.Stderr, "Error: no choice made\n")
		os.Exit(1)
	}
	// Numeric pick.
	for i, o := range orgs {
		if choice == fmt.Sprintf("%d", i+1) {
			return o.ID
		}
	}
	// Slug or ID match.
	for _, o := range orgs {
		if choice == o.Slug || choice == o.ID {
			return o.ID
		}
	}
	fmt.Fprintf(os.Stderr, "Error: %q didn't match any household\n", choice)
	os.Exit(1)
	return "" // unreachable
}

func runRegister(args []string) {
	var email, inviteToken, orgArg, approvalPolicyArg string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--help" || a == "-h" {
			fmt.Fprintf(os.Stderr, "Usage: hearth login <email> [--invite <token>] [--org <slug-or-id>] [--approval-policy owner_only|org_members]\n")
			os.Exit(0)
		}
		if a == "--invite" {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --invite requires a token\n")
				os.Exit(1)
			}
			inviteToken = strings.TrimSpace(args[i+1])
			i++
			continue
		}
		if a == "--org" {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --org requires a slug or organization_id\n")
				os.Exit(1)
			}
			orgArg = strings.TrimSpace(args[i+1])
			i++
			continue
		}
		if a == "--approval-policy" {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --approval-policy requires owner_only or org_members\n")
				os.Exit(1)
			}
			approvalPolicyArg = strings.TrimSpace(args[i+1])
			if approvalPolicyArg != "owner_only" && approvalPolicyArg != "org_members" {
				fmt.Fprintf(os.Stderr, "Error: --approval-policy must be owner_only or org_members\n")
				os.Exit(1)
			}
			i++
			continue
		}
		if email == "" {
			email = strings.TrimSpace(a)
			continue
		}
		fmt.Fprintf(os.Stderr, "Usage: hearth login <email> [--invite <token>] [--org <slug-or-id>]\n")
		os.Exit(1)
	}

	if email == "" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Fprint(os.Stderr, "Email: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		email = strings.TrimSpace(line)
		if email == "" {
			fmt.Fprintf(os.Stderr, "Error: email is required\n")
			os.Exit(1)
		}
	}

	baseURL, err := serverBaseURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Step 1: trigger an OTP email.
	if err := requestAuthCode(baseURL, email); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "A 6-digit code has been sent to %s.\n", email)

	// Step 2: read the code from the user and verify it. The server's
	// response to /auth/verify is deliberately uniform on any failure
	// (expired, wrong code, burnt OTP, etc.); the CLI surfaces a generic
	// message rather than trying to disambiguate.
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stderr, "Enter the code: ")
	line, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	code := strings.TrimSpace(line)
	if len(code) != 6 {
		fmt.Fprintf(os.Stderr, "Error: expected a 6-digit code, got %q\n", code)
		os.Exit(1)
	}

	sessionToken, isNewUser, err := verifyAuthCode(baseURL, email, code, "enroll_host")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Step 3: enroll this host. Three sub-paths:
	//   (a) credentials file already has host_id  → reclaim that id.
	//   (b) credentials file is empty/missing AND
	//       the server can find a previously-enrolled host of ours that
	//       no live daemon currently holds → suggest reclaiming it
	//       (covers the "borked credentials file" recovery case).
	//   (c) otherwise → fresh enroll a new host_id.
	existingHostID := readConfigValue("host_id")
	mode := "fresh"
	hostID := generateUUID()
	hostname, _ := os.Hostname()
	if existingHostID != "" {
		mode = "reclaim"
		hostID = existingHostID
	} else {
		// (b): peek at the user's reclaimable hosts. Best-effort — any
		// failure falls through to fresh enroll. Hostname-matched
		// candidate sorts first; the user just hits enter to accept.
		if picked := pickReclaimableHost(reader, baseURL, sessionToken, hostname); picked != "" {
			mode = "reclaim"
			hostID = picked
		}
	}

	// Only prompt for display names on the very first enrollment for this
	// email (server tells us via is_new_user). Registering an additional
	// host under an existing account, or reclaiming one, skips the prompt
	// — the user already named themselves and their org at first enroll.
	// The server's maybeApplyEnrollmentNaming guard would no-op either
	// way, but the prompt itself is annoying noise for existing users.
	var userName, orgName string
	if isNewUser {
		userName, orgName = promptAccountNames(reader, email)
	}

	// Resolve target org for this host. Hosts are strictly org-scoped
	// on the server side: the host_id is bound to a single
	// organization at first enroll and is immutable thereafter
	// (transfer_host was removed in phase 3 step 8).
	// If the user passed --org we use that; otherwise we peek at
	// their memberships and prompt when ambiguous.
	//
	// Brand-new users (no memberships) skip the prompt — the server
	// bootstrap-creates an org from --user-name / --organization-name.
	// Same-owner reclaim ignores any --org we send (the server keeps
	// the prior binding), so we just send what we resolved here for
	// the same-org or first-enroll cases and tolerate the silent ignore.
	targetOrgID := ""
	if !isNewUser {
		targetOrgID = resolveTargetOrgID(reader, baseURL, sessionToken, orgArg)
	}

	// Approval policy: only meaningful on fresh enrollment (reclaim
	// preserves whatever's on the existing row). Prompt when the flag
	// wasn't given; default owner_only on bare Enter or non-interactive.
	approvalPolicy := approvalPolicyArg
	if approvalPolicy == "" && mode == "fresh" {
		approvalPolicy = promptApprovalPolicy(reader)
	}

	enroll, err := enrollHost(baseURL, sessionToken, hostID, hostname, mode, userName, orgName, targetOrgID, approvalPolicy)
	if err != nil {
		if mode != "reclaim" {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		// Reclaim now transparently upgrades to fresh on the server
		// side when the row is missing, so a genuine reclaim failure
		// means this host_id is either owned by another account or
		// has been revoked. Offer to enroll as a new host on this
		// machine — we abandon the stale host_id (never transfer
		// ownership, which would be a hijack vector) and mint a new
		// one under the currently-authenticated user.
		fmt.Fprintf(os.Stderr, "This host was registered to another account or has been revoked.\n")
		fmt.Fprintf(os.Stderr, "Enroll this machine as a new host under %s? [Y/n]: ", email)
		ans, _ := reader.ReadString('\n')
		ans = strings.TrimSpace(strings.ToLower(ans))
		if ans != "" && ans != "y" && ans != "yes" {
			_ = writeConfigValue("host_id", "")
			_ = writeConfigValue("host_secret", "")
			_ = writeConfigValue("io_device_id", "")
			_ = writeConfigValue("io_device_secret", "")
			fmt.Fprintf(os.Stderr, "Aborted. Local credentials cleared.\n")
			os.Exit(1)
		}
		// Reuse the original session token — the server only marks it
		// consumed on successful enrollment, so our prior failed attempt
		// didn't burn it. Mint a fresh host_id and retry as fresh.
		hostID = generateUUID()
		mode = "fresh"
		if approvalPolicy == "" {
			approvalPolicy = promptApprovalPolicy(reader)
		}
		enroll, err = enrollHost(baseURL, sessionToken, hostID, hostname, mode, userName, orgName, targetOrgID, approvalPolicy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	if err := writeConfigValue("user_id", enroll.HumanUserID); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving user_id: %v\n", err)
		os.Exit(1)
	}
	// Persist the email so `hearth status` can show identity in degraded
	// mode (daemon stopped → no server push to read it from). Best-effort.
	_ = writeConfigValue("email", strings.ToLower(strings.TrimSpace(email)))
	if err := writeConfigValue("host_id", hostID); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving host_id: %v\n", err)
		os.Exit(1)
	}
	if err := writeConfigValue("host_secret", enroll.HostSecret); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving host_secret: %v\n", err)
		os.Exit(1)
	}
	if err := writeConfigValue("io_device_id", enroll.IODeviceID); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving io_device_id: %v\n", err)
		os.Exit(1)
	}
	if err := writeConfigValue("io_device_secret", enroll.IODeviceSecret); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving io_device_secret: %v\n", err)
		os.Exit(1)
	}
	// organization_id is NOT written to config — source of truth is
	// io_devices.organization_id on the server, fetched per-command via
	// list_my_organizations. Keeps the CLI and phone in sync when either
	// one runs `org org switch`.

	// If a daemon is currently running (typical under systemd, where
	// the unit restarts it after logout's stopDaemon exits), nudge it
	// to re-read the fresh credentials. No-op when no daemon is
	// listening — the next start reads creds from disk anyway.
	reloadDaemonCredentials()

	fmt.Fprintf(os.Stderr, "Logged in as user %s\n", enroll.HumanUserID)
	fmt.Fprintf(os.Stderr, "Enrolled host %s (io_device %s)\n", hostID, enroll.IODeviceID)
	if enroll.OrganizationID != "" {
		fmt.Fprintf(os.Stderr, "Current household: %s\n", enroll.OrganizationID)
	}
	// Mismatch between the org the user asked for and the org the server
	// stamped on the host means this host is already bound — login can't
	// rebind it. Surface a clear next step rather than letting the user
	// silently work in the "wrong" org. Only fires when --org or the
	// interactive prompt produced a non-empty target.
	if targetOrgID != "" && enroll.OrganizationID != "" && enroll.OrganizationID != targetOrgID {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintf(os.Stderr, "Note: this host is already bound to household %s; login can't rebind it.\n", enroll.OrganizationID)
		fmt.Fprintf(os.Stderr, "      Hosts are immutable post-enroll. To relocate this machine to %s:\n", targetOrgID)
		fmt.Fprintf(os.Stderr, "        1. From any logged-in device in %s, run: hearth hh host forget --id %s\n", enroll.OrganizationID, hostID)
		fmt.Fprintf(os.Stderr, "        2. On this machine, run: hearth login   (mints a fresh host in %s)\n", targetOrgID)
	}

	// Agent home directory: prompt only when this is genuinely a new host
	// row server-side (is_new_host=true). Reclaim of an existing or
	// revoked-then-revived host preserves the prior value, so we skip the
	// prompt to avoid pestering the user. Server is SOT for this value;
	// the daemon will receive it via the agent_home_path push on its next
	// WS connect.
	if enroll.IsNewHost {
		dir := promptAgentHomePath(reader, enroll.Organizations)
		if err := setAgentHomePathOnServer(baseURL, enroll.IODeviceID, enroll.IODeviceSecret, hostID, dir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save agent home directory: %v\n", err)
			fmt.Fprintf(os.Stderr, "  You can set it later from host management.\n")
		}
	}

	// Invite accept path for brand-new users: /hosts/enroll doesn't take an
	// invite_token, so we chain a device-authed POST /invites/accept using
	// the credentials we just minted. Email match is enforced server-side.
	if inviteToken != "" {
		data, err := deviceAuthedPost(baseURL, "/invites/accept", enroll.IODeviceID, enroll.IODeviceSecret,
			ActionTuple{Kind: "invite", ID: inviteToken, Action: "accept"},
			map[string]string{
				"token": inviteToken,
			})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: invite accept failed: %v\n", err)
			fmt.Fprintf(os.Stderr, "Registration succeeded; re-run 'hearth hh invite accept %s' to retry.\n", inviteToken)
			return
		}
		var resp struct {
			OrganizationID   string `json:"organization_id"`
			OrganizationName string `json:"organization_name"`
		}
		if err := json.Unmarshal(data, &resp); err == nil && resp.OrganizationName != "" {
			fmt.Fprintf(os.Stderr, "Joined %s. Your current household is now %s.\n", resp.OrganizationName, resp.OrganizationID)
		}
	}
}
