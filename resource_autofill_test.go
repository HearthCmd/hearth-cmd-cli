//go:build darwin || linux

package main

import "testing"

// mergeSecretBindings folds server autofill under explicit --secret: autofill
// fills unset names, an explicit binding wins on a collision, empties are dropped,
// and an all-empty result is nil (so resolveSecretBindings keeps its no-op path).
func TestMergeSecretBindings(t *testing.T) {
	if got := mergeSecretBindings(nil, nil); got != nil {
		t.Errorf("both empty should be nil, got %v", got)
	}

	// Autofill only — the common case (agent passes no --secret).
	got := mergeSecretBindings(map[string]string{"ha_token": "sec-1"}, nil)
	if len(got) != 1 || got["ha_token"] != "sec-1" {
		t.Errorf("autofill-only = %v, want {ha_token: sec-1}", got)
	}

	// Explicit --secret overrides autofill on a name collision; other autofilled
	// names survive.
	got = mergeSecretBindings(
		map[string]string{"ha_token": "sec-1", "other": "sec-2"},
		map[string]string{"ha_token": "override"},
	)
	if got["ha_token"] != "override" {
		t.Errorf("explicit should win on collision, got ha_token=%q", got["ha_token"])
	}
	if got["other"] != "sec-2" {
		t.Errorf("non-colliding autofill should survive, got other=%q", got["other"])
	}

	// Empty name or id on either side is dropped; an all-empty merge is nil.
	got = mergeSecretBindings(
		map[string]string{"a": "", "b": "1"},
		map[string]string{"": "x", "c": ""},
	)
	if len(got) != 1 || got["b"] != "1" {
		t.Errorf("empties should be dropped, got %v", got)
	}
	if got := mergeSecretBindings(map[string]string{"a": ""}, map[string]string{"": "x"}); got != nil {
		t.Errorf("all-empty merge should be nil, got %v", got)
	}
}
