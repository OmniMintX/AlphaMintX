package store

// Backup engine tests (docs/specs/ops-backup.md §Test obligations, store
// side): live backup under concurrent writers, count-parity injection and
// the `.failed` rename, the retention matrix, the `.tmp` lifecycle, the
// checkpoint-failure path, and the zero-logical-writes check.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// pinBackupClock fixes the artifact-name clock.
func pinBackupClock(s *Store, at time.Time) {
	s.backupNow = func() time.Time { return at }
}

// writeFile creates a fixture file in dir.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
	return path
}

// dirNames lists the entry names in dir, sorted by ReadDir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestBackupConcurrentWriters: a backup of a live store with writer
// goroutines running — writers queue on the pool's only connection during
// the hold (invariant 1), the run succeeds, and the artifact passes OB-5
// (the engine's own verification ran) with the digest and size of the
// artifact file.
func TestBackupConcurrentWriters(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	dir := t.TempDir()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				err := s.CreateStrategy(Strategy{
					StrategyID: uid(10000 + w*1000000 + i), TenantID: "tenant-1",
					Name: fmt.Sprintf("w%d-%d", w, i), LifecycleState: "paper",
					CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
				})
				if err != nil {
					t.Errorf("writer %d insert %d: %v", w, i, err)
					return
				}
			}
		}(w)
	}

	res, err := s.Backup(context.Background(), dir, 0)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !res.Verified {
		t.Error("result.Verified = false, want true")
	}
	if !backupNamePattern.MatchString(res.Artifact) {
		t.Errorf("artifact name %q does not match the OB-4 pattern", res.Artifact)
	}
	artifact := filepath.Join(dir, res.Artifact)
	sha, err := sha256File(artifact)
	if err != nil {
		t.Fatalf("sha256File: %v", err)
	}
	if sha != res.SHA256 {
		t.Errorf("artifact digest = %s, result.SHA256 = %s", sha, res.SHA256)
	}
	info, err := os.Stat(artifact)
	if err != nil {
		t.Fatalf("Stat artifact: %v", err)
	}
	if info.Size() != res.Bytes {
		t.Errorf("artifact size = %d, result.Bytes = %d", info.Size(), res.Bytes)
	}
	if res.Tables == 0 || res.RowsTotal == 0 {
		t.Errorf("fingerprint = %d tables / %d rows, want non-zero", res.Tables, res.RowsTotal)
	}
}

// TestBackupInProgressAndSameSecond: a held engine mutex fails immediately
// with ErrBackupInProgress (OB-6a, never queues); two backups within the
// same clock second collide on the artifact name with ErrBackupExists.
func TestBackupInProgressAndSameSecond(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()

	s.backupMu.Lock()
	if _, err := s.Backup(context.Background(), dir, 0); !errors.Is(err, ErrBackupInProgress) {
		t.Errorf("held mutex: err = %v, want ErrBackupInProgress", err)
	}
	s.backupMu.Unlock()

	pinBackupClock(s, testNow)
	if _, err := s.Backup(context.Background(), dir, 0); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if _, err := s.Backup(context.Background(), dir, 0); !errors.Is(err, ErrBackupExists) {
		t.Errorf("same-second backup: err = %v, want ErrBackupExists", err)
	}
}

