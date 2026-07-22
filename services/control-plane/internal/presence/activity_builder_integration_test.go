package presence_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type activityLeaseVerifier struct {
	match         bool
	matchSequence []bool
	err           error
	calls         [][2]uuid.UUID
}

type activityGenerationVerifier struct {
	results []bool
	err     error
	calls   [][]presence.ActivityGeneration
}

func (v *activityGenerationVerifier) VerifyActiveGenerations(
	_ context.Context,
	generations []presence.ActivityGeneration,
) ([]bool, error) {
	v.calls = append(v.calls, append([]presence.ActivityGeneration(nil), generations...))
	if v.err != nil {
		return nil, v.err
	}
	if v.results != nil {
		return append([]bool(nil), v.results...), nil
	}
	results := make([]bool, len(generations))
	for index := range results {
		results[index] = true
	}
	return results, nil
}

func (v *activityLeaseVerifier) Matches(
	_ context.Context,
	conversationID uuid.UUID,
	callID uuid.UUID,
) (bool, error) {
	v.calls = append(v.calls, [2]uuid.UUID{conversationID, callID})
	if len(v.matchSequence) > 0 {
		match := v.matchSequence[0]
		v.matchSequence = v.matchSequence[1:]
		return match, v.err
	}
	return v.match, v.err
}

