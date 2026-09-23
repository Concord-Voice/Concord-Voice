package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const voiceEnforcementProtocolTestSecret = "test-key" // pragma: allowlist secret (test fixture, not a credential)

func validVoiceEnforcementSessionRequest() voiceEnforcementSessionRequest {
	return voiceEnforcementSessionRequest{
		SessionGeneration: uuid.MustParse("11111111-1111-4111-8111-111111111111").String(),
		NodeBootID:        uuid.MustParse("22222222-2222-4222-8222-222222222222").String(),
		RoomID:            uuid.MustParse("33333333-3333-4333-8333-333333333333").String(),
		RoomKind:          "dm",
		UserID:            uuid.MustParse("44444444-4444-4444-8444-444444444444").String(),
		CredentialEpoch:   strings.Repeat("0", 32),
		SocketID:          "socket-1",
	}
}

func TestVoiceEnforcementSessionRequestValid(t *testing.T) {
	t.Run("accepts canonical DM and channel identities", func(t *testing.T) {
		request := validVoiceEnforcementSessionRequest()
		assert.True(t, request.valid())

		request.RoomKind = "channel"
		assert.True(t, request.valid())
	})

	tests := []struct {
		name   string
		mutate func(*voiceEnforcementSessionRequest)
	}{
		{"rejects noncanonical UUID", func(r *voiceEnforcementSessionRequest) { r.RoomID = "33333333-3333-4333-8333-333333333333" + " " }},
		{"rejects unsupported room kind", func(r *voiceEnforcementSessionRequest) { r.RoomKind = "server" }},
		{"rejects malformed credential epoch", func(r *voiceEnforcementSessionRequest) { r.CredentialEpoch = strings.Repeat("A", 32) }},
		{"rejects newline in socket identity", func(r *voiceEnforcementSessionRequest) { r.SocketID = "socket\n1" }},
		{"rejects oversized socket identity", func(r *voiceEnforcementSessionRequest) { r.SocketID = string(make([]byte, 256)) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := validVoiceEnforcementSessionRequest()
			tc.mutate(&request)
			assert.False(t, request.valid())
		})
	}
}

func TestVoiceEnforcementRegistrationProofBindsEverySessionField(t *testing.T) {
	request := validVoiceEnforcementSessionRequest()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	tokenDigest := mediaproof.TokenDigest("test-token-123")
	fields := []string{
		tokenDigest, request.SessionGeneration, request.NodeBootID, request.RoomID,
		request.RoomKind, request.UserID, request.CredentialEpoch, request.SocketID,
	}
	proof := mediaproof.Sign(
		mediaproof.DeriveKey(voiceEnforcementProtocolTestSecret, voiceEnforcementRegistrationProofContext),
		voiceEnforcementRegistrationProofVersion, timestamp, fields...,
	)
	require.NotEmpty(t, proof)
	require.True(t, mediaproof.Verify(
		mediaproof.DeriveKey(voiceEnforcementProtocolTestSecret, voiceEnforcementRegistrationProofContext),
		proof, voiceEnforcementRegistrationProofVersion, timestamp, fields...,
	))

	for i := range fields {
		mutated := append([]string(nil), fields...)
		mutated[i] += "-changed"
		assert.False(t, mediaproof.Verify(
			mediaproof.DeriveKey(voiceEnforcementProtocolTestSecret, voiceEnforcementRegistrationProofContext),
			proof, voiceEnforcementRegistrationProofVersion, timestamp, mutated...,
		), "registration proof must bind field %d", i)
	}
}