// TestBackupCountParityFailure: verification is fed a fingerprint the
// artifact cannot match (count-parity injection through the OB-5 seam,
// exercising the REAL verifyBackupArtifact); the run fails under the
// ErrBackupVerifyFailed sentinel, the artifact is renamed `<name>.failed`
// (OB-2 step 6), no `.tmp` remains, and retention does NOT run (OB-9:
// never after a failed backup).
func TestBackupCountParityFailure(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	old := "control-20250101T000000Z.db"
	writeFile(t, dir, old, "old artifact")

	s.backupVerify = func(artifactPath string, fingerprint []tableCount) error {
		phantom := append(append([]tableCount{}, fingerprint...), tableCount{name: "zzz_phantom", count: 1})
		return verifyBackupArtifact(artifactPath, phantom)
	}
	pinBackupClock(s, testNow)
	res, err := s.Backup(context.Background(), dir, 1)
	if !errors.Is(err, ErrBackupVerifyFailed) {
		t.Fatalf("err = %v, want ErrBackupVerifyFailed", err)
	}
	name := "control-" + testNow.Format("20060102T150405") + "Z.db"
	if res.Artifact != name {
		t.Errorf("result.Artifact = %q, want %q (the API 500 message names the basename)", res.Artifact, name)
	}
	if _, err := os.Stat(filepath.Join(dir, name+".failed")); err != nil {
		t.Errorf("failed artifact: %v, want %s.failed kept for forensics", err, name)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plain artifact still present after verify failure (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, name+".tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp still present after verify failure (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, old)); err != nil {
		t.Errorf("retention ran after a FAILED backup: old artifact gone (%v)", err)
	}
}

// TestBackupRetentionMatrix: retention deletes only OB-4-matching names
// beyond the newest N ordered BY NAME — mtime is never the key, foreign
// names / `.failed` files are never touched, and the N boundary deletes
// nothing.
func TestBackupRetentionMatrix(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	oldest := "control-20250101T000000Z.db"
	middle := "control-20250102T000000Z.db"
	writeFile(t, dir, oldest, "a")
	writeFile(t, dir, middle, "b")
	foreign := []string{"notes.txt", "control-2025.db", "other-20250101T000000Z.db", oldest + ".failed"}
	for _, name := range foreign {
		writeFile(t, dir, name, "x")
	}
	// Perturb mtimes: the OLDEST name gets the NEWEST mtime (off-host
	// copy-back scenario) — deletion must still pick it by NAME.
	if err := os.Chtimes(filepath.Join(dir, oldest), testNow, testNow); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, middle), testNow.Add(-48*time.Hour), testNow.Add(-48*time.Hour)); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	pinBackupClock(s, testNow)
	res, err := s.Backup(context.Background(), dir, 2)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, oldest)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("oldest NAME survived retention (mtime must not be the key), err %v", err)
	}
	for _, name := range append(foreign, middle, res.Artifact) {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v, want untouched by retention", name, err)
		}
	}

	// N boundary: exactly retain artifacts after the run deletes nothing.
	s2 := openStore(t)
	dir2 := t.TempDir()
	writeFile(t, dir2, oldest, "a")
	writeFile(t, dir2, middle, "b")
	pinBackupClock(s2, testNow)
	if _, err := s2.Backup(context.Background(), dir2, 3); err != nil {
		t.Fatalf("boundary backup: %v", err)
	}
	if got := len(dirNames(t, dir2)); got != 3 {
		t.Errorf("dir has %d entries, want all 3 artifacts kept at the N boundary: %v", got, dirNames(t, dir2))
	}
}

// TestBackupTmpLifecycle: a crash-orphaned `.tmp` is cleaned inside the
// mutex (OB-2b); the run's OWN pre-existing tmp name is kept and collides
// on O_EXCL, failing the run without touching the foreign file; a
// cancelled copy context fails the copy (unlink path).
func TestBackupTmpLifecycle(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	orphan := "control-19990101T000000Z.db.tmp"
	writeFile(t, dir, orphan, "orphan")

	pinBackupClock(s, testNow)
	if _, err := s.Backup(context.Background(), dir, 0); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("orphan tmp survived the in-mutex cleanup (err %v)", err)
	}

	// O_EXCL collision: a pre-existing tmp under this run's own name fails
	// the run and is NOT unlinked (it was never ours).
	next := testNow.Add(time.Second)
	pinBackupClock(s, next)
	own := "control-" + next.Format("20060102T150405") + "Z.db.tmp"
	writeFile(t, dir, own, "not ours")
	_, err := s.Backup(context.Background(), dir, 0)
	if err == nil || errors.Is(err, ErrBackupInProgress) || errors.Is(err, ErrBackupExists) {
		t.Fatalf("tmp collision: err = %v, want a plain failure", err)
	}
	got, rerr := os.ReadFile(filepath.Join(dir, own))
	if rerr != nil || string(got) != "not ours" {
		t.Errorf("colliding tmp = %q, %v; want untouched", got, rerr)
	}
	if _, err := os.Stat(filepath.Join(dir, "control-"+next.Format("20060102T150405")+"Z.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("artifact created despite tmp collision (err %v)", err)
	}
}

