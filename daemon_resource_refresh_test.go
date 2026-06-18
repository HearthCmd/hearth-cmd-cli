//go:build darwin || linux

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDaemonDB_ReplaceEntitiesRoundTrip(t *testing.T) {
	db := openTestDaemonDB(t)
	in := []Entity{
		{EntityID: "light.kitchen", Kind: "ha.light", Labels: map[string]string{"area": "kitchen"}},
		{EntityID: "lock.front_door", Kind: "ha.lock", Parent: "area:entry"},
	}
	if err := db.ReplaceEntities("ha-home", in); err != nil {
		t.Fatalf("ReplaceEntities: %v", err)
	}
	got, err := db.ListEntities("ha-home")
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d; want 2 (got %+v)", len(got), got)
	}
	if got[0].EntityID != "light.kitchen" || got[0].Kind != "ha.light" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[0].Labels["area"] != "kitchen" {
		t.Errorf("labels lost: %+v", got[0].Labels)
	}
	if got[1].EntityID != "lock.front_door" || got[1].Parent != "area:entry" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestDaemonDB_ReplaceEntitiesOverwritesPrior(t *testing.T) {
	db := openTestDaemonDB(t)
	first := []Entity{{EntityID: "light.kitchen", Kind: "ha.light"}}
	if err := db.ReplaceEntities("ha-home", first); err != nil {
		t.Fatalf("first: %v", err)
	}
	second := []Entity{{EntityID: "lock.front_door", Kind: "ha.lock"}}
	if err := db.ReplaceEntities("ha-home", second); err != nil {
		t.Fatalf("second: %v", err)
	}
	got, _ := db.ListEntities("ha-home")
	if len(got) != 1 || got[0].EntityID != "lock.front_door" {
		t.Errorf("Replace should atomically swap; got %+v", got)
	}
}

func TestDaemonDB_ReplaceEntitiesScopedByConnection(t *testing.T) {
	db := openTestDaemonDB(t)
	_ = db.ReplaceEntities("ha-home", []Entity{{EntityID: "light.a", Kind: "ha.light"}})
	_ = db.ReplaceEntities("ha-vacation", []Entity{{EntityID: "light.b", Kind: "ha.light"}})
	home, _ := db.ListEntities("ha-home")
	vac, _ := db.ListEntities("ha-vacation")
	if len(home) != 1 || home[0].EntityID != "light.a" {
		t.Errorf("ha-home leaked: %+v", home)
	}
	if len(vac) != 1 || vac[0].EntityID != "light.b" {
		t.Errorf("ha-vacation leaked: %+v", vac)
	}
}

// newDeclarativeDaemonWithDB constructs a daemon plumbed for the
// refresh path: declarative manifest + connection store + executor +
// daemon DB. fakeAuthzWS isn't needed (refresh isn't IAM-gated today).
func newDeclarativeDaemonWithDB(t *testing.T, manifest PluginManifest, conns map[string]ResourceConnection) *Daemon {
	t.Helper()
	manifest.Source = ClassifyManifestSource(manifest)
	reg := NewPluginRegistry()
	reg.byPluginSlug = map[string]PluginManifest{manifest.PluginSlug: manifest}
	reg.order = []string{manifest.PluginSlug}

	normalised := make(map[string]ResourceConnection, len(conns))
	for k, rc := range conns {
		if rc.Slug == "" {
			rc.Slug = k
		}
		normalised[k] = rc
	}
	store := NewResourceConnectionStore()
	store.swap(normalised)

	return &Daemon{
		plugins:             reg,
		resourceConnections: store,
		declarativeExecutor: NewDeclarativeExecutor(),
		localDB:             openTestDaemonDB(t),
		humanUserID:         "test-user",
		resourceAuthzWS:     allowAuthzWS(),
	}
}

func haSnapshotOnlyManifest() PluginManifest {
	m := haReadOnlyManifest()
	// Strip Authorization header off the verb http so we don't need
	// credential plumbing in tests that don't exercise it.
	m.Verbs[0].HTTP.Headers = nil
	m.Snapshot = &SnapshotSpec{
		HTTP: VerbHTTPSpec{
			Method: "GET",
			URL:    "{{config.ha_url}}/api/states",
		},
		Extract: ExtractSpec{
			Iterate:  "$[*]",
			EntityID: "$.entity_id",
			Kind:     ExtractKindSpec{PrefixWithEntityDomain: "ha."},
			Labels:   map[string]string{"area": "$.attributes.area_id"},
		},
	}
	return m
}

