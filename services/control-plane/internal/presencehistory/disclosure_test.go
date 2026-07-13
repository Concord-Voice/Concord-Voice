package presencehistory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDisclosureSaaSExactCopyAndDeterministicHash(t *testing.T) {
	wantDetails := []string{
		"History includes your Custom Status text and emoji and is not end-to-end encrypted.",
		"Visibility tiers and recipient exceptions control sharing, but do not stop opted-in history from storing your status for you or make it unreadable to the server operator.",
		"History starts with your next Custom Status change; your current status and earlier activity are not added.",
		"At the retention cutoff, records become unavailable. Daily cleanup physically removes expired active-database rows, normally within 24 hours. Backup or legal-hold copies may persist when the operator is required to retain them.",
		"If the operator changes these terms, new recording pauses until you review them. Existing history remains available only until its retention cutoff unless you delete it sooner.",
	}

	first := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	second := BuildDisclosure(DisclosureOptions{
		InstanceType:     "SAAS",
		OperatorName:     "must be ignored",
		PrivacyPolicyURL: "https://must-be-ignored.example/privacy",
	})

	require.True(t, first.Available)
	require.NotNil(t, first.RequiredConsent)
	require.True(t, second.Available)
	require.NotNil(t, second.RequiredConsent)

	consent := first.RequiredConsent
	assert.Equal(t, int16(1), consent.Version)
	assert.Equal(t, "Concord Voice LLC", consent.OperatorName)
	assert.Equal(t,
		"Persistent activity history is stored on Concord servers. This data may be subject to legal subpoena. Disable to delete all history.",
		consent.RequiredText,
	)
	assert.Equal(t, wantDetails, consent.Details)
	assert.Equal(t, "https://concordvoice.com/privacy-policy", consent.PrivacyPolicyURL)
	assert.Equal(t,
		"I understand and consent to server-readable Activity History under the terms above.",
		consent.AcknowledgementLabel,
	)
	assert.Equal(t, consent.CopyHash, second.RequiredConsent.CopyHash,
		"SaaS disclosure must ignore operator-provided substitutions")
	assert.Regexp(t, `^[0-9a-f]{64}$`, consent.CopyHash)

	fields := make([]string, 0, 3+len(consent.Details)+2)
	fields = append(fields,
		strconv.FormatInt(int64(consent.Version), 10),
		consent.OperatorName,
		consent.RequiredText,
	)
	fields = append(fields, consent.Details...)
	fields = append(fields, consent.PrivacyPolicyURL, consent.AcknowledgementLabel)
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	assert.Equal(t, hex.EncodeToString(sum[:]), consent.CopyHash,
		"copy hash must use the documented field order with one NUL delimiter")
}

