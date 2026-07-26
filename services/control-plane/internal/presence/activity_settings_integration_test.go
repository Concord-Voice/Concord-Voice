package presence_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type settingsActivityDelivery struct {
	targeted []map[uuid.UUID]bool
	all      int
}

func (*settingsActivityDelivery) DeliverRichPresence(
	context.Context,
	presence.DeliveryPlan,
) error {
	return nil
}

func (d *settingsActivityDelivery) DisconnectRichPresenceClients(
	_ context.Context,
	recipients map[uuid.UUID]bool,
) error {
	copyRecipients := make(map[uuid.UUID]bool, len(recipients))
	for userID, included := range recipients {
		copyRecipients[userID] = included
	}
	d.targeted = append(d.targeted, copyRecipients)
	return nil
}

func (d *settingsActivityDelivery) DisconnectAllRichPresenceClients(context.Context) error {
	d.all++
	return nil
}

type settingsFlipVisibility struct {
	flip  func()
	calls int
}

func (v *settingsFlipVisibility) FilterVisibleUserIDsForChannelFresh(
	_ context.Context,
	_, _ string,
	candidateUserIDs []string,
) ([]string, error) {
	v.calls++
	v.flip()
	return candidateUserIDs, nil
}

func TestActivitySettingsResolver_MismatchedPrivateLeaseFailsConservatively(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(dbCleanup)
	t.Cleanup(redisCleanup)
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	participantID := testhelpers.CreateUser(t, db)
	conversationID := uuid.New()
	callID := uuid.New()
	lifecycleAt := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC)
	createPrivateCallBuilderFixture(t, db, conversationID, false, map[uuid.UUID]time.Time{
		senderID:      lifecycleAt,
		participantID: lifecycleAt,
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
	delivery := &settingsActivityDelivery{}
	service := presence.NewActivityService(
		presencehistory.NewService(nil, presencehistory.DisclosureState{}, false),
		presence.NewActivityBuilder(db, &activityLeaseVerifier{match: false}, store),
		store,
		db,
		activitySnapshotVisibility{allow: true},
		delivery,
		permitAllPresence{},
	)
	before := presence.ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: presence.TierFriends,
		ServerVoiceShowDetails: true, PrivateCallTier: presence.TierOff,
		PrivateCallShowDetails: true,
	}
	after := before
	after.PrivateCallShowDetails = false

	err = service.ApplySettingsSuppressionAlreadyGated(ctx, senderID, before, after)

	require.Error(t, err)
	assert.Equal(t, 1, delivery.all)
	assert.Empty(t, delivery.targeted)
}

func TestActivitySettingsResolver_PrivateOffTargetsOnlyCurrentParticipantsAndSender(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(dbCleanup)
	t.Cleanup(redisCleanup)
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	participantID := testhelpers.CreateUser(t, db)
	unrelatedFriendID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, unrelatedFriendID)
	conversationID := uuid.New()
	callID := uuid.New()
	lifecycleAt := time.Date(2026, 7, 15, 3, 2, 0, 0, time.UTC)
	createPrivateCallBuilderFixture(t, db, conversationID, false, map[uuid.UUID]time.Time{
		senderID:      lifecycleAt,
		participantID: lifecycleAt,
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
	seedActivitySnapshotLifecycle(t, redisClient, participantID, presence.CategoryPrivateCall, state)
	delivery := &settingsActivityDelivery{}
	service := presence.NewActivityService(
		presencehistory.NewService(nil, presencehistory.DisclosureState{}, false),
		presence.NewActivityBuilder(db, &activityLeaseVerifier{match: true}, store),
		store,
		db,
		activitySnapshotVisibility{allow: true},
		delivery,
		permitAllPresence{},
	)
	before := presence.ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: presence.TierOff,
		PrivateCallTier: presence.TierOff, PrivateCallShowDetails: true,
	}
	after := before
	after.PrivateCallShowDetails = false

	err = service.ApplySettingsSuppressionAlreadyGated(ctx, senderID, before, after)

	require.NoError(t, err)
	require.Len(t, delivery.targeted, 1)
	assert.Equal(t, map[uuid.UUID]bool{
		senderID:      true,
		participantID: true,
	}, delivery.targeted[0])
	assert.NotContains(t, delivery.targeted[0], unrelatedFriendID)
	assert.Zero(t, delivery.all)
}

