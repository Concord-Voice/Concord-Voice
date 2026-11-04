package auth

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// A failed lineage walk logs a fixed reason and leaves the 401 to the caller.
func TestLogReplayWouldRevokeWalkFailureLogsOnly(t *testing.T) {
	h, _, userID := newRejectPathHandler(t)
	var buf bytes.Buffer
	h.log = logger.NewWithWriter(&buf)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil).WithContext(ctx)
	c.Set(middleware.RequestIDContextKey, "req-walk-failure")

	h.logReplayWouldRevoke(c, uuid.NewString(), userID.String())

	assert.Contains(t, buf.String(), "error_class=replay_lineage_walk")
	assert.Contains(t, buf.String(), "request_id=req-walk-failure")
	assert.NotContains(t, buf.String(), "would_revoke")
	assert.Equal(t, http.StatusOK, w.Code, "the helper never writes a response")
}

func TestLogReplayWouldRevokeDepthBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		depth    int
		complete bool
	}{
		{name: "live terminal below cap", depth: replayLineageMaxDepth - 1, complete: true},
		{name: "live terminal exactly at cap", depth: replayLineageMaxDepth},
		{name: "only live descendant beyond cap", depth: replayLineageMaxDepth + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			userID := dbtest.CreateUser(t, db)
			// Build one deterministic chain in a single statement. Only its terminal
			// row is live, so stopping before that row cannot yield a complete count.
			result, err := db.Exec(`
				WITH chain AS (
					SELECT n, (left($1::text, 24) || lpad(n::text, 12, '0'))::uuid AS id
					FROM generate_series(0, $2::integer) AS n
				)
				INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, revoked_at, predecessor_id)
				SELECT c.id, $1::uuid, $1::text || '-replay-depth-' || c.n::text,
				       NOW() + INTERVAL '1 day',
				       CASE WHEN c.n = $2 THEN NULL ELSE NOW() - INTERVAL '31 seconds' END,
				       p.id
				FROM chain c LEFT JOIN chain p ON p.n = c.n - 1`, userID.String(), tc.depth)
			require.NoError(t, err)
			rows, err := result.RowsAffected()
			require.NoError(t, err)
			require.Equal(t, int64(tc.depth+1), rows)
			var rootID string
			require.NoError(t, db.QueryRow(`SELECT id FROM refresh_tokens WHERE user_id = $1 AND predecessor_id IS NULL`, userID).Scan(&rootID))
			const snapshot = `SELECT json_agg(rt ORDER BY id)::text FROM refresh_tokens rt WHERE user_id = $1`
			var before, after string
			require.NoError(t, db.QueryRow(snapshot, userID).Scan(&before))
			var buf bytes.Buffer
			h := &Handler{db: db, log: logger.NewWithWriter(&buf)}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)

			h.logReplayWouldRevoke(c, rootID, userID.String())

			logs := buf.String()
			assert.Contains(t, logs, "lineage_depth="+strconv.Itoa(min(tc.depth, replayLineageMaxDepth)))
			assert.Contains(t, logs, "mode=log_only")
			assert.NotContains(t, logs, "error_class=replay_lineage_walk")
			if tc.complete {
				assert.Contains(t, logs, "Refresh replay would revoke lineage")
				assert.Contains(t, logs, "would_revoke=true")
				assert.Contains(t, logs, "live_successors=1")
				assert.NotContains(t, logs, "Replay lineage observation incomplete")
			} else {
				assert.Contains(t, logs, "Replay lineage observation incomplete")
				assert.Contains(t, logs, "failure_class=replay_lineage_depth_limit")
				assert.NotContains(t, logs, "would_revoke")
				assert.NotContains(t, logs, "live_successors")
				assert.NotContains(t, logs, "Refresh replay would revoke lineage")
			}
			assert.False(t, c.Writer.Written(), "the helper must leave the caller's response unwritten")
			assert.Empty(t, w.Body.String())
			require.NoError(t, db.QueryRow(snapshot, userID).Scan(&after))
			assert.Equal(t, before, after, "log-only observation must leave every token unchanged")
		})
	}
}
