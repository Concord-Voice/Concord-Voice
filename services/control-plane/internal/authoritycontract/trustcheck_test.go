package authoritycontract

import (
	"crypto/ecdsa"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCoversIsHalfOpen(t *testing.T) {
	d := Delegation{NotBeforeMs: 100, NotAfterMs: 200}
	require.True(t, covers(d, 100), "x = not_before_ms is covered")
	require.True(t, covers(d, 199))
	require.False(t, covers(d, 200), "x = not_after_ms is not covered")
	require.False(t, covers(d, 99))
}

func TestT6(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	tr := r.trust(t)
	s := func(serial uint64, asOf uint64) (msg, sig []byte) {
		sg, err := DecodeSigned(r.status(t, func(st *Status) { st.DelegationSerial = serial; st.AsOfMs = asOf; st.ValidUntilMs = asOf + 1 }))
		require.NoError(t, err)
		return sg.Msg, sg.SigA
	}
	m, sg := s(1, testT0)
	hw, reason := tr.checkT6(HighWater{}, m, sg, 0, 1, testT0)
	require.Equal(t, ReasonOK, reason)
	require.Equal(t, uint64(1), hw.Serial)
	_, reason = tr.checkT6(HighWater{}, m, sg, 0, 7, testT0)
	require.Equal(t, ReasonDelegationUnknown, reason)
	_, reason = tr.checkT6(HighWater{}, m, signP1363(t, testKey(t), m), 0, 1, testT0)
	require.Equal(t, ReasonBadSignature, reason)
	d1, _ := tr.lookup(0, 1)
	m, sg = s(1, d1.NotAfterMs)
	_, reason = tr.checkT6(HighWater{}, m, sg, 0, 1, d1.NotAfterMs)
	require.Equal(t, ReasonDelegationWindow, reason, "as_of = not_after_ms")
	m, sg = s(1, d1.NotBeforeMs)
	_, reason = tr.checkT6(HighWater{}, m, sg, 0, 1, d1.NotBeforeMs)
	require.Equal(t, ReasonOK, reason, "as_of = not_before_ms")

	// After the client learns D(2), a status under D(1) is superseded.
	r.addDel(2, nil)
	tr2 := r.trust(t)
	m, sg = s(1, testT0)
	_, reason = tr2.checkT6(HighWater{}, m, sg, 0, 1, testT0)
	require.Equal(t, ReasonStatusSuperseded, reason)
	// A persisted HW above the Trust's own (the client saw D(3) before) also supersedes D(2).
	m, sg = s(2, testT0)
	_, reason = tr2.checkT6(HighWater{Gen: 0, Serial: 3, MinValid: 3}, m, sg, 0, 2, testT0)
	require.Equal(t, ReasonStatusSuperseded, reason)
	// min_valid binds: an HW whose min_valid exceeds the status's serial.
	_, reason = tr2.checkT6(HighWater{Gen: 0, Serial: 2, MinValid: 3}, m, sg, 0, 2, testT0)
	require.Equal(t, ReasonStatusSuperseded, reason, "serial ≥ HW.min_valid")

	noStatus := newTestRealm(t)
	noStatus.addDel(1, func(d *Delegation) { d.Scope = []string{OpEnrollNew} })
	tr3 := noStatus.trust(t)
	sg3, err := DecodeSigned(noStatus.status(t, nil))
	require.NoError(t, err)
	_, reason = tr3.checkT6(HighWater{}, sg3.Msg, sg3.SigA, 0, 1, testT0)
	require.Equal(t, ReasonOutOfScope, reason)
}

func TestT7(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	r.addDel(2, func(d *Delegation) { d.MinValidSerial = 2 })
	tr := r.trust(t)
	msg := []byte("result bytes")
	sig := signP1363(t, r.RSK, msg)
	sup, reason := tr.checkT7(HighWater{}, 0, 1, testT0, OpRotateEK, msg, sig)
	require.Equal(t, ReasonOK, reason)
	require.True(t, sup, "serial < HW.min_valid is superseded")
	sup, reason = tr.checkT7(HighWater{}, 0, 2, testT0, OpRotateEK, msg, sig)
	require.Equal(t, ReasonOK, reason)
	require.False(t, sup)
	_, reason = tr.checkT7(HighWater{}, 0, 2, testT0+181*86_400_000, OpRotateEK, msg, sig)
	require.Equal(t, ReasonDelegationWindow, reason)
	_, reason = tr.checkT7(HighWater{}, 0, 9, testT0, OpRotateEK, msg, sig)
	require.Equal(t, ReasonDelegationUnknown, reason)
	_, reason = tr.checkT7(HighWater{}, 0, 2, testT0, OpRotateEK, msg, signP1363(t, testKey(t), msg))
	require.Equal(t, ReasonBadSignature, reason)
}

// T7: op ∈ D.scope (DoR §2.10 T7).
func TestT7RefusesAnOpOutsideScope(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, func(d *Delegation) { d.Scope = []string{ScopeStatus} })
	tr := r.trust(t)
	msg := []byte("result bytes")
	_, reason := tr.checkT7(HighWater{}, 0, 1, testT0, OpRotateEK, msg, signP1363(t, r.RSK, msg))
	require.Equal(t, ReasonOutOfScope, reason)
}

