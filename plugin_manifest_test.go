package main

import (
	"strings"
	"testing"
)

const sampleHAManifest = `
plugin_slug: ha
display_name: Home Assistant
version: 0.1.0
manifest_schema: 1
description: |
  Control devices in this home's Home Assistant install.

credentials:
  - name: ha_url
    description: WebSocket URL of the HA instance
    secret: false
  - name: ha_token
    description: Long-lived access token
    secret: true

executable: ./hearth-plugin-ha

verbs:
  - name: turn_on
    description: Turn on a light, switch, or scene.
    args:
      entity_id:
        type: string
        required: true
    output: json
  - name: lock
    description: Lock a smart lock.
    args:
      entity_id:
        type: string
        required: true
    output: json

default_rules:
  - action: external_resource.ha.turn_on
    decision: allow
    when:
      entity_kind: ha.light
  - action: external_resource.ha.lock
    decision: ask
`

func TestParseManifest_HappyPath(t *testing.T) {
	m, err := ParseManifest([]byte(sampleHAManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	if m.PluginSlug != "ha" {
		t.Errorf("PluginSlug = %q; want ha", m.PluginSlug)
	}
	if m.DisplayName != "Home Assistant" {
		t.Errorf("DisplayName = %q", m.DisplayName)
	}
	if m.Version != "0.1.0" {
		t.Errorf("Version = %q", m.Version)
	}
	if m.ManifestSchema != 1 {
		t.Errorf("ManifestSchema = %d", m.ManifestSchema)
	}
	if !strings.HasPrefix(m.Description, "Control devices") {
		t.Errorf("Description = %q", m.Description)
	}
	if m.Executable != "./hearth-plugin-ha" {
		t.Errorf("Executable = %q", m.Executable)
	}

	if len(m.Credentials) != 2 {
		t.Fatalf("Credentials len = %d; want 2", len(m.Credentials))
	}
	if m.Credentials[0].Name != "ha_url" || m.Credentials[0].Secret {
		t.Errorf("Credentials[0] = %+v", m.Credentials[0])
	}
	if m.Credentials[1].Name != "ha_token" || !m.Credentials[1].Secret {
		t.Errorf("Credentials[1] = %+v", m.Credentials[1])
	}

	if len(m.Verbs) != 2 {
		t.Fatalf("Verbs len = %d; want 2", len(m.Verbs))
	}
	if m.Verbs[0].Name != "turn_on" {
		t.Errorf("Verbs[0].Name = %q", m.Verbs[0].Name)
	}
	if m.Verbs[0].Output != "json" {
		t.Errorf("Verbs[0].Output = %q", m.Verbs[0].Output)
	}
	entSpec, ok := m.Verbs[0].Args["entity_id"]
	if !ok {
		t.Fatalf("Verbs[0].Args missing entity_id")
	}
	if entSpec.Type != "string" || !entSpec.Required {
		t.Errorf("Verbs[0].Args[entity_id] = %+v", entSpec)
	}

	if len(m.DefaultRules) != 2 {
		t.Fatalf("DefaultRules len = %d; want 2", len(m.DefaultRules))
	}
	if m.DefaultRules[0].Action != "external_resource.ha.turn_on" {
		t.Errorf("DefaultRules[0].Action = %q", m.DefaultRules[0].Action)
	}
	if m.DefaultRules[0].Decision != "allow" {
		t.Errorf("DefaultRules[0].Decision = %q", m.DefaultRules[0].Decision)
	}
	if k, _ := m.DefaultRules[0].When["entity_kind"].(string); k != "ha.light" {
		t.Errorf("DefaultRules[0].When[entity_kind] = %v", m.DefaultRules[0].When["entity_kind"])
	}

	// SourceDir is NOT set by ParseManifest — only by the registry
	// at load time. Confirm it's empty here.
	if m.SourceDir != "" {
		t.Errorf("derived field populated by ParseManifest: SourceDir=%q", m.SourceDir)
	}
}

func TestParseManifest_MalformedYAMLReturnsError(t *testing.T) {
	_, err := ParseManifest([]byte("{ not valid yaml at all"))
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error should mention yaml: %v", err)
	}
}

func TestParseManifest_EmptyInputProducesZeroValue(t *testing.T) {
	m, err := ParseManifest([]byte(""))
	if err != nil {
		t.Fatalf("ParseManifest(empty): %v", err)
	}
	// All fields default-zero; not an error at parse time. The
	// validation step (next commit) catches missing required fields.
	if m.PluginSlug != "" || m.ManifestSchema != 0 {
		t.Errorf("expected zero values from empty input, got %+v", m)
	}
}

// ---------- ValidateManifest ----------

// validManifest is the baseline that ValidateManifest accepts; the
// subtests below mutate one field at a time to assert the gate.
func validManifest() PluginManifest {
	return PluginManifest{
		PluginSlug:     "ha",
		DisplayName:    "Home Assistant",
		Version:        "0.1.0",
		ManifestSchema: 1,
		Executable:     "./hearth-plugin-ha",
		Verbs: []PluginVerb{
			{Name: "turn_on", Description: "Turn it on."},
		},
	}
}

func TestValidateManifest_HappyPath(t *testing.T) {
	if err := ValidateManifest(validManifest()); err != nil {
		t.Fatalf("baseline manifest must validate: %v", err)
	}
}

func TestValidateManifest_MissingPluginSlug(t *testing.T) {
	m := validManifest()
	m.PluginSlug = ""
	err := ValidateManifest(m)
	if err == nil || !strings.Contains(err.Error(), "plugin_slug") {
		t.Errorf("expected plugin_slug error, got %v", err)
	}
}

func TestValidateManifest_MissingSchema(t *testing.T) {
	m := validManifest()
	m.ManifestSchema = 0
	err := ValidateManifest(m)
	if err == nil || !strings.Contains(err.Error(), "manifest_schema") {
		t.Errorf("expected manifest_schema error, got %v", err)
	}
}

func TestValidateManifest_UnsupportedSchema(t *testing.T) {
	m := validManifest()
	m.ManifestSchema = 99
	err := ValidateManifest(m)
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("expected unsupported-schema error, got %v", err)
	}
}

