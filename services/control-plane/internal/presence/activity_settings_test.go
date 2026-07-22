package presence

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivityService_ApplySettingsSuppressionAlreadyGated(t *testing.T) {
	tests := []struct {
		name          string
		masterEnabled bool
		serverTier    Tier
		privateTier   Tier
		wantDeletes   []Category
	}{
		{
			name:          "master off deletes both categories",
			masterEnabled: false,
			serverTier:    TierServers,
			privateTier:   TierFriends,
			wantDeletes:   []Category{CategoryServerVoice, CategoryPrivateCall},
		},
		{
			name:          "server voice off deletes only server voice",
			masterEnabled: true,
			serverTier:    TierOff,
			privateTier:   TierFriends,
			wantDeletes:   []Category{CategoryServerVoice},
		},
		{
			name:          "private call off retains participant-only current state",
			masterEnabled: true,
			serverTier:    TierFriends,
			privateTier:   TierOff,
			wantDeletes:   []Category{},
		},
		{
			name:          "friends tier retains current state",
			masterEnabled: true,
			serverTier:    TierFriends,
			privateTier:   TierFriends,
			wantDeletes:   []Category{},
		},
		{
			name:          "servers tier retains current state",
			masterEnabled: true,
			serverTier:    TierServers,
			privateTier:   TierServers,
			wantDeletes:   []Category{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, _, store, delivery, coordinator := newActivityServiceFixture(CategoryServerVoice)
			before := testActivityPolicySettings(true, TierFriends, TierFriends)
			after := testActivityPolicySettings(
				test.masterEnabled, test.serverTier, test.privateTier,
			)

			err := service.ApplySettingsSuppressionAlreadyGated(
				context.Background(), activityServiceSender, before, after,
			)
			require.NoError(t, err)

			gotDeletes := make([]Category, 0, len(store.exactDeletes))
			for _, call := range store.exactDeletes {
				assert.Equal(t, activityServiceSender, call.userID)
				gotDeletes = append(gotDeletes, call.category)
			}
			assert.Equal(t, test.wantDeletes, gotDeletes)
			if before == after {
				assert.Empty(t, delivery.disconnects)
			} else {
				assert.Len(t, delivery.disconnects, 1)
			}
			assert.Zero(t, delivery.disconnectAllCalls)
			assert.Zero(t, coordinator.calls, "caller already owns the sender gate")
		})
	}
}

func TestActivitySettingsQueriesStayCandidateCorrelated(t *testing.T) {
	source, err := os.ReadFile("activity_settings.go")
	require.NoError(t, err)
	assert.NotContains(t, string(source), "WITH accepted_edges AS",
		"bounded settings cleanup must not materialize the global friendship graph")
}

func TestQueryBoundedSettingsRecipients_AllowsExactLimitAndRejectsOverflow(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	query := `
		SELECT ('00000000-0000-0000-0000-' || lpad(value::text, 12, '0'))::uuid
		FROM generate_series(1, $1) AS value
	`

	exact, err := queryBoundedSettingsRecipients(
		ctx, db, query, activitySettingsRecipientLimit,
	)
	require.NoError(t, err)
	assert.Len(t, exact, activitySettingsRecipientLimit)

	overflow, err := queryBoundedSettingsRecipients(
		ctx, db, query, activitySettingsRecipientLimit+1,
	)
	assert.Error(t, err)
	assert.Nil(t, overflow)
}

func TestActivityService_SettingsChangeTargetsOnlyAffectedCurrentRecipients(t *testing.T) {
	service, _, _, delivery, coordinator := newActivityServiceFixture(CategoryServerVoice)
	before := ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: TierFriends,
		ServerVoiceShowDetails: true, PrivateCallTier: TierFriends,
	}
	after := before
	after.ServerVoiceShowDetails = false
	resolverCalls := 0
	service.settingsRecipients = func(
		_ context.Context,
		userID uuid.UUID,
		gotBefore, gotAfter ActivityPolicySettings,
	) (map[uuid.UUID]bool, error) {
		resolverCalls++
		assert.Equal(t, activityServiceSender, userID)
		assert.Equal(t, before, gotBefore)
		assert.Equal(t, after, gotAfter)
		return map[uuid.UUID]bool{
			activityServiceSender: true,
			activityServiceViewer: true,
		}, nil
	}

	err := service.ApplySettingsSuppressionAlreadyGated(
		context.Background(), activityServiceSender, before, after,
	)

	require.NoError(t, err)
	assert.Equal(t, 1, resolverCalls)
	assert.Equal(t, []map[uuid.UUID]bool{{
		activityServiceSender: true,
		activityServiceViewer: true,
	}}, delivery.disconnects)
	assert.Zero(t, delivery.disconnectAllCalls)
	assert.Zero(t, coordinator.calls, "caller already owns the sender gate")
}