func openUnqueriedVoiceEnforcementTestDB(t *testing.T) *sql.DB {
	t.Helper()
	// fakeConnector is deliberately connection-only. These tests must fail at
	// the trust boundary before database/sql gets a chance to open a connection.
	db := sql.OpenDB(fakeConnector{})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func serveVoiceEnforcementRequest(t *testing.T, handler gin.HandlerFunc, method string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, "/api/v1/voice/enforcement-sessions", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler(ctx)
	ctx.Writer.WriteHeaderNow()
	return recorder
}

func TestVoiceEnforcementReleaseRejectsOversizedBodyBeforeDatabase(t *testing.T) {
	handler := newVoiceEnforcementSessionHandler(openUnqueriedVoiceEnforcementTestDB(t), voiceEnforcementProtocolTestSecret)
	recorder := serveVoiceEnforcementRequest(t, handler.Release, http.MethodPost,
		bytes.Repeat([]byte("x"), voiceEnforcementReleaseMaxBodyBytes+1))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestVoiceEnforcementReleaseRejectsInvalidProofBeforeDatabase(t *testing.T) {
	request := validVoiceEnforcementSessionRequest()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/voice/enforcement-sessions", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set(voiceEnforcementReleaseTimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))
	ctx.Request.Header.Set(voiceEnforcementReleaseProofHeader, strings.Repeat("0", 64))
	newVoiceEnforcementSessionHandler(openUnqueriedVoiceEnforcementTestDB(t), voiceEnforcementProtocolTestSecret).Release(ctx)
	ctx.Writer.WriteHeaderNow()
	require.Equal(t, http.StatusForbidden, recorder.Code)
}

func TestVoiceEnforcementRegisterRejectsInvalidServiceProofBeforeDatabase(t *testing.T) {
	request := validVoiceEnforcementSessionRequest()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/voice/enforcement-sessions", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("Authorization", "Bearer test-token-123")
	ctx.Request.Header.Set(voiceEnforcementRegistrationTimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))
	ctx.Request.Header.Set(voiceEnforcementRegistrationProofHeader, strings.Repeat("0", 64))
	ctx.Set("concord_service_hop", true)
	ctx.Set("user_id", request.UserID)
	newVoiceEnforcementSessionHandler(openUnqueriedVoiceEnforcementTestDB(t), voiceEnforcementProtocolTestSecret).Register(ctx)
	ctx.Writer.WriteHeaderNow()
	require.Equal(t, http.StatusForbidden, recorder.Code)
}

type voiceEnforcementRequesterStub struct {
	subject string
	payload map[string]interface{}
	respond func(map[string]interface{}) []byte
	err     error
	calls   int
}

func (s *voiceEnforcementRequesterStub) RequestWithContext(_ context.Context, subject string, data interface{}) ([]byte, error) {
	s.calls++
	s.subject = subject
	switch payload := data.(type) {
	case map[string]interface{}:
		s.payload = payload
	default:
		return nil, errors.New("unexpected voice enforcement payload type")
	}
	if s.respond != nil {
		return s.respond(s.payload), s.err
	}
	return nil, s.err
}

func signedVoiceEnforcementACK(t *testing.T, request map[string]interface{}, ok bool) []byte {
	t.Helper()
	stringField := func(name string) string {
		value, present := request[name].(string)
		require.True(t, present, name)
		return value
	}
	ack := map[string]interface{}{
		"version":           voiceEnforcementSessionEjectVersion,
		"parentGeneration":  stringField("parentGeneration"),
		"sessionGeneration": stringField("sessionGeneration"),
		"nodeBootId":        stringField("nodeBootId"),
		"roomId":            stringField("roomId"),
		"roomKind":          stringField("roomKind"),
		"userId":            stringField("userId"),
		"credentialEpoch":   stringField("credentialEpoch"),
		"socketId":          stringField("socketId"),
		"timestamp":         stringField("timestamp"),
		"nonce":             stringField("nonce"),
		"ok":                ok,
	}
	timestamp := stringField("timestamp")
	fields := make([]string, 0, 9)
	for _, name := range []string{"parentGeneration", "sessionGeneration", "nodeBootId", "roomId", "roomKind", "userId", "credentialEpoch", "socketId", "nonce"} {
		fields = append(fields, stringField(name))
	}
	ack["proof"] = mediaproof.Sign(
		mediaproof.DeriveKey(voiceEnforcementProtocolTestSecret, voiceEnforcementSessionEjectACKProof),
		voiceEnforcementSessionEjectProofVersion, timestamp,
		append(fields, map[bool]string{true: "true", false: "false"}[ok])...,
	)
	encoded, err := json.Marshal(ack)
	require.NoError(t, err)
	return encoded
}

func validVoiceEnforcementHealthRequest() voiceEnforcementHealthRequest {
	return voiceEnforcementHealthRequest{
		NodeBootID: uuid.MustParse("22222222-2222-4222-8222-222222222222").String(),
		Nonce:      strings.Repeat("a", 64),
	}
}

func signedVoiceEnforcementHealthACK(t *testing.T, request map[string]interface{}, nodeBootID string, ok bool) []byte {
	t.Helper()
	challenge, present := request["challenge"].(string)
	require.True(t, present)
	timestamp, present := request["timestamp"].(string)
	require.True(t, present)
	ack := map[string]interface{}{
		"version":    2,
		"kind":       "health",
		"nodeBootId": nodeBootID,
		"challenge":  challenge,
		"timestamp":  timestamp,
		"ok":         ok,
	}
	ack["proof"] = mediaproof.Sign(
		mediaproof.DeriveKey(voiceEnforcementProtocolTestSecret, "concord/voice-enforcement-session/health/ack/v1"),
		"v1", timestamp, nodeBootID, challenge, "health", strconv.FormatBool(ok),
	)
	encoded, err := json.Marshal(ack)
	require.NoError(t, err)
	return encoded
}

func voiceEnforcementHealthProof(request voiceEnforcementHealthRequest, timestamp, method, uri string) string {
	return mediaproof.Sign(
		mediaproof.DeriveKey(voiceEnforcementProtocolTestSecret, voiceEnforcementHealthProofContext),
		voiceEnforcementHealthProofVersion, timestamp, request.NodeBootID, request.Nonce, method, uri,
	)
}

func TestVoiceEnforcementHealthBootstrapTargetsExactBootAndAcceptsSignedACK(t *testing.T) {
	request := validVoiceEnforcementHealthRequest()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	requester := &voiceEnforcementRequesterStub{}
	requester.respond = func(payload map[string]interface{}) []byte {
		return signedVoiceEnforcementHealthACK(t, payload, request.NodeBootID, true)
	}
	handler := newVoiceEnforcementSessionHandler(openUnqueriedVoiceEnforcementTestDB(t), voiceEnforcementProtocolTestSecret, requester)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/internal/voice/enforcement/health", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set(voiceEnforcementReleaseTimestampHeader, timestamp)
	ctx.Request.Header.Set(voiceEnforcementReleaseProofHeader, voiceEnforcementHealthProof(request, timestamp, ctx.Request.Method, ctx.Request.URL.RequestURI()))
	handler.HealthBootstrap(ctx)
	ctx.Writer.WriteHeaderNow()

	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, 1, requester.calls)
	assert.Equal(t, "voice.enforce.session."+request.NodeBootID, requester.subject)
	assert.Equal(t, "health", requester.payload["kind"])
	assert.Equal(t, request.NodeBootID, requester.payload["nodeBootId"])
}

