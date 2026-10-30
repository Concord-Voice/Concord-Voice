package decide

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plan Task 11, generator 3: every sequence of length 1 to 3 (4 + 16 + 64 = 84) over
// {T1, T1b, rotation, origin removal}, run on the world model. Parent §7.6 P3: no sequence of
// T1/T1b actions or epoch rotations changes a request's status (F4: a request is bound to
// neither the credential epoch nor the survival of its origin credential).

type p3Step uint8

const (
	p3T1            p3Step = iota // SignOutOthers with {password, totp}, then the runner rotates the password
	p3T1b                         // SignOutEverywhere with {totp}
	p3Rotation                    // an epoch rotation: Input has no epoch, so the world does not change
	p3OriginRemoval               // Remove{sso} with {totp}: T2, immediate; sso is an origin of bg
	p3NumSteps
)

func (s p3Step) String() string {
	return [...]string{"T1", "T1b", "rotation", "origin-removal"}[s]
}

// p3Sequences returns every sequence of length 1..3.
func p3Sequences() [][]p3Step {
	var out [][]p3Step
	var rec func(prefix []p3Step)
	rec = func(prefix []p3Step) {
		if len(prefix) > 0 {
			out = append(out, prefix)
		}
		if len(prefix) == 3 {
			return
		}
		for s := p3Step(0); s < p3NumSteps; s++ {
			rec(append(append([]p3Step(nil), prefix...), s))
		}
	}
	rec(nil)
	return out
}

// p3StartWorld: mature pw, addr, sso, totp and a backup set; an immature phisher wa1 enrolled at
// C2 after the totp matured; a pending break-glass bg (origins {pw v1, sso}); a pending
// credential_remove cr that targets the backup set (a target no step touches; a request that
// targeted the password is deliberately absent, because T1's rotation correctly supersedes it at
// T11); and a disputed pair d1, d2.
//
// bg carries Targets [pw v1] although a real break-glass row has none (ruling 4): Decide must
// never read them, and the snapshot is what makes a "break-glass is fenced like the others"
// defect (mutation #38) visible once T1 moves the password to v2.
func p3StartWorld() *world {
	phisher := matureCred(fxWA1)
	phisher.MaturesAt = t0.Add(70 * time.Hour)
	phisher.EnrolledAt = t0.Add(-30 * time.Minute)
	phisher.EnrolClass = C2
	w := newWorld(matureCred(fxPW), matureCred(fxAddr), matureCred(fxSSO), matureCred(fxTOTP), matureCred(fxPaper), phisher)

	opened := t0.Add(-2 * time.Hour)
	w.reqs = []Req{
		{ID: "bg", Kind: ReqBreakGlass, Status: StatusPending, OriginClass: C1, Origins: []CredRef{fxPW, fxSSO},
			Targets: []CredRef{fxPW}, OpenedAt: opened, CompletesAt: opened.Add(DeltaCP)},
		{ID: "cr", Kind: ReqCredentialRemove, Status: StatusPending, OriginClass: C2, Origins: []CredRef{fxTOTP},
			Targets: []CredRef{fxPaper}, OpenedAt: opened, CompletesAt: opened.Add(DeltaCP)},
		{ID: "d1", Kind: ReqBreakGlass, Status: StatusDisputed, OriginClass: C1, Origins: []CredRef{cref(CredPassword, "origin-1")},
			OpenedAt: opened, CompletesAt: opened.Add(DeltaCP), DisputedAt: t0.Add(-time.Hour), DisputeGroup: "g"},
		{ID: "d2", Kind: ReqBreakGlass, Status: StatusDisputed, OriginClass: C1, Origins: []CredRef{cref(CredPassword, "origin-2")},
			OpenedAt: opened, CompletesAt: opened.Add(DeltaCP), DisputedAt: t0.Add(-time.Hour), DisputeGroup: "g"},
	}
	return w
}

// p3Run performs one step on the world and reports the outcome of its Decide call, if any.
func p3Run(t *testing.T, w *world, step p3Step) (Outcome, bool) {
	t.Helper()
	var out Outcome
	switch step {
	case p3T1:
		out = w.decide(t, SignOutOthers{}, w.passwordRef(), fxTOTP)
		w.apply(t, out)
		if out.Verdict == VerdictApplied {
			w.rotatePassword()
		}
	case p3T1b:
		out = w.decide(t, SignOutEverywhere{}, fxTOTP)
		w.apply(t, out)
	case p3OriginRemoval:
		out = w.decide(t, Remove{Target: fxSSO}, fxTOTP)
		w.apply(t, out)
	default:
		return Outcome{}, false
	}
	return out, true
}

// p3Result is what a Complete decided. Row and Verdict alone cannot tell a completed
// break-glass from one cancelled as superseded (both are T11 and applied), so whether it
// completes and why it cancels are part of the result.
type p3Result struct {
	Row       Row
	Verdict   Verdict
	Completes bool
	Cancels   []CancelReason
}

