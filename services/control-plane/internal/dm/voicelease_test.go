package dm_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/dm"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMVoiceCallLease_ExactOwnerRefreshAndDelete(t *testing.T) {
	redisClient, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	conversationID := uuid.New()
	callID := uuid.New()
	ringID := uuid.New()
	callerUserID := uuid.New()
	lease := dm.VoiceCallLease{
		ConversationID: conversationID,
		CallID:         callID,
		RingID:         ringID,
		CallerUserID:   callerUserID,
	}
	require.NoError(t, dm.RefreshDMVoiceCallLease(ctx, redisClient, lease, time.Minute, true))

	stored, ok, err := dm.LookupDMVoiceCallLease(ctx, redisClient, conversationID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, lease, stored)

	// A joined event carries the joining user as a creation fallback; it must
	// not replace an already-authoritative ring caller.
	joinFallback := lease
	joinFallback.RingID = uuid.Nil
	joinFallback.CallerUserID = uuid.New()
	require.NoError(t,
		dm.RefreshDMVoiceCallLease(ctx, redisClient, joinFallback, time.Minute, false))
	stored, ok, err = dm.LookupDMVoiceCallLease(ctx, redisClient, conversationID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, lease, stored)

	// Heartbeats carry the room's authoritative metadata and can repair a
	// lease recreated after Redis state loss.
	repaired := lease
	repaired.RingID = uuid.New()
	repaired.CallerUserID = uuid.New()
	require.NoError(t,
		dm.RefreshDMVoiceCallLease(ctx, redisClient, repaired, time.Minute, true))
	stored, ok, err = dm.LookupDMVoiceCallLease(ctx, redisClient, conversationID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, repaired, stored)

	conflict := lease
	conflict.CallID = uuid.New()
	require.ErrorIs(t,
		dm.RefreshDMVoiceCallLease(ctx, redisClient, conflict, time.Minute, true),
		dm.ErrDMVoiceCallLeaseConflict,
	)

	// A delayed terminal event for another ID cannot erase the owner.
	require.NoError(t, dm.DeleteDMVoiceCallLease(ctx, redisClient, conversationID, conflict.CallID))
	_, ok, err = dm.LookupDMVoiceCallLease(ctx, redisClient, conversationID)
	require.NoError(t, err)
	assert.True(t, ok)

	require.NoError(t, dm.DeleteDMVoiceCallLease(ctx, redisClient, conversationID, callID))
	_, ok, err = dm.LookupDMVoiceCallLease(ctx, redisClient, conversationID)
	require.NoError(t, err)
	assert.False(t, ok)

	require.ErrorIs(t,
		dm.RefreshDMVoiceCallLease(ctx, redisClient, repaired, time.Minute, true),
		dm.ErrDMVoiceCallLeaseClosed,
		"a delayed heartbeat must not resurrect a terminal call",
	)
	replacement := repaired
	replacement.CallID = uuid.New()
	require.NoError(t,
		dm.RefreshDMVoiceCallLease(ctx, redisClient, replacement, time.Minute, true),
		"a different call ID may own the conversation after terminal cleanup",
	)
}

func TestDMVoiceCallLease_ValidatesDependenciesAndIdentity(t *testing.T) {
	ctx := context.Background()
	lease := dm.VoiceCallLease{
		ConversationID: uuid.New(),
		CallID:         uuid.New(),
		CallerUserID:   uuid.New(),
	}

	err := dm.RefreshDMVoiceCallLease(ctx, nil, lease, time.Minute, true)
	assert.Error(t, err)
	assert.False(t, errors.Is(err, dm.ErrDMVoiceCallLeaseConflict))
}

