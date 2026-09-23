package rbac_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSetChannelPermissionSync_RetriesWhenChannelBecomesVoice(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "sync-type-race-owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Sync type race")
	var categoryID string
	require.NoError(t, ts.DB.QueryRow(`INSERT INTO channel_groups (id, server_id, name, position) VALUES (gen_random_uuid(), $1, 'sync-type-race', 0) RETURNING id`, serverID).Scan(&categoryID))
	channelID := ts.CreateTestChannel(t, serverID, "sync-type-race-channel")
	_, err := ts.DB.Exec(`UPDATE channels SET group_id = $1, sync_permissions = FALSE, type = 'text' WHERE id = $2`, categoryID, channelID)
	require.NoError(t, err)

	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
	h := rbac.NewHandler(ts.DB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)
	capture := &categoryCaptureScopeRecorder{}
	h.SetPresenceRecheck(capture)
	preflightReached := make(chan struct{})
	var once sync.Once
	rbac.SetSyncedCategoryPreflightForTest(h, func() { once.Do(func() { close(preflightReached) }) })

	// Hold the visibility lock, then change the channel type. The first attempt
	// captured the text-only state; the retry must capture the newly-voice child.
	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), tx, serverID))
	defer tx.Rollback() //nolint:errcheck // cleanup after commit is a no-op

	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: channelID}}
		c.Set("user_id", owner.ID)
		body, _ := json.Marshal(map[string]bool{"sync_permissions": true})
		c.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.SetChannelPermissionSync(c)
		completed <- w
	}()

	<-preflightReached
	_, err = tx.Exec(`UPDATE channels SET type = 'voice' WHERE id = $1`, channelID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	w := <-completed
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, [][]string{{}, {channelID}}, capture.prepared)
}
