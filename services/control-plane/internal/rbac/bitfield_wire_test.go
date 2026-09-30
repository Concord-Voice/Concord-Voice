package rbac

// Wire-form pins for issue #3406 (W1/W2). UpsertOverrideRequest,
// ChannelOverride and CategoryOverride carry allow/deny as decimal strings in
// both directions: the request refuses a JSON number and every non-decimal
// spelling, and the responses emit strings, exact above 2^53. Before #3406
// they bound bare int64, refusing the form the desktop sends. See
// [internal]specs/2026-09-26-3406-override-bitfield-wire-form-design.md
// §4 "Go wire pin" and §2 D2 for the oracle table this file encodes.
//
// No DB. Raw JSON literals only -- never marshal the production struct for
// the request side, since that would follow whatever tag the struct
// currently carries and hide the exact regression this test exists to prove.

import (
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin/binding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bitfieldWireTestUUID = "11111111-1111-1111-1111-111111111111"

// bitfieldWireCase is one row of spec §2 D2. allowLiteral is the raw JSON
// text of the "allow" field's value, or the empty string to mean "omit the
// key entirely" (the key-absent row).
type bitfieldWireCase struct {
	name         string
	allowLiteral string
	omitKey      bool
	wantErr      bool
	wantValue    int64
}

func (tc bitfieldWireCase) body() string {
	if tc.omitKey {
		return `{"target_type":"user","target_id":"` + bitfieldWireTestUUID + `","deny":"0"}`
	}
	return `{"target_type":"user","target_id":"` + bitfieldWireTestUUID + `","allow":` + tc.allowLiteral + `,"deny":"0"}`
}

// TestBitfieldWireForm_RequestDecode is W1: binding.JSON.BindBody is the same
// decode-and-validate path ShouldBindJSON uses. Every row of the D2 table is
// run with target_type/target_id fixed and valid, so only allow varies.
func TestBitfieldWireForm_RequestDecode(t *testing.T) {
	cases := []bitfieldWireCase{
		// Accepted, exact.
		{name: "decimal string 1024", allowLiteral: `"1024"`, wantValue: 1024},
		{name: "decimal string above 2^62", allowLiteral: `"4611686018427387905"`, wantValue: 1<<62 | 1},

		// Refused: a bare JSON number or boolean is not a string.
		{name: "bare number", allowLiteral: `1024`, wantErr: true},
		{name: "boolean true", allowLiteral: `true`, wantErr: true},

		// Refused: decodes to -1, then gte=0 fails validation (#2869). Still an
		// error from BindBody either way, which is all this test asserts.
		{name: "negative one", allowLiteral: `"-1"`, wantErr: true},

		// Refused: none of these decimal-string forms.
		{name: "hex", allowLiteral: `"0x40"`, wantErr: true},
		{name: "exponent", allowLiteral: `"1e3"`, wantErr: true},
		{name: "decimal point", allowLiteral: `"1.0"`, wantErr: true},
		{name: "underscore separator", allowLiteral: `"1_0"`, wantErr: true},
		{name: "leading plus", allowLiteral: `"+64"`, wantErr: true},
		{name: "empty string", allowLiteral: `""`, wantErr: true},
		{name: "leading space", allowLiteral: `" 1"`, wantErr: true},
		{name: "trailing space", allowLiteral: `"1 "`, wantErr: true},
		{name: "int64 overflow", allowLiteral: `"9223372036854775808"`, wantErr: true},
		{name: "double-quoted string", allowLiteral: `"\"1\""`, wantErr: true},

		// Accepted as 0.
		{name: "null literal", allowLiteral: `null`, wantValue: 0},
		{name: "key absent", omitKey: true, wantValue: 0},
		{name: "string null", allowLiteral: `"null"`, wantValue: 0},
		{name: "negative zero string", allowLiteral: `"-0"`, wantValue: 0},

		// Accepted, read as decimal (never octal).
		{name: "leading zeros", allowLiteral: `"0010"`, wantValue: 10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &UpsertOverrideRequest{}
			err := binding.JSON.BindBody([]byte(tc.body()), req)

			if tc.wantErr {
				assert.Error(t, err, "allow must round-trip as a decimal string: %q must be refused, got no error", tc.allowLiteral)
				return
			}

			require.NoError(t, err, "allow must round-trip as a decimal string: %q must be accepted, got error: %v", tc.allowLiteral, err)
			assert.Equal(t, tc.wantValue, req.Allow, "allow must round-trip as a decimal string: %q must decode to the exact value", tc.allowLiteral)
		})
	}
}

// TestBitfieldWireForm_ChannelOverrideResponseEncode is half of W2.
func TestBitfieldWireForm_ChannelOverrideResponseEncode(t *testing.T) {
	co := ChannelOverride{Allow: 1<<62 | 1, Deny: 0}
	b, err := json.Marshal(co)
	require.NoError(t, err)

	want := `{"id":"","channel_id":"","target_type":"","target_id":"","allow":"4611686018427387905","deny":"0","created_at":"","updated_at":""}`
	assert.JSONEq(t, want, string(b), "ChannelOverride.allow/deny must round-trip as decimal strings, exact above 2^53")
}

// TestBitfieldWireForm_CategoryOverrideResponseEncode is the other half of W2.
func TestBitfieldWireForm_CategoryOverrideResponseEncode(t *testing.T) {
	co := CategoryOverride{Allow: 1<<62 | 1, Deny: 0}
	b, err := json.Marshal(co)
	require.NoError(t, err)

	want := `{"id":"","category_id":"","target_type":"","target_id":"","allow":"4611686018427387905","deny":"0","created_at":"","updated_at":""}`
	assert.JSONEq(t, want, string(b), "CategoryOverride.allow/deny must round-trip as decimal strings, exact above 2^53")
}
