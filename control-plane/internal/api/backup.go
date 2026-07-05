package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// BackupEngine is the ops-backup seam (ops-backup.md §Wiring seams, the
// ReconStatusProvider pattern): Run takes one verified snapshot and List
// enumerates OB-4-matching artifacts newest first BY NAME. main.go wires it
// iff CONTROLPLANE_BACKUP_DIR is configured; nil leaves both routes
// unregistered (404, invariant 6). Mode-independent: paper deployments
// configure and run backups too (OB-6).
type BackupEngine interface {
	Run(ctx context.Context) (store.BackupResult, error)
	List() ([]store.BackupInfo, error)
}

// backupRunResponse is the exact OB-6 success shape.
type backupRunResponse struct {
	Artifact   string `json:"artifact"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	Tables     int    `json:"tables"`
	RowsTotal  int64  `json:"rows_total"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Verified   bool   `json:"verified"`
}

// backupItem is one OB-7 list entry; only the artifact basename is ever
// echoed, never a filesystem path (OB-13).
type backupItem struct {
	Artifact   string `json:"artifact"`
	Bytes      int64  `json:"bytes"`
	ModifiedAt string `json:"modified_at"`
}

type listBackupsResponse struct {
	Items []backupItem `json:"items"`
}

// handlePostBackupRun is POST /api/v1/ops/backups/run (ops-backup.md OB-6):
// env-admin ONLY, empty request body (never parsed). The two 500s
// deliberately bypass the uniform INTERNAL envelope and carry the specific
// codes; their messages name at most the artifact basename, never a path.
// The trigger evidence is this handler's log line plus the HTTP response —
// deliberately NOT a DB row (OB-3).
func (s *Server) handlePostBackupRun(w http.ResponseWriter, r *http.Request) {
	res, err := s.cfg.Backup.Run(r.Context())
	switch {
	case errors.Is(err, store.ErrBackupInProgress):
		writeError(w, http.StatusConflict, codeBackupInProgress,
			"a backup is already in progress; it never queues — retry after it finishes")
	case errors.Is(err, store.ErrBackupVerifyFailed):
		s.cfg.Logf("api: backup verify failed: %v", err)
		writeError(w, http.StatusInternalServerError, codeBackupVerifyFailed,
			"artifact verification failed; artifact renamed "+res.Artifact+".failed")
	case errors.Is(err, store.ErrBackupExists):
		writeError(w, http.StatusConflict, codeBackupExists,
			"an artifact with the target name already exists (same-second backup); retry")
	case err != nil:
		s.cfg.Logf("api: backup failed: %v", err)
		writeError(w, http.StatusInternalServerError, codeBackupFailed, "backup failed")
	default:
		s.cfg.Logf("api: backup by %s: artifact %s sha256 %s bytes %d duration %s",
			s.actorID(principalFrom(r)), res.Artifact, res.SHA256, res.Bytes,
			res.FinishedAt.Sub(res.StartedAt))
		writeJSON(w, http.StatusOK, backupRunResponse{
			Artifact: res.Artifact, Bytes: res.Bytes, SHA256: res.SHA256,
			Tables: res.Tables, RowsTotal: res.RowsTotal,
			StartedAt: formatTime(res.StartedAt), FinishedAt: formatTime(res.FinishedAt),
			Verified: res.Verified,
		})
	}
}

// handleListBackups is GET /api/v1/ops/backups (ops-backup.md OB-7):
// env-admin ONLY, newest first BY NAME, items never null. Read-only; the
// guard charges non-GET requests only, so no rate bucket is charged.
func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	infos, err := s.cfg.Backup.List()
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	items := make([]backupItem, 0, len(infos))
	for _, in := range infos {
		items = append(items, backupItem{
			Artifact: in.Artifact, Bytes: in.Bytes, ModifiedAt: formatTime(in.ModifiedAt),
		})
	}
	writeJSON(w, http.StatusOK, listBackupsResponse{Items: items})
}
