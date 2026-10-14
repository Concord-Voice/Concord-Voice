package authoritycontract

import (
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// The committed vectors (restoreVectors in gen.mjs) carry §2.9's
// restore-cancel checks end to end. These tests pin what no well-formed chain
// can reach: check 3's naming clauses, which linkCheck and R5 also enforce
// on every bundle; check 4's clauses for a fix the API never stores (another
// head, a D(x) head); and check 2 over a (gen, serial) that retains two
// delegations, which only a retired generation holds.

const restoreIssued = testT0 + 60_000

// restoreFixture is a row-22 bundleCtx that passes checks 1–4: a held
// P(active) from-head at seq 3, and a held_status naming it, signed by sign
// as of the result's issued_at, under the result's (gen, serial).
func restoreFixture(t *testing.T, tr *Trust, gen, serial uint64, sign func([]byte) []byte) *bundleCtx {
	t.Helper()
	realm, account := newID(), newID()
	f := pub{Seq: 3, Entry: rnd(48), State: StatePending, Prior: StateActive, Susp: []string{}, Root: testPoint(t),
		EK: rnd(48), PendRoot: testPoint(t), PendEK: rnd(48), ANB: restoreIssued + 1_000_000, Op: OpReplaceRoot}
	st := derivedFields(f, Status{RealmID: realm, AccountID: account, Held: 1, ActivationNotBeforeMs: ANBNever,
		AsOfMs: restoreIssued, ValidUntilMs: restoreIssued + 259_200_000, DelegationGen: gen, DelegationSerial: serial})
	require.Equal(t, ReasonOK, checkR5(f, st), "the fixture's held_status passes R5")
	msg := st.Encode()
	require.NotNil(t, msg)
	sg := Signed{Type: ScopeStatus, Msg: msg, SigA: sign(msg)}
	held := sg.Encode()
	require.NotNil(t, held)
	pb := parsedBundle{raw: Bundle{HeldStatus: held}, authz: CPAuthz{Actor: ActorRestore, EvidenceDigest: Digest(held)},
		res: Result{RealmID: realm, AccountID: account, Seq: f.Seq + 1, PrevEntryDigest: f.Entry,
			IssuedAtMs: restoreIssued, DelegationGen: gen, DelegationSerial: serial},
		held: &st, heldSigned: &sg}
	return &bundleCtx{t: tr, pb: pb, f: f}
}

func newRestoreCtx(t *testing.T) *bundleCtx {
	t.Helper()
	r := newTestRealm(t)
	r.addDel(1, nil)
	return restoreFixture(t, r.trust(t), 0, 1, func(msg []byte) []byte { return signP1363(t, r.RSK, msg) })
}

func TestCheckRestoreCancelAcceptsAHeldFromHead(t *testing.T) {
	require.Equal(t, ReasonOK, checkRestoreCancel(newRestoreCtx(t)))
}

// Check 3 names the from-head through the result (seq − 1 and
// prev_entry_digest). linkCheck has already tied the result to the head the
// hypothesis came from, and R5 compares seq and head_entry_digest with that
// head, so on a published bundle these clauses refuse nothing R5 would not.
// Here the result names a head the status does not, and only they refuse.
// Realm and account are E3-09's: R5 copies both from the status.
func TestCheckRestoreCancelNamesTheResultsFromHead(t *testing.T) {
	cases := map[string]func(c *bundleCtx){
		"result seq is not the status's + 1": func(c *bundleCtx) { c.pb.res.Seq++ },
		"prev_entry_digest is another head":  func(c *bundleCtx) { c.pb.res.PrevEntryDigest = rnd(48) },
		"another realm":                      func(c *bundleCtx) { c.pb.res.RealmID = newID() },
		"another account":                    func(c *bundleCtx) { c.pb.res.AccountID = newID() },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			c := newRestoreCtx(t)
			mut(c)
			require.Equal(t, ReasonTransitionInvalid, checkRestoreCancel(c))
		})
	}
}

