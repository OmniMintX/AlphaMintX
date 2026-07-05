package live

import "github.com/OmniMintX/AlphaMintX/control-plane/internal/store"

// statusRank mirrors the store's FSM rank table (spec §FSM): transitions
// are monotone in rank; rank-3 statuses are terminal and immutable.
var statusRank = map[string]int{
	"pending_new": 0, "open": 1, "partially_filled": 2,
	"filled": 3, "canceled": 3, "rejected": 3, "expired": 3,
}

// isTerminal reports whether a persisted status is rank 3.
func isTerminal(status string) bool { return statusRank[status] == 3 }

// localStatus maps a venue order status to the persisted FSM status. The
// empty result means "keep the current status": PENDING_CANCEL maps to the
// order's CURRENT non-terminal status (never a regression, never
// terminalizes by itself); unknown venue statuses are likewise kept.
func localStatus(venueStatus string) string {
	switch venueStatus {
	case "NEW":
		return "open"
	case "PARTIALLY_FILLED":
		return "partially_filled"
	case "FILLED":
		return "filled"
	case "CANCELED":
		return "canceled"
	case "REJECTED":
		return "rejected"
	case "EXPIRED", "EXPIRED_IN_MATCH":
		return "expired"
	default: // PENDING_CANCEL and anything unrecognized
		return ""
	}
}

// protectiveShaped reports whether a venue order type is protective-shaped:
// the Reconciler NEVER cancels an unattributable protective (invariant 11).
func protectiveShaped(venueType string) bool {
	switch venueType {
	case "STOP_LOSS", "STOP_LOSS_LIMIT", "TAKE_PROFIT", "TAKE_PROFIT_LIMIT":
		return true
	}
	return false
}

// advanceStatus applies one venue status report to the order FSM. The store
// mutator enforces monotone rank; a dropped regression with a differing
// payload appends stale_update_dropped (observational, after the fact).
// Returns the status now on the row.
func (o *OMS) advanceStatus(ord store.LiveOrder, venueStatus string, runID *string) (string, error) {
	mapped := localStatus(venueStatus)
	if mapped == "" {
		return ord.Status, nil
	}
	nowStatus, err := o.st.RecordOrderStatus(ord.OrderID, mapped)
	if err != nil {
		return "", err
	}
	if nowStatus != mapped && statusRank[mapped] < statusRank[nowStatus] {
		ev := o.event("stale_update_dropped", map[string]any{
			"venue_status": venueStatus, "kept_status": nowStatus,
		})
		ev.RunID = runID
		ev.StrategyID = &ord.StrategyID
		ev.Symbol = &ord.Symbol
		ev.ClientOrderID = ord.ClientOrderID
		if err := o.st.AppendOMSReconEvent(ev); err != nil {
			return "", err
		}
	}
	return nowStatus, nil
}
