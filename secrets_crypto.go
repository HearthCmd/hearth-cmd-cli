//go:build darwin || linux

package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Crypto primitives for the secrets vault.

// secretKeyPath returns the on-disk location of the daemon's
// X25519 private key. ~/.hearth/key, mode 0600, dir 0700.
func secretKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hearth", "key"), nil
}

func generateSecretsKeypair() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

// loadSecretsPrivateKey reads a base64-encoded X25519 private key
// from ~/.hearth/key. Returns os.ErrNotExist when the file is
// absent so callers can branch on first-boot vs reload.
func loadSecretsPrivateKey() (*ecdh.PrivateKey, error) {
	path, err := secretKeyPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	return ecdh.X25519().NewPrivateKey(raw)
}

// saveSecretsPrivateKey writes the X25519 private key with 0600
// perms. Creates ~/.hearth with 0700 if missing. Refuses overwrite
// unless explicitly requested — accidentally regenerating the key
// orphans every existing ciphertext (the matching private key is
// gone), so the daemon's load-or-generate path always passes
// overwrite=false on first boot.
func saveSecretsPrivateKey(key *ecdh.PrivateKey, overwrite bool) error {
	path, err := secretKeyPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create key dir: %w", err)
	}
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("key already exists at %s", path)
		}
	}
	encoded := base64.StdEncoding.EncodeToString(key.Bytes())
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(encoded), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadOrGenerateSecretsKey is the boot-time entry point. Loads if
// present, otherwise generates and persists. Returns the private
// key. Caller derives the public key on demand via .PublicKey().
func loadOrGenerateSecretsKey() (*ecdh.PrivateKey, error) {
	priv, err := loadSecretsPrivateKey()
	if err == nil {
		return priv, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("load secrets key: %w", err)
	}
	priv, err = generateSecretsKeypair()
	if err != nil {
		return nil, fmt.Errorf("generate secrets key: %w", err)
	}
	if err := saveSecretsPrivateKey(priv, false); err != nil {
		return nil, fmt.Errorf("save secrets key: %w", err)
	}
	return priv, nil
}

// encryptSecretEnvelope seals plaintext to recipientPub using
// X25519 + HKDF-SHA256 + ChaCha20Poly1305. Format:
//
//	ephemeral_public(32) || nonce(12) || ciphertext+tag(16)
//
// Wire format: ephemeral_public(32) || nonce(12) || ciphertext+tag(16).
// iOS envelopes use the same format so the daemon can decrypt them.
func encryptSecretEnvelope(recipientPub *ecdh.PublicKey, plaintext []byte) ([]byte, error) {
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral key: %w", err)
	}
	shared, err := ephemeral.ECDH(recipientPub)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}
	symKey := make([]byte, chacha20poly1305.KeySize)
	if _, err := hkdf.New(sha256.New, shared, nil, nil).Read(symKey); err != nil {
		return nil, fmt.Errorf("HKDF: %w", err)
	}
	aead, err := chacha20poly1305.New(symKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := aead.Seal(nil, nonce, plaintext, nil)

	out := make([]byte, 0, 32+len(nonce)+len(sealed))
	out = append(out, ephemeral.PublicKey().Bytes()...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// secretsPubFingerprint returns 8 hex chars of SHA-256 over the
// pubkey. Used in boot logs so operators can confirm the
// keypair didn't silently change between restarts.
func secretsPubFingerprint(priv *ecdh.PrivateKey) string {
	pub := priv.PublicKey().Bytes()
	h := sha256.Sum256(pub)
	return fmt.Sprintf("%x", h[:4])
}

// enrollHostPubkeyAtBoot uploads d.secretsPrivKey's public half to
// the server via the enroll_host_pubkey WS endpoint. Boot-time
// one-shot. Same wait-for-WS pattern as the rule-seed goroutine.
//
// Server refuses different-pubkey overwrites (1d server commit 3);
// same-pubkey re-enrolls are idempotent. So repeat boots are
// no-ops; a regenerated key would fail enrollment and dogfooding
// would surface a clear "rotation requires manual intervention"
// log line.
func (d *Daemon) enrollHostPubkeyAtBoot() {
	if d.secretsPrivKey == nil {
		// Boot order quirk: load happened earlier. Defensive.
		log.Printf("daemon: enroll_host_pubkey skipped: no secrets keypair")
		return
	}
	if !d.waitForDaemonWS(seedWSConnectTimeout) {
		log.Printf("daemon: enroll_host_pubkey skipped: WS did not connect")
		return
	}
	pub := d.secretsPrivKey.PublicKey().Bytes()
	payload, err := json.Marshal(map[string]string{
		"pubkey": base64.StdEncoding.EncodeToString(pub),
	})
	if err != nil {
		log.Printf("daemon: enroll_host_pubkey marshal: %v", err)
		return
	}
	raw, err := d.daemonWS.SendWSRequest(generateUUID(), "enroll_host_pubkey", payload)
	if err != nil {
		log.Printf("daemon: enroll_host_pubkey send: %v", err)
		return
	}
	var resp struct {
		Type    string `json:"type"`
		Error   string `json:"error"`
		Updated bool   `json:"updated"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		log.Printf("daemon: enroll_host_pubkey decode: %v", err)
		return
	}
	if resp.Type == "error" {
		log.Printf("daemon: enroll_host_pubkey server error: %s", resp.Error)
		return
	}
	if resp.Updated {
		log.Printf("daemon: enrolled secrets pubkey with server")
	} else {
		log.Printf("daemon: secrets pubkey already enrolled (matches existing)")
	}
}

// decryptSecretEnvelope opens a blob produced by encryptSecretEnvelope.
func decryptSecretEnvelope(priv *ecdh.PrivateKey, blob []byte) ([]byte, error) {
	if len(blob) < 32+12+16 {
		return nil, fmt.Errorf("ciphertext too short")
	}
	ephPub, err := ecdh.X25519().NewPublicKey(blob[:32])
	if err != nil {
		return nil, fmt.Errorf("parse ephemeral pubkey: %w", err)
	}
	nonce := blob[32:44]
	ct := blob[44:]

	shared, err := priv.ECDH(ephPub)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}
	symKey := make([]byte, chacha20poly1305.KeySize)
	if _, err := hkdf.New(sha256.New, shared, nil, nil).Read(symKey); err != nil {
		return nil, fmt.Errorf("HKDF: %w", err)
	}
	aead, err := chacha20poly1305.New(symKey)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ct, nil)
}