func TestDMVoiceCallCleanup_FencesReplacementUntilEnd(t *testing.T) {
	redisClient, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	conversationID := uuid.New()
	callerUserID := uuid.New()
	terminal := dm.VoiceCallLease{
		ConversationID: conversationID,
		CallID:         uuid.New(),
		CallerUserID:   callerUserID,
	}
	replacement := terminal
	replacement.CallID = uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(ctx, redisClient, terminal, time.Minute, true))

	acquired, err := dm.BeginDMVoiceCallCleanup(
		ctx, redisClient, conversationID, terminal.CallID,
	)
	require.NoError(t, err)
	require.True(t, acquired)
	require.ErrorIs(t,
		dm.RefreshDMVoiceCallLease(ctx, redisClient, replacement, time.Minute, true),
		dm.ErrDMVoiceCallLeaseConflict,
		"a replacement cannot claim the lease during conversation-wide cleanup",
	)

	require.NoError(t, dm.EndDMVoiceCallCleanup(
		ctx, redisClient, conversationID, terminal.CallID,
	))
	require.NoError(t,
		dm.RefreshDMVoiceCallLease(ctx, redisClient, replacement, time.Minute, true),
		"the replacement may claim the lease after cleanup releases its guard",
	)
}

func TestDMVoiceCallCleanup_StaleBeginPreservesReplacement(t *testing.T) {
	redisClient, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	conversationID := uuid.New()
	replacement := dm.VoiceCallLease{
		ConversationID: conversationID,
		CallID:         uuid.New(),
		CallerUserID:   uuid.New(),
	}
	require.NoError(t, dm.RefreshDMVoiceCallLease(ctx, redisClient, replacement, time.Minute, true))

	acquired, err := dm.BeginDMVoiceCallCleanup(
		ctx, redisClient, conversationID, uuid.New(),
	)
	require.NoError(t, err)
	assert.False(t, acquired, "a stale terminal call cannot acquire the cleanup guard")
	stored, found, err := dm.LookupDMVoiceCallLease(ctx, redisClient, conversationID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, replacement, stored)
}

func TestDMVoiceJoinAdmission_IsScopedToConversationAndUser(t *testing.T) {
	redisClient, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	conversationID := uuid.New()
	userID := uuid.New()
	callID := uuid.New()
	lease := dm.VoiceCallLease{
		ConversationID: conversationID,
		CallID:         callID,
		CallerUserID:   userID,
	}
	require.NoError(t, dm.RefreshDMVoiceCallLease(ctx, redisClient, lease, time.Minute, true))
	require.NoError(t, dm.RememberDMVoiceJoinAdmission(
		ctx, redisClient, conversationID, userID, callID, time.Minute,
	))

	stored, found, err := dm.LookupDMVoiceJoinAdmission(
		ctx, redisClient, conversationID, userID,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, callID, stored)

	_, found, err = dm.LookupDMVoiceJoinAdmission(
		ctx, redisClient, conversationID, uuid.New(),
	)
	require.NoError(t, err)
	assert.False(t, found, "another member cannot consume this user's admission")

	_, found, err = dm.LookupDMVoiceJoinAdmission(
		ctx, redisClient, uuid.New(), userID,
	)
	require.NoError(t, err)
	assert.False(t, found, "an admission cannot cross conversation boundaries")
}

func TestDMVoiceJoinAdmission_StaleCallCannotOverwriteReplacement(t *testing.T) {
	redisClient, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	conversationID := uuid.New()
	userID := uuid.New()
	oldLease := dm.VoiceCallLease{
		ConversationID: conversationID,
		CallID:         uuid.New(),
		CallerUserID:   userID,
	}
	require.NoError(t, dm.RefreshDMVoiceCallLease(ctx, redisClient, oldLease, time.Minute, true))
	require.NoError(t, dm.RememberDMVoiceJoinAdmission(
		ctx, redisClient, conversationID, userID, oldLease.CallID, time.Minute,
	))
	require.NoError(t, dm.DeleteDMVoiceCallLease(
		ctx, redisClient, conversationID, oldLease.CallID,
	))

	replacement := oldLease
	replacement.CallID = uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(ctx, redisClient, replacement, time.Minute, true))
	require.NoError(t, dm.RememberDMVoiceJoinAdmission(
		ctx, redisClient, conversationID, userID, replacement.CallID, time.Minute,
	))
	require.ErrorIs(t, dm.RememberDMVoiceJoinAdmission(
		ctx, redisClient, conversationID, userID, oldLease.CallID, time.Minute,
	), dm.ErrDMVoiceCallLeaseConflict)

	stored, found, err := dm.LookupDMVoiceJoinAdmission(
		ctx, redisClient, conversationID, userID,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, replacement.CallID, stored,
		"a delayed join cannot replace the member's newer admitted call")
}

