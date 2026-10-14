package authoritycontract

import (
	"crypto/ecdsa"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func requireHalt(t *testing.T, err error) {
	t.Helper()
	var h *HaltError
	require.True(t, errors.As(err, &h), "want a halt, got %v", err)
}

// requireHalted asserts a T4 halt through both of its readers: Halted, and the
// haltReason the verifiers consult, which must not lose a stored halt to a
// valid delegation of the current generation.
func requireHalted(t *testing.T, tr *Trust) {
	t.Helper()
	require.NotNil(t, tr.Halted())
	require.Equal(t, ReasonTrustHalt, tr.haltReason())
}

func TestNewTrustRefusesEqualAnchorKeys(t *testing.T) {
	r := newTestRealm(t)
	a := r.Anchors
	a.RecoveryB = a.PrimaryA
	_, err := NewTrust(a)
	require.ErrorIs(t, err, ErrMalformed, "T1: four pairwise-distinct keys")
	_, err = NewTrust(r.Anchors)
	require.NoError(t, err)
}

// A verifier holding no Trust refuses through both halt readers instead of
// panicking at its first check.
func TestNilTrustIsHalted(t *testing.T) {
	var tr *Trust
	require.NotNil(t, tr.Halted())
	require.Equal(t, ReasonTrustHalt, tr.haltReason())
	require.Equal(t, uint64(0), tr.CurrentGen(), "a nil Trust reads as generation 0, not a wrapped len-1")
}

func TestAddSignedAcceptsAValidDelegation(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	tr := r.trust(t)
	require.Equal(t, HighWater{Gen: 0, Serial: 1, MinValid: 1}, tr.HighWater())
	require.Nil(t, tr.Halted())
	_, ok := tr.lookup(0, 1)
	require.True(t, ok)
}

// DoR §3.1 "Dual control (LD-3, D-222, D-223)".
func TestDualControl(t *testing.T) {
	r := newTestRealm(t)
	g := r.Gens[0]
	other := testKey(t)
	tr := r.trust(t)
	d := r.del(1, nil)
	msg := d.Encode()
	env := func(a, b []byte) []byte { return Signed{Type: "delegation", Msg: msg, SigA: a, SigB: b}.Encode() }
	require.ErrorIs(t, tr.AddSigned(env(signP1363(t, g.A, msg), signP1363(t, other, msg))), ErrIgnored, "A only")
	require.ErrorIs(t, tr.AddSigned(env(signP1363(t, other, msg), signP1363(t, g.B, msg))), ErrIgnored, "B only")
	require.ErrorIs(t, tr.AddSigned(env(signP1363(t, g.B, msg), signP1363(t, g.A, msg))), ErrIgnored, "A and B swapped")
	require.ErrorIs(t, tr.AddSigned(env(signP1363(t, g.A, msg), signP1363(t, g.A, msg))), ErrIgnored, "the same key twice")
	// No contradiction was retained (I9). Halted() would also report T4's
	// verification-time "no valid delegation" rule, which holds here because
	// nothing has been admitted yet.
	require.Nil(t, tr.halt)
	require.NoError(t, tr.AddSigned(env(signP1363(t, g.A, msg), signP1363(t, g.B, msg))), "both primaries")
	require.Nil(t, tr.Halted())

	// A succession with sig_b by a key other than recovery B′ is ignored.
	r2 := newTestRealm(t)
	r2.addDel(1, nil)
	tr2 := r2.trust(t)
	next := r2.succeed(nil)
	sg, err := DecodeSigned(next)
	require.NoError(t, err)
	sg.SigB = signP1363(t, other, sg.Msg)
	require.ErrorIs(t, tr2.AddSigned(sg.Encode()), ErrIgnored)

	// A succession reusing a prior generation's key is ignored.
	r3 := newTestRealm(t)
	tr3 := r3.trust(t)
	reuse := r3.succeed(func(s *Succession) { s.NewPrimaryA = r3.Anchors.PrimaryA })
	require.ErrorIs(t, tr3.AddSigned(reuse), ErrIgnored)
}

// Dual control is positional for both trust objects (§2.6 `signed`): sig_a
// verifies under A (A′) and sig_b under B (B′) only, and each pair signs only
// its own object. A verifier that accepted either signature under either key
// would accept a one-signer object; one that accepted the other pair would let
// a recovery key alone stand in for a primary.
func TestDualControlIsPositionalAndRoleBound(t *testing.T) {
	r := newTestRealm(t)
	g := r.Gens[0]
	tr, err := NewTrust(r.Anchors)
	require.NoError(t, err)

	msg := r.del(1, nil).Encode()
	require.NotNil(t, msg)
	for name, pair := range map[string][2]*ecdsa.PrivateKey{
		"B twice":                            {g.B, g.B},
		"recovery A′ and B′ on a delegation": {g.RA, g.RB},
		"primary A with recovery B′":         {g.A, g.RB},
	} {
		env := Signed{Type: "delegation", Msg: msg, SigA: signP1363(t, pair[0], msg), SigB: signP1363(t, pair[1], msg)}
		require.ErrorIs(t, tr.AddSigned(env.Encode()), ErrIgnored, name)
	}
	require.Equal(t, HighWater{}, tr.HighWater())

	sg, err := DecodeSigned(r.succeed(nil))
	require.NoError(t, err)
	for name, pair := range map[string][2]*ecdsa.PrivateKey{
		"A′ and B′ swapped":                 {g.RB, g.RA},
		"A′ twice":                          {g.RA, g.RA},
		"B′ twice":                          {g.RB, g.RB},
		"primaries A and B on a succession": {g.A, g.B},
	} {
		sg.SigA, sg.SigB = signP1363(t, pair[0], sg.Msg), signP1363(t, pair[1], sg.Msg)
		require.ErrorIs(t, tr.AddSigned(sg.Encode()), ErrIgnored, name)
	}
	require.Equal(t, uint64(0), tr.CurrentGen())
	sg.SigA, sg.SigB = signP1363(t, g.RA, sg.Msg), signP1363(t, g.RB, sg.Msg)
	require.NoError(t, tr.AddSigned(sg.Encode()), "both recovery keys, in position")
	require.Equal(t, uint64(1), tr.CurrentGen())
	require.Nil(t, tr.halt)
}

// T2/T3 "realm matches", T3 "gen ≤ current_gen", and a non-trust `signed`.
func TestAddSignedRealmGenerationAndType(t *testing.T) {
	r := newTestRealm(t)
	g := r.Gens[0]
	tr := r.trust(t)
	require.ErrorIs(t, tr.AddSigned(r.signDel(r.del(1, func(d *Delegation) { d.RealmID = newID() }), g.A, g.B)),
		ErrIgnored, "a delegation of another realm")
	require.ErrorIs(t, tr.AddSigned(r.status(t, nil)), ErrMalformed, "a status is not a trust object")
	require.ErrorIs(t, tr.AddSigned([]byte("not signed")), ErrMalformed)

	r2 := newTestRealm(t)
	other := r2.succeed(func(s *Succession) { s.RealmID = newID() })
	tr2, err := NewTrust(r2.Anchors)
	require.NoError(t, err)
	require.ErrorIs(t, tr2.AddSigned(other), ErrIgnored, "a succession of another realm")
	require.Equal(t, uint64(0), tr2.CurrentGen())

	r3 := newTestRealm(t)
	succ := r3.succeed(nil)
	ahead := r3.addDel(1, nil) // generation 1
	tr3, err := NewTrust(r3.Anchors)
	require.NoError(t, err)
	require.ErrorIs(t, tr3.AddSigned(ahead), ErrIgnored, "a generation this Trust has not reached")
	require.NoError(t, tr3.AddSigned(succ))
	require.NoError(t, tr3.AddSigned(ahead), "admitted once its succession is")
	require.Equal(t, HighWater{Gen: 1, Serial: 1, MinValid: 1}, tr3.HighWater())
}

// A malleated-signature pair: D-222's dedupe on msg, never a halt.
func TestMalleatedSignatureIsOneDelegation(t *testing.T) {
	r := newTestRealm(t)
	first := r.addDel(1, nil)
	tr := r.trust(t)
	sg, err := DecodeSigned(first)
	require.NoError(t, err)
	sg.SigA = highS(t, sg.SigA)
	require.ErrorIs(t, tr.AddSigned(sg.Encode()), ErrIgnored)
	require.Nil(t, tr.Halted())
}

// T4 judges msg bytes only (D-222): two retained records with one msg are one object.
func TestT4JudgesMsgBytesOnly(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	tr := r.trust(t)
	recs := tr.dels[[2]uint64{0, 1}]
	tr.dels[[2]uint64{0, 1}] = append(recs, delRec{D: recs[0].D, Msg: recs[0].Msg, SigA: highS(t, recs[0].SigA)})
	tr.evalHalt()
	require.Nil(t, tr.Halted())
}

func TestT4Halts(t *testing.T) {
	both := func(t *testing.T, a, b []byte, r *testRealm, want bool) {
		for _, order := range [][][]byte{{a, b}, {b, a}} {
			tr, err := NewTrust(r.Anchors)
			require.NoError(t, err)
			var last error
			for _, it := range order {
				last = tr.AddSigned(it)
			}
			if want {
				requireHalt(t, last)
				requireHalted(t, tr)
			} else {
				require.Nil(t, tr.Halted(), "no halt in either order")
			}
		}
	}
	t.Run("two successions for one generation", func(t *testing.T) {
		r := newTestRealm(t)
		s1 := r.succeed(nil)
		r.Gens = r.Gens[:1]
		s2 := r.succeed(nil)
		both(t, s1, s2, r, true)
	})
	t.Run("equal (gen, serial), different msg", func(t *testing.T) {
		r := newTestRealm(t)
		g := r.Gens[0]
		d1 := r.signDel(r.del(2, nil), g.A, g.B)
		d2 := r.signDel(r.del(2, func(d *Delegation) { d.TauMs = 300_000 }), g.A, g.B)
		both(t, d1, d2, r, true)
	})
	t.Run("a higher serial lowers a non-zero cutover", func(t *testing.T) {
		r := newTestRealm(t)
		g := r.Gens[0]
		d1 := r.signDel(r.del(1, func(d *Delegation) { d.CutoverAtMs = testT0 + 10 }), g.A, g.B)
		d2 := r.signDel(r.del(2, func(d *Delegation) { d.CutoverAtMs = testT0 + 5 }), g.A, g.B)
		both(t, d1, d2, r, true)
	})
	t.Run("a contradiction carrying one invalid signature is ignored, not a halt", func(t *testing.T) {
		r := newTestRealm(t)
		g := r.Gens[0]
		d1 := r.signDel(r.del(2, nil), g.A, g.B)
		d2 := r.signDel(r.del(2, func(d *Delegation) { d.TauMs = 300_000 }), g.A, testKey(t))
		both(t, d1, d2, r, false)
	})
	t.Run("a contradiction inside a retired generation does not halt", func(t *testing.T) {
		r := newTestRealm(t)
		g0 := r.Gens[0]
		d1 := r.signDel(r.del(2, nil), g0.A, g0.B)
		d2 := r.signDel(r.del(2, func(d *Delegation) { d.TauMs = 300_000 }), g0.A, g0.B)
		succ := r.succeed(nil)
		g1 := r.Gens[1]
		live := r.signDel(r.del(1, nil), g1.A, g1.B)
		tr, err := NewTrust(r.Anchors)
		require.NoError(t, err)
		require.NoError(t, tr.AddSigned(succ))
		require.NoError(t, tr.AddSigned(live))
		require.NoError(t, tr.AddSigned(d1))
		require.NoError(t, tr.AddSigned(d2))
		require.Nil(t, tr.Halted())
	})
	t.Run("a succession before its generation's delegation halts nothing in AddSigned", func(t *testing.T) {
		r := newTestRealm(t)
		r.addDel(1, nil)
		succ := r.succeed(nil)
		tr, err := NewTrust(r.Anchors)
		require.NoError(t, err)
		require.NoError(t, tr.AddSigned(r.Items[0]))
		require.NoError(t, tr.AddSigned(succ))
		require.Nil(t, tr.halt, "AddSigned never evaluates the no-delegation rule")
		require.Equal(t, ReasonTrustHalt, tr.haltReason(), "but verification time does")
	})
}

// T4 is evaluated over the retained set, independent of arrival order (DoR
// §2.10), so the retired-generation case above holds in the other order too:
// a gen-0 contradiction halts while gen 0 is current and stops halting once a
// succession retires it, and the halt then ends with a valid delegation of the
// new generation (§2.13). A latched halt would make the outcome depend on
// which item arrived first and would let a stolen retired primary pair halt a
// client that happened to see its contradiction before the succession.
func TestT4IsAFunctionOfTheRetainedSet(t *testing.T) {
	r := newTestRealm(t)
	g0 := r.Gens[0]
	d1 := r.signDel(r.del(2, nil), g0.A, g0.B)
	d2 := r.signDel(r.del(2, func(d *Delegation) { d.TauMs = 300_000 }), g0.A, g0.B)
	succ := r.succeed(nil)
	g1 := r.Gens[1]
	live := r.signDel(r.del(1, nil), g1.A, g1.B)

	tr, err := NewTrust(r.Anchors)
	require.NoError(t, err)
	require.NoError(t, tr.AddSigned(d1))
	requireHalt(t, tr.AddSigned(d2))
	requireHalted(t, tr)
	require.NoError(t, tr.AddSigned(succ), "the succession retires generation 0")
	require.Equal(t, ReasonTrustHalt, tr.haltReason(), "until generation 1 has a delegation")
	require.NoError(t, tr.AddSigned(live))
	require.Nil(t, tr.Halted())
	require.Len(t, tr.delegations(0, 2), 2, "both gen-0 records stay retained")

	// A succession contradiction is never retired: later generations keep the halt.
	r2 := newTestRealm(t)
	s1 := r2.succeed(nil)
	canonical := append([]testGen(nil), r2.Gens...)
	r2.Gens = r2.Gens[:1]
	s2 := r2.succeed(nil)
	r2.Gens = canonical
	s3 := r2.succeed(nil)
	g2 := r2.Gens[2]
	d := r2.signDel(r2.del(1, nil), g2.A, g2.B)
	tr2, err := NewTrust(r2.Anchors)
	require.NoError(t, err)
	require.NoError(t, tr2.AddSigned(s1))
	requireHalt(t, tr2.AddSigned(s2))
	requireHalt(t, tr2.AddSigned(s3)) // advancing the chain does not clear it
	requireHalt(t, tr2.AddSigned(d))
	require.Equal(t, uint64(2), tr2.CurrentGen())
	requireHalted(t, tr2)
}

// T2's "equal no key of any EARLIER generation" with T4's "two successions
// for one prev_root_gen": a second succession for gen 0 is judged against the
// keys of generations 0..0 only, exactly as the first was. Judged against
// every generation this Trust knows, a fork that reuses one key of the
// published gen 1 would be ignored instead of halting, and which branch
// became gen 1 would depend on arrival order.
func TestT4ForkReusingASuccessorKeyHalts(t *testing.T) {
	r := newTestRealm(t)
	s1 := r.succeed(nil)
	g1 := r.Gens[1]
	r.Gens = r.Gens[:1]
	s2 := r.succeed(func(s *Succession) { s.NewPrimaryA = pointOf(t, g1.A) })
	for _, order := range [][][]byte{{s1, s2}, {s2, s1}} {
		tr, err := NewTrust(r.Anchors)
		require.NoError(t, err)
		require.NoError(t, tr.AddSigned(order[0]))
		requireHalt(t, tr.AddSigned(order[1]))
		requireHalted(t, tr)
	}
}

// DoR §3.1 "Negative, delegation bounds" (T3: ignored).
func TestT3Ignored(t *testing.T) {
	cases := map[string]func(*Delegation){
		"delta_ms = INTENT_WINDOW_MS + EPSILON_MS": func(d *Delegation) { d.DeltaMs = IntentWindowMs + EpsilonMs },
		"delta_ms = DELTA_MAX_MS + 1":              func(d *Delegation) { d.DeltaMs = DeltaMaxMs + 1 },
		"lifetime 400 d + 1 ms":                    func(d *Delegation) { d.NotAfterMs = d.NotBeforeMs + DelegationLifetimeMaxMs + 1 },
		"g_ms above delta_ms":                      func(d *Delegation) { d.GMs = d.DeltaMs + 1 },
		"tau_ms above V_MAX":                       func(d *Delegation) { d.TauMs = VMaxMs + 1 },
		"min_valid_serial above serial":            func(d *Delegation) { d.MinValidSerial = d.Serial + 1 },
		"min_valid_serial = 0":                     func(d *Delegation) { d.MinValidSerial = 0 },
		"tau_ms = 0":                               func(d *Delegation) { d.TauMs = 0 },
		"g_ms = 0":                                 func(d *Delegation) { d.GMs = 0 },
		"not_after_ms = not_before_ms":             func(d *Delegation) { d.NotAfterMs = d.NotBeforeMs },
		"not_after_ms before not_before_ms":        func(d *Delegation) { d.NotAfterMs = d.NotBeforeMs - 1 },
		"no \"1\" in accepted_versions":            func(d *Delegation) { d.AcceptedVersions = []string{"2"} },
		"shared RSK and CAK point":                 func(d *Delegation) { d.CAKPublicKeys = d.RSKPublicKeys },
		"shared operator and TLS point": func(d *Delegation) {
			d.OperatorPublicKeys = d.AuthorityTLSKeys
		},
		"an authority point whose digest is in cp_tls_pins": func(d *Delegation) {
			pin, err := TLSPin(d.AuthorityTLSKeys[0])
			if err == nil {
				d.CPTLSPins = [][]byte{pin}
			}
		},
		"an anchor key used as an RSK": nil,
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := newTestRealm(t)
			r.addDel(1, nil)
			tr := r.trust(t)
			if mut == nil {
				mut = func(d *Delegation) { d.RSKPublicKeys = [][]byte{r.Anchors.PrimaryA} }
			}
			g := r.Gens[0]
			require.ErrorIs(t, tr.AddSigned(r.signDel(r.del(2, mut), g.A, g.B)), ErrIgnored)
			require.Nil(t, tr.Halted(), "T3 never halts")
			require.Equal(t, uint64(1), tr.HighWater().Serial)
		})
	}
	r := newTestRealm(t)
	r.addDel(1, nil)
	tr := r.trust(t)
	g := r.Gens[0]
	exact := r.signDel(r.del(2, func(d *Delegation) { d.NotAfterMs = d.NotBeforeMs + DelegationLifetimeMaxMs }), g.A, g.B)
	require.NoError(t, tr.AddSigned(exact), "a lifetime of exactly 400 d is accepted")
}

