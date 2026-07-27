//go:build darwin || linux

package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// installHearthInstructions creates an agent-specific instruction file
// that teaches the agent how to interpret [GREENLIGHT] permission denial
// messages. (The bracket literal is still `[GREENLIGHT]` because the
// prebuilt libhook-*.gz blobs emit it; rename when the blobs get rebuilt.)
// For codex, aiAgentInstanceID is embedded as a sentinel so we can match the
// transcript to this instance even when multiple instances share the same CWD.
// hearthBody is the server-owned, versioned hearth boilerplate
// (spawn_context.system_prompt). Empty falls back to the compiled-in
// hearthSystemPromptFallback so the instruction file is never written without
// the permission-denial semantics.
func installHearthInstructions(agent, aiAgentInstanceID, identityPrompt, cwd, hearthBody string) error {
	if hearthBody == "" {
		hearthBody = hearthSystemPromptFallback
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	var instrPath string
	switch agent {
	case "gemini":
		instrPath = filepath.Join(cwd, "GEMINI.md")
	case "copilot":
		if err := os.MkdirAll(filepath.Join(cwd, ".github"), 0755); err != nil {
			return err
		}
		instrPath = filepath.Join(cwd, ".github", "copilot-instructions.md")
	case "codex":
		instrPath = filepath.Join(cwd, "AGENTS.md")
	default:
		return nil
	}

	// Don't overwrite an existing file that the user created. Ours starts
	// with the `<!-- hearth -->` marker.
	if _, err := os.Stat(instrPath); err == nil {
		existing, err := os.ReadFile(instrPath)
		if err == nil && !isHearthInstructionFile(string(existing)) {
			log.Printf("Skipping %s — user file exists", instrPath)
			return nil
		}
	}

	content := "<!-- hearth -->\n"
	if identityPrompt != "" {
		content += identityPrompt + "\n\n"
	}
	content += hearthBody + "\n"
	if agent == "codex" && aiAgentInstanceID != "" {
		content += "<!-- hearth-agent-instance:" + aiAgentInstanceID + " -->\n"
	}
	if err := os.WriteFile(instrPath, []byte(content), 0644); err != nil {
		return err
	}
	log.Printf("Installed hearth instructions in %s", instrPath)
	return nil
}

// removeHearthInstructions removes the instruction file only if it was
// created by hearth (contains our marker).
func removeHearthInstructions(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if isHearthInstructionFile(string(data)) {
		if err := os.Remove(path); err == nil {
			log.Printf("Removed hearth instructions %s", path)
		}
	}
}

func isHearthInstructionFile(content string) bool {
	return strings.Contains(content, "<!-- hearth -->")
}

// appendSkillToInstructionFile installs the skill body (YAML frontmatter
// stripped) into an existing hearth-owned instruction file at instrPath.
// The section is delimited by a <!-- hearth-skill:<connectionID> --> marker.
// If instrPath doesn't exist or isn't a hearth file, this is a no-op.
//
// The marked section is entirely hearth-owned (the plugin's skill.md is the
// source of truth, not a user document). So when the section already exists we
// replace it in place if the plugin's skill body changed — a plugin upgrade
// shipping a fixed skill must reach an agent that already holds the old one,
// which is the whole point of re-running this on every (re)spawn — and no-op
// when it's byte-identical to avoid needless rewrites.
func appendSkillToInstructionFile(instrPath, connectionID, pluginSlug string, skillContent []byte) error {
	existing, err := os.ReadFile(instrPath)
	if err != nil || !isHearthInstructionFile(string(existing)) {
		return nil
	}
	content := string(existing)
	body := stripYAMLFrontmatter(skillContent)
	section := "\n<!-- hearth-skill:" + connectionID + " -->\n\n## " + pluginSlug + " (" + connectionID + ")\n\n" + strings.TrimSpace(string(body)) + "\n"

	// The section always lands on disk preceded by its leading "\n", so match
	// on the newline-prefixed marker to find its true span.
	markerNL := "\n<!-- hearth-skill:" + connectionID + " -->"
	idx := strings.Index(content, markerNL)
	if idx < 0 {
		// Not present yet — append.
		f, err := os.OpenFile(instrPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(section)
		return err
	}
	// Already installed — replace the section in place, preserving any later
	// skill sections and their order. The section runs from its leading "\n"
	// up to the next hearth-skill marker (or EOF).
	end := len(content)
	if n := strings.Index(content[idx+len(markerNL):], "\n<!-- hearth-skill:"); n >= 0 {
		end = idx + len(markerNL) + n
	}
	updated := content[:idx] + section + content[end:]
	if updated == content {
		return nil // already up to date
	}
	return os.WriteFile(instrPath, []byte(updated), 0o644)
}

// stripSkillFromInstructionFile removes a previously-appended hearth-skill
// section from instrPath. The section is identified by the
// <!-- hearth-skill:<connectionID> --> marker written by
// appendSkillToInstructionFile. Idempotent — no-op if the marker isn't
// present or the file isn't a hearth-owned instruction file.
func stripSkillFromInstructionFile(instrPath, connectionID string) error {
	data, err := os.ReadFile(instrPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	content := string(data)
	if !isHearthInstructionFile(content) {
		return nil
	}
	marker := "\n<!-- hearth-skill:" + connectionID + " -->"
	idx := strings.Index(content, marker)
	if idx < 0 {
		return nil // not installed
	}
	// Find the start of the next skill section (if any) so we don't eat it.
	rest := content[idx+len(marker):]
	nextIdx := strings.Index(rest, "\n<!-- hearth-skill:")
	var stripped string
	if nextIdx < 0 {
		stripped = strings.TrimRight(content[:idx], "\n") + "\n"
	} else {
		stripped = content[:idx] + "\n" + rest[nextIdx+1:]
	}
	return os.WriteFile(instrPath, []byte(stripped), 0o644)
}

// stripYAMLFrontmatter removes the leading --- ... --- YAML block from
// markdown content, returning just the body. If no frontmatter is
// present the content is returned unchanged.
func stripYAMLFrontmatter(content []byte) []byte {
	s := string(content)
	if !strings.HasPrefix(s, "---") {
		return content
	}
	// find the closing ---
	rest := s[3:]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return content
	}
	body := rest[idx+4:] // skip past "\n---"
	return []byte(strings.TrimLeft(body, "\n"))
}