func TestActivityBuilder_PostgresAuthoritativeState(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	t.Run("server voice uses current membership names and stable start", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		senderID, serverID, channelID := createServerVoiceBuilderFixture(t, db, "Concord", "General")
		joinedAt := time.Date(2026, 7, 14, 18, 30, 0, 0, time.UTC)
		lifecycleAt := joinedAt.Add(5*time.Minute + 321*time.Microsecond)
		_, err := db.Exec(`
			INSERT INTO voice_participants (
				channel_id, user_id, joined_at, lifecycle_event_at
			) VALUES ($1, $2, $3, $4)
		`, channelID, senderID, joinedAt, lifecycleAt)
		require.NoError(t, err)

		builder := presence.NewActivityBuilder(db, nil)
		built, err := builder.Build(ctx, senderID, presence.Scope{
			Category:    presence.CategoryServerVoice,
			RoomID:      channelID,
			LifecycleID: channelID,
			EventAt:     lifecycleAt.Add(time.Minute),
		})
		require.NoError(t, err)
		require.NotNil(t, built.Input.ServerVoice)
		assert.Equal(t, senderID, built.Input.SenderID)
		assert.Equal(t, presence.CategoryServerVoice, built.Input.Category)
		assert.Equal(t, channelID, built.SourceToken)
		assert.Equal(t, lifecycleAt.UnixMicro(), built.SourceVersion)
		assert.Equal(t, presence.ServerVoiceContext{
			ServerID: serverID, ChannelID: channelID,
		}, built.Input.ServerVoice.Context)
		assert.Equal(t, serverID, built.Input.ServerVoice.Payload.ServerID)
		assert.Equal(t, channelID, built.Input.ServerVoice.Payload.ChannelID)
		assert.Equal(t, "Concord", built.Input.ServerVoice.Payload.ServerName)
		assert.Equal(t, "General", built.Input.ServerVoice.Payload.ChannelName)
		require.NotNil(t, built.Input.ServerVoice.Payload.StartedAt)
		assert.Equal(t, joinedAt.Unix(), *built.Input.ServerVoice.Payload.StartedAt)
	})

	t.Run("server voice rejects join timestamps above the exact JSON Lua second ceiling", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		senderID, _, channelID := createServerVoiceBuilderFixture(
			t, db, "Safe Seconds", "Too Far Future",
		)
		joinedAt := time.Unix(presence.MaxActivityUnixSeconds+1, 0).UTC()
		lifecycleAt := time.Date(2026, 7, 14, 18, 40, 0, 123456000, time.UTC)
		_, err := db.Exec(`
			INSERT INTO voice_participants (
				channel_id, user_id, joined_at, lifecycle_event_at
			) VALUES ($1, $2, $3, $4)
		`, channelID, senderID, joinedAt, lifecycleAt)
		require.NoError(t, err)

		builder := presence.NewActivityBuilder(db, nil)
		_, err = builder.Build(ctx, senderID, presence.Scope{
			Category: presence.CategoryServerVoice, RoomID: channelID,
			LifecycleID: channelID, EventAt: lifecycleAt,
		})
		assert.ErrorIs(t, err, presence.ErrActivityNotCurrent)
	})

	t.Run("server voice rejects missing membership and ambiguous active scope", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		senderID, firstServerID, channelID := createServerVoiceBuilderFixture(t, db, "Concord", "General")
		lifecycleAt := time.Date(2026, 7, 14, 19, 0, 0, 0, time.UTC)
		_, err := db.Exec(`
			INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at)
			VALUES ($1, $2, $3)
		`, channelID, senderID, lifecycleAt)
		require.NoError(t, err)
		builder := presence.NewActivityBuilder(db, nil)
		scope := presence.Scope{
			Category: presence.CategoryServerVoice, RoomID: channelID,
			LifecycleID: channelID, EventAt: lifecycleAt,
		}

		_, err = db.Exec(`DELETE FROM server_members WHERE user_id = $1`, senderID)
		require.NoError(t, err)
		_, err = builder.Build(ctx, senderID, scope)
		assert.ErrorIs(t, err, presence.ErrActivityNotCurrent)
		testhelpers.AddServerMember(t, db, firstServerID, senderID)

		_, _, secondChannelID := createServerVoiceBuilderFixtureForUser(
			t, db, senderID, "Elsewhere", "Lounge",
		)
		_, err = db.Exec(`
			INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at)
			VALUES ($1, $2, $3)
		`, secondChannelID, senderID, lifecycleAt.Add(time.Second))
		require.NoError(t, err)
		_, err = builder.Build(ctx, senderID, scope)
		assert.ErrorIs(t, err, presence.ErrActivityNotCurrent)
	})

	for _, test := range []struct {
		name     string
		isGroup  bool
		callType string
	}{
		{name: "one to one", isGroup: false, callType: "dm"},
		{name: "group", isGroup: true, callType: "group"},
	} {
		t.Run("private call "+test.name+" uses exact lease and complete participants", func(t *testing.T) {
			require.NoError(t, testhelpers.TruncateAllTables(db))
			senderID := testhelpers.CreateUser(t, db)
			peerID := testhelpers.CreateUser(t, db)
			conversationID := uuid.New()
			callID := uuid.New()
			senderLifecycle := time.Date(2026, 7, 14, 20, 0, 0, 456000, time.UTC)
			createPrivateCallBuilderFixture(
				t, db, conversationID, test.isGroup,
				map[uuid.UUID]time.Time{
					senderID: senderLifecycle,
					peerID:   senderLifecycle.Add(-time.Minute),
				},
			)
			leases := &activityLeaseVerifier{match: true}
			lifecycles := &activityGenerationVerifier{}
			builder := presence.NewActivityBuilder(db, leases, lifecycles)

			built, err := builder.Build(ctx, senderID, presence.Scope{
				Category: presence.CategoryPrivateCall, RoomID: conversationID,
				LifecycleID: callID, EventAt: senderLifecycle.Add(time.Minute),
			})
			require.NoError(t, err)
			require.NotNil(t, built.Input.PrivateCall)
			assert.Equal(t, [][2]uuid.UUID{
				{conversationID, callID},
				{conversationID, callID},
			}, leases.calls)
			require.Len(t, lifecycles.calls, 1)
			assert.ElementsMatch(t, []presence.ActivityGeneration{
				{UserID: senderID, Category: presence.CategoryPrivateCall, SourceToken: callID, SourceVersion: senderLifecycle.UnixMicro()},
				{UserID: peerID, Category: presence.CategoryPrivateCall, SourceToken: callID, SourceVersion: senderLifecycle.Add(-time.Minute).UnixMicro()},
			}, lifecycles.calls[0])
			assert.Equal(t, callID, built.SourceToken)
			assert.Equal(t, senderLifecycle.UnixMicro(), built.SourceVersion)
			assert.Equal(t, senderID, built.Input.SenderID)
			assert.Equal(t, presence.CategoryPrivateCall, built.Input.Category)
			assert.Equal(t, conversationID, built.Input.PrivateCall.Context.ConversationID)
			assert.ElementsMatch(t,
				[]uuid.UUID{senderID, peerID},
				built.Input.PrivateCall.Context.ParticipantIDs,
			)
			assert.Equal(t, test.callType, built.Input.PrivateCall.Payload.CallType)
			assert.Equal(t, 2, built.Input.PrivateCall.Payload.ParticipantCount)
			assert.Nil(t, built.Input.PrivateCall.Payload.StartedAt)
			payload, err := json.Marshal(built.Input.PrivateCall.Payload)
			require.NoError(t, err)
			assert.JSONEq(t,
				`{"call_type":"`+test.callType+`","participant_count":2}`,
				string(payload),
			)
		})
	}

	t.Run("private call rejects stale lease membership and ambiguous active scope", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		senderID := testhelpers.CreateUser(t, db)
		peerID := testhelpers.CreateUser(t, db)
		conversationID := uuid.New()
		callID := uuid.New()
		lifecycleAt := time.Date(2026, 7, 14, 21, 0, 0, 0, time.UTC)
		createPrivateCallBuilderFixture(t, db, conversationID, true, map[uuid.UUID]time.Time{
			senderID: lifecycleAt,
			peerID:   lifecycleAt,
		})
		scope := presence.Scope{
			Category: presence.CategoryPrivateCall, RoomID: conversationID,
			LifecycleID: callID, EventAt: lifecycleAt,
		}

		leases := &activityLeaseVerifier{match: false}
		lifecycles := &activityGenerationVerifier{}
		builder := presence.NewActivityBuilder(db, leases, lifecycles)
		_, err := builder.Build(ctx, senderID, scope)
		assert.ErrorIs(t, err, presence.ErrActivityNotCurrent)
		assert.Equal(t, [][2]uuid.UUID{{conversationID, callID}}, leases.calls)

		leases.match = true
		leases.calls = nil
		_, err = db.Exec(`
			DELETE FROM dm_participants
			WHERE conversation_id = $1 AND user_id = $2
		`, conversationID, peerID)
		require.NoError(t, err)
		_, err = builder.Build(ctx, senderID, scope)
		assert.ErrorIs(t, err, presence.ErrActivityNotCurrent)
		_, err = db.Exec(`
			INSERT INTO dm_participants (conversation_id, user_id)
			VALUES ($1, $2)
		`, conversationID, peerID)
		require.NoError(t, err)

		lifecycles.results = []bool{true, false}
		_, err = builder.Build(ctx, senderID, scope)
		assert.ErrorIs(t, err, presence.ErrActivityNotCurrent)
		lifecycles.results = nil

		secondConversationID := uuid.New()
		createPrivateCallBuilderFixture(t, db, secondConversationID, false, map[uuid.UUID]time.Time{
			senderID: lifecycleAt.Add(time.Second),
		})
		_, err = builder.Build(ctx, senderID, scope)
		assert.ErrorIs(t, err, presence.ErrActivityNotCurrent)

		leases.err = errors.New("forced lease read failure")
		_, err = builder.Build(ctx, senderID, scope)
		assert.ErrorContains(t, err, "verify private call lease")
		assert.ErrorContains(t, err, "forced lease read failure")
	})

	t.Run("private call rejects lease rotation during authoritative read", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		senderID := testhelpers.CreateUser(t, db)
		peerID := testhelpers.CreateUser(t, db)
		conversationID := uuid.New()
		callID := uuid.New()
		lifecycleAt := time.Date(2026, 7, 14, 22, 0, 0, 0, time.UTC)
		createPrivateCallBuilderFixture(t, db, conversationID, false, map[uuid.UUID]time.Time{
			senderID: lifecycleAt,
			peerID:   lifecycleAt,
		})
		leases := &activityLeaseVerifier{matchSequence: []bool{true, false}}

		_, err := presence.NewActivityBuilder(
			db, leases, &activityGenerationVerifier{},
		).Build(ctx, senderID, presence.Scope{
			Category: presence.CategoryPrivateCall, RoomID: conversationID,
			LifecycleID: callID, EventAt: lifecycleAt,
		})

		assert.ErrorIs(t, err, presence.ErrActivityNotCurrent)
		assert.Equal(t, [][2]uuid.UUID{
			{conversationID, callID},
			{conversationID, callID},
		}, leases.calls)
	})
}

