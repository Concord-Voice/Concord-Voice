package websocket

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// #2201: the WS upgrade applies the same user-disabled + credential-epoch gates
// as HTTP bearer auth on BOTH the ticket and JWT paths. The fence answers from
// the seeded Redis cache (no DB), so these unit tests exercise the reject/admit
// branches directly.

func fenceWiredWSHandler(t *testing.T) (*Handler, *redis.Client) {
	t.Helper()
	rc := setupHubTestRedis(t)
	fence := credepoch.New(nil, rc, logger.NewWithWriter(io.Discard))
	return NewHandler(nil, nil, rc, testJWTSecret, nil, fence, nil), rc
}

func signedJWTWithClaims(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)
	return s
}

func TestAuthenticateWS_JWTStaleEpochRejected(t *testing.T) {
	userID := uuid.New()
	h, rc := fenceWiredWSHandler(t)
	require.NoError(t, rc.Set(context.Background(), credepoch.Key(userID.String()), "active:newEpoch", time.Minute).Err())

	token := signedJWTWithClaims(t, jwt.MapClaims{
		"user_id":    userID.String(),
		"cred_epoch": "oldEpoch",
		"exp":        jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	r, _ := http.NewRequest("GET", "/ws", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	_, _, err := h.authenticateWebSocket(newGinContext(r))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential epoch")
}

func TestAuthenticateWS_JWTMatchingEpochAdmitsWithSession(t *testing.T) {
	userID := uuid.New()
	h, rc := fenceWiredWSHandler(t)
	require.NoError(t, rc.Set(context.Background(), credepoch.Key(userID.String()), "active:goodEpoch", time.Minute).Err())

	token := signedJWTWithClaims(t, jwt.MapClaims{
		"user_id":    userID.String(),
		"sid":        "sess-xyz",
		"cred_epoch": "goodEpoch",
		"exp":        jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	r, _ := http.NewRequest("GET", "/ws", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	gotID, sessionID, err := h.authenticateWebSocket(newGinContext(r))
	require.NoError(t, err)
	assert.Equal(t, userID, gotID)
	assert.Equal(t, "sess-xyz", sessionID)
}

func TestAuthenticateWS_JWTDisabledUserRejected(t *testing.T) {
	userID := uuid.New()
	h, rc := fenceWiredWSHandler(t)
	require.NoError(t, rc.Set(context.Background(), middleware.UserDisabledKey(userID.String()), "1", time.Minute).Err())

	token := generateTestJWT(t, userID.String(), testJWTSecret)
	r, _ := http.NewRequest("GET", "/ws", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	_, _, err := h.authenticateWebSocket(newGinContext(r))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account disabled")
}

func TestAuthenticateWS_TicketStaleEpochRejected(t *testing.T) {
	userID := uuid.New()
	h, rc := fenceWiredWSHandler(t)
	require.NoError(t, rc.Set(context.Background(), credepoch.Key(userID.String()), "active:newEpoch", time.Minute).Err())

	ticket := "tickstale"
	require.NoError(t, rc.Set(context.Background(),
		wsTicketKeyPfx+ticket, userID.String()+":sess1:oldEpoch", 30*time.Second).Err())

	r, _ := http.NewRequest("GET", wsTicketPath+ticket, nil)
	_, _, err := h.authenticateWebSocket(newGinContext(r))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential epoch")
}

func TestAuthenticateWS_TicketDisabledUserRejected(t *testing.T) {
	userID := uuid.New()
	h, rc := fenceWiredWSHandler(t)
	require.NoError(t, rc.Set(context.Background(), middleware.UserDisabledKey(userID.String()), "1", time.Minute).Err())

	ticket := "tickdisabled"
	require.NoError(t, rc.Set(context.Background(),
		wsTicketKeyPfx+ticket, userID.String()+":sess1:someEpoch", 30*time.Second).Err())

	r, _ := http.NewRequest("GET", wsTicketPath+ticket, nil)
	_, _, err := h.authenticateWebSocket(newGinContext(r))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account disabled")
}
