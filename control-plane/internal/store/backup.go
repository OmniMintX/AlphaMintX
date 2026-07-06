package store

// Online backup of control.db (docs/specs/ops-backup.md OB-1..OB-9): a
// byte-copy of the main DB file taken while the pool's only connection is
// held and the WAL is fully checkpointed, double-digested (OB-5a), then
// verified read-only with immutable=1 (OB-5). VACUUM is FORBIDDEN on the
// source and on any artifact (OB-1: implicit rowids are load-bearing).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrBackupInProgress: another backup holds the engine mutex; the request
// fails immediately, it never queues (ops-backup.md OB-6a, 409).
var ErrBackupInProgress = errors.New("BACKUP_IN_PROGRESS")

// ErrBackupExists: the target artifact name already exists — two backups
// within the same second; the operator retries (ops-backup.md OB-4, 409).
var ErrBackupExists = errors.New("BACKUP_EXISTS")

// ErrBackupVerifyFailed: OB-5/OB-5a verification failed; the artifact was
// renamed `<name>.failed` for forensics (ops-backup.md OB-2 step 6, 500).
var ErrBackupVerifyFailed = errors.New("BACKUP_VERIFY_FAILED")

// backupNamePattern is the exact OB-4 artifact name; ONLY matching files
// are considered by retention and the list endpoint. Fixed width, so
// lexicographic order == chronological order (the normative ordering key).
var backupNamePattern = regexp.MustCompile(`^control-[0-9]{8}T[0-9]{6}Z\.db$`)

// BackupResult carries the OB-6 success response fields.
type BackupResult struct {
	Artifact   string
	Bytes      int64
	SHA256     string
	Tables     int
	RowsTotal  int64
	StartedAt  time.Time
	FinishedAt time.Time
	Verified   bool
}

// BackupInfo is one OB-7 list entry.
type BackupInfo struct {
	Artifact   string
	Bytes      int64
	ModifiedAt time.Time
}

// tableCount is one source-fingerprint entry (OB-2 step 3): per-table row
// count under the pinned predicate, ordered by name.
type tableCount struct {
	name  string
	count int64
}

// Backup takes one verified snapshot of the DB into dir (ops-backup.md
// OB-2..OB-5a) and, on success with retain >= 1, applies retention (OB-9).
// It runs entirely inside the engine mutex; a concurrent call returns
// ErrBackupInProgress immediately.
func (s *Store) Backup(ctx context.Context, dir string, retain int) (BackupResult, error) {
	if !s.backupMu.TryLock() {
		return BackupResult{}, ErrBackupInProgress
	}
	defer s.backupMu.Unlock()

	now := time.Now
	if s.backupNow != nil {
		now = s.backupNow
	}
	started := now().UTC()
	name := "control-" + started.Format("20060102T150405") + "Z.db"
	final := filepath.Join(dir, name)
	tmp := final + ".tmp"
	if _, err := os.Lstat(final); err == nil {
		return BackupResult{}, ErrBackupExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return BackupResult{}, fmt.Errorf("stat artifact target: %w", err)
	}

	// OB-2b in-mutex cleanup of crash-orphaned `.tmp` files. This run's own
	// name is kept so the O_EXCL collision below still fails the run.
	cleanupOrphanTmp(dir, name+".tmp")

	fingerprint, sha, written, err := s.copySnapshot(ctx, tmp)
	if err != nil {
		return BackupResult{}, err
	}

	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return BackupResult{}, fmt.Errorf("rename artifact: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return BackupResult{Artifact: name}, fmt.Errorf("fsync backup dir: %w", err)
	}

	// OB-5a: re-read the renamed artifact and require the copy digest,
	// BEFORE any SQLite handle opens on it.
	reread, err := sha256File(final)
	if err != nil {
		return BackupResult{Artifact: name}, failVerify(final, name, fmt.Errorf("re-read artifact: %w", err))
	}
	if reread != sha {
		return BackupResult{Artifact: name}, failVerify(final, name,
			fmt.Errorf("copy digest %s != artifact digest %s", sha, reread))
	}
	verify := s.backupVerify
	if verify == nil {
		verify = verifyBackupArtifact
	}
	if err := verify(final, fingerprint); err != nil {
		return BackupResult{Artifact: name}, failVerify(final, name, err)
	}

	// OB-9: retention runs only after a SUCCESSFUL backup, inside the
	// engine mutex, ordered BY NAME.
	if retain >= 1 {
		if err := applyRetention(dir, retain); err != nil {
			return BackupResult{Artifact: name}, fmt.Errorf("retention: %w", err)
		}
	}

	var rows int64
	for _, tc := range fingerprint {
		rows += tc.count
	}
	return BackupResult{
		Artifact: name, Bytes: written, SHA256: sha,
		Tables: len(fingerprint), RowsTotal: rows,
		StartedAt: started, FinishedAt: now().UTC(), Verified: true,
	}, nil
}

