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

type absentClearMarkerStub struct {
	marked    map[string]bool
	readErr   error
	markErr   error
	forgetErr error
	forgets   int
	reads     int
}

func absentMarkerKey(userID uuid.UUID, category Category, token uuid.UUID) string {
	return userID.String() + "|" + string(category) + "|" + absentClearValue(token)
}

func (m *absentClearMarkerStub) AbsentClearDelivered(
	_ context.Context, userID uuid.UUID, category Category, token uuid.UUID,
) (bool, error) {
	m.reads++
	if m.readErr != nil {
		return false, m.readErr
	}
	return m.marked[absentMarkerKey(userID, category, token)], nil
}

func (m *absentClearMarkerStub) MarkAbsentClearDelivered(
	_ context.Context, userID uuid.UUID, category Category, token uuid.UUID,
) error {
	if m.markErr != nil {
		return m.markErr
	}
	if m.marked == nil {
		m.marked = map[string]bool{}
	}
	m.marked[absentMarkerKey(userID, category, token)] = true
	return nil
}

func (m *absentClearMarkerStub) ForgetAbsentClear(context.Context, uuid.UUID, Category) error {
	m.forgets++
	if m.forgetErr != nil {
		return m.forgetErr
	}
	m.marked = nil
	return nil
}

func newAbsentClearFixture(t *testing.T) (*ActivityService, *activityServiceStoreStub, *activityServiceDeliveryStub, *absentClearMarkerStub, *int) {
	t.Helper()
	service, _, store, delivery, marker, resolves := newAbsentClearFixtureWithBuilder(t)
	return service, store, delivery, marker, resolves
}

func newAbsentClearFixtureWithBuilder(t *testing.T) (*ActivityService, *activityServiceBuilderStub, *activityServiceStoreStub, *activityServiceDeliveryStub, *absentClearMarkerStub, *int) {
	t.Helper()
	service, builder, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		return Decision{Audience: map[uuid.UUID]bool{}}, nil
	}
	resolves := 0
	service.absentRecipients = func(context.Context, uuid.UUID, PolicyInput) (map[uuid.UUID]bool, error) {
		resolves++
		return map[uuid.UUID]bool{activityServiceViewer: true}, nil
	}
	marker := &absentClearMarkerStub{}
	service.absentClears = marker
	store.deleteResult = false // nothing stored: the steady state
	return service, builder, store, delivery, marker, &resolves
}

func TestActivityService_AbsentClearOncePerSession(t *testing.T) {
	service, _, delivery, _, resolves := newAbsentClearFixture(t)

	for i := 0; i < 10; i++ {
		require.NoError(t, service.RefreshServerVoice(
			context.Background(), activityServiceSender, serverActivityScope(), nil,
		))
	}

	assert.Zero(t, delivery.disconnectAllCalls)
	assert.Equal(t, 1, *resolves, "later heartbeats in the same session skip the audience query")
	require.Len(t, delivery.plans, 1, "one clear per voice session")
	assert.Equal(t, map[uuid.UUID]bool{activityServiceViewer: true}, delivery.plans[0].ClearRecipients)
}

// Every heartbeat advances the source version, so the marker must not key on
// it, while a move to another channel (a new lifecycle token) owes a new clear.
func TestActivityService_AbsentClearIgnoresVersionButRepeatsForNewChannel(t *testing.T) {
	service, builder, _, delivery, _, _ := newAbsentClearFixtureWithBuilder(t)

	require.NoError(t, service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil))
	builder.built.SourceVersion++
	require.NoError(t, service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil))
	assert.Len(t, delivery.plans, 1, "a heartbeat's newer version is the same session")

	builder.built.SourceToken = uuid.New()
	require.NoError(t, service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil))

	assert.Zero(t, delivery.disconnectAllCalls)
	assert.Len(t, delivery.plans, 2, "a new channel owes its own clear")
}

// captureAbsentMarkerWarnings records each failure_class the service logs.
func captureAbsentMarkerWarnings(service *ActivityService) *[]string {
	classes := []string{}
	service.SetAbsentMarkerFailureReporter(func(failureClass string) {
		classes = append(classes, failureClass)
	})
	return &classes
}

