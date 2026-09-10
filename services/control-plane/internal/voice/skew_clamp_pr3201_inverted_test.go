package voice_test

// The four #3201 red-team proofs, INVERTED -- acceptance criterion A9 of the
// #3205 design (spec section 5, test 2).
//
// PR #3201 REFUSED any producer stamp more than maxVoiceLifecycleForwardSkew
// (180 s) ahead of the control-plane clock. Four executable proofs closed it
// unmerged, because refusing the stamp threw away the frame, and on the
// heartbeat subject the frame is not an ordering carrier -- it is the periodic
// restatement of WHO IS PRESENT. Two things ride on that restatement and both
// were lost: voice_participants.lifecycle_observed_at, the database-clock lease
// #2907's reconciler evicts on, and recheckServerHeartbeatPermissions, the
// CV-CAN-007 sweep that force-disconnects a member banned, kicked or timed out
// while already inside a room. Refusing a heartbeat therefore switched
// mid-session RBAC enforcement OFF for every room that producer served, for the
// whole duration of the skew episode (CWE-863).
//
// #3205 CLAMPS instead: eventAt = min(producedAt, receivedAt). The frame is
// applied, so the presence claim survives and both consumers keep running. Each
// test below is the corresponding #3201 proof with its verdict inverted -- what
// was "PASS == exploited" is now "PASS == the exploit is closed".
//
// TWO HARNESS DEFECTS from the original set are repaired here, both recorded in
// ~/pr3201-redteam-poc/README.md:
//
//  1. ForwardSkewStripsTheRosterAndNeverRestoresIt asserted `reapedInRound == 0`
//     while the reap actually lands in round 2, so it FAILED while the exploit
//     it described was real. The sentinel for "never reaped" is -1, not 0. The
//     inverted assertions below test against -1 explicitly and therefore do not
//     depend on WHICH round a reap would have landed in.
//  2. clampSkewEpisode adds +round x 1 s to each stamp (modelling the media
//     plane's monotonic ratchet), so an "inside the bound" fixture within 6 s of
//     the boundary CROSSES it mid-episode. The control uses 150 s for that
//     reason, not 179 s.
//
// Every fixture ESTABLISHES A LIVE PARTICIPANT FIRST. This is spec section 5
// test 2's explicit requirement and it is load-bearing: an empty roster is the
// one starting state in which refusing a heartbeat has no consequence, so a
// proof that starts there cannot distinguish a working clamp from a broken one.
//
// WHAT THESE TESTS DISCRIMINATE AGAINST, MEASURED RATHER THAN ASSUMED. Both
// controls were run against this file:
//
//   - Mutant A -- parseVoiceEventTime REFUSES a stamp beyond the bound, which is
//     PR #3201's shape. All five tests go RED. This is the control that matters:
//     it is what makes them evidence for A9 rather than five green assertions.
//   - Mutant B -- parseVoiceEventTime returns the producer stamp verbatim with no
//     clamp, which is pre-#3205 `main`. All five tests still PASS.
//
// Mutant B is not a defect in these tests; it is the boundary of what they can
// claim, and it is worth stating plainly because the green is otherwise easy to
// over-read. A far-future stamp accepted verbatim still APPLIES -- the upsert
// wins on freshness, the trigger renews the lease, the sweep runs -- so nothing
// here can tell a clamp from no clamp at all. What #3205 buys over pre-#3205 is
// ORDERING (the far-future stamp poisons every later honest event), and that is
// pinned elsewhere: TestStaleRoomEmptyDoesNotEvictARejoinedParticipant in
// skew_clamp_integration_test.go and
// TestVoiceLifecycleDispatcherStampsReceiptTimeAtEnqueue in
// lifecycle_dispatcher_test.go both go red under a drain-time clamp.
//
// Read this file as "#3205 does not reproduce #3201's CWE-863 failure", which is
// exactly what A9 asks for -- never as "the clamp is wired".

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clampHeartbeatInterval is the media plane's real publish cadence
// (services/media-plane/src/index.ts, "Publish room heartbeat every 30s")
// against presence.ActivityStateTTL's 90 s lease. Modelling the real interval
// rather than a whole lease window per round is what makes the episode a LIVE
// room losing its lease three heartbeats after the clock steps, rather than an
// already-expired lease being asked to resurrect.
const clampHeartbeatInterval = 30