// T7 marks an entry superseded when a persisted HW is a generation ahead of
// this Trust, and maxHW keeps the larger min_valid on a (gen, serial) tie.
func TestT7SupersededByALaterPersistedGeneration(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	tr := r.trust(t)
	msg := []byte("result bytes")
	sup, reason := tr.checkT7(HighWater{Gen: 1, Serial: 1, MinValid: 1}, 0, 1, testT0, OpRotateEK, msg, signP1363(t, r.RSK, msg))
	require.Equal(t, ReasonOK, reason)
	require.True(t, sup)
	require.Equal(t, uint64(3), maxHW(HighWater{Serial: 4, MinValid: 3}, HighWater{Serial: 4, MinValid: 1}).MinValid)
	require.Equal(t, uint64(3), maxHW(HighWater{Serial: 4, MinValid: 1}, HighWater{Serial: 4, MinValid: 3}).MinValid)
}

func TestT8(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	newCAK := testKey(t)
	r.addDel(2, func(d *Delegation) { d.CAKPublicKeys = [][]byte{pointOf(t, newCAK)} })
	tr := r.trust(t)
	authz := []byte("cp-authz bytes")
	old := signP1363(t, r.CAK, authz)
	require.Equal(t, ReasonOK, tr.checkT8(authz, old, 0, 2, testT0), "D′.serial ≤ the result's serial")
	require.Equal(t, ReasonCAKUnlisted, tr.checkT8(authz, signP1363(t, newCAK, authz), 0, 1, testT0),
		"a CAK first listed at a higher serial")
	require.Equal(t, ReasonCAKUnlisted, tr.checkT8(authz, signP1363(t, testKey(t), authz), 0, 2, testT0))
	d1, _ := tr.lookup(0, 1)
	require.Equal(t, ReasonOK, tr.checkT8(authz, old, 0, 1, d1.NotBeforeMs))
	require.Equal(t, ReasonDelegationWindow, tr.checkT8(authz, old, 0, 1, d1.NotAfterMs))

	// Same-gen: a CAK listed only in generation 0 does not authorize a generation-1 result.
	r.succeed(nil)
	r.addDel(1, func(d *Delegation) { d.CAKPublicKeys = [][]byte{pointOf(t, testKey(t))} })
	tr2 := r.trust(t)
	require.Equal(t, ReasonCAKUnlisted, tr2.checkT8(authz, old, 1, 1, testT0))
}