// Each clause of checks 1–3 refused on its own, and the positions parseBundle
// guarantees (both held fields set on actor restore) refused if absent.
func TestCheckRestoreCancelRefusesEachClause(t *testing.T) {
	cases := map[string]func(c *bundleCtx){
		"no held_status":            func(c *bundleCtx) { c.pb.held, c.pb.heldSigned = nil, nil },
		"no signed held_status":     func(c *bundleCtx) { c.pb.heldSigned = nil },
		"evidence_digest":           func(c *bundleCtx) { c.pb.authz.EvidenceDigest = rnd(48) },
		"as_of is not issued_at":    func(c *bundleCtx) { c.pb.res.IssuedAtMs++ },
		"signed by a non-RSK":       func(c *bundleCtx) { c.pb.heldSigned.SigA = signP1363(t, testKey(t), c.pb.heldSigned.Msg) },
		"no delegation at the name": func(c *bundleCtx) { c.pb.res.DelegationSerial, c.pb.held.DelegationSerial = 2, 2 },
		// The signing RSK is the result's delegation's; only the name differs
		// (R5 exempts both fields, and the signature covers the original msg).
		"names another delegation serial": func(c *bundleCtx) { c.pb.held.DelegationSerial = 2 },
		"names another delegation gen":    func(c *bundleCtx) { c.pb.held.DelegationGen = 1 },
		// Held 0 with the head's own anb still passes R5 (effective pending
		// before anb), so only the held clause refuses it.
		"held = 0": func(c *bundleCtx) { c.pb.held.Held, c.pb.held.ActivationNotBeforeMs = 0, c.f.ANB },
		"fails R5": func(c *bundleCtx) { c.pb.held.EKBindingDigest = rnd(48) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			c := newRestoreCtx(t)
			mut(c)
			require.Equal(t, ReasonTransitionInvalid, checkRestoreCancel(c))
		})
	}
}

// activationFix is a retained status showing the fixture's from-head
// activated at asOf (R5: held 0, anb 0, as_of ≥ the head's anb).
func activationFix(c *bundleCtx, asOf uint64) *Status {
	fx := *c.pb.held
	fx.Held, fx.ActivationNotBeforeMs, fx.AsOfMs = 0, 0, asOf
	return &fx
}

// Check 4: a fix that names the from-head and shows it activated refuses the
// restore-cancel at any issued_at (DoR:641, :644, :1320), a back-dated one
// included; a held_status never overrides it. A fix showing the head pending
// (held or before anb) and a fix for another head constrain nothing.
func TestCheckRestoreCancelCheck4HeldStatusNeverOverridesAFix(t *testing.T) {
	cases := []struct {
		name string
		fix  func(c *bundleCtx) *Status
		want Reason
	}{
		{"activated before issued", func(c *bundleCtx) *Status { return activationFix(c, restoreIssued-1) }, ReasonHypothesisConflict},
		{"activated at issued", func(c *bundleCtx) *Status { return activationFix(c, restoreIssued) }, ReasonHypothesisConflict},
		{"activated after issued", func(c *bundleCtx) *Status { return activationFix(c, restoreIssued+1) }, ReasonHypothesisConflict},
		{"pending before anb, not held", func(c *bundleCtx) *Status {
			fx := *c.pb.held
			fx.Held, fx.ActivationNotBeforeMs, fx.AsOfMs = 0, c.f.ANB, restoreIssued+1
			return &fx
		}, ReasonOK},
		{"held at a later instant", func(c *bundleCtx) *Status {
			fx := *c.pb.held
			fx.AsOfMs = restoreIssued + 1
			return &fx
		}, ReasonOK},
		{"activation fix of another head", func(c *bundleCtx) *Status {
			fx := activationFix(c, restoreIssued-1)
			fx.Seq--
			return fx
		}, ReasonOK},
		{"activation fix of another entry digest", func(c *bundleCtx) *Status {
			fx := activationFix(c, restoreIssued-1)
			fx.HeadEntryDigest = rnd(48)
			return fx
		}, ReasonOK},
		{"no fix", func(*bundleCtx) *Status { return nil }, ReasonOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newRestoreCtx(t)
			c.fix = tc.fix(c)
			require.Equal(t, tc.want, checkRestoreCancel(c))
		})
	}
}

// A D(x) from-head never activates, so no fix can show it activated: a fix
// naming it (anb 0, not held) is the status of D itself.
func TestCheckRestoreCancelCheck4NeverRefusesFromD(t *testing.T) {
	c := newRestoreCtx(t)
	c.f.State, c.f.PendRoot, c.f.PendEK, c.f.ANB = StateDisputed, nil, nil, 0
	st := derivedFields(c.f, Status{RealmID: c.pb.res.RealmID, AccountID: c.pb.res.AccountID, Held: 1,
		AsOfMs: restoreIssued, ValidUntilMs: restoreIssued + 259_200_000, DelegationSerial: 1})
	require.Equal(t, ReasonOK, checkR5(c.f, st))
	c.pb.held = &st
	c.fix = activationFix(c, restoreIssued-1)
	require.Equal(t, ReasonOK, checkRestoreCancel(c))
}