func TestValidateManifest_EmptyDisplayName(t *testing.T) {
	m := validManifest()
	m.DisplayName = ""
	err := ValidateManifest(m)
	if err == nil || !strings.Contains(err.Error(), "display_name") {
		t.Errorf("expected display_name error, got %v", err)
	}
}

func TestValidateManifest_EmptyExecutable(t *testing.T) {
	m := validManifest()
	m.Executable = ""
	err := ValidateManifest(m)
	if err == nil || !strings.Contains(err.Error(), "executable") {
		t.Errorf("expected executable error, got %v", err)
	}
}

func TestValidateManifest_NamespaceConsistency(t *testing.T) {
	m := validManifest()
	m.Namespace = "verge_labs"
	m.PluginSlug = "verge_labs/ha"
	if err := ValidateManifest(m); err != nil {
		t.Errorf("namespace + matching slug must validate; got %v", err)
	}

	m2 := validManifest()
	m2.Namespace = "verge_labs"
	m2.PluginSlug = "ha" // missing namespace prefix
	err := ValidateManifest(m2)
	if err == nil {
		t.Error("namespace mismatch should fail validation")
	}
}

func TestPluginVerb_NewFields(t *testing.T) {
	v := PluginVerb{
		Name:            "create_event",
		GrantCategories: []string{"write", "events"},
		Deprecated:      true,
		ReplacedBy:      "create_event_v2",
	}
	if len(v.GrantCategories) != 2 || v.GrantCategories[0] != "write" {
		t.Errorf("GrantCategories = %v; want [write events]", v.GrantCategories)
	}
	if !v.Deprecated || v.ReplacedBy != "create_event_v2" {
		t.Errorf("Deprecated/ReplacedBy not round-tripped: deprecated=%v replaced_by=%q", v.Deprecated, v.ReplacedBy)
	}
}

func TestValidateManifest_ZeroVerbsAllowed(t *testing.T) {
	m := validManifest()
	m.Verbs = nil
	if err := ValidateManifest(m); err != nil {
		t.Errorf("zero-verb manifest must validate (degenerate but legitimate); got %v", err)
	}
}

func TestManifestSchemaSupported(t *testing.T) {
	if !manifestSchemaSupported(1) {
		t.Error("schema 1 must be supported")
	}
	if !manifestSchemaSupported(2) {
		t.Error("schema 2 must be supported (declarative adapters)")
	}
	if manifestSchemaSupported(0) {
		t.Error("schema 0 (zero-value) must not be supported")
	}
	if manifestSchemaSupported(99) {
		t.Error("schema 99 must not be supported (no future-faking)")
	}
}