// D-223: the cross-delegation RSK bullet is never T3.
func TestCrossDelegationRSKReuseIsNotT3(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	r.addDel(2, nil) // same RSK as serial 1
	tr := r.trust(t)
	require.Equal(t, uint64(2), tr.HighWater().Serial)
}

func TestBelowHWDelegationIsRetained(t *testing.T) {
	r := newTestRealm(t)
	g := r.Gens[0]
	d1 := r.signDel(r.del(1, nil), g.A, g.B)
	d2 := r.signDel(r.del(2, nil), g.A, g.B)
	tr, err := NewTrust(r.Anchors)
	require.NoError(t, err)
	require.NoError(t, tr.AddSigned(d2))
	require.NoError(t, tr.AddSigned(d1), "D(s) after D(s+1)")
	_, ok := tr.lookup(0, 1)
	require.True(t, ok, "retained for T7/T8 lookups")
	require.Equal(t, uint64(2), tr.HighWater().Serial, "never raises HW")
}

func TestCheckTrustEnvelope(t *testing.T) {
	r := newTestRealm(t)
	d1, d2 := r.addDel(1, nil), r.addDel(2, nil)
	s1 := r.succeed(nil)
	d11 := r.addDel(1, nil) // (gen 1, serial 1)
	_, err := CheckTrustEnvelope(TrustEnvelope{Items: [][]byte{s1, d1, d2}}.Encode())
	require.NoError(t, err)
	_, err = CheckTrustEnvelope(TrustEnvelope{Items: [][]byte{s1, d1, d2, d11}}.Encode())
	require.NoError(t, err, "(0, 2) then (1, 1): the generation orders first")
	for name, items := range map[string][][]byte{
		"delegations out of order":        {d2, d1},
		"duplicate (gen, serial)":         {d1, d1},
		"a succession after a delegation": {d1, s1},
		"(1, 1) then (0, 2)":              {s1, d11, d2},
	} {
		_, err := CheckTrustEnvelope(TrustEnvelope{Items: items}.Encode())
		require.ErrorIs(t, err, ErrMalformed, name)
	}
	tr, err := NewTrust(r.Anchors)
	require.NoError(t, err)
	require.NoError(t, tr.AddTrust(TrustEnvelope{Items: [][]byte{s1, d1, d2}}.Encode()))
	require.ErrorIs(t, tr.AddTrust(TrustEnvelope{Items: [][]byte{d2, d1}}.Encode()), ErrMalformed)
}