func TestClearUnpromotedDMVoiceCallReservation_ExactAndTerminal(t *testing.T) {
	redisClient, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	conversationID := uuid.New()
	oldLease := dm.VoiceCallLease{
		ConversationID: conversationID,
		CallID:         uuid.New(),
		CallerUserID:   uuid.New(),
	}
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		ctx, redisClient, oldLease, dm.DMVoiceCallReservationTTL, true,
	))
	require.NoError(t, dm.DeleteDMVoiceCallLease(
		ctx, redisClient, conversationID, oldLease.CallID,
	))

	replacement := oldLease
	replacement.CallID = uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		ctx, redisClient, replacement, dm.DMVoiceCallReservationTTL, true,
	))
	cleared, err := dm.ClearUnpromotedDMVoiceCallReservation(
		ctx, redisClient, conversationID, oldLease.CallID,
	)
	require.NoError(t, err)
	assert.False(t, cleared, "a stale observed call ID cannot clear a replacement reservation")
	stored, found, err := dm.LookupDMVoiceCallLease(ctx, redisClient, conversationID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, replacement, stored)

	ttlBefore, err := redisClient.PTTL(
		ctx, "dm_voice_call_lease:"+conversationID.String(),
	).Result()
	require.NoError(t, err)
	require.NoError(t, dm.MarkDMVoiceCallMediaAuthorized(
		ctx, redisClient, conversationID, replacement.CallID,
	))
	ttlAfter, err := redisClient.PTTL(
		ctx, "dm_voice_call_lease:"+conversationID.String(),
	).Result()
	require.NoError(t, err)
	assert.Positive(t, ttlAfter)
	assert.LessOrEqual(t, ttlAfter, ttlBefore,
		"media authorization marks but never refreshes the reservation")
	cleared, err = dm.ClearUnpromotedDMVoiceCallReservation(
		ctx, redisClient, conversationID, replacement.CallID,
	)
	require.NoError(t, err)
	assert.False(t, cleared,
		"a successful media authorization protects the pre-presence handoff")
	stored, found, err = dm.LookupDMVoiceCallLease(ctx, redisClient, conversationID)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, stored.MediaAuthorized)

	require.NoError(t, dm.DeleteDMVoiceCallLease(
		ctx, redisClient, conversationID, replacement.CallID,
	))
	unadmitted := replacement
	unadmitted.CallID = uuid.New()
	unadmitted.MediaAuthorized = false
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		ctx, redisClient, unadmitted, dm.DMVoiceCallReservationTTL, true,
	))
	cleared, err = dm.ClearUnpromotedDMVoiceCallReservation(
		ctx, redisClient, conversationID, unadmitted.CallID,
	)
	require.NoError(t, err)
	require.True(t, cleared)
	require.ErrorIs(t, dm.RefreshDMVoiceCallLease(
		ctx, redisClient, unadmitted, dm.DMVoiceCallReservationTTL, true,
	), dm.ErrDMVoiceCallLeaseClosed,
		"delayed media events cannot resurrect a superseded reservation")

	next := unadmitted
	next.CallID = uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		ctx, redisClient, next, dm.DMVoiceCallReservationTTL, true,
	), "a new call may reserve the conversation after the exact superseded ID is closed")
}