// ---------- ClassifyManifestSource + declarative validation ----------

func validDeclarativeManifest() PluginManifest {
	return PluginManifest{
		PluginSlug:     "ha",
		DisplayName:    "Home Assistant",
		Version:        "0.1.0",
		ManifestSchema: 2,
		Verbs: []PluginVerb{
			{
				Name:        "get_state",
				Description: "Read current state.",
				HTTP: &VerbHTTPSpec{
					Method: "GET",
					URL:    "{{config.ha_url}}/api/states/{{args.entity_id}}",
				},
			},
		},
	}
}

func TestClassifyManifestSource_BinaryByDefault(t *testing.T) {
	if got := ClassifyManifestSource(validManifest()); got != SourceBinary {
		t.Errorf("baseline binary manifest classified as %q; want %q", got, SourceBinary)
	}
}

func TestClassifyManifestSource_HTTPVerbDeclarative(t *testing.T) {
	if got := ClassifyManifestSource(validDeclarativeManifest()); got != SourceDeclarative {
		t.Errorf("manifest with http: verb classified as %q; want %q", got, SourceDeclarative)
	}
}

func TestValidateManifest_DeclarativeHappyPath(t *testing.T) {
	if err := ValidateManifest(validDeclarativeManifest()); err != nil {
		t.Fatalf("declarative manifest must validate: %v", err)
	}
}

func TestValidateManifest_DeclarativeRejectsSchema1(t *testing.T) {
	m := validDeclarativeManifest()
	m.ManifestSchema = 1
	err := ValidateManifest(m)
	if err == nil || !strings.Contains(err.Error(), "manifest_schema >= 2") {
		t.Errorf("expected schema-version refusal; got %v", err)
	}
}

func TestValidateManifest_DeclarativeRejectsExecutable(t *testing.T) {
	m := validDeclarativeManifest()
	m.Executable = "./should-not-be-here"
	err := ValidateManifest(m)
	if err == nil || !strings.Contains(err.Error(), "declarative plugins must not declare executable") {
		t.Errorf("expected declarative+executable refusal; got %v", err)
	}
}

func TestValidateManifest_RejectsMixedVerbs(t *testing.T) {
	m := validDeclarativeManifest()
	// Add a verb with no http: block. Mixed manifests are refused.
	m.Verbs = append(m.Verbs, PluginVerb{Name: "ad_hoc", Description: "no http here"})
	err := ValidateManifest(m)
	if err == nil || !strings.Contains(err.Error(), "mixed verbs not allowed") {
		t.Errorf("expected mixed-verb refusal; got %v", err)
	}
}

