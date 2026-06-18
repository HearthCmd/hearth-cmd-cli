//go:build darwin || linux

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// localAgentHomeBase returns the local-default base directory used as a
// suggestion in CLI prompts ($HOME/hearth_agents). It does NOT consult
// the server-side agent_home_path — server is SOT for the host's actual
// configured value. Callers that need the configured value should query
// the daemon over IPC (see `hearth status` → identity_response).
//
// Returns "" only when the user has no home directory; callers fall
// back to cwd in that case.
func localAgentHomeBase() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "hearth_agents")
}

// defaultAgentWorkingDir returns the default base directory used as the
// pre-filled value in interactive `hearth wd` / `hearth select` prompts:
// $HOME/hearth_agents/<org_slug>. This is a suggestion only — the host's
// authoritative agent_home_path lives server-side and is set at first
// enrollment. If the user's actual configured base differs from
// $HOME/hearth_agents, they retype the path at this prompt. Falls back
// to the current working directory if the home directory can't be
// resolved. The directory is NOT created.
func defaultAgentWorkingDir(orgSlug string) string {
	base := localAgentHomeBase()
	if base == "" {
		if cwd, err := os.Getwd(); err == nil {
			return cwd
		}
		return ""
	}
	if orgSlug == "" {
		return base
	}
	return filepath.Join(base, orgSlug)
}

// defaultAgentWorkingDirFor returns the suggested working directory for a
// given org + position name: $HOME/hearth_agents/<org_slug>/full_time/<snake_name>.
// If the name is empty, returns defaultAgentWorkingDir(orgSlug).
func defaultAgentWorkingDirFor(orgSlug, positionName string) string {
	base := defaultAgentWorkingDir(orgSlug)
	sub := toSnakeCase(positionName)
	if sub == "" {
		return base
	}
	return filepath.Join(base, "full_time", sub)
}

// toSnakeCase lowercases the input and replaces any run of non-alphanumeric
// characters with a single underscore. Apostrophes are dropped entirely
// (not treated as separators) so "Zed's Household" → "zeds_household"
// instead of "zed_s_household". Leading / trailing underscores are
// trimmed. Example: "Head Gardener" → "head_gardener".
func toSnakeCase(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevUnderscore := true // suppress leading underscores
	for _, r := range s {
		switch {
		case r == '\'' || r == '’':
			// Drop apostrophes (ASCII ' and curly ’) without inserting
			// a separator so possessives stay glued to the root word.
			continue
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
			prevUnderscore = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return strings.TrimRight(b.String(), "_")
}

// --- Prompt helpers ---

func promptLine(r *bufio.Reader, label string) string {
	fmt.Print(label)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

func promptWithDefault(r *bufio.Reader, label, def string) string {
	fmt.Printf("%s [%s]: ", label, def)
	line, _ := r.ReadString('\n')
	s := strings.TrimSpace(line)
	if s == "" {
		return def
	}
	return s
}

// =============================================================================
// hearth wd — working directory CRUD (top-level command)
// =============================================================================

func runWD(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, `Usage: hearth wd <command> [flags]

Commands:
  list      List working directories in the current household
  get       Get a working directory by ID
  create    Create a new working directory entry
  update    Update a working directory's host or path
  abandon   Mark a working directory as abandoned

Run 'hearth wd <command> --help' for details.
`)
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("wd list", flag.ExitOnError)
		orgID := fs.String("household", "", "Household ID (defaults to current household from config)")
		fs.Parse(args[1:])
		if *orgID == "" {
			*orgID = requireWorkingOrgID()
		}
		data, err := sendWSRequest("list_working_directories", map[string]interface{}{"organization_id": *orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("wd get", flag.ExitOnError)
		id := fs.String("id", "", "Working directory ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_working_directory", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("wd create", flag.ExitOnError)
		hostID := fs.String("host-id", "", "Host ID (defaults to daemon's host)")
		dir := fs.String("dir", "", "Directory path (defaults to $HOME/hearth_agents/<org_slug>)")
		fs.Parse(args[1:])

		orgID := requireWorkingOrgID()

		reader := bufio.NewReader(os.Stdin)

		if *hostID == "" {
			*hostID = readConfigValue("host_id")
		}
		if *hostID == "" {
			*hostID = promptLine(reader, "Host ID: ")
		}
		if *hostID == "" {
			fmt.Fprintf(os.Stderr, "hearth: host ID required (run 'hearth start' to enroll)\n")
			os.Exit(1)
		}
		if *dir == "" {
			*dir = promptWithDefault(reader, "Directory path", defaultAgentWorkingDir(orgSlugForID(orgID)))
		}

		payload := map[string]interface{}{
			"organization_id": orgID,
			"host_id":         *hostID,
			"directory_path":  *dir,
		}
		runCreateOrUpdateWithCollisionPrompt("create_working_directory", "working_directory", "abandon", "directory_path", payload)
	case "update":
		fs := flag.NewFlagSet("wd update", flag.ExitOnError)
		id := fs.String("id", "", "Working directory ID")
		hostID := fs.String("host-id", "", "New host ID")
		dir := fs.String("dir", "", "New directory path")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		payload := map[string]interface{}{"id": *id}
		if *hostID != "" {
			payload["host_id"] = *hostID
		}
		if *dir != "" {
			payload["directory_path"] = *dir
		}
		runCreateOrUpdateWithCollisionPrompt("update_working_directory", "working_directory", "abandon", "directory_path", payload)
	case "abandon":
		fs := flag.NewFlagSet("wd abandon", flag.ExitOnError)
		id := fs.String("id", "", "Working directory ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "hearth: --id required\n")
			os.Exit(1)
		}
		runArchivalWithCascadePrompt("abandon_working_directory", "working_directory", *id)
	default:
		fmt.Fprintf(os.Stderr, "hearth wd: unknown command %q\nRun 'hearth wd --help' for usage.\n", args[0])
		os.Exit(1)
	}
}
