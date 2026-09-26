//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// claudeSessionIDRe matches a Claude Code session id (a UUID): the name of a
// transcript file, and of the per-session directories it keeps.
var claudeSessionIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// eraseClaudeSessionState removes what Claude Code keeps about cwd outside cwd
// itself, so a destroyed temp agent really leaves nothing behind (its working
// directory is removed separately). Under ~/.claude:
//
//   - projects/<cwd as a name>/ — the session transcripts: every prompt, tool
//     call, result and answer;
//   - file-history/<session>/ and session-env/<session>/ for those sessions —
//     backups of files it edited, and its per-session environment;
//   - history.jsonl lines whose "project" is cwd — the prompts it was sent;
//
// and the projects[cwd] entry in ~/.claude.json. Best-effort: every step logs
// and carries on. Only called for a directory Hearth created (the relay sends
// a working_dir to erase only then), and a no-op for an agent that never ran
// Claude Code there.
func eraseClaudeSessionState(cwd string) {
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) == "/" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	claudeDir := filepath.Join(home, ".claude")
	name := sanitizeClaudeProjectHash(cwd)
	if strings.Trim(name, "-") == "" {
		return
	}
	projDir := filepath.Join(claudeDir, "projects", name)

	if entries, err := os.ReadDir(projDir); err == nil {
		for _, e := range entries {
			id := strings.TrimSuffix(e.Name(), ".jsonl")
			if id == e.Name() || !claudeSessionIDRe.MatchString(id) {
				continue
			}
			for _, sub := range []string{"file-history", "session-env"} {
				if err := os.RemoveAll(filepath.Join(claudeDir, sub, id)); err != nil {
					log.Printf("eraseClaudeSessionState: %s/%s: %v", sub, id, err)
				}
			}
		}
	}
	if err := os.RemoveAll(projDir); err != nil {
		log.Printf("eraseClaudeSessionState: %s: %v", projDir, err)
	}
	dropClaudeHistoryForProject(filepath.Join(claudeDir, "history.jsonl"), cwd)
	dropClaudeProjectEntry(filepath.Join(home, ".claude.json"), cwd)
}

// dropClaudeHistoryForProject rewrites Claude Code's prompt history without
// the lines recorded for project cwd. Lines it can't parse are kept.
func dropClaudeHistoryForProject(path, cwd string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var kept bytes.Buffer
	dropped := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var entry struct {
			Project string `json:"project"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Project == cwd {
			dropped++
			continue
		}
		kept.Write(line)
		kept.WriteByte('\n')
	}
	if sc.Err() != nil || dropped == 0 {
		return
	}
	tmp := path + ".hearth-tmp"
	if err := os.WriteFile(tmp, kept.Bytes(), 0o600); err != nil {
		log.Printf("eraseClaudeSessionState: history: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("eraseClaudeSessionState: history: %v", err)
		_ = os.Remove(tmp)
	}
}

// dropClaudeProjectEntry removes projects[cwd] from ~/.claude.json, preserving
// everything else. A file that isn't valid JSON is left alone.
func dropClaudeProjectEntry(path, cwd string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		log.Printf("eraseClaudeSessionState: %s is not valid JSON, leaving alone: %v", path, err)
		return
	}
	projects, _ := root["projects"].(map[string]any)
	if _, ok := projects[cwd]; !ok {
		return
	}
	delete(projects, cwd)
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		log.Printf("eraseClaudeSessionState: write %s: %v", path, err)
	}
}