// AddTrust admits every item and reports the halt state after the last one.
func TestAddTrustReportsTheFinalHaltState(t *testing.T) {
	r := newTestRealm(t)
	g0 := r.Gens[0]
	d1 := r.signDel(r.del(1, nil), g0.A, g0.B)
	d2a := r.signDel(r.del(2, nil), g0.A, g0.B)
	d2b := r.signDel(r.del(2, func(d *Delegation) { d.TauMs = 300_000 }), g0.A, g0.B)
	d3 := r.signDel(r.del(3, nil), g0.A, g0.B)
	tr, err := NewTrust(r.Anchors)
	require.NoError(t, err)
	require.NoError(t, tr.AddSigned(d2a))
	requireHalt(t, tr.AddTrust(TrustEnvelope{Items: [][]byte{d1, d2b, d3}}.Encode()))
	requireHalted(t, tr)
	_, ok := tr.lookup(0, 3)
	require.True(t, ok, "items after the contradiction are still admitted")
	require.Equal(t, uint64(3), tr.HighWater().Serial)

	// A trust file that retires the contradicting generation ends the halt.
	succ := r.succeed(nil)
	g1 := r.Gens[1]
	live := r.signDel(r.del(1, nil), g1.A, g1.B)
	require.NoError(t, tr.AddTrust(TrustEnvelope{Items: [][]byte{succ, live}}.Encode()))
	require.Nil(t, tr.Halted())
}

