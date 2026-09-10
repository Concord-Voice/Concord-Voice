package voice_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skewRoomEmptyFrame builds a voice.room_empty payload stamped at `at`.
func skewRoomEmptyFrame(t *testing.T, channelID string, at time.Time) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"channelId": channelID,
		"timestamp": at.UTC().Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	return payload
}

// TestStaleRoomEmptyDoesNotEvictARejoinedParticipant is the test that
// distinguishes RECEIPT-time clamping from HANDLER-DRAIN-time clamping, and it
// is the only one that does.
//
// Every other test in this change passes under either choice. That matters
// because drain time is the natural thing to reach for -- it needs no plumbing,
// no dispatcher field, and no widened handler signature -- so without this test
// a later reader deletes ~8 files of threading as dead weight and every suite
// stays green.
//
// The scenario is ordinary, not adversarial. There is no queue group on the
// voice wildcard subscribe (nats.go), so EVERY replica receives EVERY event.
// Replica 2's dispatcher is backlogged behind other rooms:
//
//	t0        R2 RECEIVES room_empty (far-future producer stamp) -- not drained
//	t0+5s     R1 applies a legitimate rejoin
//	t0+30s    R2 finally drains the room_empty it received at t0
//
// Under RECEIPT-time clamping the stale room_empty carries t0, which is older
// than the rejoin, and the `lifecycle_event_at <= $3` delete fence refuses it.
// Under DRAIN-time clamping it carries t0+30s, which is NEWER than the rejoin,
// and the fence deletes a participant who is still in the call -- the CWE-863
// shape that got PR #3201 closed, reintroduced through a different door.
//
// The clamp only ever moves a stamp DOWN, so dispatcher lateness must never be
// able to move one up. That is the whole invariant.
func TestStaleRoomEmptyDoesNotEvictARejoinedParticipant(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	_, _, channel, memberID := skewVoiceRoom(t, ts, "skew-recv")

	// t0 is 30 s in the PAST, and that is the load-bearing part of the fixture.
	// The backlog has to be real: if t0 were time.Now(), drain time would be
	// microseconds later and STILL older than the rejoin, so the test would pass
	// under drain-time clamping too and prove nothing. Measured -- the first
	// version of this test used time.Now() and survived the mutation below.
	t0 := time.Now().UTC().Add(-30 * time.Second)

	// R2 receives room_empty at t0. Its producer stamp is far-future -- this is
	// the wrong-clocked media host the whole change exists for -- so the clamp
	// is what decides the value that reaches the delete fence.
	roomEmpty := skewRoomEmptyFrame(t, channel, t0.Add(24*time.Hour))

	// A live participant, then a legitimate rejoin applied by R1 at t0+5s.
	insertVoiceParticipant(t, ts.DB, channel, memberID)
	require.True(t, voiceParticipantExists(t, ts.DB, channel, memberID),
		"setup: the participant must be live before the stale terminal drains")
	_, err := ts.DB.Exec(
		`UPDATE voice_participants SET lifecycle_event_at = $1
		  WHERE channel_id = $2 AND user_id = $3`,
		t0.Add(5*time.Second), channel, memberID)
	require.NoError(t, err)

	// R2 drains the stale terminal NOW, 30 s after receiving it. receivedAt is
	// t0 -- when it ARRIVED -- not the wall clock at drain.
	sub.HandleRoomEmptyAt(roomEmpty, t0)

	require.True(t, voiceParticipantExists(t, ts.DB, channel, memberID),
		"a stale room_empty evicted a live participant: the clamp reference is "+
			"drain time, not receipt time")
}