// p3Completions advances the world by delta_cp, accepts every request's notice, and decides a
// Complete on each of the four requests.
func p3Completions(t *testing.T, w *world) map[string]p3Result {
	t.Helper()
	w.advance(DeltaCP)
	got := map[string]p3Result{}
	for _, id := range []string{"bg", "cr", "d1", "d2"} {
		w.accept(id)
	}
	for _, id := range []string{"bg", "cr", "d1", "d2"} {
		out := w.decide(t, Complete{ID: id})
		res := p3Result{Row: out.Row, Verdict: out.Verdict}
		for _, e := range out.Effects {
			if e.Kind == EffectComplete {
				res.Completes = true
			}
			if e.Kind == EffectCancel {
				res.Cancels = append(res.Cancels, e.Reason)
			}
		}
		got[id] = res
	}
	return got
}

func TestP3_SignOutsAndRotationsNeverChangeARequest(t *testing.T) {
	startFacts := p3StartWorld().facts()
	require.Len(t, startFacts, 4)

	// The baseline: the starting world, advanced the same way. bg and cr complete; the disputed
	// pair is not pending, so a Complete is refused.
	baseline := p3Completions(t, p3StartWorld())
	require.Equal(t, map[string]p3Result{
		"bg": {Row: RowT11, Verdict: VerdictApplied, Completes: true},
		"cr": {Row: RowT11, Verdict: VerdictApplied, Completes: true},
		"d1": {Row: RowNone, Verdict: VerdictRefused},
		"d2": {Row: RowNone, Verdict: VerdictRefused},
	}, baseline, "positive control: the baseline must complete bg and cr")

	t.Run("positive control: each step does something on its own", func(t *testing.T) {
		w := p3StartWorld()
		out, _ := p3Run(t, w, p3T1)
		require.Equal(t, VerdictApplied, out.Verdict)
		assert.EqualValues(t, 2, w.passwordRef().Version, "T1 moves the password to v2")

		w = p3StartWorld()
		out, _ = p3Run(t, w, p3T1b)
		require.Equal(t, VerdictApplied, out.Verdict)
		i := w.credIndex(fxWA1)
		require.GreaterOrEqual(t, i, 0)
		assert.False(t, w.creds[i].ContestedAt.IsZero(), "T1b contests the phisher's factor (3-A)")

		w = p3StartWorld()
		out, _ = p3Run(t, w, p3OriginRemoval)
		require.Equal(t, VerdictApplied, out.Verdict)
		assert.Equal(t, -1, w.credIndex(fxSSO), "the origin credential is gone")
	})

	forbidden := []EffectKind{EffectCancel, EffectDispute, EffectReturnToPending, EffectComplete, EffectOpen, EffectHold}
	seqs := p3Sequences()
	require.Len(t, seqs, 84, "4 + 16 + 64")
	applied := map[p3Step]int{}
	for _, seq := range seqs {
		names := make([]string, len(seq))
		for i, s := range seq {
			names[i] = s.String()
		}
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			w := p3StartWorld()
			for i, step := range seq {
				out, ran := p3Run(t, w, step)
				if ran {
					// (i) a sign-out, a rotation or an origin removal touches no request.
					for _, e := range out.Effects {
						for _, k := range forbidden {
							require.NotEqual(t, k, e.Kind, "step %d (%s) emitted %+v", i, step, e)
						}
					}
					if out.Verdict == VerdictApplied {
						applied[step]++
					}
				}
				// (ii) every request keeps its status, completion instant and held time.
				require.Equal(t, startFacts, w.facts(), "step %d (%s) changed a request", i, step)
			}
			// (iii) the requests complete (or refuse) exactly as in the world no step touched.
			require.Equal(t, baseline, p3Completions(t, w))
		})
	}
	for _, step := range []p3Step{p3T1, p3T1b, p3OriginRemoval} {
		assert.Positive(t, applied[step], "no sequence applied step %s", step)
	}
}

// epochFieldNames walks a type and returns the path of every struct field whose name contains
// "epoch", through slices, arrays, maps, pointers and nested structs.
func epochFieldNames(typ reflect.Type, path string, seen map[reflect.Type]bool, found *[]string) {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || typ == reflect.TypeOf(time.Time{}) || seen[typ] {
		return
	}
	seen[typ] = true
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if strings.Contains(strings.ToLower(f.Name), "epoch") {
			*found = append(*found, path+"."+f.Name)
		}
		epochFieldNames(f.Type, path+"."+f.Name, seen, found)
	}
}

// TestP3_InputCarriesNoEpoch: P3 holds because Decide cannot see the credential epoch. If Input
// (or anything it holds, including every Action) gained an epoch field, the depth-3 sequences
// above would stop modelling the system. The detector is proven live on Outcome, which does
// carry RotateEpoch.
func TestP3_InputCarriesNoEpoch(t *testing.T) {
	var found []string
	epochFieldNames(reflect.TypeOf(Input{}), "Input", map[reflect.Type]bool{}, &found)
	for _, act := range []Action{SignOutOthers{}, SignOutEverywhere{}, Remove{}, SelfRemoveBackup{}, Enrol{}, Contest{},
		OpenBreakGlass{}, Cancel{}, Complete{}, Resolve{}, Expire{}} {
		epochFieldNames(reflect.TypeOf(act), reflect.TypeOf(act).Name(), map[reflect.Type]bool{}, &found)
	}
	assert.Empty(t, found, "the decision input must not carry an epoch")

	var control []string
	epochFieldNames(reflect.TypeOf(Outcome{}), "Outcome", map[reflect.Type]bool{}, &control)
	assert.Equal(t, []string{"Outcome.RotateEpoch"}, control, "positive control: the walk finds Outcome.RotateEpoch")
}
