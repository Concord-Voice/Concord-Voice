package websocket

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// DB/Redis-backed proof for the renewal authority (token_renewal.go and
// Handler.validateAccessToken). Tokens are minted through the real
// auth.GenerateAccessToken and mfa generators, per backend.md token-class
// binding: a hand-rolled lookalike drifts from the production shape.

func insertRefreshSession(t *testing.T, db *sql.DB, userID uuid.UUID, predecessor *uuid.UUID, revoked bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var revokedAt interface{}
	if revoked {
		revokedAt = time.Now()
	}
	var pred interface{}
	if predecessor != nil {
		pred = *predecessor
	}
	_, err := db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, predecessor_id, revoked_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, userID, "hash-"+id.String(), time.Now().Add(time.Hour), pred, revokedAt,
	)
	require.NoError(t, err)
	return id
}

func closedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return db
}

func TestSessionPredecessor_DB(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(nil, db, nil, testJWTSecret, nil, nil, nil)
	ctx := context.Background()
	user := dbtest.CreateUser(t, db)
	other := dbtest.CreateUser(t, db)
	root := insertRefreshSession(t, db, user, nil, false)
	live := insertRefreshSession(t, db, user, &root, false)
	revoked := insertRefreshSession(t, db, user, &root, true)

	t.Run("live row returns its predecessor", func(t *testing.T) {
		got, err := h.sessionPredecessor(ctx, user, live.String())
		require.NoError(t, err)
		assert.Equal(t, root.String(), got)
	})
	t.Run("live row with NULL predecessor returns empty", func(t *testing.T) {
		got, err := h.sessionPredecessor(ctx, user, root.String())
		require.NoError(t, err)
		assert.Empty(t, got)
	})
	t.Run("uppercase spelling resolves to the same row", func(t *testing.T) {
		got, err := h.sessionPredecessor(ctx, user, fmt.Sprintf("%X", live[:]))
		require.NoError(t, err)
		assert.Equal(t, root.String(), got)
	})
	for name, args := range map[string]struct {
		user uuid.UUID
		sid  string
	}{
		"revoked row":      {user, revoked.String()},
		"missing row":      {user, uuid.NewString()},
		"other user's row": {other, live.String()},
		"non-uuid sid":     {user, "not-a-uuid"},
		"empty sid":        {user, ""},
	} {
		t.Run(name+" is errSessionNotFound", func(t *testing.T) {
			got, err := h.sessionPredecessor(ctx, args.user, args.sid)
			require.ErrorIs(t, err, errSessionNotFound)
			assert.Empty(t, got)
		})
	}
	t.Run("database failure is NOT errSessionNotFound", func(t *testing.T) {
		broken := NewHandler(nil, closedDB(t), nil, testJWTSecret, nil, nil, nil)
		_, err := broken.sessionPredecessor(ctx, user, live.String())
		require.Error(t, err)
		assert.NotErrorIs(t, err, errSessionNotFound, "an outage must be told apart from a lineage miss")
	})
}

func TestSessionRevoked_DB(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(nil, db, nil, testJWTSecret, nil, nil, nil)
	ctx := context.Background()
	user := dbtest.CreateUser(t, db)
	other := dbtest.CreateUser(t, db)
	live := insertRefreshSession(t, db, user, nil, false)
	revoked := insertRefreshSession(t, db, user, nil, true)

	t.Run("live row is not revoked", func(t *testing.T) {
		got, err := h.sessionRevoked(ctx, user, live.String())
		require.NoError(t, err)
		assert.False(t, got)
	})
	for name, args := range map[string]struct {
		user uuid.UUID
		sid  string
	}{
		"revoked row":      {user, revoked.String()},
		"missing row":      {user, uuid.NewString()},
		"other user's row": {other, live.String()},
	} {
		t.Run(name+" counts as revoked", func(t *testing.T) {
			got, err := h.sessionRevoked(ctx, args.user, args.sid)
			require.NoError(t, err)
			assert.True(t, got)
		})
	}
	t.Run("non-uuid sid is revoked with errSessionNotFound", func(t *testing.T) {
		got, err := h.sessionRevoked(ctx, user, "not-a-uuid")
		require.ErrorIs(t, err, errSessionNotFound)
		assert.True(t, got)
	})
	t.Run("database failure is revoked with an error", func(t *testing.T) {
		broken := NewHandler(nil, closedDB(t), nil, testJWTSecret, nil, nil, nil)
		got, err := broken.sessionRevoked(ctx, user, live.String())
		require.Error(t, err)
		assert.True(t, got, "an unreadable revocation state fails closed")
	})
}

func renewalValidatorHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	rc := setupHubTestRedis(t)
	db, _ := dbtest.SetupTestDB(t)
	fence := credepoch.New(nil, rc, logger.NewWithWriter(io.Discard))
	userID := uuid.NewString()
	require.NoError(t, rc.Set(context.Background(), credepoch.Key(userID), "active:epoch-a", time.Minute).Err())
	return NewHandler(nil, db, rc, testJWTSecret, nil, fence, nil), userID
}

