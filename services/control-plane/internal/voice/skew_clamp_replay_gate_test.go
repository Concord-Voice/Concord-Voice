package voice_test

// #3205 Task 0 — GATE, run before any production code changes.
//
// Direction B (the skew clamp) rests on one unproven claim: a re-delivered
// far-future frame, clamped to a FRESHER value on each delivery, cannot keep
// a row alive that #2907's reconciliation would otherwise reap. During
// PR #3187 review a delayed replay was found to renew a stale observed lease
// indefinitely, defeating #2907; migration 000133 rewrote the observed-lease
// trigger to fire only when lifecycle_event_at actually CHANGES.
//
// This gate RAN before the clamp existed, when parseVoiceEventTime still took
// a single `raw string` argument, and it stays written that way: it must keep
// proving the replay claim from the database side alone, independently of the
// clamp it gated. It therefore simulates the clamp rather than calling it --
// each replay round supplies an "already clamped"
// timestamp constructed the way the real clamp will construct it once Task 2
// lands (min(producerStamp, receivedAt), receivedAt taken fresh per
// delivery) and drives it through the REAL production entry points --
// renewObservedLeaseForFutureStampedRows (the #3187 renewer) and the
// moveServerVoiceParticipant upsert 000133's trigger sits on -- rather than
// hand-computing an expected row.
//
// Two independent claims, kept in separate tests because they exercise
// different code paths and different mechanisms:
//
//   - Half 1: renewObservedLeaseForFutureStampedRows is a bare UPDATE. It has
//     no INSERT arm, so a row #2907 already deleted cannot be brought back by
//     it -- structurally, not because of 000133's condition. Recorded here as
//     a named claim rather than assumed, and the mutation step below checks
//     whether it actually depends on 000133 at all.
//   - Half 2: the upsert's `ON CONFLICT ... DO UPDATE ... WHERE
//     participant.lifecycle_event_at <= EXCLUDED.lifecycle_event_at` clause
//     governs whether an UPDATE runs at all. A STRICTLY OLDER incoming stamp
//     never enters the UPDATE, so the trigger never fires for it regardless
//     of 000133's own condition -- tested as
//     TestRefusedOlderReplayDoesNotAdvanceTheObservedLease. An EQUAL incoming
//     stamp (the exact shape a byte-identical redelivery produces) DOES enter
//     the UPDATE (`<=` admits equality) and is the case 000133 actually
//     discriminates: pre-000133 the trigger re-stamped
//     lifecycle_observed_at unconditionally on that UPDATE; 000133 added the
//     IS DISTINCT FROM guard so an unchanged value leaves the lease alone.
//     Tested as TestByteIdenticalReplayDoesNotAdvanceTheObservedLease.

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// skewVoiceRoom builds the one fixture every test in this file needs: a server
// with a voice channel and one member of it, returned as the parsed IDs the
// production calls take plus the raw strings the SQL assertions take.
//
// Extracted because five near-identical copies of it tripped SonarCloud's 3%
// new-code duplication gate at 4.4%. That is the gate doing its job -- the
// blocks differed only in a name prefix, which is the definition of a helper
// waiting to be written.
func skewVoiceRoom(
	t *testing.T, ts *testhelpers.TestServer, prefix string,
) (channelID, userID uuid.UUID, channel, member string) {
	t.Helper()
	owner := ts.CreateTestUser(t, prefix+"-owner")
	server := ts.CreateTestServer(t, owner.ID, prefix+"-server")
	channel = ts.CreateVoiceChannel(t, server, prefix+"-channel")
	memberUser := ts.CreateTestUser(t, prefix+"-member")
	ts.AddMemberToServer(t, server, memberUser.ID, "member")
	return uuid.MustParse(channel), uuid.MustParse(memberUser.ID), channel, memberUser.ID
}

