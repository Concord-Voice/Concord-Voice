package websocket

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// epochOf and openOf mirror the packing so each test asserts on both halves
// independently. A helper that read only the low half would hide the exact bug
// this representation is most exposed to: a decrement that borrows out of the
// count and into the epoch.
func epochOf(state uint64) uint64 { return state >> presenceAuthzEpochShift }
func openOf(state uint64) uint64  { return state & presenceAuthzOpenMask }

func TestBeginAudienceRevocationBracketsBothHalves(t *testing.T) {
	h := &Hub{}

	require.Equal(t, uint64(0), openOf(h.presenceAuthzState.Load()))

	closer := h.BeginAudienceRevocation()
	state := h.presenceAuthzState.Load()
	assert.Equal(t, uint64(1), epochOf(state),
		"opening a revocation must bump the epoch, so a query starting AFTER the open is caught by the epoch arm")
	assert.Equal(t, uint64(1), openOf(state),
		"opening a revocation must raise openCount, so a query starting BEFORE the open is caught by the level arm")

	closer()
	state = h.presenceAuthzState.Load()
	assert.Equal(t, uint64(1), epochOf(state), "closing must NOT rewind the epoch")
	assert.Equal(t, uint64(0), openOf(state), "closing must return openCount to zero")
}

// Double-close is what makes the packed representation safe. Add(^uint64(0)) is
// -1 on the whole 64-bit word, so an unbalanced close borrows out of the epoch
// half -- rewinding the epoch AND setting openCount to 0xFFFFFFFF, which would
// suppress base presence hub-wide forever. sync.Once is the structural
// guarantee; this test is what proves it is actually wired.
func TestBeginAudienceRevocationCloserIsIdempotent(t *testing.T) {
	h := &Hub{}
	closer := h.BeginAudienceRevocation()
	closer()
	before := h.presenceAuthzState.Load()

	closer()
	closer()

	after := h.presenceAuthzState.Load()
	assert.Equal(t, before, after,
		"a repeated close must be inert: a second decrement would borrow from the epoch half")
	assert.Equal(t, uint64(1), epochOf(after), "the epoch must not have been rewound by the extra closes")
	assert.Equal(t, uint64(0), openOf(after), "openCount must not have underflowed to 0xFFFFFFFF")
}

// Concurrent brackets must nest, not clobber. The count is what makes the fence
// level-triggered, so two overlapping revocations must keep it raised until BOTH
// have closed -- a boolean flag would pass every other test in this file and
// fail this one.
func TestBeginAudienceRevocationNestsConcurrentBrackets(t *testing.T) {
	h := &Hub{}
	first := h.BeginAudienceRevocation()
	second := h.BeginAudienceRevocation()

	assert.Equal(t, uint64(2), openOf(h.presenceAuthzState.Load()))

	first()
	assert.Equal(t, uint64(1), openOf(h.presenceAuthzState.Load()),
		"one revocation still in flight must keep the fence raised")

	second()
	assert.Equal(t, uint64(0), openOf(h.presenceAuthzState.Load()))
}

// The level arm, and the whole point of #2992: a result read while a revocation
// was in flight is unproven even though its epoch still matches.
func TestPresenceAudienceUnprovenWhenRevocationWasInFlightAtRead(t *testing.T) {
	h := &Hub{}
	closer := h.BeginAudienceRevocation()

	result := presenceAudienceResult{
		audience:   map[uuid.UUID]bool{uuid.New(): true},
		authzState: h.presenceAuthzState.Load(),
		computedAt: time.Now(),
	}

	// Positive control. The epoch half MATCHES, so the pre-#2992 fence would have
	// called this proven. Without this assertion the check below could pass
	// because the epoch arm caught it, leaving the level arm untested.
	require.Equal(t, epochOf(result.authzState), epochOf(h.presenceAuthzState.Load()),
		"the epoch arm must NOT be what catches this -- otherwise the level arm is untested")

	assert.True(t, h.presenceAudienceUnproven(result),
		"a revocation open at read time makes the audience unproven regardless of the epoch")

	closer()
	assert.True(t, h.presenceAudienceUnproven(result),
		"the result was READ under an open revocation; closing it later does not retroactively prove the audience")
}

