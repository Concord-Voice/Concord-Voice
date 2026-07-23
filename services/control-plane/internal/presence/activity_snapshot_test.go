package presence

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivitySnapshotQueryStaysCandidateCorrelated(t *testing.T) {
	source, err := os.ReadFile("activity_snapshot.go")
	require.NoError(t, err)
	assert.NotContains(t, string(source), "WITH accepted_edges AS",
		"bounded activity snapshots must not materialize the global friendship graph")
}

type activitySnapshotBuilderStub struct {
	built BuiltActivity
	err   error
	calls []Scope
	hook  func(Scope)
	build func(uuid.UUID, Scope) (BuiltActivity, error)
}

func (b *activitySnapshotBuilderStub) Build(
	_ context.Context,
	senderID uuid.UUID,
	scope Scope,
) (BuiltActivity, error) {
	b.calls = append(b.calls, scope)
	if b.hook != nil {
		b.hook(scope)
	}
	if b.build != nil {
		return b.build(senderID, scope)
	}
	return b.built, b.err
}

type activitySnapshotPipelineHook struct {
	mu      sync.Mutex
	batches [][]string
	failAt  int
	err     error
}

type activitySnapshotCoordinatorStub struct {
	mu           sync.Mutex
	recordMu     sync.Mutex
	multiIDs     []uuid.UUID
	multiStarted chan struct{}
	startOnce    sync.Once
}

func (c *activitySnapshotCoordinatorStub) WithSender(
	ctx context.Context,
	_ uuid.UUID,
	work func() error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return work()
}

func (c *activitySnapshotCoordinatorStub) WithSenders(
	ctx context.Context,
	senderIDs []uuid.UUID,
	work func() error,
) error {
	c.recordMu.Lock()
	c.multiIDs = append([]uuid.UUID(nil), senderIDs...)
	c.recordMu.Unlock()
	if c.multiStarted != nil {
		c.startOnce.Do(func() { close(c.multiStarted) })
	}
	return c.WithSender(ctx, uuid.Nil, work)
}

func (*activitySnapshotPipelineHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (*activitySnapshotPipelineHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (h *activitySnapshotPipelineHook) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		names := make([]string, len(commands))
		for index, command := range commands {
			names[index] = command.Name()
		}
		h.mu.Lock()
		h.batches = append(h.batches, names)
		batchNumber := len(h.batches)
		h.mu.Unlock()
		if batchNumber == h.failAt {
			return h.err
		}
		return next(ctx, commands)
	}
}

func TestLoadActivitySnapshotStatesUsesBoundedExactGetPipelines(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	hook := &activitySnapshotPipelineHook{}
	rdb.AddHook(hook)

	keys := make([]activitySnapshotKey, activitySnapshotRedisBatchSize*2+2)
	for index := range keys {
		keys[index] = activitySnapshotKey{
			SenderID: uuid.New(),
			Category: CategoryServerVoice,
		}
	}

	states, err := loadActivitySnapshotStates(
		context.Background(),
		NewActivityStore(rdb),
		keys,
	)
	require.NoError(t, err)
	assert.Empty(t, states)

	hook.mu.Lock()
	batches := append([][]string(nil), hook.batches...)
	hook.mu.Unlock()
	require.Len(t, batches, 3)
	require.Equal(t, []int{activitySnapshotRedisBatchSize, activitySnapshotRedisBatchSize, 2}, []int{
		len(batches[0]), len(batches[1]), len(batches[2]),
	})
	for _, batch := range batches {
		assert.LessOrEqual(t, len(batch), activitySnapshotRedisBatchSize)
		for _, command := range batch {
			if !assert.Equal(t, "eval", command) {
				break
			}
		}
	}
}

func TestLoadActivitySnapshotStatesDeletesMalformedValues(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	key := activitySnapshotKey{SenderID: uuid.New(), Category: CategoryPrivateCall}
	redisKey, err := activityKey(key.SenderID, key.Category)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), redisKey, `{"payload":"untrusted"}`, time.Minute).Err())

	states, err := loadActivitySnapshotStates(context.Background(), store, []activitySnapshotKey{key})
	require.NoError(t, err)
	assert.Empty(t, states)
	assert.Zero(t, rdb.Exists(context.Background(), redisKey).Val())
}

