package main

import (
	"os"
	"path/filepath"
	"testing"
)

// pluginManifestPath returns the path to the named plugin's manifest.yaml
// under the plugins/ source directory tree. slug is the full namespaced
// slug, e.g. "verge_labs/ha".
func pluginManifestPath(slug string) string {
	return filepath.Join("plugins", filepath.FromSlash(slug), "manifest.yaml")
}

func testPluginManifest(t *testing.T, slug string, mustVerbs []string) PluginManifest {
	t.Helper()
	data, err := os.ReadFile(pluginManifestPath(slug))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if m.PluginSlug != slug {
		t.Errorf("plugin_slug = %q; want %q", m.PluginSlug, slug)
	}
	if ClassifyManifestSource(m) != SourceDeclarative {
		t.Errorf("classification = %q; want declarative", ClassifyManifestSource(m))
	}
	verbNames := map[string]bool{}
	for _, v := range m.Verbs {
		verbNames[v.Name] = true
	}
	for _, must := range mustVerbs {
		if !verbNames[must] {
			t.Errorf("verb %q missing from %s manifest", must, slug)
		}
	}
	return m
}

func TestGitHub_ManifestParsesAndValidates(t *testing.T) {
	m := testPluginManifest(t, "verge_labs/github", []string{
		"get_repo", "list_issues", "get_issue",
		"list_pull_requests", "get_pull_request",
		"get_file", "list_commits", "search_issues",
		"create_issue", "update_issue", "add_issue_comment",
		"create_pull_request", "add_pr_review_comment",
	})
	if m.Snapshot == nil {
		t.Error("snapshot block missing")
	}

	verbNames := map[string]bool{}
	for _, v := range m.Verbs {
		verbNames[v.Name] = true
	}
	for _, r := range m.DefaultRules {
		prefix := "external_resource.verge_labs/github."
		verb := r.Action[len(prefix):]
		if !verbNames[verb] {
			t.Errorf("default_rule action %q references undeclared verb %q", r.Action, verb)
		}
	}

	si, err := os.Stat(filepath.Join("plugins", "verge_labs", "github", "skill.md"))
	if err != nil {
		t.Fatalf("skill.md missing: %v", err)
	}
	if si.Size() == 0 {
		t.Error("skill.md is empty")
	}
}

func TestHomeAssistant_ManifestParsesAndValidates(t *testing.T) {
	m := testPluginManifest(t, "verge_labs/home_assistant", []string{
		"get_state", "list_entities", "turn_on", "turn_off", "set_scene", "lock", "unlock",
	})
	if m.Snapshot == nil {
		t.Error("snapshot block missing")
	}

	var turnOn *PluginVerb
	for i := range m.Verbs {
		if m.Verbs[i].Name == "turn_on" {
			turnOn = &m.Verbs[i]
			break
		}
	}
	if turnOn == nil || turnOn.HTTP == nil {
		t.Fatal("turn_on verb missing or has no http block")
	}
	got, err := substitute(turnOn.HTTP.URL, map[string]any{
		"config": map[string]any{"ha_url": "http://ha.local:8123"},
		"args":   map[string]any{"entity_id": "light.kitchen"},
	})
	if err != nil {
		t.Fatalf("substitute turn_on url: %v", err)
	}
	if got != "http://ha.local:8123/api/services/light/turn_on" {
		t.Errorf("turn_on url substituted = %q", got)
	}
}

func TestGooglePeople_ManifestParsesAndValidates(t *testing.T) {
	m := testPluginManifest(t, "verge_labs/google_people", []string{
		"search_people", "list_people", "get_person",
	})
	if m.Snapshot == nil {
		t.Error("snapshot block missing")
	}

	si, err := os.Stat(filepath.Join("plugins", "verge_labs", "google_people", "skill.md"))
	if err != nil {
		t.Fatalf("skill.md missing: %v", err)
	}
	if si.Size() == 0 {
		t.Error("skill.md is empty")
	}
}

func TestGoogleCalendar_ManifestParsesAndValidates(t *testing.T) {
	m := testPluginManifest(t, "verge_labs/google_calendar", []string{
		"list_calendars", "list_events", "get_event", "check_availability",
		"create_event", "update_event", "cancel_event",
	})
	if m.Snapshot == nil {
		t.Error("snapshot block missing")
	}

	si, err := os.Stat(filepath.Join("plugins", "verge_labs", "google_calendar", "skill.md"))
	if err != nil {
		t.Fatalf("skill.md missing: %v", err)
	}
	if si.Size() == 0 {
		t.Error("skill.md is empty")
	}
}

func TestGoogleDrive_ManifestParsesAndValidates(t *testing.T) {
	m := testPluginManifest(t, "verge_labs/google_drive", []string{
		"list_files", "list_folder_contents", "get_file_metadata",
		"download_file", "export_file", "search_files",
		"create_file", "upload_file_content",
		"rename_file", "move_file", "create_folder",
		"trash_file", "share_file",
	})
	if m.Snapshot == nil {
		t.Error("snapshot block missing")
	}

	si, err := os.Stat(filepath.Join("plugins", "verge_labs", "google_drive", "skill.md"))
	if err != nil {
		t.Fatalf("skill.md missing: %v", err)
	}
	if si.Size() == 0 {
		t.Error("skill.md is empty")
	}
}