// §2.6 `trust`: successions by new_root_gen strictly ascending (a duplicate is
// malformed), and only successions and delegations travel in it.
func TestCheckTrustEnvelopeSuccessionsAndTypes(t *testing.T) {
	r := newTestRealm(t)
	s1 := r.succeed(nil)
	s2 := r.succeed(nil)
	_, err := CheckTrustEnvelope(TrustEnvelope{Items: [][]byte{s1, s2}}.Encode())
	require.NoError(t, err)
	for name, items := range map[string][][]byte{
		"successions out of order": {s2, s1},
		"duplicate new_root_gen":   {s1, s1},
		"a status":                 {s1, r.status(t, nil)},
	} {
		_, err := CheckTrustEnvelope(TrustEnvelope{Items: items}.Encode())
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

// D-223 applies T3's key separation to each delegation alone, "so a
// verifier's result never depends on which other delegations it holds or on
// their arrival order", and DoR §2.10 T4 makes the retained set "independent
// of arrival order". A14 reads "none contains an anchor or succession key" as
// the keys of generations 0..product_root_gen, which are fixed once that
// generation exists. A generation-0 delegation listing a key that generation
// 1 later introduces is therefore retained whether it arrives before the
// succession or after it, with the same lookup and the same HW.
func TestT3KeySeparationIsIndependentOfArrivalOrder(t *testing.T) {
	r := newTestRealm(t)
	g0 := r.Gens[0]
	succ := r.succeed(nil)
	g1 := r.Gens[1]
	later := pointOf(t, g1.A)
	del := r.signDel(r.del(1, func(d *Delegation) {
		d.ProductRootGen = 0
		d.RSKPublicKeys = [][]byte{later}
	}), g0.A, g0.B)

	for name, order := range map[string][][]byte{
		"delegation, then succession": {del, succ},
		"succession, then delegation": {succ, del},
	} {
		tr, err := NewTrust(r.Anchors)
		require.NoError(t, err)
		for _, it := range order {
			require.NoError(t, tr.AddSigned(it), name)
		}
		require.Equal(t, uint64(1), tr.CurrentGen(), name)
		require.Equal(t, HighWater{Gen: 0, Serial: 1, MinValid: 1}, tr.HighWater(), name)
		got, ok := tr.lookup(0, 1)
		require.True(t, ok, "%s: retained", name)
		require.Equal(t, [][]byte{later}, got.RSKPublicKeys, name)
		require.Nil(t, tr.halt, name)
		require.Equal(t, ReasonTrustHalt, tr.haltReason(), "%s: generation 1 has no delegation yet", name)
	}
}

// DoR:249 (§2.4's first bullet, applied by T3 per A14): a delegation of
// generation g lists no anchor or succession key of generations 0..g, in any
// of its four key arrays. Generation 1's own keys are inside that range, so a
// check that read the anchors alone would admit every generation-1 case.
func TestT3KeySeparationCoversGenerationsUpToItsOwn(t *testing.T) {
	r := newTestRealm(t)
	r.succeed(nil)
	g0, g1 := r.Gens[0], r.Gens[1]
	tr := r.trust(t)
	require.Equal(t, uint64(1), tr.CurrentGen())
	keys := map[string]*ecdsa.PrivateKey{
		"gen-0 primary A": g0.A, "gen-0 recovery B′": g0.RB,
		"gen-1 primary A": g1.A, "gen-1 primary B": g1.B, "gen-1 recovery A′": g1.RA, "gen-1 recovery B′": g1.RB,
	}
	arrays := map[string]func(*Delegation, []byte){
		"rsk_public_keys":      func(d *Delegation, k []byte) { d.RSKPublicKeys = [][]byte{k} },
		"cak_public_keys":      func(d *Delegation, k []byte) { d.CAKPublicKeys = [][]byte{k} },
		"operator_public_keys": func(d *Delegation, k []byte) { d.OperatorPublicKeys = [][]byte{k} },
		"authority_tls_keys":   func(d *Delegation, k []byte) { d.AuthorityTLSKeys = [][]byte{k} },
	}
	for kn, key := range keys {
		p := pointOf(t, key)
		for an, set := range arrays {
			b := r.signDel(r.del(1, func(d *Delegation) { set(d, p) }), g1.A, g1.B)
			require.ErrorIs(t, tr.AddSigned(b), ErrIgnored, "%s in %s", kn, an)
		}
	}
	require.Equal(t, HighWater{}, tr.HighWater())
	require.NoError(t, tr.AddSigned(r.signDel(r.del(1, nil), g1.A, g1.B)), "the same delegation without the key")
	require.Equal(t, HighWater{Gen: 1, Serial: 1, MinValid: 1}, tr.HighWater())
}

// The generation-bounded key set fails closed past the chain's top, including
// at the uint64 ceiling where last+1 would wrap to an empty set, so neither
// in-package rule can judge a key against a set that silently stops short.
func TestKeysThroughRefusesAnUnreachedGeneration(t *testing.T) {
	r := newTestRealm(t)
	tr := r.trust(t)
	keys, ok := tr.keysThrough(0)
	require.True(t, ok)
	require.Len(t, keys, 4)
	for _, last := range []uint64{1, ^uint64(0)} {
		_, ok := tr.keysThrough(last)
		require.False(t, ok, "generation %d", last)
	}
	require.False(t, tr.keySeparationOK(r.del(1, func(d *Delegation) { d.ProductRootGen = 1 })))
	fresh := genKeys{pointOf(t, testKey(t)), pointOf(t, testKey(t)), pointOf(t, testKey(t)), pointOf(t, testKey(t))}
	require.True(t, tr.freshSuccessor(fresh, 0))
	require.False(t, tr.freshSuccessor(fresh, 1))
}

// DoR §2.10 T2: a succession is valid only if new = prev + 1 and its four new
// keys are pairwise distinct (freshness: TestDualControl). An invalid one is
// ignored, never a halt, and adopts no generation.
func TestT2Ignored(t *testing.T) {
	cases := map[string]func(*Succession){
		"new_root_gen = prev_root_gen":      func(s *Succession) { s.NewRootGen = s.PrevRootGen },
		"new_root_gen = prev_root_gen + 2":  func(s *Succession) { s.NewRootGen = s.PrevRootGen + 2 },
		"new primary A = new primary B":     func(s *Succession) { s.NewPrimaryB = s.NewPrimaryA },
		"new primary B = new recovery A′":   func(s *Succession) { s.NewRecoveryA = s.NewPrimaryB },
		"new primary A = new recovery B′":   func(s *Succession) { s.NewRecoveryB = s.NewPrimaryA },
		"new recovery A′ = new recovery B′": func(s *Succession) { s.NewRecoveryB = s.NewRecoveryA },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := newTestRealm(t)
			r.addDel(1, nil)
			tr := r.trust(t)
			require.ErrorIs(t, tr.AddSigned(r.succeed(mut)), ErrIgnored)
			require.Equal(t, uint64(0), tr.CurrentGen())
			require.Nil(t, tr.Halted(), "T2 never halts")
		})
	}
	r := newTestRealm(t)
	tr := r.trust(t)
	require.NoError(t, tr.AddSigned(r.succeed(nil)))
	require.Equal(t, uint64(1), tr.CurrentGen(), "the unmodified succession is adopted")
}

