package voice

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/ingressbudget"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Issue #2854 stage B1 — ingress admission control on the erasure door.
//
// These share the harness in nats_erasure_clear_test.go (erasureClearObserver,
// newErasureClearSubscriber, withTestDB), which observes BOTH sinks the handler
// could reach. That two-sink shape exists because a one-sink version of these
// tests went vacuous in PR #2840, and the same hazard applies here in a new
// form — see the vacuity note on the first test.

// dropPoisonUserSQL removes the fixture account. Used twice in the dedup
// pre-poison test: once as cleanup, once as the erasure the test simulates.
const dropPoisonUserSQL = `DELETE FROM users WHERE id = $1`

// gatedErasureSubscriber wires the ingress gates as NewNATSSubscriber does in
// production. newErasureClearSubscriber deliberately leaves them nil, so the
// pre-existing tests keep exercising the unwired no-op path.
func gatedErasureSubscriber(o *erasureClearObserver) *NATSSubscriber {
	// Freeze the budget clock so refill cannot fire mid-loop. Each admitted
	// clear does a DB round-trip, so a wall-clock loop can span a refill
	// boundary and admit one extra -- a flake, not a finding.
	//
	// Assigned to THIS subscriber, never through a package global: a global
	// outlives the test that set it and freezes every later test's budget.
	frozen := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	s := newErasureClearSubscriber(o)
	s.erasureBudget = ingressbudget.NewBucketWithClock(
		erasureBudgetRefill, erasureBudgetBurst, func() time.Time { return frozen })
	s.erasureSeen = newErasureSeen()
	return s
}

// Regression lock (a).
//
// VACUITY NOTE, do not delete: asserting "zero clients disconnected" ALONE is
// already green on the pre-fix tree, because #2840 replaced the fleet
// disconnect with a targeted clear and #2855 made the fan-out non-closing. A
// test written only to the issue's headline criterion cannot fail on the code
// it targets. The non-vacuous dimension is ADMITTED COUNT.
func TestForgedDistinctUUIDBurstIsBudgetBounded(t *testing.T) {
	var o erasureClearObserver
	s := gatedErasureSubscriber(&o)
	s.db = withTestDB(t)

	for range 1000 {
		s.handlePresenceErasureCleared(
			[]byte(`{"user_id":"` + uuid.New().String() + `"}`))
	}

	require.Zero(t, o.disconnects, "no forged message may reach a fleet disconnect")
	require.LessOrEqual(t, len(o.cleared), erasureBudgetBurst,
		"1000 forged messages must not buy 1000 fan-outs; the burst plus a small "+
			"refill allowance is the ceiling")
	require.NotEmpty(t, o.cleared, "the gate must throttle the path, not silence it")
}

// Regression lock (b): a replay costs no database queries, which is precisely
// what putting dedup ahead of the existence check buys.
func TestReplayOfOneUUIDCostsAtMostOneExistenceQuery(t *testing.T) {
	var o erasureClearObserver
	s := gatedErasureSubscriber(&o)
	s.db = withTestDB(t)

	queries := 0
	s.erasureExistenceProbedHook = func() { queries++ }

	payload := []byte(`{"user_id":"` + uuid.New().String() + `"}`)
	for range 1000 {
		s.handlePresenceErasureCleared(payload)
	}

	require.Equal(t, 1, queries,
		"dedup must sit AHEAD of the existence check, so a replay costs zero queries")
	require.Len(t, o.cleared, 1, "and reaches the sink exactly once")
}

// Regression lock (c) — the most important test in this change. It is what makes
// dedup-before-budget load-bearing rather than cosmetic: with the order
// reversed the flood consumes the budget and starves this genuine clear.
func TestAFloodOfOneUUIDDoesNotStarveAGenuineClearForAnother(t *testing.T) {
	var o erasureClearObserver
	s := gatedErasureSubscriber(&o)
	s.db = withTestDB(t)

	flooded := []byte(`{"user_id":"` + uuid.New().String() + `"}`)
	for range 1000 {
		s.handlePresenceErasureCleared(flooded)
	}

	genuine := uuid.New()
	s.handlePresenceErasureCleared([]byte(`{"user_id":"` + genuine.String() + `"}`))

	require.Contains(t, o.cleared, genuine,
		"a replay flood must not consume the budget a genuine clear needs")
}

