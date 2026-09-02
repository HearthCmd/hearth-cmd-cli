package main

// `hearth blueprint export` — turn a role this household already got right into
// a draft blueprint (docs/blueprints.md §11).
//
// The division of labour: the RELAY decides what belongs in a blueprint and
// replaces this household's ids with placeholders (it has all the data in one
// place); this renders the result as YAML. Rendering lives here because
// gopkg.in/yaml.v3 is already a CLI dependency, and adding one to the relay
// purely to save a round trip would be a worse trade.
//
// Field ORDER is the reason these are structs rather than a map: yaml.v3 emits
// struct fields in declaration order, and a blueprint that opens with its slug
// and version reads like a document instead of an alphabetised dump.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type exportRequirement struct {
	Alias     string              `json:"alias" yaml:"alias"`
	Kind      string              `json:"kind" yaml:"kind"`
	Role      string              `json:"role,omitempty" yaml:"role,omitempty"`
	AnyOf     []map[string]string `json:"any_of,omitempty" yaml:"any_of,omitempty"`
	Guidance  string              `json:"guidance,omitempty" yaml:"guidance,omitempty"`
	Optional  bool                `json:"optional,omitempty" yaml:"optional,omitempty"`
	IfMissing string              `json:"if_missing,omitempty" yaml:"if_missing,omitempty"`
}

type exportSkillRef struct {
	Skill   string `json:"skill" yaml:"skill"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
}

type exportItem struct {
	Handle    string                 `json:"handle,omitempty" yaml:"handle,omitempty"`
	Op        string                 `json:"op" yaml:"op"`
	Primitive string                 `json:"primitive" yaml:"primitive"`
	Risk      string                 `json:"risk,omitempty" yaml:"risk,omitempty"`
	Why       string                 `json:"why,omitempty" yaml:"why,omitempty"`
	DependsOn []string               `json:"depends_on,omitempty" yaml:"depends_on,omitempty,flow"`
	Fields    map[string]interface{} `json:"fields" yaml:"fields"`
}

type exportBlueprint struct {
	Blueprint       string              `json:"blueprint" yaml:"blueprint"`
	BlueprintSchema int                 `json:"blueprint_schema" yaml:"blueprint_schema"`
	Version         string              `json:"version" yaml:"version"`
	DisplayName     string              `json:"display_name" yaml:"display_name"`
	Summary         string              `json:"summary" yaml:"summary"`
	Maintainer      string              `json:"maintainer" yaml:"maintainer"`
	Requires        []exportRequirement `json:"requires,omitempty" yaml:"requires,omitempty"`
	Skills          []exportSkillRef    `json:"skills,omitempty" yaml:"skills,omitempty"`
	Items           []exportItem        `json:"items" yaml:"items"`
}

// draftHeader is prepended to the file. An export is a DRAFT, and saying so in
// the artifact is more reliable than saying it in a terminal someone scrolled
// past — the prose fields are the half a machine cannot produce, and a blueprint
// published with "TODO" still in it helps nobody.
const draftHeader = `# Draft blueprint, exported from a live household.
#
# What is here was derived from a role that actually works. What is NOT here is
# the prose only you can write:
#
#   * summary      — one sentence; it is what a list row shows.
#   * maintainer   — who to come back to.
#   * guidance     — for each requirement, when it is the right choice and what
#                    it costs. This is the part a reasoner cannot work out.
#   * setup        — numbered, human-world steps to obtain each requirement. You
#                    know where the vendor hides the setting; nobody else does.
#
# Check the mandate before publishing: anything specific to your household
# (names, rooms, habits) either belongs in a parameter or should come out.
#
# Format reference: SCHEMA.md in HearthCmd/hearth-blueprints
`

func runBlueprintExport(args []string) {
	fs := flag.NewFlagSet("hearth blueprint export", flag.ExitOnError)
	positionID := fs.String("position", "", "The organization_position id to export. Find it with `hearth hh position list`.")
	out := fs.String("out", "", "Write to this file instead of stdout.")
	fs.Parse(args)

	if *positionID == "" {
		fmt.Fprintln(os.Stderr, "hearth blueprint export: --position is required")
		os.Exit(1)
	}

	data, err := sendWSRequest("export_blueprint", map[string]interface{}{
		"organization_position_id": *positionID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth blueprint export: %v\n", err)
		os.Exit(1)
	}
	var resp struct {
		Blueprint exportBlueprint `json:"blueprint"`
		Error     string          `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "hearth blueprint export: could not read response: %v\n", err)
		os.Exit(1)
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "hearth blueprint export: %s\n", resp.Error)
		os.Exit(1)
	}

	rendered, err := renderBlueprintYAML(resp.Blueprint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth blueprint export: %v\n", err)
		os.Exit(1)
	}

	if *out == "" {
		fmt.Print(rendered)
		return
	}
	if err := os.WriteFile(*out, []byte(rendered), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "hearth blueprint export: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Wrote %s — a draft. Fill in the TODOs before publishing it.\n", *out)
}

// renderBlueprintYAML is separated from the command so the output shape is
// testable without a server.
func renderBlueprintYAML(bp exportBlueprint) (string, error) {
	body, err := yaml.Marshal(bp)
	if err != nil {
		return "", fmt.Errorf("could not render the blueprint: %v", err)
	}
	return draftHeader + "\n" + string(body), nil
}

// runBlueprint dispatches `hearth blueprint <subcommand>`. Only `export` today;
// installing a blueprint is not a CLI act — a person reviews and applies one
// from the app, which is the guardrail, not a missing feature.
func runBlueprint(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hearth blueprint export --position <id> [--out <path>]")
		os.Exit(1)
	}
	switch args[0] {
	case "export":
		runBlueprintExport(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "hearth blueprint: unknown subcommand %q (try: export)\n", args[0])
		os.Exit(1)
	}
}
