package main

// Verifier CLI tests (docs/specs/ops-backup.md §Test obligations, CLI
// side): exit codes on a good artifact, a bit-flipped artifact, and an
// FK-violating file; read-only proof (bytes unchanged, no sidecars).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// goodArtifact builds a standalone DB file: store.Open applies the full
// schema; the clean Close checkpoints and removes the WAL.
func goodArtifact(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func sha256File(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return sha256.Sum256(b)
}

// TestVerifyGoodArtifact: exit 0, the report shows integrity ok and zero
// FK violations, and verification is read-only — the file's bytes are
// unchanged and no sidecar files appear beside it (invariant 7).
func TestVerifyGoodArtifact(t *testing.T) {
	path := goodArtifact(t)
	before := sha256File(t, path)
	dir := filepath.Dir(path)
	entriesBefore, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"-db", path}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d (stdout %q, stderr %q), want 0", code, out.String(), errOut.String())
	}
	for _, want := range []string{"integrity_check: ok", "foreign_key_check violations: 0",
		"user_version: 0", "newest runs.created_at:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report %q missing %q", out.String(), want)
		}
	}

	if after := sha256File(t, path); after != before {
		t.Error("artifact bytes changed: the verifier wrote to the file it checks")
	}
	entriesAfter, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entriesAfter) != len(entriesBefore) {
		t.Errorf("dir grew from %d to %d entries: sidecar files created", len(entriesBefore), len(entriesAfter))
	}
}

// TestVerifyCorruptedArtifact: a bit-flipped page fails the integrity
// check — exit 1.
func TestVerifyCorruptedArtifact(t *testing.T) {
	path := goodArtifact(t)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Page size lives at header bytes 16-17 (big-endian; 1 means 65536).
	page := int(binary.BigEndian.Uint16(b[16:18]))
	if page == 1 {
		page = 65536
	}
	if len(b) <= page+32 {
		t.Fatalf("artifact has a single page (%d bytes); cannot corrupt page 2", len(b))
	}
	for i := 0; i < 32; i++ {
		b[page+i] ^= 0xFF
	}
	corrupted := filepath.Join(t.TempDir(), "corrupted.db")
	if err := os.WriteFile(corrupted, b, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"-db", corrupted}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d (stdout %q, stderr %q), want 1", code, out.String(), errOut.String())
	}
}

// TestVerifyFKViolation: integrity passes but foreign_key_check reports a
// violation — exit 1.
func TestVerifyFKViolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fk.db")
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// foreign_keys is OFF by default, so the orphan row inserts cleanly.
	stmts := []string{
		`CREATE TABLE parent (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER NOT NULL REFERENCES parent (id))`,
		`INSERT INTO child (id, pid) VALUES (1, 999)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"-db", path}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d (stdout %q, stderr %q), want 1", code, out.String(), errOut.String())
	}
	for _, want := range []string{"integrity_check: ok", "foreign_key_check violations: 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report %q missing %q", out.String(), want)
		}
	}
}

// TestVerifyStampedArtifact: an engine artifact carries the DS-1
// user_version stamp; verification passes unchanged (deploy-and-
// survive.md DS-1a) and the report prints the user_version line.
func TestVerifyStampedArtifact(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	dir := t.TempDir()
	res, err := s.Backup(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"-db", filepath.Join(dir, res.Artifact)}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d (stdout %q, stderr %q), want 0", code, out.String(), errOut.String())
	}
	for _, want := range []string{"integrity_check: ok", "foreign_key_check violations: 0", "user_version: 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report %q missing %q", out.String(), want)
		}
	}
}

// TestVerifyMissingFlag: no -db is a usage error, exit 2.
func TestVerifyMissingFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