// retiredRestorePair is a realm whose retired generation 0 retains two valid
// delegations at (0, 2): A, with r.RSK and every scope, and B, with rskB and
// B's scope; generation 1 is live, so T4 does not halt.
func retiredRestorePair(t *testing.T, scopeB []string) (*testRealm, *Trust, func([]byte) []byte) {
	t.Helper()
	r := newTestRealm(t)
	g0 := r.Gens[0]
	rskB := testKey(t)
	a := r.signDel(r.del(2, nil), g0.A, g0.B)
	b := r.signDel(r.del(2, func(d *Delegation) {
		d.RSKPublicKeys = [][]byte{pointOf(t, rskB)}
		d.Scope = scopeB
	}), g0.A, g0.B)
	succ := r.succeed(nil)
	live := r.signDel(r.del(1, nil), r.Gens[1].A, r.Gens[1].B)
	tr, err := NewTrust(r.Anchors)
	require.NoError(t, err)
	for _, it := range [][]byte{a, b, succ, live} {
		var h *HaltError
		if err := tr.AddSigned(it); err != nil && !errors.Is(err, ErrIgnored) && !errors.As(err, &h) {
			require.NoError(t, err)
		}
	}
	require.Nil(t, tr.Halted())
	_, ok := tr.lookup(0, 2)
	require.False(t, ok, "lookup refuses the ambiguous (0, 2)")
	return r, tr, func(msg []byte) []byte { return signP1363(t, rskB, msg) }
}

// Check 2 is existential over the delegations retained at the result's
// (gen, serial), as T6–T8 are: lookup refuses the ambiguous key, so reading
// the delegation through it would refuse an honest held_status. Scope and
// key must hold under ONE delegation: B's RSK signing while only A lists
// `status` is refused.
func TestCheckRestoreCancelCheck2IsExistentialOverRetainedDelegations(t *testing.T) {
	r, tr, signB := retiredRestorePair(t, slices.Clone(scopeValues))
	require.Equal(t, ReasonOK, checkRestoreCancel(restoreFixture(t, tr, 0, 2, signB)), "B's RSK, B in scope")
	signA := func(msg []byte) []byte { return signP1363(t, r.RSK, msg) }
	require.Equal(t, ReasonOK, checkRestoreCancel(restoreFixture(t, tr, 0, 2, signA)), "A's RSK, A in scope")

	noStatus := slices.DeleteFunc(slices.Clone(scopeValues), func(s string) bool { return s == ScopeStatus })
	_, tr, signB = retiredRestorePair(t, noStatus)
	require.Equal(t, ReasonTransitionInvalid, checkRestoreCancel(restoreFixture(t, tr, 0, 2, signB)),
		"B's RSK signed, but only A holds `status` in its scope")
}

// Check 2's window clause (DoR §2.10 T6 and §2.9 check 2): the delegation
// whose RSK signed held_status must also cover its as_of, under the same
// delegation. B lists the signing RSK and `status`, A covers as_of but does
// not list B's RSK, so only B's window decides. Both window edges are pinned
// (not_before ≤ as_of < not_after).
func TestCheckRestoreCancelCheck2RequiresTheSignersWindowToCoverAsOf(t *testing.T) {
	for name, tc := range map[string]struct {
		nb, na uint64
		want   Reason
	}{
		"window ends at as_of":         {testT0 - 86_400_000, restoreIssued, ReasonTransitionInvalid},
		"window begins after as_of":    {restoreIssued + 1, restoreIssued + 86_400_000, ReasonTransitionInvalid},
		"window begins at as_of":       {restoreIssued, restoreIssued + 86_400_000, ReasonOK},
		"window ends just after as_of": {testT0 - 86_400_000, restoreIssued + 1, ReasonOK},
	} {
		t.Run(name, func(t *testing.T) {
			_, tr, signB := retiredRestorePairWithB(t, func(d *Delegation) { d.NotBeforeMs, d.NotAfterMs = tc.nb, tc.na })
			require.Equal(t, tc.want, checkRestoreCancel(restoreFixture(t, tr, 0, 2, signB)))
		})
	}
}

// retiredRestorePairWithB is retiredRestorePair with B (rskB, every scope)
// edited by mut before it is signed.
func retiredRestorePairWithB(t *testing.T, mut func(*Delegation)) (*testRealm, *Trust, func([]byte) []byte) {
	t.Helper()
	r := newTestRealm(t)
	g0 := r.Gens[0]
	rskB := testKey(t)
	a := r.signDel(r.del(2, nil), g0.A, g0.B)
	b := r.signDel(r.del(2, func(d *Delegation) {
		d.RSKPublicKeys = [][]byte{pointOf(t, rskB)}
		mut(d)
	}), g0.A, g0.B)
	succ := r.succeed(nil)
	live := r.signDel(r.del(1, nil), r.Gens[1].A, r.Gens[1].B)
	tr, err := NewTrust(r.Anchors)
	require.NoError(t, err)
	for _, it := range [][]byte{a, b, succ, live} {
		var h *HaltError
		if err := tr.AddSigned(it); err != nil && !errors.Is(err, ErrIgnored) && !errors.As(err, &h) {
			require.NoError(t, err)
		}
	}
	require.Nil(t, tr.Halted())
	require.Len(t, tr.delegations(0, 2), 2, "both delegations are retained at (0, 2)")
	return r, tr, func(msg []byte) []byte { return signP1363(t, rskB, msg) }
}
