package contract

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/shopspring/decimal"
)

// Decimal-as-string per ADR-0003: money/price/size fields are decimal strings,
// never JSON numbers. Unmarshal parses into shopspring decimal and keeps the
// original string so marshaling round-trips byte-identically.
var (
	decimalPattern       = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)
	signedDecimalPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)
	timestampPattern     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$`)
)

// Decimal is a non-negative decimal string (contract $defs/decimal).
type Decimal struct {
	dec decimal.Decimal
	raw string
}

// NewDecimal wraps a shopspring decimal; it marshals as d.String().
func NewDecimal(d decimal.Decimal) Decimal {
	return Decimal{dec: d, raw: d.String()}
}

// ParseDecimal parses and validates the unsigned decimal-string form.
func ParseDecimal(s string) (Decimal, error) {
	if len(s) > 34 || !decimalPattern.MatchString(s) {
		return Decimal{}, fmt.Errorf("invalid decimal string %q", s)
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Decimal{}, err
	}
	return Decimal{dec: d, raw: s}, nil
}

func (d Decimal) Decimal() decimal.Decimal { return d.dec }
func (d Decimal) String() string           { return d.raw }

func (d Decimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.raw)
}

func (d *Decimal) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("decimal field must be a JSON string: %w", err)
	}
	parsed, err := ParseDecimal(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// SignedDecimal is a possibly-negative decimal string
// (contract $defs/signed_decimal; only daily_realized_pnl_quote in v1).
type SignedDecimal struct {
	dec decimal.Decimal
	raw string
}

// NewSignedDecimal wraps a shopspring decimal; it marshals as d.String().
func NewSignedDecimal(d decimal.Decimal) SignedDecimal {
	return SignedDecimal{dec: d, raw: d.String()}
}

// ParseSignedDecimal parses and validates the signed decimal-string form.
func ParseSignedDecimal(s string) (SignedDecimal, error) {
	if len(s) > 35 || !signedDecimalPattern.MatchString(s) {
		return SignedDecimal{}, fmt.Errorf("invalid signed decimal string %q", s)
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return SignedDecimal{}, err
	}
	return SignedDecimal{dec: d, raw: s}, nil
}

func (d SignedDecimal) Decimal() decimal.Decimal { return d.dec }
func (d SignedDecimal) String() string           { return d.raw }

func (d SignedDecimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.raw)
}

func (d *SignedDecimal) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("signed decimal field must be a JSON string: %w", err)
	}
	parsed, err := ParseSignedDecimal(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// UTCTime is an RFC 3339 UTC timestamp with mandatory Z suffix
// (contract $defs/utc_timestamp). The raw string is preserved for round-trip.
type UTCTime struct {
	t   time.Time
	raw string
}

// NewUTCTime wraps a time; it marshals in RFC 3339 UTC "Z" form.
func NewUTCTime(t time.Time) UTCTime {
	u := t.UTC()
	return UTCTime{t: u, raw: u.Format("2006-01-02T15:04:05Z")}
}

// ParseUTCTime parses and validates the timestamp string form.
func ParseUTCTime(s string) (UTCTime, error) {
	if len(s) > 35 || !timestampPattern.MatchString(s) {
		return UTCTime{}, fmt.Errorf("invalid UTC timestamp %q", s)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return UTCTime{}, err
	}
	return UTCTime{t: t, raw: s}, nil
}

func (u UTCTime) Time() time.Time { return u.t }
func (u UTCTime) String() string  { return u.raw }

func (u UTCTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(u.raw)
}

func (u *UTCTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("timestamp field must be a JSON string: %w", err)
	}
	parsed, err := ParseUTCTime(s)
	if err != nil {
		return err
	}
	*u = parsed
	return nil
}
