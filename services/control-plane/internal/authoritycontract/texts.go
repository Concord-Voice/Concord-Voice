package authoritycontract

import (
	"crypto/sha512"
	"fmt"
	"regexp"
	"strings"
)

var (
	reHex64   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reClientV = regexp.MustCompile(`^[0-9A-Za-z.+\-]{1,32}$`)
)

// AgeClaimV2 is the dsk-signed age claim of an enrolled account (§2.20, LD-29).
type AgeClaimV2 struct {
	UserID                 string
	ValidAge               bool
	NSFWAuth               bool
	JurisdictionObligation int
	Nonce                  string
	Timestamp              int64
	DeviceID               string
	ClientVersion          string
}

// AgeClaimV2Bytes builds the frozen age-claim/v2 text, or nil when a field
// would break the fixed line form. internal/age calls it; this package never
// imports internal/age.
func AgeClaimV2Bytes(c AgeClaimV2) []byte {
	if _, ok := canonicalUUID(c.UserID); !ok || !isUUIDv4(c.DeviceID) {
		return nil
	}
	if c.JurisdictionObligation < 0 || c.JurisdictionObligation > 2 || c.Timestamp <= 0 || c.Timestamp > int64(MaxSafeInt) ||
		!reHex64.MatchString(c.Nonce) || !reClientV.MatchString(c.ClientVersion) {
		return nil
	}
	return fmt.Appendf(nil, "age-claim/v2\ncanonical_version=2\nuser_id=%s\nvalid_age=%t\nnsfw_auth=%t\n"+
		"jurisdiction_obligation=%d\nnonce=%s\ntimestamp=%d\ndevice_id=%s\nclient_version=%s",
		c.UserID, c.ValidAge, c.NSFWAuth, c.JurisdictionObligation, c.Nonce, c.Timestamp, c.DeviceID, c.ClientVersion)
}

// SessionBind is the dsk-signed session binding (§2.19, D-245).
type SessionBind struct {
	RealmID, UserID, DeviceID, SessionID, Challenge string
	IssuedAtMs                                      uint64
}

// SessionBindBytes builds the frozen concord-session-bind/v1 text, or nil.
func SessionBindBytes(b SessionBind) []byte {
	for _, s := range []string{b.RealmID, b.UserID, b.SessionID} {
		if _, ok := canonicalUUID(s); !ok {
			return nil
		}
	}
	if !isUUIDv4(b.DeviceID) || !reHex64.MatchString(b.Challenge) || b.IssuedAtMs == 0 || b.IssuedAtMs > MaxSafeInt {
		return nil
	}
	return fmt.Appendf(nil, "concord-session-bind/v1\nrealm_id=%s\nuser_id=%s\ndevice_id=%s\nsession_id=%s\n"+
		"challenge=%s\nissued_at_ms=%d", b.RealmID, b.UserID, b.DeviceID, b.SessionID, b.Challenge, b.IssuedAtMs)
}

// SafetyNumber is D-221's frozen derivation: 12 zero-padded 5-digit groups,
// group i = uint40(h[5i:5i+5]) mod 100000, joined by single spaces. It returns
// "" for a non-canonical account ID or a pin digest that is not 48 bytes.
func SafetyNumber(accountID string, pinDigest []byte) string {
	if _, ok := canonicalUUID(accountID); !ok || len(pinDigest) != 48 {
		return ""
	}
	h := sha512.New()
	h.Write([]byte("concord-safety-number/v1"))
	h.Write([]byte{0})
	h.Write([]byte(accountID))
	h.Write(pinDigest)
	sum := h.Sum(nil)
	groups := make([]string, 12)
	for i := range groups {
		var v uint64
		for _, c := range sum[5*i : 5*i+5] {
			v = v<<8 | uint64(c)
		}
		groups[i] = fmt.Sprintf("%05d", v%100000)
	}
	return strings.Join(groups, " ")
}
