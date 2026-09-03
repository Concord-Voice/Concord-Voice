// Package mediaproof authenticates internal media-plane hops to the control
// plane, independently of the end-user JWT the hop carries.
//
// A hop presents an HMAC over the canonical fields of the request it is making,
// keyed by a domain-separated derivation of the shared JWT secret. A member
// possesses their own bearer token but not that secret, so they cannot forge a
// hop; binding the bearer token's digest means a proof observed on the service
// network cannot be replayed with a different user's token; and the timestamp
// bounds replay to a narrow window.
//
// Each caller supplies its OWN version and context strings. Domain separation
// is what keeps a proof minted for one purpose from being replayable as another
// even though both derive from the same secret, so neither may be shared
// between distinct authorization decisions. The version is per-caller for the
// same reason: these are independent wire formats that must be able to move
// separately, and a package-global would re-version every caller at once.
//
// The `_, _ = mac.Write(...)` discards below are the documented-safe case, not
// oversights: `hash.Hash.Write` "never returns an error" per the stdlib
// contract, so the error is structurally unreachable and checking it would add
// a branch no test could cover. `[internal]rules/backend.md` makes review the
// enforcement for blank discards, so this note is that enforcement discharged
// once for the package rather than re-derived at each call.
package mediaproof

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// MaxClockSkew bounds how far a proof's timestamp may sit from the verifier's
// clock in either direction.
const MaxClockSkew = 30 * time.Second

// hexProofLen is the hex-encoded width of a SHA-256 MAC.
const hexProofLen = sha256.Size * 2

// DeriveKey produces the signing key for one context from the shared secret.
// Returns nil for an empty secret so a misconfigured deployment cannot produce
// a usable key — Sign and Verify both treat a nil key as never-valid.
func DeriveKey(sharedSecret, purpose string) []byte {
	if sharedSecret == "" {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(sharedSecret))
	_, _ = mac.Write([]byte(purpose))
	return mac.Sum(nil)
}

// TokenDigest is the canonical bearer-token binding: a hex SHA-256 of the
// token, never the token itself, so a proof carries no reusable credential.
func TokenDigest(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return hex.EncodeToString(sum[:])
}

// payload joins the canonical prefix and the caller's fields into the exact
// bytes that get signed, and reports whether they are signable.
//
// Fields are newline-separated, so a field containing a newline could shift the
// boundary between two fields and let two different field lists produce one
// payload. An earlier revision argued no caller could supply one — that was
// FALSE: `c.Request.URL.Path` is percent-decoded, so `%0A` in a request line
// yields a literal newline. Enforcing it is cheap and survives the next caller;
// arguing about provenance does not.
func payload(version, timestamp string, fields []string) ([]byte, bool) {
	all := make([]string, 0, len(fields)+2)
	all = append(all, version, timestamp)
	all = append(all, fields...)
	for _, f := range all {
		if strings.Contains(f, "\n") {
			return nil, false
		}
	}
	return []byte(strings.Join(all, "\n")), true
}

// Sign returns the hex-encoded proof for fields under key, or "" when the proof
// cannot be produced (nil key, or a field carrying a newline).
//
// Callers pass only their OWN fields: the version and timestamp form a
// canonical prefix this package prepends, so the timestamp cannot be
// skew-checked against one value and signed as another.
func Sign(key []byte, version, timestamp string, fields ...string) string {
	if len(key) == 0 {
		return ""
	}
	body, ok := payload(version, timestamp, fields)
	if !ok {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether proofHex authenticates fields under key, with
// timestamp inside MaxClockSkew of now.
//
// Fail-closed at every step: a nil key (unconfigured secret), an unparseable
// timestamp, a stale or future timestamp, a malformed proof, a field carrying a
// newline, or a mismatch all return false. The comparison is constant-time.
//
// The timestamp is passed ONCE and is both skew-checked and signed, so a caller
// cannot check one value and bind another.
func Verify(key []byte, proofHex, version, timestamp string, fields ...string) bool {
	if len(key) == 0 {
		return false
	}
	// Length before decode: rejects an oversized proof without allocating.
	if len(proofHex) != hexProofLen {
		return false
	}
	unixSeconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	skew := time.Since(time.Unix(unixSeconds, 0))
	if skew < -MaxClockSkew || skew > MaxClockSkew {
		return false
	}
	provided, err := hex.DecodeString(proofHex)
	if err != nil {
		return false
	}
	body, ok := payload(version, timestamp, fields)
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}
