package authoritycontract

// ReplayAuthorityBundle (#3587 PR-B3, decision B3-D15 R3) is VerifyBundle in
// the issuing authority's form. The authority replays its own committed chain
// to rebuild a head, so it differs from a client's VerifyBundle in exactly two
// places and nowhere else:
//
//   - at genesis it takes the seq-0 pin as an argument (a legacy-population
//     account's first result is a PoP enroll_existing, which VerifyBundle,
//     holding no AccountMemory, refuses as transition_invalid); the pin is read
//     only when prev is the genesis head;
//   - a superseded entry returns its after-head with superseded = true, in
//     place of prev and superseded_uncovered: the authority signed that entry,
//     so there is no status to cover, and refusing it would stop a replay at
//     the first renewal.
//
// Everything else (link, seq, hypothesis, fix, halt) is VerifyBundle's, and the
// first test below holds the two together on the committed corpus.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// replayPinChain is the committed legacy-population account whose seq 1 is a
// PoP enroll_existing under a first-sight pin.
func replayPinChain(t *testing.T) (c chainInputs, bundle, pin []byte) {
	t.Helper()
	cv := vectorOf(t, "pin/seq0/chain-under-the-first-sight-pin", "chain")
	require.Len(t, cv.Bundles, 1)
	c = chainInputsOf(t, cv)
	require.Empty(t, c.prev.HeadResult, "the vector starts at genesis")
	require.NotEmpty(t, c.mem.Pin, "the chain vector's memory holds the account's own pin")
	return c, mustB64(t, cv.Bundles[0]), c.mem.Pin
}

// ---- 1. Agreement with VerifyBundle on the corpus ----

// Off genesis and off a superseded entry the two functions are one function:
// the same head, byte for byte, and the same Reason, refusals included. A
// divergence is a vector the authority and a client would judge differently,
// which is AC9's drift.
func TestReplayAuthorityBundleAgreesWithVerifyBundleOnTheCorpus(t *testing.T) {
	accepted, refused := 0, 0
	for _, v := range loadVectors(t).Vectors {
		var bundle []byte
		switch v.Type {
		case "bundle-verify":
			bundle = mustB64(t, v.BytesB64)
		case "chain":
			if len(v.Bundles) == 0 {
				continue
			}
			bundle = mustB64(t, v.Bundles[0]) // the only bundle with a head to verify against
		default:
			continue
		}
		tr, r := trustOf(t, v)
		if r != ReasonOK {
			continue
		}
		prev := prevHeadOf(t, v)
		if len(prev.HeadResult) == 0 {
			continue // genesis: the pin argument is the difference, tested below
		}
		want, wr := VerifyBundle(tr, hwOf(v.HW), prev, bundle, nil)
		if wr == ReasonSupersededUncovered {
			continue // the other difference, tested below
		}
		got, sup, gr := ReplayAuthorityBundle(tr, hwOf(v.HW), prev, bundle, nil)
		require.Equal(t, wr, gr, "%s: the Reason", v.Name)
		require.False(t, sup, "%s: not superseded", v.Name)
		require.Equal(t, want.Bytes(), got.Bytes(), "%s: the head", v.Name)
		if wr == ReasonOK {
			accepted++
		} else {
			refused++
		}
	}
	// Measured 2026-10-22 at 60 accepted and 78 refused: the floors sit just
	// below, so a filter above that starts skipping vectors fails here rather
	// than comparing nothing.
	require.Greater(t, accepted, 57, "the corpus's accepted non-genesis bundles are all compared")
	require.Greater(t, refused, 75, "and its refusals, so the Reason is compared on both sides")
}

// ---- 2 and 3. The seq-0 pin ----

