//go:build darwin || linux

package main

import "testing"

// The agent's system prompt labels each connection with its slug and
// falls back to the UUID when the slug is empty (resource_prompt.go).
// The agent then invokes exactly the identifier it was shown, with no
// way to tell which kind it got. Resolve has to accept both spellings
// or connections without a slug are silently unusable — the invoke
// comes back "unknown connection" for the very string we handed over.
func TestResolveAcceptsSlugAndUUID(t *testing.T) {
	store := NewResourceConnectionStore()
	store.swap(map[string]ResourceConnection{
		"conn-uuid-1": {ConnectionID: "conn-uuid-1", Slug: "my_calendar", PluginSlug: "verge_labs/google_calendar_oauth"},
		"conn-uuid-2": {ConnectionID: "conn-uuid-2", Slug: "", PluginSlug: "verge_labs/google_drive_oauth"},
	})

	tests := []struct {
		name   string
		ref    string
		wantID string
		wantOK bool
	}{
		{"by slug", "my_calendar", "conn-uuid-1", true},
		{"by uuid", "conn-uuid-1", "conn-uuid-1", true},
		{"slugless connection resolves by uuid", "conn-uuid-2", "conn-uuid-2", true},
		{"unknown ref", "nope", "", false},
		{"empty ref", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := store.Resolve(tt.ref)
			if ok != tt.wantOK {
				t.Fatalf("Resolve(%q) ok = %v, want %v", tt.ref, ok, tt.wantOK)
			}
			if got.ConnectionID != tt.wantID {
				t.Errorf("Resolve(%q) = %q, want %q", tt.ref, got.ConnectionID, tt.wantID)
			}
		})
	}
}

// A slug must win over a UUID that happens to equal it. Slugs are
// snake_case and UUIDs aren't, so this can't collide in production —
// the test pins the precedence anyway so a future slug format change
// can't silently reorder the lookup.
func TestResolvePrefersSlugOnCollision(t *testing.T) {
	store := NewResourceConnectionStore()
	store.swap(map[string]ResourceConnection{
		"ambiguous": {ConnectionID: "ambiguous", Slug: "other"},
		"real-uuid": {ConnectionID: "real-uuid", Slug: "ambiguous"},
	})

	got, ok := store.Resolve("ambiguous")
	if !ok {
		t.Fatal("Resolve(ambiguous) failed")
	}
	if got.ConnectionID != "real-uuid" {
		t.Errorf("Resolve(ambiguous) = %q, want real-uuid (slug should win)", got.ConnectionID)
	}
}
