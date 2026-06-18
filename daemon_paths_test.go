//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Coverage for the small per-process state on Daemon (SetAccount /
// SetOrganizations) and the path helpers daemonSockPath / daemonPidPath.
// Daemon spawn / IPC is intentionally out of scope here — those need
// the full socket dance and are exercised by the integration tests.

// ---------- daemonSockPath ----------

func TestDaemonSockPath_DefaultIsPerUID(t *testing.T) {
	t.Setenv("HEARTH_DAEMON_SOCK", "")
	want := fmt.Sprintf("/tmp/hearth-daemon-%d.sock", os.Getuid())
	if got := daemonSockPath(); got != want {
		t.Errorf("default sock path got %q, want %q", got, want)
	}
}

func TestDaemonSockPath_HoldsToEnvOverride(t *testing.T) {
	t.Setenv("HEARTH_DAEMON_SOCK", "/custom/path.sock")
	if got := daemonSockPath(); got != "/custom/path.sock" {
		t.Errorf("got %q", got)
	}
}

// ---------- daemonPidPath ----------

func TestDaemonPidPath_UnderHomeDotHearth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := daemonPidPath()
	want := filepath.Join(home, ".hearth", "daemon.pid")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---------- Daemon.SetAccount / SetOrganizations ----------

func TestDaemon_SetAccount_PopulatesFields(t *testing.T) {
	d := &Daemon{}
	d.SetAccount("user-1", "alice@example.com")
	d.identityMu.RLock()
	defer d.identityMu.RUnlock()
	if d.humanUserID != "user-1" {
		t.Errorf("humanUserID = %q", d.humanUserID)
	}
	if d.email != "alice@example.com" {
		t.Errorf("email = %q", d.email)
	}
}

func TestDaemon_SetAccount_EmptyHumanUserIDPreservesPrevious(t *testing.T) {
	// Documented contract: SetAccount only overwrites humanUserID when
	// the new value is non-empty (server can re-push email-only updates
	// without clobbering identity).
	d := &Daemon{humanUserID: "existing", email: "old@x"}
	d.SetAccount("", "new@x")
	d.identityMu.RLock()
	defer d.identityMu.RUnlock()
	if d.humanUserID != "existing" {
		t.Errorf("humanUserID should be preserved, got %q", d.humanUserID)
	}
	if d.email != "new@x" {
		t.Errorf("email should be updated, got %q", d.email)
	}
}

func TestDaemon_SetOrganizations_ReplacesList(t *testing.T) {
	d := &Daemon{orgs: []daemonOrgEntry{{ID: "old"}}}
	d.SetOrganizations([]daemonOrgEntry{
		{ID: "a", Name: "Alpha"},
		{ID: "b", Name: "Beta", IsCurrent: true},
	})
	d.identityMu.RLock()
	defer d.identityMu.RUnlock()
	if len(d.orgs) != 2 {
		t.Fatalf("len = %d", len(d.orgs))
	}
	if d.orgs[1].ID != "b" || !d.orgs[1].IsCurrent {
		t.Errorf("second entry: %+v", d.orgs[1])
	}
}

func TestDaemon_SetOrganizations_EmptyClears(t *testing.T) {
	d := &Daemon{orgs: []daemonOrgEntry{{ID: "old"}, {ID: "older"}}}
	d.SetOrganizations(nil)
	d.identityMu.RLock()
	defer d.identityMu.RUnlock()
	if len(d.orgs) != 0 {
		t.Errorf("expected empty, got %v", d.orgs)
	}
}