func TestReplayAuthorityBundleGenesisPin(t *testing.T) {
	c, bundle, pin := replayPinChain(t)

	t.Run("control: VerifyBundle refuses the PoP enrolment, holding no pin", func(t *testing.T) {
		head, r := VerifyBundle(c.tr, c.hw, c.prev, bundle, nil)
		require.Equal(t, ReasonTransitionInvalid, r, "A0 admits no enroll_existing")
		require.Equal(t, c.prev.Bytes(), head.Bytes())
	})

	t.Run("the right pin admits it, to the head VerifyChain reaches under AccountMemory{Pin}", func(t *testing.T) {
		next, sup, r := ReplayAuthorityBundle(c.tr, c.hw, c.prev, bundle, pin)
		require.Equal(t, ReasonOK, r)
		require.False(t, sup)

		want, _, _, cr := VerifyChain(c.tr, c.hw, c.prev, ChainInput{Bundles: [][]byte{bundle}, Status: c.status},
			AccountMemory{Pin: pin}, c.now, c.use)
		require.Equal(t, ReasonOK, cr, "control: the honest chain verifies under the memory pin")
		want.Fix = nil // VerifyChain retains the status it saw for a pending head (R4); a replay is offered none
		require.Equal(t, want.Bytes(), next.Bytes())

		hp, r := headPub(next)
		require.Equal(t, ReasonOK, r)
		require.Equal(t, uint64(1), hp.Seq)
		require.Equal(t, pin, hp.Pin, "the pin is carried into the head")
		require.Equal(t, StatePending, hp.State)
		require.Equal(t, AssuranceContinuity, hp.Assurance)
	})

	t.Run("a head that outlives the caller's pin buffer", func(t *testing.T) {
		own := append([]byte(nil), pin...)
		next, _, r := ReplayAuthorityBundle(c.tr, c.hw, c.prev, bundle, own)
		require.Equal(t, ReasonOK, r)
		before := append([]byte(nil), next.Bytes()...)
		for i := range own {
			own[i] ^= 0xff
		}
		require.Equal(t, before, next.Bytes(), "CWE-375: the head does not alias the argument")
		hp, r := headPub(next)
		require.Equal(t, ReasonOK, r)
		require.Equal(t, pin, hp.Pin)
	})

	for name, bad := range map[string][]byte{
		"a wrong pin":  d48("a pin that is not the account's"),
		"no pin":       nil,
		"an empty pin": {},
	} {
		t.Run("refused with "+name, func(t *testing.T) {
			next, sup, r := ReplayAuthorityBundle(c.tr, c.hw, c.prev, bundle, bad)
			require.NotEqual(t, ReasonOK, r, "the chain cannot start under a pin its first result does not name")
			require.False(t, sup)
			require.Equal(t, c.prev.Bytes(), next.Bytes(), "prev is returned unchanged, as VerifyBundle does")
		})
	}
}

// ---- 4. The pin is read at genesis only ----

func TestReplayAuthorityBundleIgnoresThePinOffGenesis(t *testing.T) {
	_, _, rightPin := replayPinChain(t)
	v := vectorOf(t, "row/06/rotate-ek", "bundle-verify")
	tr, r := trustOf(t, v)
	require.Equal(t, ReasonOK, r)
	prev, bundle := prevHeadOf(t, v), mustB64(t, v.BytesB64)
	require.NotEmpty(t, prev.HeadResult, "precondition: prev is not genesis")
	want, wr := VerifyBundle(tr, hwOf(v.HW), prev, bundle, nil)
	require.Equal(t, ReasonOK, wr, "control")

	for name, pin := range map[string][]byte{
		"nil":       nil,
		"the right": rightPin,
		"a wrong":   d48("a pin that names no account"),
		"empty":     {},
	} {
		t.Run(name+" pin", func(t *testing.T) {
			got, sup, r := ReplayAuthorityBundle(tr, hwOf(v.HW), prev, bundle, pin)
			require.Equal(t, ReasonOK, r)
			require.False(t, sup)
			require.Equal(t, want.Bytes(), got.Bytes())
		})
	}

	t.Run("a refusal off genesis is the same under any pin", func(t *testing.T) {
		broken := rebundle(t, bundle, func(b *Bundle) { b.ResultSig = flipped(b.ResultSig) })
		for _, pin := range [][]byte{nil, rightPin} {
			got, sup, r := ReplayAuthorityBundle(tr, hwOf(v.HW), prev, broken, pin)
			require.Equal(t, ReasonBadSignature, r)
			require.False(t, sup)
			require.Equal(t, prev.Bytes(), got.Bytes())
		}
	})
}

// ---- 5. A superseded entry ----

// replaySuperseded is a realm with delegation serial 1 that then renewed to
// serial 2 (MinValidSerial 2), and a chain signed before the renewal: enroll_new
// at seq 1, then a replace_root at seq 2, both under (0, 1). fresh is the Trust
// before the renewal, under which the chain verifies in full; renewed is the
// Trust after it, under which every entry is superseded.
type replaySuperseded struct {
	fresh, renewed *Trust
	b1, b2         []byte
	genesis        ChainHead
}

