//go:build darwin || linux

package main

// Coverage for config.go: ~/.hearth/credentials read/write round-trips,
// upsert preserving siblings, comment + blank line tolerance, and the
// 0600 perms guarantee on the credentials file (it holds bearer
// secrets — must not be world-readable on multi-user boxes).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- helpers ----------

// withFakeHome points HOME at a fresh temp dir so config_test doesn't
// touch the real ~/.hearth.
func withFakeHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

// ---------- configPath ----------

func TestConfigPath_UnderHomeDotHearth(t *testing.T) {
	dir := withFakeHome(t)
	got, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, ".hearth", "credentials")
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// ---------- readConfigValue ----------

func TestReadConfigValue_MissingFileReturnsEmpty(t *testing.T) {
	withFakeHome(t)
	if got := readConfigValue("anything"); got != "" {
		t.Errorf("missing file should yield empty, got %q", got)
	}
}

func TestReadConfigValue_HappyPath(t *testing.T) {
	withFakeHome(t)
	if err := writeConfigValue("k1", "v1"); err != nil {
		t.Fatal(err)
	}
	if got := readConfigValue("k1"); got != "v1" {
		t.Errorf("expected v1, got %q", got)
	}
}

func TestReadConfigValue_TolerantOfCommentsAndBlanks(t *testing.T) {
	dir := withFakeHome(t)
	path := filepath.Join(dir, ".hearth", "credentials")
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	contents := "# leading comment\n\n  k1 = v1 \n# another\nk2=v2\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readConfigValue("k1"); got != "v1" {
		t.Errorf("expected v1 (whitespace trimmed), got %q", got)
	}
	if got := readConfigValue("k2"); got != "v2" {
		t.Errorf("expected v2, got %q", got)
	}
}

func TestReadConfigValue_UnknownKeyReturnsEmpty(t *testing.T) {
	withFakeHome(t)
	_ = writeConfigValue("k1", "v1")
	if got := readConfigValue("nope"); got != "" {
		t.Errorf("unknown key should yield empty, got %q", got)
	}
}

// ---------- writeConfigValue ----------

func TestWriteConfigValue_CreatesFileAndDir(t *testing.T) {
	dir := withFakeHome(t)
	if err := writeConfigValue("k", "v"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".hearth", "credentials")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("credentials file should exist, got %v", err)
	}
}

func TestWriteConfigValue_UpsertPreservesSiblings(t *testing.T) {
	withFakeHome(t)
	_ = writeConfigValue("a", "1")
	_ = writeConfigValue("b", "2")
	_ = writeConfigValue("a", "3") // upsert a, b should remain
	if got := readConfigValue("a"); got != "3" {
		t.Errorf("a should be upserted to 3, got %q", got)
	}
	if got := readConfigValue("b"); got != "2" {
		t.Errorf("b should be untouched, got %q", got)
	}
}

func TestWriteConfigValue_PerformsAtomicMode0600(t *testing.T) {
	dir := withFakeHome(t)
	if err := writeConfigValue("secret", "shh"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, ".hearth", "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected 0600, got %v", info.Mode().Perm())
	}
}

func TestWriteConfigValue_TightensPermsOnExistingFile(t *testing.T) {
	dir := withFakeHome(t)
	path := filepath.Join(dir, ".hearth", "credentials")
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	// Older CLI versions wrote 0644 — verify we tighten on next write.
	if err := os.WriteFile(path, []byte("old=value\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigValue("new", "x"); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected 0600 after upsert on stale-perms file, got %v", info.Mode().Perm())
	}
}

func TestWriteConfigValue_PreservesCommentsAcrossUpsert(t *testing.T) {
	dir := withFakeHome(t)
	path := filepath.Join(dir, ".hearth", "credentials")
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	initial := "# user-edited header\nk1=v1\n# inline note\nk2=v2\n"
	_ = os.WriteFile(path, []byte(initial), 0600)

	if err := writeConfigValue("k1", "v1-new"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "# user-edited header") {
		t.Error("upsert dropped comment header")
	}
	if !strings.Contains(string(got), "# inline note") {
		t.Error("upsert dropped inline comment")
	}
	if !strings.Contains(string(got), "k1=v1-new") {
		t.Error("upsert lost the new value")
	}
	if !strings.Contains(string(got), "k2=v2") {
		t.Error("upsert dropped sibling key")
	}
}

func TestWriteConfigValue_AppendsWhenKeyAbsent(t *testing.T) {
	withFakeHome(t)
	_ = writeConfigValue("a", "1")
	_ = writeConfigValue("b", "2")
	if readConfigValue("a") != "1" || readConfigValue("b") != "2" {
		t.Error("both keys should be present after independent writes")
	}
}
