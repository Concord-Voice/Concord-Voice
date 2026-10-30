package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plan Task 5: the Decide skeleton, malformed-input handling, source selection and the
// generator-2 check of T1b's precondition. The per-row vectors live in testdata/vectors.json.

// bogusAction implements the closed Action interface from inside the package, standing in for
// an action type Decide does not know (D8).
type bogusAction struct{}

func (bogusAction) isAction() {}

func requireValidSource(t *testing.T, o Outcome) {
	t.Helper()
	require.Contains(t, []Source{SourceUserSession, SourceBreakGlass}, o.Source,
		"Decide emits only user_session and break_glass in C4 (spec §7)")
}

// TestDecide_MalformedInputRefuses (Review Focus 5, D8): a nil Act, an unknown action type, a
// zero enum, an ID naming no request, a recovery_key row named by Cancel/Complete/Resolve and
// Complete on a non-pending row each produce Refused with no effects and no panic.
func TestDecide_MalformedInputRefuses(t *testing.T) {
	strong := []CredRef{fxRK, fxTOTP}
	creds := standardCreds()
	rkPending := Req{ID: "rkp", Kind: ReqRecoveryKey, Status: StatusPending, OriginClass: C1,
		OpenedAt: t0.Add(-time.Hour), CompletesAt: t0.Add(-time.Minute), AcceptedAddressNotice: true, MatureNoticesSettled: true}
	rkDisputed := Req{ID: "rkd", Kind: ReqRecoveryKey, Status: StatusDisputed, DisputeGroup: "g",
		OpenedAt: t0.Add(-3 * time.Hour), DisputedAt: t0.Add(-2 * time.Hour)}
	zeroEnums := Req{ID: "zero"}
	completed := Req{ID: "done", Kind: ReqCredentialRemove, Status: StatusCompleted, CompletesAt: t0.Add(-time.Hour),
		Targets: []CredRef{fxSSO}, AcceptedAddressNotice: true, MatureNoticesSettled: true}

	cases := []struct {
		name string
		act  Action
	}{
		{"nil Act", nil},
		{"an action type Decide does not know", bogusAction{}},
		{"Remove of CredUnknown", Remove{Target: CredRef{Kind: CredUnknown, ID: "x", Version: 1}}},
		{"Remove of a zero ref", Remove{}},
		{"Enrol of CredUnknown", Enrol{Ref: CredRef{Kind: CredUnknown, ID: "x", Version: 1}}},
		{"Cancel naming no request", Cancel{IDs: []string{"missing"}}},
		{"Cancel with no IDs", Cancel{}},
		{"Cancel naming a recovery_key row", Cancel{IDs: []string{"rkp"}}},
		{"Cancel naming a zero-enum row", Cancel{IDs: []string{"zero"}}},
		{"Complete naming no request", Complete{ID: "missing"}},
		{"Complete naming a recovery_key row", Complete{ID: "rkp"}},
		{"Complete naming a zero-enum row", Complete{ID: "zero"}},
		{"Complete naming a completed row", Complete{ID: "done"}},
		{"Resolve of a group with no members", Resolve{Group: "nope"}},
		{"Resolve of a recovery_key group", Resolve{Group: "g", Chosen: "rkd"}},
		{"Resolve with a recovery_key member and no choice", Resolve{Group: "g"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := inputAt(tc.act, creds, strong...)
			in.Reqs = []Req{rkPending, rkDisputed, zeroEnums, completed}
			var out Outcome
			require.NotPanics(t, func() { out = Decide(in) })
			assert.Equal(t, VerdictRefused, out.Verdict)
			assert.Equal(t, RowNone, out.Row, "ruling 8: the table has no row for malformed input")
			assert.Nil(t, out.Effects)
			assert.Nil(t, out.Notices)
			assert.False(t, out.RotateEpoch || out.SetsPassword || out.Continuation || out.PermGenBump)
			requireValidSource(t, out)
		})
	}
}

