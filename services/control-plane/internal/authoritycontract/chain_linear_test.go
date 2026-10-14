package authoritycontract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Linear-time chain verification (headmove.go, "Moving a head in time linear
// in the chain"; the CWE-400 finding on VerifyChain/VerifyDeviceLedger). These
// tests pin three properties: a step's work does not grow with the head it
// moves, the incremental E3-08 accounting answers exactly what encoding the
// whole head answers, and no step writes an array a caller or another
// hypothesis can see.

// stepFixture is the row/07/suspend-user vector (a step that appends a
// seq_index record and inserts no EK) as trust, high water, bundle and
// previous head. It reads through testing.TB, which the vector helpers do
// not take, so the benchmark can share it; it admits trust items as trustOf
// does.
func stepFixture(tb testing.TB) (*Trust, HighWater, []byte, ChainHead) {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "vectors.json"))
	require.NoError(tb, err)
	var f vectorFile
	require.NoError(tb, json.Unmarshal(raw, &f))
	i := slices.IndexFunc(f.Vectors, func(v vector) bool { return v.Name == "row/07/suspend-user" })
	require.GreaterOrEqual(tb, i, 0)
	v := f.Vectors[i]
	require.Equal(tb, "bundle-verify", v.Type)
	require.Empty(tb, v.Fix)
	dec := func(s string) []byte {
		b, err := base64.StdEncoding.Strict().DecodeString(s)
		require.NoError(tb, err)
		return b
	}
	a, err := DecodeAnchors(dec(v.Anchors))
	require.NoError(tb, err)
	tr, err := NewTrust(a)
	require.NoError(tb, err)
	for _, it := range v.Trust {
		err := tr.AddSigned(dec(it))
		var h *HaltError
		if err != nil && !errors.As(err, &h) && !errors.Is(err, ErrIgnored) && !errors.Is(err, ErrMalformed) {
			require.NoError(tb, err)
		}
	}
	prev, err := ParseChainHead(dec(v.PrevHead))
	require.NoError(tb, err)
	return tr, hwOf(v.HW), dec(v.BytesB64), prev
}

// paddedPrev is prev0 with n ek_index filler entries merged in (the padding
// TestVerifyBundleRefusesAHeadOverHeadBlobMax uses: headPub ties seq_index to
// the head result's seq, ek_index carries no such tie), clipped as
// VerifyBundle and verifyChainSteps clip the head they are given. The filler
// is ekFiller's: "<8 digits>AAAA…", ascending in base64-text order.
func paddedPrev(tb testing.TB, prev0 ChainHead, n int) ChainHead {
	tb.Helper()
	p := prev0
	p.EKIndex = make([][]byte, n)
	for i := range p.EKIndex {
		b, err := base64.StdEncoding.DecodeString(fmt.Sprintf("%08d", i) + strings.Repeat("A", 56))
		require.NoError(tb, err)
		p.EKIndex[i] = b
	}
	for _, e := range prev0.EKIndex {
		p.EKIndex = insertB48(p.EKIndex, e)
	}
	return clipIndexes(p)
}

// measured is h's index length, which h must have (it is canonical).
func measured(tb testing.TB, h ChainHead) int {
	tb.Helper()
	n, ok := indexLen(h)
	require.True(tb, ok, "the head is canonical")
	return n
}

// stepBytes is the bytes one verifyBundleStep allocates, averaged over runs,
// with prev measured as verifyChainSteps measures it. Deterministic: the step
// is a pure function of its inputs, and no parallel test runs alongside a
// sequential one.
func stepBytes(t *testing.T, tr *Trust, hw HighWater, prev ChainHead, bundle []byte) uint64 {
	t.Helper()
	idx := measured(t, prev)
	const runs = 8
	_, _, r := verifyBundleStep(tr, hw, prev, idx, bundle, nil) // warm every lazy path once
	require.Equal(t, ReasonOK, r)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fails := 0
	for range runs {
		if _, _, r := verifyBundleStep(tr, hw, prev, idx, bundle, nil); r != ReasonOK {
			fails++
		}
	}
	runtime.ReadMemStats(&after)
	require.Zero(t, fails)
	return (after.TotalAlloc - before.TotalAlloc) / runs
}

