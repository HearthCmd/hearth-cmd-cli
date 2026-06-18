//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/manifoldco/promptui"
)

// selectItem holds a display label and the underlying ID. The ID type is
// generic so we can share the picker between INTEGER PKs (harnesses,
// organization_positions) and TEXT/UUID PKs (organizations, agents, etc.).
type selectItem[T any] struct {
	Label string
	ID    T
}

// selectFromList renders an interactive selector and returns the chosen ID.
func selectFromList[T any](label string, items []selectItem[T]) (T, error) {
	var zero T
	if len(items) == 0 {
		return zero, fmt.Errorf("no items to select from")
	}

	labels := make([]string, len(items))
	for i, item := range items {
		labels[i] = item.Label
	}

	sel := promptui.Select{
		Label: label,
		Items: labels,
		Size:  10,
	}

	idx, _, err := sel.Run()
	if err != nil {
		return zero, err
	}
	return items[idx].ID, nil
}

// =============================================================================
// selectHarness
// =============================================================================

func selectHarness() (string, string, error) {
	data, err := sendWSRequest("list_harnesses", nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to fetch harnesses: %w", err)
	}

	var resp struct {
		Harnesses []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"harnesses"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", fmt.Errorf("failed to parse harnesses: %w", err)
	}

	if len(resp.Harnesses) == 0 {
		return "", "", fmt.Errorf("no harnesses found — seed the database first")
	}

	items := make([]selectItem[string], len(resp.Harnesses))
	names := make(map[string]string, len(resp.Harnesses))
	for i, h := range resp.Harnesses {
		items[i] = selectItem[string]{Label: h.Name, ID: h.ID}
		names[h.ID] = h.Name
	}
	id, err := selectFromList("Harness", items)
	if err != nil {
		return "", "", err
	}
	return id, names[id], nil
}

// harnessNameByID resolves a harness id to its name via list_harnesses.
// Used when the user passed --harness <id> on the CLI and we still need
// the name to decide whether the model env var is honored.
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

// harnessHonorsModelEnv reports whether the given harness honors a
// per-org model selection. Today only claude-code (ANTHROPIC_MODEL) and
// codex (OPENAI_MODEL) read a model env var; copilot/gemini/pi
// configure their model inside their own UI and ignore the value, so we
// skip the model picker entirely and leave ai_brain_model_id NULL on
// the resulting ai_agent_instances row.
func harnessHonorsModelEnv(harnessName string) bool {
	switch harnessName {
	case "claude-code", "codex":
		return true
	default:
		return false
	}
}

// =============================================================================
// selectHost — picks from the caller's enrolled hosts. No "create new" option
// because host enrollment happens out-of-band via `hearth start`.
// =============================================================================