func newReplaySuperseded(t *testing.T) replaySuperseded {
	t.Helper()
	r := newTestRealm(t)
	r.addDel(1, nil)
	fresh := r.trust(t)
	c := &forgeChain{t: t, r: r, tr: fresh, realm: r.ID, account: newID()}

	root1, ek1 := testKey(t), forgedEK(t)
	b1, entry1, ekb1D, _ := c.enrollNew(root1, ek1, testT0)

	root2, ek2 := testKey(t), forgedEK(t)
	ekb2, ekb2D, ekb2Sig := c.ekBinding(root2, 2, ek2)
	asserted := testT0 + 3_600_000
	anb := asserted + 259_200_000 + 900_000 // asserted + Δ + G of (0, 1)
	in := Intent{RequestID: newID(), Op: OpReplaceRoot, ExpiresAtMs: asserted + 600_000, ExpectedSeq: 1,
		ExpectedHeadDigest: entry1, PredecessorRootDigest: Digest(pointOf(t, root1)),
		CandidateRoot: pointOf(t, root2), EKBindingDigest: ekb2D, DeviceIntentDigest: digestOf("dev-2")}
	a := CPAuthz{Actor: ActorUser, AssertedAtMs: asserted, EvidenceDigest: digestOf("ev-2")}
	res := Result{Seq: 2, State: StatePending, PriorState: StateActive, ActiveRoot: pointOf(t, root1),
		EKBindingDigest: ekb1D, PendingRoot: pointOf(t, root2), PendingEKBindingDigest: ekb2D,
		PrevEntryDigest: entry1, RecoveryGen: 1, ActivationNotBeforeMs: anb, Assurance: AssuranceUnverified,
		IssuedAtMs: asserted}
	b2, _ := c.finish(in, a, res, forgeSigs{candidate: root2, ekb: ekb2, ekbSig: ekb2Sig})

	r.addDel(2, nil) // MinValidSerial = 2: every result under serial 1 is superseded
	renewed := r.trust(t)
	require.Equal(t, uint64(2), renewed.HighWater().MinValid, "precondition: the renewal raised min_valid")
	return replaySuperseded{fresh: fresh, renewed: renewed, b1: b1, b2: b2, genesis: GenesisHead(r.ID, c.account)}
}

func TestReplayAuthorityBundleReturnsTheAfterHeadOfASupersededEntry(t *testing.T) {
	s := newReplaySuperseded(t)

	// The expected heads: the same bundles, verified before the renewal.
	want1, r := VerifyBundle(s.fresh, HighWater{}, s.genesis, s.b1, nil)
	require.Equal(t, ReasonOK, r, "harness: seq 1 verifies before the renewal")
	want2, r := VerifyBundle(s.fresh, HighWater{}, want1, s.b2, nil)
	require.Equal(t, ReasonOK, r, "harness: seq 2 chains from seq 1 before the renewal")

	t.Run("control: VerifyBundle refuses the superseded entry and returns prev", func(t *testing.T) {
		head, r := VerifyBundle(s.renewed, HighWater{}, s.genesis, s.b1, nil)
		require.Equal(t, ReasonSupersededUncovered, r)
		require.Equal(t, s.genesis.Bytes(), head.Bytes())
	})

	t.Run("seq 1: the after-head, superseded, ok", func(t *testing.T) {
		got, sup, r := ReplayAuthorityBundle(s.renewed, HighWater{}, s.genesis, s.b1, nil)
		require.Equal(t, ReasonOK, r, "not superseded_uncovered: the authority signed it")
		require.True(t, sup)
		require.NotEqual(t, s.genesis.Bytes(), got.Bytes(), "the after-head, not prev")
		require.Equal(t, want1.Bytes(), got.Bytes())
	})

	t.Run("seq 2 chains from that after-head, and is superseded too", func(t *testing.T) {
		h1, sup, r := ReplayAuthorityBundle(s.renewed, HighWater{}, s.genesis, s.b1, nil)
		require.Equal(t, ReasonOK, r)
		require.True(t, sup)
		got, sup, r := ReplayAuthorityBundle(s.renewed, HighWater{}, h1, s.b2, nil)
		require.Equal(t, ReasonOK, r)
		require.True(t, sup)
		require.Equal(t, want2.Bytes(), got.Bytes())
		hp, r := headPub(got)
		require.Equal(t, ReasonOK, r)
		require.Equal(t, uint64(2), hp.Seq)
		require.Equal(t, StatePending, hp.State)
	})

	t.Run("a persisted high-water mark past the serial supersedes as the Trust's own does", func(t *testing.T) {
		got, sup, r := ReplayAuthorityBundle(s.fresh, HighWater{Gen: 0, Serial: 2, MinValid: 2}, s.genesis, s.b1, nil)
		require.Equal(t, ReasonOK, r)
		require.True(t, sup)
		require.Equal(t, want1.Bytes(), got.Bytes())
	})

	t.Run("not superseded before the renewal", func(t *testing.T) {
		got, sup, r := ReplayAuthorityBundle(s.fresh, HighWater{}, s.genesis, s.b1, nil)
		require.Equal(t, ReasonOK, r)
		require.False(t, sup)
		require.Equal(t, want1.Bytes(), got.Bytes())
	})

	t.Run("a superseded entry that does not link is still refused", func(t *testing.T) {
		// seq 2 offered at genesis: supersession excuses T7 only, never the link.
		got, sup, r := ReplayAuthorityBundle(s.renewed, HighWater{}, s.genesis, s.b2, nil)
		require.NotEqual(t, ReasonOK, r)
		require.False(t, sup)
		require.Equal(t, s.genesis.Bytes(), got.Bytes())
	})

	t.Run("the committed succession vector VerifyBundle refuses", func(t *testing.T) {
		cv := vectorOf(t, "trust/t7/succession-serial-reset-covered", "chain")
		ci := chainInputsOf(t, cv)
		bundle := mustB64(t, cv.Bundles[0])
		_, vr := VerifyBundle(ci.tr, ci.hw, ci.prev, bundle, nil)
		require.Equal(t, ReasonSupersededUncovered, vr, "control")
		got, sup, r := ReplayAuthorityBundle(ci.tr, ci.hw, ci.prev, bundle, nil)
		require.Equal(t, ReasonOK, r)
		require.True(t, sup)
		require.NotEqual(t, ci.prev.Bytes(), got.Bytes())
	})
}

