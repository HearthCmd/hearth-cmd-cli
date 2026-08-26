//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Non-interactive lookup/resolve helpers used by the flag-driven `hearth hh`
// create flows. These replaced the interactive `select*` pickers that used to
// live in select.go — day-to-day entity creation happens in the app; the CLI
// keeps only the single-command, flags-in path (for humans scripting and for
// agents shelling out to `hearth hh …`).

// harnessNameByID resolves a harness id to its name via list_harnesses.
// Used when the caller passed --harness <id> and we still need the name to
// decide whether the model env var is honored.
func harnessNameByID(id string) (string, error) {
	data, err := sendWSRequest("list_harnesses", nil)
	if err != nil {
		return "", fmt.Errorf("failed to fetch harnesses: %w", err)
	}
	var resp struct {
		Harnesses []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"harnesses"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse harnesses: %w", err)
	}
	for _, h := range resp.Harnesses {
		if h.ID == id {
			return h.Name, nil
		}
	}
	return "", fmt.Errorf("harness id %s not found", id)
}

// harnessHonorsModelEnv reports whether the given harness honors a per-org
// model selection. Today only claude-code (ANTHROPIC_MODEL) and codex
// (OPENAI_MODEL) read a model env var; copilot/gemini/pi configure their model
// inside their own UI and ignore the value, so a supplied --model is left off
// the ai_agent_instances row.
func harnessHonorsModelEnv(harnessName string) bool {
	switch harnessName {
	case "claude-code", "codex":
		return true
	default:
		return false
	}
}

// fetchAgentJobDescriptionTitle returns the title of a JD by id. Used by the
// position-create flow so the default working-directory path can be
// snake-cased from the JD's title (positions carry no name of their own).
func fetchAgentJobDescriptionTitle(id string) (string, error) {
	data, err := sendWSRequest("get_agent_job_description", map[string]interface{}{"id": id})
	if err != nil {
		return "", fmt.Errorf("failed to fetch job description: %w", err)
	}
	var resp struct {
		AgentJobDescription struct {
			Title string `json:"title"`
		} `json:"agent_job_description"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse job description: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("get job description: %s", resp.Error)
	}
	if resp.AgentJobDescription.Title == "" {
		return "", fmt.Errorf("job description %s has no title", id)
	}
	return resp.AgentJobDescription.Title, nil
}

// findOrCreateWorkingDirectory asks the server to reuse a matching
// non-abandoned working_directory at dir, or create one. Non-interactive
// (the interactive path-prompting variant was removed with the create
// wizards). Prints which row was reused/created to stderr so a scripted
// caller's stdout stays clean for the entity it's creating.
func findOrCreateWorkingDirectory(orgID, hostID, dir string) (string, error) {
	if orgID == "" {
		orgID = workingOrgID()
	}
	if orgID == "" {
		return "", fmt.Errorf("no current household set (re-bind this host with 'hearth login <email>')")
	}
	if hostID == "" {
		hostID = readConfigValue("host_id")
	}
	if hostID == "" {
		return "", fmt.Errorf("host ID not found — run 'hearth start' to enroll this host")
	}

	data, err := sendWSRequest("find_or_create_working_directory", map[string]interface{}{
		"organization_id": orgID,
		"host_id":         hostID,
		"directory_path":  dir,
	})
	if err != nil {
		return "", fmt.Errorf("find_or_create_working_directory: %w", err)
	}
	var resp struct {
		WorkingDirectory struct {
			ID string `json:"id"`
		} `json:"working_directory"`
		Created bool   `json:"created"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse find_or_create response: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("find_or_create_working_directory: %s", resp.Error)
	}
	if resp.Created {
		fmt.Fprintf(os.Stderr, "Created working directory %s at %s.\n", resp.WorkingDirectory.ID, dir)
	} else {
		fmt.Fprintf(os.Stderr, "Reusing existing working directory %s at %s.\n", resp.WorkingDirectory.ID, dir)
	}
	return resp.WorkingDirectory.ID, nil
}
