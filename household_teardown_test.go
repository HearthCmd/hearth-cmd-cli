//go:build darwin || linux

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Deleting a household erases its folder under the agent home, Claude Code's
// state about the agent folders inside it, and the plugins; it leaves a
// folder someone pointed an agent at themselves, the Claude history about
// that folder, and every other household's folder alone.
func TestEraseHouseholdFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentHome := filepath.Join(home, "hearth_agents")
	root := filepath.Join(agentHome, "lake_house")
	agentDir := filepath.Join(root, "full_time", "gardener")
	ownDir := filepath.Join(home, "code", "project")
	otherHousehold := filepath.Join(agentHome, "lake_house_2", "full_time", "cook")
	plugins := filepath.Join(home, ".hearth", "plugins")
	claude := filepath.Join(home, ".claude")
	sid := "11111111-2222-4333-8444-555555555555"
	ownSid := "99999999-2222-4333-8444-555555555555"

	eraseTestWrite(t, filepath.Join(agentDir, "notes.md"), "x")
	eraseTestWrite(t, filepath.Join(root, "chats", "general", "photo.jpg"), "x")
	eraseTestWrite(t, filepath.Join(ownDir, "main.go"), "package main")
	eraseTestWrite(t, filepath.Join(otherHousehold, "notes.md"), "x")
	eraseTestWrite(t, filepath.Join(plugins, "echo", "manifest.yaml"), "x")
	eraseTestWrite(t, filepath.Join(claude, "projects", sanitizeClaudeProjectHash(agentDir), sid+".jsonl"), `{}`)
	eraseTestWrite(t, filepath.Join(claude, "projects", sanitizeClaudeProjectHash(ownDir), ownSid+".jsonl"), `{}`)

	errs := eraseHouseholdFiles(agentHome, "lake_house", []string{agentDir, ownDir}, plugins)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	for _, gone := range []string{
		root,
		plugins,
		filepath.Join(claude, "projects", sanitizeClaudeProjectHash(agentDir)),
	} {
		if eraseTestExists(gone) {
			t.Errorf("still there: %s", gone)
		}
	}
	for _, kept := range []string{
		filepath.Join(ownDir, "main.go"),
		filepath.Join(otherHousehold, "notes.md"),
		filepath.Join(claude, "projects", sanitizeClaudeProjectHash(ownDir), ownSid+".jsonl"),
	} {
		if !eraseTestExists(kept) {
			t.Errorf("removed something that isn't the household's: %s", kept)
		}
	}
}

// A slug that could escape the agent home, or an agent home that is the
// whole filesystem, erases nothing.
func TestEraseHouseholdFiles_RefusesUnsafePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentHome := filepath.Join(home, "hearth_agents")
	keep := filepath.Join(agentHome, "keep.txt")
	eraseTestWrite(t, keep, "x")

	for _, slug := range []string{"", "..", "../x", "a/b", ".hidden", "/abs", "Lake"} {
		errs := eraseHouseholdFiles(agentHome, slug, nil, "")
		if len(errs) == 0 || !strings.Contains(errs[0], "refusing") {
			t.Errorf("slug %q: errs = %v, want a refusal", slug, errs)
		}
	}
	for _, ah := range []string{"/", "relative/path", ""} {
		errs := eraseHouseholdFiles(ah, "lake_house", nil, "")
		if len(errs) == 0 || !strings.Contains(errs[0], "refusing") {
			t.Errorf("agent home %q: errs = %v, want a refusal", ah, errs)
		}
	}
	if !eraseTestExists(keep) {
		t.Error("a refused teardown removed files")
	}
}

func TestPathInside(t *testing.T) {
	root := "/home/u/hearth_agents/lake_house"
	for _, c := range []struct {
		p    string
		want bool
	}{
		{root, true},
		{root + "/full_time/gardener", true},
		{root + "/../lake_house_2/x", false},
		{"/home/u/hearth_agents/lake_house_2", false},
		{"/home/u/code", false},
		{"relative/x", false},
		{root + "/..hidden", true},
	} {
		if got := pathInside(root, c.p); got != c.want {
			t.Errorf("pathInside(%q) = %v, want %v", c.p, got, c.want)
		}
	}
}

