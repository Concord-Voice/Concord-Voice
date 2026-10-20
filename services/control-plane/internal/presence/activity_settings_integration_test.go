package presence_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type settingsActivityDelivery struct {
	targeted []map[uuid.UUID]bool
	plans    []presence.DeliveryPlan
	all      int
}

func (d *settingsActivityDelivery) DeliverRichPresence(
	_ context.Context,
	plan presence.DeliveryPlan,
) error {
	d.plans = append(d.plans, plan)
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

// Existing delivery fixtures deliberately exercise the fallback without a local
// client snapshot. This fixture exposes the hub's optional snapshot of every
// connected user, including legacy clients holding bootstrap projections.
type connectedSettingsActivityDelivery struct {
	settingsActivityDelivery
	connectedUserIDs []uuid.UUID
}

func (d *connectedSettingsActivityDelivery) RichPresenceClientUserIDs() []uuid.UUID {
	if d.connectedUserIDs == nil {
		return nil
	}
	return append([]uuid.UUID{}, d.connectedUserIDs...)
}

type connectedSettingsVisibility struct {
	deniedID   uuid.UUID
	batchSizes []int
	failOnCall int
	err        error
}

func (v *connectedSettingsVisibility) FilterVisibleUserIDsForChannelFresh(
	_ context.Context,
	_, _ string,
	candidateUserIDs []string,
) ([]string, error) {
	v.batchSizes = append(v.batchSizes, len(candidateUserIDs))
	if len(v.batchSizes) == v.failOnCall {
		return nil, v.err
	}
	visible := make([]string, 0, len(candidateUserIDs))
	for _, candidateID := range candidateUserIDs {
		if candidateID != v.deniedID.String() {
			visible = append(visible, candidateID)
		}
	}
	return visible, nil
}

func createSettingsServerMembers(t *testing.T, db *sql.DB, serverID uuid.UUID, count int) []uuid.UUID {
	t.Helper()
	userIDs := make([]uuid.UUID, count)
	texts := make([]string, count)
	for index := range userIDs {
		userIDs[index] = uuid.New()
		texts[index] = userIDs[index].String()
	}
	if count == 0 {
		return userIDs
	}
	// Audience tests never authenticate these users. Match CreateUser's inert
	// password placeholder, and insert large fixtures in two database round trips.
	_, err := db.Exec(`
		INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		SELECT user_id, user_id::text || '@test.local', 'u_' || user_id::text, 'x', TRUE, TRUE
		FROM unnest($1::uuid[]) AS fixture(user_id)
	`, pq.Array(texts))
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO server_members (server_id, user_id)
		SELECT $1, user_id FROM unnest($2::uuid[]) AS fixture(user_id)
	`, serverID, pq.Array(texts))
	require.NoError(t, err)
	return userIDs
}

func TestActivitySettingsResolver_PrivateCallFiltersConnectedServerPeersBeforeLimit(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(dbCleanup)
	t.Cleanup(redisCleanup)
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	_, serverID, _ := createServerVoiceBuilderFixtureForUser(t, db, senderID, "Private peers", "Voice")
	offlineIDs := createSettingsServerMembers(t, db, serverID, 513)
	viewerID := createSettingsServerMembers(t, db, serverID, 1)[0]
	unrelatedID := testhelpers.CreateUser(t, db)
	participantID := testhelpers.CreateUser(t, db)
	lifecycleAt := time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)
	createPrivateCallBuilderFixture(t, db, uuid.New(), false, map[uuid.UUID]time.Time{
		senderID: lifecycleAt, participantID: lifecycleAt,
	})
	store := presence.NewActivityStore(redisClient)
	state := presence.ActivityState{
		SourceToken: uuid.New(), SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryPrivateCall, state)
	require.NoError(t, err)
	require.True(t, stored)
	seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryPrivateCall, state)
	seedActivitySnapshotLifecycle(t, redisClient, participantID, presence.CategoryPrivateCall, state)
	delivery := &connectedSettingsActivityDelivery{
		connectedUserIDs: []uuid.UUID{senderID, unrelatedID, viewerID},
	}
	service := presence.NewActivityService(
		presencehistory.NewService(nil, presencehistory.DisclosureState{}, false),
		presence.NewActivityBuilder(db, &activityLeaseVerifier{match: true}, store), store, db,
		activitySnapshotVisibility{allow: true}, delivery, permitAllPresence{},
	)
	before := presence.ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: presence.TierOff,
		PrivateCallTier: presence.TierServers, PrivateCallShowDetails: true,
	}
	after := before
	after.PrivateCallShowDetails = false

	err = service.ApplySettingsSuppressionAlreadyGated(ctx, senderID, before, after)

	require.NoError(t, err, "offline server peers must not exhaust Private Call recipients")
	assert.Zero(t, delivery.all)
	require.Len(t, delivery.plans, 1)
	assert.Equal(t, presence.CategoryPrivateCall, delivery.plans[0].Category)
	assert.Equal(t, map[uuid.UUID]bool{viewerID: true}, delivery.plans[0].ClearRecipients)
	assert.Equal(t, []map[uuid.UUID]bool{{viewerID: true}}, delivery.targeted)
	for _, excludedID := range append([]uuid.UUID{senderID, unrelatedID, participantID}, offlineIDs...) {
		assert.NotContains(t, delivery.plans[0].ClearRecipients, excludedID)
	}
	_, found, err := store.Get(ctx, senderID, presence.CategoryPrivateCall)
	require.NoError(t, err)
	assert.True(t, found, "a details change retains the active Private Call generation")
}

func TestActivitySettingsResolver_NoConnectedClientsSuppressesWithoutDelivery(t *testing.T) {
	for _, test := range []struct {
		name      string
		connected []uuid.UUID
	}{
		{name: "nil snapshot"},
		{name: "empty snapshot", connected: []uuid.UUID{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, dbCleanup := testhelpers.SetupTestDB(t)
			redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
			t.Cleanup(dbCleanup)
			t.Cleanup(redisCleanup)
			ctx := context.Background()
			senderID := testhelpers.CreateUser(t, db)
			_, serverID, channelID := createServerVoiceBuilderFixtureForUser(t, db, senderID, "Offline peers", "Voice")
			createSettingsServerMembers(t, db, serverID, 513)
			store := presence.NewActivityStore(redisClient)
			lifecycleAt := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			_, err := db.Exec(`
				INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
				VALUES ($1, $2, $3, $3)
			`, channelID, senderID, lifecycleAt)
			require.NoError(t, err)
			state := presence.ActivityState{
				SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
				Payload: json.RawMessage(`{}`), UpdatedAt: lifecycleAt.Unix(),
			}
			stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryServerVoice, state)
			require.NoError(t, err)
			require.True(t, stored)
			seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryServerVoice, state)
			delivery := &connectedSettingsActivityDelivery{connectedUserIDs: test.connected}
			service := presence.NewActivityService(
				presencehistory.NewService(nil, presencehistory.DisclosureState{}, false),
				presence.NewActivityBuilder(db, nil, store), store, db,
				activitySnapshotVisibility{allow: true}, delivery, permitAllPresence{},
			)
			before := presence.ActivityPolicySettings{
				MasterEnabled: true, ServerVoiceTier: presence.TierServers, PrivateCallTier: presence.TierServers,
			}
			after := before
			after.MasterEnabled = false

			err = service.ApplySettingsSuppressionAlreadyGated(ctx, senderID, before, after)

			require.NoError(t, err)
			assert.Zero(t, delivery.all)
			assert.Empty(t, delivery.plans)
			assert.Empty(t, delivery.targeted)
			for _, category := range []presence.Category{presence.CategoryServerVoice, presence.CategoryPrivateCall} {
				_, found, err := store.Get(ctx, senderID, category)
				require.NoError(t, err)
				assert.False(t, found, "master suppression removes stored %s even without connected viewers", category)
			}
		})
	}
}

func TestHiddenSenderSuppression_NoConnectedClientsStillValidatesStoredGeneration(t *testing.T) {
	for _, test := range []struct {
		name      string
		connected []uuid.UUID
		stale     bool
	}{
		{name: "nil clients with valid generation"},
		{name: "empty clients with valid generation", connected: []uuid.UUID{}},
		{name: "nil clients with expired lifecycle", stale: true},
		{name: "empty clients with expired lifecycle", connected: []uuid.UUID{}, stale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, dbCleanup := testhelpers.SetupTestDB(t)
			redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
			t.Cleanup(dbCleanup)
			t.Cleanup(redisCleanup)
			ctx := context.Background()
			senderID := testhelpers.CreateUser(t, db)
			_, _, channelID := createServerVoiceBuilderFixtureForUser(t, db, senderID, "No clients", "Hidden")
			lifecycleAt := time.Date(2026, 10, 20, 13, 0, 0, 0, time.UTC)
			_, err := db.Exec(`
				INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
				VALUES ($1, $2, $3, $3)
			`, channelID, senderID, lifecycleAt)
			require.NoError(t, err)
			store := presence.NewActivityStore(redisClient)
			state := presence.ActivityState{
				SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
				Payload: json.RawMessage(`{"server_id":"hidden-evidence"}`), UpdatedAt: lifecycleAt.Unix(),
			}
			stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryServerVoice, state)
			require.NoError(t, err)
			require.True(t, stored)
			if !test.stale {
				seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryServerVoice, state)
			}
			delivery := &connectedSettingsActivityDelivery{connectedUserIDs: test.connected}
			service := presence.NewActivityService(
				presencehistory.NewService(nil, presencehistory.DisclosureState{}, false),
				presence.NewActivityBuilder(db, nil, store), store, db,
				activitySnapshotVisibility{allow: true}, delivery, permitAllPresence{},
			)

			err = service.ForceSuppressHiddenSenderActivityAlreadyGated(ctx, senderID)

			if test.stale {
				assert.Error(t, err, "an empty client snapshot cannot validate stale published evidence")
				assert.Equal(t, 1, delivery.all, "unknown published evidence keeps the existing fail-closed fallback")
			} else {
				assert.NoError(t, err)
				assert.Zero(t, delivery.all)
			}
			assert.Empty(t, delivery.plans)
			assert.Empty(t, delivery.targeted)
			retained, found, getErr := store.Get(ctx, senderID, presence.CategoryServerVoice)
			require.NoError(t, getErr)
			assert.Equal(t, test.stale, found, "only validated hidden generations may be deleted")
			if test.stale {
				assert.Equal(t, state, retained, "failed resolution must preserve exact stored evidence")
			}
		})
	}
}

func TestActivitySettingsResolver_SecondConnectedBatchFailureDeliversNothing(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(dbCleanup)
	t.Cleanup(redisCleanup)
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	_, serverID, channelID := createServerVoiceBuilderFixtureForUser(t, db, senderID, "Batch failure", "Voice")
	viewers := createSettingsServerMembers(t, db, serverID, 513)
	lifecycleAt := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	_, err := db.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, channelID, senderID, lifecycleAt)
	require.NoError(t, err)
	store := presence.NewActivityStore(redisClient)
	state := presence.ActivityState{
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{}`), UpdatedAt: lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryServerVoice, state)
	require.NoError(t, err)
	require.True(t, stored)
	seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryServerVoice, state)
	visibilityErr := errors.New("second connected audience authorization failed")
	visibility := &connectedSettingsVisibility{failOnCall: 2, err: visibilityErr}
	delivery := &connectedSettingsActivityDelivery{connectedUserIDs: viewers}
	service := presence.NewActivityService(
		presencehistory.NewService(nil, presencehistory.DisclosureState{}, false),
		presence.NewActivityBuilder(db, nil, store), store, db,
		visibility, delivery, permitAllPresence{},
	)
	before := presence.ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: presence.TierServers, ServerVoiceShowDetails: true,
	}
	after := before
	after.ServerVoiceShowDetails = false

	err = service.ApplySettingsSuppressionAlreadyGated(ctx, senderID, before, after)

	assert.ErrorIs(t, err, visibilityErr)
	assert.Equal(t, []int{512, 1}, visibility.batchSizes, "the failure must occur after a successful first batch")
	assert.Equal(t, 1, delivery.all)
	assert.Empty(t, delivery.plans, "no partial clear may escape before all authorization batches succeed")
	assert.Empty(t, delivery.targeted)
}

