package middleware_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
)

// #2201: AuthRequired enforces the per-user credential epoch. These tests run
// the full TestServer stack (real DB + Redis), driving the fence through its
// cache states and the DB read-through.

// epochToken mints a token for the user with an explicit cred_epoch claim
// ("" = omit the claim, the legacy shape).
func epochToken(t *testing.T, userID, epoch string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"user_id":        userID,
		"email_verified": true,
		"exp":            time.Now().Add(15 * time.Minute).Unix(),
		"iat":            time.Now().Unix(),
		"jti":            "epoch-test-" + userID + "-" + epoch,
	}
	if epoch != "" {
		claims["cred_epoch"] = epoch
	}
	return makeToken(t, claims, testSecret)
}

func TestAuthRequired_CredentialEpoch(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	t.Run("no epoch marker admits legacy token", func(t *testing.T) {
		user := ts.CreateTestUser(t, "epochnone")
		// Fresh user: users.credential_epoch is NULL, no cache key.
		w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, user.ID, ""))
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("cached active epoch admits only the matching claim", func(t *testing.T) {
		user := ts.CreateTestUser(t, "epochactive")
		require.NoError(t, ts.Redis.Set(ctx, credepoch.Key(user.ID), "active:epochE1", 15*time.Minute).Err())

		w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, user.ID, "epochE1"))
		assert.Equal(t, http.StatusOK, w.Code, "matching epoch claim must admit")

		w = doAuthRequest(t, ts, bearerPrefix+epochToken(t, user.ID, "epochStale"))
		assert.Equal(t, http.StatusUnauthorized, w.Code, "superseded epoch must reject")

		w = doAuthRequest(t, ts, bearerPrefix+epochToken(t, user.ID, ""))
		assert.Equal(t, http.StatusUnauthorized, w.Code, "missing claim after rotation must fail closed")
	})

	t.Run("blocked marker fails closed for every token", func(t *testing.T) {
		user := ts.CreateTestUser(t, "epochblocked")
		require.NoError(t, ts.Redis.Set(ctx, credepoch.Key(user.ID), "blocked:op1", 5*time.Minute).Err())

		w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, user.ID, "anything"))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		w = doAuthRequest(t, ts, bearerPrefix+epochToken(t, user.ID, ""))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("cache miss reads through to the DB and back-fills", func(t *testing.T) {
		user := ts.CreateTestUser(t, "epochreadthru")
		_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = 'dbepoch1' WHERE id = $1`, user.ID)
		require.NoError(t, err)
		require.NoError(t, ts.Redis.Del(ctx, credepoch.Key(user.ID)).Err())

		w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, user.ID, "dbepoch1"))
		assert.Equal(t, http.StatusOK, w.Code, "read-through must admit the DB epoch")

		cached, err := ts.Redis.Get(ctx, credepoch.Key(user.ID)).Result()
		require.NoError(t, err)
		assert.Equal(t, "active:dbepoch1", cached, "read-through must back-fill the cache")

		w = doAuthRequest(t, ts, bearerPrefix+epochToken(t, user.ID, "stale"))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("flush then read-through repopulates (AC-5 flush/rebuild)", func(t *testing.T) {
		user := ts.CreateTestUser(t, "epochflush")
		_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = 'dbepoch2' WHERE id = $1`, user.ID)
		require.NoError(t, err)
		// Simulate a flush of this key (a full FLUSHALL would break other tests
		// sharing the DB-1 test Redis; deleting the key is the same state).
		require.NoError(t, ts.Redis.Del(ctx, credepoch.Key(user.ID)).Err())

		w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, user.ID, "dbepoch2"))
		assert.Equal(t, http.StatusOK, w.Code)
	})
}
