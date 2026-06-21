package age

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validClaim() Claim {
	return Claim{
		CanonicalVersion: 1, UserID: "11111111-1111-4111-8111-111111111111",
		ValidAge: true, NSFWAuth: false, JurisdictionObligation: 1,
		Nonce:     "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Timestamp: 1750000000, KeyVersion: 1, ClientVersion: "0.2.0",
	}
}

func TestCanonicalBytes_ExactFormat(t *testing.T) {
	got, err := validClaim().CanonicalBytes()
	require.NoError(t, err)
	want := "age-claim/v1\ncanonical_version=1\nuser_id=11111111-1111-4111-8111-111111111111\n" +
		"valid_age=true\nnsfw_auth=false\njurisdiction_obligation=1\n" +
		"nonce=deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n" +
		"timestamp=1750000000\nkey_version=1\nclient_version=0.2.0"
	assert.Equal(t, want, string(got))
}

// TestCanonicalBytes_NoTrailingNewline locks the "no trailing newline" contract term:
// client_version is the terminal field and must NOT be followed by \n (a trailing newline
// would diverge from the child-B WebCrypto signer and fail every verify).
func TestCanonicalBytes_NoTrailingNewline(t *testing.T) {
	got, err := validClaim().CanonicalBytes()
	require.NoError(t, err)
	require.NotEmpty(t, got)
	assert.NotEqual(t, byte('\n'), got[len(got)-1])
}

func TestCanonicalBytes_RejectsBadCanonicalVersion(t *testing.T) {
	c := validClaim()
	c.CanonicalVersion = 2
	_, err := c.CanonicalBytes()
	assert.ErrorIs(t, err, ErrBadCanonicalVer)
}

func TestValidate_RejectsBadFields(t *testing.T) {
	cases := map[string]func(*Claim){
		"newline in client_version": func(c *Claim) { c.ClientVersion = "0.2\n0" },
		"equals in client_version":  func(c *Claim) { c.ClientVersion = "0=2" },
		"empty client_version":      func(c *Claim) { c.ClientVersion = "" },
		"oversize client_version":   func(c *Claim) { c.ClientVersion = string(make([]byte, 33)) },
		"obligation out of range":   func(c *Claim) { c.JurisdictionObligation = 3 },
		"negative obligation":       func(c *Claim) { c.JurisdictionObligation = -1 },
		"short nonce":               func(c *Claim) { c.Nonce = "abc" },
		"uppercase nonce":           func(c *Claim) { c.Nonce = "AA112233445566778899aabbccddeeff00112233445566778899aabbccddeeff" },
		"non-hex nonce":             func(c *Claim) { c.Nonce = "zz112233445566778899aabbccddeeff00112233445566778899aabbccddeeff" },
		"bad uuid":                  func(c *Claim) { c.UserID = "not-a-uuid" },
		"uppercase uuid":            func(c *Claim) { c.UserID = "11111111-1111-4111-8111-11111111111A" },
		"nonpositive timestamp":     func(c *Claim) { c.Timestamp = 0 },
		"nonpositive key_version":   func(c *Claim) { c.KeyVersion = 0 },
		"bad canonical_version":     func(c *Claim) { c.CanonicalVersion = 0 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			c := validClaim()
			mut(&c)
			assert.Error(t, c.Validate())
		})
	}
}

func TestValidate_AcceptsValid(t *testing.T) { assert.NoError(t, validClaim().Validate()) }
