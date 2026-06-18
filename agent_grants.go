package main

import "sync"

// AgentGrantsStore is the daemon's in-memory cache of the
// (agent_id → granted resource_connection_ids) relationship. Mirrors
// the agent_resource_grants_list view on the server, scoped to
// agents running on this host. Read-mostly with sync.RWMutex.
//
// Fed by fetchAgentResourceGrantsAtBoot. The server's
// agent_resource_grants_changed live-push triggers a refetch.
//
// Used by resource_prompt.go to filter the per-agent prompt: only
// connections an agent has been explicitly granted appear in its
// system prompt. Replaces the prior "all connections in every
// agent's prompt" behavior with explicit per-agent gating.
type AgentGrantsStore struct {
	mu       sync.RWMutex
	byAgent  map[string]map[string]struct{} // agent_id → set of connection_ids
}

func NewAgentGrantsStore() *AgentGrantsStore {
	return &AgentGrantsStore{
		byAgent: map[string]map[string]struct{}{},
	}
}

// swap atomically replaces the cache with the supplied
// (agent_id → connection_id) mapping. Called by the fetcher after a
// successful fetch.
func (s *AgentGrantsStore) swap(next map[string]map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byAgent = next
}

// HasGrant returns true if the agent has been granted access to the
// given connection. Empty agentID or connID returns false.
// Concurrency-safe.
func (s *AgentGrantsStore) HasGrant(agentID, connID string) bool {
	if s == nil || agentID == "" || connID == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	set, ok := s.byAgent[agentID]
	if !ok {
		return false
	}
	_, granted := set[connID]
	return granted
}

// GrantedConnectionIDs returns the set of connection ids the agent
// has been granted, in undefined order. Empty slice if the agent
// has zero grants. Concurrency-safe.
func (s *AgentGrantsStore) GrantedConnectionIDs(agentID string) []string {
	if s == nil || agentID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	set, ok := s.byAgent[agentID]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(set))
	for connID := range set {
		out = append(out, connID)
	}
	return out
}
