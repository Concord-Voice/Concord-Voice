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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVoiceEnforcementHealthBootstrapRequiresRequester(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/health", bytes.NewReader(nil))

	var handler *voiceEnforcementSessionHandler
	handler.HealthBootstrap(ctx)
	ctx.Writer.WriteHeaderNow()

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestVoiceEnforcementRegisterRejectsCredentialEpochMismatchBeforeDatabase(t *testing.T) {
	request := validVoiceEnforcementSessionRequest()
	request.RoomKind = "channel"
	body, err := json.Marshal(request)
	require.NoError(t, err)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	token := "test-token-123"
	proof := mediaproof.Sign(
		mediaproof.DeriveKey(voiceEnforcementProtocolTestSecret, voiceEnforcementRegistrationProofContext),
		voiceEnforcementRegistrationProofVersion, timestamp,
		mediaproof.TokenDigest(token), request.SessionGeneration, request.NodeBootID,
		request.RoomID, request.RoomKind, request.UserID, request.CredentialEpoch, request.SocketID,
	)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/register", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("Authorization", "Bearer "+token)
	ctx.Request.Header.Set(voiceEnforcementRegistrationTimestampHeader, timestamp)
	ctx.Request.Header.Set(voiceEnforcementRegistrationProofHeader, proof)
	ctx.Set("concord_service_hop", true)
	ctx.Set("user_id", request.UserID)
	ctx.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"cred_epoch": strings.Repeat("f", 32)})

	handler := newVoiceEnforcementSessionHandler(openUnqueriedVoiceEnforcementTestDB(t), voiceEnforcementProtocolTestSecret)
	handler.Register(ctx)
	ctx.Writer.WriteHeaderNow()

	assert.Equal(t, http.StatusForbidden, recorder.Code)
}

func TestVoiceEnforcementRequireMediaCapabilityPassesNonServiceHop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/capability", strings.NewReader("body"))

	handler := newVoiceEnforcementSessionHandler(sql.OpenDB(fakeConnector{}), voiceEnforcementProtocolTestSecret)
	handler.RequireMediaCapability(ctx)

	assert.False(t, ctx.IsAborted())
}

func TestVoiceEnforcementRequireMediaCapabilityFailsClosedWhenUnwired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/capability", nil)

	(&voiceEnforcementSessionHandler{}).RequireMediaCapability(ctx)
	ctx.Writer.WriteHeaderNow()

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.True(t, ctx.IsAborted())
}

func TestVoiceEnforcementSessionRequestSameComparesAllFields(t *testing.T) {
	request := validVoiceEnforcementSessionRequest()
	assert.True(t, request.same(request))
	request.SocketID = "different"
	assert.False(t, validVoiceEnforcementSessionRequest().same(request))
}

func TestVoiceEnforcementHealthRequesterErrorIsServiceUnavailable(t *testing.T) {
	request := validVoiceEnforcementHealthRequest()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	requester := &voiceEnforcementRequesterStub{err: errors.New("request failed")}
	handler := newVoiceEnforcementSessionHandler(openUnqueriedVoiceEnforcementTestDB(t), voiceEnforcementProtocolTestSecret, requester)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/health", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set(voiceEnforcementReleaseTimestampHeader, timestamp)
	ctx.Request.Header.Set(voiceEnforcementReleaseProofHeader, voiceEnforcementHealthProof(request, timestamp, ctx.Request.Method, ctx.Request.URL.RequestURI()))
	handler.HealthBootstrap(ctx)
	ctx.Writer.WriteHeaderNow()

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestVoiceEnforcementHealthBootstrapRejectsNonCanonicalBootID(t *testing.T) {
	request := validVoiceEnforcementHealthRequest()
	request.NodeBootID = strings.ToUpper(request.NodeBootID)
	body, err := json.Marshal(request)
	require.NoError(t, err)
	handler := newVoiceEnforcementSessionHandler(openUnqueriedVoiceEnforcementTestDB(t), voiceEnforcementProtocolTestSecret, &voiceEnforcementRequesterStub{})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/health", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set(voiceEnforcementReleaseTimestampHeader, "1700000000")
	handler.HealthBootstrap(ctx)
	ctx.Writer.WriteHeaderNow()

	assert.Equal(t, http.StatusForbidden, recorder.Code)
}

func TestVoiceEnforcementHealthContextCancellationPropagatesToRequester(t *testing.T) {
	requester := &voiceEnforcementRequesterStub{err: context.Canceled}
	err := publishVoiceEnforcementHealth(context.Background(), requester, []byte("request"), []byte("ack"), uuid.New(), strings.Repeat("a", 64))
	assert.ErrorIs(t, err, context.Canceled)
}
