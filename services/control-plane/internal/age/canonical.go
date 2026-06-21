// Package age implements the identity-blind age-verification server foundation
// (#1623, child A of epic #272). It accepts a client-signed claim carrying only
// booleans (valid_age, nsfw_auth) and a jurisdiction-obligation integer, verifies
// the RSA-PSS signature against the user's registered key, enforces replay/freshness,
// persists one record per user, and terminally disables the account on valid_age=false.
// No DOB, age value, jurisdiction name, or location is ever accepted or stored.
package age

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// CanonicalVersion is the only canonical signing-form version child A understands.
// It is authenticated inside the signed bytes (the "age-claim/v1" prefix + the
// canonical_version field), so a v1<->v2 downgrade cannot be forced by an unsigned field.
const CanonicalVersion = 1

// Claim-validation sentinel errors. They are intentionally coarse: the handler
// maps every one of them to a single HTTP 400 "malformed" code, so the wire
// response never reveals which field failed (no field-level validation oracle).
var (
	ErrBadUserID        = errors.New("invalid user_id")
	ErrBadObligation    = errors.New("jurisdiction_obligation out of range")
	ErrBadNonce         = errors.New("invalid nonce")
	ErrBadTimestamp     = errors.New("invalid timestamp")
	ErrBadKeyVersion    = errors.New("invalid key_version")
	ErrBadClientVersion = errors.New("invalid client_version")
	ErrBadCanonicalVer  = errors.New("unsupported canonical_version")
)

var (
	// nonce: exactly 64 lowercase-hex chars (= 32 bytes). Anchored, no \n.
	reNonce64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// client_version: 1..32 ASCII chars from [0-9A-Za-z.+-]. The class excludes
	// '\n' and '=', so a value cannot inject a canonical-form line break or
	// key=value ambiguity. Anchored to forbid leading/trailing junk.
	reClientV = regexp.MustCompile(`^[0-9A-Za-z.+\-]{1,32}$`)
)

// Claim is the signed core of an age-verification claim (child A scope).
// The forward-compat metadata fields (confidence, obligation_sources, assurance/
// attestation) are intentionally NOT part of the signed claim in child A — they are
// owned and signed by downstream children C/D/E/F.
type Claim struct {
	CanonicalVersion       int
	UserID                 string
	ValidAge               bool
	NSFWAuth               bool
	JurisdictionObligation int
	Nonce                  string
	Timestamp              int64
	KeyVersion             int
	ClientVersion          string
}

// Validate enforces the §4.2 charset/range contract. Call before CanonicalBytes.
// Every string field that flows into the canonical bytes is charset-validated here so
// no field can inject a line break ('\n') or key=value ambiguity into the signed form.
func (c Claim) Validate() error {
	if c.CanonicalVersion != CanonicalVersion {
		return ErrBadCanonicalVer
	}
	// user_id must be a lowercase RFC4122 UUID. uuid.Parse is permissive about case,
	// so the explicit lowercase check pins the canonical byte form (the signer emits lowercase).
	if _, err := uuid.Parse(c.UserID); err != nil || c.UserID != strings.ToLower(c.UserID) {
		return ErrBadUserID
	}
	if c.JurisdictionObligation < 0 || c.JurisdictionObligation > 2 {
		return ErrBadObligation
	}
	if !reNonce64.MatchString(c.Nonce) {
		return ErrBadNonce
	}
	if c.Timestamp <= 0 {
		return ErrBadTimestamp
	}
	if c.KeyVersion <= 0 {
		return ErrBadKeyVersion
	}
	if !reClientV.MatchString(c.ClientVersion) {
		return ErrBadClientVersion
	}
	return nil
}

// CanonicalBytes builds the v1 fixed-field signing string: ASCII-only, '\n'-joined,
// no trailing newline. The server NEVER parses an attacker-supplied string — it
// reconstructs these bytes from the typed, validated request fields and verifies the
// signature against them. Field order and separators are a hard contract with child B
// (locked by testdata/age-claim-canonical-v1.json).
func (c Claim) CanonicalBytes() ([]byte, error) {
	if c.CanonicalVersion != CanonicalVersion {
		return nil, ErrBadCanonicalVer
	}
	var b strings.Builder
	b.WriteString("age-claim/v1\n")
	fmt.Fprintf(&b, "canonical_version=%d\n", c.CanonicalVersion)
	fmt.Fprintf(&b, "user_id=%s\n", c.UserID)
	fmt.Fprintf(&b, "valid_age=%t\n", c.ValidAge)
	fmt.Fprintf(&b, "nsfw_auth=%t\n", c.NSFWAuth)
	fmt.Fprintf(&b, "jurisdiction_obligation=%d\n", c.JurisdictionObligation)
	fmt.Fprintf(&b, "nonce=%s\n", c.Nonce)
	fmt.Fprintf(&b, "timestamp=%d\n", c.Timestamp)
	fmt.Fprintf(&b, "key_version=%d\n", c.KeyVersion)
	fmt.Fprintf(&b, "client_version=%s", c.ClientVersion)
	return []byte(b.String()), nil
}