// Regression for the CWE-400 finding: one step against a head padded to
// 100 000 index records allocates within a small factor of the same step
// against 1 000. Before the fix each step cloned all four index slices and
// re-encoded the whole after-head, so the 100k step allocated about a hundred
// times what the 1k step did.
func TestVerifyChainStepWorkIsIndependentOfHeadSize(t *testing.T) {
	tr, hw, bundle, prev0 := stepFixture(t)
	small, big := paddedPrev(t, prev0, 1_000), paddedPrev(t, prev0, 100_000)
	require.Greater(t, len(big.Bytes()), 6_000_000, "the large head is several MiB")
	smallBytes, bigBytes := stepBytes(t, tr, hw, small, bundle), stepBytes(t, tr, hw, big, bundle)
	t.Logf("bytes allocated per step: 1k-record head %d, 100k-record head %d", smallBytes, bigBytes)
	require.LessOrEqual(t, bigBytes, 2*smallBytes, "a step's allocation must not grow with the head")
}

// BenchmarkVerifyChainStep records a step's cost against heads of increasing
// size (for the record; the regression gate is the test above).
func BenchmarkVerifyChainStep(b *testing.B) {
	tr, hw, bundle, prev0 := stepFixture(b)
	for _, n := range []int{1_000, 10_000, 100_000} {
		prev := paddedPrev(b, prev0, n)
		idx := measured(b, prev)
		b.Run(fmt.Sprintf("ek_index=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, _, r := verifyBundleStep(tr, hw, prev, idx, bundle, nil); r != ReasonOK {
					b.Fatal(r)
				}
			}
		})
	}
}

// E3-08 on the measured (multi-step) path, both sides of the boundary: the
// incremental check accepts the largest padding whose after-head fits and
// refuses one more entry, exactly as the whole-head encoding does, and the
// index length it carries forward is the after-head's real one.
func TestVerifyChainStepRefusesAHeadOverHeadBlobMax(t *testing.T) {
	tr, hw, bundle, prev0 := stepFixture(t)

	after0, got := VerifyBundle(tr, hw, prev0, bundle, nil)
	require.Equal(t, ReasonOK, got)
	base, growth := len(prev0.Bytes()), len(after0.Bytes())-len(prev0.Bytes())
	step := len(paddedPrev(t, prev0, 1).Bytes()) - base
	fit := (HeadBlobMax - growth - base) / step

	atCap, over := paddedPrev(t, prev0, fit), paddedPrev(t, prev0, fit+1)
	require.LessOrEqual(t, len(atCap.Bytes())+growth, HeadBlobMax)
	require.NotNil(t, over.Bytes(), "the previous head itself stays within HEAD_BLOB_MAX")
	require.Greater(t, len(over.Bytes())+growth, HeadBlobMax)

	st, _, got := verifyBundleStep(tr, hw, atCap, measured(t, atCap), bundle, nil)
	require.Equal(t, ReasonOK, got, "a head at the cap is accepted")
	require.LessOrEqual(t, len(st.after.Bytes()), HeadBlobMax)
	require.Equal(t, measured(t, st.after), st.afterIdx, "the carried index length is the after-head's")

	_, _, got = verifyBundleStep(tr, hw, over, measured(t, over), bundle, nil)
	require.Equal(t, ReasonMalformed, got, "a head over the cap is refused as malformed")
}

