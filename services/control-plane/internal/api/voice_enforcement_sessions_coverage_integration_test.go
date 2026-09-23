//go:build integration

package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openVoiceEnforcementCoverageDB(t *testing.T) *sql.DB {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("requires an explicitly configured isolated DATABASE_URL")
	}
	parsed, err := url.Parse(databaseURL)
	require.NoError(t, err)
	if !strings.HasSuffix(strings.TrimSuffix(path.Base(parsed.Path), "/"), "_test") {
		t.Skip("DATABASE_URL must name an isolated *_test database")
	}
	db, _ := testdb.SetupTestDB(t)
	return db
}

type voiceEnforcementCoverageCall struct {
	request   voiceEnforcementSessionRequest
	token     string
	timestamp string
	proof     string
}

func newVoiceEnforcementCoverageCall(request voiceEnforcementSessionRequest, protocolKey, token, proofContext, proofVersion string) voiceEnforcementCoverageCall {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	var proof string
	if proofContext == voiceEnforcementRegistrationProofContext {
		proof = mediaproof.Sign(mediaproof.DeriveKey(protocolKey, proofContext), proofVersion, timestamp,
			mediaproof.TokenDigest(token), request.SessionGeneration, request.NodeBootID,
			request.RoomID, request.RoomKind, request.UserID, request.CredentialEpoch, request.SocketID)
	} else {
		proof = mediaproof.Sign(mediaproof.DeriveKey(protocolKey, proofContext), proofVersion, timestamp,
			request.SessionGeneration, request.NodeBootID, request.RoomID, request.RoomKind,
			request.UserID, request.CredentialEpoch, request.SocketID)
	}
	return voiceEnforcementCoverageCall{request: request, token: token, timestamp: timestamp, proof: proof}
}

func serveVoiceEnforcementCoverageRequest(t *testing.T, handler gin.HandlerFunc, method string, call voiceEnforcementCoverageCall) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(call.request)
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, "/api/v1/voice/enforcement-sessions", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("Authorization", "Bearer "+call.token)
	ctx.Request.Header.Set(voiceEnforcementRegistrationTimestampHeader, call.timestamp)
	ctx.Request.Header.Set(voiceEnforcementRegistrationProofHeader, call.proof)
	ctx.Request.Header.Set(voiceEnforcementReleaseTimestampHeader, call.timestamp)
	ctx.Request.Header.Set(voiceEnforcementReleaseProofHeader, call.proof)
	ctx.Set("concord_service_hop", true)
	ctx.Set("user_id", call.request.UserID)
	ctx.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"cred_epoch": call.request.CredentialEpoch})
	handler(ctx)
	ctx.Writer.WriteHeaderNow()
	return recorder
}

func TestVoiceEnforcementSessionHandlersPersistAndReleaseExactRows(t *testing.T) {
	db := openVoiceEnforcementCoverageDB(t)
	userID := testdb.CreateUser(t, db)
	request := voiceEnforcementSessionRequest{
		SessionGeneration: uuid.NewString(),
		NodeBootID:        uuid.NewString(),
		RoomID:            uuid.NewString(),
		RoomKind:          "channel",
		UserID:            userID.String(),
		SocketID:          "coverage-socket-1",
	}
	const token = "coverage-token-3140"
	const protocolKey = voiceEnforcementProtocolTestSecret
	handler := newVoiceEnforcementSessionHandler(db, protocolKey)

	t.Run("registers exact session and accepts idempotent retry", func(t *testing.T) {
		recorder := serveVoiceEnforcementCoverageRequest(t, handler.Register, http.MethodPost, newVoiceEnforcementCoverageCall(request, protocolKey, token, voiceEnforcementRegistrationProofContext, voiceEnforcementRegistrationProofVersion))
		require.Equal(t, http.StatusNoContent, recorder.Code)

		retry := serveVoiceEnforcementCoverageRequest(t, handler.Register, http.MethodPost, newVoiceEnforcementCoverageCall(request, protocolKey, token, voiceEnforcementRegistrationProofContext, voiceEnforcementRegistrationProofVersion))
		assert.Equal(t, http.StatusNoContent, retry.Code)

		conflicting := request
		conflicting.SocketID = "coverage-socket-2"
		conflict := serveVoiceEnforcementCoverageRequest(t, handler.Register, http.MethodPost, newVoiceEnforcementCoverageCall(conflicting, protocolKey, token, voiceEnforcementRegistrationProofContext, voiceEnforcementRegistrationProofVersion))
		assert.Equal(t, http.StatusConflict, conflict.Code)
	})

	t.Run("release requires the exact session identity", func(t *testing.T) {
		wrongSocket := request
		wrongSocket.SocketID = "coverage-socket-2"
		conflict := serveVoiceEnforcementCoverageRequest(t, handler.Release, http.MethodPost, newVoiceEnforcementCoverageCall(wrongSocket, protocolKey, token, voiceEnforcementReleaseProofContext, voiceEnforcementReleaseProofVersion))
		require.Equal(t, http.StatusConflict, conflict.Code)

		released := serveVoiceEnforcementCoverageRequest(t, handler.Release, http.MethodPost, newVoiceEnforcementCoverageCall(request, protocolKey, token, voiceEnforcementReleaseProofContext, voiceEnforcementReleaseProofVersion))
		require.Equal(t, http.StatusNoContent, released.Code)

		idempotent := serveVoiceEnforcementCoverageRequest(t, handler.Release, http.MethodPost, newVoiceEnforcementCoverageCall(request, protocolKey, token, voiceEnforcementReleaseProofContext, voiceEnforcementReleaseProofVersion))
		assert.Equal(t, http.StatusNoContent, idempotent.Code)

		var count int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_enforcement_sessions WHERE session_generation = $1`, request.SessionGeneration).Scan(&count))
		assert.Zero(t, count)
	})
}
