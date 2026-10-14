package authoritycontract

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The E2EE review's published-side batch (A-B1, A-N1, A-N2/Sec-N3, Sec-N2,
// B-N3) and the linear-fix residual (binary-searched index lookups).

// A-B1: every row whose To installs the intent's candidate root (1, 2, 3, 5,
// 11, 20) refuses a candidate root_index already records; rows that burn
// theirs (4, 12) and rows with no candidate do not consult the index. The
// vector neg/transition/row-11-reinstalls-an-old-root carries one such
// refusal through VerifyBundle end to end.
func TestReinstallsRootOnEveryInstallingRow(t *testing.T) {
	root, other := testPoint(t), testPoint(t)
	head := ChainHead{RootIndex: []string{RootRecord(Digest(root), root, AssuranceInitial)}}
	installing := map[int]bool{1: true, 2: true, 3: true, 5: true, 11: true, 20: true}
	for _, r := range Rows() {
		for _, cand := range [][]byte{root, other, nil} {
			c := &bundleCtx{head: head, row: r, pb: parsedBundle{intent: Intent{CandidateRoot: cand}}}
			want := installing[r.N] && bytes.Equal(cand, root)
			require.Equal(t, want, c.reinstallsRoot(), "row %d, candidate recorded: %v", r.N, bytes.Equal(cand, root))
		}
	}
}

// Sec-N2: no bundle is admitted by two rows of one (op, actor). The sweep
// covers every from-code and every combination of guard verdicts a head can
// produce (guardHolds reads only the fields varied here), against every
// presence pattern, half pairs included.
func TestSelectRowIsUniqueOverEveryFromAndPattern(t *testing.T) {
	samples := selectRowSignatures()
	codes := map[FromCode]bool{}
	for _, p := range samples {
		codes[classify(p)] = true
	}
	require.Len(t, codes, 12, "the sample reaches every §2.9 from-code")

	type opActor struct{ op, actor string }
	pairs := map[opActor]bool{}
	for _, r := range Rows() {
		pairs[opActor{r.Op, r.Actor}] = true
	}
	checked := 0
	for k := range pairs {
		rows := rowsFor(k.op, k.actor)
		for _, p := range samples {
			for pres := presence(0); pres < pHalf<<1; pres++ {
				hit := admittingRows(rows, p, pres)
				require.LessOrEqual(t, len(hit), 1, "%s/%s from %s (%+v), presence %09b: rows %v",
					k.op, k.actor, classify(p), p, pres, hit)
				checked++
			}
		}
	}
	require.Equal(t, len(pairs)*len(samples)*int(pHalf<<1), checked)
}

// admittingRows is the numbers of the rows that admit (p, pres).
func admittingRows(rows []Row, p pub, pres presence) []int {
	var hit []int
	for _, r := range rows {
		if rowAdmits(r, classify(p), p, pres) {
			hit = append(hit, r.N)
		}
	}
	return hit
}