// clampAdvanceObservedLease ages the database-observed lease by one heartbeat
// interval, RELATIVELY, so successive rounds accumulate. It deliberately does
// not name lifecycle_event_at, so the 000132/000133 trigger does not fire and
// the only thing that can renew the lease is a heartbeat the ingress accepted.
func clampAdvanceObservedLease(t *testing.T, ts *testhelpers.TestServer, channel, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`UPDATE voice_participants
		    SET lifecycle_observed_at =
		        lifecycle_observed_at - ($1::bigint * INTERVAL '1 second')
		  WHERE channel_id = $2 AND user_id = $3`,
		clampHeartbeatInterval, channel, userID)
	require.NoError(t, err)
}

// clampSkewEpisode drives the production shape of a producer-clock forward
// step: the room is live, the member never leaves, and the media plane keeps
// naming them in every heartbeat at its real cadence. Each round is one
// heartbeat interval of wall clock, then the heartbeat that should renew the
// lease, then the reconciler tick.
//
// Returns the round in which the live participant was reaped (-1 if never) and
// whether they were present at the end.
func clampSkewEpisode(
	t *testing.T,
	ts *testhelpers.TestServer,
	sub *voice.NATSSubscriber,
	channel, memberID string,
	skew time.Duration,
	rounds int,
) (reapedInRound int, presentAtEnd bool) {
	t.Helper()
	ctx := context.Background()
	reapedInRound = -1
	for round := 0; round < rounds; round++ {
		clampAdvanceObservedLease(t, ts, channel, memberID)
		// The producer's stamp generator is a monotonic ratchet, so the jumped
		// offset persists and each successive stamp is strictly greater. This
		// is the +round x 1 s the header warns about.
		stamp := time.Now().Add(skew + time.Duration(round)*time.Second).UTC()
		sub.HandleHeartbeat(leaseHeartbeatFrame(t, channel, []string{memberID}, stamp))
		removed, err := sub.ReconcileStaleServerVoiceParticipants(ctx, 10)
		require.NoError(t, err)
		if removed == 1 && reapedInRound < 0 {
			reapedInRound = round
		}
	}
	return reapedInRound, voiceParticipantExists(t, ts.DB, channel, memberID)
}

// clampLiveRoom builds a room with a participant already established by an
// honest heartbeat, which is the starting state every proof in this file needs.
func clampLiveRoom(
	t *testing.T,
	prefix string,
) (*testhelpers.TestServer, *voice.NATSSubscriber, string, string) {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, prefix+"-owner")
	server := ts.CreateTestServer(t, owner.ID, prefix+"-server")
	channel := ts.CreateVoiceChannel(t, server, prefix+"-channel")
	member := ts.CreateTestUser(t, prefix+"-member")
	ts.AddMemberToServer(t, server, member.ID, "member")

	sub.HandleHeartbeat(leaseHeartbeatFrame(t, channel, []string{member.ID}, time.Now().UTC()))
	require.True(t, voiceParticipantExists(t, ts.DB, channel, member.ID),
		"setup: the honest heartbeat must have created the roster row -- every "+
			"proof in this file is about what happens to a LIVE participant")
	sub.CompleteServerVoiceCleanupGraceForTest()
	return ts, sub, channel, member.ID
}

// TestClampKeepsMidSessionVoiceEnforcementUnderForwardSkew inverts
// TestRedteamPR3201_ForwardSkewSuppressesMidSessionVoiceEnforcement.
//
// Under #3201 this member stayed in the room, unmutable and undisconnectable,
// for as long as the producer's clock was wrong: AuthorizeJoin bars a timed-out
// member from JOINING, so the heartbeat-driven sweep is the ONLY thing that
// evicts one already inside. Under the clamp the frame is applied, the sweep
// runs, and the eviction happens on the skewed frame itself.
func TestClampKeepsMidSessionVoiceEnforcementUnderForwardSkew(t *testing.T) {
	r := setupEnforcerRig(t)

	owner := r.ts.CreateTestUser(t, "clamp3201enfowner")
	timedOut := r.ts.CreateTestUser(t, "clamp3201enftimedout")
	serverID := r.ts.CreateTestServer(t, owner.ID, "Clamp3201 Enforcement")
	r.ts.AddMemberToServer(t, serverID, timedOut.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "clamp3201-enforce-channel")
	r.addVoiceParticipant(t, channelID, timedOut.ID)

	// The moderator times the member out mid-session.
	_, err := r.ts.DB.Exec(`
		UPDATE server_members
		   SET timed_out_until = NOW() + INTERVAL '1 hour'
		 WHERE server_id = $1 AND user_id = $2
	`, serverID, timedOut.ID)
	require.NoError(t, err)

	sub := newTestSubscriber(r.ts)
	sub.SetPermissionEnforcer(r.enforcer)

	// The producer's clock has stepped far past the old bound. The media plane
	// is still reporting the timed-out member as present -- precisely the frame
	// the enforcement sweep exists to act on, and precisely the frame #3201
	// discarded.
	beyond := time.Now().
		Add(voice.MaxVoiceLifecycleForwardSkewForTest + 7*time.Minute).UTC()
	sub.HandleHeartbeat(leaseHeartbeatFrame(t, channelID, []string{timedOut.ID}, beyond))

	disconnect := waitPayloadFor(
		t, r.disc, "voice.enforce.disconnect", map[string]bool{timedOut.ID: true},
	)
	assert.Equal(t, channelID, disconnect["channelId"],
		"CWE-863 floor: a timed-out member must be force-disconnected even when "+
			"the producer clock is hours ahead. Under #3201 this frame was refused "+
			"at ingress and no voice.enforce.disconnect was ever published")
}