func TestLoadActivitySnapshotStatesWrongTypesHealWithoutAbortingPeers(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	validKey := activitySnapshotKey{
		SenderID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Category: CategoryServerVoice,
	}
	missingKey := activitySnapshotKey{
		SenderID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Category: CategoryServerVoice,
	}
	malformedKey := activitySnapshotKey{
		SenderID: uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Category: CategoryServerVoice,
	}
	validState := wrongTypeActivityTestState()
	stored, err := store.CompareAndSet(
		ctx, validKey.SenderID, validKey.Category, validState,
	)
	require.NoError(t, err)
	require.True(t, stored)

	malformedRedisKey, err := activityKey(malformedKey.SenderID, malformedKey.Category)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(
		ctx, malformedRedisKey, `{"payload":"untrusted"}`, time.Minute,
	).Err())

	wrongTypeSeeds := wrongTypeActivitySeeds()
	keys := make([]activitySnapshotKey, 0, 3+len(wrongTypeSeeds))
	keys = append(keys, validKey, missingKey, malformedKey)
	invalidRedisKeys := make([]string, 0, 1+len(wrongTypeSeeds))
	invalidRedisKeys = append(invalidRedisKeys, malformedRedisKey)
	wrongTypeSenderIDs := []uuid.UUID{
		uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		uuid.MustParse("55555555-5555-5555-5555-555555555555"),
		uuid.MustParse("66666666-6666-6666-6666-666666666666"),
		uuid.MustParse("77777777-7777-7777-7777-777777777777"),
		uuid.MustParse("88888888-8888-8888-8888-888888888888"),
	}
	for index, seed := range wrongTypeSeeds {
		key := activitySnapshotKey{
			SenderID: wrongTypeSenderIDs[index],
			Category: CategoryServerVoice,
		}
		redisKey, keyErr := activityKey(key.SenderID, key.Category)
		require.NoError(t, keyErr)
		require.NoError(t, seed.seed(ctx, rdb, redisKey))
		keys = append(keys, key)
		invalidRedisKeys = append(invalidRedisKeys, redisKey)
	}
	require.NoError(t, rdb.Set(ctx, "sentinel:2405:snapshot", "untouched", time.Minute).Err())

	states, err := loadActivitySnapshotStates(ctx, store, keys)
	require.NoError(t, err)
	assert.Equal(t, map[activitySnapshotKey]ActivityState{validKey: validState}, states)

	validRedisKey, err := activityKey(validKey.SenderID, validKey.Category)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rdb.Exists(ctx, validRedisKey).Val())
	missingRedisKey, err := activityKey(missingKey.SenderID, missingKey.Category)
	require.NoError(t, err)
	assert.Zero(t, rdb.Exists(ctx, missingRedisKey).Val())
	for _, redisKey := range invalidRedisKeys {
		assert.Zero(t, rdb.Exists(ctx, redisKey).Val(), redisKey)
	}
	assert.Equal(t, "untouched", rdb.Get(ctx, "sentinel:2405:snapshot").Val())
}

func TestLoadActivitySnapshotStatesCommandErrorReturnsNoPartialState(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	ctx := context.Background()
	store := NewActivityStore(rdb)
	key := activitySnapshotKey{
		SenderID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Category: CategoryServerVoice,
	}
	stored, err := store.CompareAndSet(
		ctx, key.SenderID, key.Category, wrongTypeActivityTestState(),
	)
	require.NoError(t, err)
	require.True(t, stored)

	forcedErr := errors.New("forced snapshot pipeline failure")
	rdb.AddHook(&activitySnapshotPipelineHook{failAt: 2, err: forcedErr})
	keys := make([]activitySnapshotKey, activitySnapshotRedisBatchSize+1)
	for index := range keys {
		keys[index] = key
	}

	states, err := loadActivitySnapshotStates(ctx, store, keys)
	assert.Nil(t, states)
	assert.ErrorIs(t, err, forcedErr)
}

func TestGroupActivitySnapshotCandidatesGuardsBeforeDedupAndMarksAmbiguity(t *testing.T) {
	senderID := uuid.New()
	roomID := uuid.New()
	base := activitySnapshotCandidate{
		activitySnapshotKey: activitySnapshotKey{SenderID: senderID, Category: CategoryServerVoice},
		RoomID:              roomID,
		LifecycleAt:         time.Unix(100, 0),
	}
	overLimit := make([]activitySnapshotCandidate, activitySnapshotCandidateLimit+1)
	for index := range overLimit {
		overLimit[index] = base
	}

	_, err := groupActivitySnapshotCandidates(overLimit)
	assert.ErrorIs(t, err, ErrActivitySnapshotCandidateLimit)

	second := base
	second.RoomID = uuid.New()
	second.LifecycleAt = base.LifecycleAt.Add(time.Second)
	groups, err := groupActivitySnapshotCandidates([]activitySnapshotCandidate{base, second})
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.True(t, groups[base.activitySnapshotKey].Ambiguous)
	assert.Equal(t, second.LifecycleAt.UnixMicro(), groups[base.activitySnapshotKey].MaxSourceVersion)

	poisoned := base
	poisoned.LifecycleAt = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = groupActivitySnapshotCandidates([]activitySnapshotCandidate{poisoned})
	assert.ErrorIs(t, err, ErrInvalidActivitySnapshot)
}

func TestActivitySnapshotUsesFreshPolicyProjectionAndRetainsUnauthorizedState(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	senderID := uuid.New()
	channelID := uuid.New()
	lifecycleAt := time.Unix(1784088000, 456000000)
	state := ActivityState{
		SourceToken:   channelID,
		SourceVersion: lifecycleAt.UnixMicro(),
		Minimized:     false,
		Payload:       json.RawMessage(`{"channel_id":"stale"}`),
		UpdatedAt:     lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, CategoryServerVoice, state)
	require.NoError(t, err)
	require.True(t, stored)
	require.NoError(t, setActivityLifecycleForTest(
		ctx, rdb, senderID, CategoryServerVoice, state, true,
	))

	builder := &activitySnapshotBuilderStub{built: BuiltActivity{
		Input:       PolicyInput{SenderID: senderID, Category: CategoryServerVoice},
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
	}}
	service := newActivitySnapshotService(nil, builder, store, func(
		_ context.Context,
		_ PolicyInput,
	) (Decision, error) {
		return Decision{
			Audience:  map[uuid.UUID]bool{viewerID: true},
			Payload:   json.RawMessage(`{"channel_id":"fresh"}`),
			Minimized: true,
		}, nil
	})
	service.candidateLoader = func(context.Context, uuid.UUID) ([]activitySnapshotCandidate, error) {
		return []activitySnapshotCandidate{{
			activitySnapshotKey: activitySnapshotKey{SenderID: senderID, Category: CategoryServerVoice},
			RoomID:              channelID, LifecycleAt: lifecycleAt,
		}}, nil
	}

	snapshot, err := service.Snapshot(ctx, viewerID)
	require.NoError(t, err)
	require.Contains(t, snapshot, senderID)
	entry, found := snapshot[senderID][CategoryServerVoice]
	require.True(t, found)
	assert.True(t, entry.Minimized)
	assert.JSONEq(t, `{"channel_id":"fresh"}`, string(entry.Payload))
	assert.Equal(t, lifecycleAt.Unix(), entry.UpdatedAt)
	require.Len(t, builder.calls, 1)
	assert.Equal(t, channelID, builder.calls[0].RoomID)
	assert.Equal(t, channelID, builder.calls[0].LifecycleID)

	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		return Decision{Audience: map[uuid.UUID]bool{}}, nil
	}
	snapshot, err = service.Snapshot(ctx, viewerID)
	require.NoError(t, err)
	assert.Empty(t, snapshot)
	_, found, err = store.Get(ctx, senderID, CategoryServerVoice)
	require.NoError(t, err)
	assert.True(t, found, "viewer exclusion must not delete state valid for other viewers")
}

