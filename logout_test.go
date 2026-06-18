//go:build darwin || linux

package main

// Coverage for cleanupTmpArtifacts. The function fans out across
// hard-coded /tmp glob patterns plus the daemon socket path; we
// can't redirect /tmp from a test, so we scope the assertion to
// "files we created get removed when they match a pattern."

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupTmpArtifacts_RemovesMatchingFiles(t *testing.T) {
	// Create files in /tmp matching each glob pattern. Use a per-test
	// suffix so concurrent runs don't collide.
	suffix := "test-cleanup-" + t.Name()
	created := []string{
		filepath.Join("/tmp", "hearth-bridge-"+suffix),
		filepath.Join("/tmp", "hearth-stream-"+suffix+".pid"),
		filepath.Join("/tmp", "hearth-interpose-"+suffix+".log"),
		filepath.Join("/tmp", ".gl-"+suffix),
		filepath.Join("/tmp", "gl-"+suffix+".sock"),
	}
	for _, p := range created {
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
		t.Cleanup(func() { _ = os.Remove(p) }) // belt-and-suspenders
	}

	cleanupTmpArtifacts()

	for _, p := range created {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, got %v", p, err)
		}
	}
}

func TestCleanupTmpArtifacts_TolerantOfMissingFiles(t *testing.T) {
	// No matching files exist — must not panic, must not error.
	// (We can't fully isolate /tmp, but the function ignores missing
	// paths via os.Remove returning an error we discard.)
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("cleanupTmpArtifacts panicked with no files: %v", r)
		}
	}()
	cleanupTmpArtifacts()
}

func TestCleanupTmpArtifacts_LeavesNonMatchingFilesAlone(t *testing.T) {
	// A file that does NOT match any glob pattern should not be touched.
	keep := filepath.Join(t.TempDir(), "unrelated.txt")
	if err := os.WriteFile(keep, []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}

	cleanupTmpArtifacts()

	if _, err := os.Stat(keep); err != nil {
		t.Errorf("non-matching file must be preserved, got %v", err)
	}
}
