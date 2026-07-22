package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"log"
)

// Signature verification for the plugin catalog index.
//
// TLS proves you reached github.com. It does not prove that what github.com
// served is what Verge Labs published — a compromised repo, a bad release
// upload, or a stolen GitHub credential all serve perfectly valid TLS. The
// signature is what closes that gap, and it is the only reason installing by
// name is meaningfully safer than piping a URL to a shell.
//
// One signature over index.json covers the entire catalog, because the index
// carries a hash of every published file. That includes every version number,
// which is what makes a rollback attack — serving a real, correctly-signed,
// but known-vulnerable older plugin — detectable. Signing each file
// separately would not: every old file remains validly signed forever.

// trustedCatalogKeys is the pinned set of public keys whose signature over
// index.json this binary will accept. Raw 32-byte ed25519 public keys, hex.
//
// A SET rather than a single key, deliberately, and this is not premature.
// Rotation is: ship a binary trusting {old, new}, wait for it to propagate,
// then ship one trusting {new}. A single-key binary cannot be taught a second
// key without exactly the release you would be scrambling to publish, so the
// plural is impossible to retrofit at the moment you need it. It costs one
// character today.
//
// Empty means unsigned catalogs are tolerated on dev builds only — see
// verifyCatalogIndex. A release binary with an empty set refuses everything.
var trustedCatalogKeys = []string{
	// Verge Labs catalog signing key, generated 2026-07-21. Public half
	// only — this is meant to be published; it is the private half in cold
	// storage on the signing machine that matters.
	"c1b393d1a4ee6563c6cb4d5a141681c0adece64c75fcfde3ae28abbd76f65cdf",
}

// parseTrustedCatalogKeys decodes the pinned hex keys. A malformed pin is a
// programming error caught at first use rather than silently reducing the
// trusted set — dropping a key because someone fat-fingered a hex digit would
// be a security failure that looks like a typo.
func parseTrustedCatalogKeys() ([]ed25519.PublicKey, error) {
	out := make([]ed25519.PublicKey, 0, len(trustedCatalogKeys))
	for i, h := range trustedCatalogKeys {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("trustedCatalogKeys[%d] is not valid hex: %w", i, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("trustedCatalogKeys[%d] is %d bytes, want %d",
				i, len(raw), ed25519.PublicKeySize)
		}
		out = append(out, ed25519.PublicKey(raw))
	}
	return out, nil
}

// isReleaseBuild reports whether this binary carries a real version.
//
// Dev and local builds inject "dev" (scripts/cli/cli-build.sh); releases
// inject a semver, v-prefixed. Reusing the version parser rather than a
// separate build flag means there is exactly one notion of "is this a real
// build" in the codebase, and no way for the two to disagree.
func isReleaseBuild() bool {
	return semverParts(version) != nil
}

// catalogVerification records what actually happened when the index was
// checked, so the outcome can be reported rather than assumed.
//
// This exists because a signature check nobody can observe is
// indistinguishable from no signature check at all. The install path reports
// this to the operator: silently succeeding is the wrong behaviour whether
// the signature was verified or skipped.
type catalogVerification struct {
	// Verified is true only when a signature was checked and matched.
	Verified bool
	// KeyID is a short prefix of the trusted key that matched, so an
	// operator can tell which key signed without printing the whole thing.
	KeyID string
	// SkipReason explains why no signature was checked. Empty when Verified.
	SkipReason string
}

// Describe renders the outcome for a human, leading with whether the thing
// that matters actually happened.
func (v *catalogVerification) Describe() string {
	if v == nil {
		return "signature status unknown"
	}
	if v.Verified {
		return fmt.Sprintf("signature verified (key %s)", v.KeyID)
	}
	return "NOT VERIFIED — " + v.SkipReason
}

// verifyCatalogIndex checks sig over body against the pinned key set.
//
// Fail-closed by construction: the only path that skips verification is an
// empty key set on a non-release build, which exists so the catalog can be
// developed against before a signing key exists. A release binary always
// requires a good signature — including when no key is pinned, where it
// refuses everything rather than degrading to trust-on-TLS.
//
// Note this is the opposite posture to checkMinDaemonVersion, which fails
// OPEN on a dev build. The difference is what a wrong answer costs: there,
// a developer cannot load a plugin; here, a host installs code from an
// unverified source.
func verifyCatalogIndex(body, sig []byte) (*catalogVerification, error) {
	keys, err := parseTrustedCatalogKeys()
	if err != nil {
		return nil, fmt.Errorf("catalog signing keys are misconfigured: %w", err)
	}

	if len(keys) == 0 {
		if isReleaseBuild() {
			return nil, fmt.Errorf("this build has no pinned catalog signing key, so it cannot verify " +
				"the catalog; refusing to install. This is a build defect — report it")
		}
		reason := "no signing key pinned in this dev build; a release build would refuse"
		log.Printf("catalog: WARNING — %s. Installing WITHOUT verifying the catalog signature.", reason)
		return &catalogVerification{SkipReason: reason}, nil
	}

	if len(sig) == 0 {
		return nil, fmt.Errorf("the catalog release has no index.json.sig; refusing to install unsigned plugins")
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("catalog signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}

	for _, k := range keys {
		if ed25519.Verify(k, body, sig) {
			return &catalogVerification{
				Verified: true,
				KeyID:    hex.EncodeToString(k)[:8],
			}, nil
		}
	}
	return nil, fmt.Errorf("catalog signature does not verify against any trusted key — " +
		"the catalog may be tampered with, or this binary may be too old to know the current signing key")
}

// catalogSignatureRequired reports whether fetchCatalogIndex should download
// index.json.sig at all. With no keys pinned on a dev build there is nothing
// to check it against, and a 404 for a signature we would ignore is noise
// rather than an error.
func catalogSignatureRequired() bool {
	keys, err := parseTrustedCatalogKeys()
	if err != nil {
		return true // misconfiguration must surface, not silently skip
	}
	return len(keys) > 0 || isReleaseBuild()
}
