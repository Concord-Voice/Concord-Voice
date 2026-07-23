package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// #2201 (Codex #2397 review, finding 920): rotateAndRespond must not mint a
// successor session for a refresh token whose row was revoked by a concurrent
// destructive credential reset. The reset holds the user row FOR NO KEY UPDATE
// and bulk-revokes; a refresh that fetched the token as active but reaches
// rotation after the reset committed hits the conditional revoke
// (WHERE revoked_at IS NULL RETURNING) with zero rows and MUST fail closed.
//
// A pre-revoked source row is the deterministic stand-in for "the reset won the
// race": the endpoint's fetchActiveRefreshToken filters revoked tokens (routing
// them to the grace path), so this narrow window is only reachable by calling
// rotateAndRespond directly.
func TestRotateAndRespond_RejectsRevokedSourceToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, _ := dbtest.SetupTestDB(t)
	userID := dbtest.CreateUser(t, db)

	// The source token exists but is ALREADY revoked (reset won the race).
	tokenID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, revoked_at)
		 VALUES ($1, $2, $3, NOW() + INTERVAL '30 days', NOW())`,
		tokenID, userID, "revoked-source-"+uuid.NewString(),
	)
	require.NoError(t, err)

	// A fully wired Handler: the reject path returns before touching redis/entCache,
	// but wiring them means a reverted guard mints cleanly and this test fails on the
	// security-relevant assertion (a live session appeared) instead of a nil panic.
	rdb := setupAuthAttemptRedis(t)
	h := &Handler{
		db:       db,
		redis:    rdb,
		log:      logger.NewWithWriter(io.Discard),
		entCache: entitlements.NewCache(rdb, db),
	}

	resp := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(resp)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)

	h.rotateAndRespond(c, models.RefreshToken{
		ID:        tokenID.String(),
		UserID:    userID.String(),
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}, "")

	assert.Equal(t, http.StatusUnauthorized, resp.Code,
		"a refresh whose source token a reset already revoked must be rejected")

	// No successor token was minted — the only row remains the revoked source.
	var total, active int
	require.NoError(t, db.QueryRow(
		`SELECT count(*), count(*) FILTER (WHERE revoked_at IS NULL) FROM refresh_tokens WHERE user_id = $1`,
		userID,
	).Scan(&total, &active))
	assert.Equal(t, 1, total, "no new refresh_tokens row may be inserted")
	assert.Zero(t, active, "no live session may exist after the reject")
}