// TestClampNeverMovesAStampForward is the same invariant stated directly on the
// parse boundary, as a property rather than a scenario.
//
// The integration test above can only fail one way at a time; this one closes
// the general case, including subjects it does not exercise. Both are cheap and
// they fail for different reasons, which is the point: a refactor that breaks
// the threading fails the integration test, while one that inverts a comparison
// fails this one.
//
// It also pins the microsecond GRANULARITY of the returned stamp. The equality
// assertions below are written against the truncation rather than against the
// raw input, and that is not a relaxation to accommodate the implementation --
// granularity is a contract this rail depends on, asserted directly by the
// alignment check and by TestClampTruncatesToStorablePrecision. A stamp finer
// than timestamptz can represent is rounded on write and reads back LATER than
// the value that wrote it, which the Go-side After() comparisons then read as a
// newer row. Do not restore at.Equal(produced): it passes only while the stamp
// is unstorable.
func TestClampNeverMovesAStampForward(t *testing.T) {
	receivedAt := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	for _, offset := range []time.Duration{
		-365 * 24 * time.Hour, -time.Hour, -time.Nanosecond,
		0,
		time.Nanosecond, time.Hour, 365 * 24 * time.Hour,
	} {
		produced := receivedAt.Add(offset)
		at, skew, err := voice.ParseVoiceEventTimeForTest(
			produced.Format(time.RFC3339Nano), receivedAt)
		require.NoError(t, err)
		require.False(t, at.After(receivedAt),
			"offset %s: the clamp moved a stamp FORWARD of its receipt time", offset)
		require.Equal(t, offset, skew,
			"offset %s: skew must be the PRE-clamp difference", offset)
		require.Zero(t, at.Nanosecond()%1000,
			"offset %s: the stamp carries sub-microsecond precision that "+
				"timestamptz cannot store", offset)
		if offset > 0 {
			require.True(t, at.Equal(receivedAt.Truncate(time.Microsecond)),
				"offset %s: not clamped to receipt", offset)
		} else {
			require.True(t, at.Equal(produced.Truncate(time.Microsecond)),
				"offset %s: a non-future stamp was altered beyond truncation", offset)
		}
	}
}

// TestClampTruncatesToStorablePrecision states the granularity contract on its
// own, in the one direction that matters.
//
// TRUNCATE, never round. Rounding advances a stamp by up to 500ns, and advancing
// is the single direction that lets DELETE ... lifecycle_event_at <= $3 remove a
// LIVE row -- the CWE-863 breach PR #3201 was closed for. A rounding
// implementation passes every assertion in the test above except this one, so
// the explicit not-after check is what separates the two.
func TestClampTruncatesToStorablePrecision(t *testing.T) {
	// 502ns and 999ns round UP; 498ns and 0ns do not. A rounding implementation
	// is distinguishable only on the first two.
	for _, remainder := range []time.Duration{0, 1, 498, 499, 500, 501, 502, 999} {
		receivedAt := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC).Add(remainder)
		produced := receivedAt.AddDate(1, 0, 0)

		at, _, err := voice.ParseVoiceEventTimeForTest(
			produced.Format(time.RFC3339Nano), receivedAt)
		require.NoError(t, err)
		require.Zero(t, at.Nanosecond()%1000,
			"remainder %dns: sub-microsecond precision survived the clamp", remainder)
		require.False(t, at.After(receivedAt),
			"remainder %dns: the stamp was ROUNDED, not truncated -- it now sits "+
				"after its own receipt time and can delete a live row", remainder)
		require.True(t, at.Equal(receivedAt.Truncate(time.Microsecond)),
			"remainder %dns: not truncated to the storable microsecond", remainder)
	}
}

