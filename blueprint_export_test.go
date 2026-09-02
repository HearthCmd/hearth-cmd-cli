package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Field ORDER is why these are structs and not a map: a blueprint that opens
// with its slug and version reads like a document, not an alphabetised dump.
func TestRenderBlueprintYAMLKeepsFieldOrder(t *testing.T) {
	out, err := renderBlueprintYAML(exportBlueprint{
		Blueprint: "your_namespace/gardener", BlueprintSchema: 1, Version: "0.1.0",
		DisplayName: "Gardener", Summary: "Keeps the garden.", Maintainer: "TODO",
		Items: []exportItem{{
			Handle: "jd", Op: "create", Primitive: "agent_job_description",
			Fields: map[string]interface{}{"title": "Gardener"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"blueprint:", "blueprint_schema:", "version:", "display_name:", "summary:", "maintainer:", "items:"}
	at := -1
	for _, key := range order {
		i := strings.Index(out, key)
		if i < 0 {
			t.Fatalf("rendered output is missing %q:\n%s", key, out)
		}
		if i < at {
			t.Fatalf("%q came out of order:\n%s", key, out)
		}
		at = i
	}
}

// The header says it is a draft, and names the prose a machine cannot produce.
// Saying that in the artifact is more reliable than saying it in a terminal
// someone scrolled past — a blueprint published with TODOs in it helps nobody.
func TestRenderBlueprintYAMLCarriesTheDraftHeader(t *testing.T) {
	out, err := renderBlueprintYAML(exportBlueprint{Blueprint: "a/b", BlueprintSchema: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "# Draft blueprint") {
		t.Fatalf("no draft header:\n%s", out)
	}
	for _, want := range []string{"summary", "maintainer", "guidance", "setup"} {
		if !strings.Contains(out, want) {
			t.Fatalf("header does not name %q:\n%s", want, out)
		}
	}
}

// It must round-trip: what we emit has to parse back as the same document, or
// we are shipping people a file the catalog's own validator will reject.
func TestRenderBlueprintYAMLRoundTrips(t *testing.T) {
	in := exportBlueprint{
		Blueprint: "your_namespace/dj", BlueprintSchema: 1, Version: "0.1.0",
		DisplayName: "DJ", Summary: "Runs music.", Maintainer: "TODO",
		Requires: []exportRequirement{{
			Alias: "music", Kind: "resource_connection",
			AnyOf: []map[string]string{{"plugin": "verge_labs/sonos"}}, IfMissing: "advisory",
		}},
		Skills: []exportSkillRef{{Skill: "verge_labs/music_rooms", Version: "0.1.0"}},
		Items: []exportItem{{
			Handle: "jd", Op: "create", Primitive: "agent_job_description", Risk: "read",
			// A multi-line mandate with punctuation YAML cares about is the case
			// most likely to break rendering, so it is the case under test.
			Fields: map[string]interface{}{
				"title":   "DJ",
				"mandate": "You run music.\n\nPeople will ask: \"put something on\".\n- and lists\n",
			},
		}, {
			Op: "grant", Primitive: "rule", DependsOn: []string{"jd"},
			Fields: map[string]interface{}{"action": "Read", "decision": "allow"},
		}},
	}
	out, err := renderBlueprintYAML(in)
	if err != nil {
		t.Fatal(err)
	}
	var back exportBlueprint
	if err := yaml.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("emitted YAML does not parse: %v\n%s", err, out)
	}
	if back.Blueprint != in.Blueprint || back.DisplayName != in.DisplayName {
		t.Fatalf("header did not survive: %+v", back)
	}
	if len(back.Items) != 2 || back.Items[0].Fields["mandate"] != in.Items[0].Fields["mandate"] {
		t.Fatalf("mandate did not survive the round trip:\n%q", back.Items[0].Fields["mandate"])
	}
	if len(back.Requires) != 1 || back.Requires[0].Alias != "music" {
		t.Fatalf("requirements did not survive: %+v", back.Requires)
	}
	if len(back.Skills) != 1 || back.Skills[0].Skill != "verge_labs/music_rooms" {
		t.Fatalf("skills did not survive: %+v", back.Skills)
	}
}

// Empty optional blocks are omitted rather than emitted as `requires: []`,
// which would read as "this needs nothing" instead of "nothing was found".
func TestRenderBlueprintYAMLOmitsEmptyBlocks(t *testing.T) {
	out, err := renderBlueprintYAML(exportBlueprint{Blueprint: "a/b", BlueprintSchema: 1, Version: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"requires:", "skills:"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("emitted an empty %q:\n%s", unwanted, out)
		}
	}
}