// selectRowSignatures is one sample head per distinct (from-code, guard
// verdicts) signature, which is all selectRow can tell apart.
func selectRowSignatures() []pub {
	type signature struct {
		code   FromCode
		guards uint16
	}
	seen := map[signature]bool{}
	var out []pub
	for _, p := range selectRowSamples() {
		var g uint16
		for gv := gNone; gv <= gAbsentPrior; gv++ {
			if guardHolds(gv, p) {
				g |= 1 << gv
			}
		}
		if k := (signature{classify(p), g}); !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

// selectRowSamples is the product of every head field classify and guardHolds
// read, over the values that change their answers: state × prior ×
// suspended_by × op × five presence/revocation bits, decoded from one index.
func selectRowSamples() []pub {
	sets := [][]string{{}, {ActorUser}, {ActorOperator}, {ActorOperator, ActorUser}}
	states := []string{StateAbsent, StateActive, StateSuspended, StatePending, StateDisputed, StateErased}
	priors := []string{"", StateAbsent, StateActive, StateSuspended}
	ops := []string{"", OpReplaceRoot, OpRecover, OpErase}
	n := len(states) * len(priors) * len(sets) * len(ops) * 32
	out := make([]pub, 0, n)
	for i := range n {
		mask, rest := i%32, i/32
		p := pub{State: states[rest%len(states)], Prior: priors[rest/len(states)%len(priors)],
			Susp: sets[rest/(len(states)*len(priors))%len(sets)], Op: ops[rest/(len(states)*len(priors)*len(sets))],
			RootRevoked: uint64(mask & 1), PinRevoked: uint64(mask >> 1 & 1)}
		p.Root = bitBytes(mask&4 != 0, "root")
		p.Pin = bitBytes(mask&8 != 0, "pin")
		p.PendRoot = bitBytes(mask&16 != 0, "cand")
		out = append(out, p)
	}
	return out
}

// bitBytes is s's bytes when on, else nil.
func bitBytes(on bool, s string) []byte {
	if on {
		return []byte(s)
	}
	return nil
}

// Sec-N2: two rows admitting one bundle is a table defect, refused rather
// than settled by table order. A duplicate of row 16 makes it reachable.
func TestSelectRowRefusesASecondMatch(t *testing.T) {
	f := pub{State: StatePending, Prior: StateActive, Root: []byte("root"), PendRoot: []byte("cand"), Susp: []string{}}
	row, r := selectRow(f, OpRevokeRoot, ActorOperator, pOperator)
	require.Equal(t, ReasonOK, r, "control: the real table admits it once")
	require.Equal(t, 16, row.N)

	orig := table
	t.Cleanup(func() { table = orig })
	dup := orig[15].clone()
	dup.N = 23
	table = append(slices.Clone(orig), dup)
	_, r = selectRow(f, OpRevokeRoot, ActorOperator, pOperator)
	require.Equal(t, ReasonTransitionInvalid, r)
}

// A-N1: rows 4 and 12 have only a D outcome, whose column is "" (DoR:612,
// :620, :685), whatever the evidence; a row number that is not a row of the
// op falls through to the op's derivation.
func TestDeriveAssuranceDOnlyRowsAreEmpty(t *testing.T) {
	for _, ev := range []Evidence{{}, full()} {
		for _, pop := range []bool{false, true} {
			for _, seg := range []Assurance{Assurance(AssuranceInitial), Assurance(AssuranceUnverified)} {
				in := AssuranceInput{Ev: ev, ValidPoP: pop, HasRoot: true, Segment: seg}
				in.Op, in.Row = Op(OpEnrollExisting), 4
				require.Equal(t, Assurance(""), DeriveAssurance(in), "row 4: %+v", in)
				in.Op, in.Row = Op(OpReplaceRoot), 12
				require.Equal(t, Assurance(""), DeriveAssurance(in), "row 12: %+v", in)
				in.Op, in.Row = Op(OpReplaceRoot), 4 // not a replace_root row
				require.NotEqual(t, Assurance(""), DeriveAssurance(in), "%+v", in)
				in.Op, in.Row = Op(OpEnrollExisting), 12 // not an enroll_existing row
				require.NotEqual(t, Assurance(""), DeriveAssurance(in), "%+v", in)
			}
		}
	}
}

// firstVector returns the first committed vector of type typ whose expected
// Reason is one a verified chain may carry (stateReasons) and whose previous
// head records a seq, a root and an EK.
func firstVector(t *testing.T, typ string) vector {
	t.Helper()
	for _, v := range loadVectors(t).Vectors {
		if v.Type != typ || !stateReasons[Reason(v.Expect.Reason)] || v.PrevHead == "" {
			continue
		}
		h := prevHeadOf(t, v)
		if len(h.SeqIndex) > 0 && len(h.RootIndex) > 0 && len(h.EKIndex) > 0 {
			return v
		}
	}
	require.Failf(t, "vector not found", "no %s vector with a seq, a root and an EK in its previous head", typ)
	return vector{}
}

// nonCanonical are hand-built heads ParseChainHead would refuse: each a<…>
// position out of EN3 order (a repeated record), and a seq_index record with
// no ".", which recordActivation used to slice by its "." (Sec-N3).
func nonCanonical(h ChainHead) map[string]ChainHead {
	dupRoot := h
	dupRoot.RootIndex = append(slices.Clone(h.RootIndex), h.RootIndex[0])
	dupEK := h
	dupEK.EKIndex = append(slices.Clone(h.EKIndex), h.EKIndex[0])
	noDot := h
	noDot.SeqIndex = slices.Clone(h.SeqIndex)
	noDot.SeqIndex[0] = strings.ReplaceAll(noDot.SeqIndex[0], ".", "")
	return map[string]ChainHead{"root_index out of order": dupRoot, "ek_index out of order": dupEK,
		"seq record without a dot": noDot}
}

// A-N2/Sec-N3: VerifyBundle measures prev before reading a record and
// refuses a head Parse… would refuse; without that, a repeated ek_index entry
// slipped through the incremental E3-08 check and came back as an ok head.
func TestVerifyBundleRefusesANonCanonicalHead(t *testing.T) {
	v := firstVector(t, "bundle-verify")
	tr, r := trustOf(t, v)
	require.Equal(t, ReasonOK, r)
	hw, bundle, prev := hwOf(v.HW), mustB64(t, v.BytesB64), prevHeadOf(t, v)
	_, got := VerifyBundle(tr, hw, prev, bundle, nil)
	require.Equal(t, Reason(v.Expect.Reason), got, "control: %s verifies from its parsed head", v.Name)
	for name, h := range nonCanonical(prev) {
		out, got := VerifyBundle(tr, hw, h, bundle, nil)
		require.Equal(t, ReasonMalformed, got, name)
		require.Equal(t, h, out, "%s: the head comes back unchanged", name)
	}
}

// A-N2/Sec-N3: VerifyChain refuses a non-canonical `from` as malformed before
// any step or status verdict; the status here is stale at `now`, so a
// verifier that read on would report the status instead.
func TestVerifyChainRefusesANonCanonicalHead(t *testing.T) {
	v := firstVector(t, "chain")
	c := chainInputsOf(t, v)
	bundles := make([][]byte, len(v.Bundles))
	for i, b := range v.Bundles {
		bundles[i] = mustB64(t, b)
	}
	late := c.now.AddDate(1, 0, 0)
	_, _, _, got := VerifyChain(c.tr, c.hw, c.prev, ChainInput{Bundles: bundles, Status: c.status}, c.mem, late, c.use)
	require.NotEqual(t, ReasonMalformed, got, "control: the parsed head with a stale status is not malformed")
	require.False(t, stateReasons[got], "control: %s is refused at a year later", v.Name)
	for name, h := range nonCanonical(c.prev) {
		out, _, _, got := VerifyChain(c.tr, c.hw, h, ChainInput{Bundles: bundles, Status: c.status}, c.mem, late, c.use)
		require.Equal(t, ReasonMalformed, got, name)
		require.Equal(t, h, out, "%s: the head comes back unchanged", name)
	}
}

// Sec-N3, defence in depth behind the entry guards: an activation that meets
// a seq_index record it cannot read marks the move bad, which fits refuses
// on both paths, rather than panicking.
func TestRecordActivationRefusesAnUnreadableSeqRecord(t *testing.T) {
	root := testPoint(t)
	prev := Result{Seq: 1, Op: OpReplaceRoot, PendingRoot: root, Assurance: AssuranceRecovery}
	act := pub{State: StateActive}
	for name, seq := range map[string][]string{"no dot": {b64s(digestOf("e1"))}, "no record": nil} {
		h := ChainHead{RealmID: newID(), AccountID: newID(), SeqIndex: seq}
		var m headMove
		require.NotPanics(t, func() { m.recordActivation(&h, prev, act) }, name)
		require.True(t, m.bad, name)
		_, ok := m.fits(h, -1)
		require.False(t, ok, "%s: unmeasured path", name)
		_, ok = m.fits(h, 0)
		require.False(t, ok, "%s: incremental path", name)
	}
	h := ChainHead{SeqIndex: []string{SeqRecord(digestOf("e1"), nil)}}
	var m headMove
	m.recordActivation(&h, prev, act)
	require.False(t, m.bad, "control: a canonical record takes its bind root")
	require.Equal(t, SeqRecord(digestOf("e1"), Digest(root)), h.SeqIndex[0])
}

// A-N2: an admitted_by record the codec refuses yields the empty view, not a
// view that skips or misreads it.
func TestDeviceHeadViewRefusesAnUnreadableAdmittedRecord(t *testing.T) {
	realm, acct := newID(), newID()
	a, b := testDev(t, 0), testDev(t, 0)
	s := setB{realm: realm, acct: acct, active: []DeviceEntry{a, b}}
	h := DeviceHead{RealmID: realm, AccountID: acct, DeviceSet: s.bytes(t), AdmittedBy: []string{AdmittedRecord(b.ID, a.ID)}}
	require.Len(t, h.View().Active, 2, "control")
	for _, rec := range []string{b.ID, b.ID + "." + a.ID + ".x", "not-a-uuid." + a.ID} {
		h.AdmittedBy = []string{rec}
		v := h.View()
		require.Empty(t, v.Active, "%q", rec)
		require.Empty(t, v.AdmittedBy(b.ID, true), "%q", rec)
		_, _, ok := v.BoundRecovery()
		require.False(t, ok, "%q", rec)
	}
}

// A last device result the codec refuses yields the empty view too, rather
// than a view of the set at dseq 0.
func TestDeviceHeadViewRefusesAnUnreadableLastResult(t *testing.T) {
	realm, acct := newID(), newID()
	a, b := testDev(t, 0), testDev(t, 0)
	s := setB{realm: realm, acct: acct, active: []DeviceEntry{a, b}}
	h := DeviceHead{RealmID: realm, AccountID: acct, DeviceSet: s.bytes(t), AdmittedBy: []string{AdmittedRecord(b.ID, a.ID)}}
	require.Len(t, h.View().Active, 2, "control")
	h.LastDeviceResult = []byte("not a device-result")
	v := h.View()
	require.Empty(t, v.Active)
	require.Empty(t, v.AdmittedBy(b.ID, true))
}

// B-N3: a nil *Trust admits nothing, reports the zero high-water mark, and
// its T7/T8/T12 checks are trust_halt, as haltReason has it, never a panic.
// The trust item and envelope are well formed, so each refusal is the nil
// guard's own and not the decoder's; AddTrust's carries its own rule name,
// because with AddSigned's guard in place its loop would refuse the same
// envelope as malformed anyway.
func TestNilTrustFailsClosed(t *testing.T) {
	v := firstVector(t, "bundle-verify")
	item := mustB64(t, v.Trust[0])
	env := TrustEnvelope{Items: [][]byte{item}}.Encode()
	require.NotNil(t, env)
	_, err := CheckTrustEnvelope(env)
	require.NoError(t, err, "control: the envelope is well formed")

	var tr *Trust
	for _, b := range [][]byte{[]byte("x"), item} {
		require.True(t, errors.Is(tr.AddSigned(b), ErrMalformed))
	}
	err = tr.AddTrust(env)
	require.True(t, errors.Is(err, ErrMalformed))
	require.ErrorContains(t, err, "envelope not read", "AddTrust's own guard refused it")
	require.Equal(t, HighWater{}, tr.HighWater())
	require.Equal(t, ReasonTrustHalt, tr.haltReason())
	sup, r := tr.checkT7(HighWater{}, 1, 1, 1, OpRotateEK, []byte("m"), []byte("s"))
	require.False(t, sup)
	require.Equal(t, ReasonTrustHalt, r)
	require.Equal(t, ReasonTrustHalt, tr.checkT8([]byte("a"), []byte("s"), 1, 1, 1))
	require.Equal(t, ReasonTrustHalt, tr.checkT12(digestOf("i"), []byte("s"), 1, 1, 1))
}

// The linear-fix residual: rootRecordOf and ekIndexHas binary-search the
// sorted indexes and must answer what a scan in index order answers, on every
// head the committed vectors carry (each previous head and each expected
// head), for every digest the head records and for one it does not.
func TestIndexLookupsAgreeWithAScanOnEveryVector(t *testing.T) {
	heads, probes := 0, 0
	for _, v := range loadVectors(t).Vectors {
		for _, enc := range []string{v.PrevHead, v.Expect.HeadB64} {
			if enc == "" {
				continue
			}
			h, err := ParseChainHead(mustB64(t, enc))
			if err != nil {
				continue // a negative vector's head; not one a verifier reads
			}
			heads++
			probes += checkIndexLookups(t, v.Name, h)
		}
	}
	require.Positive(t, heads)
	require.Greater(t, probes, heads, "the vectors' heads record roots and EKs to look up")
}

// checkIndexLookups compares the binary searches with scans in index order
// over every digest h records and one it does not; it returns the probes made.
func checkIndexLookups(t *testing.T, name string, h ChainHead) int {
	t.Helper()
	ds := make([][]byte, 0, 1+len(h.RootIndex)+len(h.EKIndex))
	ds = append(ds, rnd(48))
	for _, rec := range h.RootIndex {
		d, _, _ := strings.Cut(rec, ".")
		b, err := decodeB64(d)
		require.NoError(t, err)
		ds = append(ds, b)
	}
	ds = append(ds, h.EKIndex...)
	for _, d := range ds {
		gotRest, gotOK := rootRecordOf(h, d)
		wantRest, wantOK := scanRootRecord(h, d)
		require.Equal(t, wantOK, gotOK, name)
		require.Equal(t, wantRest, gotRest, name)
		want := slices.ContainsFunc(h.EKIndex, func(x []byte) bool { return bytes.Equal(x, d) })
		require.Equal(t, want, ekIndexHas(h, d), name)
	}
	return len(ds)
}

// scanRootRecord is the linear lookup rootRecordOf replaced.
func scanRootRecord(h ChainHead, d []byte) (string, bool) {
	prefix := b64s(d) + "."
	for _, rec := range h.RootIndex {
		if rest, ok := strings.CutPrefix(rec, prefix); ok {
			return rest, true
		}
	}
	return "", false
}