// copySnapshot holds the pool's ONLY connection for OB-2 steps 2-4:
// checkpoint TRUNCATE (require the zero triple), fingerprint, byte-copy the
// raw path to tmp with a streaming SHA-256, fsync the file, then release
// the connection — verification never runs under the hold (OB-2a). The tmp
// file is unlinked on every failure path where this run created it (OB-2b).
func (s *Store) copySnapshot(ctx context.Context, tmp string) (fp []tableCount, sha string, written int64, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, "", 0, fmt.Errorf("checkout connection: %w", err)
	}
	holdStart := time.Now()
	defer func() {
		_ = conn.Close()
		if hold := time.Since(holdStart); hold > 5*time.Second {
			log.Printf("WARNING: backup held the DB connection for %s (> 5s, ops-backup.md OB-2a)", hold)
		}
	}()

	var busy, walFrames, checkpointed int
	if err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").
		Scan(&busy, &walFrames, &checkpointed); err != nil {
		return nil, "", 0, fmt.Errorf("wal_checkpoint(TRUNCATE): %w", err)
	}
	if busy != 0 || walFrames != 0 || checkpointed != 0 {
		if walFrames == -1 && checkpointed == -1 {
			return nil, "", 0, fmt.Errorf(
				"wal_checkpoint(TRUNCATE) returned (%d, %d, %d): database is not in WAL mode",
				busy, walFrames, checkpointed)
		}
		return nil, "", 0, fmt.Errorf(
			"wal_checkpoint(TRUNCATE) returned (%d, %d, %d), want (0, 0, 0)",
			busy, walFrames, checkpointed)
	}

	fp, err = fingerprintTables(ctx, conn)
	if err != nil {
		return nil, "", 0, fmt.Errorf("fingerprint: %w", err)
	}

	src, err := os.Open(s.path)
	if err != nil {
		return nil, "", 0, fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	// O_EXCL: a pre-existing tmp of the same name fails the run, never
	// silently clobbered — and is then NOT ours to unlink.
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, "", 0, fmt.Errorf("create tmp artifact: %w", err)
	}
	h := sha256.New()
	// DS-1: the user_version stamp substitutes bytes 60-63 IN-STREAM, so
	// the tmp file and the streaming digest both see the stamped bytes —
	// OB-5a's copy-digest == re-read-digest property holds verbatim.
	written, err = copyContext(ctx, &stampWriter{w: io.MultiWriter(dst, h)}, src)
	if err == nil {
		err = dst.Sync()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return nil, "", 0, fmt.Errorf("copy to tmp artifact: %w", err)
	}
	return fp, hex.EncodeToString(h.Sum(nil)), written, nil
}

// stampedUserVersion is the DS-1 restore-detection stamp: every engine
// artifact carries user_version = 1 in the SQLite header (bytes 60-63,
// 4-byte big-endian), so a DB booted from an artifact engages the restore
// gate by construction (deploy-and-survive.md D1). The stamp never goes
// through a SQLite connection and never touches the live DB.
const stampedUserVersion = 1

// userVersionOffset is the SQLite header offset of the user_version field.
const userVersionOffset = 60

// stampWriter substitutes the DS-1 user_version bytes as they pass
// through the copy loop — one write pass, no post-copy WriteAt. The
// caller's buffer is never mutated (io.Writer contract): the overlapping
// chunk is cloned before patching.
type stampWriter struct {
	w   io.Writer
	off int64 // absolute artifact offset of the next incoming byte
}

func (sw *stampWriter) Write(p []byte) (int, error) {
	if sw.off < userVersionOffset+4 && sw.off+int64(len(p)) > userVersionOffset {
		p = append([]byte(nil), p...)
		var stamp [4]byte
		binary.BigEndian.PutUint32(stamp[:], stampedUserVersion)
		for i := int64(0); i < 4; i++ {
			if idx := userVersionOffset + i - sw.off; idx >= 0 && idx < int64(len(p)) {
				p[idx] = stamp[i]
			}
		}
	}
	n, err := sw.w.Write(p)
	sw.off += int64(n)
	return n, err
}

