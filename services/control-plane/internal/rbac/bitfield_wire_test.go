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

// TestBitfieldWireForm_LeadingPlusRefused pins the D2 "+64" refusal on every
// request field that carries a bitfield. Go 1.27's encoding/json accepts a
// leading '+', so the refusal lives in bitfield_wire.go, and ErrorIs pins that
// branch rather than any error a decoder might also return: with the branch
// deleted these rows fail on Go 1.26 (a different error) and on Go 1.27 (no
// error). The escaped, case-variant and later-duplicate rows reach the field
// without a literal '+' after the member's opening quote. The earlier-duplicate
// rows prove every occurrence is checked, not only the last one the real decode
// keeps: a later null leaves the signed value in place. Each refusal has an
// accepted twin, so a decoder that refuses everything cannot pass.
func TestBitfieldWireForm_LeadingPlusRefused(t *testing.T) {
	override := func(members string) string {
		return `{"target_type":"user","target_id":"` + bitfieldWireTestUUID + `",` + members + `}`
	}
	refused := []struct {
		name string
		body string
		req  any
	}{
		{"create role permissions", `{"name":"r","permissions":"+8"}`, &CreateRoleRequest{}},
		{"update role permissions", `{"permissions":"+8"}`, &UpdateRoleRequest{}},
		{"override allow", override(`"allow":"+64","deny":"0"`), &UpsertOverrideRequest{}},
		{"override deny", override(`"allow":"0","deny":"+8"`), &UpsertOverrideRequest{}},
		{"plus zero", override(`"allow":"+0","deny":"0"`), &UpsertOverrideRequest{}},
		{"escaped plus", override(`"allow":"\u002b64","deny":"0"`), &UpsertOverrideRequest{}},
		{"case-variant key", override(`"ALLOW":"+64","deny":"0"`), &UpsertOverrideRequest{}},
		{"signed later duplicate", override(`"allow":"1","allow":"+64","deny":"0"`), &UpsertOverrideRequest{}},
		{"signed earlier duplicate, later null", override(`"allow":"+64","allow":null,"deny":"0"`), &UpsertOverrideRequest{}},
		{"signed earlier duplicate, later null string", override(`"allow":"+64","allow":"null","deny":"0"`), &UpsertOverrideRequest{}},
		{"signed earlier duplicate, later clean", override(`"allow":"+64","allow":"1","deny":"0"`), &UpsertOverrideRequest{}},
		{"signed earlier duplicate, case-variant null", override(`"allow":"+64","ALLOW":null,"deny":"0"`), &UpsertOverrideRequest{}},
		{"deny signed earlier duplicate", override(`"allow":"0","deny":"+8","deny":null`), &UpsertOverrideRequest{}},
		{"create role signed earlier duplicate", `{"name":"r","permissions":"+8","permissions":null}`, &CreateRoleRequest{}},
		{"update role signed earlier duplicate", `{"permissions":"+8","permissions":null}`, &UpdateRoleRequest{}},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			err := binding.JSON.BindBody([]byte(tc.body), tc.req)
			assert.ErrorIs(t, err, errBitfieldSign, "%s must be refused for its leading '+'", tc.body)
		})
	}

	t.Run("accepted/create role", func(t *testing.T) {
		req := &CreateRoleRequest{}
		require.NoError(t, binding.JSON.BindBody([]byte(`{"name":"r","permissions":"8","mfa_code":"123456"}`), req))
		assert.Equal(t, int64(8), req.Permissions)
		assert.Equal(t, "r", req.Name)
		assert.Equal(t, "123456", req.MFACode)
	})
	t.Run("accepted/update role", func(t *testing.T) {
		req := &UpdateRoleRequest{}
		require.NoError(t, binding.JSON.BindBody([]byte(`{"permissions":"8"}`), req))
		require.NotNil(t, req.Permissions)
		assert.Equal(t, int64(8), *req.Permissions)
	})
	t.Run("accepted/update role without permissions", func(t *testing.T) {
		req := &UpdateRoleRequest{}
		require.NoError(t, binding.JSON.BindBody([]byte(`{"name":"r"}`), req))
		assert.Nil(t, req.Permissions)
	})
	t.Run("accepted/clean duplicate keeps the last value", func(t *testing.T) {
		req := &UpsertOverrideRequest{}
		require.NoError(t, binding.JSON.BindBody([]byte(override(`"allow":"1","allow":"8","deny":"0"`)), req))
		assert.Equal(t, int64(8), req.Allow)
	})
	// Go 1.27 clears a ",string" pointer only for the literal "null"; an escaped
	// spelling of it left a present 0 that UpdateRole would write. Go 1.26 cleared
	// both, and so must the wrapper. The literal row is the control.
	for _, tc := range []struct{ name, body string }{
		{"literal quoted null", `{"permissions":"null"}`},
		{"escaped quoted null", `{"permissions":"nul\u006c"}`},
		{"escaped quoted null after a value", `{"permissions":"8","permissions":"nul\u006c"}`},
	} {
		t.Run("accepted/update role "+tc.name+" leaves permissions absent", func(t *testing.T) {
			req := &UpdateRoleRequest{}
			require.NoError(t, binding.JSON.BindBody([]byte(tc.body), req))
			assert.Nil(t, req.Permissions, "%s must leave permissions absent, as Go 1.26 did", tc.body)
		})
	}
	t.Run("accepted/override deny", func(t *testing.T) {
		req := &UpsertOverrideRequest{}
		require.NoError(t, binding.JSON.BindBody([]byte(override(`"allow":"0","deny":"8"`)), req))
		assert.Equal(t, int64(8), req.Deny)
		assert.Equal(t, "user", req.TargetType)
	})
	t.Run("array body is refused by the shadow decode", func(t *testing.T) {
		err := binding.JSON.BindBody([]byte(`[]`), &UpsertOverrideRequest{})
		require.Error(t, err)
		assert.NotErrorIs(t, err, errBitfieldSign)
	})
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
