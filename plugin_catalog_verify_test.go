package main

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testCatalogPubKeyHex is the public half of a THROWAWAY keypair generated
// solely for these tests. The private half was never saved and has no
// relationship to the real catalog signing key.
//
// It exists to pin OpenSSL/Go interop. testdata/catalog/index.json.sig was
// produced by hearth-plugins' scripts/sign-index.sh running real OpenSSL —
// so if `openssl pkeyutl -sign -rawin` and Go's ed25519.Verify ever stop
// agreeing, or the "raw key is the last 32 bytes of the DER" extraction the
// keygen script relies on changes, this test fails. That interop is an
// assumption spanning two languages and a shell script, which makes it
// exactly the kind of thing that should not be assumed.
const testCatalogPubKeyHex = "11a7e0940d2466b7f45236522fe0897087010fcab9dedb2614251d35a9b0f08d"

// withTrustedKeys swaps the pinned key set for one test.
func withTrustedKeys(t *testing.T, keys ...string) {
	t.Helper()
	prev := trustedCatalogKeys
	trustedCatalogKeys = keys
	t.Cleanup(func() { trustedCatalogKeys = prev })
}

func readCatalogSigFixtures(t *testing.T) (body, sig []byte) {
	t.Helper()
	var err error
	body, err = os.ReadFile(filepath.Join("testdata", "catalog", "index.json"))
	if err != nil {
		t.Fatalf("read index fixture: %v", err)
	}
	sig, err = os.ReadFile(filepath.Join("testdata", "catalog", "index.json.sig"))
	if err != nil {
		t.Fatalf("read signature fixture: %v", err)
	}
	return body, sig
}

// The headline interop test: a signature made by the OpenSSL shell script
// must verify with Go's standard library.
func TestVerifyCatalogIndex_AcceptsOpenSSLSignature(t *testing.T) {
	withTrustedKeys(t, testCatalogPubKeyHex)
	body, sig := readCatalogSigFixtures(t)

	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("fixture signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	if _, err := verifyCatalogIndex(body, sig); err != nil {
		t.Fatalf("a valid OpenSSL-produced signature must verify: %v", err)
	}
}

func TestVerifyCatalogIndex_RejectsTamperedBody(t *testing.T) {
	withTrustedKeys(t, testCatalogPubKeyHex)
	body, sig := readCatalogSigFixtures(t)

	// One flipped byte anywhere in the index must invalidate it — this is
	// what stops a modified version number or file hash from being accepted.
	tampered := make([]byte, len(body))
	copy(tampered, body)
	tampered[len(tampered)/2] ^= 0x01

	if _, err := verifyCatalogIndex(tampered, sig); err == nil {
		t.Fatal("a modified index must not verify")
	}
}

