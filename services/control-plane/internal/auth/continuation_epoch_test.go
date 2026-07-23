package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// #2201 (Codex #2397 review, F1): the post-rotation continuation pair must bind
// to the epoch the destructive flow committed. If a concurrent second flow
// advanced users.credential_epoch in the window between that commit and this
// best-effort mint, IssueTokenPair must skip — no live pair, no refresh row —
// so the earlier request cannot receive a session the later flow meant to kill.

func TestIssueTokenPair_SkipsWhenEpochAdvanced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, _ := dbtest.SetupTestDB(t)
	userID := dbtest.CreateUser(t, db)

	// The durable epoch is "currentEpoch"; the caller's flow committed
	// "staleEpoch" and a later flow advanced it — the mismatch must skip.
	_, err := db.Exec(`UPDATE users SET credential_epoch = 'currentEpoch' WHERE id = $1`, userID)
	require.NoError(t, err)

	// The mismatch is rejected before entCache/token minting, so a DB-only
	// handler exercises the guard in isolation.
	h := &Handler{db: db, log: logger.NewWithWriter(io.Discard)}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	_, _, _, err = h.IssueTokenPair(c, userID.String(), "staleEpoch")
	assert.ErrorIs(t, err, ErrContinuationEpochAdvanced,
		"a continuation mint under a superseded epoch must be refused")

	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id = $1`, userID).Scan(&n))
	assert.Zero(t, n, "no refresh row may be minted when the epoch advanced")
}

func TestIssueTokenPair_MintsWhenEpochMatches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, _ := dbtest.SetupTestDB(t)
	rdb := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)

	_, err := db.Exec(`UPDATE users SET credential_epoch = 'e1', email_verified = true WHERE id = $1`, userID)
	require.NoError(t, err)

	h := &Handler{
		db:        db,
		redis:     rdb,
		log:       logger.NewWithWriter(io.Discard),
		jwtSecret: "test-jwt-secret-continuation",
		entCache:  entitlements.NewCache(rdb, db),
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	access, refresh, sessionID, err := h.IssueTokenPair(c, userID.String(), "e1")
	require.NoError(t, err)
	assert.NotEmpty(t, access)
	assert.NotEmpty(t, refresh)
	assert.NotEmpty(t, sessionID)

	var storedID string
	require.NoError(t, db.QueryRow(
		`SELECT id FROM refresh_tokens WHERE user_id = $1`, userID).Scan(&storedID))
	assert.Equal(t, sessionID, storedID, "the minted refresh row must carry the returned session id")
}
