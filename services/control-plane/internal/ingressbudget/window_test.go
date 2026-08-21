package ingressbudget

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWindowSeenOnlyAfterMark(t *testing.T) {
	clock := newTestClock()
	w := newWindowAt(64, 10*time.Minute, clock.now)

	require.False(t, w.Seen("a"), "unmarked keys are not seen")
	w.Mark("a")
	require.True(t, w.Seen("a"))
	require.False(t, w.Seen("b"), "marking a must not affect b")
}

// Seen must NOT mark. This is the type-level guarantee that makes
// mark-on-accept-only expressible; a combined SeenOrMark would lose it.
func TestSeenDoesNotMark(t *testing.T) {
	clock := newTestClock()
	w := newWindowAt(64, 10*time.Minute, clock.now)

	require.False(t, w.Seen("a"))
	require.False(t, w.Seen("a"), "a check is not a mark; a is still unseen")
}

func TestWindowForgetsAfterTwoTTLs(t *testing.T) {
	clock := newTestClock()
	w := newWindowAt(64, 10*time.Minute, clock.now)

	w.Mark("a")
	clock.advance(11 * time.Minute)
	w.Mark("rotation-trigger")
	require.True(t, w.Seen("a"), "one rotation: a is in the previous generation")

	clock.advance(11 * time.Minute)
	w.Mark("rotation-trigger-2")
	require.False(t, w.Seen("a"), "two rotations: a has aged out")
}

// Capacity is the safety property; the TTL is only freshness. An attacker
// minting distinct UUIDs must not grow this set without limit.
func TestWindowRotatesAtCapacityBeforeTTL(t *testing.T) {
	clock := newTestClock()
	const capacity = 8
	w := newWindowAt(capacity, time.Hour, clock.now)

	for i := range capacity * 4 {
		w.Mark("id-" + strconv.Itoa(i))
	}
	require.LessOrEqual(t, w.residentLen(), capacity*2,
		"two generations bound the set at 2x capacity even with no TTL elapse")
}

func TestNilWindowIsInert(t *testing.T) {
	var w *Window
	require.False(t, w.Seen("x"))
	require.NotPanics(t, func() { w.Mark("x") })
}

// Locks the ACTUAL rotation contract, which is mark-driven rather than
// time-driven: with no Mark traffic, an idle window never rotates and a key
// stays Seen past any number of TTL intervals.
//
// This is the safe direction -- a stale entry only sheds a replay for a key
// already marked, and can never admit anything -- but the doc comment used to
// claim rotation happened "at least every ttl", which was false for an idle
// window (CodeRabbit, PR #2871). Asserting the real behaviour stops the claim
// drifting back.
func TestAnIdleWindowNeverRotatesBecauseSeenDoesNotMutate(t *testing.T) {
	clock := newTestClock()
	w := newWindowAt(64, 10*time.Minute, clock.now)

	w.Mark("a")
	require.True(t, w.Seen("a"))

	// Far past two TTLs, with reads but no writes.
	for range 20 {
		clock.advance(time.Hour)
		require.True(t, w.Seen("a"),
			"Seen is a pure read: it must not rotate the window, so the key persists")
	}

	// A single Mark is what advances the generations.
	w.Mark("trigger-1")
	clock.advance(11 * time.Minute)
	w.Mark("trigger-2")
	require.False(t, w.Seen("a"), "two mark-driven rotations age the key out")
}
