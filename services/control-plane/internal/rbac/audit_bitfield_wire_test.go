package rbac

// Audit metadata pins for issue #3406 (W3). decodeAuditMetadata re-emits the
// bitfield keys registered per action as exact decimal strings, keeps
// negatives as stored, leaves other values and unregistered actions alone,
// and refuses trailing data. auditMetadataForStorage stores every int64 or
// Permission value as a string, so rows written since #3406 need no
// conversion. See [internal]specs/2026-09-26-3406-override-bitfield-wire-form-design.md
// §2 D3 and §4 "Go wire pin" W3.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuditBitfieldWireForm_RegisteredKeysBecomeExactStrings covers every
// action/key pair in spec D3's auditBitfieldKeys map: a bitfield key stored as
// a JSON number above 2^53 must be re-emitted as its exact decimal string.
func TestAuditBitfieldWireForm_RegisteredKeysBecomeExactStrings(t *testing.T) {
	cases := []struct {
		name   string
		action string
		raw    string
		key    string
		want   string
	}{
		{name: "role_created.permissions", action: "role_created", raw: `{"role_name":"Mods","permissions":4611686018427387905}`, key: "permissions", want: "4611686018427387905"},
		{name: "role_updated.new_permissions", action: "role_updated", raw: `{"new_permissions":4611686018427387905}`, key: "new_permissions", want: "4611686018427387905"},
		{name: "channel_override_created.allow", action: "channel_override_created", raw: `{"target_type":"user","allow":4611686018427387905,"deny":0}`, key: "allow", want: "4611686018427387905"},
		{name: "channel_override_created.deny", action: "channel_override_created", raw: `{"target_type":"user","allow":0,"deny":4611686018427387905}`, key: "deny", want: "4611686018427387905"},
		{name: "channel_override_updated.allow", action: "channel_override_updated", raw: `{"allow":4611686018427387905,"deny":0}`, key: "allow", want: "4611686018427387905"},
		{name: "category_override_created.allow", action: "category_override_created", raw: `{"allow":4611686018427387905,"deny":0}`, key: "allow", want: "4611686018427387905"},
		{name: "category_override_updated.deny", action: "category_override_updated", raw: `{"allow":0,"deny":4611686018427387905}`, key: "deny", want: "4611686018427387905"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta, err := decodeAuditMetadata(tc.action, []byte(tc.raw))
			require.NoError(t, err, "%s.%s must decode without error", tc.action, tc.key)
			got, ok := meta[tc.key].(string)
			require.True(t, ok, "%s.%s must be re-emitted as a JSON string, not %T", tc.action, tc.key, meta[tc.key])
			assert.Equal(t, tc.want, got, "%s.%s must carry the exact stored digits, precision must not be lost above 2^53", tc.action, tc.key)
		})
	}
}

// TestAuditBitfieldWireForm_NegativeValuePreserved: a pre-#2869 negative value
// is audit evidence and must be re-encoded, never clamped or dropped.
func TestAuditBitfieldWireForm_NegativeValuePreserved(t *testing.T) {
	meta, err := decodeAuditMetadata("role_updated", []byte(`{"new_permissions":-1}`))
	require.NoError(t, err)
	got, ok := meta["new_permissions"].(string)
	require.True(t, ok, "new_permissions must be re-emitted as a string even when negative")
	assert.Equal(t, "-1", got, "a negative bitfield value from before #2869 must be preserved as evidence, not clamped")
}

// TestAuditBitfieldWireForm_UnregisteredActionKeepsJSONNumber: the stringify
// pass must be scoped by ACTION, not by key name -- an "allow" key under an
// action absent from auditBitfieldKeys must not be touched.
func TestAuditBitfieldWireForm_UnregisteredActionKeepsJSONNumber(t *testing.T) {
	meta, err := decodeAuditMetadata("member_updated", []byte(`{"allow":4611686018427387905}`))
	require.NoError(t, err)
	_, isNumber := meta["allow"].(json.Number)
	assert.True(t, isNumber, "an 'allow' key under an action not in auditBitfieldKeys must stay a json.Number: stringification must be scoped by action, not by key name alone")
}