// The incremental accounting is exact on every committed chain: after every
// step of every chain vector, the index length carried forward equals the
// after-head's measured one, and the after-head encodes.
func TestVerifyChainIndexAccountingMatchesEveryChainVector(t *testing.T) {
	steps := 0
	for _, v := range loadVectors(t).Vectors {
		if v.Type != "chain" {
			continue
		}
		tr, r := trustOf(t, v)
		if r != ReasonOK {
			continue
		}
		// verifyChainSteps keeps no head per step (chainStep), so walk its loop here.
		hw, bundles := hwOf(v.HW), listB64(t, v.Bundles)
		seen, _, _ := signedStatus(tr, hw, mustB64(t, v.BytesB64))
		pin := chainSeq0Pin(prevHeadOf(t, v), bundles, seen, memOf(t, v.Mem))
		head := clipIndexes(prevHeadOf(t, v))
		idx := measured(t, head)
		for i, b := range bundles {
			st, _, r := verifyBundleStep(tr, hw, head, idx, b, pin)
			if r != ReasonOK {
				break
			}
			require.NotNil(t, st.after.Bytes(), "%s step %d: the after-head encodes", v.Name, i)
			require.Equal(t, measured(t, st.after), st.afterIdx, "%s step %d", v.Name, i)
			head, idx = st.after, st.afterIdx
			steps++
		}
	}
	require.Positive(t, steps, "the vectors exercise multi-step chains")
}

// The measured and the whole-encoding E3-08 checks agree on every committed
// bundle: same Reason, same after-head bytes, and the carried index length is
// the after-head's.
func TestVerifyChainStepMeasuredAgreesWithEncodingOnEveryBundleVector(t *testing.T) {
	n := 0
	for _, v := range loadVectors(t).Vectors {
		if v.Type != "bundle-verify" || v.Fix != "" {
			continue
		}
		tr, r := trustOf(t, v)
		require.Equal(t, ReasonOK, r)
		hw, bundle, prev := hwOf(v.HW), mustB64(t, v.BytesB64), clipIndexes(prevHeadOf(t, v))
		stM, supM, rM := verifyBundleStep(tr, hw, prev, measured(t, prev), bundle, nil)
		stE, supE, rE := verifyBundleStep(tr, hw, prev, -1, bundle, nil)
		require.Equal(t, rE, rM, v.Name)
		require.Equal(t, supE, supM, v.Name)
		if rM == ReasonOK {
			require.Equal(t, stE.after.Bytes(), stM.after.Bytes(), v.Name)
			require.Equal(t, measured(t, stM.after), stM.afterIdx, v.Name)
			n++
		}
	}
	require.Positive(t, n)
}

// headMove.fits answers what after.Bytes() != nil answers when an edit adds a
// record the codec refuses (a root_index record with no segment) and when the
// edits are valid, and the length it carries is the after-head's.
func TestHeadMoveFitsAgreesWithEncoding(t *testing.T) {
	root := testPoint(t)
	prev := GenesisHead(newID(), newID())
	m := headMove{}
	m.appendResult(&prev, parsedBundle{res: Result{Op: OpEnrollNew, State: StateActive, ActiveRoot: root},
		entry: d48("e1"), resBytes: []byte("r1")}, "")
	idx, ok := m.fits(prev, measured(t, GenesisHead(prev.RealmID, prev.AccountID)))
	require.True(t, ok)
	require.NotNil(t, prev.Bytes())
	require.Equal(t, measured(t, prev), idx)

	bad, mb := prev, headMove{}
	mb.appendResult(&bad, parsedBundle{res: Result{Op: OpRotateRoot, State: StateActive, ActiveRoot: testPoint(t)},
		entry: d48("e2"), resBytes: []byte("r2")}, "") // a rotated root with no segment
	require.Nil(t, bad.Bytes(), "the codec refuses a root_index record with no segment")
	_, ok = mb.fits(bad, idx)
	require.False(t, ok, "the incremental check refuses it too")

	good, mg := prev, headMove{}
	mg.appendResult(&good, parsedBundle{res: Result{Op: OpRotateRoot, State: StateActive, ActiveRoot: testPoint(t)},
		entry: d48("e2"), resBytes: []byte("r2")}, AssuranceUnverified)
	gi, ok := mg.fits(good, idx)
	require.True(t, ok)
	require.Equal(t, measured(t, good), gi)
}

// sentinel fills the spare capacity of a slice, so a write past its length is
// visible to the test.
const sentinel = "sentinel"