func TestActivitySettingsResolver_ConnectedAudienceIsIndependentOfServerSize(t *testing.T) {
	tests := []struct {
		name             string
		offlineMembers   int
		connectedViewers int
		hidden           bool
	}{
		{name: "below limit control", connectedViewers: 1},
		{name: "offline server members do not exhaust the recipient limit", offlineMembers: 513, connectedViewers: 1},
		{name: "every connected viewer across multiple batches is refreshed", offlineMembers: 1, connectedViewers: 513},
		{name: "hidden sender uses the same bounded connected audience", offlineMembers: 513, connectedViewers: 1, hidden: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, dbCleanup := testhelpers.SetupTestDB(t)
			redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
			t.Cleanup(dbCleanup)
			t.Cleanup(redisCleanup)
			ctx := context.Background()
			senderID := testhelpers.CreateUser(t, db)
			_, serverID, channelID := createServerVoiceBuilderFixtureForUser(
				t, db, senderID, "Connected audience", "Settings",
			)
			viewers := createSettingsServerMembers(t, db, serverID, test.connectedViewers)
			offlineIDs := createSettingsServerMembers(t, db, serverID, test.offlineMembers)
			deniedID := createSettingsServerMembers(t, db, serverID, 1)[0]
			unrelatedID := testhelpers.CreateUser(t, db)
			lifecycleAt := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
			_, err := db.Exec(`
				INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
				VALUES ($1, $2, $3, $3)
			`, channelID, senderID, lifecycleAt)
			require.NoError(t, err)
			_, err = db.Exec(`
				INSERT INTO user_presence_settings (user_id, server_voice_tier, server_voice_show_details)
				VALUES ($1, 2, TRUE)
				ON CONFLICT (user_id) DO UPDATE
				SET server_voice_tier = EXCLUDED.server_voice_tier,
				    server_voice_show_details = EXCLUDED.server_voice_show_details
			`, senderID)
			require.NoError(t, err)
			store := presence.NewActivityStore(redisClient)
			state := presence.ActivityState{
				SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
				Payload: json.RawMessage(`{"server_id":"connected-audience"}`), UpdatedAt: lifecycleAt.Unix(),
			}
			stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryServerVoice, state)
			require.NoError(t, err)
			require.True(t, stored)
			seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryServerVoice, state)
			delivery := &connectedSettingsActivityDelivery{
				connectedUserIDs: append([]uuid.UUID{senderID, unrelatedID, deniedID}, viewers...),
			}
			visibility := &connectedSettingsVisibility{deniedID: deniedID}
			service := presence.NewActivityService(
				presencehistory.NewService(nil, presencehistory.DisclosureState{}, false),
				presence.NewActivityBuilder(db, nil, store), store, db,
				visibility, delivery, permitAllPresence{},
			)

			if test.hidden {
				err = service.ForceSuppressHiddenSenderActivityAlreadyGated(ctx, senderID)
			} else {
				before := presence.ActivityPolicySettings{
					MasterEnabled: true, ServerVoiceTier: presence.TierServers,
					ServerVoiceShowDetails: true, PrivateCallTier: presence.TierOff,
				}
				after := before
				after.ServerVoiceShowDetails = false
				err = service.ApplySettingsSuppressionAlreadyGated(ctx, senderID, before, after)
			}

			assert.NoError(t, err, "server population must not trigger a recipient-limit error")
			assert.Zero(t, delivery.all, "a known connected audience must not disconnect the whole replica")
			want := make(map[uuid.UUID]int, len(viewers))
			for _, viewerID := range viewers {
				want[viewerID] = 1
			}
			cleared := make(map[uuid.UUID]int)
			for _, plan := range delivery.plans {
				assert.Equal(t, senderID, plan.SenderID)
				assert.Equal(t, presence.CategoryServerVoice, plan.Category)
				for recipientID, included := range plan.ClearRecipients {
					if included {
						cleared[recipientID]++
					}
				}
			}
			assert.Equal(t, want, cleared, "every authorized connected viewer receives exactly one clear")
			reconnected := make(map[uuid.UUID]int)
			for _, recipients := range delivery.targeted {
				for recipientID, included := range recipients {
					if included {
						reconnected[recipientID]++
					}
				}
			}
			if test.hidden {
				assert.Empty(t, reconnected, "a successful terminal clear needs no reconnect")
			} else {
				assert.Equal(t, want, reconnected, "every authorized connected viewer refreshes its projection")
			}
			for _, excludedID := range append([]uuid.UUID{senderID, unrelatedID, deniedID}, offlineIDs...) {
				assert.NotContains(t, cleared, excludedID)
				assert.NotContains(t, reconnected, excludedID)
			}
			require.NotEmpty(t, visibility.batchSizes, "the server audience must pass fresh channel authorization")
			for _, size := range visibility.batchSizes {
				assert.LessOrEqual(t, size, 512, "fresh authorization must retain its bounded input")
			}
			if test.connectedViewers > 512 {
				assert.GreaterOrEqual(t, len(visibility.batchSizes), 2, "authorization must cover later batches")
			}
			_, found, getErr := store.Get(ctx, senderID, presence.CategoryServerVoice)
			require.NoError(t, getErr)
			assert.Equal(t, !test.hidden, found, "detail changes preserve the active generation; hidden suppression removes it")
		})
	}
}