func TestVerifyCatalogIndex_RejectsWrongKey(t *testing.T) {
	// A well-formed key that simply is not the signer. This is the
	// compromised-or-rotated-key case.
	withTrustedKeys(t, strings.Repeat("ab", 32))
	body, sig := readCatalogSigFixtures(t)

	_, err := verifyCatalogIndex(body, sig)
	if err == nil {
		t.Fatal("a signature from an untrusted key must be refused")
	}
	if !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Rotation depends on a binary trusting more than one key at a time. If this
// breaks, rotation becomes impossible without a flag day.
func TestVerifyCatalogIndex_AcceptsAnyKeyInTheSet(t *testing.T) {
	body, sig := readCatalogSigFixtures(t)

	withTrustedKeys(t, strings.Repeat("ab", 32), testCatalogPubKeyHex)
	if _, err := verifyCatalogIndex(body, sig); err != nil {
		t.Errorf("signer listed second must still be accepted: %v", err)
	}

	withTrustedKeys(t, testCatalogPubKeyHex, strings.Repeat("cd", 32))
	if _, err := verifyCatalogIndex(body, sig); err != nil {
		t.Errorf("signer listed first must still be accepted: %v", err)
	}
}

func TestVerifyCatalogIndex_RejectsMissingOrMalformedSignature(t *testing.T) {
	withTrustedKeys(t, testCatalogPubKeyHex)
	body, sig := readCatalogSigFixtures(t)

	if _, err := verifyCatalogIndex(body, nil); err == nil {
		t.Error("an absent signature must be refused when a key is pinned")
	}
	if _, err := verifyCatalogIndex(body, sig[:32]); err == nil {
		t.Error("a truncated signature must be refused")
	}
	if _, err := verifyCatalogIndex(body, append(sig, 0x00)); err == nil {
		t.Error("an over-long signature must be refused")
	}
}

func TestVerifyCatalogIndex_MalformedPinIsAnError(t *testing.T) {
	// A fat-fingered pin must fail loudly. Silently dropping an
	// unparseable key would shrink the trusted set — a security failure
	// disguised as a typo.
	body, sig := readCatalogSigFixtures(t)

	withTrustedKeys(t, "nothex!!")
	if _, err := verifyCatalogIndex(body, sig); err == nil {
		t.Error("a non-hex pinned key must be an error")
	}
	withTrustedKeys(t, "abcd")
	if _, err := verifyCatalogIndex(body, sig); err == nil {
		t.Error("a wrong-length pinned key must be an error")
	}
}

func TestVerifyCatalogIndex_NoPinnedKeys(t *testing.T) {
	body, sig := readCatalogSigFixtures(t)
	withTrustedKeys(t)

	// Dev build: tolerated, so the catalog can be developed against before a
	// signing key exists.
	withDaemonVersion(t, "dev")
	if _, err := verifyCatalogIndex(body, sig); err != nil {
		t.Errorf("a dev build with no pinned key should tolerate an unsigned catalog: %v", err)
	}

	// Release build: refused. A shipped binary must never fall back to
	// trusting TLS alone, even if someone forgets to pin a key.
	withDaemonVersion(t, "v1.1.0")
	_, err := verifyCatalogIndex(body, sig)
	if err == nil {
		t.Fatal("a release build with no pinned key must refuse, not degrade to trusting TLS")
	}
	if !strings.Contains(err.Error(), "build defect") {
		t.Errorf("the error should name this as a build defect, got: %v", err)
	}
}

func TestCatalogSignatureRequired(t *testing.T) {
	// Only a keyless dev build skips fetching the signature.
	withTrustedKeys(t)
	withDaemonVersion(t, "dev")
	if catalogSignatureRequired() {
		t.Error("keyless dev build should not fetch a signature it cannot check")
	}

	withDaemonVersion(t, "v1.1.0")
	if !catalogSignatureRequired() {
		t.Error("release build must always fetch the signature")
	}

	withTrustedKeys(t, testCatalogPubKeyHex)
	withDaemonVersion(t, "dev")
	if !catalogSignatureRequired() {
		t.Error("a pinned key means the signature is required even on a dev build")
	}
}

// The verification result has to be reportable, not just correct. A check
// whose outcome never reaches the operator is indistinguishable from no check
// at all — which is exactly how a build with an empty key set managed to
// "successfully install" while verifying nothing.
func TestCatalogVerification_ReportsOutcome(t *testing.T) {
	withTrustedKeys(t, testCatalogPubKeyHex)
	body, sig := readCatalogSigFixtures(t)

	v, err := verifyCatalogIndex(body, sig)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.Verified {
		t.Fatal("a good signature must report Verified")
	}
	if v.KeyID != testCatalogPubKeyHex[:8] {
		t.Errorf("KeyID = %q, want the first 8 hex chars of the matching key", v.KeyID)
	}
	desc := v.Describe()
	if !strings.Contains(desc, "verified") || !strings.Contains(desc, v.KeyID) {
		t.Errorf("Describe() should name the outcome and the key, got %q", desc)
	}
	if strings.Contains(desc, "NOT VERIFIED") {
		t.Errorf("a verified index must not describe itself as unverified: %q", desc)
	}
}

func TestCatalogVerification_ReportsSkip(t *testing.T) {
	withTrustedKeys(t)
	withDaemonVersion(t, "dev")
	body, sig := readCatalogSigFixtures(t)

	v, err := verifyCatalogIndex(body, sig)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Verified {
		t.Fatal("a keyless dev build must not claim it verified anything")
	}
	if v.SkipReason == "" {
		t.Error("a skipped verification must say why")
	}
	if !strings.Contains(v.Describe(), "NOT VERIFIED") {
		t.Errorf("a skipped verification must be conspicuous, got %q", v.Describe())
	}
}

func TestIsReleaseBuild(t *testing.T) {
	for _, v := range []string{"v1.1.0", "1.1.0", "v0.9.3"} {
		withDaemonVersion(t, v)
		if !isReleaseBuild() {
			t.Errorf("%q should count as a release build", v)
		}
	}
	for _, v := range []string{"dev", "", "some-build-id"} {
		withDaemonVersion(t, v)
		if isReleaseBuild() {
			t.Errorf("%q should not count as a release build", v)
		}
	}
}