func TestActivitySnapshotFinalizationNormalizesLaggingCandidateToCurrentGeneration(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	senderID := uuid.New()
	channelID := uuid.New()
	candidateAt := time.Unix(1784088000, 1000)
	currentAt := candidateAt.Add(time.Second)
	state := ActivityState{
		SourceToken: channelID, SourceVersion: currentAt.UnixMicro(),
		Payload: json.RawMessage(`{"channel_id":"current"}`), UpdatedAt: currentAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, CategoryServerVoice, state)
	require.NoError(t, err)
	require.True(t, stored)
	require.NoError(t, setActivityLifecycleForTest(
		ctx, rdb, senderID, CategoryServerVoice, state, true,
	))
	builder := &activitySnapshotBuilderStub{built: BuiltActivity{
		Input:       PolicyInput{SenderID: senderID, Category: CategoryServerVoice},
		SourceToken: channelID, SourceVersion: currentAt.UnixMicro(),
	}}
	authorizeCalls := 0
	service := newActivitySnapshotService(nil, builder, store, func(context.Context, PolicyInput) (Decision, error) {
		authorizeCalls++
		return Decision{
			Audience: map[uuid.UUID]bool{viewerID: true},
			Payload:  json.RawMessage(`{"channel_id":"fresh"}`),
		}, nil
	})
	service.candidateLoader = oneActivitySnapshotCandidate(senderID, channelID, candidateAt)

	projected, err := service.Snapshot(ctx, viewerID)
	require.NoError(t, err)
	finalized, err := finalizeActivitySnapshotForTest(ctx, service, viewerID, projected)
	require.NoError(t, err)
	require.Contains(t, finalized, senderID)
	assert.Contains(t, finalized[senderID], CategoryServerVoice)
	assert.Len(t, builder.calls, 2)
	assert.Equal(t, currentAt.UnixMicro(), builder.calls[1].EventAt.UnixMicro())
	assert.Equal(t, 2, authorizeCalls)
}

func TestActivitySnapshotFinalizationDropsViewerRevokedAfterProjection(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	senderID := uuid.New()
	channelID := uuid.New()
	lifecycleAt := time.Unix(1784088000, 456000000)
	state := ActivityState{
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{"channel_id":"stale"}`), UpdatedAt: lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, CategoryServerVoice, state)
	require.NoError(t, err)
	require.True(t, stored)
	require.NoError(t, setActivityLifecycleForTest(
		ctx, rdb, senderID, CategoryServerVoice, state, true,
	))

	service := newActivitySnapshotService(nil, &activitySnapshotBuilderStub{built: BuiltActivity{
		Input:       PolicyInput{SenderID: senderID, Category: CategoryServerVoice},
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
	}}, store, func(context.Context, PolicyInput) (Decision, error) {
		return Decision{
			Audience: map[uuid.UUID]bool{viewerID: true},
			Payload:  json.RawMessage(`{"channel_id":"fresh"}`),
		}, nil
	})
	service.candidateLoader = oneActivitySnapshotCandidate(senderID, channelID, lifecycleAt)

	projected, err := service.Snapshot(ctx, viewerID)
	require.NoError(t, err)
	require.Contains(t, projected, senderID)
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		return Decision{Audience: map[uuid.UUID]bool{}}, nil
	}

	finalized, err := finalizeActivitySnapshotForTest(ctx, service, viewerID, projected)
	require.NoError(t, err)
	assert.Empty(t, finalized)

	_, found, err := store.Get(ctx, senderID, CategoryServerVoice)
	require.NoError(t, err)
	assert.True(t, found, "viewer revocation must not delete state valid for another viewer")
}

func TestActivitySnapshotFinalizationReloadsExactGenerationWithoutDeletingSuccessor(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	senderID := uuid.New()
	channelID := uuid.New()
	lifecycleAt := time.Unix(1784088000, 456000000)
	original := ActivityState{
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{"channel_id":"original"}`), UpdatedAt: lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, CategoryServerVoice, original)
	require.NoError(t, err)
	require.True(t, stored)
	require.NoError(t, setActivityLifecycleForTest(
		ctx, rdb, senderID, CategoryServerVoice, original, true,
	))

	authorizeCalls := 0
	service := newActivitySnapshotService(nil, &activitySnapshotBuilderStub{built: BuiltActivity{
		Input:       PolicyInput{SenderID: senderID, Category: CategoryServerVoice},
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
	}}, store, func(context.Context, PolicyInput) (Decision, error) {
		authorizeCalls++
		return Decision{
			Audience: map[uuid.UUID]bool{viewerID: true},
			Payload:  json.RawMessage(`{"channel_id":"fresh"}`),
		}, nil
	})
	service.candidateLoader = oneActivitySnapshotCandidate(senderID, channelID, lifecycleAt)

	projected, err := service.Snapshot(ctx, viewerID)
	require.NoError(t, err)
	require.Contains(t, projected, senderID)
	successor := ActivityState{
		SourceToken: uuid.New(), SourceVersion: original.SourceVersion + 1,
		Payload: json.RawMessage(`{"channel_id":"successor"}`), UpdatedAt: original.UpdatedAt + 1,
	}
	require.NoError(t, setActivityLifecycleForTest(
		ctx, rdb, senderID, CategoryServerVoice, successor, true,
	))
	stored, err = store.CompareAndSet(ctx, senderID, CategoryServerVoice, successor)
	require.NoError(t, err)
	require.True(t, stored)

	finalized, err := finalizeActivitySnapshotForTest(ctx, service, viewerID, projected)
	require.NoError(t, err)
	assert.Empty(t, finalized)
	assert.Equal(t, 1, authorizeCalls, "a superseded projection must be rejected before reauthorization")

	current, found, err := store.Get(ctx, senderID, CategoryServerVoice)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, successor, current, "finalization must never delete a successor generation")
}