// The inclusive edges of T3's §2.5 bounds are accepted, so a bound tightened
// by one (`<=` written `<`, `>` written `>=`) fails here; TestT3Ignored holds
// the first value past each edge.
func TestT3AcceptsTheBoundEdges(t *testing.T) {
	r := newTestRealm(t)
	g := r.Gens[0]
	for name, mut := range map[string]func(*Delegation){
		"delta_ms = DELTA_MAX_MS": func(d *Delegation) { d.DeltaMs = DeltaMaxMs },
		"delta_ms = INTENT_WINDOW_MS + EPSILON_MS + 1": func(d *Delegation) {
			d.DeltaMs = IntentWindowMs + EpsilonMs + 1
			d.GMs = d.DeltaMs
		},
		"tau_ms = 1":                       func(d *Delegation) { d.TauMs = 1 },
		"tau_ms = V_MAX":                   func(d *Delegation) { d.TauMs = VMaxMs },
		"g_ms = 1":                         func(d *Delegation) { d.GMs = 1 },
		"g_ms = delta_ms":                  func(d *Delegation) { d.GMs = d.DeltaMs },
		"min_valid_serial = 1 below 2":     func(d *Delegation) { d.MinValidSerial = 1 },
		"not_after_ms = not_before_ms + 1": func(d *Delegation) { d.NotAfterMs = d.NotBeforeMs + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			tr, err := NewTrust(r.Anchors)
			require.NoError(t, err)
			require.NoError(t, tr.AddSigned(r.signDel(r.del(2, mut), g.A, g.B)))
			require.Equal(t, uint64(2), tr.HighWater().Serial)
		})
	}
}