// TestEmptyDMHeartbeatCompletesTerminalUnderAFarFutureStamp covers the rail
// where this change's design constraint INVERTS.
//
// The issue frames heartbeat as a LEVEL — a periodic restatement of who is
// present, carried by the participant set rather than by the stamp — and
// joined/left/room_empty as EDGES whose meaning IS their position in stamp
// order. That holds on the server rail. It does not hold here:
// handleDMHeartbeat routes len(UserIDs) == 0 to handleEmptyDMHeartbeat, which
// synthesizes a voiceRoomEmptyEvent FROM the heartbeat, so on the DM rail a
// zero-participant heartbeat IS the terminal edge.
//
// That inversion is why Direction A was rejected. dm_voice_participants has no
// lifecycle_observed_at column (000132 covers voice_participants only) and
// staleServerVoiceDiscoverySQL reads only that table, so there is no lease to
// renew and no sweeper behind it: refusing this frame's stamp would lose the
// room_empty permanently. Under the clamp the frame is applied instead, and
// nothing DM-specific was needed to achieve that.
//
// KNOW WHAT THIS TEST DOES AND DOES NOT DISCRIMINATE. It passes with the clamp
// REVERTED, and that is not a flaw to be silently tolerated -- it is a property
// of the fence being exercised. The DM terminal deletes under
// `lifecycle_event_at <= $3`, so an unclamped year-ahead stamp satisfies the
// predicate just as the clamped one does and every assertion below still holds.
// What this test discriminates against is the REJECTED Direction A (refuse the
// stamp, keep the frame), under which the terminal never runs at all and the
// rows survive. Read it as "Direction A would have broken the DM rail", never as
// "the clamp is wired". The tests that actually pin the clamp are
// TestStaleRoomEmptyDoesNotEvictARejoinedParticipant below and
// TestVoiceLifecycleDispatcherStampsReceiptTimeAtEnqueue in
// lifecycle_dispatcher_test.go, both of which go red under a drain-time clamp.
func TestEmptyDMHeartbeatCompletesTerminalUnderAFarFutureStamp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "skew-dm-caller")
	peer := ts.CreateTestUser(t, "skew-dm-peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callerID, peerID := uuid.MustParse(caller.ID), uuid.MustParse(peer.ID)
	callID := uuid.New()
	joinedAt := time.Now().UTC().Add(-time.Minute)

	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: callerID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $4, $4), ($1, $3, $4, $4)
	`, conversationID, callerID, peerID, joinedAt)
	require.NoError(t, err)
	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID),
		"setup: the call must be live before the terminal arrives")

	// Observe each terminal delete. This is what makes the test assert the
	// MECHANISM rather than a side effect: the delete is a callback the presence
	// service invokes, and the row being absent afterwards does not prove the
	// terminal ran. `applied` is the only value that separates the two, and it
	// is deliberately not recoverable from the return value or the logs (see the
	// hook's field comment in nats.go).
	type deleteOutcome struct {
		applied bool
		err     error
	}
	outcomes := map[uuid.UUID]deleteOutcome{}
	sub.SetDMTerminalDeleteObservedHookForTest(
		func(participantID uuid.UUID, applied bool, err error) {
			outcomes[participantID] = deleteOutcome{applied: applied, err: err}
		},
	)

	// A stamp a year ahead — the wrong-clocked media host this change exists
	// for. receivedAt is now, so the clamp bounds it to now.
	receivedAt := time.Now().UTC()
	sub.HandleHeartbeatAt(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(),
		"callId":    callID.String(),
		"userIds":   []string{},
		"timestamp": receivedAt.AddDate(1, 0, 0).Format(time.RFC3339Nano),
	}), receivedAt)

	// Diagnose before asserting. The terminal is gated by
	// dmCallEventMayMutateLiveState, which refuses when a lease EXISTS carrying a
	// different call ID -- so a bare "row still present" tells you nothing about
	// which of the several fences declined. Reading the lease back here turns a
	// one-line failure into the actual reason.
	postLease, postHasLease, leaseErr := dm.LookupDMVoiceCallLease(
		context.Background(), ts.Redis, conversationID,
	)
	leaseState := "lookup failed: " + fmt.Sprint(leaseErr)
	if leaseErr == nil {
		leaseState = fmt.Sprintf("hasLease=%t leaseCallID=%v wantCallID=%v",
			postHasLease, postLease.CallID, callID)
	}
	// Read BOTH rows, not just the caller's. The first CI failure this seam
	// caught reported `peer: ran but removed no row` while printing the CALLER's
	// row state -- which cannot distinguish the two explanations that matter:
	// the peer's row was already gone (so applied=false is benign and this
	// assertion is too strict), or it is still present (a real defect).
	rowState := func(participantID uuid.UUID) string {
		var at sql.NullTime
		err := ts.DB.QueryRow(
			`SELECT lifecycle_event_at FROM dm_voice_participants
			  WHERE conversation_id = $1 AND user_id = $2`,
			conversationID, participantID).Scan(&at)
		switch {
		case err == sql.ErrNoRows:
			return "ABSENT"
		case err != nil:
			return "read failed: " + fmt.Sprint(err)
		default:
			return "PRESENT lifecycle_event_at=" + at.Time.Format(time.RFC3339Nano)
		}
	}
	rows := fmt.Sprintf("caller=%s peer=%s", rowState(callerID), rowState(peerID))

	// Assert the mechanism first: the clear path must have RUN the delete for
	// both participants. A CI-only failure here names its own cause, which the
	// row assertion below structurally cannot -- a missing row proves nothing
	// about which stage produced it.
	// assert, not require: report BOTH participants. The caller can succeed while
	// the peer fails, and stopping at the first one hides exactly the asymmetry
	// that makes this interesting.
	for _, p := range []struct {
		name string
		id   uuid.UUID
	}{{"caller", callerID}, {"peer", peerID}} {
		outcome, observed := outcomes[p.id]
		assert.True(t, observed,
			"%s: the terminal delete was never attempted — the clear path returned "+
				"before reaching the participant loop.\n"+
				"  lease:         %s\n"+
				"  clamped stamp: %s\n"+
				"  rows:          %s",
			p.name, leaseState, receivedAt.Format(time.RFC3339Nano), rows)
		if !observed {
			continue
		}
		assert.True(t, outcome.applied,
			"%s: the terminal delete ran but removed no row.\n"+
				"  clear error:   %v\n"+
				"  lease:         %s\n"+
				"  clamped stamp: %s\n"+
				"  rows:          %s\n"+
				"  all outcomes:  %v",
			p.name, outcome.err, leaseState,
			receivedAt.Format(time.RFC3339Nano), rows, outcomes)
	}

	for _, id := range []string{caller.ID, peer.ID} {
		require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), id),
			"the DM terminal did not complete under a far-future stamp for %s — "+
				"this rail has no sweeper, so the row would be permanent.\n"+
				"  lease:         %s\n"+
				"  clamped stamp: %s\n"+
				"  rows:          %s",
			id, leaseState, receivedAt.Format(time.RFC3339Nano), rows)
	}
}

// TestDMTerminalAppliesUnderASubMicrosecondReceiptStamp is the failure the
// granularity contract exists for, on the rail where it is permanent.
//
// This is the defect the clamp ARMED rather than one it inherited. Every
// producer stamp is microsecond-aligned by construction -- the media plane mints
// them from a monotonic microsecond counter formatted with a six-digit fraction
// (nextVoiceLifecycleTimestamp, services/media-plane/src/lib/nats.ts) -- so
// before the clamp, eventAt was always storable and the round-trip through
// timestamptz was the identity. The clamp substitutes receivedAt, which on Linux
// is a nanosecond-granular time.Now(); on macOS it is microsecond-granular, so
// this reproduces on CI and cannot reproduce locally.
//
// The chain: the first participant's terminal commits, and
// commitPrivateVoiceParticipantSetClaim's advancePrivateVoiceParticipantRows
// stamps the SURVIVING participant's row to eventAt. PostgreSQL rounds it.
// The second participant's terminal then reads that row back and
// privateVoiceParticipantSetLockedIDs evaluates
// participant.lifecycleEventAt.After(eventAt) in GO -- round-trip against the
// unrounded original -- which is true whenever the nanoseconds rounded UP. The
// claim is rejected, clearDMRoomEmptyParticipant reports applied=false with a
// NIL error, and because dm_voice_participants has neither
// lifecycle_observed_at nor a sweeper, the row is permanent.
//
// The DELETE fence is NOT the defect and must not be "fixed": it compares inside
// PostgreSQL, where the parameter rounds exactly as the column did, so it always
// matched. Truncating only that parameter breaks a working comparison -- it makes
// the stored value strictly greater than the predicate -- and manufactures the
// permanent orphan it appears to prevent.
//
// The remainders are chosen to discriminate, not to sample: 502ns and 999ns
// round UP and fail without the truncation; 0ns and 498ns do not and pass either
// way. A run that only exercised the latter two is green against the defect.
func TestDMTerminalAppliesUnderASubMicrosecondReceiptStamp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remainder time.Duration
	}{
		{"microsecond_aligned", 0},
		{"nanoseconds_round_down", 498},
		{"nanoseconds_round_up", 502},
		{"nanoseconds_round_up_max", 999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriber(ts)

			caller := ts.CreateTestUser(t, "skew-us-caller")
			peer := ts.CreateTestUser(t, "skew-us-peer")
			conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
			callerID, peerID := uuid.MustParse(caller.ID), uuid.MustParse(peer.ID)
			callID := uuid.New()
			joinedAt := time.Now().UTC().Add(-time.Minute)

			require.NoError(t, dm.RefreshDMVoiceCallLease(
				context.Background(), ts.Redis, dm.VoiceCallLease{
					ConversationID: conversationID, CallID: callID, CallerUserID: callerID,
				}, dm.DMVoiceCallLeaseTTL, true,
			))
			_, err := ts.DB.Exec(`
				INSERT INTO dm_voice_participants
					(conversation_id, user_id, joined_at, lifecycle_event_at)
				VALUES ($1, $2, $4, $4), ($1, $3, $4, $4)
			`, conversationID, callerID, peerID, joinedAt)
			require.NoError(t, err)

			// applied is the only value that separates "the delete ran and removed
			// the row" from "the claim was rejected before the delete". The reject
			// path returns a nil error, so neither the return value nor the logs
			// can tell them apart.
			type deleteOutcome struct {
				applied bool
				err     error
			}
			outcomes := map[uuid.UUID]deleteOutcome{}
			sub.SetDMTerminalDeleteObservedHookForTest(
				func(participantID uuid.UUID, applied bool, err error) {
					outcomes[participantID] = deleteOutcome{applied: applied, err: err}
				},
			)

			// Force the remainder rather than sampling the clock: the natural
			// failure rate is ~50% per run on a nanosecond-clock host and 0% on a
			// microsecond one, which is a flake rather than a test.
			receivedAt := time.Now().UTC().Truncate(time.Microsecond).Add(tc.remainder)
			require.EqualValues(t, tc.remainder, receivedAt.Nanosecond()%1000,
				"setup: the remainder must be exactly as specified or this case "+
					"does not discriminate")

			sub.HandleHeartbeatAt(mustJSON(t, map[string]interface{}{
				"channelId": conversationID.String(),
				"callId":    callID.String(),
				"userIds":   []string{},
				"timestamp": receivedAt.AddDate(1, 0, 0).Format(time.RFC3339Nano),
			}), receivedAt)

			rowState := func(participantID uuid.UUID) string {
				var at sql.NullTime
				readErr := ts.DB.QueryRow(
					`SELECT lifecycle_event_at FROM dm_voice_participants
					  WHERE conversation_id = $1 AND user_id = $2`,
					conversationID, participantID).Scan(&at)
				switch {
				case readErr == sql.ErrNoRows:
					return "ABSENT"
				case readErr != nil:
					return "read failed: " + fmt.Sprint(readErr)
				default:
					return "PRESENT lifecycle_event_at=" + at.Time.Format(time.RFC3339Nano)
				}
			}
			rows := fmt.Sprintf("caller=%s peer=%s", rowState(callerID), rowState(peerID))

			// assert, not require: the caller succeeds while the peer is rejected,
			// and stopping at the first hides exactly that asymmetry.
			for _, p := range []struct {
				name string
				id   uuid.UUID
			}{{"caller", callerID}, {"peer", peerID}} {
				outcome, observed := outcomes[p.id]
				if assert.True(t, observed, "%s: the terminal delete was never attempted", p.name) {
					assert.True(t, outcome.applied,
						"%s: the terminal delete removed no row under a %dns receipt "+
							"remainder. The stamp was stored ROUNDED and read back later "+
							"than the event that wrote it, so the Go-side After() check "+
							"rejected the claim. This rail has no sweeper, so the row is "+
							"permanent.\n"+
							"  clamped stamp: %s\n"+
							"  rows:          %s\n"+
							"  outcomes:      %+v",
						p.name, tc.remainder, receivedAt.Format(time.RFC3339Nano),
						rows, outcomes)
				}
			}
			assert.False(t,
				dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID),
				"caller row survived the terminal: %s", rows)
			assert.False(t,
				dmVoiceParticipantExists(t, ts.DB, conversationID.String(), peer.ID),
				"peer row survived the terminal: %s", rows)
		})
	}
}