func TestActivitySnapshotFinalizationRebuildsPrivateCallAfterLeaseRevocation(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	senderID := uuid.New()
	peerID := uuid.New()
	conversationID := uuid.New()
	callID := uuid.New()
	lifecycleAt := time.Unix(1784088000, 456000000)
	original := ActivityState{
		SourceToken: callID, SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, CategoryPrivateCall, original)
	require.NoError(t, err)
	require.True(t, stored)
	require.NoError(t, setActivityLifecycleForTest(
		ctx, rdb, senderID, CategoryPrivateCall, original, true,
	))

	builder := &activitySnapshotBuilderStub{built: BuiltActivity{
		Input: PolicyInput{SenderID: senderID, Category: CategoryPrivateCall,
			PrivateCall: &PrivateCallPolicyInput{Context: PrivateCallContext{
				ConversationID: conversationID, ParticipantIDs: []uuid.UUID{senderID, peerID},
			}}},
		SourceToken: callID, SourceVersion: lifecycleAt.UnixMicro(),
	}}
	authorizeCalls := 0
	coordinator := &activitySnapshotCoordinatorStub{}
	service := newActivitySnapshotService(nil, builder, store, func(context.Context, PolicyInput) (Decision, error) {
		authorizeCalls++
		return Decision{
			Audience: map[uuid.UUID]bool{viewerID: true},
			Payload:  json.RawMessage(`{"call_type":"dm","participant_count":2}`),
		}, nil
	}, coordinator)
	service.candidateLoader = func(context.Context, uuid.UUID) ([]activitySnapshotCandidate, error) {
		return []activitySnapshotCandidate{{
			activitySnapshotKey: activitySnapshotKey{SenderID: senderID, Category: CategoryPrivateCall},
			RoomID:              conversationID, LifecycleAt: lifecycleAt,
		}}, nil
	}

	projected, err := service.Snapshot(ctx, viewerID)
	require.NoError(t, err)
	require.Contains(t, projected, senderID)
	builder.err = ErrActivityNotCurrent // the authoritative call lease ended after projection

	finalized, err := finalizeActivitySnapshotForTest(ctx, service, viewerID, projected)
	require.NoError(t, err)
	assert.Empty(t, finalized)
	assert.Len(t, builder.calls, 2, "publication finalization must re-run builder/lease verification")
	assert.Equal(t, 1, authorizeCalls, "an ended lease must be rejected before reauthorization")
	assert.ElementsMatch(t, []uuid.UUID{senderID, peerID}, coordinator.multiIDs,
		"publication must gate the sender and every projected call participant")

	_, found, err := store.Get(ctx, senderID, CategoryPrivateCall)
	require.NoError(t, err)
	assert.False(t, found, "an exact ended-lease generation should self-heal from Redis")
}

func TestActivitySnapshotFinalizationLeaseRevocationDoesNotDeleteConcurrentSuccessor(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	senderID := uuid.New()
	conversationID := uuid.New()
	callID := uuid.New()
	lifecycleAt := time.Unix(1784088000, 456000000)
	original := ActivityState{
		SourceToken: callID, SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, CategoryPrivateCall, original)
	require.NoError(t, err)
	require.True(t, stored)
	require.NoError(t, setActivityLifecycleForTest(
		ctx, rdb, senderID, CategoryPrivateCall, original, true,
	))
	builder := &activitySnapshotBuilderStub{built: BuiltActivity{
		Input: PolicyInput{SenderID: senderID, Category: CategoryPrivateCall,
			PrivateCall: &PrivateCallPolicyInput{Context: PrivateCallContext{
				ConversationID: conversationID, ParticipantIDs: []uuid.UUID{senderID},
			}}},
		SourceToken: callID, SourceVersion: original.SourceVersion,
	}}
	service := newActivitySnapshotService(nil, builder, store, func(context.Context, PolicyInput) (Decision, error) {
		return Decision{
			Audience: map[uuid.UUID]bool{viewerID: true},
			Payload:  json.RawMessage(`{"call_type":"dm","participant_count":2}`),
		}, nil
	})
	service.candidateLoader = func(context.Context, uuid.UUID) ([]activitySnapshotCandidate, error) {
		return []activitySnapshotCandidate{{
			activitySnapshotKey: activitySnapshotKey{SenderID: senderID, Category: CategoryPrivateCall},
			RoomID:              conversationID, LifecycleAt: lifecycleAt,
		}}, nil
	}
	projected, err := service.Snapshot(ctx, viewerID)
	require.NoError(t, err)

	successor := ActivityState{
		SourceToken: uuid.New(), SourceVersion: original.SourceVersion + 1,
		Payload: json.RawMessage(`{"call_type":"group"}`), UpdatedAt: original.UpdatedAt + 1,
	}
	builder.err = ErrActivityNotCurrent
	builder.hook = func(Scope) {
		require.NoError(t, setActivityLifecycleForTest(
			ctx, rdb, senderID, CategoryPrivateCall, successor, true,
		))
		stored, hookErr := store.CompareAndSet(ctx, senderID, CategoryPrivateCall, successor)
		require.NoError(t, hookErr)
		require.True(t, stored)
		builder.hook = nil
	}

	finalized, err := finalizeActivitySnapshotForTest(ctx, service, viewerID, projected)
	require.NoError(t, err)
	assert.Empty(t, finalized)
	current, found, err := store.Get(ctx, senderID, CategoryPrivateCall)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, successor, current, "compare-delete must not remove a successor installed after exact reload")
}