func selectHost(defaultHostID string) (string, error) {
	data, err := sendWSRequest("list_hosts", map[string]interface{}{})
	if err != nil {
		return "", fmt.Errorf("failed to fetch hosts: %w", err)
	}
	var resp struct {
		Hosts []struct {
			HostID   string `json:"host_id"`
			Hostname string `json:"hostname"`
		} `json:"hosts"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse hosts: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("list_hosts: %s", resp.Error)
	}
	if len(resp.Hosts) == 0 {
		return "", fmt.Errorf("no hosts enrolled — run 'hearth start' on the host you want to use")
	}

	// Put the default host first so it's the initial cursor position in the
	// picker. (selectFromList highlights items[0] by default.)
	items := make([]selectItem[string], 0, len(resp.Hosts))
	var rest []selectItem[string]
	for _, h := range resp.Hosts {
		label := h.Hostname
		if label == "" {
			label = h.HostID
		}
		if h.HostID == defaultHostID {
			label += " (this host)"
			items = append(items, selectItem[string]{Label: label, ID: h.HostID})
		} else {
			rest = append(rest, selectItem[string]{Label: label, ID: h.HostID})
		}
	}
	items = append(items, rest...)

	return selectFromList("Host", items)
}

// =============================================================================
// selectAIBrainModel
// =============================================================================

func selectAIBrainModel(orgID string) (string, error) {
	payload := map[string]interface{}{}
	if orgID != "" {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_ai_brain_models", payload)
	if err != nil {
		return "", fmt.Errorf("failed to fetch AI brain models: %w", err)
	}

	var resp struct {
		AIBrainModels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"ai_brain_models"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse AI brain models: %w", err)
	}

	if len(resp.AIBrainModels) == 0 {
		return "", fmt.Errorf("no AI brain models found — seed the database first")
	}

	items := make([]selectItem[string], len(resp.AIBrainModels))
	for i, m := range resp.AIBrainModels {
		items[i] = selectItem[string]{Label: m.Name, ID: m.ID}
	}
	return selectFromList("AI brain model", items)
}

// =============================================================================
// selectUserOrganization — picker scoped to one human user's memberships
// =============================================================================

// selectUserOrganization fetches every organization the given user belongs to
// (joined via organization_human_users) and renders an interactive picker. A
// "→ Create new…" row at the bottom drops the user into the create flow,
// which both creates the org and adds the user as its owner in one step.
func selectUserOrganization(userID string) (string, error) {
	if userID == "" {
		return "", fmt.Errorf("user_id required")
	}
	data, err := sendWSRequest("list_organizations", map[string]interface{}{"for_human_user_id": userID})
	if err != nil {
		return "", fmt.Errorf("failed to fetch organizations: %w", err)
	}

	var resp struct {
		Organizations []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"organizations"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse organizations: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("list organizations: %s", resp.Error)
	}

	if len(resp.Organizations) == 0 {
		fmt.Println("You don't belong to any households yet. Let's create one.")
		return createUserOrganizationInteractive(userID)
	}

	items := make([]selectItem[string], len(resp.Organizations))
	for i, o := range resp.Organizations {
		items[i] = selectItem[string]{Label: o.Name, ID: o.ID}
	}
	items = append(items, selectItem[string]{Label: "→ Create new…", ID: sentinelCreateNew})

	id, err := selectFromList("Household", items)
	if err != nil {
		return "", err
	}
	if id == sentinelCreateNew {
		return createUserOrganizationInteractive(userID)
	}
	return id, nil
}

// createUserOrganizationInteractive prompts for an org name, creates the row,
// and adds the given user as its owner.
func createUserOrganizationInteractive(userID string) (string, error) {
	if userID == "" {
		return "", fmt.Errorf("user_id required")
	}
	reader := bufio.NewReader(os.Stdin)
	name := promptLine(reader, "Name: ")
	if name == "" {
		return "", fmt.Errorf("name required")
	}

	createData, err := sendWSRequest("create_organization", map[string]interface{}{"name": name})
	if err != nil {
		return "", fmt.Errorf("failed to create household: %w", err)
	}
	var createResp struct {
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(createData, &createResp); err != nil {
		return "", fmt.Errorf("failed to parse create response: %w", err)
	}
	if createResp.Error != "" {
		return "", fmt.Errorf("create household: %s", createResp.Error)
	}
	orgID := createResp.Organization.ID

	addData, err := sendWSRequest("add_organization_member", map[string]interface{}{
		"organization_id": orgID,
		"human_user_id":   userID,
		"role":            "owner",
	})
	if err != nil {
		return "", fmt.Errorf("created household %s but failed to add owner membership: %w", orgID, err)
	}
	var addResp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(addData, &addResp); err == nil && addResp.Error != "" {
		return "", fmt.Errorf("created household %s but failed to add owner membership: %s", orgID, addResp.Error)
	}

	fmt.Printf("Created household %s.\n", orgID)
	return orgID, nil
}

// =============================================================================
// selectAgentJobDescription
// =============================================================================

// sentinelCreateNew is returned by the optional/required selectors to indicate
// that the user picked the "→ Create new…" option. It can never collide with a
// real UUID since UUIDs are 36 chars long with dashes in fixed positions.
const sentinelCreateNew = "__create_new__"

func selectAgentJobDescription(orgID string) (string, error) {
	payload := map[string]interface{}{}
	if orgID != "" {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_agent_job_descriptions", payload)
	if err != nil {
		return "", fmt.Errorf("failed to fetch job descriptions: %w", err)
	}

	var resp struct {
		AgentJobDescriptions []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"agent_job_descriptions"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse job descriptions: %w", err)
	}

	if len(resp.AgentJobDescriptions) == 0 {
		fmt.Println("No job descriptions found. Let's create one.")
		return createAgentJobDescriptionInteractive(orgID)
	}

	items := make([]selectItem[string], len(resp.AgentJobDescriptions))
	for i, j := range resp.AgentJobDescriptions {
		items[i] = selectItem[string]{Label: j.Title, ID: j.ID}
	}
	items = append(items, selectItem[string]{Label: "→ Create new…", ID: sentinelCreateNew})

	id, err := selectFromList("Agent job description", items)
	if err != nil {
		return "", err
	}
	if id == sentinelCreateNew {
		return createAgentJobDescriptionInteractive(orgID)
	}
	return id, nil
}

// selectOptionalAgentJobDescription is like selectAgentJobDescription but adds a
// "→ Skip" option. Returns "" when the user chooses to skip.
func selectOptionalAgentJobDescription(orgID string) (string, error) {
	payload := map[string]interface{}{}
	if orgID != "" {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_agent_job_descriptions", payload)
	if err != nil {
		return "", fmt.Errorf("failed to fetch job descriptions: %w", err)
	}

	var resp struct {
		AgentJobDescriptions []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"agent_job_descriptions"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse job descriptions: %w", err)
	}

	items := make([]selectItem[string], len(resp.AgentJobDescriptions))
	for i, j := range resp.AgentJobDescriptions {
		items[i] = selectItem[string]{Label: j.Title, ID: j.ID}
	}
	items = append(items, selectItem[string]{Label: "→ Create new…", ID: sentinelCreateNew})
	items = append(items, selectItem[string]{Label: "→ Skip (none)", ID: ""})

	id, err := selectFromList("Agent job description (optional)", items)
	if err != nil {
		return "", err
	}
	if id == sentinelCreateNew {
		return createAgentJobDescriptionInteractive(orgID)
	}
	return id, nil // "" = skip
}

func createAgentJobDescriptionInteractive(orgID string) (string, error) {
	if orgID == "" {
		orgID = workingOrgID()
	}
	if orgID == "" {
		return "", fmt.Errorf("no current household set (run 'hearth hh household switch')")
	}

	reader := bufio.NewReader(os.Stdin)
	title := promptLine(reader, "Title: ")
	if title == "" {
		return "", fmt.Errorf("title required")
	}
	mandate := promptLine(reader, "Mandate (optional): ")

	payload := map[string]interface{}{
		"organization_id": orgID,
		"title":           title,
		"priority":        5,
	}
	if mandate != "" {
		payload["mandate"] = mandate
	}
	data, err := sendWSRequest("create_agent_job_description", payload)
	if err != nil {
		return "", fmt.Errorf("failed to create job description: %w", err)
	}

	var resp struct {
		AgentJobDescription struct {
			ID string `json:"id"`
		} `json:"agent_job_description"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("create job description: %s", resp.Error)
	}
	fmt.Printf("Created job description %s.\n", resp.AgentJobDescription.ID)
	return resp.AgentJobDescription.ID, nil
}

