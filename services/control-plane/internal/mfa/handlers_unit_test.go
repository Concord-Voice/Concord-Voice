package mfa

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// --- hasEmailOrSms ---

func TestHasEmailOrSms(t *testing.T) {
	assert.True(t, hasEmailOrSms([]string{"email"}))
	assert.True(t, hasEmailOrSms([]string{"sms"}))
	assert.True(t, hasEmailOrSms([]string{"totp", "email"}))
	assert.False(t, hasEmailOrSms([]string{"totp"}))
	assert.False(t, hasEmailOrSms([]string{}))
	assert.False(t, hasEmailOrSms(nil))
}

// --- countLoginEligible ---

func TestCountLoginEligible(t *testing.T) {
	tests := []struct {
		name     string
		enabled  []string
		excluded []string
		want     int
	}{
		{"no exclusions", []string{"totp", "email"}, nil, 2},
		{"exclude one", []string{"totp", "email"}, []string{"email"}, 1},
		{"exclude all", []string{"email"}, []string{"email"}, 0},
		{"exclude non-existent", []string{"totp"}, []string{"email"}, 1},
		{"empty enabled", nil, nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, countLoginEligible(tc.enabled, tc.excluded))
		})
	}
}

// --- filterValidRecoveryOnly ---

func TestFilterValidRecoveryOnly(t *testing.T) {
	enabled := map[string]bool{"totp": true, "email": true}

	assert.Equal(t, []string{"email"}, filterValidRecoveryOnly([]string{"email"}, enabled))
	assert.Equal(t, []string{"totp", "email"}, filterValidRecoveryOnly([]string{"totp", "email"}, enabled))
	assert.Nil(t, filterValidRecoveryOnly([]string{"sms"}, enabled))
	assert.Nil(t, filterValidRecoveryOnly(nil, enabled))
}

// --- containsStr ---

func TestContainsStr(t *testing.T) {
	assert.True(t, containsStr([]string{"a", "b", "c"}, "b"))
	assert.False(t, containsStr([]string{"a", "b"}, "c"))
	assert.False(t, containsStr(nil, "a"))
	assert.False(t, containsStr([]string{}, "a"))
}

// --- isValidEmail ---

func TestIsValidEmail(t *testing.T) {
	assert.True(t, isValidEmail("user@example.com"))
	assert.True(t, isValidEmail("test+tag@sub.domain.org"))
	assert.False(t, isValidEmail(""))
	assert.False(t, isValidEmail("noatsign"))
	assert.False(t, isValidEmail("@nodomain"))
	assert.False(t, isValidEmail("user@"))
	// user@.com is technically accepted by this simple validator (has chars on both sides of @)
}

// --- generateNumericCode ---

func TestGenerateNumericCode(t *testing.T) {
	code, err := generateNumericCode(6)
	assert.NoError(t, err)
	assert.Len(t, code, 6)

	code, err = generateNumericCode(8)
	assert.NoError(t, err)
	assert.Len(t, code, 8)

	// Leading zeros should be preserved
	for i := 0; i < 50; i++ {
		c, err := generateNumericCode(6)
		assert.NoError(t, err)
		assert.Len(t, c, 6)
	}
}

// --- ValidateHardenedModeCodes ---

func TestValidateHardenedModeCodesInternal(t *testing.T) {
	assert.NotEmpty(t, ValidateHardenedModeCodes(map[string]string{"email": "123"}))
	assert.NotEmpty(t, ValidateHardenedModeCodes(map[string]string{"sms": "123"}))
	assert.Empty(t, ValidateHardenedModeCodes(map[string]string{"email": "123", "sms": "456"}))
	assert.NotEmpty(t, ValidateHardenedModeCodes(map[string]string{}))
}

// --- ValidateCircleConstraints ---

func TestValidateCircleConstraintsInternal(t *testing.T) {
	errMsg, _ := ValidateCircleConstraints(2, 3, 3)
	assert.Empty(t, errMsg, "valid 2-of-3")

	errMsg, _ = ValidateCircleConstraints(1, 3, 3)
	assert.NotEmpty(t, errMsg, "k<2")

	errMsg, _ = ValidateCircleConstraints(2, 7, 7)
	assert.Empty(t, errMsg, "valid 2-of-7")

	errMsg, _ = ValidateCircleConstraints(2, 8, 8)
	assert.NotEmpty(t, errMsg, "n>7")
}

// --- DecodeApprovalPayloads ---

func TestDecodeApprovalPayloadsInternal(t *testing.T) {
	enc, pub, errMsg, _ := DecodeApprovalPayloads("cGF5bG9hZA==", "cHVia2V5")
	assert.Empty(t, errMsg)
	assert.Equal(t, []byte("payload"), enc)
	assert.Equal(t, []byte("pubkey"), pub)
}

// --- isValidEmailCode ---

func TestIsValidEmailCode(t *testing.T) {
	assert.True(t, isValidEmailCode("123456"))
	assert.True(t, isValidEmailCode("000000"))
	assert.False(t, isValidEmailCode("12345"))   // too short
	assert.False(t, isValidEmailCode("1234567")) // too long
	assert.False(t, isValidEmailCode("12345a"))  // non-digit
	assert.False(t, isValidEmailCode(""))        // empty
	assert.False(t, isValidEmailCode("abcdef"))  // all letters
}

// --- ValidateShareUniqueness ---

func TestValidateShareUniquenessShareIndexOverTotal(t *testing.T) {
	shares := []CircleShareEntry{
		{ContactID: "a", ShareIndex: 5, EncryptedShare: "x"},
	}
	errMsg, _ := ValidateShareUniqueness(shares, 3)
	assert.Contains(t, errMsg, "share_index must be between")
}

// --- DecodeCircleShares ---

func TestDecodeCircleSharesMultiple(t *testing.T) {
	shares := []CircleShareEntry{
		{ContactID: "a", ShareIndex: 1, EncryptedShare: "YWJj"},
		{ContactID: "b", ShareIndex: 2, EncryptedShare: "ZGVm"},
	}
	decoded, errMsg, _ := DecodeCircleShares(shares)
	assert.Empty(t, errMsg)
	assert.Len(t, decoded, 2)
	assert.Equal(t, "a", decoded[0].ContactID)
	assert.Equal(t, []byte("abc"), decoded[0].EncryptedShare)
	assert.Equal(t, "b", decoded[1].ContactID)
	assert.Equal(t, []byte("def"), decoded[1].EncryptedShare)
}

// --- socialRecoveryRequestInfo ---

func TestSocialRecoveryRequestInfoStruct(t *testing.T) {
	info := socialRecoveryRequestInfo{
		Status:     "pending",
		CircleID:   "circle-1",
		ThresholdK: 3,
	}
	assert.Equal(t, "pending", info.Status)
	assert.Equal(t, 3, info.ThresholdK)
}

// --- CircleDecodedShare ---

func TestCircleDecodedShareStruct(t *testing.T) {
	s := CircleDecodedShare{
		ContactID:      "user-1",
		ShareIndex:     2,
		EncryptedShare: []byte("encrypted"),
	}
	assert.Equal(t, "user-1", s.ContactID)
	assert.Equal(t, 2, s.ShareIndex)
}
