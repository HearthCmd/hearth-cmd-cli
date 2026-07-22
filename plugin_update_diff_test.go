package main

import (
	"strings"
	"testing"
)

func manifestWithVerbs(names ...string) PluginManifest {
	m := PluginManifest{PluginSlug: "verge_labs/x"}
	for _, n := range names {
		m.Verbs = append(m.Verbs, PluginVerb{Name: n})
	}
	return m
}

func withRequiredConfig(m PluginManifest, keys ...string) PluginManifest {
	req := make([]interface{}, 0, len(keys))
	for _, k := range keys {
		req = append(req, k)
	}
	m.ConfigSchema = map[string]interface{}{"required": req}
	return m
}

// Adding capability must not interrupt anyone. This is the common case — a
// plugin gains a verb — and making it prompt would train people to click
// through the prompt that actually matters.
func TestDiff_AddedVerbsAreNotBreaking(t *testing.T) {
	old := manifestWithVerbs("list", "get")
	new := manifestWithVerbs("list", "get", "create")

	d := diffPluginManifests(old, new)
	if d.Breaking() {
		t.Errorf("adding a verb must not be breaking: %+v", d)
	}
	if len(d.VerbsAdded) != 1 || d.VerbsAdded[0] != "create" {
		t.Errorf("VerbsAdded = %v, want [create]", d.VerbsAdded)
	}
	if len(d.VerbsRemoved) != 0 {
		t.Errorf("VerbsRemoved = %v, want none", d.VerbsRemoved)
	}
}

// The core hazard: rules are keyed on the verb name, so a removed verb takes
// every rule naming it out of circulation with no error anywhere.
func TestDiff_RemovedVerbIsBreaking(t *testing.T) {
	old := manifestWithVerbs("list", "get", "delete")
	new := manifestWithVerbs("list", "get")

	d := diffPluginManifests(old, new)
	if !d.Breaking() {
		t.Fatal("removing a verb must be breaking")
	}
	if len(d.VerbsRemoved) != 1 || d.VerbsRemoved[0] != "delete" {
		t.Errorf("VerbsRemoved = %v, want [delete]", d.VerbsRemoved)
	}
}

// A rename is a remove plus an add, and the remove half is what breaks. This
// is the case the whole feature exists for: it looks like a no-op in a
// changelog and silently orphans every rule naming the old verb.
func TestDiff_RenameIsDetectedAsRemoval(t *testing.T) {
	old := manifestWithVerbs("list_events")
	new := manifestWithVerbs("list_calendar_events")

	d := diffPluginManifests(old, new)
	if !d.Breaking() {
		t.Fatal("a verb rename must be breaking")
	}
	if len(d.VerbsRemoved) != 1 || d.VerbsRemoved[0] != "list_events" {
		t.Errorf("VerbsRemoved = %v, want [list_events]", d.VerbsRemoved)
	}
	if len(d.VerbsAdded) != 1 || d.VerbsAdded[0] != "list_calendar_events" {
		t.Errorf("VerbsAdded = %v, want [list_calendar_events]", d.VerbsAdded)
	}
}

func TestDiff_IdenticalManifestIsNotBreaking(t *testing.T) {
	m := manifestWithVerbs("list", "get")
	if d := diffPluginManifests(m, m); d.Breaking() {
		t.Errorf("identical manifests must not be breaking: %+v", d)
	}
}

// A newly required config key invalidates every existing connection, which
// has no value for it.
func TestDiff_NewlyRequiredConfigIsBreaking(t *testing.T) {
	old := manifestWithVerbs("list")
	new := withRequiredConfig(manifestWithVerbs("list"), "base_url")

	d := diffPluginManifests(old, new)
	if !d.Breaking() {
		t.Fatal("a newly required config key must be breaking")
	}
	if len(d.ConfigKeysNowRequired) != 1 || d.ConfigKeysNowRequired[0] != "base_url" {
		t.Errorf("ConfigKeysNowRequired = %v, want [base_url]", d.ConfigKeysNowRequired)
	}
}

func TestDiff_AlreadyRequiredConfigIsNotBreaking(t *testing.T) {
	old := withRequiredConfig(manifestWithVerbs("list"), "base_url")
	new := withRequiredConfig(manifestWithVerbs("list"), "base_url")

	if d := diffPluginManifests(old, new); d.Breaking() {
		t.Errorf("an already-required key must not re-trigger: %+v", d)
	}
}

func TestDiff_DroppingARequirementIsNotBreaking(t *testing.T) {
	// Relaxing a constraint cannot invalidate an existing connection.
	old := withRequiredConfig(manifestWithVerbs("list"), "base_url")
	new := manifestWithVerbs("list")

	if d := diffPluginManifests(old, new); d.Breaking() {
		t.Errorf("dropping a required key must not be breaking: %+v", d)
	}
}

