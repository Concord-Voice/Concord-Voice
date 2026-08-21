package voice

// Inverted red-team regression locks for issue #2854 stage B1.
//
// PROVENANCE. These are the INVERSION of a red-team proof-of-concept preserved
// verbatim in a comment on issue #2854 (poc_redteam_erasure_flood_test.go, marked
// NOT FOR MERGE and deliberately not copied into the repo). The PoC asserts the
// exploit WORKS; each test here asserts the same primitive is now denied.
//
// FALSIFICATION RECORD. The PoC was executed against the pre-fix tree at commit
// 29e332cd9 and ALL THREE of its assertions PASSED: 257 forged distinct-UUID
// messages each reached the broadcast sink, replaying ONE captured message 257
// times reached that sink 257 times, and a broken database lookup let a clear
// through for a live account. The exploit was live, not theoretical, so these
// inversions lock a demonstrated class rather than a hypothesis.
//
// VACUITY WARNING, DO NOT DELETE. The issue's headline criterion -- "a forged
// burst disconnects ZERO clients" -- is ALREADY GREEN on the pre-fix tree:
// #2840 replaced the fleet-wide disconnect with a targeted clear, and #2855 made
// the fan-out non-closing. A test asserting only the disconnect count therefore
// cannot fail on the code it targets; it would pass identically with every gate
// B1 adds deleted. Every assertion below is on ADMITTED COUNT or QUERY COUNT --
// the dimension B1 actually changes. The disconnect counter is asserted only as
// a secondary guard against a regression in the OTHER direction, never as the
// property under test.

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// erasureFloodBurst is the PoC's number: the production client Send capacity
// plus one, i.e. the smallest burst that overflows a Rich Presence client's
// outbound queue once fanned out.
const erasureFloodBurst = 257

// forgedErasureClear builds the exact wire payload the PoC forges: a well-formed
// envelope naming a UUID that was never an account.
func forgedErasureClear(id uuid.UUID) []byte {
	return []byte(`{"user_id":"` + id.String() + `"}`)
}

// brokenLookupDB returns a handle whose every query errors, by opening and
// immediately closing it. That is the PoC's mechanism for reaching the
// lookup-error arm without degrading the shared test database.
func brokenLookupDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return db
}

// Inversion of PoC assertion 1.
//
// Pre-fix: 257 forged distinct-UUID messages reached the sink 257 times -- one
// fan-out bought per message, at no cost to the attacker. Distinct UUIDs are
// what make this the dedup window's blind spot, so the budget is the only gate
// that can answer it.
func TestForgedErasureBurstNoLongerBuysOneFanOutPerMessage(t *testing.T) {
	var o erasureClearObserver
	s := gatedErasureSubscriber(&o)
	s.db = withTestDB(t)

	for range erasureFloodBurst {
		s.handlePresenceErasureCleared(forgedErasureClear(uuid.New()))
	}

	require.Less(t, len(o.cleared), erasureFloodBurst,
		"PoC assertion 1 inverted: %d forged messages must no longer buy %d fan-outs",
		erasureFloodBurst, erasureFloodBurst)
	require.LessOrEqual(t, len(o.cleared), erasureBudgetBurst,
		"the burst plus a small refill allowance is the ceiling")
	require.NotEmpty(t, o.cleared,
		"the gate must throttle the path, not silence it -- a genuine clear still lands")
	require.Zero(t, o.disconnects,
		"secondary guard only; see the VACUITY WARNING at the top of this file")
}

// Inversion of PoC assertion 2.
//
// Pre-fix: the handler kept no seen-set, so ONE captured frame was an infinite
// ammunition supply -- 257 replays reached the sink 257 times. Exactly-once is
// the assertion, not merely "bounded": dedup sits ahead of the budget, so a
// replay must cost neither a fan-out nor a budget token after the first.
func TestReplayedErasureClearReachesTheSinkExactlyOnce(t *testing.T) {
	var o erasureClearObserver
	s := gatedErasureSubscriber(&o)
	s.db = withTestDB(t)

	captured := forgedErasureClear(uuid.New())
	for range erasureFloodBurst {
		s.handlePresenceErasureCleared(captured)
	}

	require.Len(t, o.cleared, 1,
		"PoC assertion 2 inverted: replaying one captured message %d times must "+
			"reach the sink exactly once", erasureFloodBurst)
	require.Zero(t, o.disconnects,
		"secondary guard only; see the VACUITY WARNING at the top of this file")
}

// PoC assertion 3 is DELIBERATELY NOT INVERTED. The lookup-error arm still
// PROCEEDS with the clear, and that remains correct.
//
// On a revocation path the conservative direction is to ACT. Per
// [internal]rules/backend.md, unknown state fails closed but proven no-change must
// not -- and converting this arm to refuse would manufacture a right-to-erasure
// RESIDUAL: a transient database blip silently dropping a genuine clear, with no
// TTL to recover it, because Custom Status carries no TTL and converges only at
// the viewer's next presence_snapshot.
//
// What stage B1 changed is not the POSTURE but its JUSTIFICATION. The old
// comment held the arm affordable because the action is inert on a false claim.
// Post-#2855 that is true PER CLIENT and false IN AGGREGATE: every admitted
// clear costs O(Rich Presence clients) enqueues, so an attacker who can degrade
// the database was buying that fan-out per message. The budget is what makes the
// arm affordable now, so this test asserts both halves together -- proceed, AND
// bounded.
func TestErasureClearStillProceedsOnALookupFailureButIsBudgetBounded(t *testing.T) {
	t.Run("a single clear still proceeds when the lookup fails", func(t *testing.T) {
		var o erasureClearObserver
		s := gatedErasureSubscriber(&o)
		s.db = brokenLookupDB(t)

		erased := uuid.New()
		s.handlePresenceErasureCleared(forgedErasureClear(erased))

		require.Equal(t, []uuid.UUID{erased}, o.cleared,
			"a database fault must not suppress a legitimate erasure clear -- "+
				"refusing here manufactures an unrecoverable privacy residual")
	})

	t.Run("but the arm is no longer an unmetered fan-out primitive", func(t *testing.T) {
		var o erasureClearObserver
		s := gatedErasureSubscriber(&o)
		s.db = brokenLookupDB(t)

		for range erasureFloodBurst {
			s.handlePresenceErasureCleared(forgedErasureClear(uuid.New()))
		}

		require.Less(t, len(o.cleared), erasureFloodBurst,
			"degrading the database must no longer buy one fan-out per message")
		require.LessOrEqual(t, len(o.cleared), erasureBudgetBurst,
			"the budget bounds the fail-open arm exactly as it bounds the normal one")
		require.NotEmpty(t, o.cleared, "while the proceed posture is retained")
		require.Zero(t, o.disconnects,
			"secondary guard only; see the VACUITY WARNING at the top of this file")
	})
}
