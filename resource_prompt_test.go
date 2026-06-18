//go:build darwin || linux

package main

import (
	"strings"
	"testing"
)

// makeManifestRegistry builds a PluginRegistry containing the
// supplied manifests keyed by PluginSlug. Skips the disk-discovery
// path so tests can exercise the prompt builder against any verb
// shape.
func makeManifestRegistry(manifests ...PluginManifest) *PluginRegistry {
	r := NewPluginRegistry()
	r.byPluginSlug = map[string]PluginManifest{}
	for _, m := range manifests {
		r.byPluginSlug[m.PluginSlug] = m
		r.order = append(r.order, m.PluginSlug)
	}
	return r
}

// makeResourceConns builds an in-memory ResourceConnectionStore.
func makeResourceConns(conns ...ResourceConnection) *ResourceConnectionStore {
	s := NewResourceConnectionStore()
	for _, c := range conns {
		s.byConnID[c.ConnectionID] = c
	}
	return s
}

// makeGrantsFor builds an AgentGrantsStore granting `agentID` access
// to every supplied connection id. Mirrors what the server's
// agent_resource_grants_fetch would return for a single agent.
func makeGrantsFor(agentID string, connIDs ...string) *AgentGrantsStore {
	g := NewAgentGrantsStore()
	next := map[string]map[string]struct{}{}
	if len(connIDs) > 0 {
		set := map[string]struct{}{}
		for _, id := range connIDs {
			set[id] = struct{}{}
		}
		next[agentID] = set
	}
	g.swap(next)
	return g
}

const testAgentID = "agt-test"

func TestBuildResourcePluginPrompt_OmitsWhenEmpty(t *testing.T) {
	cases := []struct {
		name   string
		agent  string
		grants *AgentGrantsStore
		conns  *ResourceConnectionStore
		plugs  *PluginRegistry
	}{
		{"nil stores", testAgentID, nil, nil, nil},
		{"empty stores", testAgentID, makeGrantsFor(testAgentID), makeResourceConns(), makeManifestRegistry()},
		{"conns but no registered plugin", testAgentID,
			makeGrantsFor(testAgentID, "echo-test"),
			makeResourceConns(ResourceConnection{ConnectionID: "echo-test", PluginSlug: "echo"}),
			makeManifestRegistry() /* echo not registered */},
		{"no grants for this agent", testAgentID,
			makeGrantsFor("some-other-agent", "echo-test"),
			makeResourceConns(ResourceConnection{ConnectionID: "echo-test", PluginSlug: "echo"}),
			makeManifestRegistry(PluginManifest{PluginSlug: "echo", DisplayName: "Echo", Verbs: []PluginVerb{{Name: "v"}}})},
		{"empty agent id", "",
			makeGrantsFor(testAgentID, "echo-test"),
			makeResourceConns(ResourceConnection{ConnectionID: "echo-test", PluginSlug: "echo"}),
			makeManifestRegistry(PluginManifest{PluginSlug: "echo", DisplayName: "Echo", Verbs: []PluginVerb{{Name: "v"}}})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildResourcePluginPrompt(c.agent, c.grants, c.conns, c.plugs, nil); got != "" {
				t.Errorf("want empty, got:\n%s", got)
			}
		})
	}
}

func TestBuildResourcePluginPrompt_RendersVerbs(t *testing.T) {
	m := PluginManifest{
		PluginSlug:  "echo",
		DisplayName: "Echo Test Plugin",
		Verbs: []PluginVerb{
			{
				Name:        "echo",
				Description: "Round-trip args back as the stdout result.",
				Args:        map[string]PluginArgSpec{"payload": {Type: "string"}},
				Output:      "json",
			},
			{
				Name:        "fail",
				Description: "Return a structured error.",
				Output:      "json",
			},
		},
	}
	got := buildResourcePluginPrompt(
		testAgentID,
		makeGrantsFor(testAgentID, "echo-test"),
		makeResourceConns(ResourceConnection{ConnectionID: "echo-test", PluginSlug: "echo"}),
		makeManifestRegistry(m),
		nil,
	)

	assertContains := func(t *testing.T, hay, needle string) {
		t.Helper()
		if !strings.Contains(hay, needle) {
			t.Errorf("prompt missing %q\nfull:\n%s", needle, hay)
		}
	}
	assertContains(t, got, "hearth resource invoke")
	assertContains(t, got, "echo-test (Echo Test Plugin)")
	assertContains(t, got, "echo — Round-trip args back")
	assertContains(t, got, "fail — Return a structured error")
	assertContains(t, got, `"payload": "string"`)
	assertContains(t, got, "output: json")
}

