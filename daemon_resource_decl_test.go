//go:build darwin || linux

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newDeclarativeDaemon spins up a *Daemon whose router will pick the
// declarative branch for the given manifest. No subprocess supervisor —
// the declarative path doesn't need one. authzWS defaults to
// allow-everything; tests that care override.
func newDeclarativeDaemon(t *testing.T, manifest PluginManifest, conns map[string]ResourceConnection) *Daemon {
	t.Helper()
	manifest.Source = ClassifyManifestSource(manifest)
	if manifest.Source != SourceDeclarative {
		t.Fatalf("test setup error: manifest must classify as declarative; got %q", manifest.Source)
	}
	reg := NewPluginRegistry()
	reg.byPluginSlug = map[string]PluginManifest{manifest.PluginSlug: manifest}
	reg.order = []string{manifest.PluginSlug}

	// Ensure Slug is populated so GetBySlug works in tests that set
	// only ConnectionID. In production, Slug comes from the server;
	// tests use the map key as a stand-in for both UUID and slug.
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
		humanUserID:         "test-user",
		resourceAuthzWS:     allowAuthzWS(),
	}
}

func haReadOnlyManifest() PluginManifest {
	return PluginManifest{
		PluginSlug:     "ha",
		DisplayName:    "Home Assistant",
		ManifestSchema: 2,
		Verbs: []PluginVerb{
			{
				Name: "get_state",
				HTTP: &VerbHTTPSpec{
					Method: "GET",
					URL:    "{{config.ha_url}}/api/states/{{args.entity_id}}",
					Headers: map[string]string{
						"Authorization": "Bearer {{credentials.ha_token}}",
					},
					Response: VerbHTTPResponse{Output: "$.state"},
				},
			},
		},
	}
}

func TestHandleResourceInvoke_DeclarativeHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/states/light.kitchen" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"state":"on","attributes":{"brightness":255}}`)
	}))
	defer srv.Close()

	// Trim the Authorization header — credential plumbing
	// (resolveSecretBindings → executor credentials map) has its own
	// test below. Here we just want the router → executor wiring
	// asserted end-to-end on the happy path.
	m := haReadOnlyManifest()
	m.Verbs[0].HTTP.Headers = nil

	d := newDeclarativeDaemon(t, m, map[string]ResourceConnection{
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
		t.Fatalf("type = %q msg=%q errCode=%q", resp.Type, resp.Message, resp.ResourceErrCode)
	}
	if resp.ResourceStdout != "on" {
		t.Errorf("stdout = %q; want \"on\"", resp.ResourceStdout)
	}
}

// TestHandleResourceInvoke_DeclarativeMissingCredentialFailsBadArgs
// pins the "template substitution refuses unknown keys" contract at
// the daemon layer — if a manifest references {{credentials.X}} and
// nothing in the resolved SecretBindings provides X, the call fails
// before any HTTP request fires.
func TestHandleResourceInvoke_DeclarativeMissingCredentialFailsBadArgs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("upstream must not be hit when credentials are missing")
	}))
	defer srv.Close()

	d := newDeclarativeDaemon(t, haReadOnlyManifest(), map[string]ResourceConnection{
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
		// No SecretBindings — credentials map will be empty.
	})

	if resp.Type != "error" {
		t.Fatalf("expected error; got %q", resp.Type)
	}
	if resp.ResourceErrCode != string(ErrBadArgs) {
		t.Errorf("err code = %q; want %q", resp.ResourceErrCode, ErrBadArgs)
	}
}

func TestHandleResourceInvoke_DeclarativeUpstream500MapsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	// Trim the Authorization header so this test doesn't depend on
	// credential plumbing — different concern from status mapping.
	m := haReadOnlyManifest()
	m.Verbs[0].HTTP.Headers = nil

	d := newDeclarativeDaemon(t, m, map[string]ResourceConnection{
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

	if resp.Type != "error" {
		t.Fatalf("expected error; got %q", resp.Type)
	}
	if resp.ResourceErrCode != string(ErrUnavailable) {
		t.Errorf("err code = %q; want %q", resp.ResourceErrCode, ErrUnavailable)
	}
}

func TestHandleResourceInvoke_DeclarativeUnknownVerb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("upstream must not be hit when verb is unknown")
	}))
	defer srv.Close()

	m := haReadOnlyManifest()
	m.Verbs[0].HTTP.Headers = nil

	d := newDeclarativeDaemon(t, m, map[string]ResourceConnection{
		"ha-home": {
			ConnectionID: "ha-home",
			PluginSlug:   "ha",
			Config:       `{"ha_url":"` + srv.URL + `"}`,
		},
	})

	resp := ipcRoundTrip(t, d, ipcRequest{
		Type:                 "resource_invoke",
		ResourceConnectionID: "ha-home",
		ResourceVerb:         "does_not_exist",
		ResourceArgs:         json.RawMessage(`{}`),
	})

	if resp.Type != "error" {
		t.Fatalf("expected error; got %q", resp.Type)
	}
	if resp.ResourceErrCode != string(ErrBadArgs) {
		t.Errorf("err code = %q; want %q", resp.ResourceErrCode, ErrBadArgs)
	}
}
