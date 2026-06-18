//go:build darwin || linux

package main

// `hearth hh host check` — post-reclaim (or just any-time) reconciliation
// between the server's view of a host's working directories and what's
// actually on disk. Use after a credential-loss recovery (`hearth login`
// will suggest reclaiming a host_id whose WDs may or may not still match
// the filesystem) or any time disk state has drifted from the database.
//
// Lists agents on this host as informational context — non-retired
// agents are shown with their status/pid_status so the user can decide
// whether to wake or retire them via existing commands. We don't
// auto-retire here; that's the user's call.
//
// Uses ws_request via the daemon, so the daemon must be running. The
// caller's working household must be the host's bound household, since
// strict org-scoping means cross-org list queries return empty.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

type checkWD struct {
	ID            string `json:"id"`
	HostID        string `json:"host_id"`
	DirectoryPath string `json:"directory_path"`
	AbandonedAt   string `json:"abandoned_at,omitempty"`
}

type checkAgent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	HostID    string `json:"host_id"`
	Status    string `json:"status"`
	PIDStatus string `json:"pid_status"`
}

func runHostCheck(args []string) {
	fs := flag.NewFlagSet("host check", flag.ExitOnError)
	yes := fs.Bool("yes", false, "Auto-confirm abandoning missing working directories")
	reportOnly := fs.Bool("report-only", false, "Just report drift; never prompt or abandon anything")
	fs.Parse(args)

	hostID := readConfigValue("host_id")
	if hostID == "" {
		fmt.Fprintln(os.Stderr, "hearth: no host_id in config — run 'hearth login' first")
		os.Exit(1)
	}
	orgID := requireWorkingOrgID()

	// Fetch WDs in the working household, filter client-side by host.
	wds, err := fetchWorkingDirectoriesForHost(orgID, hostID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}
	agents, err := fetchAgentsForHost(orgID, hostID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		os.Exit(1)
	}

	if len(wds) == 0 && len(agents) == 0 {
		fmt.Fprintln(os.Stderr, "No working directories or non-retired agents on this host in the current household.")
		fmt.Fprintln(os.Stderr, "If this host is bound to a different household, switch first:")
		fmt.Fprintln(os.Stderr, "  hearth hh household switch <slug>")
		return
	}

	var (
		matched int
		missing []checkWD
	)
	if len(wds) > 0 {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Working directories on this host:")
		for _, wd := range wds {
			info, statErr := os.Stat(wd.DirectoryPath)
			switch {
			case statErr == nil && info.IsDir():
				matched++
				fmt.Fprintf(os.Stderr, "  ✓ %s\n", wd.DirectoryPath)
			case statErr == nil:
				fmt.Fprintf(os.Stderr, "  ! %s — exists but is not a directory\n", wd.DirectoryPath)
			case os.IsNotExist(statErr):
				missing = append(missing, wd)
				fmt.Fprintf(os.Stderr, "  ✗ %s — missing on disk\n", wd.DirectoryPath)
			default:
				fmt.Fprintf(os.Stderr, "  ? %s — stat error: %v\n", wd.DirectoryPath, statErr)
			}
		}
	}

	if len(agents) > 0 {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Non-retired agents on this host:")
		for _, a := range agents {
			pid := a.PIDStatus
			if pid == "" {
				pid = "unknown"
			}
			fmt.Fprintf(os.Stderr, "  - %s (status=%s, pid=%s, id=%s)\n", a.Name, a.Status, pid, a.ID)
		}
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  (Use 'hearth hh agent ...' commands to wake / sleep / retire individual agents.)")
	}

	if len(missing) == 0 {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintf(os.Stderr, "All %d working directories present on disk.\n", matched)
		return
	}

	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "%d working director%s missing on disk.\n", len(missing), pluralY(len(missing)))
	if *reportOnly {
		fmt.Fprintln(os.Stderr, "(--report-only: no changes made.)")
		return
	}

	reader := bufio.NewReader(os.Stdin)
	abandoned := 0
	for _, wd := range missing {
		proceed := *yes
		if !proceed {
			fmt.Fprintf(os.Stderr, "Abandon working directory %s in the database? [y/N]: ", wd.DirectoryPath)
			line, _ := reader.ReadString('\n')
			ans := strings.TrimSpace(strings.ToLower(line))
			proceed = ans == "y" || ans == "yes"
		}
		if !proceed {
			continue
		}
		if _, err := sendWSRequest("abandon_working_directory", map[string]interface{}{"id": wd.ID}); err != nil {
			fmt.Fprintf(os.Stderr, "  failed to abandon %s: %v\n", wd.ID, err)
			continue
		}
		abandoned++
		fmt.Fprintf(os.Stderr, "  abandoned %s\n", wd.DirectoryPath)
	}
	if abandoned > 0 {
		fmt.Fprintf(os.Stderr, "\nDone. Abandoned %d working director%s.\n", abandoned, pluralY(abandoned))
	}
}

func fetchWorkingDirectoriesForHost(orgID, hostID string) ([]checkWD, error) {
	data, err := sendWSRequest("list_working_directories", map[string]interface{}{"organization_id": orgID})
	if err != nil {
		return nil, err
	}
	var resp struct {
		WorkingDirectories []checkWD `json:"working_directories"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode list_working_directories: %w", err)
	}
	out := make([]checkWD, 0, len(resp.WorkingDirectories))
	for _, wd := range resp.WorkingDirectories {
		if wd.HostID != hostID || wd.AbandonedAt != "" {
			continue
		}
		out = append(out, wd)
	}
	return out, nil
}

func fetchAgentsForHost(orgID, hostID string) ([]checkAgent, error) {
	data, err := sendWSRequest("list_ai_agent_instances", map[string]interface{}{"organization_id": orgID})
	if err != nil {
		return nil, err
	}
	var resp struct {
		AIAgentInstances []checkAgent `json:"ai_agent_instances"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode list_ai_agent_instances: %w", err)
	}
	out := make([]checkAgent, 0, len(resp.AIAgentInstances))
	for _, a := range resp.AIAgentInstances {
		if a.HostID != hostID || a.Status == "retired" {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
