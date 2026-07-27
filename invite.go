//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// runOrganizationInvite dispatches `hearth hh invite <sub>`.
//
//	send   <email>   create an invite (owner-only, server-enforced)
//	list             invitations this household has sent
//	revoke <id>      cancel a pending invite
//	accept           join a household you were invited to (tokenless —
//	                 resolved from your verified email)
func runOrganizationInvite(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh invite <send|list|revoke|accept> [args]\n")
		os.Exit(0)
	}
	switch args[0] {
	case "send":
		inviteSend(args[1:])
	case "list":
		inviteList()
	case "revoke":
		inviteRevoke(args[1:])
	case "accept":
		inviteAccept(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "hearth hh invite: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

func inviteSend(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh invite send <email>\n")
		os.Exit(1)
	}
	email := strings.ToLower(strings.TrimSpace(args[0]))
	if email == "" {
		fmt.Fprintf(os.Stderr, "hearth: email required\n")
		os.Exit(1)
	}
	orgID := requireWorkingOrgID()
	baseURL, err := serverBaseURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	ioDeviceID := readConfigValue("io_device_id")
	ioDeviceSecret := readConfigValue("io_device_secret")
	if ioDeviceID == "" || ioDeviceSecret == "" {
		fmt.Fprintf(os.Stderr, "hearth: not enrolled (run 'hearth login <email>' first)\n")
		os.Exit(1)
	}
	data, err := deviceAuthedPost(baseURL, "/invites", ioDeviceID, ioDeviceSecret,
		ActionTuple{Kind: "invite", Action: "create"},
		map[string]string{
			"organization_id": orgID,
			"email":           email,
		})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	var resp struct {
		InviteID  string `json:"invite_id"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	// No token to print — the invitation is keyed on the recipient's
	// address. They join by signing in as that address (app or
	// `hearth hh invite accept`); the email is just a heads-up.
	fmt.Printf("Invite sent to %s\n  invite_id:  %s\n  expires_at: %s\n",
		email, resp.InviteID, resp.ExpiresAt)
}

func inviteList() {
	orgID := requireWorkingOrgID()
	data, err := sendWSRequest("list_invites", map[string]interface{}{"organization_id": orgID})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	printJSON(data)
}

// inviteAccept joins a household the caller was invited to. Tokenless: the
// server resolves invitations from the device owner's verified email
// (list_my_invites), and the join goes through accept_invites — the same
// email-gated path the app uses. No argument to paste.
//
//	hearth hh invite accept          # list, then pick
//	hearth hh invite accept --yes    # accept every pending invitation
func inviteAccept(args []string) {
	acceptAll := false
	for _, a := range args {
		if a == "--yes" || a == "-y" {
			acceptAll = true
		}
	}

	ioDeviceID := readConfigValue("io_device_id")
	if ioDeviceID == "" {
		fmt.Fprintf(os.Stderr, "hearth: not enrolled (run 'hearth login <email>' first)\n")
		os.Exit(1)
	}

	listData, err := sendWSRequest("list_my_invites", map[string]interface{}{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	var list struct {
		Invites []struct {
			ID               string `json:"id"`
			OrganizationName string `json:"organization_name"`
			InviterEmail     string `json:"inviter_email"`
			ExpiresAt        string `json:"expires_at"`
		} `json:"invites"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(listData, &list); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	if list.Error != "" {
		fmt.Fprintf(os.Stderr, "hearth: %s\n", list.Error)
		os.Exit(1)
	}
	if len(list.Invites) == 0 {
		fmt.Println("No invitations. If you were expecting one, make sure you're signed in as the invited email address.")
		return
	}

	fmt.Println("You've been invited to:")
	for i, inv := range list.Invites {
		fmt.Printf("  %d) %s — from %s\n", i+1, inv.OrganizationName, inv.InviterEmail)
	}

	// Choose which to accept. --yes takes all; otherwise prompt for a
	// number, "all", or "q" to cancel.
	var chosen []string
	if acceptAll {
		for _, inv := range list.Invites {
			chosen = append(chosen, inv.ID)
		}
	} else {
		fmt.Printf("Accept which? [1-%d / all / q]: ", len(list.Invites))
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		switch answer {
		case "q", "":
			fmt.Println("Cancelled.")
			return
		case "all":
			for _, inv := range list.Invites {
				chosen = append(chosen, inv.ID)
			}
		default:
			n, convErr := strconv.Atoi(answer)
			if convErr != nil || n < 1 || n > len(list.Invites) {
				fmt.Fprintf(os.Stderr, "hearth: invalid selection %q\n", answer)
				os.Exit(1)
			}
			chosen = append(chosen, list.Invites[n-1].ID)
		}
	}

	acceptData, err := sendWSRequest("accept_invites", map[string]interface{}{
		"invite_ids":   chosen,
		"io_device_id": ioDeviceID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	var resp struct {
		Accepted []struct {
			OrganizationName string `json:"organization_name"`
		} `json:"accepted"`
		LandingOrganizationID string            `json:"landing_organization_id"`
		Failed                map[string]string `json:"failed"`
		Error                 string            `json:"error"`
	}
	if err := json.Unmarshal(acceptData, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "hearth: %s\n", resp.Error)
		os.Exit(1)
	}
	if len(resp.Accepted) == 0 {
		fmt.Fprintln(os.Stderr, "hearth: nothing was joined (the invitations may have been withdrawn).")
		os.Exit(1)
	}
	names := make([]string, 0, len(resp.Accepted))
	for _, a := range resp.Accepted {
		names = append(names, a.OrganizationName)
	}
	fmt.Printf("Joined %s. It's now available in the Hearth app on your phone; this host's CLI stays bound to its own household.\n", strings.Join(names, ", "))
	if len(resp.Failed) > 0 {
		fmt.Fprintf(os.Stderr, "  (%d invitation(s) could not be accepted)\n", len(resp.Failed))
	}
}

func inviteRevoke(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh invite revoke <invite_id>\n")
		os.Exit(1)
	}
	data, err := sendWSRequest("revoke_invite", map[string]interface{}{"id": args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	printJSON(data)
}
