//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
)

// AgentSetup holds the results of building the agent command, the
// agent-internal session ID (used to locate the agent's transcript
// JSONL on disk), and the ai_agent_instance_id used for server routing.
type AgentSetup struct {
	Command           string
	Args              []string
	AgentSessionID    string // deterministic agent-internal session ID for transcript path derivation
	AIAgentInstanceID string // ai_agent_instance_id for server communication
}

// InterposeSetup holds interpose library state for the caller to manage cleanup.
type InterposeSetup struct {
	LibPath      string
	LibExtracted bool
	SockPath     string
	SockCleanup  func()
	Relay        *interposeRelay // per-instance relay reference; caller must call SetRelay
}

// buildAgentCommand constructs the agent binary command, flags, and IDs.
// The ai_agent_instance_id returned here is a placeholder — callers that
// already have one (the wake/reconcile paths, via spawnAgentInstance →
// req.AIAgentInstanceID) override it in newAgentInstance so the existing
// server row is reused across sleep/wake.
//
// lastSessionID, when non-empty, is the harness-internal session id from
// the prior spawn. For harnesses that accept a session-id flag (claude
// --session-id, copilot --resume, pi --session) we reuse it instead of
// minting fresh, so the model reattaches its prior context window. We
// stat the would-be transcript path first; if the file is gone (user
// cleaned it up, harness GC'd, etc.) we fall back to a fresh id rather
// than handing the harness an id it can't load.
//
// identityPrompt, if non-empty, is prepended to the hearth permission
// instructions so the model also learns who it is and what role it
// fills. Harnesses that don't accept --append-system-prompt receive the
// identity via their on-disk instruction file (see installHearthInstructions).
// resourcePluginPrompt, when non-empty, is appended after the
// standard hearth instructions so the agent knows what plugin
// verbs / connections are available. Built by
// buildResourcePluginPrompt; empty when the daemon has no dev
// connections (zero token cost for non-plugin setups).
func buildAgentCommand(agent, identityPrompt, cwd, lastSessionID, resourcePluginPrompt string) (*AgentSetup, error) {
	command := agentBinary(agent)
	var cmdArgs []string

	// Placeholder ai_agent_instance_id; overridden by newAgentInstance for
	// wake/reconcile callers that already have one.
	aiAgentInstanceID := generateUUID()

	// Generate an agent-internal session ID for agents that support it. This
	// lets us derive the transcript path deterministically, avoiding the bug
	// where two concurrent sessions in the same CWD pick up the same transcript.
	//
	// Resume: if the wake spawn_context handed us a prior session id AND the
	// matching on-disk transcript still exists, reuse it so the harness loads
	// its prior context. Stat-then-decide (rather than blindly trusting the
	// server-stored id) keeps us robust to user cleanup and harness GC.
	var agentSessionID string
	resumingPriorSession := false
	if h, ok := getHarness(agent); ok {
		policy := h.SessionIDPolicy()
		// Mint and HarnessAssigned share the "carry forward last_session_id
		// when the prior transcript is still on disk" branch — both pass
		// the id into Argv on resume. They diverge on fresh spawn: Mint
		// generates a UUID and hands it to the binary up front;
		// HarnessAssigned lets the binary pick its own UUID and the
		// daemon discovers it post-spawn via AssignedSessionID.
		if policy == SessionIDMint || policy == SessionIDHarnessAssigned {
			if lastSessionID != "" && priorSessionUsable(agent, lastSessionID, cwd) {
				agentSessionID = lastSessionID
				resumingPriorSession = true
				log.Printf("agent %s: resuming prior session %s", agent, lastSessionID)
			} else if policy == SessionIDMint {
				if lastSessionID != "" {
					log.Printf("agent %s: prior session %s not on disk, starting fresh", agent, lastSessionID)
				}
				agentSessionID = generateUUID()
			} else if lastSessionID != "" {
				log.Printf("agent %s: prior session %s not on disk, starting fresh (harness will pick a new id)", agent, lastSessionID)
			}
		}
	}

	// Combine identity with the standard hearth prompt for harnesses that
	// take a system-prompt arg. Identity comes first so the model has the
	// "who am I" framing before the operational instructions.
	systemPrompt := hearthSystemPrompt
	if identityPrompt != "" {
		systemPrompt = identityPrompt + "\n\n" + hearthSystemPrompt
	}
	if resourcePluginPrompt != "" {
		systemPrompt = systemPrompt + "\n\n" + resourcePluginPrompt
	}

	// Agent-specific flags come from the Harness registry. Unknown
	// agents land here with no extra flags — buildAgentCommand still
	// returns a command for "claude" (default binary) so the spawn
	// path is at least defensive against typos. See harness_iface.go.
	if h, ok := getHarness(agent); ok {
		cmdArgs = append(cmdArgs, h.Argv(HarnessCtx{
			AgentSessionID:       agentSessionID,
			ResumingPriorSession: resumingPriorSession,
			SystemPrompt:         systemPrompt,
			Cwd:                  cwd,
		})...)
	}

	// Codex's agentSessionID stays empty here. We used to set it to
	// aiAgentInstanceID so startTranscriptStreamer could bypass its
	// mtime-after-spawn gate via a deterministic sentinel lookup. Codex
	// 0.128 broke that approach; we now resolve via session_meta.cwd,
	// which IS ambiguous when an agent name has been reused on the same
	// host. Keeping the bypass on would let the streamer latch onto a
	// previous, retired agent's session file — the new agent's first turn
	// would silently land in a different file that nobody tails. Leaving
	// sessionID empty restores the mtime gate, which correctly demands
	// "this file was written after this agent spawned."

	return &AgentSetup{
		Command:           command,
		Args:              cmdArgs,
		AgentSessionID:    agentSessionID,
		AIAgentInstanceID: aiAgentInstanceID,
	}, nil
}