func TestValidateAccessToken(t *testing.T) {
	ctx := context.Background()

	t.Run("valid access token returns the canonical fields", func(t *testing.T) {
		h, userID := renewalValidatorHandler(t)
		sid := uuid.NewString()
		token, err := auth.GenerateAccessToken(userID, testJWTSecret, true, "epoch-a", sid)
		require.NoError(t, err)
		parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
		require.NoError(t, err)
		exp, err := parsed.Claims.GetExpirationTime()
		require.NoError(t, err)

		got, err := h.validateAccessToken(ctx, token)
		require.NoError(t, err)
		assert.Equal(t, uuid.MustParse(userID), got.UserID)
		assert.Equal(t, sid, got.SessionID)
		assert.Equal(t, "epoch-a", got.CredEpoch)
		assert.True(t, exp.Equal(got.ExpiresAt), "ExpiresAt is the token's own exp")
	})

	t.Run("MFA challenge token is rejected", func(t *testing.T) {
		h, userID := renewalValidatorHandler(t)
		token, _, err := mfa.GenerateChallengeToken(userID, mfa.PurposeMFAUpgrade, mfa.JWTSecret(testJWTSecret), mfa.CredEpoch("epoch-a"))
		require.NoError(t, err)
		_, err = h.validateAccessToken(ctx, token)
		require.ErrorContains(t, err, "invalid authentication")
	})

	t.Run("recovery token is rejected", func(t *testing.T) {
		h, userID := renewalValidatorHandler(t)
		token, _, err := mfa.GenerateRecoveryToken(userID, mfa.JWTSecret(testJWTSecret))
		require.NoError(t, err)
		_, err = h.validateAccessToken(ctx, token)
		require.ErrorContains(t, err, "invalid authentication")
	})

	t.Run("expired access token is rejected", func(t *testing.T) {
		h, userID := renewalValidatorHandler(t)
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"iss": middleware.AccessTokenIssuer, "user_id": userID, "sid": uuid.NewString(),
			"cred_epoch": "epoch-a", "exp": time.Now().Add(-time.Minute).Unix(),
		}).SignedString([]byte(testJWTSecret))
		require.NoError(t, err)
		_, err = h.validateAccessToken(ctx, token)
		require.ErrorIs(t, err, jwt.ErrTokenExpired)
	})

	t.Run("blacklisted jti is rejected", func(t *testing.T) {
		h, userID := renewalValidatorHandler(t)
		token, err := auth.GenerateAccessToken(userID, testJWTSecret, true, "epoch-a", uuid.NewString())
		require.NoError(t, err)
		parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
		require.NoError(t, err)
		claims, ok := parsed.Claims.(jwt.MapClaims)
		require.True(t, ok)
		jti, ok := claims["jti"].(string)
		require.True(t, ok)
		require.NoError(t, h.redis.Set(ctx, "blacklist:"+jti, "1", time.Minute).Err())
		_, err = h.validateAccessToken(ctx, token)
		require.ErrorContains(t, err, "token has been revoked")
	})

	t.Run("stale credential epoch is rejected", func(t *testing.T) {
		h, userID := renewalValidatorHandler(t)
		token, err := auth.GenerateAccessToken(userID, testJWTSecret, true, "epoch-old", uuid.NewString())
		require.NoError(t, err)
		_, err = h.validateAccessToken(ctx, token)
		require.ErrorContains(t, err, "credential epoch")
	})
}

// End to end through the real Handler as the renewal authority: a rotated
// token whose sid descends from the socket's sid renews and rebinds; a sibling
// or a revoked successor does not.
func TestRenewToken_RealAuthorityLineage(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	rc := setupHubTestRedis(t)
	ctx := context.Background()

	setup := func(t *testing.T) (*Handler, *Hub, *Client, uuid.UUID, uuid.UUID) {
		t.Helper()
		user := dbtest.CreateUser(t, db)
		require.NoError(t, rc.Set(ctx, credepoch.Key(user.String()), "active:epoch-a", time.Minute).Err())
		fence := credepoch.New(db, rc, logger.NewWithWriter(io.Discard))
		hub := NewHub(nil, rc)
		h := NewHandler(hub, db, rc, testJWTSecret, nil, fence, nil)
		old := insertRefreshSession(t, db, user, nil, false)
		c := &Client{
			ID: uuid.New(), UserID: user, SessionID: old.String(), CredEpoch: "epoch-a",
			TokenExpiresAt: time.Now().Add(time.Minute), Hub: hub, Send: make(chan []byte, 8),
			renewAuthority: h,
		}
		return h, hub, c, user, old
	}

	t.Run("rotated successor renews and rebinds", func(t *testing.T) {
		_, hub, c, user, old := setup(t)
		startHub(t, hub)
		successor := insertRefreshSession(t, db, user, &old, false)
		token, err := auth.GenerateAccessToken(user.String(), testJWTSecret, true, "epoch-a", successor.String())
		require.NoError(t, err)
		before := c.tokenDeadline()
		require.True(t, c.renewToken(token))
		assert.True(t, c.tokenDeadline().After(before))
		assert.Equal(t, successor.String(), c.SessionID)
	})

	t.Run("sibling session of the same user is refused", func(t *testing.T) {
		_, hub, c, user, _ := setup(t)
		startHub(t, hub)
		otherRoot := insertRefreshSession(t, db, user, nil, false)
		sibling := insertRefreshSession(t, db, user, &otherRoot, false)
		token, err := auth.GenerateAccessToken(user.String(), testJWTSecret, true, "epoch-a", sibling.String())
		require.NoError(t, err)
		before := c.tokenDeadline()
		assert.False(t, c.renewToken(token))
		assert.Equal(t, before, c.tokenDeadline())
	})

	t.Run("revoked successor is refused", func(t *testing.T) {
		_, hub, c, user, old := setup(t)
		startHub(t, hub)
		successor := insertRefreshSession(t, db, user, &old, true)
		token, err := auth.GenerateAccessToken(user.String(), testJWTSecret, true, "epoch-a", successor.String())
		require.NoError(t, err)
		before := c.tokenDeadline()
		assert.False(t, c.renewToken(token))
		assert.Equal(t, before, c.tokenDeadline())
	})
}