// A sender can be present in Server Voice with no Private Call activity when it
// goes offline. Hidden-sender cleanup evaluates both widest-prior categories;
// the absent sibling must not turn the valid Server Voice audience into a
// replica-wide disconnect before its terminal clear is delivered.
func TestHiddenSenderSuppression_ServerVoiceWithAbsentPrivateCallTargetsServerAudience(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(dbCleanup)
	t.Cleanup(redisCleanup)
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	_, serverID, channelID := createServerVoiceBuilderFixtureForUser(
		t, db, senderID, "Presence", "Terminal",
	)
	viewerID := testhelpers.CreateUser(t, db)
	testhelpers.AddServerMember(t, db, serverID, viewerID)
	testhelpers.AddFriendship(t, db, senderID, viewerID)
	lifecycleAt := time.Date(2026, 9, 23, 13, 0, 0, 0, time.UTC)
	_, err := db.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, channelID, senderID, lifecycleAt)
	require.NoError(t, err)
	// The viewer is a real prior recipient: Server Voice was published at the
	// Servers tier before the sender became hidden.
	_, err = db.Exec(`
		INSERT INTO user_presence_settings (user_id, server_voice_tier)
		VALUES ($1, 2)
		ON CONFLICT (user_id) DO UPDATE
		SET server_voice_tier = EXCLUDED.server_voice_tier
	`, senderID)
	require.NoError(t, err)

	store := presence.NewActivityStore(redisClient)
	state := presence.ActivityState{
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{"server_id":"presence"}`), UpdatedAt: lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryServerVoice, state)
	require.NoError(t, err)
	require.True(t, stored)
	seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryServerVoice, state)

	delivery := &settingsActivityDelivery{}
	service := presence.NewActivityService(
		presencehistory.NewService(nil, presencehistory.DisclosureState{}, false),
		presence.NewActivityBuilder(db, nil, store), store, db,
		activitySnapshotVisibility{allow: true}, delivery, permitAllPresence{},
	)

	err = service.ForceSuppressHiddenSenderActivityAlreadyGated(ctx, senderID)

	require.NoError(t, err)
	assert.Zero(t, delivery.all, "a missing inactive sibling category is not unknown audience evidence")
	require.Len(t, delivery.plans, 1, "only the stored Server Voice category is cleared")
	for _, plan := range delivery.plans {
		assert.Equal(t, senderID, plan.SenderID)
		assert.Equal(t, presence.CategoryServerVoice, plan.Category)
		assert.NotContains(t, plan.ClearRecipients, senderID)
		assert.Contains(t, plan.ClearRecipients, viewerID)
	}
}

// A settings tier-down refreshes prior recipients so revoked viewers lose the
// badge and retained viewers fetch the reduced projection. The publisher is
// not an audience: closing its last application socket makes it appear offline
// and can erase the still-active generation before retained viewers snapshot.
func TestSettingsTierDown_ExcludesPublisherAndRetainsGenerationForEligibleViewer(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(dbCleanup)
	t.Cleanup(redisCleanup)
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	_, serverID, channelID := createServerVoiceBuilderFixtureForUser(
		t, db, senderID, "Presence", "Tier Down",
	)
	viewerID := testhelpers.CreateUser(t, db)
	testhelpers.AddServerMember(t, db, serverID, viewerID)
	testhelpers.AddFriendship(t, db, senderID, viewerID)
	lifecycleAt := time.Date(2026, 9, 23, 13, 5, 0, 0, time.UTC)
	_, err := db.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, channelID, senderID, lifecycleAt)
	require.NoError(t, err)

	store := presence.NewActivityStore(redisClient)
	state := presence.ActivityState{
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{"server_id":"tier-down"}`), UpdatedAt: lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, presence.CategoryServerVoice, state)
	require.NoError(t, err)
	require.True(t, stored)
	seedActivitySnapshotLifecycle(t, redisClient, senderID, presence.CategoryServerVoice, state)

	delivery := &settingsActivityDelivery{}
	service := presence.NewActivityService(
		presencehistory.NewService(nil, presencehistory.DisclosureState{}, false),
		presence.NewActivityBuilder(db, nil, store), store, db,
		activitySnapshotVisibility{allow: true}, delivery, permitAllPresence{},
	)
	before := presence.ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: presence.TierServers,
		ServerVoiceShowDetails: true, PrivateCallTier: presence.TierOff,
	}
	after := before
	after.ServerVoiceTier = presence.TierFriends

	err = service.ApplySettingsSuppressionAlreadyGated(ctx, senderID, before, after)

	require.NoError(t, err)
	require.Len(t, delivery.plans, 1)
	assert.Equal(t, map[uuid.UUID]bool{viewerID: true}, delivery.plans[0].ClearRecipients)
	require.Len(t, delivery.targeted, 1)
	assert.Equal(t, map[uuid.UUID]bool{viewerID: true}, delivery.targeted[0])
	_, found, err := store.Get(ctx, senderID, presence.CategoryServerVoice)
	require.NoError(t, err)
	assert.True(t, found, "the retained viewer needs the active generation on its reconnect snapshot")
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