// priorSessionUsable reports whether the harness's on-disk transcript
// for sessionID still exists, so handing it back via --session-id /
// --resume / --session will actually reattach the prior conversation
// instead of dropping the model into a phantom-empty context.
//
// Conservative: any error or empty path counts as "not usable" — better
// to start a fresh session than to advertise resume and fail silently.
func priorSessionUsable(agent, sessionID, cwd string) bool {
	if sessionID == "" {
		return false
	}
	p := deriveTranscriptPath(agent, sessionID, cwd)
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	// Empty file means the prior spawn never made it past its own auth /
	// trust dialogs — there's nothing to resume.
	return info.Size() > 0
}

// preAcceptClaudeTrust ensures claude's "Do you trust the files in this folder?"
// dialog is pre-accepted for cwd. Without this, on a fresh directory claude
// silently consumes the first user message as the trust-dialog answer — the
// message never becomes a conversation turn and no jsonl is written until the
// second message.
//
// Claude stores trust per-project in ~/.claude.json under
// projects.<abs-cwd>.hasTrustDialogAccepted. We read-modify-write that file,
// preserving every other field.
func preAcceptClaudeTrust(cwd string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".claude.json")

	var root map[string]any
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &root); err != nil {
			log.Printf("preAcceptClaudeTrust: %s is not valid JSON, leaving alone: %v", path, err)
			return
		}
	}
	if root == nil {
		root = map[string]any{}
	}

	projects, _ := root["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		root["projects"] = projects
	}
	proj, _ := projects[cwd].(map[string]any)
	if proj == nil {
		proj = map[string]any{}
		projects[cwd] = proj
	}
	if proj["hasTrustDialogAccepted"] == true {
		return // already trusted
	}
	proj["hasTrustDialogAccepted"] = true

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		log.Printf("preAcceptClaudeTrust: marshal failed: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("preAcceptClaudeTrust: write %s failed: %v", path, err)
	}
}

