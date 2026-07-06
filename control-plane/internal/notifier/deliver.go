package notifier

// The AN-13 wire envelope, batch materialization with the 8 KiB wire
// bounds, the AN-15/AN-16 delivery attempt, and the AN-11 failure-class
// derivation. err.Error() from a delivery NEVER reaches a log: url.Error
// embeds the full URL, DNS errors carry hosts, TLS errors carry names.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// envelope is the AN-13 wire shape; Event carries raw row facts, nullable
// columns as JSON null.
type envelope struct {
	Schema      string `json:"schema"`
	Source      string `json:"source"`
	ID          string `json:"id"`
	Seq         int64  `json:"seq"`
	DeliveredAt string `json:"delivered_at"`
	Event       any    `json:"event"`
}

// item is one materialized source row ready for delivery.
type item struct {
	rowid int64
	id    string
	event any
}

// loadBatch materializes one source's pending rows (rowid > after, rowid
// ASC, at most maxPerTick — AN-2/AN-6), applying the AN-13 wire bounds to
// the operator/venue-supplied text columns. The DB rows stay complete.
func (e *Engine) loadBatch(src string, after int64) ([]item, error) {
	switch src {
	case store.AlertSourceKillBreaker:
		rows, err := e.st.ListKillBreakerEventsAfter(after, e.maxPerTick)
		if err != nil {
			return nil, err
		}
		out := make([]item, 0, len(rows))
		for _, r := range rows {
			ev := r.KillBreakerEvent
			ev.TriggerRef = truncatePtr(ev.TriggerRef)
			out = append(out, item{r.Rowid, ev.EventID, ev})
		}
		return out, nil
	case store.AlertSourceKillClear:
		rows, err := e.st.ListKillClearEventsAfter(after, e.maxPerTick)
		if err != nil {
			return nil, err
		}
		out := make([]item, 0, len(rows))
		for _, r := range rows {
			ev := r.KillClearEvent
			ev.Reason = truncate(ev.Reason)
			out = append(out, item{r.Rowid, ev.ClearID, ev})
		}
		return out, nil
	case store.AlertSourceSafetyAlert:
		rows, err := e.st.ListSafetyAlertsAfter(after, e.maxPerTick)
		if err != nil {
			return nil, err
		}
		out := make([]item, 0, len(rows))
		for _, r := range rows {
			a := r.SafetyAlert
			a.DetailsJSON = truncate(a.DetailsJSON)
			out = append(out, item{r.Rowid, a.AlertID, a})
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown alert source %q", src)
}

// truncate bounds a wire string at 8 KiB plus the marker suffix (AN-13);
// the truncated value then may not parse as JSON, which is accepted.
func truncate(s string) string {
	if len(s) <= truncateBytes {
		return s
	}
	return s[:truncateBytes] + truncateSuffix
}

func truncatePtr(s *string) *string {
	if s == nil {
		return nil
	}
	t := truncate(*s)
	return &t
}

// deliver sends one envelope: a webhook POST (AN-15/AN-16) or the AN-14
// log-only marker line. On failure class is the AN-11 derived class and
// status is nonzero for HTTP status failures (3xx/4xx/5xx).
func (e *Engine) deliver(ctx context.Context, env envelope) (class string, status int, ok bool) {
	body, err := json.Marshal(env)
	if err != nil {
		return "other", 0, false
	}
	if e.logOnly {
		e.events.Printf("SAFETY-EVENT %s", body)
		return "", 0, true
	}
	rctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return "other", 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	if e.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+e.bearer)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return classify(err), 0, false
	}
	defer resp.Body.Close()
	// AN-16: bounded read, discarded, never parsed, never logged.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyRead))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "", resp.StatusCode, true
	}
	return fmt.Sprintf("status:%d", resp.StatusCode), resp.StatusCode, false
}

// classify derives the AN-11 failure class from a transport error.
func classify(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var certErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError
	if errors.As(err, &certErr) || errors.As(err, &recordErr) ||
		errors.As(err, &hostErr) || errors.As(err, &authErr) {
		return "tls"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return "connect"
	}
	if errors.Is(err, http.ErrUseLastResponse) {
		return "redirect"
	}
	return "other"
}
