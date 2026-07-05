package exchange

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestClassifyRules(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		venueCode  int
		retryAfter time.Duration
		wantClass  Class
		wantRetry  time.Duration
	}{
		{"4xx with code is definite reject", 400, -2010, 0, ClassDefiniteReject, 0},
		{"filter failure", 400, -1013, 0, ClassDefiniteReject, 0},
		{"clock skew", 400, -1021, 0, ClassDefiniteReject, 0},
		{"-2013 not found", 400, -2013, 0, ClassNotFound, 0},
		{"-2011 cancel reject not found", 400, -2011, 0, ClassNotFound, 0},
		{"429 with retry-after", 429, -1003, 3 * time.Second, ClassThrottled, 3 * time.Second},
		{"418 with retry-after", 418, -1003, 60 * time.Second, ClassThrottled, 60 * time.Second},
		{"503 with retry-after", 503, 0, 5 * time.Second, ClassThrottled, 5 * time.Second},
		{"429 without retry-after degrades", 429, -1003, 0, ClassAmbiguous, 0},
		{"500 without retry-after", 500, 0, 0, ClassAmbiguous, 0},
		{"-1007 timeout code", 400, -1007, 0, ClassAmbiguous, 0},
		{"4xx without code", 404, 0, 0, ClassAmbiguous, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			class, retry := classify(tc.status, tc.venueCode, tc.retryAfter)
			if class != tc.wantClass {
				t.Fatalf("classify(%d, %d, %s) class = %s, want %s",
					tc.status, tc.venueCode, tc.retryAfter, class, tc.wantClass)
			}
			if retry != tc.wantRetry {
				t.Fatalf("classify(%d, %d, %s) retry = %s, want %s",
					tc.status, tc.venueCode, tc.retryAfter, retry, tc.wantRetry)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	ve := &VenueError{Op: "PlaceOrder", Class: ClassThrottled, RetryAfter: time.Second}
	if got := Classify(ve); got != ClassThrottled {
		t.Fatalf("Classify(VenueError) = %s, want throttled", got)
	}
	if got := Classify(fmt.Errorf("wrapped: %w", ve)); got != ClassThrottled {
		t.Fatalf("Classify(wrapped VenueError) = %s, want throttled", got)
	}
	if got := Classify(errors.New("dial tcp: connection refused")); got != ClassAmbiguous {
		t.Fatalf("Classify(plain error) = %s, want ambiguous", got)
	}
	if got := Classify(nil); got != ClassAmbiguous {
		t.Fatalf("Classify(nil) = %s, want ambiguous", got)
	}
}

func TestVenueErrorRedaction(t *testing.T) {
	ve := &VenueError{
		Op:        "PlaceOrder",
		Class:     ClassDefiniteReject,
		VenueCode: -2010,
		VenueMsg:  "Account has insufficient balance for requested action.",
	}
	assertRedacted(t, ve.Error())
	if !strings.Contains(ve.Error(), "PlaceOrder") ||
		!strings.Contains(ve.Error(), "-2010") ||
		!strings.Contains(ve.Error(), "definite_reject") {
		t.Fatalf("Error() missing op/class/code: %q", ve.Error())
	}
}

// assertRedacted enforces the spec's Redaction bullet: no URLs, query
// strings, headers, or signatures in error strings.
func assertRedacted(t *testing.T, msg string) {
	t.Helper()
	lower := strings.ToLower(msg)
	for _, banned := range []string{"http", "?", "signature", "x-mbx"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("error string %q contains banned token %q", msg, banned)
		}
	}
}