// TestCopyContextCanceled: the copy checks its context between chunks, so
// a cancellation mid-run fails the copy (the OB-2b unlink path trigger).
func TestCopyContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copyContext(ctx, io.Discard, strings.NewReader("payload")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestBackupCancelledContextLeavesNoResidue: a Backup driven by an
// already-cancelled request context fails and leaves the backup dir empty
// — no `.tmp`, no artifact (OB-2b: unlink on EVERY failure path; the
// mid-copy trigger itself is pinned by TestCopyContextCanceled and the
// no-tmp assertions in TestBackupCountParityFailure).
func TestBackupCancelledContextLeavesNoResidue(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Backup(ctx, dir, 0); err == nil {
		t.Fatal("Backup with cancelled ctx succeeded, want error")
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("backup dir contents = %v, want empty after cancelled run", names)
	}
}

// TestBackupCheckpointFailure: a database not in WAL mode fails the
// checkpoint step in the single attempt (OB-2 step 2: the zero triple is
// REQUIRED) — a plain BACKUP_FAILED-mapped error, no artifact, no tmp.
func TestBackupCheckpointFailure(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	// Convert the DB to rollback mode on the pool's only (cached)
	// connection; wal_checkpoint then reports (0, -1, -1).
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		t.Fatalf("journal_mode=DELETE: %v", err)
	}
	if mode != "delete" {
		t.Fatalf("journal_mode = %q, want delete", mode)
	}

	_, err := s.Backup(context.Background(), dir, 0)
	if err == nil || errors.Is(err, ErrBackupInProgress) ||
		errors.Is(err, ErrBackupExists) || errors.Is(err, ErrBackupVerifyFailed) {
		t.Fatalf("non-WAL backup: err = %v, want a plain checkpoint failure", err)
	}
	if !strings.Contains(err.Error(), "WAL") {
		t.Errorf("err = %v, want the not-in-WAL-mode detail", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("backup dir has %v, want empty (no artifact, no tmp)", names)
	}
}

// TestBackupZeroLogicalWrites: a backup whose WAL was already checkpointed
// leaves the source main file bit-identical (invariant 3 observable form)
// and produces an artifact with the source's exact digest (invariant 2).
func TestBackupZeroLogicalWrites(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	dir := t.TempDir()

	pinBackupClock(s, testNow)
	if _, err := s.Backup(context.Background(), dir, 0); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	before, err := sha256File(s.path)
	if err != nil {
		t.Fatalf("sha256File(source): %v", err)
	}

	pinBackupClock(s, testNow.Add(time.Second))
	res, err := s.Backup(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	after, err := sha256File(s.path)
	if err != nil {
		t.Fatalf("sha256File(source, after): %v", err)
	}
	if before != after {
		t.Errorf("source main file changed across an already-checkpointed backup: %s -> %s", before, after)
	}
	if res.SHA256 != before {
		t.Errorf("artifact digest %s != source digest %s (invariant 2)", res.SHA256, before)
	}
}

// TestListBackups: only OB-4-matching names, newest first BY NAME; `.tmp`,
// `.failed`, and foreign files are invisible.
func TestListBackups(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	older := "control-20250101T000000Z.db"
	newer := "control-20260101T000000Z.db"
	writeFile(t, dir, older, "aa")
	writeFile(t, dir, newer, "bbbb")
	for _, name := range []string{"notes.txt", older + ".tmp", newer + ".failed"} {
		writeFile(t, dir, name, "x")
	}

	infos, err := s.ListBackups(dir)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	var names []string
	for _, in := range infos {
		names = append(names, in.Artifact)
	}
	if want := []string{newer, older}; !reflect.DeepEqual(names, want) {
		t.Fatalf("artifacts = %v, want %v (newest first BY NAME)", names, want)
	}
	if infos[0].Bytes != 4 || infos[1].Bytes != 2 {
		t.Errorf("sizes = %d, %d, want 4, 2", infos[0].Bytes, infos[1].Bytes)
	}
}