// TestDecide_RefusedOpenBreakGlassCarriesBreakGlassSource (D11): a refused outcome carries the
// source its action would have, so the runner never branches before SourceTx.
func TestDecide_RefusedOpenBreakGlassCarriesBreakGlassSource(t *testing.T) {
	out := Decide(inputAt(OpenBreakGlass{}, standardCreds())) // empty proof: ClassNone
	assert.Equal(t, VerdictRefused, out.Verdict)
	assert.Equal(t, SourceBreakGlass, out.Source)
}

// TestDecide_PermGenBumpAndResync (D17): PermGenBump is true exactly when the effects contain a
// contest or a remove, and one EffectResyncMFAFlags is appended whenever they contain a
// contest, remove or restore-primary.
func TestDecide_PermGenBumpAndResync(t *testing.T) {
	t.Run("T2 removal of the TOTP", func(t *testing.T) {
		in := inputAt(Remove{Target: fxTOTP}, standardCreds(), fxRK, fxWA0)
		in.Proof = proofBound(in.Proof.Used, fxWA0)
		out := Decide(in)
		require.Equal(t, VerdictApplied, out.Verdict)
		assert.Len(t, effectsOfKind(out, EffectRemove), 1)
		assert.True(t, out.PermGenBump)
		assert.Len(t, effectsOfKind(out, EffectResyncMFAFlags), 1)
	})
	t.Run("T5 contest of a newer enrolment", func(t *testing.T) {
		// Mirrors vector t5/c2-contests-new-totp: a legacy TOTP contests the C2-enrolled
		// WebAuthn that is still maturing. The contest bumps the generation and re-syncs.
		wa := Cred{Ref: fxWA0, MaturesAt: t0.Add(70 * time.Hour), EnrolledAt: t0.Add(-time.Hour), EnrolClass: C2}
		creds := []Cred{
			matureCred(fxPW), matureCred(fxAddr), credMaturing(fxTOTP, t0.Add(-2*time.Hour)), wa,
		}
		out := Decide(inputAt(Contest{}, creds, fxTOTP))
		assert.Equal(t, VerdictApplied, out.Verdict)
		assert.Equal(t, RowT5, out.Row)
		assert.Equal(t, SourceUserSession, out.Source)
		assert.Equal(t, []Effect{{Kind: EffectContest, Cred: fxWA0}, {Kind: EffectResyncMFAFlags}}, out.Effects)
		assert.Equal(t, []Notice{{Kind: NoticeEnrolmentContested, Subject: fxWA0}}, out.Notices)
		assert.True(t, out.RotateEpoch)
		assert.True(t, out.PermGenBump)
		assert.Len(t, effectsOfKind(out, EffectResyncMFAFlags), 1)
	})
	t.Run("T3 opening bumps nothing", func(t *testing.T) {
		out := Decide(inputAt(Remove{Target: fxTOTP}, standardCreds(), fxPW, fxTOTP))
		require.Equal(t, VerdictOpened, out.Verdict)
		assert.False(t, out.PermGenBump)
		assert.Empty(t, effectsOfKind(out, EffectResyncMFAFlags))
	})
	t.Run("T1 and T4 bump nothing", func(t *testing.T) {
		out := Decide(inputAt(SignOutOthers{}, credsOf(fxPW, fxAddr), fxPW))
		require.Equal(t, VerdictApplied, out.Verdict)
		assert.False(t, out.PermGenBump)
		assert.Empty(t, effectsOfKind(out, EffectResyncMFAFlags))

		out = Decide(inputAt(Enrol{Ref: cref(CredTOTP, "new")}, credsOf(fxPW, fxAddr), fxPW))
		require.Equal(t, VerdictApplied, out.Verdict)
		assert.False(t, out.PermGenBump)
	})
}