func withSpareCapacity(h ChainHead) ChainHead {
	grow := func(xs []string) []string {
		out := make([]string, len(xs), len(xs)+3)
		copy(out, xs)
		for i := len(xs); i < cap(out); i++ {
			out[:cap(out)][i] = sentinel
		}
		return out
	}
	h.SeqIndex, h.RootIndex, h.RecoverIndex = grow(h.SeqIndex), grow(h.RootIndex), grow(h.RecoverIndex)
	ek := make([][]byte, len(h.EKIndex), len(h.EKIndex)+3)
	copy(ek, h.EKIndex)
	for i := len(h.EKIndex); i < cap(ek); i++ {
		ek[:cap(ek)][i] = []byte(sentinel)
	}
	h.EKIndex = ek
	return h
}

func deepCopyHead(h ChainHead) ChainHead {
	h.SeqIndex, h.RootIndex, h.RecoverIndex = slices.Clone(h.SeqIndex), slices.Clone(h.RootIndex), slices.Clone(h.RecoverIndex)
	h.EKIndex = slices.Clone(h.EKIndex)
	return h
}

func requireUntouched(t *testing.T, name string, got, want ChainHead) {
	t.Helper()
	require.Equal(t, want, deepCopyHead(got), "%s: the caller's head keeps its records", name)
	for _, xs := range [][]string{got.SeqIndex, got.RootIndex, got.RecoverIndex} {
		for _, s := range xs[len(xs):cap(xs)] {
			require.Equal(t, sentinel, s, "%s: nothing is written past the caller's length", name)
		}
	}
	for _, b := range got.EKIndex[len(got.EKIndex):cap(got.EKIndex)] {
		require.Equal(t, sentinel, string(b), "%s: nothing is written past the caller's length", name)
	}
}

// No verification writes the arrays of the head it is given: not its records,
// and not the spare capacity past its length, on every committed bundle and
// chain (VerifyBundle, VerifyChain), accepted or refused.
func TestVerifyChainLeavesTheCallersHeadUntouched(t *testing.T) {
	for _, v := range loadVectors(t).Vectors {
		switch v.Type {
		case "bundle-verify":
			tr, r := trustOf(t, v)
			require.Equal(t, ReasonOK, r)
			var fix *Fix
			if v.Fix != "" {
				fix = &Fix{Status: mustB64(t, v.Fix)}
			}
			prev := withSpareCapacity(prevHeadOf(t, v))
			want := deepCopyHead(prev)
			_, _ = VerifyBundle(tr, hwOf(v.HW), prev, mustB64(t, v.BytesB64), fix)
			requireUntouched(t, v.Name, prev, want)
		case "chain":
			tr, r := trustOf(t, v)
			if r != ReasonOK {
				continue
			}
			use, ok := ParseUse(v.Use)
			require.True(t, ok)
			from := withSpareCapacity(prevHeadOf(t, v))
			want := deepCopyHead(from)
			_, _, _, _ = VerifyChain(tr, hwOf(v.HW), from, chainInputOf(t, v), memOf(t, v.Mem), nowOf(t, v.NowMs), use)
			requireUntouched(t, v.Name, from, want)
		}
	}
}

