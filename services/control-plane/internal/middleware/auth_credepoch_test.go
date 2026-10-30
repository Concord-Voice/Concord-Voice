package middleware_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
)

// #2201: AuthRequired enforces the per-user credential epoch. These tests run
// the full TestServer stack (real DB + Redis), driving the fence through its
// cache states and the DB read-through.

// familyAccessToken persists real SQL authority and signs exactly its bound expiry.
// Empty epoch is legitimate for a never-rotated account; it still requires a family.
func familyAccessToken(t *testing.T, db *sql.DB, userID, secret string) string {
	t.Helper()
	ctx := t.Context()
	id, err := uuid.Parse(userID)
	require.NoError(t, err)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback fixture: %v", err)
		}
	}()
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{id}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(id)
	require.NoError(t, err)
	var epoch string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1`, id).Scan(&epoch))
	family, err := loginfamily.CreateFamilyTx(ctx, tx, user, epoch)
	require.NoError(t, err)
	sid := uuid.New()
	prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: id, SID: sid, CredentialEpoch: epoch, EmailVerified: true}, time.Now())
	require.NoError(t, err)
	require.NoError(t, loginfamily.BindSIDTx(ctx, tx, family, sid, prepared.ExpiresAt()))
	require.NoError(t, tx.Commit())
	token, err := prepared.Sign(secret)
	require.NoError(t, err)
	return token
}

// epochToken changes only the requested spelling/epoch on otherwise valid family
// evidence, so stale/missing epoch cases still exercise the earlier Redis fence.
func epochToken(t *testing.T, db *sql.DB, userID, epoch string) string {
	t.Helper()
	claims := signedFixtureClaims(t, familyAccessToken(t, db, userID, testSecret), testSecret)
	claims["user_id"] = userID
	delete(claims, "cred_epoch")
	if epoch != "" {
		claims["cred_epoch"] = epoch
	}
	return makeToken(t, claims, testSecret)
}

func signedFixtureClaims(t *testing.T, token, secret string) jwt.MapClaims {
	t.Helper()
	parsed, err := jwt.Parse(token, func(*jwt.Token) (interface{}, error) { return []byte(secret), nil })
	require.NoError(t, err)
	require.True(t, parsed.Valid)
	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)
	return claims
}

func TestAuthRequired_CredentialEpoch(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	t.Run("no epoch marker admits family token with legacy empty epoch", func(t *testing.T) {
		user := ts.CreateTestUser(t, "epochnone")
		// Fresh user: users.credential_epoch is NULL, no cache key.
		w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, ts.DB, user.ID, ""))
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("cached active epoch admits only the matching claim", func(t *testing.T) {
		user := ts.CreateTestUser(t, "epochactive")
		_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, "11111111111111111111111111111111", user.ID)
		require.NoError(t, err)
		require.NoError(t, ts.Redis.Set(ctx, credepoch.Key(user.ID), "active:11111111111111111111111111111111", 15*time.Minute).Err())

		w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, ts.DB, user.ID, "11111111111111111111111111111111"))
		assert.Equal(t, http.StatusOK, w.Code, "matching epoch claim must admit")

		w = doAuthRequest(t, ts, bearerPrefix+epochToken(t, ts.DB, user.ID, "22222222222222222222222222222222"))
		assert.Equal(t, http.StatusUnauthorized, w.Code, "superseded epoch must reject")

		w = doAuthRequest(t, ts, bearerPrefix+epochToken(t, ts.DB, user.ID, ""))
		assert.Equal(t, http.StatusUnauthorized, w.Code, "missing claim after rotation must fail closed")
	})

	t.Run("blocked marker fails closed for every token", func(t *testing.T) {
		user := ts.CreateTestUser(t, "epochblocked")
		require.NoError(t, ts.Redis.Set(ctx, credepoch.Key(user.ID), "blocked:op1", 5*time.Minute).Err())

		w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, ts.DB, user.ID, "66666666666666666666666666666666"))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		w = doAuthRequest(t, ts, bearerPrefix+epochToken(t, ts.DB, user.ID, ""))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("cache miss reads through to the DB and back-fills", func(t *testing.T) {
		user := ts.CreateTestUser(t, "epochreadthru")
		_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = '33333333333333333333333333333333' WHERE id = $1`, user.ID)
		require.NoError(t, err)
		require.NoError(t, ts.Redis.Del(ctx, credepoch.Key(user.ID)).Err())

		w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, ts.DB, user.ID, "33333333333333333333333333333333"))
		assert.Equal(t, http.StatusOK, w.Code, "read-through must admit the DB epoch")

		cached, err := ts.Redis.Get(ctx, credepoch.Key(user.ID)).Result()
		require.NoError(t, err)
		assert.Equal(t, "active:33333333333333333333333333333333", cached, "read-through must back-fill the cache")

		w = doAuthRequest(t, ts, bearerPrefix+epochToken(t, ts.DB, user.ID, "22222222222222222222222222222222"))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("flush then read-through repopulates (AC-5 flush/rebuild)", func(t *testing.T) {
		user := ts.CreateTestUser(t, "epochflush")
		_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = '44444444444444444444444444444444' WHERE id = $1`, user.ID)
		require.NoError(t, err)
		// Simulate a flush of this key (a full FLUSHALL would break other tests
		// sharing the DB-1 test Redis; deleting the key is the same state).
		require.NoError(t, ts.Redis.Del(ctx, credepoch.Key(user.ID)).Err())

		w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, ts.DB, user.ID, "44444444444444444444444444444444"))
		assert.Equal(t, http.StatusOK, w.Code)
		cached, err := ts.Redis.Get(ctx, credepoch.Key(user.ID)).Result()
		require.NoError(t, err)
		assert.Equal(t, "active:44444444444444444444444444444444", cached)
	})
}
