//go:build darwin || linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// withTempHome redirects $HOME for the test so keystore reads/writes
// land in t.TempDir() and don't touch the operator's real
// ~/.hearth/key. Restores HOME on cleanup.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev, hadPrev := os.LookupEnv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() {
		if hadPrev {
			os.Setenv("HOME", prev)
		} else {
			os.Unsetenv("HOME")
		}
	})
	return dir
}

func TestSecretsCrypto_GenerateAndRoundTrip(t *testing.T) {
	priv, err := generateSecretsKeypair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	plaintext := []byte("ha-token-value-with-non-ascii-éxöß")
	envelope, err := encryptSecretEnvelope(priv.PublicKey(), plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Envelope length floor matches what the server validates.
	if len(envelope) < 32+12+16 {
		t.Errorf("envelope too short: %d bytes", len(envelope))
	}
	got, err := decryptSecretEnvelope(priv, envelope)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestSecretsCrypto_WrongKeyFailsDecrypt(t *testing.T) {
	alice, _ := generateSecretsKeypair()
	bob, _ := generateSecretsKeypair()
	envelope, err := encryptSecretEnvelope(alice.PublicKey(), []byte("secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := decryptSecretEnvelope(bob, envelope); err == nil {
		t.Error("decrypt with wrong key must fail")
	}
}

func TestSecretsCrypto_ShortEnvelopeRejected(t *testing.T) {
	priv, _ := generateSecretsKeypair()
	if _, err := decryptSecretEnvelope(priv, []byte("too short")); err == nil {
		t.Error("decrypt of too-short blob must fail")
	}
}

func TestKeystore_FirstBootGeneratesAndPersists(t *testing.T) {
	home := withTempHome(t)

	priv1, err := loadOrGenerateSecretsKey()
	if err != nil {
		t.Fatalf("first-boot load-or-generate: %v", err)
	}
	keyFile := filepath.Join(home, ".hearth", "key")
	fi, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("key file not written: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file perms = %o; want 0600", fi.Mode().Perm())
	}

	// Second call returns the SAME key (load path, not generate).
	priv2, err := loadOrGenerateSecretsKey()
	if err != nil {
		t.Fatalf("second-boot load: %v", err)
	}
	if !bytes.Equal(priv1.Bytes(), priv2.Bytes()) {
		t.Error("second load returned a different key — persistence broken")
	}
}

func TestKeystore_RefusesOverwriteOfExisting(t *testing.T) {
	withTempHome(t)
	priv, _ := generateSecretsKeypair()
	if err := saveSecretsPrivateKey(priv, false); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// Second save with overwrite=false must refuse.
	if err := saveSecretsPrivateKey(priv, false); err == nil {
		t.Error("save with overwrite=false must refuse existing file")
	}
	// overwrite=true succeeds (only used for explicit rotation paths).
	if err := saveSecretsPrivateKey(priv, true); err != nil {
		t.Errorf("save with overwrite=true should succeed: %v", err)
	}
}

func TestSecretsPubFingerprint_DeterministicAndShort(t *testing.T) {
	priv, _ := generateSecretsKeypair()
	a := secretsPubFingerprint(priv)
	b := secretsPubFingerprint(priv)
	if a != b {
		t.Errorf("fingerprint not deterministic: %s vs %s", a, b)
	}
	if len(a) != 8 { // 4 bytes hex-encoded
		t.Errorf("fingerprint = %q (len %d); want 8-char hex", a, len(a))
	}
}