func TestVoiceEnforcementHealthBootstrapRejectsInvalidHMACWithoutRequesting(t *testing.T) {
	request := validVoiceEnforcementHealthRequest()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		proof string
	}{
		{name: "missing proof"},
		{name: "invalid proof", proof: strings.Repeat("0", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requester := &voiceEnforcementRequesterStub{}
			handler := newVoiceEnforcementSessionHandler(openUnqueriedVoiceEnforcementTestDB(t), voiceEnforcementProtocolTestSecret, requester)
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/internal/voice/enforcement/health", bytes.NewReader(body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Request.Header.Set(voiceEnforcementReleaseTimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))
			ctx.Request.Header.Set(voiceEnforcementReleaseProofHeader, tc.proof)
			handler.HealthBootstrap(ctx)
			ctx.Writer.WriteHeaderNow()

			require.Equal(t, http.StatusForbidden, recorder.Code)
			assert.Zero(t, requester.calls)
		})
	}
}

func TestVoiceEnforcementHealthBootstrapRejectsMalformedBodies(t *testing.T) {
	requester := &voiceEnforcementRequesterStub{}
	handler := newVoiceEnforcementSessionHandler(openUnqueriedVoiceEnforcementTestDB(t), voiceEnforcementProtocolTestSecret, requester)
	request := validVoiceEnforcementHealthRequest()
	validBody, err := json.Marshal(request)
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		body []byte
		code int
	}{
		{name: "oversized", body: bytes.Repeat([]byte("x"), voiceEnforcementReleaseMaxBodyBytes+1), code: http.StatusRequestEntityTooLarge},
		{name: "trailing JSON", body: append(append([]byte(nil), validBody...), []byte(`{"extra":true}`)...), code: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveVoiceEnforcementRequest(t, handler.HealthBootstrap, http.MethodPost, tc.body)
			require.Equal(t, tc.code, recorder.Code)
		})
	}
	assert.Zero(t, requester.calls)
}

func TestVoiceEnforcementHealthBootstrapRejectsBadAcknowledgements(t *testing.T) {
	request := validVoiceEnforcementHealthRequest()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		respond func(*testing.T, map[string]interface{}) []byte
	}{
		{name: "malformed", respond: func(*testing.T, map[string]interface{}) []byte { return []byte("{") }},
		{name: "wrong node", respond: func(t *testing.T, payload map[string]interface{}) []byte {
			return signedVoiceEnforcementHealthACK(t, payload, uuid.MustParse("99999999-9999-4999-8999-999999999999").String(), true)
		}},
		{name: "negative acknowledgement", respond: func(t *testing.T, payload map[string]interface{}) []byte {
			return signedVoiceEnforcementHealthACK(t, payload, request.NodeBootID, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requester := &voiceEnforcementRequesterStub{respond: func(payload map[string]interface{}) []byte {
				return tc.respond(t, payload)
			}}
			handler := newVoiceEnforcementSessionHandler(openUnqueriedVoiceEnforcementTestDB(t), voiceEnforcementProtocolTestSecret, requester)
			timestamp := strconv.FormatInt(time.Now().Unix(), 10)
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/internal/voice/enforcement/health", bytes.NewReader(body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Request.Header.Set(voiceEnforcementReleaseTimestampHeader, timestamp)
			ctx.Request.Header.Set(voiceEnforcementReleaseProofHeader, voiceEnforcementHealthProof(request, timestamp, ctx.Request.Method, ctx.Request.URL.RequestURI()))
			handler.HealthBootstrap(ctx)
			ctx.Writer.WriteHeaderNow()

			require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
			require.Equal(t, 1, requester.calls)
		})
	}
}