func TestClearUnpromotedDMVoiceCallReservation_PreservesPromotedLease(t *testing.T) {
	redisClient, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	lease := dm.VoiceCallLease{
		ConversationID: uuid.New(),
		CallID:         uuid.New(),
		CallerUserID:   uuid.New(),
	}
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		ctx, redisClient, lease, dm.DMVoiceCallLeaseTTL, true,
	))
	cleared, err := dm.ClearUnpromotedDMVoiceCallReservation(
		ctx, redisClient, lease.ConversationID, lease.CallID,
	)
	require.NoError(t, err)
	assert.False(t, cleared, "a joined/heartbeat-promoted lease must be preserved")
	stored, found, err := dm.LookupDMVoiceCallLease(ctx, redisClient, lease.ConversationID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, lease, stored)
}

func TestAbortAuthorizedDMVoiceCallReservation_ExactShortDirectReservationOnly(t *testing.T) {
	redisClient, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	lease := dm.VoiceCallLease{
		ConversationID: uuid.New(),
		CallID:         uuid.New(),
		CallerUserID:   uuid.New(),
	}
	record := func(candidate dm.VoiceCallLease, ttl time.Duration) {
		require.NoError(t, dm.RefreshDMVoiceCallLease(ctx, redisClient, candidate, ttl, true))
		require.NoError(t, dm.MarkDMVoiceCallMediaAuthorized(
			ctx, redisClient, candidate.ConversationID, candidate.CallID,
		))
	}

	record(lease, dm.DMVoiceCallReservationTTL)
	aborted, err := dm.AbortAuthorizedDMVoiceCallReservation(
		ctx, redisClient, lease.ConversationID, lease.CallID,
	)
	require.NoError(t, err)
	require.True(t, aborted)
	_, found, err := dm.LookupDMVoiceCallLease(ctx, redisClient, lease.ConversationID)
	require.NoError(t, err)
	assert.False(t, found)
	require.ErrorIs(t, dm.RefreshDMVoiceCallLease(
		ctx, redisClient, lease, dm.DMVoiceCallReservationTTL, true,
	), dm.ErrDMVoiceCallLeaseClosed)

	replacement := lease
	replacement.CallID = uuid.New()
	record(replacement, dm.DMVoiceCallReservationTTL)
	aborted, err = dm.AbortAuthorizedDMVoiceCallReservation(
		ctx, redisClient, lease.ConversationID, lease.CallID,
	)
	require.NoError(t, err)
	assert.False(t, aborted, "a stale abort cannot clear a successor")

	require.NoError(t, dm.DeleteDMVoiceCallLease(
		ctx, redisClient, replacement.ConversationID, replacement.CallID,
	))
	promoted := replacement
	promoted.CallID = uuid.New()
	record(promoted, dm.DMVoiceCallLeaseTTL)
	require.NoError(t, redisClient.PExpire(
		ctx, "dm_voice_call_lease:"+promoted.ConversationID.String(), 30*time.Second,
	).Err())
	aborted, err = dm.AbortAuthorizedDMVoiceCallReservation(
		ctx, redisClient, promoted.ConversationID, promoted.CallID,
	)
	require.NoError(t, err)
	assert.False(t, aborted, "a promoted active call cannot be aborted")

	require.NoError(t, dm.DeleteDMVoiceCallLease(
		ctx, redisClient, promoted.ConversationID, promoted.CallID,
	))
	ring := promoted
	ring.CallID = uuid.New()
	ring.RingID = uuid.New()
	record(ring, dm.DMVoiceCallReservationTTL)
	aborted, err = dm.AbortAuthorizedDMVoiceCallReservation(
		ctx, redisClient, ring.ConversationID, ring.CallID,
	)
	require.NoError(t, err)
	assert.False(t, aborted, "accepted rings require their own terminal policy")
}
