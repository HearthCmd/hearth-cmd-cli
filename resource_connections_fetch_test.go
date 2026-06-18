//go:build darwin || linux

package main

import "testing"

func TestMergeServerConnections_FiltersOtherHosts(t *testing.T) {
	server := []serverResourceConnection{
		{ID: "mine", HostID: "this-host", PluginSlug: "echo"},
		{ID: "theirs", HostID: "other-host", PluginSlug: "echo"},
	}
	next, kept, skipped := mergeServerConnections(nil, server, "this-host")
	if kept != 1 || skipped != 1 {
		t.Errorf("kept=%d skipped=%d; want 1/1", kept, skipped)
	}
	if _, ok := next["theirs"]; ok {
		t.Errorf("other-host entry leaked into merge")
	}
	if _, ok := next["mine"]; !ok {
		t.Errorf("this-host entry missing from merge")
	}
}

func TestMergeServerConnections_EmptyServerResponseDropsAll(t *testing.T) {
	current := []ResourceConnection{
		{ConnectionID: "echo-test", PluginSlug: "echo"},
	}
	next, kept, _ := mergeServerConnections(current, nil, "this-host")
	if kept != 0 {
		t.Errorf("kept = %d; want 0", kept)
	}
	if len(next) != 0 {
		t.Errorf("expected empty map; got %+v", next)
	}
}
