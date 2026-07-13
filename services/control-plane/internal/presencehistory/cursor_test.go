package presencehistory

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCursorRoundTrip(t *testing.T) {
	recordedAt, err := time.Parse(time.RFC3339Nano, "2026-07-12T14:00:00.123456789Z")
	require.NoError(t, err)
	want := PageCursor{
		Version:    1,
		RecordedAt: recordedAt,
		ID:         uuid.MustParse("11111111-1111-4111-8111-111111111111"),
	}

	encoded, err := EncodeCursor(want)
	require.NoError(t, err)
	assert.NotContains(t, encoded, "=", "cursor must use unpadded base64url")
	assert.LessOrEqual(t, len(encoded), maxEncodedCursorBytes)

	got, err := DecodeCursor(encoded)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestCursorRejectsMalformedAndOversizedInput(t *testing.T) {
	validJSON := `{"v":1,"recorded_at":"2026-07-12T14:00:00.000000000Z","id":"11111111-1111-4111-8111-111111111111"}`
	valid := base64.RawURLEncoding.EncodeToString([]byte(validJSON))

	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "padding", value: valid + "="},
		{name: "invalid base64", value: "not+a+cursor"},
		{name: "encoded over limit", value: strings.Repeat("a", maxEncodedCursorBytes+1)},
		{
			name:  "decoded over limit",
			value: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(" ", maxDecodedCursorBytes+1))),
		},
		{name: "empty object", value: encodeCursorFixture(`{}`)},
		{name: "array", value: encodeCursorFixture(`[]`)},
		{name: "unknown field", value: encodeCursorFixture(strings.TrimSuffix(validJSON, "}") + `,"extra":true}`)},
		{name: "case-insensitive alias", value: encodeCursorFixture(strings.Replace(validJSON, `"v":1`, `"V":1`, 1))},
		{name: "duplicate key", value: encodeCursorFixture(strings.Replace(validJSON, `"v":1`, `"v":1,"v":1`, 1))},
		{name: "trailing JSON", value: encodeCursorFixture(validJSON + `{}`)},
		{name: "version zero", value: encodeCursorFixture(strings.Replace(validJSON, `"v":1`, `"v":0`, 1))},
		{name: "version two", value: encodeCursorFixture(strings.Replace(validJSON, `"v":1`, `"v":2`, 1))},
		{
			name: "non UTC timestamp",
			value: encodeCursorFixture(strings.Replace(
				validJSON, "2026-07-12T14:00:00.000000000Z", "2026-07-12T10:00:00-04:00", 1,
			)),
		},
		{
			name: "invalid timestamp",
			value: encodeCursorFixture(strings.Replace(
				validJSON, "2026-07-12T14:00:00.000000000Z", "not-a-time", 1,
			)),
		},
		{
			name: "invalid UUID",
			value: encodeCursorFixture(strings.Replace(
				validJSON, "11111111-1111-4111-8111-111111111111", "not-a-uuid", 1,
			)),
		},
		{
			name: "nil UUID",
			value: encodeCursorFixture(strings.Replace(
				validJSON, "11111111-1111-4111-8111-111111111111", uuid.Nil.String(), 1,
			)),
		},
		{
			name: "noncanonical UUID",
			value: encodeCursorFixture(strings.Replace(
				validJSON,
				"11111111-1111-4111-8111-111111111111",
				"urn:uuid:11111111-1111-4111-8111-111111111111",
				1,
			)),
		},
		{
			name: "wrong timestamp type",
			value: encodeCursorFixture(strings.Replace(
				validJSON, `"2026-07-12T14:00:00.000000000Z"`, "123", 1,
			)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeCursor(tc.value)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidCursor)
		})
	}
}

func TestCursorEncodeRejectsInvalidValues(t *testing.T) {
	validTime := time.Date(2026, time.July, 12, 14, 0, 0, 0, time.UTC)
	validID := uuid.MustParse("11111111-1111-4111-8111-111111111111")

	for _, cursor := range []PageCursor{
		{Version: 0, RecordedAt: validTime, ID: validID},
		{Version: 2, RecordedAt: validTime, ID: validID},
		{Version: 1, ID: validID},
		{Version: 1, RecordedAt: validTime, ID: uuid.Nil},
	} {
		_, err := EncodeCursor(cursor)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidCursor)
	}
}

func encodeCursorFixture(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}
