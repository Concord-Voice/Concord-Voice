package authoritycontract

import (
	"crypto/sha512"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	userA    = "0f8fad5b-d9cb-469f-a165-70867728950e"
	deviceA  = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	realmA   = "b2c1d4e5-3f6a-4781-9a0b-c1d2e3f4a5b6"
	sessionA = "5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d"
	nonceA   = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff" // pragma: allowlist secret
)

func validAgeClaim() AgeClaimV2 {
	return AgeClaimV2{UserID: userA, ValidAge: true, NSFWAuth: false, JurisdictionObligation: 2,
		Nonce: nonceA, Timestamp: 1700000000, DeviceID: deviceA, ClientVersion: "0.2.48"}
}

// validSessionBind gives each UUID position its own value, so the expected text
// pins every position and an argument swap in the frozen text cannot hide.
func validSessionBind() SessionBind {
	return SessionBind{RealmID: realmA, UserID: userA, DeviceID: deviceA, SessionID: sessionA,
		Challenge: nonceA, IssuedAtMs: 1700000000123}
}

func TestAgeClaimV2Bytes(t *testing.T) {
	want := "age-claim/v2\ncanonical_version=2\nuser_id=" + userA + "\nvalid_age=true\nnsfw_auth=false\n" +
		"jurisdiction_obligation=2\nnonce=" + nonceA + "\ntimestamp=1700000000\ndevice_id=" + deviceA +
		"\nclient_version=0.2.48"
	require.Equal(t, want, string(AgeClaimV2Bytes(validAgeClaim())))

	for name, mut := range map[string]func(*AgeClaimV2){
		"uppercase user":  func(c *AgeClaimV2) { c.UserID = strings.ToUpper(userA) },
		"obligation 3":    func(c *AgeClaimV2) { c.JurisdictionObligation = 3 },
		"nonce uppercase": func(c *AgeClaimV2) { c.Nonce = strings.ToUpper(nonceA) },
		"timestamp 0":     func(c *AgeClaimV2) { c.Timestamp = 0 },
		"LF in version":   func(c *AgeClaimV2) { c.ClientVersion = "1\nuser_id=x" },
		"v1 device id":    func(c *AgeClaimV2) { c.DeviceID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8" },
	} {
		c := validAgeClaim()
		mut(&c)
		require.Nil(t, AgeClaimV2Bytes(c), name)
	}
}

func TestSessionBindBytes(t *testing.T) {
	b := validSessionBind()
	want := "concord-session-bind/v1\nrealm_id=" + realmA + "\nuser_id=" + userA + "\ndevice_id=" + deviceA +
		"\nsession_id=" + sessionA + "\nchallenge=" + nonceA + "\nissued_at_ms=1700000000123"
	require.Equal(t, want, string(SessionBindBytes(b)))
	bad := b
	bad.Challenge = nonceA[:63]
	require.Nil(t, SessionBindBytes(bad))
	bad = b
	bad.IssuedAtMs = MaxSafeInt + 1
	require.Nil(t, SessionBindBytes(bad))
}

func TestSafetyNumber(t *testing.T) {
	// SHA-384 of "pin": a 48-byte pin digest without Task 3's digestOf helper.
	pinSum := sha512.Sum384([]byte("pin"))
	pin := pinSum[:]
	got := SafetyNumber(userA, pin)
	require.Len(t, got, 71)
	require.Regexp(t, `^[0-9]{5}( [0-9]{5}){11}$`, got)

	// Independent recomputation with math/big.
	h := sha512.Sum512(append(append([]byte("concord-safety-number/v1\x00"), userA...), pin...))
	var groups []string
	for i := 0; i < 12; i++ {
		v := new(big.Int).SetBytes(h[5*i : 5*i+5])
		groups = append(groups, fmt.Sprintf("%05d", new(big.Int).Mod(v, big.NewInt(100000)).Int64()))
	}
	require.Equal(t, strings.Join(groups, " "), got)

	require.Empty(t, SafetyNumber(strings.ToUpper(userA), pin))
	require.Empty(t, SafetyNumber(userA, pin[:47]))
}

func TestCanonicalTextsAreNotEN1Messages(t *testing.T) {
	// EN2 forbids LF, so none of the texts can be presented as a message (§2.1).
	for _, text := range [][]byte{AgeClaimV2Bytes(validAgeClaim()),
		SessionBindBytes(validSessionBind())} {
		require.ErrorIs(t, en2(text), ErrMalformed)
	}
}

// TestTextRefusals pins the field checks the byte-for-byte tests above do not
// reach. Each refused spelling would break a text's fixed line form or let a
// client and the server sign different bytes; each accepted value is the edge
// of its range.
func TestTextRefusals(t *testing.T) {
	const v1ID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"

	ageRefused := map[string]func(*AgeClaimV2){
		"obligation -1":        func(c *AgeClaimV2) { c.JurisdictionObligation = -1 },
		"negative timestamp":   func(c *AgeClaimV2) { c.Timestamp = -1 },
		"timestamp 2^53":       func(c *AgeClaimV2) { c.Timestamp = 1 << 53 },
		"nonce 63 hex":         func(c *AgeClaimV2) { c.Nonce = nonceA[:63] },
		"nonce 65 hex":         func(c *AgeClaimV2) { c.Nonce = nonceA + "0" },
		"empty client version": func(c *AgeClaimV2) { c.ClientVersion = "" },
		"client version 33":    func(c *AgeClaimV2) { c.ClientVersion = strings.Repeat("1", 33) },
		"client version space": func(c *AgeClaimV2) { c.ClientVersion = "1 2" },
		"uppercase device id":  func(c *AgeClaimV2) { c.DeviceID = strings.ToUpper(deviceA) },
	}
	for name, mut := range ageRefused {
		c := validAgeClaim()
		mut(&c)
		require.Nil(t, AgeClaimV2Bytes(c), name)
	}
	edge := validAgeClaim()
	edge.JurisdictionObligation = 0
	edge.Timestamp = 1
	edge.ClientVersion = strings.Repeat("a", 32)
	require.NotNil(t, AgeClaimV2Bytes(edge), "obligation 0, timestamp 1, 32-character client version")
	edge.Timestamp = int64(MaxSafeInt)
	require.NotNil(t, AgeClaimV2Bytes(edge), "timestamp at MaxSafeInt, the bound session-bind and the JS clients share")
	v1User := validAgeClaim()
	v1User.UserID = v1ID
	require.NotNil(t, AgeClaimV2Bytes(v1User), "user_id is s36, not s36v4")

	bind := validSessionBind()
	bindRefused := map[string]func(*SessionBind){
		"issued_at_ms 0":      func(b *SessionBind) { b.IssuedAtMs = 0 },
		"uppercase realm":     func(b *SessionBind) { b.RealmID = strings.ToUpper(realmA) },
		"uppercase user":      func(b *SessionBind) { b.UserID = strings.ToUpper(userA) },
		"uppercase session":   func(b *SessionBind) { b.SessionID = strings.ToUpper(sessionA) },
		"uppercase device id": func(b *SessionBind) { b.DeviceID = strings.ToUpper(deviceA) },
		"v1 device id":        func(b *SessionBind) { b.DeviceID = v1ID },
		"uppercase challenge": func(b *SessionBind) { b.Challenge = strings.ToUpper(nonceA) },
		"challenge 65 hex":    func(b *SessionBind) { b.Challenge = nonceA + "0" },
	}
	for name, mut := range bindRefused {
		b := bind
		mut(&b)
		require.Nil(t, SessionBindBytes(b), name)
	}
	b := bind
	b.IssuedAtMs = MaxSafeInt
	require.NotNil(t, SessionBindBytes(b), "issued_at_ms at 2^53-1")
	b = bind
	b.RealmID, b.UserID, b.SessionID = v1ID, v1ID, v1ID // all three are s36, not s36v4
	require.NotNil(t, SessionBindBytes(b), "realm_id, user_id and session_id may carry any UUID version")

	pinSum := sha512.Sum384([]byte("pin"))
	require.Empty(t, SafetyNumber(userA, append(pinSum[:], 0)), "49-byte pin digest")
	require.Empty(t, SafetyNumber(userA, nil), "nil pin digest")
	require.NotEmpty(t, SafetyNumber(v1ID, pinSum[:]), "account_id is s36, not s36v4")
}