func TestActivitySettingsResolver_LifecycleFlipDuringAuthorizationFailsConservatively(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(dbCleanup)
	t.Cleanup(redisCleanup)
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	_, serverID, channelID := createServerVoiceBuilderFixtureForUser(
		t, db, senderID, "Settings", "Current",
	)
	viewerID := testhelpers.CreateUser(t, db)
	testhelpers.AddServerMember(t, db, serverID, viewerID)
	lifecycleAt := time.Date(2026, 7, 15, 3, 5, 0, 0, time.UTC)
	_, err := db.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, channelID, senderID, lifecycleAt)
	require.NoError(t, err)
	store := presence.NewActivityStore(redisClient)
	state := presence.ActivityState{
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{"server_id":"current"}`), UpdatedAt: lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryServerVoice, state)
	require.NoError(t, err)
	require.True(t, stored)
	seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryServerVoice, state)
	lifecycleKey, err := presence.VoiceLifecycleKey(senderID, presence.CategoryServerVoice)
	require.NoError(t, err)
	visibility := &settingsFlipVisibility{flip: func() {
		require.NoError(t, redisClient.HSet(ctx, lifecycleKey, "active", "0").Err())
	}}
	delivery := &settingsActivityDelivery{}
	service := presence.NewActivityService(
		presencehistory.NewService(nil, presencehistory.DisclosureState{}, false),
		presence.NewActivityBuilder(db, nil, store),
		store,
		db,
		visibility,
		delivery,
		permitAllPresence{},
	)
	before := presence.ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: presence.TierServers,
		ServerVoiceShowDetails: true, PrivateCallTier: presence.TierOff,
	}
	after := before
	after.ServerVoiceShowDetails = false

	err = service.ApplySettingsSuppressionAlreadyGated(ctx, senderID, before, after)

	require.Error(t, err)
	assert.Equal(t, 1, visibility.calls)
	assert.Equal(t, 1, delivery.all)
	assert.Empty(t, delivery.targeted)
}

func TestActivitySettingsResolver_MissingOrStaleEvidenceUsesPriorPolicy(t *testing.T) {
	tests := []struct {
		name           string
		category       presence.Category
		missingState   bool
		authoritative  bool
		wantDisconnect bool
	}{
		{name: "server state missing after prior suppression while active", category: presence.CategoryServerVoice, missingState: true, authoritative: true},
		{name: "server lifecycle expired while active", category: presence.CategoryServerVoice, authoritative: true, wantDisconnect: true},
		{name: "private state missing while active", category: presence.CategoryPrivateCall, missingState: true, authoritative: true, wantDisconnect: true},
		{name: "private lifecycle expired while active", category: presence.CategoryPrivateCall, authoritative: true, wantDisconnect: true},
		{name: "server state missing after prior suppression while inactive", category: presence.CategoryServerVoice, missingState: true},
		{name: "private prior eligible state missing while inactive", category: presence.CategoryPrivateCall, missingState: true, wantDisconnect: true},
		{name: "server lifecycle expired after stored delivery", category: presence.CategoryServerVoice, wantDisconnect: true},
		{name: "private lifecycle expired after stored delivery", category: presence.CategoryPrivateCall, wantDisconnect: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, dbCleanup := testhelpers.SetupTestDB(t)
			redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
			t.Cleanup(dbCleanup)
			t.Cleanup(redisCleanup)
			ctx := context.Background()
			senderID := testhelpers.CreateUser(t, db)
			sourceToken := uuid.New()
			lifecycleAt := time.Date(2026, 7, 15, 3, 10, 0, 0, time.UTC)
			if test.authoritative {
				switch test.category {
				case presence.CategoryServerVoice:
					_, _, channelID := createServerVoiceBuilderFixtureForUser(
						t, db, senderID, "Settings", "Current",
					)
					sourceToken = channelID
					_, err := db.Exec(`
						INSERT INTO voice_participants (
							channel_id, user_id, joined_at, lifecycle_event_at
						) VALUES ($1, $2, $3, $3)
					`, channelID, senderID, lifecycleAt)
					require.NoError(t, err)
				case presence.CategoryPrivateCall:
					conversationID := uuid.New()
					createPrivateCallBuilderFixture(t, db, conversationID, false, map[uuid.UUID]time.Time{
						senderID: lifecycleAt,
					})
				}
			}

			store := presence.NewActivityStore(redisClient)
			if !test.missingState {
				state := presence.ActivityState{
					SourceToken: sourceToken, SourceVersion: lifecycleAt.UnixMicro(),
					Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: lifecycleAt.Unix(),
				}
				stored, err := store.CompareAndSet(ctx, senderID, test.category, state)
				require.NoError(t, err)
				require.True(t, stored)
				// Deliberately omit the lifecycle envelope: the state can outlive it.
			}

			delivery := &settingsActivityDelivery{}
			service := presence.NewActivityService(
				presencehistory.NewService(nil, presencehistory.DisclosureState{}, false),
				presence.NewActivityBuilder(db, &activityLeaseVerifier{match: true}, store),
				store,
				db,
				activitySnapshotVisibility{allow: true},
				delivery,
				permitAllPresence{},
			)
			before := presence.ActivityPolicySettings{
				MasterEnabled: true, ServerVoiceTier: presence.TierOff,
				ServerVoiceShowDetails: true, PrivateCallTier: presence.TierOff,
				PrivateCallShowDetails: true,
			}
			after := before
			if test.category == presence.CategoryServerVoice {
				after.ServerVoiceShowDetails = false
			} else {
				after.PrivateCallShowDetails = false
			}

			err := service.ApplySettingsSuppressionAlreadyGated(ctx, senderID, before, after)

			if test.wantDisconnect {
				require.Error(t, err)
				assert.Equal(t, 1, delivery.all)
			} else {
				require.NoError(t, err)
				assert.Zero(t, delivery.all)
			}
			assert.Empty(t, delivery.targeted)
		})
	}
}