func TestActivityService_AbsentClearUnreadableMarkerClearsAgain(t *testing.T) {
	service, _, delivery, marker, _ := newAbsentClearFixture(t)
	marker.readErr = errors.New("forced marker read failure")
	warnings := captureAbsentMarkerWarnings(service)

	for i := 0; i < 3; i++ {
		require.NoError(t, service.RefreshServerVoice(
			context.Background(), activityServiceSender, serverActivityScope(), nil,
		))
	}

	assert.Zero(t, delivery.disconnectAllCalls)
	assert.Len(t, delivery.plans, 3, "an unreadable marker must never skip a clear")
	assert.Equal(t, []string{"absent_marker_read", "absent_marker_read", "absent_marker_read"}, *warnings,
		"each unreadable marker is logged, not swallowed")
}

// A failed resolve after an unreadable marker reports both causes: the marker
// failure is the first symptom of the same outage.
func TestActivityService_AbsentClearUnreadableMarkerJoinsResolveFailure(t *testing.T) {
	service, _, delivery, marker, _ := newAbsentClearFixture(t)
	readErr := errors.New("forced marker read failure")
	resolveErr := errors.New("forced resolve failure")
	marker.readErr = readErr
	service.absentRecipients = func(context.Context, uuid.UUID, PolicyInput) (map[uuid.UUID]bool, error) {
		return nil, resolveErr
	}

	err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

	require.ErrorIs(t, err, readErr)
	require.ErrorIs(t, err, resolveErr)
	assert.Equal(t, 1, delivery.disconnectAllCalls, "an unnameable audience still fails closed")
}

func TestActivityService_AbsentClearLostMarkerWriteClearsAgain(t *testing.T) {
	service, _, delivery, marker, _ := newAbsentClearFixture(t)
	marker.markErr = errors.New("forced marker write failure")
	warnings := captureAbsentMarkerWarnings(service)

	for i := 0; i < 2; i++ {
		require.NoError(t, service.RefreshServerVoice(
			context.Background(), activityServiceSender, serverActivityScope(), nil,
		))
	}

	assert.Zero(t, delivery.disconnectAllCalls)
	assert.Len(t, delivery.plans, 2)
	assert.Equal(t, []string{"absent_marker_write", "absent_marker_write"}, *warnings,
		"a lost marker write is logged, so a repeated clear is visible")
}

func TestActivityService_AbsentClearFailedDeliveryLeavesNoMarker(t *testing.T) {
	service, _, delivery, marker, _ := newAbsentClearFixture(t)
	delivery.deliverErr = errors.New("forced clear delivery failure")

	err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

	require.Error(t, err)
	assert.Empty(t, marker.marked, "an undelivered clear must be retried next heartbeat")
	assert.Equal(t, []map[uuid.UUID]bool{{activityServiceViewer: true}}, delivery.disconnects,
		"a failed clear reconnects exactly its audience")
}

// Once published, a generation can vanish while viewers hold it, so publication
// must forget the marker and the next absent state must clear again.
func TestActivityService_PublicationForgetsAbsentClear(t *testing.T) {
	service, store, delivery, marker, _ := newAbsentClearFixture(t)
	publish := false
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		if publish {
			return Decision{
				Audience: map[uuid.UUID]bool{activityServiceViewer: true},
				Payload:  []byte(`{"server_id":"published"}`),
			}, nil
		}
		return Decision{Audience: map[uuid.UUID]bool{}}, nil
	}
	store.setResult = true

	require.NoError(t, service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil))
	publish = true
	require.NoError(t, service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil))
	publish = false
	require.NoError(t, service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil))

	assert.Equal(t, 1, marker.forgets)
	assert.Zero(t, delivery.disconnectAllCalls)
	clears := 0
	for _, plan := range delivery.plans {
		if len(plan.Payload) == 0 {
			clears++
		}
	}
	assert.Equal(t, 2, clears, "the absent state after a publication clears again")
}