// TestDecide_SourceByAction (D11): everything but OpenBreakGlass and a
// break-glass Complete is user_session.
func TestDecide_SourceByAction(t *testing.T) {
	creds := standardCreds()
	assert.Equal(t, SourceUserSession, Decide(inputAt(SignOutOthers{}, credsOf(fxPW), fxPW)).Source)
	assert.Equal(t, SourceUserSession, Decide(inputAt(SignOutEverywhere{}, creds, fxTOTP)).Source)
	assert.Equal(t, SourceUserSession, Decide(inputAt(Remove{Target: fxSSO}, creds, fxTOTP)).Source)
	assert.Equal(t, SourceUserSession, Decide(inputAt(Contest{}, creds, fxTOTP)).Source)
	assert.Equal(t, SourceUserSession, Decide(inputAt(Expire{}, creds)).Source)
	assert.Equal(t, SourceBreakGlass, Decide(inputAt(OpenBreakGlass{}, creds, fxAddr)).Source)
	assert.Equal(t, SourceBreakGlass, Decide(inputAt(OpenBreakGlass{}, creds, fxRK, fxTOTP)).Source, "T6c")
}

// TestT1b_QualifiesExactlyWhenANonPaperPossessionFactorCounts walks generator 2 (class_enum_test.go):
// parent §6.2 T1b needs a counted TOTP or WebAuthn. Anchor + K, two anchors and paper + K never
// qualify, whatever the class, and every case with a counted non-paper P does.
func TestT1b_QualifiesExactlyWhenANonPaperPossessionFactorCounts(t *testing.T) {
	calls := forEachGen2Case(func(c gen2Case) {
		in := Input{Now: t0, Act: SignOutEverywhere{}, Proof: c.proof(), Creds: c.creds}
		out := Decide(in)
		requireValidSource(t, out)
		if c.nonPaperCounted() == 0 {
			assert.Equal(t, VerdictRefused, out.Verdict, c.label())
			assert.False(t, out.RotateEpoch, c.label())
			assert.Nil(t, out.Effects, c.label())
			return
		}
		assert.Equal(t, VerdictApplied, out.Verdict, c.label())
		assert.Equal(t, RowT1b, out.Row, c.label())
		assert.True(t, out.RotateEpoch, c.label())
		assert.False(t, out.Continuation, c.label())
		assert.Len(t, noticesOfKind(out, NoticeSignedOutEverywhere), 1, c.label())
	})
	t.Logf("T1b decided %d times", calls)
	require.Greater(t, calls, 2000)
}