// config_schema is author-supplied JSON Schema. A shape we cannot read must
// not become a refusal nobody can act on.
func TestDiff_MalformedConfigSchemaIsTolerated(t *testing.T) {
	old := manifestWithVerbs("list")
	for _, bad := range []map[string]interface{}{
		{"required": "base_url"},                  // string, not a list
		{"required": []interface{}{1, 2}},         // non-strings
		{"required": nil},                         // null
		{"properties": map[string]interface{}{}},  // no required key at all
	} {
		new := manifestWithVerbs("list")
		new.ConfigSchema = bad
		if d := diffPluginManifests(old, new); d.Breaking() {
			t.Errorf("unreadable config_schema %v must not be treated as breaking: %+v", bad, d)
		}
	}
}

// Stable ordering: map iteration would otherwise reshuffle the verb list and
// make two identical refusals read as different problems.
func TestDiff_RemovedVerbsAreSorted(t *testing.T) {
	old := manifestWithVerbs("zebra", "alpha", "mike")
	new := manifestWithVerbs()

	d := diffPluginManifests(old, new)
	want := []string{"alpha", "mike", "zebra"}
	if len(d.VerbsRemoved) != len(want) {
		t.Fatalf("VerbsRemoved = %v", d.VerbsRemoved)
	}
	for i := range want {
		if d.VerbsRemoved[i] != want[i] {
			t.Fatalf("VerbsRemoved = %v, want %v", d.VerbsRemoved, want)
		}
	}
}

// The message has to name what breaks. "This update is breaking" is not
// something anyone can act on.
func TestDiff_DescribeNamesTheConsequence(t *testing.T) {
	old := manifestWithVerbs("list", "delete")
	new := withRequiredConfig(manifestWithVerbs("list"), "base_url")

	msg := diffPluginManifests(old, new).Describe("verge_labs/x")
	for _, want := range []string{"verge_labs/x", "delete", "base_url", "rule"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Describe() should mention %q, got: %s", want, msg)
		}
	}
}

func TestDiff_DescribeIsEmptyWhenNotBreaking(t *testing.T) {
	m := manifestWithVerbs("list")
	if msg := diffPluginManifests(m, m).Describe("verge_labs/x"); msg != "" {
		t.Errorf("non-breaking diff should describe as empty, got %q", msg)
	}
}

// ---------- impact reporting ----------

// The three impact states must read differently. Conflating "no rules break"
// with "we could not check" is the same failure this project keeps producing
// in new places — an absence rendered as reassurance.
func TestDescribe_ImpactStatesAreDistinct(t *testing.T) {
	base := diffPluginManifests(manifestWithVerbs("list", "delete"), manifestWithVerbs("list"))

	unknown := base
	unknown.ImpactFetched = false
	msgUnknown := unknown.Describe("verge_labs/x")

	zero := base
	zero.ImpactFetched = true
	msgZero := zero.Describe("verge_labs/x")

	some := base
	some.ImpactFetched = true
	some.OrphanedRuleCount = 3
	some.AffectedAgents = []string{"Calendar Assistant"}
	msgSome := some.Describe("verge_labs/x")

	if msgUnknown == msgZero || msgZero == msgSome || msgUnknown == msgSome {
		t.Fatalf("impact states must read differently:\n unknown=%q\n zero=%q\n some=%q",
			msgUnknown, msgZero, msgSome)
	}
	if !strings.Contains(msgUnknown, "unknown") {
		t.Errorf("an undetermined impact must say so, got: %s", msgUnknown)
	}
	// Must not blame the network: a reachable relay that refuses the request
	// lands in this same state, and "could not reach the server" would send
	// someone debugging connectivity that is fine.
	if strings.Contains(msgUnknown, "reach the server") {
		t.Errorf("undetermined impact must not assert a cause, got: %s", msgUnknown)
	}
	if !strings.Contains(msgZero, "nothing loses access") {
		t.Errorf("a real zero should say so plainly, got: %s", msgZero)
	}
	if !strings.Contains(msgSome, "3") || !strings.Contains(msgSome, "Calendar Assistant") {
		t.Errorf("a non-zero impact must name the count and the agents, got: %s", msgSome)
	}
}

// An unreachable server must never look like a clean bill of health.
func TestDescribe_UnfetchedImpactDoesNotImplySafety(t *testing.T) {
	d := diffPluginManifests(manifestWithVerbs("list", "delete"), manifestWithVerbs("list"))
	msg := d.Describe("verge_labs/x")
	for _, forbidden := range []string{"nothing loses access", "no permission rules"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("unfetched impact must not read as safe (%q): %s", forbidden, msg)
		}
	}
}