// ---- 6. Halt ----

func TestReplayAuthorityBundleRefusesOnAHaltedTrust(t *testing.T) {
	cv := vectorOf(t, "trust/t7/succession-serial-reset-covered", "chain")
	hv := vectorOf(t, "trust/t4/no-delegation-for-current-generation", "chain")
	c := chainInputsOf(t, cv)
	halted, r := trustOf(t, hv)
	require.Equal(t, ReasonOK, r)
	require.NotNil(t, halted.Halted(), "the retained set halts (T4)")
	bundle := mustB64(t, cv.Bundles[0])
	broken := rebundle(t, bundle, func(b *Bundle) { b.ResultSig = flipped(b.ResultSig) })

	// Controls under the live trust: the bundle replays, and the broken one
	// fails on a Reason of its own.
	_, _, r = ReplayAuthorityBundle(c.tr, c.hw, c.prev, bundle, nil)
	require.Equal(t, ReasonOK, r)
	_, _, r = ReplayAuthorityBundle(c.tr, c.hw, c.prev, broken, nil)
	require.Equal(t, ReasonBadSignature, r)

	for name, b := range map[string][]byte{"a bundle that replays": bundle, "a bundle that fails on its own": broken} {
		t.Run(name, func(t *testing.T) {
			got, sup, r := ReplayAuthorityBundle(halted, c.hw, c.prev, b, nil)
			require.Equal(t, ReasonTrustHalt, r, "the halt outranks the bundle's own Reason")
			require.False(t, sup)
			require.Equal(t, c.prev.Bytes(), got.Bytes())
		})
	}

	t.Run("a recorded T4 contradiction", func(t *testing.T) {
		v := vectorOf(t, "row/06/rotate-ek", "bundle-verify")
		tr, r := trustOf(t, v)
		require.Equal(t, ReasonOK, r)
		prev, b := prevHeadOf(t, v), mustB64(t, v.BytesB64)
		_, _, r = ReplayAuthorityBundle(tr, hwOf(v.HW), prev, b, nil)
		require.Equal(t, ReasonOK, r, "control")
		tr.halt = &HaltError{Rule: "T4: two delegations at one (gen, serial)"}
		got, sup, r := ReplayAuthorityBundle(tr, hwOf(v.HW), prev, b, nil)
		require.Equal(t, ReasonTrustHalt, r)
		require.False(t, sup)
		require.Equal(t, prev.Bytes(), got.Bytes())
	})

	t.Run("a halted Trust at genesis with the right pin", func(t *testing.T) {
		pc, pb, pin := replayPinChain(t)
		pc.tr.halt = &HaltError{Rule: "T4: two delegations at one (gen, serial)"}
		_, _, r := ReplayAuthorityBundle(pc.tr, pc.hw, pc.prev, pb, pin)
		require.Equal(t, ReasonTrustHalt, r)
	})

	t.Run("a nil Trust", func(t *testing.T) {
		got, sup, r := ReplayAuthorityBundle(nil, c.hw, c.prev, bundle, nil)
		require.Equal(t, ReasonTrustHalt, r)
		require.False(t, sup)
		require.Equal(t, c.prev.Bytes(), got.Bytes())
	})
}
