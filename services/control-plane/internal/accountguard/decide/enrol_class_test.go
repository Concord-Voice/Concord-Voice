package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round 3, red-team F2 (architect rulings-3): the 000189 trigger NULLs password_enrol_class on
// every password change unless the writer sets it, and Decide reads NULL as ClassNone, which
// contestThreshold maps to the rank floor C1. A runner that does not write the class lets a lone
// mailbox (C1) contest the owner's fresh password. Decide already knows the class, so the
// contract is now a field: Outcome.EnrolClass is the class the runner writes, with enrolled_at,
// on the credential the outcome writes.
//
//	T1                  ClassOf(Proof, Creds, Now)       (ClassNone when no mature credential proved it)
//	T4                  ClassOf(Proof, Creds, Now)
//	T6c                 C3
//	T11 completion of a break_glass or an address_change  Req.OriginClass
//	every other outcome (a credential_remove completion included)  ClassNone

// enrolClassWant is the expected EnrolClass of each zeroNowCases control outcome.
func enrolClassWant(in Input, name string) Class {
	switch name {
	case "SignOutOthers", "Enrol":
		return ClassOf(in.Proof, in.Creds, in.Now)
	case "OpenBreakGlass (T6c)":
		return C3
	case "Complete (break_glass)":
		return C1 // dueRemoval's OriginClass; a credential_remove completion writes no credential
	}
	return ClassNone
}

func TestOutcome_EnrolClassByRow(t *testing.T) {
	for _, tc := range zeroNowCases() {
		t.Run(tc.name, func(t *testing.T) {
			out := Decide(tc.in)
			require.NotEqual(t, VerdictRefused, out.Verdict, "positive control: the action is accepted")
			assert.Equal(t, enrolClassWant(tc.in, tc.name), out.EnrolClass)
		})
	}
}

func TestOutcome_EnrolClassPinsTheProofClass(t *testing.T) {
	t.Run("T1 at C1 and at C2", func(t *testing.T) {
		creds := []Cred{matureCred(fxPW), matureCred(fxAddr), matureCred(fxTOTP)}
		assert.Equal(t, C2, Decide(inputAt(SignOutOthers{}, creds, fxPW, fxTOTP)).EnrolClass)
		assert.Equal(t, C1, Decide(inputAt(SignOutOthers{}, credsOf(fxPW, fxAddr), fxPW)).EnrolClass)
	})
	t.Run("T1 with an immature password is ClassNone: no mature credential proved it (T5)", func(t *testing.T) {
		creds := []Cred{credMaturing(fxPW, t0.Add(48*time.Hour)), matureCred(fxAddr)}
		out := Decide(inputAt(SignOutOthers{}, creds, fxPW))
		require.Equal(t, VerdictApplied, out.Verdict)
		assert.Equal(t, ClassNone, out.EnrolClass)
	})
	t.Run("T4 at the class of its proof, replacing the runner-computed class", func(t *testing.T) {
		creds := []Cred{matureCred(fxPW), matureCred(fxAddr), matureCred(fxTOTP), matureCred(fxRK)}
		out := Decide(inputAt(Enrol{Ref: cref(CredWebAuthn, "new")}, creds, fxPW, fxTOTP))
		require.Equal(t, VerdictApplied, out.Verdict)
		assert.Equal(t, C2, out.EnrolClass)

		in := inputAt(Enrol{Ref: cref(CredRecoveryDevice, "new")}, creds, fxRK, fxTOTP)
		out = Decide(in)
		require.Equal(t, VerdictApplied, out.Verdict)
		assert.Equal(t, C3, out.EnrolClass, "a TOTP + anchor proof is C3")
	})
	t.Run("T6c is C3 whatever the request's other fields", func(t *testing.T) {
		creds := []Cred{matureCred(fxPW), matureCred(fxAddr), matureCred(fxTOTP), matureCred(fxRK)}
		out := Decide(inputAt(OpenBreakGlass{Reset: true}, creds, fxRK, fxTOTP))
		require.Equal(t, RowT6c, out.Row)
		assert.Equal(t, C3, out.EnrolClass)
	})
}

func TestOutcome_EnrolClassOfAT11CompletionThatWritesACredentialIsTheOriginClass(t *testing.T) {
	for _, oc := range []Class{C1, C2, C3} {
		for _, kind := range []ReqKind{ReqAddressChange, ReqBreakGlass} {
			r := dueRemoval()
			r.Kind, r.OriginClass = kind, oc
			out := Decide(completeInput(r))
			require.Equal(t, VerdictApplied, out.Verdict, "kind %d class %d", kind, oc)
			require.Len(t, effectsOfKind(out, EffectComplete), 1)
			assert.Equal(t, oc, out.EnrolClass, "kind %d class %d", kind, oc)
		}
	}
}

// Architect ruling on the open question: EnrolClass is the class written with enrolled_at on the
// credential the outcome writes, and a removal writes none. A credential_remove completion (and
// the kinds that complete with no credential effect) therefore carries ClassNone.
func TestOutcome_EnrolClassOfACredentialRemoveCompletionIsZero(t *testing.T) {
	for _, oc := range []Class{C1, C2, C3} {
		for _, kind := range []ReqKind{ReqCredentialRemove, ReqSocialRecovery, ReqCircleChange} {
			r := dueRemoval()
			r.Kind, r.OriginClass = kind, oc
			out := Decide(completeInput(r))
			require.Equal(t, VerdictApplied, out.Verdict, "kind %d class %d", kind, oc)
			require.Len(t, effectsOfKind(out, EffectComplete), 1, "positive control: the request completes")
			if kind == ReqCredentialRemove {
				assert.Len(t, effectsOfKind(out, EffectRemove), 1, "positive control: it removes its target")
			}
			assert.Equal(t, ClassNone, out.EnrolClass, "kind %d class %d", kind, oc)
		}
	}
}