func TestActivitySnapshotFinalizationOmitsChangedPrivateParticipantSet(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	senderID := uuid.New()
	peerID := uuid.New()
	newParticipantID := uuid.New()
	conversationID := uuid.New()
	callID := uuid.New()
	lifecycleAt := time.Unix(1784088000, 456000000)
	state := ActivityState{
		SourceToken: callID, SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, CategoryPrivateCall, state)
	require.NoError(t, err)
	require.True(t, stored)
	require.NoError(t, setActivityLifecycleForTest(
		ctx, rdb, senderID, CategoryPrivateCall, state, true,
	))

	buildCalls := 0
	builder := &activitySnapshotBuilderStub{build: func(
		actualSenderID uuid.UUID,
		_ Scope,
	) (BuiltActivity, error) {
		buildCalls++
		participantIDs := []uuid.UUID{senderID, peerID}
		if buildCalls > 1 {
			participantIDs = append(participantIDs, newParticipantID)
		}
		return BuiltActivity{
			Input: PolicyInput{SenderID: actualSenderID, Category: CategoryPrivateCall,
				PrivateCall: &PrivateCallPolicyInput{Context: PrivateCallContext{
					ConversationID: conversationID, ParticipantIDs: participantIDs,
				}}},
			SourceToken: callID, SourceVersion: lifecycleAt.UnixMicro(),
		}, nil
	}}
	authorizeCalls := 0
	coordinator := &activitySnapshotCoordinatorStub{}
	service := newActivitySnapshotService(nil, builder, store, func(
		context.Context,
		PolicyInput,
	) (Decision, error) {
		authorizeCalls++
		return Decision{
			Audience: map[uuid.UUID]bool{viewerID: true},
			Payload:  json.RawMessage(`{"call_type":"dm","participant_count":2}`),
		}, nil
	}, coordinator)
	service.candidateLoader = func(context.Context, uuid.UUID) ([]activitySnapshotCandidate, error) {
		return []activitySnapshotCandidate{{
			activitySnapshotKey: activitySnapshotKey{
				SenderID: senderID, Category: CategoryPrivateCall,
			},
			RoomID: conversationID, LifecycleAt: lifecycleAt,
		}}, nil
	}

	projected, err := service.Snapshot(ctx, viewerID)
	require.NoError(t, err)
	finalized, err := finalizeActivitySnapshotForTest(ctx, service, viewerID, projected)
	require.NoError(t, err)

	assert.Empty(t, finalized)
	assert.Equal(t, 2, buildCalls)
	assert.Equal(t, 1, authorizeCalls,
		"a participant-set change must be omitted before final authorization")
	current, found, err := store.Get(ctx, senderID, CategoryPrivateCall)
	require.NoError(t, err)
	require.True(t, found, "a participant-set race must not delete a current sender generation")
	assert.Equal(t, state, current)
	assert.NotContains(t, coordinator.multiIDs, newParticipantID,
		"a participant discovered after gate acquisition must force fail-closed omission")
}

