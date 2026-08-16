package presence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recheckDBStub is a DBTX whose reads always fail. It exercises the error
// classes of the two capture helpers without a database; their happy paths are
// covered by the integration suite, which needs real *sql.Rows.
type recheckDBStub struct {
	queryCalls int
	err        error
}

func (d *recheckDBStub) QueryContext(
	_ context.Context, _ string, _ ...any,
) (*sql.Rows, error) {
	d.queryCalls++
	return nil, d.err
}

func (d *recheckDBStub) QueryRowContext(_ context.Context, _ string, _ ...any) *sql.Row {
	return nil
}

// recheckPresenceStub reports a fixed base-presence verdict.
type recheckPresenceStub struct{ permitted bool }

func (p *recheckPresenceStub) RichPresenceEmissionPermitted(
	context.Context, uuid.UUID,
) bool {
	return p.permitted
}

func (p *recheckPresenceStub) RichPresenceEmissionState(
	ctx context.Context, senderID uuid.UUID,
) (bool, error) {
	// Test double: always DETERMINED, so it exercises the
	// suppression path rather than the indeterminate one.
	return p.RichPresenceEmissionPermitted(ctx, senderID), nil
}

// undeterminedPresenceStub is the Redis-blip shape: it cannot answer at all.
type undeterminedPresenceStub struct{}

func (undeterminedPresenceStub) RichPresenceEmissionPermitted(
	context.Context, uuid.UUID,
) bool {
	return false
}

func (undeterminedPresenceStub) RichPresenceEmissionState(
	context.Context, uuid.UUID,
) (bool, error) {
	return false, errors.New("presence lookup unavailable")
}

// The two entry points must answer an UNDETERMINED base presence differently,
// and the lenient half is an RBAC-availability guarantee rather than a
// stylistic default.
//
// voicepresence.Executor.PrepareCapture calls the lenient form before an RBAC
// authority write, and rbac.withAuthorityCapture turns a PrepareCapture error
// into a 500 with the write blocked. Propagating a transient Redis fault there
// would put Redis availability in front of CreateRole / UpdateRole /
// AssignRole / RemoveMember / ban — which is exactly what the first version of
// this change did, before review caught it (PR #2770, Gitar).
func TestCaptureServerVoiceCandidates_UndeterminedPresence_LenientVsStrict(t *testing.T) {
	sender, server := uuid.New(), uuid.New()

	t.Run("lenient absorbs it so an RBAC write is never blocked by a blip", func(t *testing.T) {
		db := &recheckDBStub{}

		candidates, err := CaptureServerVoiceCandidates(
			context.Background(), db, undeterminedPresenceStub{}, sender, server,
		)

		require.NoError(t, err,
			"an error here reaches rbac.withAuthorityCapture and 500s a privileged write")
		assert.Empty(t, candidates)
		assert.Zero(t, db.queryCalls,
			"and it must short-circuit before the settings read, as the suppressed path does")
	})

	t.Run("strict refuses so the capture's posture actually runs", func(t *testing.T) {
		db := &recheckDBStub{}

		candidates, err := CaptureServerVoiceCandidatesStrict(
			context.Background(), db, undeterminedPresenceStub{}, sender, server,
		)

		require.Error(t, err,
			"an empty set here is indistinguishable from a suppression, so the leg is "+
				"dropped and the caller's FailPosture never applies")
		assert.Nil(t, candidates)
	})

	t.Run("a DETERMINED suppression is still empty on both", func(t *testing.T) {
		for name, capture := range map[string]func() (map[uuid.UUID]bool, error){
			"lenient": func() (map[uuid.UUID]bool, error) {
				return CaptureServerVoiceCandidates(context.Background(),
					&recheckDBStub{}, &recheckPresenceStub{permitted: false}, sender, server)
			},
			"strict": func() (map[uuid.UUID]bool, error) {
				return CaptureServerVoiceCandidatesStrict(context.Background(),
					&recheckDBStub{}, &recheckPresenceStub{permitted: false}, sender, server)
			},
		} {
			candidates, err := capture()
			require.NoError(t, err, "%s: a determined suppression is not a failure", name)
			assert.Empty(t, candidates, "%s", name)
		}
	})
}

func TestRefreshServerVoiceRecheck_LostViewer_ProducesExactClear(t *testing.T) {
	service, _, store, delivery, coordinator := newActivityServiceFixture(CategoryServerVoice)
	lostViewer := uuid.New()

	err := service.RefreshServerVoiceRecheck(
		context.Background(),
		activityServiceSender,
		serverActivityScope(),
		map[uuid.UUID]bool{lostViewer: true, activityServiceViewer: true},
	)

	require.NoError(t, err)
	require.Equal(t, 1, coordinator.calls, "must enter the sender gate exactly once")
	require.Len(t, store.sets, 1)
	require.Len(t, delivery.plans, 1)
	plan := delivery.plans[0]
	assert.Equal(t, map[uuid.UUID]bool{lostViewer: true}, plan.ClearRecipients,
		"only the viewer absent from the fresh audience is cleared")
	assert.Equal(t, map[uuid.UUID]bool{activityServiceViewer: true}, plan.UpdateRecipients)
	assert.Zero(t, delivery.disconnectAllCalls)
}

func TestRefreshServerVoiceRecheck_RetainedViewersOnly_ProducesNoClear(t *testing.T) {
	service, _, _, delivery, _ := newActivityServiceFixture(CategoryServerVoice)

	err := service.RefreshServerVoiceRecheck(
		context.Background(),
		activityServiceSender,
		serverActivityScope(),
		map[uuid.UUID]bool{activityServiceViewer: true},
	)

	require.NoError(t, err)
	require.Len(t, delivery.plans, 1)
	assert.Empty(t, delivery.plans[0].ClearRecipients)
}

