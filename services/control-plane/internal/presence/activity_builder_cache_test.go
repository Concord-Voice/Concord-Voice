package presence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const activityBuilderCacheDriverName = "presence-activity-builder-cache-test"

var (
	activityBuilderCacheDriverOnce sync.Once
	activityBuilderCacheScenarios  sync.Map
)

type activityBuilderCacheParticipant struct {
	userID      uuid.UUID
	lifecycleAt time.Time
	isMember    bool
	ambiguous   bool
}

type activityBuilderCacheScenario struct {
	isGroup                    bool
	participants               []activityBuilderCacheParticipant
	participantsByConversation map[uuid.UUID][]activityBuilderCacheParticipant
	queries                    atomic.Int64
}

type activityBuilderCacheDriver struct{}

func (activityBuilderCacheDriver) Open(name string) (driver.Conn, error) {
	value, found := activityBuilderCacheScenarios.Load(name)
	if !found {
		return nil, fmt.Errorf("unknown activity builder cache scenario %q", name)
	}
	return &activityBuilderCacheConn{scenario: value.(*activityBuilderCacheScenario)}, nil
}

type activityBuilderCacheConn struct {
	scenario *activityBuilderCacheScenario
}

func (*activityBuilderCacheConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (*activityBuilderCacheConn) Close() error { return nil }

func (*activityBuilderCacheConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

func (c *activityBuilderCacheConn) QueryContext(
	_ context.Context,
	_ string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	c.scenario.queries.Add(1)
	participants := c.scenario.participants
	if len(c.scenario.participantsByConversation) > 0 && len(args) > 0 {
		conversationID, err := uuid.Parse(fmt.Sprint(args[0].Value))
		if err != nil {
			return nil, err
		}
		participants = c.scenario.participantsByConversation[conversationID]
	}
	limit := len(participants)
	if len(args) >= 1 {
		if requested, ok := args[len(args)-1].Value.(int64); ok && requested < int64(limit) {
			limit = int(requested)
		}
	}
	return &activityBuilderCacheRows{
		isGroup:      c.scenario.isGroup,
		participants: participants[:limit],
	}, nil
}

type activityBuilderCacheRows struct {
	isGroup      bool
	participants []activityBuilderCacheParticipant
	index        int
}

func (*activityBuilderCacheRows) Columns() []string {
	return []string{
		"is_group", "user_id", "lifecycle_event_at", "is_member", "ambiguous",
	}
}

func (*activityBuilderCacheRows) Close() error { return nil }

func (r *activityBuilderCacheRows) Next(values []driver.Value) error {
	if r.index >= len(r.participants) {
		return io.EOF
	}
	participant := r.participants[r.index]
	r.index++
	values[0] = r.isGroup
	values[1] = participant.userID.String()
	values[2] = participant.lifecycleAt
	values[3] = participant.isMember
	values[4] = participant.ambiguous
	return nil
}

type activityBuilderCacheLeaseVerifier struct {
	calls atomic.Int64
}

func (v *activityBuilderCacheLeaseVerifier) Matches(
	context.Context,
	uuid.UUID,
	uuid.UUID,
) (bool, error) {
	v.calls.Add(1)
	return true, nil
}

type activityBuilderCacheGenerationVerifier struct {
	mu    sync.Mutex
	calls [][]ActivityGeneration
}

func (v *activityBuilderCacheGenerationVerifier) VerifyActiveGenerations(
	_ context.Context,
	generations []ActivityGeneration,
) ([]bool, error) {
	v.mu.Lock()
	v.calls = append(v.calls, append([]ActivityGeneration(nil), generations...))
	v.mu.Unlock()
	active := make([]bool, len(generations))
	for index := range active {
		active[index] = true
	}
	return active, nil
}

func TestActivityBuilder_PrivateCallRequestCacheBoundsNestedWork(t *testing.T) {
	registerActivityBuilderCacheDriver()

	t.Run("255 senders in one exact call share one row read and lifecycle verification", func(t *testing.T) {
		participants := activityBuilderCacheParticipants(maxPrivateCallParticipants)
		scenario, db := openActivityBuilderCacheScenario(t, participants)
		leases := &activityBuilderCacheLeaseVerifier{}
		lifecycles := &activityBuilderCacheGenerationVerifier{}
		builder := NewActivityBuilder(db, leases, lifecycles)
		conversationID := uuid.New()
		callID := uuid.New()
		ctx := WithActivityBuildCache(context.Background())

		for _, participant := range participants {
			built, err := builder.Build(ctx, participant.userID, Scope{
				Category: CategoryPrivateCall, RoomID: conversationID,
				LifecycleID: callID, EventAt: participant.lifecycleAt,
			})
			require.NoError(t, err)
			assert.Equal(t, participant.userID, built.Input.SenderID)
			assert.Equal(t, participant.lifecycleAt.UnixMicro(), built.SourceVersion)
			state, stateErr := loadPrivateCallState(
				ctx, db, participant.userID, *built.Input.PrivateCall,
			)
			require.NoError(t, stateErr)
			assert.Len(t, state.participants, maxPrivateCallParticipants)
		}

		assert.Equal(t, int64(1), scenario.queries.Load())
		assert.Equal(t, int64(2), leases.calls.Load())
		require.Len(t, lifecycles.calls, 1)
		assert.Len(t, lifecycles.calls[0], maxPrivateCallParticipants)
	})

	t.Run("raw participant budget fails before a third large verification", func(t *testing.T) {
		participants := activityBuilderCacheParticipants(maxPrivateCallParticipants)
		scenario, db := openActivityBuilderCacheScenario(t, participants)
		leases := &activityBuilderCacheLeaseVerifier{}
		lifecycles := &activityBuilderCacheGenerationVerifier{}
		builder := NewActivityBuilder(db, leases, lifecycles)
		ctx := WithActivityBuildCache(context.Background())

		for callIndex := 0; callIndex < 2; callIndex++ {
			_, err := builder.Build(ctx, participants[0].userID, Scope{
				Category: CategoryPrivateCall, RoomID: uuid.New(),
				LifecycleID: uuid.New(), EventAt: participants[0].lifecycleAt,
			})
			require.NoError(t, err)
		}
		_, err := builder.Build(ctx, participants[0].userID, Scope{
			Category: CategoryPrivateCall, RoomID: uuid.New(),
			LifecycleID: uuid.New(), EventAt: participants[0].lifecycleAt,
		})

		assert.ErrorIs(t, err, ErrActivityBuildWorkLimit)
		assert.Equal(t, int64(3), scenario.queries.Load())
		require.Len(t, lifecycles.calls, 2)
	})

	t.Run("fresh call budget fails before query and survives invalidation", func(t *testing.T) {
		participants := activityBuilderCacheParticipants(1)
		scenario, db := openActivityBuilderCacheScenario(t, participants)
		builder := NewActivityBuilder(
			db,
			&activityBuilderCacheLeaseVerifier{},
			&activityBuilderCacheGenerationVerifier{},
		)
		ctx := WithActivityBuildCache(context.Background())

		for callIndex := 0; callIndex < activityBuildPrivateCallLimit; callIndex++ {
			_, err := builder.Build(ctx, participants[0].userID, Scope{
				Category: CategoryPrivateCall, RoomID: uuid.New(),
				LifecycleID: uuid.New(), EventAt: participants[0].lifecycleAt,
			})
			require.NoError(t, err)
			InvalidateActivityBuildCache(ctx)
		}
		_, err := builder.Build(ctx, participants[0].userID, Scope{
			Category: CategoryPrivateCall, RoomID: uuid.New(),
			LifecycleID: uuid.New(), EventAt: participants[0].lifecycleAt,
		})

		assert.ErrorIs(t, err, ErrActivityBuildWorkLimit)
		assert.Equal(t, int64(activityBuildPrivateCallLimit), scenario.queries.Load())
	})
}

func TestActivityService_PrivateEventCacheInvalidatesMutationOnceAndReusesFanout(t *testing.T) {
	participants := activityBuilderCacheParticipants(4)
	scenario, db := openActivityBuilderCacheScenario(t, participants)
	builder := NewActivityBuilder(
		db,
		&activityBuilderCacheLeaseVerifier{},
		&activityBuilderCacheGenerationVerifier{},
	)
	store := &activityServiceStoreStub{setResult: true, deleteResult: true}
	delivery := &activityServiceDeliveryStub{}
	participantCounts := make([]int, 0, 3)
	service := newActivityService(
		&activityServiceCoordinatorStub{},
		builder,
		store,
		func(_ context.Context, input PolicyInput) (Decision, error) {
			require.NotNil(t, input.PrivateCall)
			participantCounts = append(
				participantCounts, input.PrivateCall.Payload.ParticipantCount,
			)
			return Decision{
				Audience: map[uuid.UUID]bool{uuid.New(): true},
				Payload:  json.RawMessage(`{"call_type":"group"}`),
			}, nil
		},
		delivery,
	)
	conversationID := uuid.New()
	callID := uuid.New()
	scope := Scope{
		Category: CategoryPrivateCall, RoomID: conversationID,
		LifecycleID: callID, EventAt: participants[0].lifecycleAt,
	}
	ctx := WithActivityBuildCache(context.Background())

	// Seed the event epoch, then apply one authoritative mutation. The mutation
	// must discard the four-row snapshot before its fresh Build; the following
	// nil-mutation participant fan-out must reuse the resulting three-row read.
	require.NoError(t, service.RefreshPrivateCall(
		ctx, participants[0].userID, scope, nil, nil,
	))
	require.NoError(t, service.RefreshPrivateCall(
		ctx,
		participants[0].userID,
		scope,
		nil,
		func(context.Context) (bool, error) {
			scenario.participants = scenario.participants[:3]
			return true, nil
		},
	))
	require.NoError(t, service.RefreshPrivateCall(
		ctx, participants[1].userID, scope, nil, nil,
	))

	assert.Equal(t, []int{4, 3, 3}, participantCounts)
	assert.Equal(t, int64(2), scenario.queries.Load(),
		"one pre-mutation read and one post-mutation read must serve the event fan-out")
	require.Len(t, delivery.plans, 3)
}

func TestFreshActivityBuildCacheDoesNotInheritEventEpoch(t *testing.T) {
	eventCtx := WithActivityBuildCache(context.Background())
	eventCache := activityBuildCacheFromContext(eventCtx)
	require.NotNil(t, eventCache)
	key := privateCallBuildCacheKey{conversationID: uuid.New(), callID: uuid.New()}
	snapshot := &privateCallBuildSnapshot{key: key}
	eventCache.privateCalls[key] = privateCallBuildCacheResult{snapshot: snapshot}
	require.True(t, activityBuildCacheOwnsPrivateSnapshot(eventCtx, snapshot))

	freshCtx := withFreshActivityBuildCache(eventCtx)
	freshCache := activityBuildCacheFromContext(freshCtx)
	require.NotNil(t, freshCache)
	assert.NotSame(t, eventCache, freshCache)
	assert.False(t, activityBuildCacheOwnsPrivateSnapshot(freshCtx, snapshot),
		"trusted policy-state reuse must be bound to the exact request/event cache")
	InvalidateActivityBuildCache(eventCtx)
	assert.False(t, activityBuildCacheOwnsPrivateSnapshot(eventCtx, snapshot))
}

func TestActivitySnapshotBuildLimitFailsGloballyAtPublicationBoundary(t *testing.T) {
	t.Run("initial snapshot returns no partial projection", func(t *testing.T) {
		service, viewerID, projected, scenario := newActivitySnapshotBuildLimitFixture(t)

		snapshot, err := service.Snapshot(context.Background(), viewerID)

		assert.ErrorIs(t, err, ErrActivityBuildWorkLimit)
		assert.Nil(t, snapshot)
		assert.Len(t, projected, activityBuildPrivateCallLimit+1,
			"fixture must exceed the exact-call work budget")
		assert.Equal(t, int64(activityBuildPrivateCallLimit), scenario.queries.Load(),
			"the over-budget call must fail before another authoritative query")
	})

	t.Run("finalizer never invokes publisher with a partial projection", func(t *testing.T) {
		service, viewerID, projected, scenario := newActivitySnapshotBuildLimitFixture(t)
		published := false

		err := service.FinalizeSnapshot(
			context.Background(),
			viewerID,
			projected,
			func(ActivitySnapshot) error {
				published = true
				return nil
			},
		)

		assert.ErrorIs(t, err, ErrActivityBuildWorkLimit)
		assert.False(t, published)
		assert.Equal(t, int64(activityBuildPrivateCallLimit), scenario.queries.Load(),
			"publication preparation must stop before an over-budget query")
	})
}

func TestActivitySnapshotLargePrivateCallReadsParticipantsOncePerFreshPhase(t *testing.T) {
	registerActivityBuilderCacheDriver()
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	conversationID := uuid.New()
	callID := uuid.New()
	participants := activityBuilderCacheParticipants(maxPrivateCallParticipants)
	scenario, db := openActivityBuilderCacheScenario(t, participants)
	leases := &activityBuilderCacheLeaseVerifier{}
	builder := NewActivityBuilder(db, leases, store)
	candidates := make([]activitySnapshotCandidate, 0, len(participants))
	baseTime := time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)
	for index, participant := range participants {
		// Keep the driver and Redis lifecycle versions identical for every sender.
		participant.lifecycleAt = baseTime.Add(time.Duration(index) * time.Microsecond)
		participants[index] = participant
		state := ActivityState{
			SourceToken: callID, SourceVersion: participant.lifecycleAt.UnixMicro(),
			Payload:   json.RawMessage(`{"call_type":"group"}`),
			UpdatedAt: participant.lifecycleAt.Unix(),
		}
		stored, err := store.CompareAndSet(
			ctx, participant.userID, CategoryPrivateCall, state,
		)
		require.NoError(t, err)
		require.True(t, stored)
		require.NoError(t, setActivityLifecycleForTest(
			ctx, rdb, participant.userID, CategoryPrivateCall, state, true,
		))
		candidates = append(candidates, activitySnapshotCandidate{
			activitySnapshotKey: activitySnapshotKey{
				SenderID: participant.userID, Category: CategoryPrivateCall,
			},
			RoomID: conversationID, LifecycleAt: participant.lifecycleAt,
		})
	}
	scenario.isGroup = true
	coordinator := &activitySnapshotCoordinatorStub{}
	service := newActivitySnapshotService(
		nil,
		builder,
		store,
		func(context.Context, PolicyInput) (Decision, error) {
			return Decision{
				Audience: map[uuid.UUID]bool{viewerID: true},
				Payload:  json.RawMessage(`{"call_type":"group"}`),
			}, nil
		},
		coordinator,
	)
	service.candidateLoader = func(context.Context, uuid.UUID) ([]activitySnapshotCandidate, error) {
		return candidates, nil
	}

	projected, err := service.Snapshot(ctx, viewerID)
	require.NoError(t, err)
	require.Len(t, projected, maxPrivateCallParticipants)
	assert.Equal(t, int64(1), scenario.queries.Load(),
		"projection must read one shared call instead of once per sender")
	assert.Equal(t, int64(2), leases.calls.Load())

	var published ActivitySnapshot
	err = service.FinalizeSnapshot(ctx, viewerID, projected, func(snapshot ActivitySnapshot) error {
		published = snapshot
		return nil
	})
	require.NoError(t, err)
	require.Len(t, published, maxPrivateCallParticipants)
	assert.Equal(t, int64(2), scenario.queries.Load(),
		"finalization must use one new authoritative call read, not the projection cache")
	assert.Equal(t, int64(4), leases.calls.Load())
	assert.Len(t, coordinator.multiIDs, maxPrivateCallParticipants,
		"participant sender gates must be deduplicated before coordination")
}

func newActivitySnapshotBuildLimitFixture(t *testing.T) (
	*ActivitySnapshotService,
	uuid.UUID,
	ActivitySnapshot,
	*activityBuilderCacheScenario,
) {
	t.Helper()
	registerActivityBuilderCacheDriver()
	rdb := setupActivityStoreRedis(t)
	store := NewActivityStore(rdb)
	ctx := context.Background()
	viewerID := uuid.New()
	participantsByConversation := make(map[uuid.UUID][]activityBuilderCacheParticipant)
	candidates := make([]activitySnapshotCandidate, 0, activityBuildPrivateCallLimit+1)
	projected := make(ActivitySnapshot, activityBuildPrivateCallLimit+1)
	baseTime := time.Date(2026, 7, 15, 16, 0, 0, 0, time.UTC)

	for index := 0; index < activityBuildPrivateCallLimit+1; index++ {
		senderID := uuid.New()
		conversationID := uuid.New()
		callID := uuid.New()
		lifecycleAt := baseTime.Add(time.Duration(index) * time.Microsecond)
		participantsByConversation[conversationID] = []activityBuilderCacheParticipant{{
			userID: senderID, lifecycleAt: lifecycleAt, isMember: true,
		}}
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
		candidates = append(candidates, activitySnapshotCandidate{
			activitySnapshotKey: activitySnapshotKey{
				SenderID: senderID, Category: CategoryPrivateCall,
			},
			RoomID: conversationID, LifecycleAt: lifecycleAt,
		})
		projected[senderID] = map[Category]ActivitySnapshotEntry{
			CategoryPrivateCall: {
				Payload: json.RawMessage(`{"call_type":"dm"}`),
				projection: activitySnapshotProjection{
					SourceToken: callID, SourceVersion: lifecycleAt.UnixMicro(),
					Scope: Scope{
						Category: CategoryPrivateCall, RoomID: conversationID,
						LifecycleID: callID, EventAt: lifecycleAt,
					},
					ParticipantIDs: []uuid.UUID{senderID},
				},
			},
		}
	}

	scenario := &activityBuilderCacheScenario{
		participantsByConversation: participantsByConversation,
	}
	db := openActivityBuilderCacheScenarioValue(t, scenario)
	builder := NewActivityBuilder(
		db,
		&activityBuilderCacheLeaseVerifier{},
		store,
	)
	service := newActivitySnapshotService(
		nil,
		builder,
		store,
		func(context.Context, PolicyInput) (Decision, error) {
			return Decision{
				Audience: map[uuid.UUID]bool{viewerID: true},
				Payload:  json.RawMessage(`{"call_type":"dm"}`),
			}, nil
		},
		&activitySnapshotCoordinatorStub{},
	)
	service.candidateLoader = func(context.Context, uuid.UUID) ([]activitySnapshotCandidate, error) {
		return candidates, nil
	}
	return service, viewerID, projected, scenario
}

func activityBuilderCacheParticipants(count int) []activityBuilderCacheParticipant {
	participants := make([]activityBuilderCacheParticipant, count)
	baseTime := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	for index := range participants {
		participants[index] = activityBuilderCacheParticipant{
			userID:      uuid.NewSHA1(uuid.Nil, []byte(strconv.Itoa(index))),
			lifecycleAt: baseTime.Add(time.Duration(index) * time.Microsecond),
			isMember:    true,
		}
	}
	return participants
}

func openActivityBuilderCacheScenario(
	t *testing.T,
	participants []activityBuilderCacheParticipant,
) (*activityBuilderCacheScenario, *sql.DB) {
	t.Helper()
	registerActivityBuilderCacheDriver()
	scenario := &activityBuilderCacheScenario{participants: participants}
	return scenario, openActivityBuilderCacheScenarioValue(t, scenario)
}

func openActivityBuilderCacheScenarioValue(
	t *testing.T,
	scenario *activityBuilderCacheScenario,
) *sql.DB {
	t.Helper()
	registerActivityBuilderCacheDriver()
	name := uuid.NewString()
	activityBuilderCacheScenarios.Store(name, scenario)
	t.Cleanup(func() { activityBuilderCacheScenarios.Delete(name) })
	db, err := sql.Open(activityBuilderCacheDriverName, name)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func registerActivityBuilderCacheDriver() {
	activityBuilderCacheDriverOnce.Do(func() {
		sql.Register(activityBuilderCacheDriverName, activityBuilderCacheDriver{})
	})
}