// preAcceptClaudeBypassPrompt pre-accepts claude 2.1.147+'s
// "Bypass Permissions mode — by proceeding, you accept all
// responsibility" interactive dialog. Without this acceptance, claude
// shows the warning at every fresh spawn and sits at the prompt waiting
// for Enter; our injected first user message gets buffered, claude
// eventually exits non-zero, and the transcript file never lands —
// observed as "Transcript: instance ended before transcript file
// appeared" in the daemon log.
//
// Claude stores the acceptance globally (not per-project) at
// ~/.claude/settings.json as `skipDangerousModePermissionPrompt: true`.
// We read-modify-write that file, preserving every other field.
// Idempotent.
func preAcceptClaudeBypassPrompt() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("preAcceptClaudeBypassPrompt: mkdir %s failed: %v", dir, err)
		return
	}
	path := filepath.Join(dir, "settings.json")

	var root map[string]any
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &root); err != nil {
			log.Printf("preAcceptClaudeBypassPrompt: %s is not valid JSON, leaving alone: %v", path, err)
			return
		}
	}
	if root == nil {
		root = map[string]any{}
	}
	if root["skipDangerousModePermissionPrompt"] == true {
		return // already accepted
	}
	root["skipDangerousModePermissionPrompt"] = true

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		log.Printf("preAcceptClaudeBypassPrompt: marshal failed: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("preAcceptClaudeBypassPrompt: write %s failed: %v", path, err)
	}
}

// seedClaudeBypassSettings writes a project-local
// .claude/settings.local.json into cwd with permissions.defaultMode set to
// "bypassPermissions". Without this, claude-code's bash sandbox is on by
// default even with --dangerously-skip-permissions on the CLI: the flag
// suppresses permission prompts but the sandbox keeps blocking writes
// outside cwd and pipelines like `ps aux | grep`. Symptoms inside hearth
// agents: `go version` (which initializes ~/.cache/go-build) hangs with
// 0-byte output and no error, while read-only commands like `ls`, `cat`,
// `which go` work fine. Autonomous agents have no human to approve
// sandboxed-bash escapes, so we bake the mode in.
//
// settings.local.json (vs settings.json) is claude-code's gitignored-by-
// convention slot — keeps this out of any git commits the agent makes.
func seedClaudeBypassSettings(cwd string) {
	dir := filepath.Join(cwd, ".claude")
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("seedClaudeBypassSettings: mkdir %s failed: %v", dir, err)
		return
	}
	path := filepath.Join(dir, "settings.local.json")

	var root map[string]any
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &root); err != nil {
			log.Printf("seedClaudeBypassSettings: %s is not valid JSON, leaving alone: %v", path, err)
			return
		}
	}
	if root == nil {
		root = map[string]any{}
	}

	perms, _ := root["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
		root["permissions"] = perms
	}
	if perms["defaultMode"] == "bypassPermissions" {
		return // already seeded
	}
	perms["defaultMode"] = "bypassPermissions"

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		log.Printf("seedClaudeBypassSettings: marshal failed: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("seedClaudeBypassSettings: write %s failed: %v", path, err)
	}
}

// preAcceptCopilotTrust ensures copilot-cli's "Confirm folder trust" prompt
// is pre-accepted for cwd. Without this, the first injected message gets
// consumed by the prompt navigator (options 1/2/3) and never reaches the
// agent — bridge stays empty.
//
// Copilot persists the answer in ~/.copilot/config.json under a top-level
// `trustedFolders` array of absolute paths. The file is JSONC (two `//`
// comment lines at the top); strip them before parsing, then re-prepend
// the canonical header on write so copilot still recognizes the file.
func preAcceptCopilotTrust(cwd string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".copilot")
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Printf("preAcceptCopilotTrust: mkdir %s failed: %v", dir, err)
		return
	}
	path := filepath.Join(dir, "config.json")

	const header = "// User settings belong in settings.json.\n// This file is managed automatically.\n"

	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		// Strip leading `//` lines until we hit the opening brace.
		body := data
		for {
			body = bytes.TrimLeft(body, " \t\n\r")
			if len(body) == 0 || body[0] != '/' {
				break
			}
			nl := bytes.IndexByte(body, '\n')
			if nl < 0 {
				body = nil
				break
			}
			body = body[nl+1:]
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &root); err != nil {
				log.Printf("preAcceptCopilotTrust: %s is not valid JSON, leaving alone: %v", path, err)
				return
			}
		}
	}

	folders, _ := root["trustedFolders"].([]any)
	for _, f := range folders {
		if s, ok := f.(string); ok && s == cwd {
			return // already trusted
		}
	}
	root["trustedFolders"] = append(folders, cwd)

	body, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		log.Printf("preAcceptCopilotTrust: marshal failed: %v", err)
		return
	}
	out := append([]byte(header), body...)
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0600); err != nil {
		log.Printf("preAcceptCopilotTrust: write %s failed: %v", path, err)
	}
}