// T8/T9 standby promotion: the standby CAK listed at D(1) signs after D(2) makes it live.
func TestT9StandbyPromotionAndDrain(t *testing.T) {
	r := newTestRealm(t)
	standby, next := testKey(t), testKey(t)
	r.addDel(1, func(d *Delegation) { d.CAKPublicKeys = [][]byte{pointOf(t, r.CAK), pointOf(t, standby)} })
	r.addDel(2, func(d *Delegation) { d.CAKPublicKeys = [][]byte{pointOf(t, standby), pointOf(t, next)} })
	tr := r.trust(t)
	authz := []byte("cp-authz")
	gen, serial, ok := tr.CAKAuthority(authz, signP1363(t, standby, authz), 0, 2, testT0)
	require.True(t, ok)
	require.Equal(t, [2]uint64{0, 2}, [2]uint64{gen, serial}, "the newest D′ listing it")
	gen, serial, ok = tr.CAKAuthority(authz, signP1363(t, r.CAK, authz), 0, 2, testT0)
	require.True(t, ok)
	require.Equal(t, [2]uint64{0, 1}, [2]uint64{gen, serial}, "the drained CAK is found in D(1)")
	installed, grace := testT0, uint64(660_000)
	require.True(t, DrainClause(true, installed, grace, installed+grace-1))
	require.False(t, DrainClause(true, installed, grace, installed+grace))
	require.True(t, DrainClause(false, 0, 0, testT0), "not superseded")
	require.False(t, DrainClause(true, MaxSafeInt, 1, 0), "an overflowing end refuses")
	_, _, ok = tr.CAKAuthority(authz, signP1363(t, next, authz), 0, 1, testT0)
	require.False(t, ok, "T8 against the authority's current delegation (0, 1)")
}

func TestT12(t *testing.T) {
	r := newTestRealm(t)
	opOld, opNew := testKey(t), testKey(t)
	r.addDel(1, func(d *Delegation) { d.OperatorPublicKeys = [][]byte{pointOf(t, opOld)} })
	r.addDel(2, func(d *Delegation) { d.OperatorPublicKeys = [][]byte{pointOf(t, opNew)} })
	tr := r.trust(t)
	id := digestOf("intent")
	good := signP1363(t, opNew, sigMsg(RoleOperator, id))
	never := func([]byte) bool { return false }
	require.Equal(t, ReasonOK, tr.checkT12(id, good, 0, 2, testT0))
	require.Equal(t, ReasonBadSignature, tr.checkT12(id, signP1363(t, testKey(t), sigMsg(RoleOperator, id)), 0, 2, testT0), "unlisted")
	require.Equal(t, ReasonBadSignature, tr.checkT12(id, signP1363(t, opOld, sigMsg(RoleOperator, id)), 0, 2, testT0),
		"a key dropped at D(2) is refused for a result naming D(2): no lookback (D-231)")
	require.Equal(t, ReasonOK, tr.checkT12(id, signP1363(t, opOld, sigMsg(RoleOperator, id)), 0, 1, testT0))
	d2, _ := tr.lookup(0, 2)
	require.Equal(t, ReasonDelegationWindow, tr.checkT12(id, good, 0, 2, d2.NotAfterMs))
	require.Equal(t, ReasonDelegationUnknown, tr.checkT12(id, good, 0, 9, testT0))
	ref1, ref2 := DelegationRef{0, 1}, DelegationRef{0, 2}
	require.True(t, tr.OperatorAuthority(id, good, ref2, testT0, ref2, never))
	require.False(t, tr.OperatorAuthority(id, good, ref2, testT0, ref2, func([]byte) bool { return true }), "revoked")
	require.False(t, tr.OperatorAuthority(id, signP1363(t, opOld, sigMsg(RoleOperator, id)), ref1, testT0, ref2, never),
		"not in the current signing delegation")

	empty := newTestRealm(t)
	empty.addDel(1, nil) // GA form: no operator key (LD-37)
	trE := empty.trust(t)
	require.Equal(t, ReasonBadSignature, trE.checkT12(id, good, 0, 1, testT0))
}