// The complement: with no revocation in flight and no epoch movement, a result
// must still be PROVEN. Without this, making presenceAudienceUnproven return
// true unconditionally would pass every other assertion in this file.
func TestPresenceAudienceProvenWhenNoRevocationTouchedTheWindow(t *testing.T) {
	h := &Hub{}
	result := presenceAudienceResult{
		audience:   map[uuid.UUID]bool{uuid.New(): true},
		authzState: h.presenceAuthzState.Load(),
		computedAt: time.Now(),
	}

	assert.False(t, h.presenceAudienceUnproven(result),
		"an untouched window must remain deliverable, or the fence is a permanent blackout")
}

// The nil-receiver guard closes an asymmetry the sibling guard cannot reach.
// BeginAuthzRevocation takes a CONCRETE *Hub so a typed-nil is caught, but
// graphpresence reaches the method through the Disconnector INTERFACE, where a
// typed-nil *Hub satisfies `!= nil`. Without a receiver guard, that call panics.
//
// Not reachable from today's router — websocket.NewHub never returns nil — so
// this locks a robustness property, not a live vulnerability. It is here because
// the two nil-tolerant Disconnector uses beside it guard explicitly, and an
// undefended third shape is what a later refactor walks into. (@red-team.)
func TestBeginAudienceRevocationToleratesANilReceiver(t *testing.T) {
	var nilHub *Hub

	var closer func()
	assert.NotPanics(t, func() { closer = nilHub.BeginAudienceRevocation() },
		"a typed-nil *Hub reached through an interface must not panic")
	require.NotNil(t, closer, "the closer must be callable even from a nil receiver")
	assert.NotPanics(t, func() { closer() })
}

// The watchdog's reporting rule (#2992 R1). Tested through the pure tick helper
// rather than the ticker loop, so it needs no wall clock and cannot flake.
func TestPresenceAuthzWatchdogReportsOncePerEpisode(t *testing.T) {
	consecutive := 0
	reports := 0
	// Ten consecutive raised observations -- well past the threshold.
	for i := 0; i < 10; i++ {
		var report bool
		consecutive, report = presenceAuthzWatchdogTick(true, consecutive)
		if report {
			reports++
		}
	}
	assert.Equal(t, 1, reports,
		"a sustained episode must emit ONCE, not once per tick -- a per-tick watchdog "+
			"is its own amplification primitive")
	assert.Equal(t, 10, consecutive, "the counter must keep climbing after it reported")
}

func TestPresenceAuthzWatchdogStaysSilentBelowTheThreshold(t *testing.T) {
	consecutive := 0
	for i := 0; i < presenceAuthzWatchdogEpisodes-1; i++ {
		var report bool
		consecutive, report = presenceAuthzWatchdogTick(true, consecutive)
		require.False(t, report,
			"an ordinary revoking transaction spanning a tick or two must not page anyone")
	}
}

// A cleared fence must RESET the counter, or an episode assembled from
// unrelated brief holds minutes apart would eventually report a blackout that
// never happened.
func TestPresenceAuthzWatchdogResetsWhenTheFenceClears(t *testing.T) {
	consecutive := 0
	for i := 0; i < presenceAuthzWatchdogEpisodes-1; i++ {
		consecutive, _ = presenceAuthzWatchdogTick(true, consecutive)
	}
	require.Equal(t, presenceAuthzWatchdogEpisodes-1, consecutive)

	consecutive, report := presenceAuthzWatchdogTick(false, consecutive)
	assert.Zero(t, consecutive, "a cleared fence must reset the episode counter")
	assert.False(t, report)

	// And the next raised tick starts a fresh episode rather than tripping.
	_, report = presenceAuthzWatchdogTick(true, consecutive)
	assert.False(t, report, "the episode must restart from zero, not resume")
}

// The watchdog must never write the fence. A reset would be a fail-open timer on
// an authorization control -- it would resume delivering presence to viewers the
// fence had not cleared. This asserts the observable consequence: driving the
// tick helper cannot move either half.
func TestPresenceAuthzWatchdogNeverWritesTheFence(t *testing.T) {
	h := &Hub{}
	closer := h.BeginAudienceRevocation()
	before := h.presenceAuthzState.Load()

	consecutive := 0
	for i := 0; i < presenceAuthzWatchdogEpisodes*3; i++ {
		consecutive, _ = presenceAuthzWatchdogTick(
			h.presenceAuthzState.Load()&presenceAuthzOpenMask != 0, consecutive)
	}

	assert.Equal(t, before, h.presenceAuthzState.Load(),
		"the watchdog observes and never writes: a reset here would be a fail-open timer")
	assert.NotZero(t, openOf(h.presenceAuthzState.Load()),
		"the fence must still be raised -- only the closer may lower it")
	closer()
}
