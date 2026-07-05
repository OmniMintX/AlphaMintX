package live

import (
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
)

// clientOrderIDRe pins the platform namespace: an order at the venue is
// OURS iff its clientOrderId matches (spec §clientOrderId namespace).
var clientOrderIDRe = regexp.MustCompile(`^amx1-[0-9A-Za-z_-]{22}-[0-9]$`)

// inNamespace reports whether a venue clientOrderId belongs to the
// platform's amx1 namespace.
func inNamespace(id string) bool { return clientOrderIDRe.MatchString(id) }

// newIntentToken draws 16 CSPRNG bytes and encodes them as 22 chars of
// unpadded base64url — the intent token shared by an intent's attempts.
func (o *OMS) newIntentToken() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(o.tokens, b[:]); err != nil {
		return "", fmt.Errorf("live: intent token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// attemptID renders "amx1-<token22>-<attempt>" (total length 29 <= 36;
// attempt is a single digit 0..9, enforced by the retry ceiling).
func attemptID(token string, attempt int) string {
	return fmt.Sprintf("amx1-%s-%d", token, attempt)
}
