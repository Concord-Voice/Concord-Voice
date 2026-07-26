package presence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hiddenSenderRefreshFixture returns a service whose policy suppresses on sender
// base presence, i.e. the level arm of #2444.
func hiddenSenderRefreshFixture(t *testing.T) (
	*ActivityService, *activityServiceStoreStub, *activityServiceDeliveryStub,
) {
	t.Helper()
	service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		return Decision{
			Audience:                   map[uuid.UUID]bool{},
			SuppressedBySenderPresence: true,
		}, nil
	}
	return service, store, delivery
}

// planCategories collects the categories a delivery stub observed, so a test can
// assert per-category coverage without depending on iteration order.
func planCategories(plans []DeliveryPlan) map[Category]int {
	seen := make(map[Category]int, len(plans))
	for _, plan := range plans {
		seen[plan.Category]++
	}
	return seen
}

// storeCallCategories does the same for store calls.
func storeCallCategories(calls []activityStoreCall) map[Category]int {
	seen := make(map[Category]int, len(calls))
	for _, call := range calls {
		seen[call.category]++
	}
	return seen
}

// U5 (spec 8.1): the FIRST suppressed heartbeat that still finds a stored row is
// the one case where the level arm has real work. It must clear the prior
// audience precisely -- one CompareAndDelete, clear-only frames, and never a
// global disconnect. Anything else turns going invisible into an observable
// correlated event for every Rich Presence client on the replica.
func TestActivityService_HiddenSenderLevelArmClearsStoredRowPrecisely(t *testing.T) {
	service, store, delivery := hiddenSenderRefreshFixture(t)
	store.deleteResult = true // a live generation was still stored

	err := service.RefreshServerVoice(
		context.Background(), activityServiceSender, serverActivityScope(), nil,
	)

	require.NoError(t, err)
	require.Len(t, store.deletes, 1, "exactly one CompareAndDelete for the generation")
	assert.Equal(t, activityServiceSender, store.deletes[0].userID)
	assert.Equal(t, CategoryServerVoice, store.deletes[0].category)
	assert.Empty(t, store.gets, "a removed generation needs no successor inspection")
	assert.Empty(t, store.sets, "a suppressed sender never persists new state")
	assert.Zero(t, delivery.disconnectAllCalls,
		"a precise clear must replace the global disconnect")
	assert.Empty(t, delivery.disconnects, "no recipient-scoped disconnect on the happy path")

	require.NotEmpty(t, delivery.plans)
	for _, plan := range delivery.plans {
		assert.Equal(t, activityServiceSender, plan.SenderID)
		assert.Empty(t, plan.UpdateRecipients, "a suppressed sender publishes nothing")
		assert.Contains(t, plan.ClearRecipients, activityServiceViewer)
		assert.NotContains(t, plan.ClearRecipients, activityServiceSender,
			"the sender is never its own recipient (spec 7.1)")
	}
	// The clear covers both categories: the widest prior audience is a superset of
	// whoever could hold a badge, and clearing a category that held nothing is inert.
	assert.Equal(t,
		map[Category]int{CategoryServerVoice: 1, CategoryPrivateCall: 1},
		planCategories(delivery.plans),
		"one clear frame per supported category")
}

// U10 (spec 8.1): when the precise clear cannot be delivered, the fallback is a
// disconnect of exactly those recipients -- never the whole replica. That
// distinction is the entire reason the hidden-sender arm exists.
func TestActivityService_HiddenSenderClearDeliveryFailureDisconnectsOnlyRecipients(t *testing.T) {
	t.Run("level arm", func(t *testing.T) {
		service, store, delivery := hiddenSenderRefreshFixture(t)
		store.deleteResult = true
		deliverErr := errors.New("forced hidden-sender clear delivery failure")
		delivery.deliverErr = deliverErr

		err := service.RefreshServerVoice(
			context.Background(), activityServiceSender, serverActivityScope(), nil,
		)

		require.ErrorIs(t, err, deliverErr)
		assert.Zero(t, delivery.disconnectAllCalls,
			"a delivery failure must not escalate to a global disconnect")
		require.NotEmpty(t, delivery.disconnects)
		for _, recipients := range delivery.disconnects {
			assert.Equal(t, map[uuid.UUID]bool{activityServiceViewer: true}, recipients,
				"the fallback disconnect is scoped to the resolved audience")
		}
	})

	t.Run("edge arm", func(t *testing.T) {
		service, _, _, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		deliverErr := errors.New("forced hidden-sender edge delivery failure")
		delivery.deliverErr = deliverErr

		err := service.SuppressHiddenSenderActivityAlreadyGated(
			context.Background(), activityServiceSender,
		)

		require.ErrorIs(t, err, deliverErr)
		assert.Zero(t, delivery.disconnectAllCalls)
		require.NotEmpty(t, delivery.disconnects)
		for _, recipients := range delivery.disconnects {
			assert.Equal(t, map[uuid.UUID]bool{activityServiceViewer: true}, recipients)
		}
	})
}

