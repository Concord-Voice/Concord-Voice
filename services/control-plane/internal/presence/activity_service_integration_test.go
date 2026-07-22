package presence

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedActivityLifecycle(
	t *testing.T,
	rdb redis.UniversalClient,
	senderID uuid.UUID,
	category Category,
	token uuid.UUID,
	version int64,
	active bool,
) {
	t.Helper()
	key, err := VoiceLifecycleKey(senderID, category)
	require.NoError(t, err)
	activeFlag := "0"
	if active {
		activeFlag = "1"
	}
	require.NoError(t, rdb.HSet(context.Background(), key,
		"token", token.String(), "version", version, "active", activeFlag,
	).Err())
	require.NoError(t, rdb.PExpire(context.Background(), key, ActivityStateTTL).Err())
}

func TestActivityService_RealRedisPersistsRefreshesAndDeletesExactState(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	builder := &activityServiceBuilderStub{built: BuiltActivity{
		Input:       PolicyInput{SenderID: activityServiceSender, Category: CategoryServerVoice, ServerVoice: &ServerVoicePolicyInput{}},
		SourceToken: activityServiceRoom, SourceVersion: activityServiceEvent.UnixMicro(),
	}}
	delivery := &activityServiceDeliveryStub{}
	service := newActivityService(
		&activityServiceCoordinatorStub{}, builder, store,
		func(context.Context, PolicyInput) (Decision, error) {
			return Decision{
				Audience: map[uuid.UUID]bool{activityServiceViewer: true},
				Payload: json.RawMessage(
					`{"channel_id":"22222222-2222-2222-2222-222222222222","server_id":"33333333-3333-3333-3333-333333333333"}`,
				),
				Minimized: true,
			}, nil
		},
		delivery,
	)
	ctx := context.Background()
	seedActivityLifecycle(
		t, rdb, activityServiceSender, CategoryServerVoice,
		activityServiceRoom, activityServiceEvent.UnixMicro(), true,
	)

	require.NoError(t, service.RefreshServerVoice(ctx, activityServiceSender, serverActivityScope(), nil))
	state, found, err := store.Get(ctx, activityServiceSender, CategoryServerVoice)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, activityServiceRoom, state.SourceToken)
	assert.Equal(t, activityServiceEvent.UnixMicro(), state.SourceVersion)
	assert.True(t, state.Minimized)

	key, err := activityKey(activityServiceSender, CategoryServerVoice)
	require.NoError(t, err)
	require.NoError(t, rdb.Expire(ctx, key, 5*time.Second).Err())
	require.NoError(t, service.RefreshServerVoice(ctx, activityServiceSender, serverActivityScope(), nil))
	ttl, err := rdb.PTTL(ctx, key).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, 89*time.Second)
	assert.LessOrEqual(t, ttl, ActivityStateTTL)

	require.NoError(t, service.ClearServerVoice(ctx, activityServiceSender, serverActivityScope(), nil))
	_, found, err = store.Get(ctx, activityServiceSender, CategoryServerVoice)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestActivityService_ActivePublicationCannotRaceNewerTerminal(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	buildStarted := make(chan struct{})
	releaseBuild := make(chan struct{})
	built := BuiltActivity{
		Input: PolicyInput{
			SenderID:    activityServiceSender,
			Category:    CategoryServerVoice,
			ServerVoice: &ServerVoicePolicyInput{},
		},
		SourceToken: activityServiceRoom, SourceVersion: activityServiceEvent.UnixMicro(),
	}
	activeBuilder := &activityServiceBuilderStub{
		built: built,
		onBuild: func() {
			close(buildStarted)
			<-releaseBuild
		},
	}
	authorize := func(context.Context, PolicyInput) (Decision, error) {
		return Decision{
			Audience: map[uuid.UUID]bool{activityServiceViewer: true},
			Payload:  json.RawMessage(`{"channel_id":"active"}`),
		}, nil
	}
	activeDelivery := &activityServiceDeliveryStub{}
	activeService := newActivityService(
		&activityServiceCoordinatorStub{}, activeBuilder, store, authorize, activeDelivery,
	)
	terminalDelivery := &activityServiceDeliveryStub{}
	terminalService := newActivityService(
		&activityServiceCoordinatorStub{},
		&activityServiceBuilderStub{built: built},
		store,
		authorize,
		terminalDelivery,
	)

	activeLifecycleKey, err := VoiceLifecycleKey(activityServiceSender, CategoryServerVoice)
	require.NoError(t, err)
	activeDone := make(chan error, 1)
	go func() {
		activeDone <- activeService.RefreshServerVoice(
			context.Background(), activityServiceSender, serverActivityScope(),
			func(ctx context.Context) (bool, error) {
				if err := rdb.HSet(ctx, activeLifecycleKey,
					"token", activityServiceRoom.String(),
					"version", activityServiceEvent.UnixMicro(), "active", "1",
				).Err(); err != nil {
					return false, err
				}
				if err := rdb.PExpire(ctx, activeLifecycleKey, ActivityStateTTL).Err(); err != nil {
					return false, err
				}
				return true, nil
			},
		)
	}()
	select {
	case <-buildStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("active refresh did not pause after its mutation")
	}

	terminalAt := activityServiceEvent.Add(time.Second)
	terminalScope := serverActivityScope()
	terminalScope.EventAt = terminalAt
	require.NoError(t, terminalService.ClearServerVoice(
		context.Background(), activityServiceSender, terminalScope,
		func(context.Context) (bool, error) {
			seedActivityLifecycle(
				t, rdb, activityServiceSender, CategoryServerVoice,
				activityServiceRoom, terminalAt.UnixMicro(), false,
			)
			return true, nil
		},
	))
	close(releaseBuild)
	require.NoError(t, <-activeDone)

	_, found, err := store.Get(
		context.Background(), activityServiceSender, CategoryServerVoice,
	)
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, activeDelivery.plans, "stale active generation must never be delivered")
	require.Len(t, activeDelivery.disconnects, 1,
		"prepared recipients are conservatively disconnected when publication loses the fence")
}
