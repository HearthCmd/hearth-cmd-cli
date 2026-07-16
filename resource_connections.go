package main

import "sync"

// ResourceConnection is one entry in the daemon's in-memory cache
// of the server's resource_connections table. Fed exclusively by
// fetchResourceConnectionsAtBoot — the server is SOT for which
// connections exist. Credentials live in the secrets vault, fetched
// at plugin Init by the resolver.
type ResourceConnection struct {
	// ConnectionID is the server-assigned UUID (resource_connections.id).
	// Used for server API calls and local entity DB keys.
	ConnectionID string
	// Slug is the snake_case human name (resource_connections.slug).
	// Used in CLI commands: `hearth resource invoke <slug> <verb>`.
	Slug       string
	PluginSlug string
	// HostID is the server-pinned host for this connection. Always
	// set on entries that come from the server fetch (which is the
	// only source after 2b).
	HostID string
	// Config is the non-sensitive per-connection config blob the
	// server stores on resource_connections.config. JSON object
	// string; empty == "{}". The declarative executor's template
	// scope sources `{{config.x}}` placeholders from this. Binary
	// plugins don't see it today (they read credentials at invoke
	// time instead).
	Config string
}

// ResourceConnectionStore is the in-memory set of Resource Connections
// the daemon is aware of. Fed by fetchResourceConnectionsAtBoot;
// the only mutator is swap(), called at boot + reconnect + live-push.
// Read-mostly with sync.RWMutex.
type ResourceConnectionStore struct {
	mu       sync.RWMutex
	byConnID map[string]ResourceConnection // keyed by UUID
	bySlug   map[string]string             // slug → UUID
}

func NewResourceConnectionStore() *ResourceConnectionStore {
	return &ResourceConnectionStore{
		byConnID: map[string]ResourceConnection{},
		bySlug:   map[string]string{},
	}
}

func (s *ResourceConnectionStore) swap(next map[string]ResourceConnection) {
	slugIndex := map[string]string{}
	for uuid, rc := range next {
		if rc.Slug != "" {
			slugIndex[rc.Slug] = uuid
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byConnID = next
	s.bySlug = slugIndex
}

// Get returns the ResourceConnection for the given UUID, if registered.
// Concurrency-safe.
func (s *ResourceConnectionStore) Get(connID string) (ResourceConnection, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.byConnID[connID]
	return c, ok
}

// GetBySlug returns the ResourceConnection for the given slug, if
// registered. CLI commands pass slugs; this resolves to the UUID-keyed
// entry. Concurrency-safe.
func (s *ResourceConnectionStore) GetBySlug(slug string) (ResourceConnection, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	uuid, ok := s.bySlug[slug]
	if !ok {
		return ResourceConnection{}, false
	}
	c, ok := s.byConnID[uuid]
	return c, ok
}

// Resolve looks a connection up by slug first, then by UUID. This is
// the entry point for anything a human or an agent typed.
//
// Slug wins on a collision. Slugs are snake_case and UUIDs are not, so
// the two namespaces can't actually overlap; the ordering just makes
// the common case one map hit.
//
// Both spellings have to work. The agent's system prompt labels each
// connection with its slug but falls back to the UUID when the slug is
// empty (resource_prompt.go), so an agent can be handed either one and
// has no way to tell which. A slug-only lookup made every connection
// without a slug silently unusable — the agent would invoke exactly the
// identifier it was shown and get "unknown connection".
func (s *ResourceConnectionStore) Resolve(ref string) (ResourceConnection, bool) {
	if ref == "" {
		return ResourceConnection{}, false
	}
	if c, ok := s.GetBySlug(ref); ok {
		return c, true
	}
	return s.Get(ref)
}

// List returns all registered ResourceConnections. Order is unspecified;
// callers that need determinism should sort. Concurrency-safe.
func (s *ResourceConnectionStore) List() []ResourceConnection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ResourceConnection, 0, len(s.byConnID))
	for _, c := range s.byConnID {
		out = append(out, c)
	}
	return out
}
