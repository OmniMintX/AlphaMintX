package live

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// newUUID mints one v4 UUID (order ids, fill ids, event ids, run ids).
func newUUID() string { return uuid.NewString() }

// event builds one oms_recon_events row. Callers set the identity columns
// (RunID, StrategyID, Symbol, ClientOrderID, ...) before appending.
func (o *OMS) event(kind string, details map[string]any) store.OMSReconEvent {
	body := "{}"
	if len(details) > 0 {
		if b, err := json.Marshal(details); err == nil {
			body = string(b)
		}
	}
	return store.OMSReconEvent{
		EventID:     uuid.NewString(),
		Kind:        kind,
		DetailsJSON: body,
		RecordedAt:  formatTime(o.now()),
	}
}

// venueErrDetails renders an adapter error for details_json under the
// normative redaction rule: ONLY {operation, venue error code, venue error
// msg} — never URLs, query strings, headers, or signatures. Non-VenueError
// errors carry only their class (transport error text may embed URLs).
func venueErrDetails(err error) map[string]any {
	var ve *exchange.VenueError
	if errors.As(err, &ve) {
		return map[string]any{
			"operation": ve.Op, "venue_code": ve.VenueCode, "venue_msg": ve.VenueMsg,
		}
	}
	return map[string]any{"error_class": exchange.Classify(err).String()}
}
