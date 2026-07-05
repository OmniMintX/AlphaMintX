package live

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Tuning mirrors CONTROLPLANE_LIVE_OMS_TUNING (spec §Config): optional JSON
// with spec-defined defaults; unknown fields are rejected.
type Tuning struct {
	ReconcileIntervalSeconds   int `json:"reconcile_interval_seconds"`
	WSSilenceTimeoutSeconds    int `json:"ws_silence_timeout_seconds"`
	ListenKeyKeepaliveSeconds  int `json:"listen_key_keepalive_seconds"`
	FilterRefreshSeconds       int `json:"filter_refresh_seconds"`
	RecvWindowMS               int `json:"recv_window_ms"`
	SLPlacementDeadlineSeconds int `json:"sl_placement_deadline_seconds"`
	ReconFailureAlertThreshold int `json:"recon_failure_alert_threshold"`
	// SafetyEffectStallSeconds is the safety.Monitor stall-scan threshold
	// (safety-wiring.md §Safety-effects driver step 5); the live OMS
	// carries it only because it joins CONTROLPLANE_LIVE_OMS_TUNING.
	SafetyEffectStallSeconds int `json:"safety_effect_stall_seconds"`
}

// DefaultTuning returns the normative defaults.
func DefaultTuning() Tuning {
	return Tuning{
		ReconcileIntervalSeconds:   60,
		WSSilenceTimeoutSeconds:    300,
		ListenKeyKeepaliveSeconds:  1800,
		FilterRefreshSeconds:       86400,
		RecvWindowMS:               5000,
		SLPlacementDeadlineSeconds: 30,
		ReconFailureAlertThreshold: 3,
		SafetyEffectStallSeconds:   600,
	}
}

// ParseTuning decodes the optional tuning JSON over the defaults; unknown
// fields are rejected (DisallowUnknownFields, config.go pattern) and every
// field must be strictly positive.
func ParseTuning(raw string) (Tuning, error) {
	t := DefaultTuning()
	if strings.TrimSpace(raw) == "" {
		return t, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return Tuning{}, fmt.Errorf("live: CONTROLPLANE_LIVE_OMS_TUNING: %w", err)
	}
	for name, v := range map[string]int{
		"reconcile_interval_seconds":    t.ReconcileIntervalSeconds,
		"ws_silence_timeout_seconds":    t.WSSilenceTimeoutSeconds,
		"listen_key_keepalive_seconds":  t.ListenKeyKeepaliveSeconds,
		"filter_refresh_seconds":        t.FilterRefreshSeconds,
		"recv_window_ms":                t.RecvWindowMS,
		"sl_placement_deadline_seconds": t.SLPlacementDeadlineSeconds,
		"recon_failure_alert_threshold": t.ReconFailureAlertThreshold,
		"safety_effect_stall_seconds":   t.SafetyEffectStallSeconds,
	} {
		if v <= 0 {
			return Tuning{}, fmt.Errorf("live: CONTROLPLANE_LIVE_OMS_TUNING: %s must be > 0", name)
		}
	}
	return t, nil
}

func (t Tuning) reconcileInterval() time.Duration {
	return time.Duration(t.ReconcileIntervalSeconds) * time.Second
}

func (t Tuning) silenceTimeout() time.Duration {
	return time.Duration(t.WSSilenceTimeoutSeconds) * time.Second
}

func (t Tuning) keepaliveInterval() time.Duration {
	return time.Duration(t.ListenKeyKeepaliveSeconds) * time.Second
}

func (t Tuning) filterRefresh() time.Duration {
	return time.Duration(t.FilterRefreshSeconds) * time.Second
}

func (t Tuning) slDeadline() time.Duration {
	return time.Duration(t.SLPlacementDeadlineSeconds) * time.Second
}