func TestActivitySettingsResolver_PrivateOffTargetsOnlyCurrentParticipants(t *testing.T) {
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
		participantID: true,
	}, delivery.targeted[0])
	assert.NotContains(t, delivery.targeted[0], senderID)
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
		// priorServerTier is the Server Voice tier in effect before the change.
		priorServerTier presence.Tier
		// Evidence that a badge may still be held although nothing is stored.
		pendingPlan, terminalOutbox bool
		// otherUserActive puts a different user in voice and in a call.
		otherUserActive bool
		// audience gives an authoritative sender someone the prior policy
		// reaches: a fellow server member, or a second call participant.
		audience bool
		// secondScope puts an authoritative sender in a second room as well.
		secondScope bool
		// wantTargeted expects the audience member to be cleared and
		// reconnected, as for a stored badge, instead of a replica-wide disconnect.
		wantTargeted bool
	}{
		{name: "server prior eligible missing with pending clear plan", category: presence.CategoryServerVoice, missingState: true, pendingPlan: true, priorServerTier: presence.TierFriends, wantDisconnect: true},
		{name: "private prior eligible missing with pending clear plan", category: presence.CategoryPrivateCall, missingState: true, pendingPlan: true, wantDisconnect: true},
		{name: "server prior eligible missing with terminal outbox", category: presence.CategoryServerVoice, missingState: true, terminalOutbox: true, priorServerTier: presence.TierFriends, wantDisconnect: true},
		{name: "server prior eligible missing while another user is in voice", category: presence.CategoryServerVoice, missingState: true, otherUserActive: true, priorServerTier: presence.TierFriends},
		{name: "private prior eligible missing while another user is in a call", category: presence.CategoryPrivateCall, missingState: true, otherUserActive: true},
		// In voice with nothing stored is treated like a stored badge: the prior
		// audience is cleared and reconnected, never the whole replica.
		{name: "server prior eligible state missing while active", category: presence.CategoryServerVoice, missingState: true, authoritative: true, audience: true, priorServerTier: presence.TierServers, wantTargeted: true},
		{name: "server state missing while active with an empty prior audience", category: presence.CategoryServerVoice, missingState: true, authoritative: true, priorServerTier: presence.TierServers},
		{name: "server state missing while active in two channels", category: presence.CategoryServerVoice, missingState: true, authoritative: true, secondScope: true, priorServerTier: presence.TierServers, wantDisconnect: true},
		{name: "server prior eligible state missing while inactive", category: presence.CategoryServerVoice, missingState: true, priorServerTier: presence.TierFriends},
		{name: "server state missing after prior suppression while active", category: presence.CategoryServerVoice, missingState: true, authoritative: true},
		{name: "server lifecycle expired while active", category: presence.CategoryServerVoice, authoritative: true, wantDisconnect: true},
		{name: "private state missing while active", category: presence.CategoryPrivateCall, missingState: true, authoritative: true, audience: true, wantTargeted: true},
		{name: "private state missing while alone in the call", category: presence.CategoryPrivateCall, missingState: true, authoritative: true},
		{name: "private state missing while in two calls", category: presence.CategoryPrivateCall, missingState: true, authoritative: true, secondScope: true, wantDisconnect: true},
		{name: "private lifecycle expired while active", category: presence.CategoryPrivateCall, authoritative: true, wantDisconnect: true},
		{name: "server state missing after prior suppression while inactive", category: presence.CategoryServerVoice, missingState: true},
		// Overrides #2408: nothing stored and not in a call means nothing was
		// published, so there is no audience to clear and nothing to disconnect.
		{name: "private prior eligible state missing while inactive", category: presence.CategoryPrivateCall, missingState: true},
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
			audienceID := testhelpers.CreateUser(t, db)
			if test.authoritative {
				switch test.category {
				case presence.CategoryServerVoice:
					_, serverID, channelID := createServerVoiceBuilderFixtureForUser(
						t, db, senderID, "Settings", "Current",
					)
					if test.audience {
						testhelpers.AddServerMember(t, db, serverID, audienceID)
					}
					sourceToken = channelID
					channels := []uuid.UUID{channelID}
					if test.secondScope {
						_, _, secondChannelID := createServerVoiceBuilderFixtureForUser(
							t, db, senderID, "Second", "Second",
						)
						channels = append(channels, secondChannelID)
					}
					for _, voiceChannelID := range channels {
						_, err := db.Exec(`
							INSERT INTO voice_participants (
								channel_id, user_id, joined_at, lifecycle_event_at
							) VALUES ($1, $2, $3, $3)
						`, voiceChannelID, senderID, lifecycleAt)
						require.NoError(t, err)
					}
				case presence.CategoryPrivateCall:
					participants := map[uuid.UUID]time.Time{senderID: lifecycleAt}
					if test.audience {
						participants[audienceID] = lifecycleAt
					}
					createPrivateCallBuilderFixture(t, db, uuid.New(), false, participants)
					if test.secondScope {
						createPrivateCallBuilderFixture(t, db, uuid.New(), false, map[uuid.UUID]time.Time{
							senderID: lifecycleAt,
						})
					}
				}
			}

			if test.pendingPlan {
				_, err := db.Exec(`
					INSERT INTO presence_active_pending_plans (
						user_id, category, operation_id, resolution, scope_event_at
					) VALUES ($1, $2, $3, 'conservative', $4)
				`, senderID, string(test.category), uuid.New(), lifecycleAt)
				require.NoError(t, err)
			}
			if test.terminalOutbox {
				_, serverID, channelID := createServerVoiceBuilderFixtureForUser(t, db, senderID, "Outbox", "Outbox")
				_, err := db.Exec(`
					INSERT INTO server_voice_terminal_outbox (channel_id, user_id, server_id, operation_id)
					VALUES ($1, $2, $3, $4)
				`, channelID, senderID, serverID, uuid.New())
				require.NoError(t, err)
			}
			if test.otherUserActive {
				otherID := testhelpers.CreateUser(t, db)
				_, _, channelID := createServerVoiceBuilderFixtureForUser(t, db, otherID, "Other", "Other")
				_, err := db.Exec(`
					INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
					VALUES ($1, $2, $3, $3)
				`, channelID, otherID, lifecycleAt)
				require.NoError(t, err)
				createPrivateCallBuilderFixture(t, db, uuid.New(), false, map[uuid.UUID]time.Time{otherID: lifecycleAt})
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
				MasterEnabled: true, ServerVoiceTier: test.priorServerTier,
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
			if test.wantTargeted {
				require.Len(t, delivery.targeted, 1)
				assert.True(t, delivery.targeted[0][audienceID], "the prior audience is reconnected")
				assert.NotContains(t, delivery.targeted[0], senderID)
			} else {
				assert.Empty(t, delivery.targeted)
			}
		})
	}
}
