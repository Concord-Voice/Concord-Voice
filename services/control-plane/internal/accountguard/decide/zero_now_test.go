package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round 3, red-team F3 (architect rulings-3): Input.Now is Decide's only clock, and no
// legitimate snapshot has a zero Now. A zero Now makes counts() false for every credential, which
// contestable() would read as "immature", so T5 would contest mature factors at equal class. Every
// action therefore refuses a zero Now with RowNone, and finalize still stamps the action's Source.

// TestDecide_ZeroNowNeverContestsAMatureFactor promotes the red-team PoC: a runner that never set
// Input.Now (or a decode that dropped it) must not let one phished code contest the owner's
// mature key.
func TestDecide_ZeroNowNeverContestsAMatureFactor(t *testing.T) {
	ownerKey := Cred{Ref: cref(CredWebAuthn, "wa0-owner"), MaturesAt: t0.Add(-428 * time.Hour),
		EnrolledAt: t0.Add(-500 * time.Hour), EnrolClass: C2} // mature for 17 days
	phished := credMaturing(fxTOTP, legacyInstant) // one real-time phished TOTP code
	creds := []Cred{credMaturing(fxPW, legacyInstant), phished, ownerKey}

	control := Decide(Input{Now: t0, Act: Contest{}, Proof: Proof{Used: []CredRef{fxTOTP}}, Creds: creds})
	require.Equal(t, VerdictRefused, control.Verdict, "positive control: a mature key is not contestable at a real Now")

	out := Decide(Input{Act: Contest{}, Proof: Proof{Used: []CredRef{fxTOTP}}, Creds: creds})
	assert.Equal(t, VerdictRefused, out.Verdict, "a zero Now must not contest anything; got %+v", out)
	assert.Equal(t, RowNone, out.Row)
	assert.Empty(t, effectsOfKind(out, EffectContest), "the owner's mature key %v is contested at once", ownerKey.Ref)
}

// zeroNowCase is one action over a snapshot that Decide accepts at t0.
type zeroNowCase struct {
	name   string
	in     Input
	source Source
}

func zeroNowCases() []zeroNowCase {
	bk := cref(CredBackupAddress, "bk")
	earlier := t0.Add(-10 * time.Hour)
	longMature := []Cred{credMaturing(fxPW, earlier), credMaturing(fxAddr, earlier), credMaturing(fxTOTP, earlier),
		credMaturing(fxRK, earlier)}

	cancelIn := inputAt(Cancel{IDs: []string{"r1"}}, cancelCreds(), fxPW)
	cancelIn.Reqs = []Req{pendingSSORemoval("r1")}

	resolveIn := inputAt(Resolve{Group: "g", Chosen: "r1"}, []Cred{credMaturing(fxRK, earlier), credMaturing(fxTOTP, earlier)}, fxRK, fxTOTP)
	resolveIn.Reqs = enDisputedPair()

	completeIn := completeInput(dueRemoval())

	completeBG := dueRemoval()
	completeBG.ID, completeBG.Kind, completeBG.Targets = "bg", ReqBreakGlass, nil
	completeBGIn := inputAt(Complete{ID: "bg"}, matureAtOpen())
	completeBGIn.Reqs = []Req{completeBG}

	contestIn := inputAt(Contest{}, append(ownerCreds(), phisherCred(cref(CredWebAuthn, "phisher"), C2)), fxTOTP)

	expired := matureCred(fxTOTP)
	expired.ContestedAt = t0.Add(-DeltaCP - time.Hour)
	expireIn := inputAt(Expire{}, []Cred{matureCred(fxPW), expired})

	return []zeroNowCase{
		{"SignOutOthers", inputAt(SignOutOthers{}, credsOf(fxPW, fxAddr), fxPW), SourceUserSession},
		{"SignOutEverywhere", inputAt(SignOutEverywhere{}, standardCreds(), fxTOTP), SourceUserSession},
		{"Remove (T2)", inputAt(Remove{Target: fxSSO}, standardCreds(), fxTOTP), SourceUserSession},
		{"Remove (T3)", inputAt(Remove{Target: fxTOTP}, standardCreds(), fxPW, fxTOTP), SourceUserSession},
		{"SelfRemoveBackup", inputAt(SelfRemoveBackup{Target: bk}, append(credsOf(bk), credsOf(fxPW)...), bk), SourceUserSession},
		{"Enrol", inputAt(Enrol{Ref: cref(CredTOTP, "new")}, credsOf(fxPW, fxAddr), fxPW), SourceUserSession},
		{"Contest", contestIn, SourceUserSession},
		{"OpenBreakGlass (T6)", inputAt(OpenBreakGlass{}, longMature, fxAddr), SourceBreakGlass},
		{"OpenBreakGlass (T6c)", inputAt(OpenBreakGlass{}, longMature, fxRK, fxTOTP), SourceBreakGlass},
		{"Cancel", cancelIn, SourceUserSession},
		{"Complete (credential_remove)", completeIn, SourceUserSession},
		{"Complete (break_glass)", completeBGIn, SourceBreakGlass},
		{"Resolve", resolveIn, SourceUserSession},
		{"Expire", expireIn, SourceUserSession},
	}
}