func TestValidateManifest_DeclarativeRequiresMethodAndURL(t *testing.T) {
	cases := map[string]func(*VerbHTTPSpec){
		"missing method": func(h *VerbHTTPSpec) { h.Method = "" },
		"missing url":    func(h *VerbHTTPSpec) { h.URL = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := validDeclarativeManifest()
			mutate(m.Verbs[0].HTTP)
			if err := ValidateManifest(m); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestValidateManifest_SnapshotShapes(t *testing.T) {
	// Baseline declarative manifest with a valid snapshot.
	withSnapshot := func() PluginManifest {
		m := validDeclarativeManifest()
		m.Snapshot = &SnapshotSpec{
			HTTP: VerbHTTPSpec{
				Method: "GET",
				URL:    "{{config.ha_url}}/api/states",
			},
			Extract: ExtractSpec{
				Iterate:  "$[*]",
				EntityID: "$.entity_id",
				Kind:     ExtractKindSpec{PrefixWithEntityDomain: "ha."},
			},
		}
		return m
	}

	if err := ValidateManifest(withSnapshot()); err != nil {
		t.Fatalf("baseline declarative+snapshot must validate: %v", err)
	}

	t.Run("missing iterate", func(t *testing.T) {
		m := withSnapshot()
		m.Snapshot.Extract.Iterate = ""
		if err := ValidateManifest(m); err == nil || !strings.Contains(err.Error(), "iterate") {
			t.Errorf("expected iterate error; got %v", err)
		}
	})
	t.Run("missing entity_id", func(t *testing.T) {
		m := withSnapshot()
		m.Snapshot.Extract.EntityID = ""
		if err := ValidateManifest(m); err == nil || !strings.Contains(err.Error(), "entity_id") {
			t.Errorf("expected entity_id error; got %v", err)
		}
	})
	t.Run("no kind mode set", func(t *testing.T) {
		m := withSnapshot()
		m.Snapshot.Extract.Kind = ExtractKindSpec{}
		if err := ValidateManifest(m); err == nil || !strings.Contains(err.Error(), "kind") {
			t.Errorf("expected kind error; got %v", err)
		}
	})
	t.Run("two kind modes set", func(t *testing.T) {
		m := withSnapshot()
		m.Snapshot.Extract.Kind = ExtractKindSpec{
			Literal:                "slack.channel",
			PrefixWithEntityDomain: "ha.",
		}
		if err := ValidateManifest(m); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("expected mutually-exclusive error; got %v", err)
		}
	})
}

func TestParseManifest_DeclarativeYAML(t *testing.T) {
	yaml := `
plugin_slug: ha
display_name: Home Assistant
manifest_schema: 2

verbs:
  - name: get_state
    description: Read current state.
    args:
      entity_id: {type: string, required: true}
    http:
      method: GET
      url: "{{config.ha_url}}/api/states/{{args.entity_id}}"
      headers:
        Authorization: "Bearer {{credentials.ha_token}}"
      response:
        success_status: [200]
        output: $.state
`
	m, err := ParseManifest([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(m.Verbs) != 1 || m.Verbs[0].HTTP == nil {
		t.Fatalf("verb http block missing: %+v", m.Verbs)
	}
	h := m.Verbs[0].HTTP
	if h.Method != "GET" {
		t.Errorf("Method = %q", h.Method)
	}
	if h.URL != "{{config.ha_url}}/api/states/{{args.entity_id}}" {
		t.Errorf("URL = %q", h.URL)
	}
	if h.Headers["Authorization"] != "Bearer {{credentials.ha_token}}" {
		t.Errorf("Authorization header = %q", h.Headers["Authorization"])
	}
	if h.Response.Output != "$.state" {
		t.Errorf("Response.Output = %q", h.Response.Output)
	}
	if len(h.Response.SuccessStatus) != 1 || h.Response.SuccessStatus[0] != 200 {
		t.Errorf("Response.SuccessStatus = %v", h.Response.SuccessStatus)
	}
	if ClassifyManifestSource(m) != SourceDeclarative {
		t.Errorf("classification = %q; want declarative", ClassifyManifestSource(m))
	}
}

func TestParseManifest_SnapshotYAML(t *testing.T) {
	yaml := `
plugin_slug: ha
display_name: Home Assistant
manifest_schema: 2

verbs:
  - name: get_state
    http:
      method: GET
      url: "{{config.ha_url}}/api/states/{{args.entity_id}}"

snapshot:
  description: Pull HA state list and emit normalized entities.
  http:
    method: GET
    url: "{{config.ha_url}}/api/states"
    headers:
      Authorization: "Bearer {{credentials.ha_token}}"
  extract:
    iterate: $[*]
    entity_id: $.entity_id
    kind:
      prefix_with_entity_domain: "ha."
    labels:
      friendly_name: $.attributes.friendly_name
      area: $.attributes.area_id
`
	m, err := ParseManifest([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if m.Snapshot == nil {
		t.Fatal("snapshot block missing")
	}
	if m.Snapshot.HTTP.Method != "GET" || m.Snapshot.HTTP.URL == "" {
		t.Errorf("snapshot.http = %+v", m.Snapshot.HTTP)
	}
	if m.Snapshot.Extract.Iterate != "$[*]" || m.Snapshot.Extract.EntityID != "$.entity_id" {
		t.Errorf("snapshot.extract = %+v", m.Snapshot.Extract)
	}
	if m.Snapshot.Extract.Kind.PrefixWithEntityDomain != "ha." {
		t.Errorf("kind = %+v", m.Snapshot.Extract.Kind)
	}
	if m.Snapshot.Extract.Labels["friendly_name"] != "$.attributes.friendly_name" {
		t.Errorf("labels = %+v", m.Snapshot.Extract.Labels)
	}
}
