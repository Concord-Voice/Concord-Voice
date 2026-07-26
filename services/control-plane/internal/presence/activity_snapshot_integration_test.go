package presence_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type activitySnapshotVisibility struct {
	allow bool
}

func (v activitySnapshotVisibility) FilterVisibleUserIDsForChannelFresh(
	_ context.Context,
	_ string,
	_ string,
	candidateUserIDs []string,
) ([]string, error) {
	if !v.allow {
		return []string{}, nil
	}
	return candidateUserIDs, nil
}

func TestActivitySnapshot_PostgresCandidateSupersetAndFreshAuthorization(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(dbCleanup)
	t.Cleanup(redisCleanup)
	ctx := context.Background()

	t.Run("server voice candidate is authorized against fresh policy", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		require.NoError(t, redisClient.FlushDB(ctx).Err())
		viewerID := testhelpers.CreateUser(t, db)
		senderID, serverID, channelID := createServerVoiceBuilderFixture(t, db, "Concord", "General")
		testhelpers.AddServerMember(t, db, serverID, viewerID)
		lifecycleAt := time.Date(2026, 7, 15, 1, 0, 0, 125000000, time.UTC)
		_, err := db.Exec(`
			INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
			VALUES ($1, $2, $3, $3)
		`, channelID, senderID, lifecycleAt)
		require.NoError(t, err)
		_, err = db.Exec(`
			INSERT INTO user_presence_settings (
				user_id, server_voice_tier, server_voice_show_details
			) VALUES ($1, 2, TRUE)
		`, senderID)
		require.NoError(t, err)

		store := presence.NewActivityStore(redisClient)
		state := presence.ActivityState{
			SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
			Payload: json.RawMessage(`{"stale":true}`), UpdatedAt: lifecycleAt.Unix(),
		}
		stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryServerVoice, state)
		require.NoError(t, err)
		require.True(t, stored)
		seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryServerVoice, state)
		service := presence.NewActivitySnapshotService(
			db,
			presence.NewActivityBuilder(db, nil, store),
			store,
			activitySnapshotVisibility{allow: true},
			nil,
			permitAllPresence{},
		)

		snapshot, err := service.Snapshot(ctx, viewerID)
		require.NoError(t, err)
		entry := snapshot[senderID][presence.CategoryServerVoice]
		assert.False(t, entry.Minimized)
		assert.Equal(t, lifecycleAt.Unix(), entry.UpdatedAt)
		assert.JSONEq(t, `{
			"channel_id":"`+channelID.String()+`",
			"channel_name":"General",
			"server_id":"`+serverID.String()+`",
			"server_name":"Concord",
			"started_at":`+jsonInt(lifecycleAt.Unix())+`
		}`, string(entry.Payload))
	})

	t.Run("hidden exact voice channel is omitted", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		require.NoError(t, redisClient.FlushDB(ctx).Err())
		viewerID := testhelpers.CreateUser(t, db)
		senderID, serverID, channelID := createServerVoiceBuilderFixture(t, db, "Hidden", "Secret")
		testhelpers.AddServerMember(t, db, serverID, viewerID)
		lifecycleAt := time.Date(2026, 7, 15, 1, 10, 0, 0, time.UTC)
		_, err := db.Exec(`
			INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
			VALUES ($1, $2, $3, $3)
		`, channelID, senderID, lifecycleAt)
		require.NoError(t, err)
		_, err = db.Exec(`
			INSERT INTO user_presence_settings (user_id, server_voice_tier)
			VALUES ($1, 2)
		`, senderID)
		require.NoError(t, err)

		store := presence.NewActivityStore(redisClient)
		state := presence.ActivityState{
			SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
			Payload: json.RawMessage(`{"stale":true}`), UpdatedAt: lifecycleAt.Unix(),
		}
		stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryServerVoice, state)
		require.NoError(t, err)
		require.True(t, stored)
		seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryServerVoice, state)
		service := presence.NewActivitySnapshotService(
			db, presence.NewActivityBuilder(db, nil, store), store,
			activitySnapshotVisibility{allow: false}, nil,
			permitAllPresence{},
		)

		snapshot, err := service.Snapshot(ctx, viewerID)
		require.NoError(t, err)
		assert.NotContains(t, snapshot, senderID)
		_, found, err := store.Get(ctx, senderID, presence.CategoryServerVoice)
		require.NoError(t, err)
		assert.True(t, found, "viewer exclusion must not delete state valid for another viewer")
	})

	t.Run("stale viewer server membership is omitted", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		require.NoError(t, redisClient.FlushDB(ctx).Err())
		viewerID := testhelpers.CreateUser(t, db)
		senderID, serverID, channelID := createServerVoiceBuilderFixture(t, db, "Former Viewer", "General")
		testhelpers.AddServerMember(t, db, serverID, viewerID)
		lifecycleAt := time.Date(2026, 7, 15, 1, 15, 0, 0, time.UTC)
		_, err := db.Exec(`
			INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
			VALUES ($1, $2, $3, $3)
		`, channelID, senderID, lifecycleAt)
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, viewerID)
		require.NoError(t, err)

		store := presence.NewActivityStore(redisClient)
		state := presence.ActivityState{
			SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
			Payload: json.RawMessage(`{"stale":true}`), UpdatedAt: lifecycleAt.Unix(),
		}
		stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryServerVoice, state)
		require.NoError(t, err)
		require.True(t, stored)
		seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryServerVoice, state)
		service := presence.NewActivitySnapshotService(
			db, presence.NewActivityBuilder(db, nil, store), store,
			activitySnapshotVisibility{allow: true}, nil,
			permitAllPresence{},
		)

		snapshot, err := service.Snapshot(ctx, viewerID)
		require.NoError(t, err)
		assert.NotContains(t, snapshot, senderID)
		_, found, err := store.Get(ctx, senderID, presence.CategoryServerVoice)
		require.NoError(t, err)
		assert.True(t, found, "viewer removal must not delete state valid for current members")
	})

	t.Run("stale server member row is omitted and cleaned", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		require.NoError(t, redisClient.FlushDB(ctx).Err())
		viewerID := testhelpers.CreateUser(t, db)
		senderID, serverID, channelID := createServerVoiceBuilderFixture(t, db, "Former", "General")
		testhelpers.AddServerMember(t, db, serverID, viewerID)
		lifecycleAt := time.Date(2026, 7, 15, 1, 20, 0, 0, time.UTC)
		_, err := db.Exec(`
			INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
			VALUES ($1, $2, $3, $3)
		`, channelID, senderID, lifecycleAt)
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, senderID)
		require.NoError(t, err)

		store := presence.NewActivityStore(redisClient)
		state := presence.ActivityState{
			SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
			Payload: json.RawMessage(`{"stale":true}`), UpdatedAt: lifecycleAt.Unix(),
		}
		stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryServerVoice, state)
		require.NoError(t, err)
		require.True(t, stored)
		seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryServerVoice, state)
		service := presence.NewActivitySnapshotService(
			db, presence.NewActivityBuilder(db, nil, store), store,
			activitySnapshotVisibility{allow: true}, nil,
			permitAllPresence{},
		)

		snapshot, err := service.Snapshot(ctx, viewerID)
		require.NoError(t, err)
		assert.NotContains(t, snapshot, senderID)
		_, found, err := store.Get(ctx, senderID, presence.CategoryServerVoice)
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("private call candidate includes asymmetric sender opted-in friend of friend", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		require.NoError(t, redisClient.FlushDB(ctx).Err())
		senderID := testhelpers.CreateUser(t, db)
		peerID := testhelpers.CreateUser(t, db)
		mutualID := testhelpers.CreateUser(t, db)
		viewerID := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, senderID, mutualID)
		testhelpers.AddFriendship(t, db, mutualID, viewerID)
		testhelpers.SetFriendsOfFriends(t, db, senderID, true)
		testhelpers.SetFriendsOfFriends(t, db, viewerID, false)
		conversationID := uuid.New()
		callID := uuid.New()
		lifecycleAt := time.Date(2026, 7, 15, 2, 0, 0, 250000000, time.UTC)
		createPrivateCallBuilderFixture(t, db, conversationID, false, map[uuid.UUID]time.Time{
			senderID: lifecycleAt,
			peerID:   lifecycleAt,
		})
		_, err := db.Exec(`
			INSERT INTO user_presence_settings (
				user_id, private_call_tier, private_call_show_details
			) VALUES ($1, 1, FALSE)
		`, senderID)
		require.NoError(t, err)

		store := presence.NewActivityStore(redisClient)
		state := presence.ActivityState{
			SourceToken: callID, SourceVersion: lifecycleAt.UnixMicro(),
			Payload: json.RawMessage(`{"stale":true}`), UpdatedAt: lifecycleAt.Unix(),
		}
		stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryPrivateCall, state)
		require.NoError(t, err)
		require.True(t, stored)
		seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryPrivateCall, state)
		seedActivitySnapshotLifecycle(t, redisClient, peerID, presence.CategoryPrivateCall, state)
		service := presence.NewActivitySnapshotService(
			db,
			presence.NewActivityBuilder(db, &activityLeaseVerifier{match: true}, store),
			store,
			activitySnapshotVisibility{allow: true},
			nil,
			permitAllPresence{},
		)

		snapshot, err := service.Snapshot(ctx, viewerID)
		require.NoError(t, err)
		entry := snapshot[senderID][presence.CategoryPrivateCall]
		assert.True(t, entry.Minimized)
		assert.JSONEq(t, `{"call_type":"dm"}`, string(entry.Payload))
	})

	t.Run("private tier off still includes current same-call stranger", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		require.NoError(t, redisClient.FlushDB(ctx).Err())
		senderID := testhelpers.CreateUser(t, db)
		viewerID := testhelpers.CreateUser(t, db)
		conversationID := uuid.New()
		callID := uuid.New()
		lifecycleAt := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC)
		createPrivateCallBuilderFixture(t, db, conversationID, false, map[uuid.UUID]time.Time{
			senderID: lifecycleAt,
			viewerID: lifecycleAt,
		})

		store := presence.NewActivityStore(redisClient)
		state := presence.ActivityState{
			SourceToken: callID, SourceVersion: lifecycleAt.UnixMicro(),
			Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: lifecycleAt.Unix(),
		}
		stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryPrivateCall, state)
		require.NoError(t, err)
		require.True(t, stored)
		seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryPrivateCall, state)
		seedActivitySnapshotLifecycle(t, redisClient, viewerID, presence.CategoryPrivateCall, state)
		service := presence.NewActivitySnapshotService(
			db,
			presence.NewActivityBuilder(db, &activityLeaseVerifier{match: true}, store),
			store,
			activitySnapshotVisibility{allow: true},
			nil,
			permitAllPresence{},
		)

		snapshot, err := service.Snapshot(ctx, viewerID)
		require.NoError(t, err)
		assert.Contains(t, snapshot[senderID], presence.CategoryPrivateCall)
	})

	t.Run("ended private call lease is omitted and cleaned", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		require.NoError(t, redisClient.FlushDB(ctx).Err())
		senderID := testhelpers.CreateUser(t, db)
		viewerID := testhelpers.CreateUser(t, db)
		conversationID := uuid.New()
		callID := uuid.New()
		lifecycleAt := time.Date(2026, 7, 15, 3, 10, 0, 0, time.UTC)
		createPrivateCallBuilderFixture(t, db, conversationID, false, map[uuid.UUID]time.Time{
			senderID: lifecycleAt,
			viewerID: lifecycleAt,
		})

		store := presence.NewActivityStore(redisClient)
		state := presence.ActivityState{
			SourceToken: callID, SourceVersion: lifecycleAt.UnixMicro(),
			Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: lifecycleAt.Unix(),
		}
		stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryPrivateCall, state)
		require.NoError(t, err)
		require.True(t, stored)
		seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryPrivateCall, state)
		service := presence.NewActivitySnapshotService(
			db,
			presence.NewActivityBuilder(db, &activityLeaseVerifier{match: false}, store),
			store,
			activitySnapshotVisibility{allow: true},
			nil,
			permitAllPresence{},
		)

		snapshot, err := service.Snapshot(ctx, viewerID)
		require.NoError(t, err)
		assert.NotContains(t, snapshot, senderID)
		_, found, err := store.Get(ctx, senderID, presence.CategoryPrivateCall)
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("terminal participant fence with lingering row is omitted and cleaned", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		require.NoError(t, redisClient.FlushDB(ctx).Err())
		senderID := testhelpers.CreateUser(t, db)
		formerParticipantID := testhelpers.CreateUser(t, db)
		conversationID := uuid.New()
		callID := uuid.New()
		lifecycleAt := time.Date(2026, 7, 15, 3, 20, 0, 0, time.UTC)
		createPrivateCallBuilderFixture(t, db, conversationID, false, map[uuid.UUID]time.Time{
			senderID:            lifecycleAt,
			formerParticipantID: lifecycleAt,
		})

		store := presence.NewActivityStore(redisClient)
		state := presence.ActivityState{
			SourceToken: callID, SourceVersion: lifecycleAt.UnixMicro(),
			Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: lifecycleAt.Unix(),
		}
		stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryPrivateCall, state)
		require.NoError(t, err)
		require.True(t, stored)
		seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryPrivateCall, state)
		seedActivitySnapshotLifecycleActive(
			t, redisClient, formerParticipantID, presence.CategoryPrivateCall, state, false,
		)
		service := presence.NewActivitySnapshotService(
			db,
			presence.NewActivityBuilder(db, &activityLeaseVerifier{match: true}, store),
			store,
			activitySnapshotVisibility{allow: true},
			nil,
			permitAllPresence{},
		)

		snapshot, err := service.Snapshot(ctx, formerParticipantID)
		require.NoError(t, err)
		assert.NotContains(t, snapshot, senderID)
		_, found, err := store.Get(ctx, senderID, presence.CategoryPrivateCall)
		require.NoError(t, err)
		assert.False(t, found, "a stale participant set must fail closed for every reconnect")
	})
}

func seedActivitySnapshotLifecycle(
	t *testing.T,
	redisClient *redis.Client,
	senderID uuid.UUID,
	category presence.Category,
	state presence.ActivityState,
) {
	t.Helper()
	seedActivitySnapshotLifecycleActive(t, redisClient, senderID, category, state, true)
}

func seedActivitySnapshotLifecycleActive(
	t *testing.T,
	redisClient *redis.Client,
	senderID uuid.UUID,
	category presence.Category,
	state presence.ActivityState,
	active bool,
) {
	t.Helper()
	key, err := presence.VoiceLifecycleKey(senderID, category)
	require.NoError(t, err)
	activeValue := "0"
	if active {
		activeValue = "1"
	}
	require.NoError(t, redisClient.HSet(context.Background(), key,
		"token", state.SourceToken.String(),
		"version", state.SourceVersion,
		"active", activeValue,
	).Err())
	require.NoError(t, redisClient.PExpire(
		context.Background(), key, presence.ActivityStateTTL,
	).Err())
}

func jsonInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
