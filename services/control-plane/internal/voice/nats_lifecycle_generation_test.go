package voice_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	voice "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestOldGenerationVoiceLeftRechecksRoomGenerationAfterLifecycleLock(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "old_generation_lock_wait")
	serverID := ts.CreateTestServer(t, user.ID, "Old Generation Lock Wait")
	channelID := ts.CreateVoiceChannel(t, serverID, "old-generation-lock-wait")
	subscriber := newTestSubscriber(ts)
	subscriber.SetLifecyclePlacementEnabled(true)
	eventAt := time.Now().UTC().Truncate(time.Microsecond)
	oldAt := eventAt.Add(-time.Minute)
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at) VALUES ($1, $2, $3)`, channelID, user.ID, oldAt)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Set(t.Context(), "voice:room_owner:"+channelID, "blue", time.Minute).Err())
	require.NoError(t, ts.Redis.Set(t.Context(), "voice:room_live:"+channelID, "blue-1", time.Minute).Err())
	require.NoError(t, ts.Redis.Expire(t.Context(), "voice:room_owner:"+channelID, 5*time.Second).Err())
	frame, err := json.Marshal(map[string]any{
		"channelId": channelID, "userId": user.ID,
		"colour": "blue", "instance_id": "blue-1",
		"timestamp": eventAt.Format(time.RFC3339Nano),
	})
	require.NoError(t, err)

	userUUID := uuid.MustParse(user.ID)
	locker, err := ts.DB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = locker.Rollback() })
	require.NoError(t, voice.LockServerVoiceLifecycleTx(t.Context(), locker, userUUID))
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		subscriber.HandleVoiceLifecycleEventForTest("voice.left", frame, eventAt)
	}()
	// The matching old-generation event must pass the real dispatcher guard
	// before it waits on SQL; owner-lease renewal is the observable proof.
	require.Eventually(t, func() bool {
		ttl, ttlErr := ts.Redis.TTL(t.Context(), "voice:room_owner:"+channelID).Result()
		return ttlErr == nil && ttl > 5*time.Second
	}, 3*time.Second, 10*time.Millisecond,
		"positive control: matching voice.left must pass ingress and renew its owner lease before waiting for SQL")
	lockKey, err := voice.ServerVoiceLifecycleAdvisoryKeyForTest(userUUID)
	require.NoError(t, err)
	testdb.WaitForAdvisoryLockWaiter(t, ts.DB, lockKey)
	require.NoError(t, ts.Redis.Set(t.Context(), "voice:room_live:"+channelID, "blue-2", time.Minute).Err())
	require.NoError(t, locker.Commit())
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("old-generation lifecycle handler did not finish after SQL lock release")
	}
	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, channelID, user.ID).Scan(&count))
	require.Equal(t, 1, count, "old-generation voice.left must not delete the successor participant after voice:room_live changes while it waits")
}