// TestDecide_ZeroNowRefusesEveryAction: for every action, the snapshot is accepted at t0 (the
// positive control, so the refusal is the zero Now and nothing else), and the same snapshot with
// a zero Now is refused at RowNone with the action's source, no effects and no notices.
func TestDecide_ZeroNowRefusesEveryAction(t *testing.T) {
	for _, tc := range zeroNowCases() {
		t.Run(tc.name, func(t *testing.T) {
			control := Decide(tc.in)
			require.NotEqual(t, VerdictRefused, control.Verdict, "positive control: accepted at t0: %+v", control)
			require.Equal(t, tc.source, control.Source)

			zero := tc.in
			zero.Now = time.Time{}
			out := Decide(zero)
			assert.Equal(t, Outcome{Row: RowNone, Verdict: VerdictRefused, Source: tc.source}, out)
			assert.Nil(t, out.Effects)
			assert.Nil(t, out.Notices)
		})
	}
	// An action that is not one of the eleven is refused too, and still names a source.
	out := Decide(Input{Act: nil})
	assert.Equal(t, Outcome{Row: RowNone, Verdict: VerdictRefused, Source: SourceUserSession}, out)
}

// TestEnumerate_NoGeneratorEmitsAZeroNow: Decide now refuses a zero Now, so a generator that
// emitted one would silently shrink its own coverage. Every family of generator 1 (and its P4
// filtered twin), the vectors and the world model must carry a non-zero Now.
func TestEnumerate_NoGeneratorEmitsAZeroNow(t *testing.T) {
	calls := 0
	check := func(c enCall) {
		calls++
		if c.in.Now.IsZero() {
			t.Fatalf("generator emitted a zero Now: %s", c.label())
		}
		if c.filtered != nil && c.filtered.Now.IsZero() {
			t.Fatalf("generator emitted a zero Now in the filtered twin: %s", c.label())
		}
	}
	for _, p4 := range []bool{false, true} {
		enEmitRemove(p4, check)
		enEmitOpenBG(p4, check)
		enEmitCancel(p4, check)
		enEmitContest(p4, check)
		enEmitResolve(p4, check)
	}
	enEmitSignOuts(check)
	enEmitEnrolAndSelfRemove(check)
	enEmitComplete(check)
	enEmitExpire(check)
	assert.Greater(t, calls, 150000, "the families emitted fewer calls than they should")

	for _, v := range loadVectors(t) {
		in, err := v.Input.build()
		require.NoError(t, err, v.Name)
		assert.False(t, in.Now.IsZero(), "vector %s has a zero Now", v.Name)
	}
	assert.False(t, newWorld().input(Expire{}).Now.IsZero(), "the world model starts at a zero Now")
}

// TestEnumerate_CompleteFamilyReachesTheSameNumberOfOutcomes pins enEmitComplete's size and the
// share of its calls that reach a row, so a fixture change that made the family vacuous (or H3's
// CompletesAt check, which must not reject a generated row) cannot pass unnoticed. The figures
// were measured before the round-3 changes.
func TestEnumerate_CompleteFamilyReachesTheSameNumberOfOutcomes(t *testing.T) {
	calls, withRow := 0, 0
	byVerdict := map[Verdict]int{}
	enEmitComplete(func(c enCall) {
		calls++
		out := Decide(c.in)
		if out.Row != RowNone {
			withRow++
		}
		byVerdict[out.Verdict]++
	})
	assert.Equal(t, 4608, calls)
	assert.Equal(t, 1920, withRow, "non-RowNone Complete outcomes")
	assert.Equal(t, map[Verdict]int{VerdictRefused: 2688, VerdictApplied: 600, VerdictHeld: 520, VerdictNoChange: 800}, byVerdict)
}
