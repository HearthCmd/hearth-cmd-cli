//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// runOrganizationInvite dispatches `hearth hh invite <sub>`. Admin
// operations only — accept-side work happens on iOS / at registration
// time via `hearth login <email> --invite <token>` (see
// register.go).
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
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	// The raw token is only returned here. In dev the server also logs it
	// via the stub mailer; in prod it ships over email. Print locally as
	// a convenience for the admin who's testing.
	fmt.Printf("Invite sent to %s\n  invite_id:  %s\n  token:      %s\n  expires_at: %s\n",
		email, resp.InviteID, resp.Token, resp.ExpiresAt)
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

// inviteAccept is the existing-user accept path — joins the caller's
// already-enrolled device to the inviting org, auto-switching to it.
// New users without credentials yet can use: hearth login <email> --invite <token>
func inviteAccept(args []string) {
	var token string
	skipConfirm := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--yes", "-y":
			skipConfirm = true
		default:
			if token == "" {
				token = strings.TrimSpace(args[i])
			}
		}
	}
	if token == "" {
		fmt.Fprintf(os.Stderr, "Usage: hearth hh invite accept <token> [--yes]\n")
		os.Exit(1)
	}
	baseURL, err := serverBaseURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}

	// Peek at the invite before committing (unauthenticated endpoint).
	peekResp, err := http.Get(baseURL + "/invites/" + token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: could not fetch invite details: %v\n", err)
		os.Exit(1)
	}
	defer peekResp.Body.Close()
	if peekResp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "hearth: invite not found or expired (HTTP %d)\n", peekResp.StatusCode)
		os.Exit(1)
	}
	var peek struct {
		OrganizationName string    `json:"organization_name"`
		InvitedEmail     string    `json:"invited_email"`
		ExpiresAt        time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(peekResp.Body).Decode(&peek); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: could not parse invite details: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Invite details:\n")
	fmt.Printf("  Organization: %s\n", peek.OrganizationName)
	fmt.Printf("  Invited email: %s\n", peek.InvitedEmail)
	fmt.Printf("  Expires: %s\n", peek.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Println()

	if !skipConfirm {
		fmt.Printf("Accept? [y/N]: ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		if answer != "y" && answer != "yes" {
			fmt.Println("Cancelled.")
			os.Exit(0)
		}
	}

	ioDeviceID := readConfigValue("io_device_id")
	ioDeviceSecret := readConfigValue("io_device_secret")
	if ioDeviceID == "" || ioDeviceSecret == "" {
		fmt.Fprintf(os.Stderr, "hearth: not enrolled (run 'hearth login <email>' first)\n")
		os.Exit(1)
	}
	data, err := deviceAuthedPost(baseURL, "/invites/accept", ioDeviceID, ioDeviceSecret,
		ActionTuple{Kind: "invite", ID: token, Action: "accept"},
		map[string]string{
			"token": token,
		})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	var resp struct {
		OrganizationID   string `json:"organization_id"`
		OrganizationName string `json:"organization_name"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Joined %q. Your current household is now %q.\n", resp.OrganizationName, resp.OrganizationID)
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