func TestPublishVoiceEnforcementSessionEjectionTargetsExactNodeAndAcceptsSignedACK(t *testing.T) {
	row := voiceEnforcementSessionRow{
		sessionGeneration: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		nodeBootID:        uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		roomID:            uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		roomKind:          "dm",
		userID:            uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		credentialEpoch:   strings.Repeat("0", 32),
		socketID:          "socket-1",
	}
	requester := &voiceEnforcementRequesterStub{}
	requester.respond = func(request map[string]interface{}) []byte {
		return signedVoiceEnforcementACK(t, request, true)
	}
	parentGeneration := uuid.MustParse("55555555-5555-4555-8555-555555555555")

	require.NoError(t, publishVoiceEnforcementSessionEjection(
		context.Background(), requester, voiceEnforcementProtocolTestSecret, parentGeneration, row,
	))
	require.Equal(t, "voice.enforce.session."+row.nodeBootID.String(), requester.subject)
	assert.Equal(t, parentGeneration.String(), requester.payload["parentGeneration"])
	assert.Equal(t, row.sessionGeneration.String(), requester.payload["sessionGeneration"])
	assert.Equal(t, row.nodeBootID.String(), requester.payload["nodeBootId"])
	assert.Equal(t, row.socketID, requester.payload["socketId"])
}

func TestPublishVoiceEnforcementSessionEjectionRejectsNegativeACK(t *testing.T) {
	row := voiceEnforcementSessionRow{
		sessionGeneration: uuid.New(), nodeBootID: uuid.New(), roomID: uuid.New(),
		roomKind: "channel", userID: uuid.New(), credentialEpoch: "", socketID: "socket-1",
	}
	requester := &voiceEnforcementRequesterStub{}
	requester.respond = func(request map[string]interface{}) []byte {
		return signedVoiceEnforcementACK(t, request, false)
	}

	err := publishVoiceEnforcementSessionEjection(
		context.Background(), requester, voiceEnforcementProtocolTestSecret, uuid.New(), row,
	)
	require.EqualError(t, err, "targeted voice enforcement acknowledgement rejected")
}

func TestRoundRobinVoiceEnforcementSessionsContactsLaterLiveBootBeforeDeadBootRetry(t *testing.T) {
	deadBoot := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	liveBoot := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	rows := []voiceEnforcementSessionRow{
		{sessionGeneration: uuid.New(), nodeBootID: deadBoot},
		{sessionGeneration: uuid.New(), nodeBootID: deadBoot},
		{sessionGeneration: uuid.New(), nodeBootID: liveBoot},
	}
	ordered := roundRobinVoiceEnforcementSessions(rows)
	require.Len(t, ordered, 3)
	assert.Equal(t, deadBoot, ordered[0].nodeBootID)
	assert.Equal(t, liveBoot, ordered[1].nodeBootID,
		"a dead boot's second row must not starve a later healthy boot")
	assert.Equal(t, deadBoot, ordered[2].nodeBootID)
}

func TestPublishVoiceEnforcementSessionRowsContinuesAfterDeadTarget(t *testing.T) {
	dead, live := uuid.New(), uuid.New()
	requested := make(chan uuid.UUID, 2)
	err := publishVoiceEnforcementSessionRows(context.Background(), []voiceEnforcementSessionRow{
		{sessionGeneration: uuid.New(), nodeBootID: dead},
		{sessionGeneration: uuid.New(), nodeBootID: live},
	}, func(row voiceEnforcementSessionRow) error {
		requested <- row.nodeBootID
		if row.nodeBootID == dead {
			return errors.New("dead target")
		}
		return nil
	})
	require.Error(t, err)
	close(requested)
	seen := map[uuid.UUID]bool{}
	for node := range requested {
		seen[node] = true
	}
	assert.True(t, seen[dead])
	assert.True(t, seen[live], "a later healthy target must be contacted despite an earlier failure")
}

func TestDMBlockVoiceEjectionTimestampIsCurrent(t *testing.T) {
	now := time.Now().Unix()
	assert.True(t, dmBlockVoiceEjectionTimestampIsCurrent(strconv.FormatInt(now, 10)))
	assert.False(t, dmBlockVoiceEjectionTimestampIsCurrent(strconv.FormatInt(now+dmBlockVoiceEjectionRequestTimeout.Milliseconds()/1000+2, 10)))
	assert.False(t, dmBlockVoiceEjectionTimestampIsCurrent("not-a-timestamp"))
}