func TestDisclosureSelfHostedSubstitutionAndValidation(t *testing.T) {
	state := BuildDisclosure(DisclosureOptions{
		InstanceType:     " self-hosted ",
		OperatorName:     "Example Cooperative",
		PrivacyPolicyURL: "https://example.test/legal/privacy",
	})
	require.True(t, state.Available)
	require.NotNil(t, state.RequiredConsent)
	assert.Equal(t, "Example Cooperative", state.RequiredConsent.OperatorName)
	assert.Equal(t, "https://example.test/legal/privacy", state.RequiredConsent.PrivacyPolicyURL)
	assert.Contains(t, state.RequiredConsent.RequiredText, "Example Cooperative")
	assert.NotContains(t, state.RequiredConsent.RequiredText, "Concord")
	for _, detail := range state.RequiredConsent.Details {
		assert.NotContains(t, detail, "Concord Voice LLC")
	}

	for _, tc := range []struct {
		name    string
		options DisclosureOptions
	}{
		{
			name: "missing operator",
			options: DisclosureOptions{
				InstanceType:     "self-hosted",
				PrivacyPolicyURL: "https://example.test/privacy",
			},
		},
		{
			name: "blank operator",
			options: DisclosureOptions{
				InstanceType:     "self-hosted",
				OperatorName:     " \t ",
				PrivacyPolicyURL: "https://example.test/privacy",
			},
		},
		{
			name: "reserved SaaS legal entity",
			options: DisclosureOptions{
				InstanceType:     "self-hosted",
				OperatorName:     "Concord Voice LLC",
				PrivacyPolicyURL: "https://example.test/privacy",
			},
		},
		{
			name: "reserved SaaS legal entity in a longer label",
			options: DisclosureOptions{
				InstanceType:     "self-hosted",
				OperatorName:     "Unofficial Concord Voice LLC Mirror",
				PrivacyPolicyURL: "https://example.test/privacy",
			},
		},
		{
			name: "missing privacy URL",
			options: DisclosureOptions{
				InstanceType: "self-hosted",
				OperatorName: "Example Cooperative",
			},
		},
		{
			name: "relative privacy URL",
			options: DisclosureOptions{
				InstanceType:     "self-hosted",
				OperatorName:     "Example Cooperative",
				PrivacyPolicyURL: "/privacy",
			},
		},
		{
			name: "external HTTP privacy URL",
			options: DisclosureOptions{
				InstanceType:     "self-hosted",
				OperatorName:     "Example Cooperative",
				PrivacyPolicyURL: "http://example.test/privacy",
				Development:      true,
			},
		},
		{
			name: "localhost HTTP outside development",
			options: DisclosureOptions{
				InstanceType:     "self-hosted",
				OperatorName:     "Example Cooperative",
				PrivacyPolicyURL: "http://localhost:3000/privacy",
			},
		},
		{
			name: "userinfo privacy URL",
			options: DisclosureOptions{
				InstanceType:     "self-hosted",
				OperatorName:     "Example Cooperative",
				PrivacyPolicyURL: "https://user@example.test/privacy",
			},
		},
		{
			name: "lookalike localhost",
			options: DisclosureOptions{
				InstanceType:     "self-hosted",
				OperatorName:     "Example Cooperative",
				PrivacyPolicyURL: "http://localhost.example.test/privacy",
				Development:      true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildDisclosure(tc.options)
			assert.False(t, got.Available)
			assert.Nil(t, got.RequiredConsent)
		})
	}

	for _, rawURL := range []string{
		"http://localhost:3000/privacy",
		"http://127.0.0.1:3000/privacy",
		"http://[::1]:3000/privacy",
	} {
		t.Run("development "+rawURL, func(t *testing.T) {
			got := BuildDisclosure(DisclosureOptions{
				InstanceType:     "self-hosted",
				OperatorName:     "Local Operator",
				PrivacyPolicyURL: rawURL,
				Development:      true,
			})
			require.True(t, got.Available)
			require.NotNil(t, got.RequiredConsent)
			assert.Equal(t, rawURL, got.RequiredConsent.PrivacyPolicyURL)
		})
	}
}

func TestCategoryTaxonomyAndAdapterRegistry(t *testing.T) {
	want := []Category{
		CategoryServerVoice,
		CategoryPrivateCall,
		CategoryGames,
		CategoryMusic,
		CategoryStreaming,
		CategoryBrowser,
		CategoryProductivity,
		CategoryCreator,
		CategoryCustomText,
	}
	assert.Equal(t, want, allCategories)
	for _, category := range want {
		assert.True(t, category.Valid(), "category %q must be valid", category)
	}
	assert.False(t, Category("unknown").Valid())

	require.Len(t, payloadReaders, 1, "only the functional custom_text adapter may be registered")
	reader, ok := payloadReaders[payloadKey{Category: CategoryCustomText, Version: 1}]
	require.True(t, ok)

	decoded, err := reader(json.RawMessage(`{"text":"Reviewing","emoji":"🔍"}`))
	require.NoError(t, err)
	assert.Equal(t, CustomTextState{Text: "Reviewing", Emoji: "🔍"}, decoded)
	_, ok = payloadReaders[payloadKey{Category: CategoryServerVoice, Version: 1}]
	assert.False(t, ok, "taxonomy membership alone must not register a storage adapter")
}

func TestAdapterCustomTextUnicodeBoundariesAndStrictJSON(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    CustomTextState
		ok      bool
	}{
		{
			name:    "one code point",
			payload: `{"text":"界"}`,
			want:    CustomTextState{Text: "界"},
			ok:      true,
		},
		{
			name:    "140 code points",
			payload: `{"text":"` + strings.Repeat("界", 140) + `","emoji":"` + strings.Repeat("🔍", 32) + `"}`,
			want:    CustomTextState{Text: strings.Repeat("界", 140), Emoji: strings.Repeat("🔍", 32)},
			ok:      true,
		},
		{name: "empty text", payload: `{"text":""}`},
		{name: "missing text", payload: `{"emoji":"🔍"}`},
		{name: "141 code points", payload: `{"text":"` + strings.Repeat("界", 141) + `"}`},
		{name: "33 emoji code points", payload: `{"text":"valid","emoji":"` + strings.Repeat("🔍", 33) + `"}`},
		{name: "unknown field", payload: `{"text":"valid","extra":true}`},
		{name: "case-insensitive alias", payload: `{"Text":"valid"}`},
		{name: "duplicate text", payload: `{"text":"first","text":"second"}`},
		{name: "trailing JSON", payload: `{"text":"valid"}{}`},
		{name: "non-object", payload: `["valid"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeCustomTextV1(json.RawMessage(tc.payload))
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