// TestAuditBitfieldWireForm_NonBitfieldNumberRemarshalsExact: UseNumber must
// apply to every metadata value, not only the registered bitfield keys, so an
// unrelated numeric field above 2^53 also survives a round trip exactly.
func TestAuditBitfieldWireForm_NonBitfieldNumberRemarshalsExact(t *testing.T) {
	meta, err := decodeAuditMetadata("role_created", []byte(`{"permissions":0,"position":9007199254740993}`))
	require.NoError(t, err)

	b, err := json.Marshal(meta)
	require.NoError(t, err)
	assert.JSONEq(t, `{"permissions":"0","position":9007199254740993}`, string(b), "a non-bitfield numeric metadata value above 2^53 must re-marshal byte-exact")
}

// TestAuditBitfieldWireForm_AlreadyStringValueUnchanged: a bitfield key that
// is already a JSON string (e.g. a row written after the fix) must not be
// double-encoded or otherwise altered.
func TestAuditBitfieldWireForm_AlreadyStringValueUnchanged(t *testing.T) {
	meta, err := decodeAuditMetadata("role_created", []byte(`{"permissions":"42"}`))
	require.NoError(t, err)
	got, ok := meta["permissions"].(string)
	require.True(t, ok, "permissions must remain a string")
	assert.Equal(t, "42", got, "an already-string bitfield value must be left unchanged")
}

// TestAuditBitfieldWireForm_NullDocumentDoesNotPanic: a null metadata
// document (no bytes stored, or a stored JSON null) must not panic.
func TestAuditBitfieldWireForm_NullDocumentDoesNotPanic(t *testing.T) {
	assert.NotPanics(t, func() {
		_, _ = decodeAuditMetadata("role_created", []byte(`null`))
	}, "decodeAuditMetadata must not panic on a null metadata document")
}

// TestAuditBitfieldWireForm_TrailingDataRefused: json.Unmarshal, which the
// reader used before #3406, rejects a second value after the document. The
// streaming decoder does not, so decodeAuditMetadata must check for it or the
// reader silently accepts a shape it used to refuse (red-team, #3406).
func TestAuditBitfieldWireForm_TrailingDataRefused(t *testing.T) {
	for _, raw := range []string{`{"permissions":1} {"permissions":2}`, `{"permissions":1} x`, `{} []`} {
		_, err := decodeAuditMetadata("role_created", []byte(raw))
		assert.Error(t, err, "trailing data after the metadata document must be refused: %s", raw)
	}
	meta, err := decodeAuditMetadata("role_created", []byte("{\"permissions\":1}\n  "))
	require.NoError(t, err, "trailing whitespace is not a second document")
	assert.Equal(t, "1", meta["permissions"])
}

// TestAuditMetadataForStorage_StringifiesBitfields pins the write side: every
// Permission value becomes its exact decimal string before the row is
// marshalled, whatever its key, so a future audit action that records a
// bitfield as Permission cannot put it on the wire as a lossy JSON number by
// forgetting auditBitfieldKeys. Other values pass through, and the caller's map
// is not mutated.
func TestAuditMetadataForStorage_StringifiesBitfields(t *testing.T) {
	in := map[string]interface{}{
		"permissions": Permission(1<<62 | 1),
		"deny":        Permission(1 << 62),
		"role_name":   "Mods",
		"sync":        true,
		"new_order":   []string{"a"},
	}
	out := auditMetadataForStorage(in)
	assert.Equal(t, "4611686018427387905", out["permissions"])
	assert.Equal(t, "4611686018427387904", out["deny"])
	assert.Equal(t, "Mods", out["role_name"])
	assert.Equal(t, true, out["sync"])
	assert.Equal(t, []string{"a"}, out["new_order"])
	assert.Equal(t, Permission(1<<62|1), in["permissions"], "the caller's map must not be mutated")
	assert.Nil(t, auditMetadataForStorage(nil))
}

// TestAuditMetadataForStorage_LeavesNonBitfieldIntegersAlone: AuditWriter also
// serves non-permission events (member timeouts record duration_seconds as an
// int64), so write-time stringification must key on the Permission type, not
// on every int64, or it changes an unrelated audit contract (#3406 review).
func TestAuditMetadataForStorage_LeavesNonBitfieldIntegersAlone(t *testing.T) {
	out := auditMetadataForStorage(map[string]interface{}{"duration_seconds": int64(300)})
	assert.Equal(t, int64(300), out["duration_seconds"], "a non-bitfield int64 must stay a JSON number")
}
