package api

import (
	"net/http"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/notifier"
)

// NotifierStatusProvider is the alert-dispatch health seam (the
// BackupEngine pattern): Status snapshots the notifier's per-source AN-17
// consecutive-failure state (alert-notifier.md). *notifier.Engine
// satisfies it; main.go wires it iff CONTROLPLANE_ALERT_WEBHOOK is
// configured — nil leaves the route unregistered (404).
type NotifierStatusProvider interface {
	Status() notifier.Status
}

// notifierSource is one per-source row of the status response.
type notifierSource struct {
	Source                 string  `json:"source"`
	ConsecutiveFailedTicks int     `json:"consecutive_failed_ticks"`
	Degraded               bool    `json:"degraded"`
	LastDegradedAt         *string `json:"last_degraded_at"`
}

type notifierStatusResponse struct {
	Degraded bool             `json:"degraded"`
	Sources  []notifierSource `json:"sources"`
}

// handleGetNotifierStatus is GET /api/v1/ops/notifier-status: env-admin
// ONLY (the GET /api/v1/ops/backups tier — platform operational data,
// never a tenant surface); sources never null.
func (s *Server) handleGetNotifierStatus(w http.ResponseWriter, _ *http.Request) {
	st := s.cfg.Notifier.Status()
	sources := make([]notifierSource, 0, len(st.Sources))
	for _, src := range st.Sources {
		row := notifierSource{
			Source:                 src.Source,
			ConsecutiveFailedTicks: src.ConsecutiveFailedTicks,
			Degraded:               src.Degraded,
		}
		if !src.LastDegradedAt.IsZero() {
			ts := formatTime(src.LastDegradedAt)
			row.LastDegradedAt = &ts
		}
		sources = append(sources, row)
	}
	writeJSON(w, http.StatusOK, notifierStatusResponse{Degraded: st.Degraded, Sources: sources})
}