func TestActivityService_PublicationFailsClosedWhenMarkerCannotBeForgotten(t *testing.T) {
	service, store, delivery, marker, _ := newAbsentClearFixture(t)
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		return Decision{
			Audience: map[uuid.UUID]bool{activityServiceViewer: true},
			Payload:  []byte(`{"server_id":"published"}`),
		}, nil
	}
	marker.forgetErr = errors.New("forced marker delete failure")

	err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

	require.ErrorIs(t, err, marker.forgetErr)
	assert.Empty(t, store.sets, "nothing is published while a stale marker could survive it")
	assert.Empty(t, delivery.plans)
	assert.Equal(t, 1, delivery.disconnectAllCalls)
}

// An RBAC recheck after a revoke, with the sender's row expired: the revoked
// viewer is no longer a channel viewer, so the fresh audience cannot name it,
// and the marker must not skip it.
func TestActivityService_RecheckAbsentClearsCapturedViewers(t *testing.T) {
	revoked := uuid.New()
	for _, markerSet := range []bool{false, true} {
		service, builder, _, delivery, marker, _ := newAbsentClearFixtureWithBuilder(t)
		if markerSet {
			marker.marked = map[string]bool{
				absentMarkerKey(activityServiceSender, CategoryServerVoice, builder.built.SourceToken): true,
			}
		}

		require.NoError(t, service.RefreshServerVoiceRecheck(
			context.Background(), activityServiceSender, serverActivityScope(),
			map[uuid.UUID]bool{revoked: true},
		))

		assert.Zero(t, delivery.disconnectAllCalls)
		require.Len(t, delivery.plans, 1, "marker set: %v", markerSet)
		assert.Equal(t, map[uuid.UUID]bool{activityServiceViewer: true, revoked: true},
			delivery.plans[0].ClearRecipients, "marker set: %v", markerSet)
	}
}

// An empty-audience sender never stores a generation, so each channel hop finds
// nothing to delete. It must clear the prior channel's possible viewers, once,
// instead of disconnecting the replica.
func TestActivityService_EmptyAudienceMoveClearsInsteadOfDisconnecting(t *testing.T) {
	service, builder, _, delivery, _, _ := newAbsentClearFixtureWithBuilder(t)
	builder.build = func(_ uuid.UUID, scope Scope) (BuiltActivity, error) {
		return BuiltActivity{
			Input: PolicyInput{
				SenderID: activityServiceSender, Category: CategoryServerVoice,
				ServerVoice: &ServerVoicePolicyInput{Context: ServerVoiceContext{ChannelID: scope.RoomID}},
			},
			SourceToken: scope.RoomID, SourceVersion: scope.EventAt.UnixMicro(),
		}, nil
	}
	oldScope := serverActivityScope()
	newScope := oldScope
	newScope.RoomID = uuid.New()
	newScope.LifecycleID = newScope.RoomID
	newScope.EventAt = oldScope.EventAt.Add(time.Second)

	for i := 0; i < 3; i++ {
		require.NoError(t, service.MoveServerVoice(
			context.Background(), activityServiceSender, oldScope, newScope, nil,
		))
	}

	assert.Zero(t, delivery.disconnectAllCalls, "a channel hop must not disconnect the replica")
	require.Len(t, delivery.plans, 1, "the prior channel is cleared once")
	assert.Equal(t, map[uuid.UUID]bool{activityServiceViewer: true}, delivery.plans[0].ClearRecipients)
}

func TestAbsentServerVoiceRecipients_RefusesWithoutAServerVoiceAudience(t *testing.T) {
	cases := []struct {
		name  string
		input PolicyInput
		class FailureClass
	}{
		{"private call", PolicyInput{Category: CategoryPrivateCall}, FailureAbsentAudienceUnsupported},
		{"server voice without context", PolicyInput{Category: CategoryServerVoice}, FailureInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A nil database fails the test if the guard ever lets a query run.
			_, err := absentServerVoiceRecipients(
				context.Background(), activitySettingsRecipientDependencies{}, nil, activityServiceSender, tc.input,
			)
			require.Error(t, err)
			assert.Equal(t, tc.class, PolicyErrorClass(err))
		})
	}
}
