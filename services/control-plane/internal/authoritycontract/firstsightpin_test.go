package authoritycontract

// RT-2 (#3587 PR-A2 red-team): VerifyBundle from the genesis head has no
// AccountMemory, so its seq-0 hypothesis is A0 even for an account the member
// pinned at first sight, and an RSK + CP forgery of row 1 (enroll_new)
// verifies there. VerifyChain refuses the same input (its seq-0 hypothesis
// carries AccountMemory.pin, so the from-state is Ap and row 1, From = A0, is
// refused). VerifyStatus is where the two meet: a status whose head pin is not
// the member's first-sight pin is pin_changed in every state (D-149, DoR:943,
// :2107: an RSK plus a CP may substitute a pin only for a client with no
// first-sight pin, or after an acceptance gesture), so no wrap reaches the
// forged EK. An honest chain never trips it: checkPost keeps the pin
// immutable from seq 0, and row 1 never runs on a population account.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyStatusRefusesAPinOtherThanFirstSight(t *testing.T) {
	c := newForgeChain(t)
	genesis := GenesisHead(c.realm, c.account)

	// First sight: the member saw the account absent with its legacy pin P
	// (D-149) and stored it: the honest pre-enrolment state of every
	// population account.
	pin := Digest(forgedEK(t))
	s0 := c.status(Status{Seq: 0, State: StateAbsent, LegacyEKDigest: pin, AsOfMs: testT0})
	vs0, _, r := VerifyStatus(c.tr, HighWater{}, genesis, s0, AccountMemory{}, nowOf(t, testT0), UseNewSecret)
	require.Equal(t, ReasonOK, r)
	require.Equal(t, pin, vs0.Pin)
	mem := AccountMemory{Pin: vs0.Pin} // what the caller persists

	// The forgery (RSK + CAK): row 1 enroll_new for that same account, an
	// attacker root and EK, no pin, no PoP.
	evilEK := forgedEK(t)
	issued := testT0 + 60_000
	b1, _, _, s1 := c.enrollNew(testKey(t), evilEK, issued)
	asOf := issued + 1_000

	// Control: VerifyChain holds the seq-0 pin and refuses row 1.
	_, _, _, r = VerifyChain(c.tr, HighWater{}, genesis, ChainInput{Bundles: [][]byte{b1}, Status: s1},
		mem, nowOf(t, asOf), UseNewSecret)
	require.Equal(t, ReasonTransitionInvalid, r, "control: VerifyChain refuses row 1 for a pinned account")

	// The same bundle through VerifyBundle, which has no AccountMemory, then
	// the status through VerifyStatus with the member's memory.
	head1, r := VerifyBundle(c.tr, HighWater{}, genesis, b1, nil)
	require.Equal(t, ReasonOK, r, "VerifyBundle's genesis is A0 (V3-05): it cannot see the pin")
	vs, _, r := VerifyStatus(c.tr, HighWater{}, head1, s1, mem, nowOf(t, asOf), UseNewSecret)
	spki, ok := vs.WrapKey(nil)
	if ok {
		t.Logf("REGRESSION: VerifyStatus=%s; WrapKey returns the attacker EK=%v", r, string(spki) == string(evilEK))
	}
	require.Equal(t, ReasonPinChanged, r, "D-149/DoR:943: a chain whose pin is not the first-sight pin")
	require.False(t, ok, "DoR:753/:943: a member with a first-sight pin must not wrap to a row-1 EK the pin never saw")

	// Without a first-sight pin there is nothing to compare: the caller then
	// stores the status pin, "" here.
	vs, _, r = VerifyStatus(c.tr, HighWater{}, head1, s1, AccountMemory{}, nowOf(t, asOf), UseNewSecret)
	require.Equal(t, ReasonOK, r)
	spki, ok = vs.WrapKey(nil)
	require.True(t, ok)
	require.Equal(t, evilEK, spki)
}

// The check covers every state, not only active (RT-2): a pending or
// suspended head of a chain verified under another seq-0 hypothesis is
// pin_changed too, held or not (NEW-2: a hold carries no notice, so it must not
// hide the substitution).
func TestStateVerdictComparesTheFirstSightPinInEveryState(t *testing.T) {
	pin, other := d48("pin"), d48("other")
	mem := AccountMemory{Pin: other}
	for _, eff := range []pub{
		{State: StateActive, Pin: pin},
		{State: StateSuspended, Pin: pin},
		{State: StateSuspended, RootRevoked: 1, Pin: pin},
		{State: StateDisputed, Pin: pin},
		{State: StateErased, Pin: pin},
		{State: StatePending, Prior: StateActive, PendRoot: testPoint(t), Pin: pin},
		{State: StatePending, Prior: StateAbsent, PendRoot: testPoint(t), Assurance: AssuranceContinuity, Pin: pin},
		{State: StatePending, Pin: pin}, // a pending erase
		{State: StateActive},            // a row-1 chain: pin ""
	} {
		var vs VerifiedStatus
		require.Equal(t, ReasonPinChanged, stateVerdict(eff, Status{}, mem, Delegation{}, UseNewSecret, &vs), "%+v", eff)
		require.Equal(t, ReasonPinChanged, stateVerdict(eff, Status{Held: 1}, mem, Delegation{}, UseNewSecret, &vs),
			"a hold does not hide it (NEW-2): %+v", eff)
		eff.Pin = other
		require.NotEqual(t, ReasonPinChanged, stateVerdict(eff, Status{}, mem, Delegation{}, UseNewSecret, &vs),
			"the first-sight pin itself: %+v", eff)
		require.Equal(t, ReasonHeld, stateVerdict(eff, Status{Held: 1}, mem, Delegation{}, UseNewSecret, &vs),
			"control: held, with the first-sight pin: %+v", eff)
	}
	// The absent path is absentVerdict's own, unchanged (LD-28's accepted pin).
	var vs VerifiedStatus
	r := stateVerdict(pub{State: StateAbsent}, Status{LegacyEKDigest: pin}, AccountMemory{Pin: other, AcceptedPin: pin},
		Delegation{}, UseNewSecret, &vs)
	require.Equal(t, ReasonOKAcceptedPin, r)
}

// NEW-2 on the absent path: a held absent status serving a changed pin is
// pin_changed too; with the first-sight pin it is held.
func TestHeldAbsentStatusStillReportsAChangedPin(t *testing.T) {
	pin, other := d48("pin"), d48("other")
	eff, mem := pub{State: StateAbsent}, AccountMemory{Pin: pin}
	var vs VerifiedStatus
	require.Equal(t, ReasonPinChanged,
		stateVerdict(eff, Status{Held: 1, LegacyEKDigest: other}, mem, Delegation{}, UseNewSecret, &vs))
	require.Equal(t, ReasonHeld,
		stateVerdict(eff, Status{Held: 1, LegacyEKDigest: pin}, mem, Delegation{}, UseNewSecret, &vs), "control")
	require.False(t, vs.AcceptedPin, "the held arm's probe leaves vs untouched")
}