func createServerVoiceBuilderFixture(
	t *testing.T,
	db *sql.DB,
	serverName string,
	channelName string,
) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	senderID := testhelpers.CreateUser(t, db)
	return createServerVoiceBuilderFixtureForUser(t, db, senderID, serverName, channelName)
}

func createServerVoiceBuilderFixtureForUser(
	t *testing.T,
	db *sql.DB,
	senderID uuid.UUID,
	serverName string,
	channelName string,
) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	ownerID := testhelpers.CreateUser(t, db)
	serverID := uuid.New()
	channelID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, $2, $3)`,
		serverID, serverName, ownerID,
	)
	require.NoError(t, err)
	testhelpers.AddServerMember(t, db, serverID, senderID)
	_, err = db.Exec(
		`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'voice')`,
		channelID, serverID, channelName,
	)
	require.NoError(t, err)
	return senderID, serverID, channelID
}

func createPrivateCallBuilderFixture(
	t *testing.T,
	db *sql.DB,
	conversationID uuid.UUID,
	isGroup bool,
	participants map[uuid.UUID]time.Time,
) {
	t.Helper()
	var createdBy uuid.UUID
	for participantID := range participants {
		createdBy = participantID
		break
	}
	require.NotEqual(t, uuid.Nil, createdBy)
	_, err := db.Exec(`
		INSERT INTO dm_conversations (id, is_group, created_by)
		VALUES ($1, $2, $3)
	`, conversationID, isGroup, createdBy)
	require.NoError(t, err)
	for participantID, lifecycleAt := range participants {
		_, err = db.Exec(`
			INSERT INTO dm_participants (conversation_id, user_id)
			VALUES ($1, $2)
		`, conversationID, participantID)
		require.NoError(t, err)
		_, err = db.Exec(`
			INSERT INTO dm_voice_participants (
				conversation_id, user_id, joined_at, lifecycle_event_at
			) VALUES ($1, $2, $3, $3)
		`, conversationID, participantID, lifecycleAt)
		require.NoError(t, err)
	}
}