// The authority forms are never weaker than the client forms they extend
// (AC9), and refuse rather than assume: no revocation predicate, a current
// delegation the authority does not hold, and a halted trust all refuse.
func TestAuthorityFormsFailClosed(t *testing.T) {
	r := newTestRealm(t)
	opKey := testKey(t)
	withOp := func(cutover uint64) func(*Delegation) {
		return func(d *Delegation) { d.OperatorPublicKeys = [][]byte{pointOf(t, opKey)}; d.CutoverAtMs = cutover }
	}
	r.addDel(1, withOp(2))
	tr := r.trust(t)
	authz, id := []byte("cp-authz"), digestOf("intent")
	cakSig, opSig := signP1363(t, r.CAK, authz), signP1363(t, opKey, sigMsg(RoleOperator, id))
	never := func([]byte) bool { return false }
	_, _, ok := tr.CAKAuthority(authz, cakSig, 0, 1, testT0)
	require.True(t, ok, "control")
	require.True(t, tr.OperatorAuthority(id, opSig, DelegationRef{0, 1}, testT0, DelegationRef{0, 1}, never), "control")

	require.False(t, tr.OperatorAuthority(id, opSig, DelegationRef{0, 1}, testT0, DelegationRef{0, 1}, nil),
		"no revocation predicate refuses")
	_, _, ok = tr.CAKAuthority(authz, cakSig, 0, 2, testT0)
	require.False(t, ok, "a current delegation the authority does not hold")
	require.False(t, tr.OperatorAuthority(id, opSig, DelegationRef{0, 1}, testT0, DelegationRef{0, 2}, never),
		"a current delegation the authority does not hold")

	// D(2) lowers D(1)'s cutover: T4 halts the trust while both keys stay unique.
	var h *HaltError
	require.ErrorAs(t, tr.AddSigned(r.addDel(2, withOp(1))), &h)
	_, okD2 := tr.lookup(0, 2)
	require.True(t, okD2)
	_, _, ok = tr.CAKAuthority(authz, cakSig, 0, 2, testT0)
	require.False(t, ok, "a halted trust refuses the CAK")
	require.False(t, tr.OperatorAuthority(id, opSig, DelegationRef{0, 2}, testT0, DelegationRef{0, 2}, never),
		"a halted trust refuses the operator")
	st := []byte("status bytes")
	_, reason := tr.checkT6(HighWater{}, st, signP1363(t, r.RSK, st), 0, 2, testT0)
	require.Equal(t, ReasonTrustHalt, reason, "a halted trust refuses every status (T4)")

	var nilTrust *Trust
	_, _, ok = nilTrust.CAKAuthority(authz, cakSig, 0, 1, testT0)
	require.False(t, ok)
	require.False(t, nilTrust.OperatorAuthority(id, opSig, DelegationRef{0, 1}, testT0, DelegationRef{0, 1}, never))
}

// retiredContradiction is a realm whose retired generation 0 holds two
// different valid delegations for (0, 2), admitted in the given order, and
// whose generation 1 is live, so T4 does not halt (DoR T4: "Delegation
// contradictions inside a retired gen are ignored"; T3 retains both "for
// T7/T8 lookups"). Delegation A is r.RSK/r.CAK's with operator key opA and the
// usual window; B lists its own RSK, CAK and operator key, and its window
// opens one day after testT0.
type retiredContradiction struct {
	r                *testRealm
	a, b, succ, live []byte
	// opA, opB, rskB and cakB sign with the keys only one record lists.
	opA, opB, rskB, cakB func(msg []byte) []byte
}

func newRetiredContradiction(t *testing.T) retiredContradiction {
	r := newTestRealm(t)
	g0 := r.Gens[0]
	rskB, cakB, opA, opB := testKey(t), testKey(t), testKey(t), testKey(t)
	a := r.signDel(r.del(2, func(d *Delegation) { d.OperatorPublicKeys = [][]byte{pointOf(t, opA)} }), g0.A, g0.B)
	b := r.signDel(r.del(2, func(d *Delegation) {
		d.RSKPublicKeys = [][]byte{pointOf(t, rskB)}
		d.CAKPublicKeys = [][]byte{pointOf(t, cakB)}
		d.OperatorPublicKeys = [][]byte{pointOf(t, opB)}
		d.NotBeforeMs, d.NotAfterMs = testT0+86_400_000, testT0+181*86_400_000
		d.DeltaMs = 300_000_000
	}), g0.A, g0.B)
	succ := r.succeed(nil)
	g1 := r.Gens[1]
	live := r.signDel(r.del(1, nil), g1.A, g1.B)
	signer := func(k *ecdsa.PrivateKey) func([]byte) []byte {
		return func(msg []byte) []byte { return signP1363(t, k, msg) }
	}
	return retiredContradiction{r: r, a: a, b: b, succ: succ, live: live,
		opA: signer(opA), opB: signer(opB), rskB: signer(rskB), cakB: signer(cakB)}
}