// failVerify renames the artifact to `<name>.failed` (kept for forensics)
// and wraps the detail under the ErrBackupVerifyFailed sentinel.
func failVerify(final, name string, detail error) error {
	if err := os.Rename(final, final+".failed"); err != nil {
		return fmt.Errorf("%w: %s: %v (rename to .failed also failed: %v)",
			ErrBackupVerifyFailed, name, detail, err)
	}
	return fmt.Errorf("%w: %s: %v", ErrBackupVerifyFailed, name, detail)
}

// verifyBackupArtifact is the OB-5 check over a SEPARATE read-only
// immutable handle: integrity_check exactly one row "ok",
// foreign_key_check zero rows, per-table counts equal to the source
// fingerprint. immutable=1 creates no sidecars and locks nothing.
func verifyBackupArtifact(path string, fingerprint []tableCount) error {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?mode=ro&immutable=1")
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()

	var results []string
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return fmt.Errorf("integrity_check: %w", err)
		}
		results = append(results, line)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	if len(results) != 1 || results[0] != "ok" {
		return fmt.Errorf("integrity_check returned %q, want exactly one row \"ok\"", results)
	}

	violations, err := countRows(ctx, db, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("foreign_key_check returned %d rows, want zero", violations)
	}

	got, err := fingerprintTables(ctx, db)
	if err != nil {
		return fmt.Errorf("artifact fingerprint: %w", err)
	}
	if len(got) != len(fingerprint) {
		return fmt.Errorf("artifact has %d user tables, source fingerprint has %d", len(got), len(fingerprint))
	}
	for i, want := range fingerprint {
		if got[i] != want {
			return fmt.Errorf("count parity: artifact table %s has %d rows, source table %s has %d",
				got[i].name, got[i].count, want.name, want.count)
		}
	}
	return nil
}

// rowQuerier is the query surface shared by *sql.Conn (source fingerprint,
// on the held connection) and *sql.DB (artifact verification).
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// fingerprintTables collects per-table row counts for every user table —
// pinned predicate type='table' AND name NOT LIKE 'sqlite_%', ordered by
// name (ops-backup.md OB-2 step 3).
func fingerprintTables(ctx context.Context, q rowQuerier) ([]tableCount, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	fp := make([]tableCount, 0, len(names))
	for _, name := range names {
		var count int64
		quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoted).Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", name, err)
		}
		fp = append(fp, tableCount{name: name, count: count})
	}
	return fp, nil
}

// countRows counts result rows without scanning columns.
func countRows(ctx context.Context, db *sql.DB, query string) (int, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// copyContext streams src to dst, checking ctx between chunks so a
// request-context cancellation mid-copy fails the run (OB-2b).
func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 1<<20)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			wn, werr := dst.Write(buf[:n])
			written += int64(wn)
			if werr != nil {
				return written, werr
			}
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, rerr
		}
	}
}

// sha256File digests a whole file.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// syncDir fsyncs the backup directory so the rename is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// cleanupOrphanTmp removes crash-orphaned `control-*.db.tmp` files (OB-2b
// MAY, inside the engine mutex); keep — the current run's own tmp name —
// is skipped so the O_EXCL collision check stays meaningful.
func cleanupOrphanTmp(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == keep {
			continue
		}
		if strings.HasPrefix(name, "control-") && strings.HasSuffix(name, ".db.tmp") {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// listBackupNames returns the OB-4-matching artifact names in dir, newest
// first BY NAME (lexicographic == chronological; mtime is never the key).
func listBackupNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && backupNamePattern.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// applyRetention deletes OB-4-matching artifacts beyond the newest retain,
// ordered BY NAME; `.tmp`, `.failed`, and foreign names are never touched.
func applyRetention(dir string, retain int) error {
	names, err := listBackupNames(dir)
	if err != nil {
		return err
	}
	if len(names) <= retain {
		return nil
	}
	for _, name := range names[retain:] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// ListBackups lists the OB-4-matching artifacts in dir, newest first BY
// NAME (ops-backup.md OB-7).
func (s *Store) ListBackups(dir string) ([]BackupInfo, error) {
	names, err := listBackupNames(dir)
	if err != nil {
		return nil, err
	}
	out := make([]BackupInfo, 0, len(names))
	for _, name := range names {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		out = append(out, BackupInfo{Artifact: name, Bytes: info.Size(), ModifiedAt: info.ModTime().UTC()})
	}
	return out, nil
}