// TestClampedHeartbeatStillRequestsAPermissionSweep inverts
// TestRedteamPR3201_SkewRejectedHeartbeatNeverRequestsAPermissionSweep.
//
// This is the narrower, NATS-timing-independent half of the proof above: it
// asserts the sweep is REQUESTED at all. Everything downstream -- timeout
// eviction, ErrNotMember disconnect, permission-bitfield push -- inherits that
// structural fact, so it is the assertion that generalises.
func TestClampedHeartbeatStillRequestsAPermissionSweep(t *testing.T) {
	r := setupEnforcerRig(t)

	owner := r.ts.CreateTestUser(t, "clamp3201sweepowner")
	member := r.ts.CreateTestUser(t, "clamp3201sweepmember")
	serverID := r.ts.CreateTestServer(t, owner.ID, "Clamp3201 Sweep")
	r.ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "clamp3201-sweep-channel")
	r.addVoiceParticipant(t, channelID, member.ID)

	sub := newTestSubscriber(r.ts)
	sub.SetPermissionEnforcer(r.enforcer)

	beyond := time.Now().
		Add(voice.MaxVoiceLifecycleForwardSkewForTest + time.Minute).UTC()
	sub.HandleHeartbeat(leaseHeartbeatFrame(t, channelID, []string{member.ID}, beyond))

	payload := waitPayloadFor(
		t, r.perms, "voice.enforce.permissions", map[string]bool{member.ID: true},
	)
	assert.Equal(t, channelID, payload["channelId"],
		"the clamped heartbeat must still reach recheckServerHeartbeatPermissions; "+
			"under #3201 the sweep was never requested for a skewed frame, and every "+
			"enforcement path downstream inherited that")
}

// TestClampKeepsTheRosterUnderForwardSkew inverts
// TestRedteamPR3201_ForwardSkewStripsTheRosterAndNeverRestoresIt.
//
// Two things went wrong under #3201 and the second is the worse one. The stale
// reconciler reaped a participant the media plane was still reporting, and then
// every later heartbeat naming that same live participant was refused too, so
// nothing re-admitted them -- the durable roster stayed empty for the whole
// episode while the user held SFU transports. Under the clamp neither step
// occurs: each heartbeat is applied, the upsert advances lifecycle_event_at,
// the 000132/000133 trigger re-stamps lifecycle_observed_at, and the lease
// never ages out.
//
// The assertion tests reapedInRound against the -1 sentinel rather than a
// specific round, which is the repair for harness defect 1 in the header.
func TestClampKeepsTheRosterUnderForwardSkew(t *testing.T) {
	ts, sub, channel, memberID := clampLiveRoom(t, "clamp3201-beyond")

	beyond := voice.MaxVoiceLifecycleForwardSkewForTest + 7*time.Minute
	reapedInRound, presentAtEnd := clampSkewEpisode(
		t, ts, sub, channel, memberID, beyond, 6,
	)

	require.Equal(t, -1, reapedInRound,
		"a live participant must never be reaped while the media plane is still "+
			"naming them, however wrong the producer clock is. Under #3201 the reap "+
			"landed in round 2")
	assert.True(t, presentAtEnd,
		"and the roster must still hold them after six further skewed heartbeats -- "+
			"under #3201 every one of those was refused, so nothing re-admitted the "+
			"participant for the whole skew episode")
}