func TestHandleResourceRefresh_HAShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/states" {
			t.Errorf("snapshot hit wrong path: %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `[
			{"entity_id":"light.kitchen","attributes":{"area_id":"kitchen"}},
			{"entity_id":"lock.front_door","attributes":{}}
		]`)
	}))
	defer srv.Close()

	d := newDeclarativeDaemonWithDB(t, haSnapshotOnlyManifest(), map[string]ResourceConnection{
		"ha-home": {
			ConnectionID: "ha-home",
			PluginSlug:   "ha",
			Config:       `{"ha_url":"` + srv.URL + `"}`,
		},
	})

	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_refresh",
		ResourceConnectionID: "ha-home",
	})

	if resp.Type != "resource_refresh_response" {
		t.Fatalf("type = %q msg=%q errCode=%q", resp.Type, resp.Message, resp.ResourceErrCode)
	}
	if resp.EntityCount != 2 {
		t.Errorf("entity_count = %d; want 2", resp.EntityCount)
	}

	// Persistence: ListEntities sees what RunSnapshot wrote.
	ents, err := d.localDB.ListEntities("ha-home")
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if len(ents) != 2 {
		t.Fatalf("persisted len = %d", len(ents))
	}
	if ents[0].EntityID != "light.kitchen" || ents[0].Kind != "ha.light" {
		t.Errorf("ents[0] = %+v", ents[0])
	}
	if ents[0].Labels["area"] != "kitchen" {
		t.Errorf("labels[area] = %q", ents[0].Labels["area"])
	}
}

func TestHandleResourceRefresh_UnknownConnection(t *testing.T) {
	d := newDeclarativeDaemonWithDB(t, haSnapshotOnlyManifest(), nil)
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_refresh",
		ResourceConnectionID: "ghost",
	})
	if resp.Type != "error" || resp.ResourceErrCode != string(ErrBadArgs) {
		t.Errorf("type=%q code=%q msg=%q", resp.Type, resp.ResourceErrCode, resp.Message)
	}
}

func TestHandleResourceRefresh_BinaryAdapterRefused(t *testing.T) {
	binManifest := PluginManifest{
		PluginSlug:     "echo",
		DisplayName:    "Echo",
		ManifestSchema: 1,
		Executable:     "./hearth-plugin-echo",
		Source:         SourceBinary,
	}
	d := newDeclarativeDaemonWithDB(t, binManifest, map[string]ResourceConnection{
		"echo-1": {ConnectionID: "echo-1", PluginSlug: "echo"},
	})
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_refresh",
		ResourceConnectionID: "echo-1",
	})
	if resp.Type != "error" {
		t.Fatalf("expected error; got %q", resp.Type)
	}
	if resp.ResourceErrCode != string(ErrBadArgs) {
		t.Errorf("code = %q", resp.ResourceErrCode)
	}
}

func TestHandleResourceRefresh_NoSnapshotBlockRefused(t *testing.T) {
	m := haReadOnlyManifest()
	m.Verbs[0].HTTP.Headers = nil
	// Snapshot intentionally absent.
	d := newDeclarativeDaemonWithDB(t, m, map[string]ResourceConnection{
		"ha-home": {ConnectionID: "ha-home", PluginSlug: "ha"},
	})
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_refresh",
		ResourceConnectionID: "ha-home",
	})
	if resp.Type != "error" || resp.ResourceErrCode != string(ErrBadArgs) {
		t.Errorf("type=%q code=%q msg=%q", resp.Type, resp.ResourceErrCode, resp.Message)
	}
}

// TestHandleResourceInvoke_AutoRefreshOnFirstInvoke confirms that an
// invoke against a declarative connection with an empty entity cache
// runs the manifest's snapshot inline. Cache must be populated AFTER
// the invoke, even when the verb itself succeeded.
func TestHandleResourceInvoke_AutoRefreshOnFirstInvoke(t *testing.T) {
	var snapshotHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/states" {
			snapshotHits++
			_, _ = io.WriteString(w, `[
				{"entity_id":"light.kitchen","attributes":{"area_id":"kitchen"}},
				{"entity_id":"switch.lamp","attributes":{}}
			]`)
			return
		}
		// Verb path /api/states/{entity_id} — get_state's output is
		// $.state, so return a state-shaped object.
		_, _ = io.WriteString(w, `{"state":"on"}`)
	}))
	defer srv.Close()

	// Manifest carries both a list_entities verb (whose path matches the
	// snapshot URL) and a snapshot block. Cache starts empty.
	d := newDeclarativeDaemonWithDB(t, haSnapshotOnlyManifest(), map[string]ResourceConnection{
		"ha-home": {
			ConnectionID: "ha-home",
			PluginSlug:   "ha",
			Config:       `{"ha_url":"` + srv.URL + `"}`,
		},
	})

	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "ha-home",
		ResourceVerb:         "get_state",
		ResourceArgs:         json.RawMessage(`{"entity_id":"light.kitchen"}`),
	})
	if resp.Type != "resource_invoke_response" {
		t.Fatalf("invoke failed: type=%q msg=%q code=%q", resp.Type, resp.Message, resp.ResourceErrCode)
	}
	// Auto-refresh should have hit /api/states once for the snapshot.
	if snapshotHits != 1 {
		t.Errorf("expected 1 snapshot hit on first invoke; got %d", snapshotHits)
	}
	// Cache should now contain the two entities.
	ents, _ := d.localDB.ListEntities("ha-home")
	if len(ents) != 2 {
		t.Errorf("expected cache populated post-invoke; got %d entities", len(ents))
	}
}

// TestHandleResourceInvoke_NoAutoRefreshWhenCachePopulated guards against
// the executor re-hitting the snapshot endpoint on every invoke; auto-
// refresh fires only when the cache is empty.
func TestHandleResourceInvoke_NoAutoRefreshWhenCachePopulated(t *testing.T) {
	var snapshotHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/states" {
			snapshotHits++
			_, _ = io.WriteString(w, `[]`)
			return
		}
		// Verb path returns a state-shaped object matching get_state's
		// `output: $.state` extractor.
		_, _ = io.WriteString(w, `{"state":"on"}`)
	}))
	defer srv.Close()

	d := newDeclarativeDaemonWithDB(t, haSnapshotOnlyManifest(), map[string]ResourceConnection{
		"ha-home": {
			ConnectionID: "ha-home",
			PluginSlug:   "ha",
			Config:       `{"ha_url":"` + srv.URL + `"}`,
		},
	})
	// Pre-seed the cache so auto-refresh shouldn't fire.
	if err := d.localDB.ReplaceEntities("ha-home", []Entity{
		{EntityID: "light.kitchen", Kind: "ha.light"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "ha-home",
		ResourceVerb:         "get_state",
		ResourceArgs:         json.RawMessage(`{"entity_id":"light.kitchen"}`),
	})
	if resp.Type != "resource_invoke_response" {
		t.Fatalf("invoke failed: %q", resp.Message)
	}
	// Verb hits /api/states/light.kitchen, NOT /api/states (snapshot
	// path). Only the snapshot path bumps snapshotHits.
	if snapshotHits != 0 {
		t.Errorf("auto-refresh should NOT have fired when cache was populated; got %d snapshot hits", snapshotHits)
	}
}

// TestHandleResourceInvoke_AutoRefreshSnapshotFailureDoesNotBlockVerb
// asserts that snapshot failures (e.g. upstream 5xx during the
// snapshot HTTP) are best-effort — the verb itself still runs against
// the same upstream when its endpoint works.
func TestHandleResourceInvoke_AutoRefreshSnapshotFailureDoesNotBlockVerb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/states" {
			// Snapshot URL — fail. The verb URL is /api/states/{entity_id}
			// which DOES include a suffix, so the path equality matters.
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, `{"state":"on"}`)
	}))
	defer srv.Close()

	d := newDeclarativeDaemonWithDB(t, haSnapshotOnlyManifest(), map[string]ResourceConnection{
		"ha-home": {
			ConnectionID: "ha-home",
			PluginSlug:   "ha",
			Config:       `{"ha_url":"` + srv.URL + `"}`,
		},
	})
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "ha-home",
		ResourceVerb:         "get_state",
		ResourceArgs:         json.RawMessage(`{"entity_id":"light.kitchen"}`),
	})
	if resp.Type != "resource_invoke_response" {
		t.Errorf("verb should succeed even though auto-refresh snapshot failed; got type=%q msg=%q",
			resp.Type, resp.Message)
	}
	// Cache should remain empty since the snapshot failed.
	ents, _ := d.localDB.ListEntities("ha-home")
	if len(ents) != 0 {
		t.Errorf("snapshot failed; cache should remain empty; got %d entities", len(ents))
	}
}

func TestDaemonDB_LatestEntityFetchedAt(t *testing.T) {
	db := openTestDaemonDB(t)
	// Empty cache → (false, zero).
	found, _, err := db.LatestEntityFetchedAt("ha-home")
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if found {
		t.Errorf("expected not-found on empty cache")
	}

	if err := db.ReplaceEntities("ha-home", []Entity{
		{EntityID: "light.kitchen", Kind: "ha.light"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	found, _, err = db.LatestEntityFetchedAt("ha-home")
	if err != nil {
		t.Fatalf("post-seed: %v", err)
	}
	if !found {
		t.Errorf("expected found after seed")
	}
}

func TestResolveEntityForAuthorize_HitsCache(t *testing.T) {
	d := &Daemon{localDB: openTestDaemonDB(t)}
	if err := d.localDB.ReplaceEntities("ha-home", []Entity{
		{EntityID: "light.kitchen", Kind: "ha.light", Labels: map[string]string{"area": "kitchen"}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	kind, labels, parent := d.resolveEntityForAuthorize("ha-home", []byte(`{"entity_id":"light.kitchen"}`))
	if kind != "ha.light" {
		t.Errorf("kind = %q; want ha.light", kind)
	}
	if labels["area"] != "kitchen" {
		t.Errorf("labels = %+v", labels)
	}
	_ = parent
}

func TestResolveEntityForAuthorize_MissesAreEmpty(t *testing.T) {
	d := &Daemon{localDB: openTestDaemonDB(t)}
	cases := []struct {
		name string
		args []byte
	}{
		{"no args", nil},
		{"empty json", []byte(`{}`)},
		{"non-object args", []byte(`["a","b"]`)},
		{"unknown entity_id", []byte(`{"entity_id":"ghost"}`)},
		{"entity_id not a string", []byte(`{"entity_id":42}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, _, _ := d.resolveEntityForAuthorize("ha-home", c.args)
			if kind != "" {
				t.Errorf("kind = %q; want empty", kind)
			}
		})
	}
}