// T5: HW is the lexicographic maximum (gen, serial) over the retained set,
// carrying that delegation's min_valid, whatever the insertion order, and it
// never decreases. The retained set is a map, so HighWater is read many times:
// an implementation that kept whichever record it read last cannot pass by the
// luck of one iteration order.
func TestHighWaterIsTheLexicographicMaximum(t *testing.T) {
	r := newTestRealm(t)
	g0 := r.Gens[0]
	d01 := r.signDel(r.del(1, nil), g0.A, g0.B)
	d03 := r.signDel(r.del(3, func(d *Delegation) { d.MinValidSerial = 2 }), g0.A, g0.B)
	succ := r.succeed(nil)
	g1 := r.Gens[1]
	d11 := r.signDel(r.del(1, nil), g1.A, g1.B)
	d12 := r.signDel(r.del(2, func(d *Delegation) { d.MinValidSerial = 1 }), g1.A, g1.B)
	want := HighWater{Gen: 1, Serial: 2, MinValid: 1}
	for _, order := range [][][]byte{
		{succ, d03, d12, d01, d11},
		{succ, d11, d01, d12, d03},
		{d03, d01, succ, d12, d11},
	} {
		tr, err := NewTrust(r.Anchors)
		require.NoError(t, err)
		var prev HighWater
		for _, it := range order {
			require.NoError(t, tr.AddSigned(it))
			hw := tr.HighWater()
			require.False(t, hw.less(prev), "HW never decreases")
			prev = hw
		}
		for range 64 {
			require.Equal(t, want, tr.HighWater())
		}
	}
}
