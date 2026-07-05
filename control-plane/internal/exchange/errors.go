package exchange

import (
	"errors"
	"fmt"
	"time"
)

// Class partitions every adapter error into exactly one of the four
// normative classes (docs/specs/live-oms-and-reconciler.md §Exchange
// adapter interface). Callers MUST branch on this classification.
type Class int

const (
	// ClassDefiniteReject: the venue processed the request and refused it
	// (HTTP 4xx carrying a Binance error code, e.g. -2010, -1013).
	ClassDefiniteReject Class = iota
	// ClassNotFound: -2013 "Order does not exist"; for cancel operations
	// -2011 "Unknown order sent" maps here too — the order is already gone.
	ClassNotFound
	// ClassThrottled: HTTP 429/418 or a 5xx maintenance response carrying
	// Retry-After. The request was NOT executed and the attempt id is not
	// poisoned; the sender resends the SAME id after RetryAfter.
	ClassThrottled
	// ClassAmbiguous: timeout, connection reset, other 5xx, -1007, or any
	// unclassifiable error — outcome unknown, resolve by query.
	ClassAmbiguous
)

func (c Class) String() string {
	switch c {
	case ClassDefiniteReject:
		return "definite_reject"
	case ClassNotFound:
		return "not_found"
	case ClassThrottled:
		return "throttled"
	case ClassAmbiguous:
		return "ambiguous"
	default:
		return fmt.Sprintf("class(%d)", int(c))
	}
}

// VenueError is the sole error shape the adapter returns. Redaction
// (normative): the message carries ONLY {operation, class, venue error code,
// venue error msg} — never request URLs, query strings, headers, or
// signatures.
type VenueError struct {
	Op         string
	Class      Class
	VenueCode  int
	VenueMsg   string
	RetryAfter time.Duration // set only for ClassThrottled
}

func (e *VenueError) Error() string {
	return fmt.Sprintf("exchange: %s: %s (venue code %d): %s", e.Op, e.Class, e.VenueCode, e.VenueMsg)
}

// Classify maps any error to its Class. A non-VenueError is Ambiguous —
// fail toward the safe path.
func Classify(err error) Class {
	var ve *VenueError
	if errors.As(err, &ve) {
		return ve.Class
	}
	return ClassAmbiguous
}

// classify applies the normative rules to one HTTP outcome: venue codes
// -2013/-2011 are NotFound and -1007 is Ambiguous regardless of transport;
// 429/418/5xx WITH Retry-After is Throttled (without it, Ambiguous); any
// other 4xx carrying a Binance code is DefiniteReject; everything else is
// Ambiguous.
func classify(status, venueCode int, retryAfter time.Duration) (Class, time.Duration) {
	switch {
	case venueCode == -2013 || venueCode == -2011:
		return ClassNotFound, 0
	case venueCode == -1007:
		return ClassAmbiguous, 0
	case status == 429 || status == 418 || status >= 500:
		if retryAfter > 0 {
			return ClassThrottled, retryAfter
		}
		return ClassAmbiguous, 0
	case status >= 400 && status < 500 && venueCode != 0:
		return ClassDefiniteReject, 0
	default:
		return ClassAmbiguous, 0
	}
}