// TestDecide_RefusalRows (architect ruling 8): a refusal names the row whose condition was
// evaluated and failed; RowNone means the table has no row for that input.
func TestDecide_RefusalRows(t *testing.T) {
	mfa := []Cred{matureCred(fxPW), matureCred(fxAddr), matureCred(fxTOTP)}
	noMFA := credsOf(fxPW, fxAddr)
	bk := cref(CredBackupAddress, "bk")
	disputed := []Req{
		{ID: "r1", Kind: ReqBreakGlass, Status: StatusDisputed, DisputeGroup: "g", Origins: []CredRef{fxAddr},
			OpenedAt: t0.Add(-3 * time.Hour), DisputedAt: t0.Add(-time.Hour)},
	}
	cases := []struct {
		name string
		in   Input
		row  Row
	}{
		{"SignOutOthers failing the login floor", inputAt(SignOutOthers{}, mfa, fxPW), RowT1},
		{"SignOutEverywhere without a possession factor", inputAt(SignOutEverywhere{}, mfa, fxPW), RowT1b},
		{"Contest with an empty set", inputAt(Contest{}, mfa, fxTOTP), RowT5},
		{"Remove failing every check", inputAt(Remove{Target: fxSSO}, mfa), RowNone},
		{"Remove of the password", inputAt(Remove{Target: fxPW}, mfa, fxRK, fxTOTP), RowNone},
		{"SelfRemoveBackup of a present backup address not in the proof", inputAt(SelfRemoveBackup{Target: bk}, append(credsOf(bk), noMFA...), fxAddr), RowT2b},
		{"SelfRemoveBackup of an absent target", inputAt(SelfRemoveBackup{Target: bk}, noMFA, bk), RowNone},
		{"SelfRemoveBackup of a non-backup credential", inputAt(SelfRemoveBackup{Target: fxAddr}, noMFA, fxAddr), RowNone},
		{"Enrol failing its step-up", inputAt(Enrol{Ref: cref(CredRecoveryKey, "new")}, noMFA, fxPW), RowT4},
		{"Enrol of a password", inputAt(Enrol{Ref: cref(CredPassword, "new")}, noMFA, fxPW), RowNone},
		{"Enrol of the primary address", inputAt(Enrol{Ref: cref(CredAddress, "new")}, noMFA, fxPW), RowNone},
		{"Enrol into an occupied slot", inputAt(Enrol{Ref: fxSSO}, credsOf(fxPW, fxAddr, fxSSO), fxPW, fxSSO), RowNone},
		{"OpenBreakGlass at ClassNone", inputAt(OpenBreakGlass{}, noMFA), RowNone},
		{"Cancel naming a pending row it cannot cancel", func() Input {
			in := inputAt(Cancel{IDs: []string{"r"}}, credsOf(fxPW, fxAddr), fxAddr)
			in.Reqs = []Req{{ID: "r", Kind: ReqCredentialRemove, Status: StatusPending, OriginClass: C2,
				Targets: []CredRef{fxSSO}, OpenedAt: t0.Add(-time.Hour), CompletesAt: t0.Add(71 * time.Hour)}}
			return in
		}(), RowT7},
		{"Cancel naming no cancellable row", inputAt(Cancel{IDs: []string{"nope"}}, noMFA, fxPW), RowNone},
		{"Resolve failing the C3 test", func() Input {
			in := inputAt(Resolve{Group: "g"}, []Cred{credMaturing(fxTOTP, t0.Add(-2*time.Hour))}, fxTOTP)
			in.Reqs = disputed
			return in
		}(), RowT12},
		{"Resolve of an unknown group", inputAt(Resolve{Group: "nope"}, noMFA, fxPW), RowNone},
		{"Complete of nothing", inputAt(Complete{ID: "nope"}, noMFA), RowNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Decide(tc.in)
			assert.Equal(t, VerdictRefused, out.Verdict)
			assert.Equal(t, tc.row, out.Row)
		})
	}
}

// TestT1b_RequiresAnUnflaggedPossessionFactor (architect ruling 15d): the qualifying ref is a TOTP
// or WebAuthn with Paper == false that matches exactly and counts at Now. BackupEligible is
// irrelevant.
func TestT1b_RequiresAnUnflaggedPossessionFactor(t *testing.T) {
	flaggedTOTP := matureCred(fxTOTP)
	flaggedTOTP.Paper = true
	out := Decide(inputAt(SignOutEverywhere{}, []Cred{matureCred(fxPW), flaggedTOTP}, fxTOTP))
	assert.Equal(t, VerdictRefused, out.Verdict, "a paper-flagged TOTP")
	assert.Equal(t, RowT1b, out.Row)

	flaggedWA := matureCred(fxWA0)
	flaggedWA.Paper = true
	out = Decide(inputAt(SignOutEverywhere{}, []Cred{matureCred(fxPW), flaggedWA}, fxWA0))
	assert.Equal(t, VerdictRefused, out.Verdict, "a paper-flagged WebAuthn")

	for _, ref := range []CredRef{fxTOTP, fxWA0, fxWA1} {
		out = Decide(inputAt(SignOutEverywhere{}, standardCreds(), ref))
		assert.Equal(t, VerdictApplied, out.Verdict, "%v, whatever its backup eligibility", ref)
	}
	// A rest of the proof that does not qualify does not matter.
	out = Decide(inputAt(SignOutEverywhere{}, standardCreds(), fxPW, fxTOTP, cref(CredTOTP, "ghost")))
	assert.Equal(t, VerdictApplied, out.Verdict)

	// A stale version of the factor does not qualify (D6).
	stale := CredRef{Kind: CredTOTP, ID: "totp", Version: 0}
	out = Decide(inputAt(SignOutEverywhere{}, standardCreds(), fxPW, stale))
	assert.Equal(t, VerdictRefused, out.Verdict)
}