// TestReplayedRenewalCannotResurrectAReapedRow is Half 1 of the #3205 gate.
// A row #2907's reconciler has already deleted must stay deleted across
// repeated renewal attempts carrying increasing (fresher-clamped) stamps.
func TestReplayedRenewalCannotResurrectAReapedRow(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	ctx := context.Background()

	channelID, userID, channel, memberID := skewVoiceRoom(t, ts, "skew-gate-h1")

	// Seed the row, then reap it exactly as #2907's reconciler would: a hard
	// DELETE, not a soft/logical removal.
	insertVoiceParticipant(t, ts.DB, channel, memberID)
	_, err := ts.DB.Exec(
		`DELETE FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
		channel, memberID)
	require.NoError(t, err)
	require.False(t, voiceParticipantExists(t, ts.DB, channel, memberID),
		"setup: the row must actually be gone before the replay rounds")

	for round := 0; round < 3; round++ {
		// The claim here is STRUCTURAL, not fixture-driven: the renewal is a bare
		// UPDATE with no INSERT arm and takes no stamp parameter at all, so no
		// value a replay could carry has an arm to travel down. Repeating it three
		// times pins that there is no accumulating side effect -- a retry counter,
		// a lazily-recreated row -- rather than varying an input, and the sleep
		// only advances the wall clock the renewal reads DB-side.
		sub.RenewObservedLeaseForFutureStampedRowsForTest(ctx, channelID, []uuid.UUID{userID})
		require.False(t, voiceParticipantExists(t, ts.DB, channel, memberID),
			"round %d: a reaped row was resurrected by a replayed renewal", round)
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRefusedOlderReplayDoesNotAdvanceTheObservedLease is the literal shape
// described in the #3205 plan's Task 0: a strictly OLDER incoming stamp,
// which the `<=` upsert fence refuses outright.
func TestRefusedOlderReplayDoesNotAdvanceTheObservedLease(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	ctx := context.Background()

	channelID, userID, channel, memberID := skewVoiceRoom(t, ts, "skew-gate-h2a")

	insertVoiceParticipant(t, ts.DB, channel, memberID)
	var before time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT lifecycle_observed_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
		channel, memberID).Scan(&before))

	// Strictly older than the seeded row's own lifecycle_event_at
	// (2026-01-01, per insertVoiceParticipant) -- refused by the fence.
	stale := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	result, err := sub.UpsertServerVoiceParticipantForTest(ctx, channelID, userID, stale)
	require.NoError(t, err)
	require.False(t, result.Applied, "control: the older stamp must actually be refused")

	var after time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT lifecycle_observed_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
		channel, memberID).Scan(&after))
	require.True(t, after.Equal(before),
		"a refused older upsert advanced the observed lease: %s -> %s", before, after)
}

// TestByteIdenticalReplayDoesNotAdvanceTheObservedLease is the case 000133
// actually discriminates: an EQUAL incoming stamp is ACCEPTED by the `<=`
// fence (equality satisfies it), so the UPDATE runs and the BEFORE UPDATE OF
// lifecycle_event_at trigger fires -- but the value is unchanged, and
// 000133's IS DISTINCT FROM guard is what keeps lifecycle_observed_at from
// moving on that no-op UPDATE. Pre-000133 this renewed unconditionally,
// which is the exact #3187 hole.
func TestByteIdenticalReplayDoesNotAdvanceTheObservedLease(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	ctx := context.Background()

	channelID, userID, channel, memberID := skewVoiceRoom(t, ts, "skew-gate-h2b")

	insertVoiceParticipant(t, ts.DB, channel, memberID)
	var seeded time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
		channel, memberID).Scan(&seeded))

	var before time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT lifecycle_observed_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
		channel, memberID).Scan(&before))
	time.Sleep(20 * time.Millisecond) // make a wrongly-renewed observed_at detectable

	// Three byte-identical redeliveries of the SAME stamp -- the exact
	// producer replay shape, not a fresher clamp.
	for round := 0; round < 3; round++ {
		result, err := sub.UpsertServerVoiceParticipantForTest(ctx, channelID, userID, seeded)
		require.NoError(t, err)
		require.True(t, result.Applied,
			"control round %d: an equal stamp must be ACCEPTED by the <= fence, not refused", round)
	}

	var after time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT lifecycle_observed_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
		channel, memberID).Scan(&after))
	require.True(t, after.Equal(before),
		"a byte-identical replay advanced the observed lease: %s -> %s", before, after)
}

// TestFresherClampedReplayRenewsTheLease is the case a clamped replay ACTUALLY
// produces, and the one the three tests above do not reach.
//
// Replaying one far-future frame at t1 < t2 < t3 yields min(farFuture, tN) = tN
// each time, so every redelivery carries a STRICTLY GREATER stamp. That enters
// the `<=` upsert's UPDATE arm, the value genuinely changes, and 000133's
// IS DISTINCT FROM guard therefore FIRES rather than suppressing. The lease is
// renewed. This test asserts that outcome rather than wishing it away.
//
// It does not kill Direction B, and the reason is not in this file: a phantom
// row kept alive this way is removed by the room's next genuine heartbeat.
// reconcileServerHeartbeatParticipants removes rows absent from the media
// participant list while refreshServerHeartbeatParticipant adds rows present in
// it, which is exactly why classifyVoiceLifecycleDrop treats joined/left/
// heartbeat as voiceLifecycleDropConvergent (lifecycle_dispatcher.go:64-70).
// Convergence is bounded by the 30 s heartbeat tick, not by the lease.
//
// What WOULD kill B is a fresher replay reaching a row nothing can subsequently
// remove. That case is room_empty, whose deletion fence is
// `lifecycle_event_at <= $3` -- a fresher stamp makes the delete MORE likely to
// apply, never less -- and it is governed by the clamp reference being NATS
// RECEIPT time rather than handler-drain time. The spec's §5.3 receipt-time
// regression is the test for that, and it belongs to Task 6, not to this gate.
//
// Recorded here so the next reader does not mistake the three passing tests
// above for coverage of the replay shape the clamp really creates.
func TestFresherClampedReplayRenewsTheLease(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	ctx := context.Background()

	channelID, userID, channel, memberID := skewVoiceRoom(t, ts, "skew-gate-h3")

	insertVoiceParticipant(t, ts.DB, channel, memberID)
	var seeded time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT lifecycle_event_at FROM voice_participants
		  WHERE channel_id = $1 AND user_id = $2`,
		channel, memberID).Scan(&seeded))

	var previous time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT lifecycle_observed_at FROM voice_participants
		  WHERE channel_id = $1 AND user_id = $2`,
		channel, memberID).Scan(&previous))

	// Three replays, each clamped to a fresher instant than the last.
	for round := 1; round <= 3; round++ {
		clamped := seeded.Add(time.Duration(round) * time.Second)
		result, err := sub.UpsertServerVoiceParticipantForTest(ctx, channelID, userID, clamped)
		require.NoError(t, err)
		require.True(t, result.Applied,
			"round %d: a fresher clamped stamp must be accepted by the <= fence", round)

		var observed time.Time
		require.NoError(t, ts.DB.QueryRow(
			`SELECT lifecycle_observed_at FROM voice_participants
			  WHERE channel_id = $1 AND user_id = $2`,
			channel, memberID).Scan(&observed))
		require.True(t, observed.After(previous),
			"round %d: 000133's guard suppressed a CHANGED lifecycle_event_at; "+
				"if this fails the trigger is over-suppressing and #3187's "+
				"renewal is dead in steady state", round)
		previous = observed
	}
}
