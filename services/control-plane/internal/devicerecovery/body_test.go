package devicerecovery

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func bodyContext(raw string) *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}
func createBody(t *testing.T) CreateBody {
	t.Helper()
	v := fixture(t)
	return CreateBody{Version, "synthetic-token", v.Context.ServerOrigin, v.Context.AccountBinding, v.Context.RequesterNonce, v.Context.RequesterPublicKey}
}
func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
func TestStrictCreateBody(t *testing.T) {
	good := marshal(t, createBody(t))
	r, err := ParseCreate(bodyContext(good))
	require.NoError(t, err)
	require.Equal(t, Version, r.ProtocolVersion)
	tests := []struct {
		raw    string
		status int
	}{
		{good + ` {}`, 400}, {good + strings.Repeat(" ", MaxBody), 413}, {strings.Repeat(" ", MaxBody+1), 413},
		{`[]`, 400}, {`null`, 400}, {`true`, 400}, {`{}`, 400}, {`{"protocol_version":1}`, 400},
		{strings.Replace(good, `"protocol_version":2`, `"protocol_version":2,"protocol_version":2`, 1), 400},
		{strings.Replace(good, `"protocol_version":2`, `"protocol_version":2,"Protocol_version":2`, 1), 400},
		{strings.Replace(good, `"protocol_version":2`, `"protocol_version":2.0`, 1), 400},
		{strings.Replace(good, `"protocol_version":2`, `"protocol_version":"2"`, 1), 400},
		{strings.Replace(good, `"recovery_token":"synthetic-token"`, `"recovery_token":null`, 1), 400},
		{strings.Replace(good, `"recovery_token":"synthetic-token"`, `"recovery_token":[]`, 1), 400},
		{strings.Replace(good, `synthetic-token`, strings.Repeat("t", 4097), 1), 400},
		{strings.Replace(good, `"requester_nonce":`, `"Requester_nonce":`, 1), 400},
	}
	for _, tt := range tests {
		_, err := ParseCreate(bodyContext(tt.raw))
		require.Error(t, err, tt.raw[:min(len(tt.raw), 100)])
		var e *APIError
		require.ErrorAs(t, err, &e)
		require.Equal(t, tt.status, e.Status)
	}
}
func TestStrictActionBodies(t *testing.T) {
	v := fixture(t)
	good := []string{marshal(t, map[string]any{"action": "offer", "protocol_version": 2, "responder_public_key": v.Offer.ResponderPublicKey, "responder_nonce": v.Offer.ResponderNonce, "transcript_hash": v.Hash}), marshal(t, map[string]any{"action": "approve", "protocol_version": 2, "transcript_hash": v.Hash, "encrypted_payload": v.Envelope}), `{"action":"reject","protocol_version":2}`}
	for _, raw := range good {
		_, err := ParseRespond(bodyContext(raw))
		require.NoError(t, err)
		_, err = ParseRespond(bodyContext(strings.TrimSuffix(raw, "}") + `,"extra":true}`))
		require.Error(t, err)
	}
	for _, raw := range []string{`{"action":"approve","protocol_version":2}`, `{"action":"reject","protocol_version":2,"transcript_hash":"` + v.Hash + `"}`, strings.Replace(good[0], v.Offer.ResponderPublicKey, Encode(make([]byte, 97)), 1), strings.Replace(good[1], v.Envelope, "AAAA", 1), strings.Replace(good[0], `"action":"offer"`, `"action":"offer","action":"offer"`, 1), strings.Replace(good[0], v.Hash, "AA==", 1), strings.Replace(good[0], v.Offer.ResponderNonce, "AA==", 1), `{"action":"other","protocol_version":2}`} {
		_, err := ParseRespond(bodyContext(raw))
		require.Error(t, err)
	}
	raw := marshal(t, CompleteBody{Version, v.Hash})
	_, err := ParseComplete(bodyContext(raw))
	require.NoError(t, err)
	for _, raw := range []string{`{}`, `{"protocol_version":2,"transcript_hash":null}`, strings.TrimSuffix(raw, "}") + `,"action":"complete"}`, strings.Replace(raw, v.Hash, "AA==", 1)} {
		_, err := ParseComplete(bodyContext(raw))
		require.Error(t, err)
	}
	c := bodyContext("{}")
	WriteError(c, ErrConflict)
	require.Equal(t, 409, c.Writer.Status())
	c = bodyContext("{}")
	WriteError(c, ErrInvalid)
	require.Equal(t, 500, c.Writer.Status())
}
