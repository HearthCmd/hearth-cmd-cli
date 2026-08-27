//go:build darwin || linux

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// cmd_agent_inbox.go — `hearth hh agent inbox [instance-id]`.
//
// Read-only view of the host's message inboxes (agent_inbox.go). This is what
// the host owner runs when `hearth status` says an agent has messages waiting
// or, worse, undelivered: it names the message, where it came from, how long it
// has been queued, how many delivery attempts it survived, and shows enough of
// the body to tell whether it mattered.
//
// Deliberately read-only. Requeueing by hand is a foot-gun (the agent may have
// moved on; the sender may have given up and said it another way), and the
// daemon already retries on its own. The value here is knowing what was lost,
// not replaying it.

func runAgentInbox(args []string) {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Fprintf(os.Stderr, "Usage: hearth hh agent inbox [ai_agent_instance_id]\n\n"+
				"Shows messages queued for delivery to agents on this host, and any the\n"+
				"daemon could not deliver. With no id, shows every agent's inbox.\n")
			os.Exit(0)
		}
	}

	var instanceID string
	if len(args) > 0 {
		instanceID = args[0]
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		if u, uerr := user.Current(); uerr == nil {
			home = u.HomeDir
		}
	}
	if home == "" {
		fmt.Fprintf(os.Stderr, "hearth: cannot resolve home directory\n")
		os.Exit(1)
	}

	// Read the daemon's sqlite directly. WAL makes a concurrent reader safe
	// while the daemon holds it open, and going through the IPC socket would
	// mean this command only worked while the daemon was running — exactly
	// the case where you most want to see what was stranded.
	dbPath := filepath.Join(home, ".hearth", "daemon.db")
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "hearth: no daemon database at %s (nothing has run on this host yet)\n", dbPath)
		os.Exit(1)
	}
	// OpenDaemonDB logs a readiness line meant for the daemon's log, not a
	// person's terminal.
	prevLog := log.Writer()
	log.SetOutput(io.Discard)
	db, err := OpenDaemonDB(home)
	log.SetOutput(prevLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	entries, err := ListAgentInbox(db, instanceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	if len(entries) == 0 {
		if instanceID != "" {
			fmt.Printf("Inbox for %s is empty.\n", shortID(instanceID))
		} else {
			fmt.Println("All agent inboxes on this host are empty.")
		}
		return
	}

	now := time.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATE\tSOURCE\tAGE\tTRIES\tMESSAGE")
	var quarantined int
	for _, e := range entries {
		state := e.State
		if e.State == "pending" && e.Expired(now) {
			state = "expired"
		}
		if e.State == "quarantined" {
			quarantined++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\n",
			shortID(e.InstanceID), state, e.Source,
			humanDuration(now.Sub(e.EnqueuedAt)),
			e.Attempts, e.MaxAttempts,
			previewPayload(e.Payload))
	}
	tw.Flush()

	if quarantined > 0 {
		fmt.Println()
		fmt.Printf("%d message(s) could not be delivered. Reasons:\n", quarantined)
		for _, e := range entries {
			if e.State != "quarantined" {
				continue
			}
			fmt.Printf("  %s  %s\n", shortID(e.InstanceID), e.Reason)
		}
		fmt.Println("\nThese never reached the agent. If one mattered, say it again from the app.")
	}
}

// previewPayload renders the human-meaningful part of a queued payload: the
// body after the hearth envelope header, collapsed onto one line and clipped.
func previewPayload(payload []byte) string {
	s := string(payload)
	if strings.HasPrefix(s, "hearth/") {
		if _, rest, found := strings.Cut(s, "\n"); found {
			s = rest
		}
	}
	s = normalizeProbeText(s)
	const max = 60
	if len(s) > max {
		return s[:max-1] + "…"
	}
	if s == "" {
		return "(empty)"
	}
	return s
}