func TestRefreshServerVoiceRecheck_SenderNotCurrent_IsBenignAndDoesNotDisconnectAll(t *testing.T) {
	service, builder, _, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	builder.err = fmt.Errorf("%w: server voice scope", ErrActivityNotCurrent)

	err := service.RefreshServerVoiceRecheck(
		context.Background(),
		activityServiceSender,
		serverActivityScope(),
		map[uuid.UUID]bool{uuid.New(): true},
	)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRecheckSenderNotCurrent),
		"the recheck entry point classifies a stale sender as benign")
	assert.Zero(t, delivery.disconnectAllCalls,
		"F3 carve-out: an RBAC sweep racing a voice leave must never disconnect the replica")
}

func TestRefreshServerVoice_SenderNotCurrent_KeepsGlobalDisconnectTerminal(t *testing.T) {
	service, builder, _, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	builder.err = fmt.Errorf("%w: server voice scope", ErrActivityNotCurrent)

	err := service.RefreshServerVoice(
		context.Background(), activityServiceSender, serverActivityScope(), nil,
	)

	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrRecheckSenderNotCurrent),
		"the flag is set ONLY by the recheck entry point")
	assert.Equal(t, 1, delivery.disconnectAllCalls,
		"every pre-existing entry point keeps its terminal byte-for-byte")
}

func TestRefreshServerVoiceRecheck_WrongCategory_IsRejected(t *testing.T) {
	service, _, _, _, _ := newActivityServiceFixture(CategoryServerVoice)
	scope := privateActivityScope()

	err := service.RefreshServerVoiceRecheck(
		context.Background(), activityServiceSender, scope, nil,
	)

	require.ErrorIs(t, err, ErrInvalidActivityScope)
}

func TestRefreshServerVoiceRecheck_BuildFailure_KeepsGlobalDisconnect(t *testing.T) {
	service, builder, _, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	builder.err = errors.New("database unavailable")

	err := service.RefreshServerVoiceRecheck(
		context.Background(),
		activityServiceSender,
		serverActivityScope(),
		map[uuid.UUID]bool{uuid.New(): true},
	)

	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrRecheckSenderNotCurrent))
	assert.Equal(t, 1, delivery.disconnectAllCalls,
		"only ErrActivityNotCurrent is carved out; genuine uncertainty still disconnects")
}

func TestCurrentServerVoiceScope_InvalidInputs_AreRejectedBeforeAnyQuery(t *testing.T) {
	t.Run("nil database", func(t *testing.T) {
		_, found, err := CurrentServerVoiceScope(context.Background(), nil, uuid.New())

		require.Error(t, err)
		assert.False(t, found)
	})

	t.Run("nil sender", func(t *testing.T) {
		db := &recheckDBStub{}

		_, found, err := CurrentServerVoiceScope(context.Background(), db, uuid.Nil)

		require.ErrorIs(t, err, ErrInvalidActivityScope)
		assert.False(t, found)
		assert.Zero(t, db.queryCalls, "an invalid sender never reaches the database")
	})
}

func TestCurrentServerVoiceScope_QueryFailure_IsWrappedAndNotFound(t *testing.T) {
	db := &recheckDBStub{err: errors.New("connection reset")}

	_, found, err := CurrentServerVoiceScope(context.Background(), db, uuid.New())

	require.Error(t, err)
	assert.False(t, found, "a read failure is never reported as a resolved scope")
	assert.Contains(t, err.Error(), "connection reset")
	assert.Equal(t, 1, db.queryCalls)
}

func TestCaptureServerVoiceCandidates_InvalidInputs_AreRejected(t *testing.T) {
	t.Run("nil database", func(t *testing.T) {
		_, err := CaptureServerVoiceCandidates(
			context.Background(), nil, &recheckPresenceStub{permitted: true},
			uuid.New(), uuid.New(),
		)

		require.Error(t, err)
	})

	t.Run("nil sender", func(t *testing.T) {
		_, err := CaptureServerVoiceCandidates(
			context.Background(), &recheckDBStub{}, &recheckPresenceStub{permitted: true},
			uuid.Nil, uuid.New(),
		)

		require.ErrorIs(t, err, ErrInvalidActivityScope)
	})

	t.Run("nil server", func(t *testing.T) {
		_, err := CaptureServerVoiceCandidates(
			context.Background(), &recheckDBStub{}, &recheckPresenceStub{permitted: true},
			uuid.New(), uuid.Nil,
		)

		require.ErrorIs(t, err, ErrInvalidActivityScope)
	})
}

func TestCaptureServerVoiceCandidates_BasePresenceOff_IsEmptyNotAnError(t *testing.T) {
	t.Run("nil resolver", func(t *testing.T) {
		db := &recheckDBStub{}

		candidates, err := CaptureServerVoiceCandidates(
			context.Background(), db, nil, uuid.New(), uuid.New(),
		)

		require.NoError(t, err)
		assert.Empty(t, candidates)
		assert.Zero(t, db.queryCalls,
			"a hidden sender is resolved without touching the settings read")
	})

	t.Run("fail-closed resolver", func(t *testing.T) {
		db := &recheckDBStub{}

		candidates, err := CaptureServerVoiceCandidates(
			context.Background(), db, &recheckPresenceStub{permitted: false},
			uuid.New(), uuid.New(),
		)

		require.NoError(t, err)
		assert.Empty(t, candidates,
			"a Redis blip yields no candidates, so it can never produce a spurious clear")
		assert.Zero(t, db.queryCalls)
	})
}