// buildExportEnvs returns the HEARTH_* environment variables for the child
// process, plus any agent-specific model env vars when modelName is non-empty.
func buildExportEnvs(devID, aiAgentInstanceID, proj, bridgePath, agent, modelName string) map[string]string {
	envs := map[string]string{
		"HEARTH_DEVICE_ID":         devID,
		"HEARTH_AGENT_INSTANCE_ID": aiAgentInstanceID,
		"HEARTH_PROJECT":           proj,
		"HEARTH_BRIDGE":            bridgePath,
		"HEARTH_AGENT":             agent,
	}
	// Pin the child to the org's chosen model. Each harness declares
	// its own env knob via ModelEnv; the empty/false return covers the
	// "configures model in own UI" case (copilot, gemini, pi) where
	// modelName is informational.
	if h, ok := getHarness(agent); ok {
		if k, v, set := h.ModelEnv(modelName); set {
			envs[k] = v
		}
	}
	return envs
}

// setupInterpose configures library interposition (DYLD_INSERT_LIBRARIES or
// LD_PRELOAD), starts the interpose socket, and resolves script commands.
// It modifies exportEnvs in place and may modify command/args for dyld.
// Returns the resolved command, args, and interpose state for cleanup.
func setupInterpose(agent, command string, args []string, aiAgentInstanceID string, cwd string, exportEnvs map[string]string) (string, []string, *InterposeSetup, error) {
	setup := &InterposeSetup{}
	setup.LibPath, setup.LibExtracted = findInterposeLib()
	if setup.LibPath == "" {
		return "", nil, nil, fmt.Errorf("interpose library not found")
	}

	if version == "" || version == "dev" {
		logPath := filepath.Join(os.TempDir(), "hearth-interpose-"+aiAgentInstanceID+".log")
		// GREENLIGHT_INTERPOSE_{LOG,SOCK} are the two env var names the
		// prebuilt libhook-*.gz blobs still read. Rename them only when
		// the blobs get rebuilt from the hearth-interpose repo.
		exportEnvs["GREENLIGHT_INTERPOSE_LOG"] = logPath
	}

	if runtime.GOOS == "darwin" {
		entitlementTarget := command
		if err := ensureDyldEntitlement(entitlementTarget); err != nil {
			return "", nil, nil, fmt.Errorf("cannot ensure dyld entitlement for %s: %w", entitlementTarget, err)
		}
		exportEnvs["DYLD_INSERT_LIBRARIES"] = setup.LibPath
		if h, ok := getHarness(agent); ok {
			h.EnsureHelperEntitlements()
		}
	} else {
		exportEnvs["LD_PRELOAD"] = setup.LibPath
	}

	// Set project dir for seccomp path classification (Linux)
	if cwd != "" {
		seccompProjectDir = cwd
	}

	// Start permission socket for interpose library
	sockPath, sockCleanup, ir, err := startInterposeSock(aiAgentInstanceID, agentServerName(agent))
	if err != nil {
		return "", nil, nil, err
	}
	setup.SockPath = sockPath
	setup.SockCleanup = sockCleanup
	setup.Relay = ir
	exportEnvs["GREENLIGHT_INTERPOSE_SOCK"] = sockPath
	log.Printf("Interpose socket: %s", sockPath)
	log.Printf("Interpose library: %s", setup.LibPath)

	// If we're injecting a dylib and the command is a script, launch the
	// interpreter directly to avoid /usr/bin/env stripping DYLD_INSERT_LIBRARIES.
	if exportEnvs["DYLD_INSERT_LIBRARIES"] != "" {
		command, args = resolveScriptCommand(command, args)
	}

	return command, args, setup, nil
}
