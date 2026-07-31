//go:build integration

package rbac

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Spec §12 "Timing" / §8 #1794.
//
// WHY THIS ASSERTS STRUCTURE, NOT WALL CLOCK. The spec phrases the invariant as
// "statistically indistinguishable latency", and the plan sketched a p95 ratio
// with a 0.35 delta. A wall-clock p95 over 60 samples is not a defensible
// assertion on this project's SHARED PostgreSQL and Redis: another worktree's
// integration run, autovacuum, or a cold buffer cache moves the p95 of a
// sub-millisecond statement by more than the effect being measured, so the test
// would fail for reasons unrelated to the code and, worse, would PASS while the
// disclosure existed whenever ambient noise happened to swamp it.
//
// The property that actually makes the latency indistinguishable is structural
// and directly observable: the authority transaction issues the SAME number of
// in-transaction visibility queries — zero — whether the channel has no active
// senders or is hidden from everyone. That is the cause; the equal latency is
// only its symptom. Asserting the cause is both stronger and deterministic.
//
// Both arms below are checked against the same env so they share connection
// pool, cache and fixture state; only the channel differs.
func TestUpsertChannelOverride_LatencyDoesNotDiscloseChannelOccupancy(t *testing.T) {
	env := newRBACPresenceEnv(t)
	defer env.Close()

	category := env.createCategory(t)
	empty := env.createVoiceChannel(t, category, false)
	hidden := env.createHiddenVoiceChannel(t, category)

	before := env.visibilityQueryCount()
	require.Equal(t, http.StatusOK, env.upsertChannelOverride(t, empty))
	emptyQueries := env.visibilityQueryCount() - before

	before = env.visibilityQueryCount()
	require.Equal(t, http.StatusOK, env.upsertChannelOverride(t, hidden))
	hiddenQueries := env.visibilityQueryCount() - before

	assert.Equal(t, hiddenQueries, emptyQueries,
		"a zero-active-sender channel and a hidden channel must run an identical "+
			"statement sequence; %d vs %d in-transaction visibility queries is a "+
			"timing side channel on channel occupancy", emptyQueries, hiddenQueries)
	assert.Zero(t, emptyQueries,
		"with no active senders there is no candidate set, so phase 2 issues no query at all")
}

// Channel enumeration keys on voice_participants, never on the actor's own
// visibility: a channel the ACTOR cannot see still captures its active senders,
// so an actor cannot probe which channels it is blind to by watching for a
// behavioural difference.
func TestUpsertChannelOverride_CaptureKeysOnParticipants_NotActorVisibility(t *testing.T) {
	env := newRBACPresenceEnv(t)
	defer env.Close()

	hidden := env.createHiddenVoiceChannel(t, env.createCategory(t))
	senderID := env.joinVoice(t, hidden)

	before := env.visibilityQueryCount()
	require.Equal(t, http.StatusOK, env.upsertChannelOverride(t, hidden))

	assert.Greater(t, env.visibilityQueryCount(), before,
		"an occupied channel captures regardless of who can see it")
	assert.Equal(t, 1, env.refreshCount(senderID),
		"its active sender is rechecked exactly once")
}

// A capture failure must be indistinguishable from any other 500 out of the same
// handler branch. If the bodies differed, an actor could probe whether a channel
// had active Rich Presence senders by reading the error text.
func TestUpsertChannelOverride_CaptureFailureBody_IsIdenticalToGenericFailure(t *testing.T) {
	env := newRBACPresenceEnv(t)
	defer env.Close()
	channelID := env.createVoiceChannel(t, env.createCategory(t), false)
	env.joinVoice(t, channelID)

	env.injectCaptureFailure()
	captureStatus, captureBody := env.upsertChannelOverrideWithBody(t, channelID)
	env.clearInjectedFailure()

	// Indistinguishability here is STRUCTURAL, not empirical, so this asserts the
	// exact shared body rather than comparing against a second induced failure.
	//
	// withAuthorityCapture returns ONE error for both a capture failure and a
	// write failure, and UpsertChannelOverride has exactly one branch for it:
	//
	//	if err != nil {
	//	    h.log.Error("Failed to upsert override", "error", err)
	//	    c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSaveOverride})
	//	    return
	//	}
	//
	// There is no reachable way to induce a *generic* write failure to compare
	// against: a bad target_type is rejected at binding (400, oneof=user role), a
	// nonexistent channel 404s before the write, target_id carries no foreign key,
	// and the UNIQUE constraint is absorbed by the upsert. Comparing two induced
	// failures would therefore only re-prove that one code path equals itself.
	require.Equal(t, http.StatusInternalServerError, captureStatus)
	assert.JSONEq(t, `{"error":"`+errMsgFailedSaveOverride+`"}`, captureBody,
		"a capture failure returns the handler's generic save-override body, "+
			"so the error text cannot reveal whether the channel had active senders")
}