func (c retiredContradiction) trust(t *testing.T, items ...[]byte) *Trust {
	tr, err := NewTrust(c.r.Anchors)
	require.NoError(t, err)
	for _, it := range items {
		var h *HaltError
		if err := tr.AddSigned(it); err != nil && !errors.Is(err, ErrIgnored) && !errors.As(err, &h) {
			require.NoError(t, err)
		}
	}
	require.Nil(t, tr.Halted(), "a retired contradiction never halts")
	return tr
}

// Ledger open item (Task 7 → Task 8): T6, T7, T8 and T12 are existential over
// every valid delegation retained at one (gen, serial), so neither record's
// signatures are lost to the other and no verdict depends on arrival order;
// lookup, which names "the" delegation, refuses an ambiguous key.
func TestRetiredContradictionIsExistential(t *testing.T) {
	c := newRetiredContradiction(t)
	day := uint64(86_400_000)
	result, authz, id := []byte("result bytes"), []byte("cp-authz bytes"), digestOf("intent")
	opMsg := sigMsg(RoleOperator, id)
	st, err := DecodeSigned(c.r.status(t, func(s *Status) { s.DelegationGen, s.DelegationSerial = 0, 2 }))
	require.NoError(t, err)
	var first []Delegation
	for name, order := range map[string][][]byte{"A first": {c.a, c.b, c.succ, c.live}, "B first": {c.b, c.a, c.succ, c.live}} {
		tr := c.trust(t, order...)
		_, ok := tr.lookup(0, 2)
		require.False(t, ok, "%s: lookup refuses an ambiguous (gen, serial)", name)
		ds := tr.delegations(0, 2)
		require.Len(t, ds, 2, name)
		if first == nil {
			first = ds
		}
		require.Equal(t, first, ds, "%s: one order whatever the arrival order", name)

		sup, reason := tr.checkT7(HighWater{}, 0, 2, testT0, OpRotateEK, result, signP1363(t, c.r.RSK, result))
		require.Equal(t, ReasonOK, reason, "%s: T7 under A", name)
		require.True(t, sup, "%s: a retired generation is superseded", name)
		sup, reason = tr.checkT7(HighWater{}, 0, 2, testT0+2*day, OpRotateEK, result, c.rskB(result))
		require.Equal(t, ReasonOK, reason, "%s: T7 under B", name)
		require.True(t, sup, name)
		_, reason = tr.checkT7(HighWater{}, 0, 2, testT0, OpRotateEK, result, c.rskB(result))
		require.Equal(t, ReasonDelegationWindow, reason, "%s: B's RSK signed, B's window misses: the furthest Reason", name)
		_, reason = tr.checkT7(HighWater{}, 0, 2, testT0, OpRotateEK, result, signP1363(t, testKey(t), result))
		require.Equal(t, ReasonBadSignature, reason, name)

		require.Equal(t, ReasonOK, tr.checkT8(authz, signP1363(t, c.r.CAK, authz), 0, 2, testT0), "%s: T8 under A", name)
		require.Equal(t, ReasonOK, tr.checkT8(authz, c.cakB(authz), 0, 2, testT0+2*day), "%s: T8 under B", name)
		require.Equal(t, ReasonDelegationWindow, tr.checkT8(authz, c.cakB(authz), 0, 2, testT0), name)

		require.Equal(t, ReasonOK, tr.checkT12(id, c.opA(opMsg), 0, 2, testT0), "%s: T12 under A", name)
		require.Equal(t, ReasonOK, tr.checkT12(id, c.opB(opMsg), 0, 2, testT0+2*day), "%s: T12 under B", name)
		require.Equal(t, ReasonDelegationWindow, tr.checkT12(id, c.opB(opMsg), 0, 2, testT0), name)

		_, reason = tr.checkT6(HighWater{}, st.Msg, st.SigA, 0, 2, testT0)
		require.Equal(t, ReasonStatusSuperseded, reason, "%s: known and signed, so superseded rather than unknown", name)
	}
}
