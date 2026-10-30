package websocket

import (
	"context"
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

func TestRenewalFamilyAuthority_DB(t *testing.T) {
	r := loginFamilyRig(t)
	token := r.token(t, r.user, r.sid, "", r.expiry)
	verified, err := r.h.validateAccessToken(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, r.family, verified.FamilyID)
	// SQL family authority survives erasure of the old refresh-session material.
	_, err = r.db.Exec(`DELETE FROM refresh_tokens WHERE user_id=$1`, r.user)
	require.NoError(t, err)
	_, err = r.h.validateAccessToken(context.Background(), token)
	require.NoError(t, err)
	r.terminal(t)
	_, err = r.h.validateAccessToken(context.Background(), token)
	require.Error(t, err)
}

func renewalValidatorHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	rc := setupHubTestRedis(t)
	db, _ := dbtest.SetupTestDB(t)
	fence := credepoch.New(nil, rc, logger.NewWithWriter(io.Discard))
	userID := dbtest.CreateUser(t, db).String()
	require.NoError(t, rc.Set(context.Background(), credepoch.Key(userID), "active:11111111111111111111111111111111", time.Minute).Err())
	return NewHandler(nil, db, rc, testJWTSecret, nil, fence, nil), userID
}

func TestValidateAccessToken(t *testing.T) {
	ctx := context.Background()

	t.Run("valid access token returns the canonical fields", func(t *testing.T) {
		h, userID := renewalValidatorHandler(t)
		sid := uuid.NewString()
		token, err := auth.GenerateAccessToken(userID, testJWTSecret, true, "11111111111111111111111111111111", sid)
		require.NoError(t, err)
		parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
		require.NoError(t, err)
		exp, err := parsed.Claims.GetExpirationTime()
		require.NoError(t, err)
		family := uuid.New()
		_, err = h.db.Exec(`INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES($1,$2,$3)`, family, userID, "11111111111111111111111111111111")
		require.NoError(t, err)
		_, err = h.db.Exec(`INSERT INTO login_family_sids(sid,family_id,user_id,access_expires_at) VALUES($1,$2,$3,$4)`, sid, family, userID, exp.Time)
		require.NoError(t, err)
		_, err = h.db.Exec(`UPDATE users SET credential_epoch=$2 WHERE id=$1`, userID, "11111111111111111111111111111111")
		require.NoError(t, err)

		got, err := h.validateAccessToken(ctx, token)
		require.NoError(t, err)
		assert.Equal(t, uuid.MustParse(userID), got.UserID)
		assert.Equal(t, sid, got.SessionID)
		assert.Equal(t, "11111111111111111111111111111111", got.CredEpoch)
		assert.True(t, exp.Equal(got.ExpiresAt), "ExpiresAt is the token's own exp")
	})

	t.Run("MFA challenge token is rejected", func(t *testing.T) {
		h, userID := renewalValidatorHandler(t)
		token, _, err := mfa.GenerateChallengeToken(userID, mfa.PurposeMFAUpgrade, mfa.JWTSecret(testJWTSecret), mfa.CredEpoch("11111111111111111111111111111111"))
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
			"cred_epoch": "11111111111111111111111111111111", "exp": time.Now().Add(-time.Minute).Unix(),
		}).SignedString([]byte(testJWTSecret))
		require.NoError(t, err)
		_, err = h.validateAccessToken(ctx, token)
		require.ErrorIs(t, err, jwt.ErrTokenExpired)
	})

	t.Run("blacklisted jti is rejected", func(t *testing.T) {
		h, userID := renewalValidatorHandler(t)
		token, err := auth.GenerateAccessToken(userID, testJWTSecret, true, "11111111111111111111111111111111", uuid.NewString())
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

// Real family authority distinguishes a retained live membership from a sibling
// family and from a terminal family, without refresh predecessor queries.
func TestRenewToken_RealAuthorityFamily(t *testing.T) {
	for _, kind := range []string{"same family", "different family", "terminal family"} {
		t.Run(kind, func(t *testing.T) {
			r := loginFamilyRig(t)
			conn, _, _ := familyInstrumentedSocket(t)
			c := familyPublishedClient(r.user, r.family, conn)
			c.SessionID = r.sid.String()
			c.TokenExpiresAt = r.expiry
			c.renewAuthority = r.h
			c.asyncWg.Add(1)
			hub := r.h.hub
			c.Hub = hub
			hub.clients[c.ID] = c
			hub.userClients[c.UserID] = map[uuid.UUID]bool{c.ID: true}
			go hub.Run()
			t.Cleanup(hub.Shutdown)
			t.Cleanup(c.asyncWg.Done)
			family := r.family
			if kind == "different family" {
				family = uuid.New()
				_, err := r.db.Exec(`INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES($1,$2,'')`, family, r.user)
				require.NoError(t, err)
			}
			next := uuid.New()
			expiry := r.expiry.Add(time.Hour)
			_, err := r.db.Exec(`INSERT INTO login_family_sids(sid,family_id,user_id,access_expires_at) VALUES($1,$2,$3,$4)`, next, family, r.user, expiry)
			require.NoError(t, err)
			if kind == "terminal family" {
				r.terminal(t)
			}
			accepted := c.renewToken(r.token(t, r.user, next, "", expiry))
			if kind == "same family" {
				require.True(t, accepted)
				assert.True(t, c.tokenDeadline().Equal(expiry))
			} else {
				require.False(t, accepted)
				assert.True(t, c.tokenDeadline().Equal(r.expiry))
			}
		})
	}
}