// Regression lock (d): mark-on-accept-only. A forged clear naming a user who
// STILL EXISTS is rejected at the existence check, and must not occupy that
// user's dedup slot -- otherwise an attacker pre-poisons the window and
// suppresses the genuine clear issued after the real erasure.
func TestARejectedClearDoesNotPoisonTheDedupSlot(t *testing.T) {
	db := withTestDB(t)
	live := uuid.New()
	_, err := db.Exec(`
		INSERT INTO users (id, username, email, password_hash, created_at, updated_at)
		VALUES ($1, $2, $3, 'x', NOW(), NOW())`,
		live, "erasurededuppoison_"+live.String()[:8], live.String()+"@example.test")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(dropPoisonUserSQL, live) })

	var o erasureClearObserver
	s := gatedErasureSubscriber(&o)
	s.db = db

	payload := []byte(`{"user_id":"` + live.String() + `"}`)
	s.handlePresenceErasureCleared(payload)
	require.Empty(t, o.cleared,
		"precondition: a still-existing user's clear is rejected")

	// Now the real erasure happens; the genuine clear must still be admitted.
	_, err = db.Exec(dropPoisonUserSQL, live)
	require.NoError(t, err)
	s.handlePresenceErasureCleared(payload)

	require.Equal(t, []uuid.UUID{live}, o.cleared,
		"a rejected clear must not have marked the dedup slot")
}

// The gates are optional by type (a nil gate is a no-op) so struct-literal test
// subscribers keep working and so a wiring bug cannot deny traffic on a
// right-to-erasure path. That makes a production wiring bug SILENT, so lock the
// production construction path instead.
func TestNewNATSSubscriberWiresTheIngressGates(t *testing.T) {
	s := NewNATSSubscriber(nil, logger.New("test"), nil, nil, nil, nil, nil)

	require.NotNil(t, s.erasureBudget, "an unwired budget silently removes the cap")
	require.NotNil(t, s.erasureSeen, "an unwired window silently removes dedup")
	require.NotNil(t, s.voiceRoomBudget, "an unwired per-room budget removes G2")
}

// Regression lock on adversarial finding C6: an UNVERIFIED accept must not
// occupy the dedup slot.
//
// The existence probe fails OPEN on a read error and proceeds -- deliberate, and
// unchanged, because on a revocation path acting is the conservative direction.
// But proceeding is not the same as having established anything. Marking there
// let a forged clear naming a LIVE user, sent during a database blip, occupy
// that user's slot and suppress their genuine clear after the real erasure
// minutes later. That is the mark-on-accept-only hazard reached through the
// ACCEPT arm rather than a reject arm.
func TestAnUnverifiedAcceptDoesNotOccupyTheDedupSlot(t *testing.T) {
	var o erasureClearObserver
	s := gatedErasureSubscriber(&o)

	// Reuse the package's single fault-injection helper rather than a second
	// copy: two copies of an open-then-close helper drift, and this one dropped
	// t.Helper() so a failure inside it reported the helper's line, not the
	// caller's (CodeRabbit, PR #2871).
	s.db = brokenLookupDB(t)

	victim := uuid.New()
	payload := []byte(`{"user_id":"` + victim.String() + `"}`)
	s.handlePresenceErasureCleared(payload)
	require.Len(t, o.cleared, 1,
		"precondition: the unverified arm still proceeds, which is deliberate")

	// The database recovers and the victim is genuinely erased. Their real clear
	// must NOT be deduped away by the forged one.
	s.db = withTestDB(t)
	s.handlePresenceErasureCleared(payload)

	require.Len(t, o.cleared, 2,
		"a genuine clear must survive a forged unverified accept for the same id")
}

// The aggregation invariant is guarded at TWO call sites -- erasureShed and
// voiceShed -- and a test that exercises only one is bounded to that path.
// ingress_gate_test.go covers the voice door; this covers the erasure door,
// which is where finding C3 actually lived.
//
// C3: the malformed-payload and invalid-identifier arms logged once per message.
// They sit ahead of the dedup and budget gates by necessity, having no key to
// work with until the payload parses, so the per-message log was reachable by
// simply sending invalid JSON -- and a gate that logs once per rejection IS the
// amplification primitive it exists to remove. Reverting that fix leaves every
// voice-door test green, which is why this one has to exist separately.
func TestErasureDoorShedsAreReportedAggregatedNotPerMessage(t *testing.T) {
	var o erasureClearObserver
	s := gatedErasureSubscriber(&o)

	classes := map[string]int{}
	reports := 0
	s.ingressShedObservedHook = func(c string) { classes[c]++ }
	s.ingressShedLoggedHook = func() { reports++ }

	for range 10_000 {
		s.handlePresenceErasureCleared([]byte(`{ not json`))
	}
	for range 10_000 {
		s.handlePresenceErasureCleared([]byte(`{"user_id":"not-a-uuid"}`))
	}

	// Assert the CLASSES, not merely the total: these two arms carry different
	// closed-vocabulary values and a swap between them was previously invisible.
	require.Equal(t, map[string]int{"malformed_payload": 10_000, "invalid_user": 10_000}, classes,
		"unparseable JSON and an unparseable id are DIFFERENT closed-vocabulary "+
			"classes, and every message must reach the reporter rather than a "+
			"bare per-message log")
	require.LessOrEqual(t, reports, 2,
		"20000 sheds must not buy 20000 log writes -- these two arms sit AHEAD "+
			"of the budget, so an attacker reaches them with invalid JSON alone")
	require.Empty(t, o.cleared, "and none of it reaches the fan-out")
}