// U11 (spec 8.1): the edge arm is category-blind on purpose -- one gate
// acquisition by the caller covers both categories, so both stored generations
// are deleted and both audiences cleared in a single pass.
func TestActivityService_HiddenSenderEdgeCoversBothCategoriesInOnePass(t *testing.T) {
	service, _, store, delivery, coordinator := newActivityServiceFixture(CategoryServerVoice)

	err := service.SuppressHiddenSenderActivityAlreadyGated(
		context.Background(), activityServiceSender,
	)

	require.NoError(t, err)
	assert.Equal(t,
		map[Category]int{CategoryServerVoice: 1, CategoryPrivateCall: 1},
		storeCallCategories(store.exactDeletes),
		"both categories' stored generations are removed")
	assert.Equal(t,
		map[Category]int{CategoryServerVoice: 1, CategoryPrivateCall: 1},
		planCategories(delivery.plans),
		"both categories' audiences are cleared")
	assert.Zero(t, delivery.disconnectAllCalls)
	assert.Zero(t, coordinator.calls,
		"AlreadyGated: the caller owns the sender gate; re-acquiring it would deadlock")
}

// The resolver is the only source of the audience, so a nil resolver must fail
// closed to a disconnect rather than silently leaving a badge published.
func TestActivityService_HiddenSenderLevelArmWithoutResolverFailsClosed(t *testing.T) {
	service, store, delivery := hiddenSenderRefreshFixture(t)
	store.deleteResult = true
	service.settingsRecipients = nil

	err := service.RefreshServerVoice(
		context.Background(), activityServiceSender, serverActivityScope(), nil,
	)

	require.NoError(t, err, "the disconnect is the remedy, not an error to propagate")
	assert.Equal(t, 1, delivery.disconnectAllCalls,
		"no resolver means no way to aim a clear; fail closed")
	assert.Empty(t, delivery.plans)
}

// A resolution failure on the level arm keeps the same fail-closed posture the
// edge arm has, and must not deliver a partial clear.
func TestActivityService_HiddenSenderLevelArmResolutionFailureDisconnectsAll(t *testing.T) {
	service, store, delivery := hiddenSenderRefreshFixture(t)
	store.deleteResult = true
	resolveErr := errors.New("forced hidden-sender level-arm resolution failure")
	service.settingsRecipients = func(
		context.Context, uuid.UUID, ActivityPolicySettings, ActivityPolicySettings,
	) (map[uuid.UUID]bool, error) {
		return nil, resolveErr
	}

	err := service.RefreshServerVoice(
		context.Background(), activityServiceSender, serverActivityScope(), nil,
	)

	require.ErrorIs(t, err, resolveErr)
	assert.Equal(t, 1, delivery.disconnectAllCalls)
	assert.Empty(t, delivery.plans, "no partial clear may be delivered")
}

// The private-call category takes the same path as server voice; the level arm
// must not be server-voice-only.
func TestActivityService_HiddenSenderLevelArmCoversPrivateCall(t *testing.T) {
	service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		return Decision{
			Audience:                   map[uuid.UUID]bool{},
			SuppressedBySenderPresence: true,
		}, nil
	}
	store.deleteResult = true

	err := service.RefreshPrivateCall(
		context.Background(), activityServiceSender, privateActivityScope(), nil, nil,
	)

	require.NoError(t, err)
	require.Len(t, store.deletes, 1)
	assert.Equal(t, CategoryPrivateCall, store.deletes[0].category)
	assert.Zero(t, delivery.disconnectAllCalls)
	assert.NotEmpty(t, delivery.plans)
}