func TestActivitySnapshotFinalizationSerializesSettingsRevocationThroughPublication(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	senderID := uuid.New()
	channelID := uuid.New()
	lifecycleAt := time.Unix(1784088000, 456000000)
	state := ActivityState{
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{"channel_id":"current"}`), UpdatedAt: lifecycleAt.Unix(),
	}
	stored, err := store.CompareAndSet(ctx, senderID, CategoryServerVoice, state)
	require.NoError(t, err)
	require.True(t, stored)
	require.NoError(t, setActivityLifecycleForTest(
		ctx, rdb, senderID, CategoryServerVoice, state, true,
	))
	allowed := true
	coordinator := &activitySnapshotCoordinatorStub{}
	service := newActivitySnapshotService(nil, &activitySnapshotBuilderStub{built: BuiltActivity{
		Input:       PolicyInput{SenderID: senderID, Category: CategoryServerVoice},
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
	}}, store, func(context.Context, PolicyInput) (Decision, error) {
		audience := make(map[uuid.UUID]bool)
		if allowed {
			audience[viewerID] = true
		}
		return Decision{Audience: audience, Payload: json.RawMessage(`{"channel_id":"fresh"}`)}, nil
	}, coordinator)
	service.candidateLoader = oneActivitySnapshotCandidate(senderID, channelID, lifecycleAt)
	projected, err := service.Snapshot(ctx, viewerID)
	require.NoError(t, err)
	require.Contains(t, projected, senderID)

	publicationObserved := make(chan bool, 1)
	releasePublication := make(chan struct{})
	finalized := make(chan error, 1)
	go func() {
		finalized <- service.FinalizeSnapshot(ctx, viewerID, projected, func(snapshot ActivitySnapshot) error {
			_, containsSender := snapshot[senderID]
			publicationObserved <- containsSender
			<-releasePublication
			return nil
		})
	}()
	var containsSender bool
	select {
	case containsSender = <-publicationObserved:
	case finalizeErr := <-finalized:
		require.NoError(t, finalizeErr)
		t.Fatal("publication callback was not invoked")
	}

	revocationStarted := make(chan struct{})
	revocationEntered := make(chan struct{})
	revoked := make(chan error, 1)
	go func() {
		close(revocationStarted)
		revoked <- coordinator.WithSender(ctx, senderID, func() error {
			allowed = false
			close(revocationEntered)
			return nil
		})
	}()
	<-revocationStarted
	select {
	case <-revocationEntered:
		t.Fatal("settings revocation committed between final authorization and publication")
	default:
	}
	close(releasePublication)
	require.NoError(t, <-finalized)
	require.NoError(t, <-revoked)
	assert.True(t, containsSender)
}

func TestActivitySnapshotFinalizationSerializesTerminalAndMoveBeforeExactReload(t *testing.T) {
	for _, test := range []struct {
		name       string
		wantStored bool
		mutate     func(context.Context, *redis.Client, *ActivityStore, uuid.UUID, ActivityState) error
	}{
		{
			name: "terminal",
			mutate: func(ctx context.Context, rdb *redis.Client, store *ActivityStore, senderID uuid.UUID, state ActivityState) error {
				if err := setActivityLifecycleForTest(
					ctx, rdb, senderID, CategoryServerVoice, state, false,
				); err != nil {
					return err
				}
				return store.Delete(ctx, senderID, CategoryServerVoice)
			},
		},
		{
			name: "move", wantStored: true,
			mutate: func(ctx context.Context, rdb *redis.Client, store *ActivityStore, senderID uuid.UUID, state ActivityState) error {
				state.SourceToken = uuid.New()
				state.SourceVersion++
				state.UpdatedAt++
				if err := setActivityLifecycleForTest(
					ctx, rdb, senderID, CategoryServerVoice, state, true,
				); err != nil {
					return err
				}
				stored, err := store.CompareAndSet(ctx, senderID, CategoryServerVoice, state)
				if err != nil {
					return err
				}
				if !stored {
					return errors.New("move successor was not stored")
				}
				return nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rdb := setupActivityStoreRedis(t)
			store := NewActivityStore(rdb)
			ctx := context.Background()
			viewerID := uuid.New()
			senderID := uuid.New()
			channelID := uuid.New()
			lifecycleAt := time.Unix(1784088000, 456000000)
			state := ActivityState{
				SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
				Payload: json.RawMessage(`{"channel_id":"current"}`), UpdatedAt: lifecycleAt.Unix(),
			}
			stored, err := store.CompareAndSet(ctx, senderID, CategoryServerVoice, state)
			require.NoError(t, err)
			require.True(t, stored)
			require.NoError(t, setActivityLifecycleForTest(
				ctx, rdb, senderID, CategoryServerVoice, state, true,
			))
			coordinator := &activitySnapshotCoordinatorStub{multiStarted: make(chan struct{})}
			service := newActivitySnapshotService(nil, &activitySnapshotBuilderStub{built: BuiltActivity{
				Input:       PolicyInput{SenderID: senderID, Category: CategoryServerVoice},
				SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
			}}, store, func(context.Context, PolicyInput) (Decision, error) {
				return Decision{
					Audience: map[uuid.UUID]bool{viewerID: true},
					Payload:  json.RawMessage(`{"channel_id":"fresh"}`),
				}, nil
			}, coordinator)
			service.candidateLoader = oneActivitySnapshotCandidate(senderID, channelID, lifecycleAt)
			projected, err := service.Snapshot(ctx, viewerID)
			require.NoError(t, err)
			require.Contains(t, projected, senderID)

			releaseWriter := make(chan struct{})
			writerEntered := make(chan struct{})
			writerDone := make(chan error, 1)
			go func() {
				writerDone <- coordinator.WithSender(ctx, senderID, func() error {
					close(writerEntered)
					<-releaseWriter
					return test.mutate(ctx, rdb, store, senderID, state)
				})
			}()
			<-writerEntered

			var published ActivitySnapshot
			finalized := make(chan error, 1)
			go func() {
				finalized <- service.FinalizeSnapshot(ctx, viewerID, projected, func(snapshot ActivitySnapshot) error {
					published = snapshot
					return nil
				})
			}()
			<-coordinator.multiStarted
			close(releaseWriter)
			require.NoError(t, <-writerDone)
			require.NoError(t, <-finalized)
			assert.Empty(t, published, "terminal or move must win before exact reload and stale publication")
			_, found, err := store.Get(ctx, senderID, CategoryServerVoice)
			require.NoError(t, err)
			assert.Equal(t, test.wantStored, found, "exact cleanup must preserve a move successor")
		})
	}
}

func TestActivitySnapshotOmitsTerminalFenceWithLingeringAuthoritativeRow(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(context.Context, *ActivitySnapshotService, uuid.UUID, ActivitySnapshot) (ActivitySnapshot, error)
	}{
		{
			name: "initial snapshot",
			run: func(ctx context.Context, service *ActivitySnapshotService, viewerID uuid.UUID, _ ActivitySnapshot) (ActivitySnapshot, error) {
				return service.Snapshot(ctx, viewerID)
			},
		},
		{
			name: "publication finalizer",
			run: func(ctx context.Context, service *ActivitySnapshotService, viewerID uuid.UUID, projected ActivitySnapshot) (ActivitySnapshot, error) {
				return finalizeActivitySnapshotForTest(ctx, service, viewerID, projected)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rdb := setupActivityStoreRedis(t)
			store := NewActivityStore(rdb)
			ctx := context.Background()
			viewerID := uuid.New()
			senderID := uuid.New()
			channelID := uuid.New()
			lifecycleAt := time.Unix(1784088000, 456000000)
			state := ActivityState{
				SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
				Payload: json.RawMessage(`{"channel_id":"current"}`), UpdatedAt: lifecycleAt.Unix(),
			}
			stored, err := store.CompareAndSet(ctx, senderID, CategoryServerVoice, state)
			require.NoError(t, err)
			require.True(t, stored)
			require.NoError(t, setActivityLifecycleForTest(
				ctx, rdb, senderID, CategoryServerVoice, state, true,
			))
			service := newActivitySnapshotService(nil, &activitySnapshotBuilderStub{built: BuiltActivity{
				Input:       PolicyInput{SenderID: senderID, Category: CategoryServerVoice},
				SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
			}}, store, func(context.Context, PolicyInput) (Decision, error) {
				return Decision{
					Audience: map[uuid.UUID]bool{viewerID: true},
					Payload:  json.RawMessage(`{"channel_id":"fresh"}`),
				}, nil
			}, &activitySnapshotCoordinatorStub{})
			service.candidateLoader = oneActivitySnapshotCandidate(senderID, channelID, lifecycleAt)
			projected, err := service.Snapshot(ctx, viewerID)
			require.NoError(t, err)
			require.Contains(t, projected, senderID)

			// The terminal fence commits before a failed PostgreSQL DELETE. The
			// builder still sees the lingering row, but reconnect must not revive it.
			require.NoError(t, setActivityLifecycleForTest(
				ctx, rdb, senderID, CategoryServerVoice, state, false,
			))
			snapshot, err := test.run(ctx, service, viewerID, projected)
			require.NoError(t, err)
			assert.Empty(t, snapshot)
			_, found, err := store.Get(ctx, senderID, CategoryServerVoice)
			require.NoError(t, err)
			assert.False(t, found, "inactive exact activity generation should self-heal")
		})
	}
}

func TestActivitySnapshotMalformedLifecycleFailsGloballyWithoutPartialProjection(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	goodSender := uuid.New()
	badSender := uuid.New()
	lifecycleAt := time.Unix(1784088000, 456000000)
	states := make(map[uuid.UUID]ActivityState)
	for _, senderID := range []uuid.UUID{goodSender, badSender} {
		state := ActivityState{
			SourceToken: uuid.New(), SourceVersion: lifecycleAt.UnixMicro(),
			Payload: json.RawMessage(`{"channel_id":"current"}`), UpdatedAt: lifecycleAt.Unix(),
		}
		states[senderID] = state
		stored, err := store.CompareAndSet(ctx, senderID, CategoryServerVoice, state)
		require.NoError(t, err)
		require.True(t, stored)
	}
	require.NoError(t, setActivityLifecycleForTest(
		ctx, rdb, goodSender, CategoryServerVoice, states[goodSender], true,
	))
	badKey, err := VoiceLifecycleKey(badSender, CategoryServerVoice)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(ctx, badKey, "corrupt", ActivityStateTTL).Err())

	service := newActivitySnapshotService(nil, &activitySnapshotBuilderStub{build: func(
		senderID uuid.UUID,
		_ Scope,
	) (BuiltActivity, error) {
		state := states[senderID]
		return BuiltActivity{
			Input:       PolicyInput{SenderID: senderID, Category: CategoryServerVoice},
			SourceToken: state.SourceToken, SourceVersion: state.SourceVersion,
		}, nil
	}}, store, func(context.Context, PolicyInput) (Decision, error) {
		return Decision{
			Audience: map[uuid.UUID]bool{viewerID: true},
			Payload:  json.RawMessage(`{"channel_id":"fresh"}`),
		}, nil
	})
	service.candidateLoader = func(context.Context, uuid.UUID) ([]activitySnapshotCandidate, error) {
		return []activitySnapshotCandidate{
			{
				activitySnapshotKey: activitySnapshotKey{SenderID: goodSender, Category: CategoryServerVoice},
				RoomID:              states[goodSender].SourceToken, LifecycleAt: lifecycleAt,
			},
			{
				activitySnapshotKey: activitySnapshotKey{SenderID: badSender, Category: CategoryServerVoice},
				RoomID:              states[badSender].SourceToken, LifecycleAt: lifecycleAt,
			},
		}, nil
	}

	snapshot, err := service.Snapshot(ctx, viewerID)
	assert.ErrorIs(t, err, ErrMalformedActivityLifecycle)
	assert.Nil(t, snapshot)
	assert.Zero(t, rdb.Exists(ctx, badKey).Val(), "malformed lifecycle must self-heal")
}

func TestActivitySnapshotRejectsStaleAmbiguousAndFailedPolicyState(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	senderID := uuid.New()
	channelID := uuid.New()
	lifecycleAt := time.Unix(1784088000, 0)
	state := ActivityState{
		SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
		Payload: json.RawMessage(`{}`), UpdatedAt: lifecycleAt.Unix(),
	}
	put := func() {
		stored, err := store.CompareAndSet(ctx, senderID, CategoryServerVoice, state)
		require.NoError(t, err)
		require.True(t, stored)
		require.NoError(t, setActivityLifecycleForTest(
			ctx, rdb, senderID, CategoryServerVoice, state, true,
		))
	}

	t.Run("definitively stale generation is deleted", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		put()
		builder := &activitySnapshotBuilderStub{err: ErrActivityNotCurrent}
		service := newActivitySnapshotService(nil, builder, store, func(
			context.Context, PolicyInput,
		) (Decision, error) {
			t.Fatal("policy must not run for stale state")
			return Decision{}, nil
		})
		service.candidateLoader = oneActivitySnapshotCandidate(senderID, channelID, lifecycleAt)

		snapshot, err := service.Snapshot(ctx, viewerID)
		require.NoError(t, err)
		assert.Empty(t, snapshot)
		_, found, err := store.Get(ctx, senderID, CategoryServerVoice)
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("newer successor is never deleted by stale candidates", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		newer := state
		newer.SourceToken = uuid.New()
		newer.SourceVersion += 2
		newer.UpdatedAt++
		require.NoError(t, setActivityLifecycleForTest(
			ctx, rdb, senderID, CategoryServerVoice, newer, true,
		))
		stored, err := store.CompareAndSet(ctx, senderID, CategoryServerVoice, newer)
		require.NoError(t, err)
		require.True(t, stored)
		service := newActivitySnapshotService(
			nil,
			&activitySnapshotBuilderStub{err: ErrActivityNotCurrent},
			store,
			func(context.Context, PolicyInput) (Decision, error) { return Decision{}, nil },
		)
		service.candidateLoader = oneActivitySnapshotCandidate(senderID, channelID, lifecycleAt)

		snapshot, err := service.Snapshot(ctx, viewerID)
		require.NoError(t, err)
		assert.Empty(t, snapshot)
		got, found, err := store.Get(ctx, senderID, CategoryServerVoice)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, newer, got)
	})

	t.Run("ambiguous active scopes are omitted and cleaned", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		put()
		service := newActivitySnapshotService(
			nil,
			&activitySnapshotBuilderStub{},
			store,
			func(context.Context, PolicyInput) (Decision, error) { return Decision{}, nil },
		)
		service.candidateLoader = func(context.Context, uuid.UUID) ([]activitySnapshotCandidate, error) {
			return []activitySnapshotCandidate{
				{
					activitySnapshotKey: activitySnapshotKey{SenderID: senderID, Category: CategoryServerVoice},
					RoomID:              channelID, LifecycleAt: lifecycleAt,
				},
				{
					activitySnapshotKey: activitySnapshotKey{SenderID: senderID, Category: CategoryServerVoice},
					RoomID:              uuid.New(), LifecycleAt: lifecycleAt.Add(time.Second),
				},
			}, nil
		}

		snapshot, err := service.Snapshot(ctx, viewerID)
		require.NoError(t, err)
		assert.Empty(t, snapshot)
		_, found, err := store.Get(ctx, senderID, CategoryServerVoice)
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("policy failure aborts instead of returning a partial snapshot", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		put()
		policyErr := errors.New("forced policy failure")
		service := newActivitySnapshotService(nil, &activitySnapshotBuilderStub{built: BuiltActivity{
			Input:       PolicyInput{SenderID: senderID, Category: CategoryServerVoice},
			SourceToken: channelID, SourceVersion: lifecycleAt.UnixMicro(),
		}}, store, func(context.Context, PolicyInput) (Decision, error) {
			return Decision{}, policyErr
		})
		service.candidateLoader = oneActivitySnapshotCandidate(senderID, channelID, lifecycleAt)

		snapshot, err := service.Snapshot(ctx, viewerID)
		assert.ErrorIs(t, err, policyErr)
		assert.Nil(t, snapshot)
	})
}

func oneActivitySnapshotCandidate(
	senderID uuid.UUID,
	roomID uuid.UUID,
	lifecycleAt time.Time,
) func(context.Context, uuid.UUID) ([]activitySnapshotCandidate, error) {
	return func(context.Context, uuid.UUID) ([]activitySnapshotCandidate, error) {
		return []activitySnapshotCandidate{{
			activitySnapshotKey: activitySnapshotKey{SenderID: senderID, Category: CategoryServerVoice},
			RoomID:              roomID, LifecycleAt: lifecycleAt,
		}}, nil
	}
}

func finalizeActivitySnapshotForTest(
	ctx context.Context,
	service *ActivitySnapshotService,
	viewerID uuid.UUID,
	projected ActivitySnapshot,
) (ActivitySnapshot, error) {
	if service.coordinator == nil {
		service.coordinator = &activitySnapshotCoordinatorStub{}
	}
	var finalized ActivitySnapshot
	err := service.FinalizeSnapshot(ctx, viewerID, projected, func(snapshot ActivitySnapshot) error {
		finalized = snapshot
		return nil
	})
	return finalized, err
}

func setActivityLifecycleForTest(
	ctx context.Context,
	rdb *redis.Client,
	senderID uuid.UUID,
	category Category,
	state ActivityState,
	active bool,
) error {
	key, err := VoiceLifecycleKey(senderID, category)
	if err != nil {
		return err
	}
	activeValue := "0"
	if active {
		activeValue = "1"
	}
	if err := rdb.HSet(ctx, key,
		"token", state.SourceToken.String(),
		"version", state.SourceVersion,
		"active", activeValue,
	).Err(); err != nil {
		return err
	}
	return rdb.PExpire(ctx, key, ActivityStateTTL).Err()
}
