//go:build darwin || linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// TestFallbackPromptMatchesSeededDefault guards against the CLI's compiled-in
// hearthSystemPromptFallback silently drifting from the "Hearth Default" prompt
// seeded server-side (relay/sql/seed.sql). The live prompt is server-owned; the
// const is only a fallback for old-server / unseeded-catalog spawns — but if
// the two diverge, an agent that falls back gets subtly different instructions
// than one served by the relay, which is exactly the class of bug this whole
// change was meant to kill.
//
// The check is by hash, not by parsing the SQL: the fallback's sha256 must
// appear verbatim in seed.sql (it's stored as the row's content_sha256). Edit
// the const without cutting a new seeded version — or vice versa — and the hash
// no longer matches, failing this test with a pointer to what to do.
//
// Skips when relay/sql/seed.sql isn't reachable (the standalone public CLI repo
// ships cli/ without the relay tree), so it runs in the monorepo and no-ops
// after cli-release.sh.
func TestFallbackPromptMatchesSeededDefault(t *testing.T) {
	const seedPath = "../relay/sql/seed.sql"
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		t.Skipf("skipping drift check: %s not reachable (%v)", seedPath, err)
	}

	sum := sha256.Sum256([]byte(hearthSystemPromptFallback))
	want := hex.EncodeToString(sum[:])

	if !strings.Contains(string(seed), want) {
		t.Fatalf(
			"hearthSystemPromptFallback sha256 %s not found in %s.\n"+
				"The CLI fallback prompt and the seeded Hearth Default have drifted.\n"+
				"Fix by keeping them in sync: if you edited the const, cut a NEW system_prompts\n"+
				"version in seed.sql (new id, same family_id, next version) with matching text +\n"+
				"content_sha256; if you edited the seed, update the const to match.",
			want, seedPath,
		)
	}
}