// findOrCreateWorkingDirectoryByPath prompts for a directory_path and asks
// the server to either reuse a matching non-abandoned working_directory or
// create one. Used by the agent-create flow, which deliberately does not
// surface existing wd entities to the user — they think in paths, not ids.
func findOrCreateWorkingDirectoryByPath(orgID, hostID, positionName string) (string, error) {
	if orgID == "" {
		orgID = workingOrgID()
	}
	if orgID == "" {
		return "", fmt.Errorf("no current household set (run 'hearth hh household switch')")
	}
	if hostID == "" {
		hostID = readConfigValue("host_id")
	}
	if hostID == "" {
		return "", fmt.Errorf("host ID not found — run 'hearth start' to enroll this host")
	}

	reader := bufio.NewReader(os.Stdin)
	dir := promptWithDefault(reader, "Directory path", defaultAgentWorkingDirFor(orgSlugForID(orgID), positionName))

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
		fmt.Printf("Created working directory %s at %s.\n", resp.WorkingDirectory.ID, dir)
	} else {
		fmt.Printf("Reusing existing working directory %s at %s.\n", resp.WorkingDirectory.ID, dir)
	}
	return resp.WorkingDirectory.ID, nil
}

// =============================================================================
// selectOrganizationPosition
// =============================================================================

