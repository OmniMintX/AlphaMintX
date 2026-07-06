package vault

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, keyBytes) }

// TestSealOpenRoundTrip: Open(Seal(p)) == p for varied payloads, and two
// Seals of the same plaintext differ (fresh random nonce per call).
func TestSealOpenRoundTrip(t *testing.T) {
	v, err := New(testKey(7))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payloads := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"json", []byte(`{"api_key":"k","api_secret":"s"}`)},
		{"unicode", []byte("pässwörd-秘密")},
		{"binary", []byte{0, 1, 2, 255, 254}},
	}
	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			sealed, err := v.Seal(tc.data)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			got, err := v.Open(sealed)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !bytes.Equal(got, tc.data) {
				t.Errorf("round trip = %q, want %q", got, tc.data)
			}
			again, err := v.Seal(tc.data)
			if err != nil {
				t.Fatalf("second Seal: %v", err)
			}
			if again == sealed {
				t.Error("two Seals of the same plaintext are identical (nonce reuse)")
			}
		})
	}
}

// TestOpenRejectsBadInput: bad base64, truncation, a flipped ciphertext
// byte, and a foreign key all fail — and no error leaks the plaintext.
func TestOpenRejectsBadInput(t *testing.T) {
	v, err := New(testKey(7))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, err := v.Seal([]byte("attack-at-dawn"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(sealed)
	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)-1] ^= 0x01
	other, _ := New(testKey(8))

	tests := []struct {
		name   string
		vault  *Vault
		sealed string
	}{
		{"not base64", v, "%%not-base64%%"},
		{"too short", v, base64.StdEncoding.EncodeToString(raw[:5])},
		{"tampered", v, base64.StdEncoding.EncodeToString(flipped)},
		{"wrong key", other, sealed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.vault.Open(tc.sealed)
			if err == nil {
				t.Fatalf("Open = %q, want error", got)
			}
			if strings.Contains(err.Error(), "attack-at-dawn") {
				t.Errorf("error %q leaks plaintext", err)
			}
		})
	}
}

// TestOpenKeyFile: first use generates a 0600 key file; a reopen loads the
// SAME key (cross-instance Open succeeds); loose permissions and malformed
// contents refuse to start.
func TestOpenKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db.secrets.key")
	v1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file permissions = %04o, want 0600", perm)
	}
	sealed, err := v1.Seal([]byte("cross-instance"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	v2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if got, err := v2.Open(sealed); err != nil || string(got) != "cross-instance" {
		t.Fatalf("reloaded key Open = %q, %v; want the same key material", got, err)
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Error("Open accepted a key file with group permission bits")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod back: %v", err)
	}

	garbage := filepath.Join(t.TempDir(), "garbage.key")
	if err := os.WriteFile(garbage, []byte("not-hex\n"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if _, err := Open(garbage); err == nil {
		t.Error("Open accepted a malformed key file")
	}
	short := filepath.Join(t.TempDir(), "short.key")
	if err := os.WriteFile(short, []byte("deadbeef\n"), 0o600); err != nil {
		t.Fatalf("write short: %v", err)
	}
	if _, err := Open(short); err == nil {
		t.Error("Open accepted a short key")
	}
}