// TestClampMakesTheForwardSkewBoundaryContinuous inverts
// TestRedteamPR3201_TheSameEpisodeInsideTheBoundKeepsTheRoster, and is the
// strongest of the four because the inversion changes what the test can claim.
//
// Under #3201 that test was a CONTROL: it showed the roster surviving at 150 s
// while the test above showed it destroyed at 187 min, which is what made the
// harm a property of the BOUND rather than of clock skew -- discontinuous at
// the boundary, and worse on the rejecting side. Under the clamp there is no
// discontinuity left to demonstrate, so running both episodes in ONE test and
// asserting they agree is a stronger statement than either alone: the boundary
// no longer selects an outcome.
//
// Keep both halves. Asserting only the inside case would pass on a build with
// no clamp at all, since 150 s was already the surviving side under #3201.
func TestClampMakesTheForwardSkewBoundaryContinuous(t *testing.T) {
	// 150 s, not 179 s: clampSkewEpisode adds +round x 1 s, so a fixture within
	// 6 s of the 180 s boundary would cross it mid-episode and stop being an
	// inside-the-bound case at all. Harness defect 2 in the header.
	inside := voice.MaxVoiceLifecycleForwardSkewForTest - 30*time.Second
	beyond := voice.MaxVoiceLifecycleForwardSkewForTest + 7*time.Minute

	tsIn, subIn, channelIn, memberIn := clampLiveRoom(t, "clamp3201-cont-in")
	reapedInside, presentInside := clampSkewEpisode(
		t, tsIn, subIn, channelIn, memberIn, inside, 6,
	)

	tsOut, subOut, channelOut, memberOut := clampLiveRoom(t, "clamp3201-cont-out")
	reapedBeyond, presentBeyond := clampSkewEpisode(
		t, tsOut, subOut, channelOut, memberOut, beyond, 6,
	)

	assert.Equal(t, reapedInside, reapedBeyond,
		"the forward-skew bound must no longer select an outcome: an episode "+
			"inside it and one far beyond it reap identically. Under #3201 these "+
			"differed discontinuously at 180 s -- surviving at 179 s, destroyed and "+
			"unrecoverable at 181 s")
	assert.Equal(t, presentInside, presentBeyond,
		"and the participant's survival must not depend on which side of the bound "+
			"the producer's clock landed on")
	require.Equal(t, -1, reapedBeyond,
		"both sides must be the SURVIVING outcome, not merely equal -- two rooms "+
			"reaped identically would satisfy the equality above while proving the "+
			"opposite of what this test is for")
	assert.True(t, presentBeyond, "and the participant is still on the roster")
}

// TestClampedEnforcementSweepCarriesTheLiveParticipantSet is the residual the
// four inverted proofs do not cover on their own: they establish THAT the sweep
// runs, and this establishes that it runs against the set the media plane
// actually named. The mid-session sweep discovers participants via
// `SELECT vp.channel_id FROM voice_participants`, so a clamp that kept the
// frame but corrupted the roster would satisfy every assertion above and still
// sweep the wrong people.
func TestClampedEnforcementSweepCarriesTheLiveParticipantSet(t *testing.T) {
	r := setupEnforcerRig(t)

	owner := r.ts.CreateTestUser(t, "clamp3201setowner")
	stays := r.ts.CreateTestUser(t, "clamp3201setstays")
	serverID := r.ts.CreateTestServer(t, owner.ID, "Clamp3201 Set")
	r.ts.AddMemberToServer(t, serverID, stays.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "clamp3201-set-channel")
	r.addVoiceParticipant(t, channelID, stays.ID)

	sub := newTestSubscriber(r.ts)
	sub.SetPermissionEnforcer(r.enforcer)

	beyond := time.Now().
		Add(voice.MaxVoiceLifecycleForwardSkewForTest + 12*time.Minute).UTC()
	sub.HandleHeartbeat(leaseHeartbeatFrame(t, channelID, []string{stays.ID}, beyond))

	payload := waitPayloadFor(
		t, r.perms, "voice.enforce.permissions", map[string]bool{stays.ID: true},
	)
	require.Equal(t, channelID, payload["channelId"])
	require.True(t, voiceParticipantExists(t, r.ts.DB, channelID, stays.ID),
		"the clamped frame must leave the durable roster row the sweep reads from; "+
			"without it recheckServerHeartbeatPermissions has nothing to discover "+
			"and enforcement is silently off for this room")

	// The enforcer is live independently of the clamped path, so a green above
	// cannot be an artefact of an unwired rig.
	r.enforcer.RecheckParticipants(serverID, channelID, []uuid.UUID{uuid.MustParse(stays.ID)})
	direct := waitPayloadFor(
		t, r.perms, "voice.enforce.permissions", map[string]bool{stays.ID: true},
	)
	assert.Equal(t, channelID, direct["channelId"],
		"control: the enforcer wired into this subscriber is functional")
}