func selectOrganizationPosition(orgID, hostID string) (string, error) {
	payload := map[string]interface{}{}
	if orgID != "" {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_organization_positions", payload)
	if err != nil {
		return "", fmt.Errorf("failed to fetch positions: %w", err)
	}

	var resp struct {
		OrganizationPositions []struct {
			ID                    string `json:"id"`
			WorkingDirectoryID    string `json:"working_directory_id"`
			AgentJobDescriptionID string `json:"agent_job_description_id"`
		} `json:"organization_positions"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse positions: %w", err)
	}

	if len(resp.OrganizationPositions) == 0 {
		fmt.Println("No positions found. Let's create one.")
		return createOrganizationPositionInteractive(orgID, hostID)
	}

	// Positions have no name of their own — label by their linked JD title.
	jdTitles, err := fetchAgentJobDescriptionTitlesByID(orgID)
	if err != nil {
		return "", err
	}
	items := make([]selectItem[string], len(resp.OrganizationPositions))
	for i, p := range resp.OrganizationPositions {
		label := jdTitles[p.AgentJobDescriptionID]
		if label == "" {
			label = "(job description missing)"
		}
		items[i] = selectItem[string]{Label: label, ID: p.ID}
	}
	items = append(items, selectItem[string]{Label: "→ Create new…", ID: sentinelCreateNew})

	id, err := selectFromList("Position", items)
	if err != nil {
		return "", err
	}
	if id == sentinelCreateNew {
		return createOrganizationPositionInteractive(orgID, hostID)
	}
	return id, nil
}

func createOrganizationPositionInteractive(orgID, hostID string) (string, error) {
	if orgID == "" {
		orgID = workingOrgID()
	}
	if orgID == "" {
		return "", fmt.Errorf("no current household set (run 'hearth hh household switch')")
	}

	// Positions no longer have names of their own — the linked JD's
	// title is the display label. Pick (or inline-create) the JD first,
	// then derive the default working-directory path from its title.
	jobID, err := selectAgentJobDescription(orgID)
	if err != nil {
		return "", err
	}

	jdTitle, err := fetchAgentJobDescriptionTitle(jobID)
	if err != nil {
		return "", err
	}
	dirSuggestion := toSnakeCase(jdTitle)
	if dirSuggestion == "" {
		return "", fmt.Errorf("job description title must contain at least one alphanumeric character")
	}

	wdID, err := findOrCreateWorkingDirectoryByPath(orgID, hostID, dirSuggestion)
	if err != nil {
		return "", err
	}

	payload := map[string]interface{}{
		"organization_id":          orgID,
		"working_directory_id":     wdID,
		"agent_job_description_id": jobID,
	}
	data, err := sendWSRequest("create_organization_position", payload)
	if err != nil {
		return "", fmt.Errorf("failed to create position: %w", err)
	}

	var resp struct {
		OrganizationPosition struct {
			ID string `json:"id"`
		} `json:"organization_position"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("create position: %s", resp.Error)
	}
	fmt.Printf("Created position %s.\n", resp.OrganizationPosition.ID)
	return resp.OrganizationPosition.ID, nil
}

// fetchAgentJobDescriptionTitle returns the title of a JD by id. Used by
// the position-create flow so the default working-directory path can be
// snake-cased from the JD's title rather than a now-absent position name.
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

// fetchAgentJobDescriptionTitlesByID returns id→title for every JD in the
// org. Used by callers that label positions by their linked JD title since
// positions no longer carry a name of their own.
func fetchAgentJobDescriptionTitlesByID(orgID string) (map[string]string, error) {
	payload := map[string]interface{}{}
	if orgID != "" {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_agent_job_descriptions", payload)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch job descriptions: %w", err)
	}
	var resp struct {
		AgentJobDescriptions []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"agent_job_descriptions"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse job descriptions: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("list job descriptions: %s", resp.Error)
	}
	out := make(map[string]string, len(resp.AgentJobDescriptions))
	for _, j := range resp.AgentJobDescriptions {
		out[j.ID] = j.Title
	}
	return out, nil
}