func TestBuildResourcePluginPrompt_RequiredArgsMarked(t *testing.T) {
	m := PluginManifest{
		PluginSlug:  "ha",
		DisplayName: "Home Assistant",
		Verbs: []PluginVerb{
			{
				Name:        "turn_on",
				Description: "Switch an entity on.",
				Args: map[string]PluginArgSpec{
					"entity_id": {Type: "string", Required: true},
				},
				Output: "json",
			},
		},
	}
	got := buildResourcePluginPrompt(
		testAgentID,
		makeGrantsFor(testAgentID, "ha-home"),
		makeResourceConns(ResourceConnection{ConnectionID: "ha-home", PluginSlug: "ha"}),
		makeManifestRegistry(m),
		nil,
	)
	if !strings.Contains(got, `"entity_id": "string (required)"`) {
		t.Errorf("required marker missing in prompt:\n%s", got)
	}
}

func TestBuildResourcePluginPrompt_StableOrdering(t *testing.T) {
	m := PluginManifest{
		PluginSlug:  "echo",
		DisplayName: "Echo",
		Verbs: []PluginVerb{
			{Name: "zeta", Description: "z"},
			{Name: "alpha", Description: "a"},
			{Name: "mu", Description: "m"},
		},
	}
	got := buildResourcePluginPrompt(
		testAgentID,
		makeGrantsFor(testAgentID, "z-echo", "a-echo"),
		makeResourceConns(
			ResourceConnection{ConnectionID: "z-echo", PluginSlug: "echo"},
			ResourceConnection{ConnectionID: "a-echo", PluginSlug: "echo"},
		),
		makeManifestRegistry(m),
		nil,
	)
	if strings.Index(got, "a-echo") > strings.Index(got, "z-echo") {
		t.Error("connections should sort by id")
	}
	a := strings.Index(got, "alpha")
	mIdx := strings.Index(got, "mu")
	z := strings.Index(got, "zeta")
	if !(a < mIdx && mIdx < z) {
		t.Errorf("verbs should sort alphabetically: alpha=%d mu=%d zeta=%d", a, mIdx, z)
	}
}

func TestBuildResourcePluginPrompt_MultilineDescriptionCollapsed(t *testing.T) {
	m := PluginManifest{
		PluginSlug:  "echo",
		DisplayName: "Echo",
		Verbs: []PluginVerb{
			{Name: "echo", Description: "First line.\nSecond line ignored."},
		},
	}
	got := buildResourcePluginPrompt(
		testAgentID,
		makeGrantsFor(testAgentID, "echo-test"),
		makeResourceConns(ResourceConnection{ConnectionID: "echo-test", PluginSlug: "echo"}),
		makeManifestRegistry(m),
		nil,
	)
	if strings.Contains(got, "Second line") {
		t.Errorf("multiline description should be first-line only; got:\n%s", got)
	}
}