func TestActivityService_SettingsChangeSameValueOrInactiveDisconnectsNobody(t *testing.T) {
	settings := ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: TierFriends,
		ServerVoiceShowDetails: true, PrivateCallTier: TierFriends,
	}

	t.Run("same value", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		resolverCalls := 0
		service.settingsRecipients = func(
			context.Context, uuid.UUID, ActivityPolicySettings, ActivityPolicySettings,
		) (map[uuid.UUID]bool, error) {
			resolverCalls++
			return map[uuid.UUID]bool{activityServiceViewer: true}, nil
		}

		err := service.ApplySettingsSuppressionAlreadyGated(
			context.Background(), activityServiceSender, settings, settings,
		)

		require.NoError(t, err)
		assert.Zero(t, resolverCalls)
		assert.Empty(t, delivery.disconnects)
		assert.Zero(t, delivery.disconnectAllCalls)
		assert.Empty(t, store.exactDeletes)
	})

	t.Run("inactive sender", func(t *testing.T) {
		service, _, _, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		service.settingsRecipients = func(
			context.Context, uuid.UUID, ActivityPolicySettings, ActivityPolicySettings,
		) (map[uuid.UUID]bool, error) {
			return map[uuid.UUID]bool{}, nil
		}
		after := settings
		after.ServerVoiceShowDetails = false

		err := service.ApplySettingsSuppressionAlreadyGated(
			context.Background(), activityServiceSender, settings, after,
		)

		require.NoError(t, err)
		assert.Empty(t, delivery.disconnects)
		assert.Zero(t, delivery.disconnectAllCalls)
	})
}

func TestActivityService_ApplySettingsSuppressionAlreadyGatedAttemptsAllCleanup(t *testing.T) {
	deleteErr := errors.New("delete unavailable")
	disconnectErr := errors.New("disconnect unavailable")
	service, _, store, delivery, coordinator := newActivityServiceFixture(CategoryServerVoice)
	store.delete = func(_ context.Context, _ uuid.UUID, category Category) error {
		if category == CategoryServerVoice {
			return deleteErr
		}
		return nil
	}
	delivery.disconnectErr = disconnectErr

	err := service.ApplySettingsSuppressionAlreadyGated(
		context.Background(), activityServiceSender,
		testActivityPolicySettings(true, TierFriends, TierFriends),
		testActivityPolicySettings(false, TierOff, TierOff),
	)

	assert.ErrorIs(t, err, deleteErr)
	assert.ErrorIs(t, err, disconnectErr)
	assert.Equal(t, []Category{CategoryServerVoice, CategoryPrivateCall}, []Category{
		store.exactDeletes[0].category,
		store.exactDeletes[1].category,
	})
	assert.Len(t, delivery.disconnects, 1)
	assert.Zero(t, delivery.disconnectAllCalls)
	assert.Zero(t, coordinator.calls)
}

func TestActivityService_ApplySettingsSuppressionAlreadyGatedSharesOneDeadlineBudget(t *testing.T) {
	service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)

	err := service.ApplySettingsSuppressionAlreadyGated(
		context.Background(), activityServiceSender,
		testActivityPolicySettings(true, TierFriends, TierFriends),
		testActivityPolicySettings(false, TierServers, TierServers),
	)
	require.NoError(t, err)
	require.Len(t, store.exactDeleteContexts, 2)
	require.Len(t, delivery.disconnectContexts, 1)
	budgetCtx := store.exactDeleteContexts[0]
	assert.True(t, budgetCtx == store.exactDeleteContexts[1], "both exact deletes must share one cleanup context")
	assert.True(t, budgetCtx == delivery.disconnectContexts[0], "disconnect must consume the same bounded budget")
	deleteDeadline, hasDeleteDeadline := budgetCtx.Deadline()
	disconnectDeadline, hasDisconnectDeadline := delivery.disconnectContexts[0].Deadline()
	require.True(t, hasDeleteDeadline)
	require.True(t, hasDisconnectDeadline)
	assert.Equal(t, deleteDeadline, disconnectDeadline)
}

func TestActivityService_SettingsRecipientTimeoutUsesFreshFailClosedDisconnectBudget(t *testing.T) {
	service, _, _, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	before := testActivityPolicySettings(true, TierFriends, TierFriends)
	after := before
	after.ServerVoiceShowDetails = false
	service.settingsRecipients = func(
		ctx context.Context,
		_ uuid.UUID,
		_ ActivityPolicySettings,
		_ ActivityPolicySettings,
	) (map[uuid.UUID]bool, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	err := service.ApplySettingsSuppressionAlreadyGated(
		context.Background(), activityServiceSender, before, after,
	)

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, delivery.disconnectAllContextErrors, 1)
	assert.NoError(t, delivery.disconnectAllContextErrors[0],
		"the fail-closed disconnect needs a fresh emergency budget")
	require.Len(t, delivery.disconnectAllContexts, 1)
	deadline, bounded := delivery.disconnectAllContexts[0].Deadline()
	require.True(t, bounded)
	assert.Greater(t, time.Until(deadline), time.Duration(0))
}

