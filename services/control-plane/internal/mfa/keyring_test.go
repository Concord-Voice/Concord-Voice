package mfa

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 64 hex chars = 32 bytes. Distinct test keys (test-only values, not credentials).
const (
	testKeyV1 = "0101010101010101010101010101010101010101010101010101010101010101"
	testKeyV2 = "0202020202020202020202020202020202020202020202020202020202020202"
)

func TestParseKeyring_SingleActiveKey_Defaults(t *testing.T) {
	ring, err := ParseKeyring(testKeyV1, 1, "")
	require.NoError(t, err)
	assert.Equal(t, 1, ring.ActiveVersion())
}

func TestParseKeyring_ActiveAndRetired(t *testing.T) {
	ring, err := ParseKeyring(testKeyV2, 2, "1:"+testKeyV1)
	require.NoError(t, err)
	assert.Equal(t, 2, ring.ActiveVersion())

	// A seed sealed under v1 must open with the retired key.
	ct, nonce, err := EncryptSecret([]byte("JBSWY3DPEHPK3PXP"), mustHex(t, testKeyV1))
	require.NoError(t, err)
	plain, err := ring.Open(ct, nonce, 1)
	require.NoError(t, err)
	assert.Equal(t, "JBSWY3DPEHPK3PXP", string(plain))
}

func TestParseKeyring_FailClosedMatrix(t *testing.T) {
	cases := []struct {
		name          string
		activeHex     string
		activeVersion int
		retired       string
		// sensitive is a fragment of the malformed input that MUST NOT appear in
		// the error (proves no key-material passthrough, not just the two good keys).
		sensitive string
	}{
		{"active key bad hex", "nothexactivekeyzz", 1, "", "nothexactivekeyzz"},
		{"active key wrong length", "abcdef0123", 1, "", "abcdef0123"},
		{"active version zero", testKeyV1, 0, "", testKeyV1},
		{"active version negative", testKeyV1, -3, "", testKeyV1},
		{"retired entry missing colon", testKeyV2, 2, "1" + testKeyV1, testKeyV1},
		{"retired entry bad version", testKeyV2, 2, "x:" + testKeyV1, testKeyV1},
		{"retired entry version zero", testKeyV2, 2, "0:" + testKeyV1, testKeyV1},
		{"retired entry bad key hex", testKeyV2, 2, "1:nothexretiredkeyzz", "nothexretiredkeyzz"},
		{"retired entry wrong key length", testKeyV2, 2, "1:00112233445566", "00112233445566"},
		{"retired duplicates retired", testKeyV2, 3, "1:" + testKeyV1 + ",1:" + testKeyV1, testKeyV1},
		{"retired collides with active", testKeyV2, 2, "2:" + testKeyV1, testKeyV1},
		{"retired empty entry", testKeyV2, 2, "1:" + testKeyV1 + ",,", testKeyV1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseKeyring(tc.activeHex, tc.activeVersion, tc.retired)
			require.Error(t, err)
			// Never echo key material in errors — neither the good keys nor the
			// (rejected) malformed input for this case.
			assert.NotContains(t, err.Error(), testKeyV1)
			assert.NotContains(t, err.Error(), testKeyV2)
			assert.NotContains(t, err.Error(), tc.sensitive)
		})
	}
}

func TestParseKeyring_VersionsFitPostgreSQLSmallint(t *testing.T) {
	tests := []struct {
		name          string
		activeVersion int
		retired       string
		wantErr       bool
	}{
		{name: "active minimum", activeVersion: 1},
		{name: "active maximum", activeVersion: 32767},
		{name: "active below minimum", activeVersion: 0, wantErr: true},
		{name: "active above maximum", activeVersion: 32768, wantErr: true},
		{name: "retired minimum", activeVersion: 2, retired: "1:" + testKeyV1},
		{name: "retired maximum", activeVersion: 1, retired: "32767:" + testKeyV1},
		{name: "retired below minimum", activeVersion: 2, retired: "0:" + testKeyV1, wantErr: true},
		{name: "retired above maximum", activeVersion: 1, retired: "32768:" + testKeyV1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseKeyring(testKeyV2, tt.activeVersion, tt.retired)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestKeyringSeal_UsesActiveVersionAndFreshNonce(t *testing.T) {
	ring, err := ParseKeyring(testKeyV2, 2, "1:"+testKeyV1)
	require.NoError(t, err)

	ct1, n1, ver, err := ring.Seal([]byte("secret-a"))
	require.NoError(t, err)
	assert.Equal(t, 2, ver)

	ct2, n2, _, err := ring.Seal([]byte("secret-a"))
	require.NoError(t, err)
	assert.NotEqual(t, n1, n2, "nonce must be fresh per seal")
	assert.NotEqual(t, ct1, ct2)

	plain, err := ring.Open(ct1, n1, 2)
	require.NoError(t, err)
	assert.Equal(t, "secret-a", string(plain))
}

func TestKeyringOpen_MissingVersionFailsClosed(t *testing.T) {
	ring, err := ParseKeyring(testKeyV1, 1, "")
	require.NoError(t, err)
	_, err = ring.Open([]byte{0x01}, make([]byte, 12), 7)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "version 7"), "error names the missing version: %v", err)
}

func TestKeyringOpen_WrongKeyFailsAuth(t *testing.T) {
	// Sealed under v1's key but stamped v2 (tamper / mis-stamp) → GCM auth failure.
	ring, err := ParseKeyring(testKeyV2, 2, "1:"+testKeyV1)
	require.NoError(t, err)
	ct, nonce, err := EncryptSecret([]byte("secret"), mustHex(t, testKeyV1))
	require.NoError(t, err)
	_, err = ring.Open(ct, nonce, 2)
	require.Error(t, err)
}

func mustHex(t *testing.T, h string) []byte {
	t.Helper()
	key, err := ParseEncryptionKey(h)
	require.NoError(t, err)
	return key
}
