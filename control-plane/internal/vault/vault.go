// Package vault seals platform secrets with AES-256-GCM
// (docs/specs/platform-secrets.md): a 32-byte master key held in a
// 0600-permission file OUTSIDE the database file, a fresh random 12-byte
// nonce prepended to every ciphertext, base64 storage. Key material and
// plaintext NEVER appear in logs or error messages.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// keyBytes is the AES-256 master key length.
const keyBytes = 32

// Vault seals and opens secret payloads with one AES-256-GCM key.
type Vault struct {
	aead cipher.AEAD
}

// New builds a Vault over a raw 32-byte key (tests inject fixed keys;
// deployments use Open over the key file).
func New(key []byte) (*Vault, error) {
	if len(key) != keyBytes {
		return nil, fmt.Errorf("vault: key must be %d bytes, got %d", keyBytes, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

// Open loads the hex-encoded master key at keyPath, auto-generating it with
// 0600 permissions on first use. An EXISTING file with any group/other
// permission bits refuses to start — a readable key file defeats
// encryption at rest. The key value never appears in errors.
func Open(keyPath string) (*Vault, error) {
	info, err := os.Stat(keyPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return generate(keyPath)
	case err != nil:
		return nil, fmt.Errorf("vault: stat key file %s: %w", keyPath, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("vault: key file %s has permissions %04o; require no group/other bits (chmod 600)", keyPath, perm)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("vault: read key file %s: %w", keyPath, err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != keyBytes {
		return nil, fmt.Errorf("vault: key file %s must hold %d hex-encoded bytes", keyPath, keyBytes)
	}
	return New(key)
}

// generate mints a fresh CSPRNG key and writes it 0600, exclusively — a
// concurrent generator loses on O_EXCL rather than truncating the winner.
func generate(keyPath string) (*Vault, error) {
	key := make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vault: create key file %s: %w", keyPath, err)
	}
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		f.Close()
		return nil, fmt.Errorf("vault: write key file %s: %w", keyPath, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("vault: close key file %s: %w", keyPath, err)
	}
	return New(key)
}

// Seal encrypts plaintext as base64(nonce || AES-256-GCM ciphertext) with a
// fresh random 12-byte nonce per call.
func (v *Vault) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(v.aead.Seal(nonce, nonce, plaintext, nil)), nil
}

// Open decrypts a Seal output. Tampered, truncated, or foreign-key input
// fails GCM authentication; errors carry no plaintext or key material.
func (v *Vault) Open(sealed string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, errors.New("vault: sealed value is not valid base64")
	}
	n := v.aead.NonceSize()
	if len(raw) < n {
		return nil, errors.New("vault: sealed value is too short")
	}
	plaintext, err := v.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return nil, errors.New("vault: decryption failed (wrong key or tampered ciphertext)")
	}
	return plaintext, nil
}