// Paths recorded with the app's "~/" shorthand name the same folders: the
// household's folder is still found, and Claude Code's state about an agent
// folder inside it is still erased.
func TestEraseHouseholdFiles_ExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDir := filepath.Join(home, "hearth_agents", "lake_house", "temp", "ab12cd34")
	claudeProject := filepath.Join(home, ".claude", "projects", sanitizeClaudeProjectHash(agentDir))
	eraseTestWrite(t, filepath.Join(agentDir, "notes.md"), "x")
	eraseTestWrite(t, filepath.Join(claudeProject, "11111111-2222-4333-8444-555555555555.jsonl"), `{}`)

	errs := eraseHouseholdFiles("~/hearth_agents", "lake_house", []string{"~/hearth_agents/lake_house/temp/ab12cd34"}, "")
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if eraseTestExists(agentDir) || eraseTestExists(claudeProject) {
		t.Error("tilde paths were not erased")
	}
}

// The whole teardown: agents stopped (none here), the household's folders and
// plugins erased, and the host signed out (credentials cleared, in memory and
// on disk) while any other setting in the credentials file is kept.
func TestRunHouseholdTeardown_ErasesAndSignsOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentHome := filepath.Join(home, "hearth_agents")
	root := filepath.Join(agentHome, "lake_house")
	plugins := filepath.Join(home, ".hearth", "plugins")
	eraseTestWrite(t, filepath.Join(root, "full_time", "gardener", "notes.md"), "x")
	eraseTestWrite(t, filepath.Join(plugins, "echo", "manifest.yaml"), "x")
	for k, v := range map[string]string{
		"user_id": "u1", "host_id": "h1", "host_secret": "s1",
		"io_device_id": "d1", "io_device_secret": "ds1", "email": "a@example.com",
		"keep_me": "yes",
	} {
		if err := writeConfigValue(k, v); err != nil {
			t.Fatal(err)
		}
	}

	d := &Daemon{
		instances:     map[string]*AgentInstance{},
		agentHomePath: agentHome,
		pluginsDir:    plugins,
		hostID:        "h1",
		hostSecret:    "s1",
		humanUserID:   "u1",
	}
	d.runHouseholdTeardown(householdTeardownFrame{
		OrganizationID:     "org-1",
		HouseholdSlug:      "lake_house",
		WorkingDirectories: []string{filepath.Join(root, "full_time", "gardener")},
	})

	if eraseTestExists(root) || eraseTestExists(plugins) {
		t.Error("household folders or plugins survived")
	}
	for _, k := range []string{"user_id", "host_id", "host_secret", "io_device_id", "io_device_secret", "email"} {
		if v := readConfigValue(k); v != "" {
			t.Errorf("%s still set: %q", k, v)
		}
	}
	if readConfigValue("keep_me") != "yes" {
		t.Error("an unrelated setting was lost")
	}
	if d.hostID != "" || d.hostSecret != "" || d.humanUserID != "" {
		t.Error("the daemon is still signed in in memory")
	}
}

// The relay's household_teardown frame reaches the daemon's handler.
func TestHandleTextFrame_RoutesHouseholdTeardown(t *testing.T) {
	d := newTestDaemonWS()
	var got []byte
	d.householdTeardownFunc = func(data []byte) { got = data }
	frame := []byte(`{"type":"household_teardown","organization_id":"org-1","household_slug":"lake_house","ai_agent_instance_id":""}`)
	if !d.handleTextFrame(frame) {
		t.Fatal("household_teardown not consumed")
	}
	if string(got) != string(frame) {
		t.Errorf("handler got %s", got)
	}
}
