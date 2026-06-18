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
func installHearthInstructions(agent, aiAgentInstanceID, identityPrompt, cwd string) error {
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
	content += hearthSystemPrompt + "\n"
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

// appendSkillToInstructionFile appends the skill body (YAML frontmatter
// stripped) to an existing hearth-owned instruction file at instrPath.
// The section is delimited by a <!-- hearth-skill:<connectionID> --> marker
// so repeated calls for the same connection are idempotent (skipped).
// If instrPath doesn't exist or isn't a hearth file, this is a no-op.
func appendSkillToInstructionFile(instrPath, connectionID, pluginSlug string, skillContent []byte) error {
	existing, err := os.ReadFile(instrPath)
	if err != nil || !isHearthInstructionFile(string(existing)) {
		return nil
	}
	marker := "<!-- hearth-skill:" + connectionID + " -->"
	if strings.Contains(string(existing), marker) {
		return nil // already installed
	}
	body := stripYAMLFrontmatter(skillContent)
	section := "\n" + marker + "\n\n## " + pluginSlug + " (" + connectionID + ")\n\n" + strings.TrimSpace(string(body)) + "\n"
	f, err := os.OpenFile(instrPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(section)
	return err
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
