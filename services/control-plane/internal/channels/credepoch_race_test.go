package channels_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
)

// #2201 AC-2: a channel creation (which stores wrapped keys) admitted before a
// destructive key reset (stale-cache window) must be stopped by GuardTx — no
// channel row and no wrapped-key rows may land under the superseded epoch.
func TestCredEpochRace_CreateChannelRejected(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "racechanowner")
	serverID := ts.CreateTestServer(t, owner.ID, "Race Channel Server")

	stale := ts.SimulateStaleEpochWindow(t, owner.ID)

	w := ts.DoRequest("POST", "/api/v1/channels", map[string]interface{}{
		"server_id": serverID,
		"name":      "fenced-channel",
		"type":      "text",
		"wrapped_keys": map[string]string{
			owner.ID: "d3JhcHBlZC1rZXktb3duZXI=",
		},
	}, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "GuardTx must reject the admitted stale channel creation")

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM channels WHERE server_id = $1 AND name = 'fenced-channel'`, serverID).Scan(&count))
	assert.Zero(t, count, "no channel may be created under the old epoch")
}

// #2530: a manual channel rotation admitted with E1 must not commit or notify
// after the durable credential epoch has advanced to E2.
func TestCredEpochRace_ManualRotationRejected(t *testing.T) {
	ts, owner, _, channelID := setupEncryptedChannel(t)

	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	conn, _, err := gorillaWS.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(wsServer.URL, "http")+"/api/v1/ws?token="+url.QueryEscape(owner.AccessToken), nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		if closeErr := conn.Close(); closeErr != nil {
			t.Errorf("close websocket: %v", closeErr)
		}
	})
	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(owner.ID)) > 0
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err = conn.ReadMessage() // connection bootstrap
	require.NoError(t, err)

	stale := ts.SimulateStaleEpochWindow(t, owner.ID)
	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "GuardTx must reject the admitted stale manual rotation")
	assert.JSONEq(t, `{"error":"Authentication required"}`, w.Body.String())

	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM key_revocations WHERE channel_id = $1`, channelID).Scan(&count))
	assert.Zero(t, count, "no revocation may be recorded under the old epoch")

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(250*time.Millisecond)))
	for {
		_, frame, readErr := conn.ReadMessage()
		if readErr != nil {
			var netErr net.Error
			require.ErrorAs(t, readErr, &netErr)
			assert.True(t, netErr.Timeout())
			break
		}
		var message struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(frame, &message))
		assert.NotEqual(t, "key_revocation", message.Type, "a rejected manual rotation must not broadcast")
	}
}

func TestCredEpochRace_ManualRotationStaleAttemptsDoNotConsumeChannelQuota(t *testing.T) {
	ts, owner, _, channelID := setupEncryptedChannel(t)

	stale := ts.SimulateStaleEpochWindow(t, owner.ID)
	for attempt := 1; attempt <= 10; attempt++ {
		w := ts.DoRequest(http.MethodPost, pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(stale))
		require.Equal(t, http.StatusUnauthorized, w.Code, "stale attempt %d", attempt)
		assert.JSONEq(t, `{"error":"Authentication required"}`, w.Body.String())
	}

	var staleRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM key_revocations WHERE channel_id = $1`, channelID,
	).Scan(&staleRows))
	assert.Zero(t, staleRows)

	// The route's independent 10/min user cap is intentionally outside this
	// regression. Clear it as TestRotateKeyRateLimitBlocks11th does so this
	// request proves only the 10/24h channel resource bucket is uncharged.
	userRLKey := fmt.Sprintf("ratelimit:user:%s:POST:/api/v1/channels/:id/rotate-key", owner.ID)
	require.NoError(t, ts.Redis.Del(context.Background(), userRLKey).Err())

	var durableEpoch string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT credential_epoch FROM users WHERE id = $1`, owner.ID,
	).Scan(&durableEpoch))
	require.NoError(t, ts.Redis.Set(context.Background(), credepoch.Key(owner.ID), "active:"+durableEpoch, 5*time.Minute).Err())
	fresh, err := auth.GenerateAccessToken(owner.ID, testhelpers.TestJWTSecret, true, durableEpoch, "")
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodPost, pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(fresh))
	require.Equal(t, http.StatusOK, w.Code, "a durable-E2 request must retain the channel quota")
	var committedRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM key_revocations WHERE channel_id = $1`, channelID,
	).Scan(&committedRows))
	assert.Equal(t, 1, committedRows)
}

// #2201: a GuardTx read failure inside the channel-create transaction (fence
// cache seeded, users renamed) maps to the generic 500, not the epoch 401
// (Codex #2397 review).
func TestCredEpochRace_CreateChannelGuardReadErrorFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "createguarderr")
	serverID := ts.CreateTestServer(t, owner.ID, "Create Guard Error Server")

	require.NoError(t, ts.Redis.Set(context.Background(), credepoch.Key(owner.ID), "none", 5*time.Minute).Err())
	_, err := ts.DB.Exec("ALTER TABLE users RENAME TO users_createguarderr")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec("ALTER TABLE users_createguarderr RENAME TO users"); err != nil {
			t.Errorf("cleanup: failed to rename users back: %v", err)
		}
	})

	w := ts.DoRequest("POST", "/api/v1/channels", map[string]interface{}{
		"server_id": serverID,
		"name":      "guard-err-channel",
		"type":      "text",
		"wrapped_keys": map[string]string{
			owner.ID: "d3JhcHBlZC1rZXktb3duZXI=",
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// #2201: a GuardTx read failure inside the distribution transaction (fault-
// injected by renaming users while the fence answers from cache) maps to the
// generic 500 — a store failure is never an epoch-mismatch 401.
func TestCredEpochRace_DistributionGuardReadErrorFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "raceguarderr")
	serverID := ts.CreateTestServer(t, owner.ID, "Guard Error Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// The caller must already hold a channel key to distribute.
	_, err := ts.DB.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, 'a2V5', 1)`,
		channelID, owner.ID)
	require.NoError(t, err)

	// Seed the fence cache so middleware passes without the users table, then
	// take the table away so GuardTx's FOR SHARE read errors inside the tx.
	require.NoError(t, ts.Redis.Set(context.Background(), credepoch.Key(owner.ID), "none", 5*time.Minute).Err())
	_, err = ts.DB.Exec("ALTER TABLE users RENAME TO users_guarderrtest")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec("ALTER TABLE users_guarderrtest RENAME TO users"); err != nil {
			t.Errorf("cleanup: failed to rename users back: %v", err)
		}
	})

	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{owner.ID: "d3JhcHBlZC1rZXk="},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
