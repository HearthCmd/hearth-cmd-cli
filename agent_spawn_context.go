//go:build darwin || linux

package main

// agentSpawnContext is the server-computed context for spawning one agent —
// the `spawn_context` block that rides create_ai_agent_instance and every wake.
//
// ONE struct, shared by both spawn paths. Until now the wake handler and the
// reconcile pass each declared their own copy of the same shape and then
// exploded it into a positional argument list that had reached thirteen strings.
// Two structs of one shape is a divergence waiting to happen — the two must
// agree, because they describe the same wire message — and a fourteenth
// positional string was the point at which "add one more" stopped being
// reasonable.
type agentSpawnContext struct {
	HarnessName   string `json:"harness_name"`
	HostID        string `json:"host_id"`
	DirectoryPath string `json:"directory_path"`
	ModelProvider string `json:"model_provider"`
	ModelName     string `json:"model_name"`
	AgentName     string `json:"agent_name"`
	JobTitle      string `json:"job_title"`
	JobMandate    string `json:"job_mandate"`

	OrganizationName string `json:"organization_name"`
	// HouseholdContext is who else works here plus the household's handbook,
	// composed server-side (docs/introductions.md §3).
	HouseholdContext string `json:"household_context"`

	// LastSessionID is the harness-internal session id from the prior spawn,
	// so the harness can reattach its context window. Empty on first wake.
	LastSessionID string `json:"last_session_id"`
	// SystemPrompt is the server-owned versioned hearth boilerplate. Empty when
	// the server didn't send one, in which case the compiled-in fallback is used.
	SystemPrompt string `json:"system_prompt"`
	// Displays is a JSON array of household screens this agent may publish to.
	Displays string `json:"displays"`
	// Skills is a JSON array of resolved catalog skills, content included — the
	// relay fetches, the daemon places (docs/blueprints.md §4).
	Skills string `json:"skills"`
}