// The move path must honour the hidden-sender reason too (#2444).
//
// suppressRefreshedActivity evaluates the `previous.built != nil` MOVE branch
// BEFORE the presence guard, so a suppressed move reached suppressMovedGeneration,
// whose !deleted path runs disconnectAfterGenerationMiss -> hasGenerationSuccessor
// -> (false, nil) -> disconnectAll. That is the same reason-blind global disconnect
// closed on the clear path in da9b9707d, still open on the move path.
//
// It is repeatable at will, not one-shot: the heartbeat bridge calls
// MoveServerVoice whenever a participant's channel differs from the stored scope,
// so an invisible user hopping between voice channels force-disconnects every
// Rich-Presence client on the replica on every hop.
func TestActivityService_HiddenSenderMoveDoesNotDisconnectAll(t *testing.T) {
	service, builder, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	oldScope := serverActivityScope()
	newScope := oldScope
	newRoom := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	newScope.RoomID = newRoom
	newScope.LifecycleID = newRoom
	newScope.EventAt = oldScope.EventAt.Add(time.Second)

	builder.build = func(_ uuid.UUID, scope Scope) (BuiltActivity, error) {
		return BuiltActivity{
			Input: PolicyInput{
				SenderID: activityServiceSender,
				Category: CategoryServerVoice,
				ServerVoice: &ServerVoicePolicyInput{Context: ServerVoiceContext{
					ChannelID: scope.RoomID,
				}},
			},
			SourceToken: scope.RoomID, SourceVersion: scope.EventAt.UnixMicro(),
		}, nil
	}
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		return Decision{
			Audience:                   map[uuid.UUID]bool{},
			SuppressedBySenderPresence: true,
		}, nil
	}
	// The edge arm already removed the row when the user went invisible.
	store.deleteResult = false

	err := service.MoveServerVoice(
		context.Background(), activityServiceSender, oldScope, newScope,
		func(context.Context) (bool, error) { return true, nil },
	)

	require.NoError(t, err)
	assert.Zero(t, delivery.disconnectAllCalls,
		"an invisible user hopping voice channels must not disconnect the whole replica")
	assert.Empty(t, store.sets, "a suppressed sender publishes no new generation")
}

// storeBackedRecipientResolver mimics the REAL resolver's load-bearing property:
// currentServerSettingsRecipients reconstructs scope from the STORED generation via
// store.Get, and a widest-prior policy makes priorEligible true, so a missing row
// yields "settings evidence unavailable for prior-eligible policy" -- an ERROR.
//
// The package fixture's resolver stub ignores the store entirely and always returns
// a fixed audience, which is why it could not catch a resolve-after-delete ordering
// bug. Any test asserting the level arm's clear ordering must use this instead.
func storeBackedRecipientResolver(
	store *activityServiceStoreStub,
	audience map[uuid.UUID]bool,
) activitySettingsRecipientResolver {
	return func(
		ctx context.Context, userID uuid.UUID, _ ActivityPolicySettings, _ ActivityPolicySettings,
	) (map[uuid.UUID]bool, error) {
		if _, found, err := store.Get(ctx, userID, CategoryServerVoice); err != nil {
			return nil, err
		} else if !found {
			return nil, errors.New(
				"rich-presence server-voice settings evidence unavailable for prior-eligible policy",
			)
		}
		return audience, nil
	}
}

// Regression lock for the resolve-after-delete storm (Gitar review, #2444).
//
// The level arm must resolve the audience while the generation is still stored. If
// it deletes first, the real resolver finds no row, errors on missing prior-eligible
// evidence, and escalates to a full-replica disconnect on the FIRST suppressed
// heartbeat of every sender who goes invisible while in voice -- the exact storm the
// guard exists to prevent.
func TestActivityService_HiddenSenderLevelArmResolvesAudienceBeforeDeleting(t *testing.T) {
	service, store, delivery := hiddenSenderRefreshFixture(t)
	store.deleteResult = true // a live generation is stored
	store.getFound = true     // ... and readable until the delete lands
	service.settingsRecipients = storeBackedRecipientResolver(
		store, map[uuid.UUID]bool{activityServiceViewer: true},
	)

	err := service.RefreshServerVoice(
		context.Background(), activityServiceSender, serverActivityScope(), nil,
	)

	require.NoError(t, err)
	assert.Zero(t, delivery.disconnectAllCalls,
		"resolving after the delete makes the real resolver fail and storms the replica")
	require.NotEmpty(t, delivery.plans, "the prior audience must receive a targeted clear")
	for _, plan := range delivery.plans {
		assert.Contains(t, plan.ClearRecipients, activityServiceViewer)
	}
}

// The steady state must stay quiet even with the store-backed resolver: nothing is
// stored, so the resolver legitimately errors -- and that error must be DISCARDED,
// because the benign terminal is reached via !deleted, not via the resolve.
func TestActivityService_HiddenSenderLevelArmSteadyStateIgnoresResolveError(t *testing.T) {
	service, store, delivery := hiddenSenderRefreshFixture(t)
	store.deleteResult = false // already removed by an earlier suppressed beat
	store.getFound = false     // so the store-backed resolver errors
	service.settingsRecipients = storeBackedRecipientResolver(
		store, map[uuid.UUID]bool{activityServiceViewer: true},
	)

	for i := 0; i < 5; i++ {
		require.NoError(t, service.RefreshServerVoice(
			context.Background(), activityServiceSender, serverActivityScope(), nil,
		))
	}

	assert.Zero(t, delivery.disconnectAllCalls,
		"a discarded steady-state resolve error must not disconnect anyone")
	assert.Empty(t, delivery.plans)
}