func TestResolveEntityForAuthorize_NilDBSafely(t *testing.T) {
	d := &Daemon{} // no localDB
	kind, _, _ := d.resolveEntityForAuthorize("ha-home", []byte(`{"entity_id":"light.kitchen"}`))
	if kind != "" {
		t.Errorf("nil-DB lookup should return empty kind; got %q", kind)
	}
}

func TestHandleResourceRefresh_UpstreamErrorPersistsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	d := newDeclarativeDaemonWithDB(t, haSnapshotOnlyManifest(), map[string]ResourceConnection{
		"ha-home": {
			ConnectionID: "ha-home",
			PluginSlug:   "ha",
			Config:       `{"ha_url":"` + srv.URL + `"}`,
		},
	})
	// Pre-seed entities so we can verify the failed refresh doesn't
	// nuke the prior good snapshot.
	if err := d.localDB.ReplaceEntities("ha-home", []Entity{{EntityID: "stale", Kind: "ha.light"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_refresh",
		ResourceConnectionID: "ha-home",
	})
	if resp.Type != "error" || resp.ResourceErrCode != string(ErrUnavailable) {
		t.Errorf("type=%q code=%q", resp.Type, resp.ResourceErrCode)
	}
	ents, _ := d.localDB.ListEntities("ha-home")
	if len(ents) != 1 || ents[0].EntityID != "stale" {
		t.Errorf("failed refresh should leave prior entities intact; got %+v", ents)
	}
}