// TestBuildResourcePluginPrompt_FiltersByGrants is the core
// phase-4 invariant: a connection in the store but NOT in the
// agent's grants must not appear in the rendered prompt.
func TestBuildResourcePluginPrompt_FiltersByGrants(t *testing.T) {
	m := PluginManifest{
		PluginSlug:  "echo",
		DisplayName: "Echo",
		Verbs:       []PluginVerb{{Name: "echo", Description: "x"}},
	}
	// Two connections in the store; only one granted to this agent.
	got := buildResourcePluginPrompt(
		testAgentID,
		makeGrantsFor(testAgentID, "granted"),
		makeResourceConns(
			ResourceConnection{ConnectionID: "granted", PluginSlug: "echo"},
			ResourceConnection{ConnectionID: "not-granted", PluginSlug: "echo"},
		),
		makeManifestRegistry(m),
		nil,
	)
	if !strings.Contains(got, "granted (") {
		t.Errorf("granted connection should appear; got:\n%s", got)
	}
	if strings.Contains(got, "not-granted") {
		t.Errorf("non-granted connection must not appear; got:\n%s", got)
	}
}

func TestBuildResourcePluginPrompt_RendersEntitiesAndCredentials(t *testing.T) {
	m := PluginManifest{
		PluginSlug:  "ha",
		DisplayName: "Home Assistant",
		Credentials: []PluginCredential{
			{Name: "ha_token", Description: "Long-lived access token.", Secret: true},
			{Name: "ha_url", Description: "Base URL of HA.", Secret: false},
		},
		Verbs: []PluginVerb{
			{Name: "get_state", Description: "Read state."},
		},
	}
	entities := map[string][]Entity{
		"ha-home": {
			{EntityID: "light.kitchen", Kind: "ha.light", Labels: map[string]string{"area": "kitchen"}},
			{EntityID: "lock.front_door", Kind: "ha.lock", Parent: "area:entry"},
		},
	}
	got := buildResourcePluginPrompt(
		testAgentID,
		makeGrantsFor(testAgentID, "ha-home"),
		makeResourceConns(ResourceConnection{ConnectionID: "ha-home", PluginSlug: "ha"}),
		makeManifestRegistry(m),
		entities,
	)
	for _, needle := range []string{
		"Credentials:",
		"ha_token (secret) — Long-lived access token.",
		"ha_url — Base URL of HA.",
		"Entities:",
		"light.kitchen  (kind=ha.light, area=kitchen)",
		"lock.front_door  (kind=ha.lock, parent=area:entry)",
		"--secret <env>=<id>",
		"hearth secret list",
	} {
		if !strings.Contains(got, needle) {
			t.Errorf("prompt missing %q\nfull:\n%s", needle, got)
		}
	}
}

func TestBuildResourcePluginPrompt_EntityCap(t *testing.T) {
	m := PluginManifest{
		PluginSlug:  "ha",
		DisplayName: "HA",
		Verbs:       []PluginVerb{{Name: "noop"}},
	}
	// Build maxEntitiesPerConn + 3 so the overflow line fires.
	ents := make([]Entity, maxEntitiesPerConn+3)
	for i := range ents {
		ents[i] = Entity{
			EntityID: "light.ent" + string(rune('A'+i%26)) + string(rune('A'+(i/26)%26)),
			Kind:     "ha.light",
		}
	}
	got := buildResourcePluginPrompt(
		testAgentID,
		makeGrantsFor(testAgentID, "ha-home"),
		makeResourceConns(ResourceConnection{ConnectionID: "ha-home", PluginSlug: "ha"}),
		makeManifestRegistry(m),
		map[string][]Entity{"ha-home": ents},
	)
	if !strings.Contains(got, "...and 3 more") {
		t.Errorf("expected overflow line for %d entities; got:\n%s", len(ents), got)
	}
}

func TestBuildResourcePluginPrompt_OmitsEntitiesWhenNone(t *testing.T) {
	m := PluginManifest{
		PluginSlug:  "ha",
		DisplayName: "HA",
		Verbs:       []PluginVerb{{Name: "noop"}},
	}
	got := buildResourcePluginPrompt(
		testAgentID,
		makeGrantsFor(testAgentID, "ha-home"),
		makeResourceConns(ResourceConnection{ConnectionID: "ha-home", PluginSlug: "ha"}),
		makeManifestRegistry(m),
		nil, // no entities map
	)
	if strings.Contains(got, "Entities:") {
		t.Errorf("Entities section should be omitted when no entities; got:\n%s", got)
	}
}