func TestOutcome_EnrolClassIsZeroWhenNothingIsWritten(t *testing.T) {
	cases := map[string]func(r *Req){
		"hold cap cancel": func(r *Req) { r.HeldMs = HoldCap.Milliseconds() },
		"superseded":      func(r *Req) { r.Targets = []CredRef{cref(CredSSO, "gone")} },
		"held":            func(r *Req) { r.AcceptedAddressNotice = false },
		"not yet due":     func(r *Req) { r.CompletesAt = r.CompletesAt.Add(time.Hour) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := dueRemoval()
			r.OriginClass = C3
			mutate(&r)
			out := Decide(completeInput(r))
			assert.Equal(t, ClassNone, out.EnrolClass)
			assert.Empty(t, effectsOfKind(out, EffectComplete), "the outcome completed nothing")
		})
	}
	t.Run("a refused action", func(t *testing.T) {
		out := Decide(inputAt(OpenBreakGlass{}, standardCreds())) // empty proof
		assert.Equal(t, Outcome{Row: RowNone, Verdict: VerdictRefused, Source: SourceBreakGlass}, out)
	})
}

// TestOutcome_EnrolClassKeepsALonePasswordFromBeingContested promotes the red-team PoC: a
// password written by T1 or a break-glass is contestable by a lone mailbox unless the runner
// writes Outcome.EnrolClass with it. The negative control writes ClassNone (what the trigger
// does) and shows the exploit; the contract writes the outcome's class and the contest is refused.
func TestOutcome_EnrolClassKeepsALonePasswordFromBeingContested(t *testing.T) {
	sharedMailbox := credMaturing(fxAddr, legacyInstant) // the griefer holds this mailbox
	ownerTOTP := credMaturing(fxTOTP, legacyInstant)
	pw1 := credMaturing(fxPW, legacyInstant)

	lonelyContest := func(pw2 Cred) Outcome {
		return Decide(Input{Now: t0.Add(time.Minute), Act: Contest{}, Proof: Proof{Used: []CredRef{fxAddr}},
			Creds: []Cred{pw2, sharedMailbox, ownerTOTP}})
	}
	written := func(version int64, class Class) Cred {
		return Cred{Ref: CredRef{Kind: CredPassword, ID: "pw", Version: version}, MaturesAt: t0.Add(DeltaCP),
			EnrolledAt: t0, EnrolClass: class}
	}

	t.Run("T1 rotation", func(t *testing.T) {
		t1 := inputAt(SignOutOthers{}, []Cred{pw1, sharedMailbox, ownerTOTP}, fxPW, fxTOTP)
		out := Decide(t1)
		require.Equal(t, VerdictApplied, out.Verdict)
		require.Equal(t, C2, out.EnrolClass, "the proof that enrolled the new password is C2")

		// The runner dropped the class: the exploit.
		pw2 := written(2, ClassNone)
		exploit := lonelyContest(pw2)
		assert.Equal(t, RowT5, exploit.Row)
		assert.Equal(t, VerdictApplied, exploit.Verdict, "negative control: with no class a lone mailbox contests the new password")
		assert.Equal(t, []Effect{{Kind: EffectContest, Cred: pw2.Ref}}, effectsOfKind(exploit, EffectContest))
		assert.True(t, exploit.RotateEpoch)

		// The contract: write Outcome.EnrolClass.
		got := lonelyContest(written(2, out.EnrolClass))
		assert.Equal(t, VerdictRefused, got.Verdict, "parent T5: C1 < the C2 that enrolled the password")
		assert.Empty(t, effectsOfKind(got, EffectContest))
	})

	t.Run("break-glass completion", func(t *testing.T) {
		opened := t0.Add(-DeltaCP - time.Hour)
		bg := Req{ID: "bg", Kind: ReqBreakGlass, Status: StatusPending, OriginClass: C2, Origins: []CredRef{fxTOTP},
			OpenedAt: opened, CompletesAt: opened.Add(DeltaCP), AcceptedAddressNotice: true, MatureNoticesSettled: true}
		pwContested := pw1
		pwContested.ContestedAt = opened.Add(-time.Minute)
		done := Input{Now: t0, Act: Complete{ID: "bg"}, Creds: []Cred{pwContested, sharedMailbox, ownerTOTP},
			Reqs: []Req{bg}, GuessableBudgetClear: true}
		out := Decide(done)
		require.Equal(t, RowT11, out.Row)
		require.True(t, out.SetsPassword)
		require.Equal(t, C2, out.EnrolClass, "the recovered password is enrolled at the break-glass's class")

		exploit := lonelyContest(written(3, ClassNone))
		assert.Equal(t, VerdictApplied, exploit.Verdict, "negative control: the recovered password dies on arrival")

		got := lonelyContest(written(3, out.EnrolClass))
		assert.Equal(t, VerdictRefused, got.Verdict, "parent T5: C1 < the C2 break-glass that set the password")
	})

	t.Run("T6c", func(t *testing.T) {
		creds := []Cred{pw1, sharedMailbox, ownerTOTP, credMaturing(fxRK, legacyInstant)}
		out := Decide(inputAt(OpenBreakGlass{}, creds, fxRK, fxTOTP))
		require.Equal(t, RowT6c, out.Row)
		require.Equal(t, C3, out.EnrolClass)
		assert.Equal(t, VerdictRefused, lonelyContest(written(2, out.EnrolClass)).Verdict)
		assert.Equal(t, VerdictApplied, lonelyContest(written(2, ClassNone)).Verdict, "negative control")
	})
}
