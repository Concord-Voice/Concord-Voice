package e2eekeys_test

import (
	"encoding/json"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/e2eekeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorResponse_JSONShape(t *testing.T) {
	// Pending=true: field must appear
	resp := e2eekeys.ErrorResponse{
		Error:   "no key yet",
		Code:    e2eekeys.CodeNoKeyYet,
		Kind:    e2eekeys.KindDM,
		Pending: true,
	}
	b, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.JSONEq(t, `{"error":"no key yet","code":"NO_KEY_YET","kind":"dm","pending":true}`, string(b))

	// Pending=false: omitempty drops the field
	resp.Pending = false
	resp.Code = e2eekeys.CodeNotMember
	b, err = json.Marshal(resp)
	require.NoError(t, err)
	assert.JSONEq(t, `{"error":"no key yet","code":"NOT_MEMBER","kind":"dm"}`, string(b))
}

func TestKeyResponse_JSONShape(t *testing.T) {
	resp := e2eekeys.KeyResponse{
		Key: e2eekeys.KeyPayload{
			WrappedKey: "c29tZS1iYXNlNjQ=",
			KeyVersion: 3,
		},
		Kind: e2eekeys.KindChannel,
	}
	b, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.JSONEq(t, `{"key":{"wrapped_key":"c29tZS1iYXNlNjQ=","key_version":3},"kind":"channel"}`, string(b))
}

func TestCodeAndKind_StringConstants(t *testing.T) {
	assert.Equal(t, "NOT_MEMBER", string(e2eekeys.CodeNotMember))
	assert.Equal(t, "NO_KEY_YET", string(e2eekeys.CodeNoKeyYet))
	assert.Equal(t, "REVOKED_EPOCH", string(e2eekeys.CodeRevokedEpoch))
	assert.Equal(t, "INVALID_REQUEST", string(e2eekeys.CodeInvalidRequest))
	assert.Equal(t, "INTERNAL_ERROR", string(e2eekeys.CodeInternalError))
	assert.Equal(t, "channel", string(e2eekeys.KindChannel))
	assert.Equal(t, "dm", string(e2eekeys.KindDM))
	assert.Equal(t, "unknown", string(e2eekeys.KindUnknown))
}