// H_pend and H_act never share an array either one writes: both after-heads
// can be built from one persisted head, and each keeps its own records,
// whether the activation replaced seq_index record n and inserted into
// root_index and recover_index (a recover's new root becoming active) or left
// the indexes alone (Pe, no pending root). The persisted head's index slices
// carry spare capacity and records that sort AFTER every inserted one ("~…"
// above any base64 text, and the all-'z' EK digest), so an insertion made in
// place would shift a record the persisted head can see.
func TestFormHypothesisKeepsHPendAndHActApart(t *testing.T) {
	pendRoot := testPoint(t)
	zz := bytes.Repeat([]byte{0xCF, 0x3C, 0xF3}, 16) // base64 "zzz…z", the last text in EN3 order
	for _, tc := range []struct {
		name    string
		prevRes Result
		act     pub
	}{
		{"activation binds a new root", Result{Seq: 2, Op: OpRecover, PendingRoot: pendRoot,
			Assurance: AssuranceRecovery}, pub{State: StateActive}},
		{"activation installs no root", Result{Seq: 2, Op: OpErase}, pub{State: StateErased}},
	} {
		prev := withSpareCapacity(ChainHead{SeqIndex: []string{SeqRecord(d48("e1"), d48("r1")), SeqRecord(d48("e2"), nil)},
			RootIndex: []string{"~1", "~2"}, RecoverIndex: []string{"~r"}, EKIndex: [][]byte{zz},
			PendingRecoveryKey: testPoint(t)})
		want := deepCopyHead(prev)
		hPend, _ := formHypothesis(prev, tc.prevRes, hyp{p: pub{State: StatePending}})
		hAct, _ := formHypothesis(prev, tc.prevRes, hyp{p: tc.act, act: true})
		pendAfter, actAfter := hPend, hAct
		appendResult(&pendAfter, parsedBundle{res: Result{Op: OpErase, State: StatePending}, entry: d48("pend"),
			ekb: &EKBinding{EKPublicKey: []byte("ek-pend"), EKGeneration: 1}}, "")
		appendResult(&actAfter, parsedBundle{res: Result{Op: OpErase, State: StatePending}, entry: d48("act")}, "")

		require.Equal(t, SeqRecord(d48("pend"), nil), pendAfter.SeqIndex[2], "%s: H_pend's append survives H_act's", tc.name)
		require.Equal(t, SeqRecord(d48("act"), nil), actAfter.SeqIndex[2], tc.name)
		require.Equal(t, [][]byte{Digest([]byte("ek-pend")), zz}, pendAfter.EKIndex, tc.name)
		require.Equal(t, want, deepCopyHead(prev), "%s: the persisted head keeps its records", tc.name)
		require.Equal(t, want.SeqIndex, hPend.SeqIndex, "%s: H_pend reads the persisted head", tc.name)
		for _, xs := range [][]string{prev.RootIndex, prev.RecoverIndex} {
			for _, s := range xs[len(xs):cap(xs)] {
				require.Equal(t, sentinel, s, "%s: no insertion lands in the persisted head's array", tc.name)
			}
		}
		for _, b := range prev.EKIndex[len(prev.EKIndex):cap(prev.EKIndex)] {
			require.Equal(t, sentinel, string(b), "%s: no insertion lands in the persisted head's array", tc.name)
		}
		if len(tc.prevRes.PendingRoot) > 0 {
			require.Equal(t, SeqRecord(d48("e2"), Digest(pendRoot)), hAct.SeqIndex[1], "%s: H_act binds the root", tc.name)
			require.Len(t, hAct.RootIndex, 3, "%s: H_act records the activated root", tc.name)
			require.Len(t, hAct.RecoverIndex, 2, "%s: H_act records the recover's key", tc.name)
			require.Equal(t, want.SeqIndex[1], pendAfter.SeqIndex[1], "%s: H_pend does not see H_act's record", tc.name)
			require.Equal(t, want.RootIndex, pendAfter.RootIndex, "%s: H_pend does not see H_act's root", tc.name)
		}
	}
}

// A chain keeps every step for VerifyDeviceLedger, so a step that kept a head
// kept that step's copy of each index it inserted into: memory quadratic in
// the chain (R1/N-1). chainStep may hold no ChainHead, directly or nested.
func TestChainStepKeepsNoHead(t *testing.T) {
	head := reflect.TypeFor[ChainHead]()
	var holds func(reflect.Type) bool
	holds = func(ty reflect.Type) bool {
		switch ty.Kind() {
		case reflect.Struct:
			if ty == head {
				return true
			}
			for f := range ty.Fields() {
				if holds(f.Type) {
					return true
				}
			}
		case reflect.Pointer, reflect.Slice, reflect.Array:
			return holds(ty.Elem())
		}
		return false
	}
	require.False(t, holds(reflect.TypeFor[chainStep]()))
	require.True(t, holds(reflect.TypeFor[stepOut]()), "control: the walk's own step does carry its head")
}