func TestActivityService_ApplySettingsSuppressionAlreadyGatedDisconnectsBeforeBlockingDeletes(t *testing.T) {
	service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	deleteEntered := make(chan struct{})
	releaseDelete := make(chan struct{})
	store.delete = func(ctx context.Context, _ uuid.UUID, _ Category) error {
		select {
		case <-deleteEntered:
		default:
			close(deleteEntered)
		}
		select {
		case <-releaseDelete:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	disconnectCalled := make(chan error, 1)
	disconnectObserved := make(chan struct{})
	delivery.onDisconnect = func(ctx context.Context) {
		disconnectCalled <- ctx.Err()
		<-disconnectObserved
	}

	done := make(chan error, 1)
	go func() {
		done <- service.ApplySettingsSuppressionAlreadyGated(
			context.Background(), activityServiceSender,
			testActivityPolicySettings(true, TierFriends, TierFriends),
			testActivityPolicySettings(false, TierServers, TierServers),
		)
	}()

	select {
	case ctxErr := <-disconnectCalled:
		assert.NoError(t, ctxErr, "conservative disconnect must receive the live cleanup budget")
	case <-deleteEntered:
		t.Error("a blocking exact delete started before the conservative disconnect")
	case <-time.After(time.Second):
		t.Error("settings cleanup did not start")
	}
	close(disconnectObserved)
	close(releaseDelete)
	require.NoError(t, <-done)
	assert.Equal(t, []Category{CategoryServerVoice, CategoryPrivateCall}, []Category{
		store.exactDeletes[0].category,
		store.exactDeletes[1].category,
	})
}

func TestActivityService_ApplySettingsSuppressionAlreadyGatedRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name        string
		service     *ActivityService
		userID      uuid.UUID
		serverTier  Tier
		privateTier Tier
	}{
		{name: "nil service", service: nil, userID: activityServiceSender, serverTier: TierFriends, privateTier: TierFriends},
		{name: "zero user", service: settingsSuppressionService(), userID: uuid.Nil, serverTier: TierFriends, privateTier: TierFriends},
		{name: "invalid server tier", service: settingsSuppressionService(), userID: activityServiceSender, serverTier: TierServers + 1, privateTier: TierFriends},
		{name: "invalid private tier", service: settingsSuppressionService(), userID: activityServiceSender, serverTier: TierFriends, privateTier: TierServers + 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.service.ApplySettingsSuppressionAlreadyGated(
				context.Background(), test.userID,
				testActivityPolicySettings(true, TierFriends, TierFriends),
				testActivityPolicySettings(true, test.serverTier, test.privateTier),
			)
			assert.Error(t, err)
		})
	}
}

func TestActivityService_SuppressAllActivityAlreadyGated(t *testing.T) {
	t.Run("deletes both exact categories and disconnects all clients", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)

		require.NoError(t, service.SuppressAllActivityAlreadyGated(
			context.Background(), activityServiceSender,
		))

		require.Len(t, store.exactDeletes, 2)
		assert.Equal(t, CategoryServerVoice, store.exactDeletes[0].category)
		assert.Equal(t, CategoryPrivateCall, store.exactDeletes[1].category)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("attempts every cleanup and joins failures", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		deleteErr := errors.New("forced account activity delete failure")
		disconnectErr := errors.New("forced account activity disconnect failure")
		store.deleteErr = deleteErr
		delivery.disconnectAllErr = disconnectErr

		err := service.SuppressAllActivityAlreadyGated(
			context.Background(), activityServiceSender,
		)

		require.ErrorIs(t, err, deleteErr)
		require.ErrorIs(t, err, disconnectErr)
		assert.Len(t, store.exactDeletes, 2)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("rejects invalid dependencies and user", func(t *testing.T) {
		service, _, _, _, _ := newActivityServiceFixture(CategoryServerVoice)
		assert.Error(t, service.SuppressAllActivityAlreadyGated(context.Background(), uuid.Nil))
		assert.Error(t, (*ActivityService)(nil).SuppressAllActivityAlreadyGated(
			context.Background(), activityServiceSender,
		))
	})
}

func settingsSuppressionService() *ActivityService {
	service, _, _, _, _ := newActivityServiceFixture(CategoryServerVoice)
	return service
}

func testActivityPolicySettings(
	masterEnabled bool,
	serverTier Tier,
	privateTier Tier,
) ActivityPolicySettings {
	return ActivityPolicySettings{
		MasterEnabled:          masterEnabled,
		ServerVoiceTier:        serverTier,
		ServerVoiceShowDetails: true,
		PrivateCallTier:        privateTier,
		PrivateCallShowDetails: true,
	}
}
