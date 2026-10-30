package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round 3, H1 (architect rulings-3): where one loader exists and nothing consumes the field yet,
// the polarity is inverted so that a missed load fails CLOSED by construction. The zero value of
// SSOLogin is a password account, of Req.MatureNoticesSettled is "a notice to a mature address is
// unsettled", and of Input.GuessableBudgetClear is "the budget is locked". A zero CancelledAt on
// a recent cancel counts as inside the reopen window.
//
// Every test below pairs the zero value (the closed side) with a positive control that sets the
// field and gets the open side, so a test cannot pass by a fixture that was always refused.

// ---- SSOLogin ------------------------------------------------------------------------------

func TestLoginFloor_ZeroSSOLoginIsAPasswordAccount(t *testing.T) {
	creds := credsOf(fxPW, fxAddr, fxSSO)
	ssoOnly := Proof{Used: []CredRef{fxSSO}}
	pwOnly := Proof{Used: []CredRef{fxPW}}

	assert.False(t, LoginFloor(ssoOnly, creds, false), "an SSO assertion alone does not meet a password account's floor")
	assert.True(t, LoginFloor(pwOnly, creds, false))

	assert.True(t, LoginFloor(ssoOnly, creds, true), "positive control: ssoLogin names the SSO leg")
	assert.False(t, LoginFloor(pwOnly, creds, true), "a password is not the K leg of an SSO account")
}

// An SSO-only owner whose runner dropped SSOLogin cannot T1 or enrol: that is a denial. The
// failure the inversion removes is an IdP compromise (an SSO assertion alone) signing out a
// password account (parent §4 IdP row).
func TestDecide_UnsetSSOLoginDeniesAnSSOOnlyProof(t *testing.T) {
	creds := credsOf(fxPW, fxAddr, fxSSO)

	t.Run("T1", func(t *testing.T) {
		in := inputAt(SignOutOthers{}, creds, fxSSO)
		out := Decide(in)
		assert.Equal(t, VerdictRefused, out.Verdict)
		assert.Equal(t, RowT1, out.Row)
		assert.False(t, out.RotateEpoch)

		in.SSOLogin = true
		out = Decide(in)
		assert.Equal(t, VerdictApplied, out.Verdict, "positive control: an SSO account signs out with SSO")
	})
	t.Run("T4", func(t *testing.T) {
		enrol := Enrol{Ref: cref(CredTOTP, "new")}
		out := Decide(inputAt(enrol, creds, fxSSO))
		assert.Equal(t, VerdictRefused, out.Verdict)
		assert.Equal(t, RowT4, out.Row)

		in := inputAt(enrol, creds, fxSSO)
		in.SSOLogin = true
		assert.Equal(t, VerdictApplied, Decide(in).Verdict, "positive control")
	})
	t.Run("T4 backup address and SSO link step-up", func(t *testing.T) {
		bk := cref(CredBackupAddress, "bk")
		in := inputAt(Enrol{Ref: bk}, credsOf(fxPW, fxAddr, fxSSO), bk, fxSSO)
		assert.Equal(t, VerdictRefused, Decide(in).Verdict, "the floor leg is the password, which the proof lacks")
		in.SSOLogin = true
		assert.Equal(t, VerdictApplied, Decide(in).Verdict, "positive control")
	})
}

// ---- MatureNoticesSettled -------------------------------------------------------------------

// TestComplete_UnsettledMatureNoticesHoldByDefault: a due request whose address notice was
// accepted but whose Req was loaded without MatureNoticesSettled is held, never completed.
func TestComplete_UnsettledMatureNoticesHoldByDefault(t *testing.T) {
	r := dueRemoval()
	r.MatureNoticesSettled = false // the zero value, as a runner that never loaded it would leave it
	require.True(t, r.AcceptedAddressNotice)

	out := Decide(completeInput(r))
	assert.Equal(t, VerdictHeld, out.Verdict)
	assert.Equal(t, RowT11, out.Row)
	assert.Empty(t, effectsOfKind(out, EffectComplete))
	assert.Equal(t, []Effect{{Kind: EffectHold, RequestID: "r1", HeldSince: t0}}, out.Effects)

	r.MatureNoticesSettled = true
	out = Decide(completeInput(r))
	assert.Equal(t, VerdictApplied, out.Verdict, "positive control: settled notices complete")
	assert.Len(t, effectsOfKind(out, EffectComplete), 1)

	var zeroReq Req
	assert.False(t, zeroReq.MatureNoticesSettled, "the field is the polarity under test")
}

// ---- GuessableBudgetClear -------------------------------------------------------------------

// TestComplete_UnreadBudgetHoldsUnlessCovered: Input.GuessableBudgetClear is true only when the
// runner read the budgets and none is locked. With it unset, hold (b) is evaluated: an
// uncovered request is held, a covered one completes.
func TestComplete_UnreadBudgetHoldsUnlessCovered(t *testing.T) {
	r := dueRemoval() // origin C1, covered by any mature WebAuthn or anchor
	uncovered := completeInput(r)
	uncovered.GuessableBudgetClear = false
	out := Decide(uncovered)
	assert.Equal(t, VerdictHeld, out.Verdict, "no WebAuthn or anchor covers the origin class")
	assert.Empty(t, effectsOfKind(out, EffectComplete))

	clearBudget := completeInput(r)
	require.True(t, clearBudget.GuessableBudgetClear, "inputAt reads the budget as clear")
	assert.Equal(t, VerdictApplied, Decide(clearBudget).Verdict, "positive control: a clear budget never holds")

	covered := completeInput(r)
	covered.GuessableBudgetClear = false
	covered.Creds = append(covered.Creds, credMaturing(fxRK, r.OpenedAt.Add(-time.Hour)))
	assert.Equal(t, VerdictApplied, Decide(covered).Verdict, "positive control: a covering anchor completes under a locked budget")

	var zero Input
	assert.False(t, zero.GuessableBudgetClear, "the field is the polarity under test")
}

// ---- zero CancelledAt -----------------------------------------------------------------------

// A recent cancel whose CancelledAt was dropped is, by contract, still younger than NCancelCP
// (RecentCancels holds only those), so it must not exempt the row: a match refuses (T9) or
// disputes (T10), the protective side.
func TestOpen_ZeroCancelledAtCountsAsInsideTheReopenWindow(t *testing.T) {
	cand := Candidate{Keyed: []byte("k"), Coarse: []byte("c"), KeyID: 1}
	cases := []struct {
		name      string
		reason    CancelReason
		canceller Class
		wantRow   Row
		wantVerd  Verdict
	}{
		{"a stronger proof canceller refuses (T9)", CancelProof, C3, RowT9, VerdictRefused},
		{"a dispute_resolved cancel refuses (T9)", CancelDisputeResolved, C3, RowT9, VerdictRefused},
		{"a weaker canceller disputes (T10)", CancelProof, C1, RowT10, VerdictDisputed},
		{"a hold_cap cancel never matches, dated or not", CancelHoldCap, C3, RowT6, VerdictOpened},
		{"a dispute_expired cancel never matches, dated or not", CancelDisputeExpired, C3, RowT6, VerdictOpened},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cancel := recentCancel(tc.reason, tc.canceller, "k", "c", 1, t0.Add(-time.Hour))
			cancel.CancelledAt = time.Time{}
			out := Decide(mailboxReopen(cand, 1, cancel))
			assert.Equal(t, tc.wantRow, out.Row)
			assert.Equal(t, tc.wantVerd, out.Verdict)
		})
	}

	t.Run("positive control: a cancel dated outside the window is ignored", func(t *testing.T) {
		old := recentCancel(CancelProof, C3, "k", "c", 1, t0.Add(-NCancelCP))
		out := Decide(mailboxReopen(cand, 1, old))
		assert.Equal(t, RowT6, out.Row)
		assert.Equal(t, VerdictOpened, out.Verdict)
	})
}

// ---- H3: completable() refuses a row that completes before OpenedAt + DeltaCP ----------------

func TestBoundary_CompletesAtBeforeOpenedPlusDeltaCPIsRefused(t *testing.T) {
	r := dueRemoval() // opened t0-72h, completes t0 == OpenedAt + DeltaCP
	require.Equal(t, r.OpenedAt.Add(DeltaCP), r.CompletesAt)

	out := Decide(completeInput(r))
	assert.Equal(t, VerdictApplied, out.Verdict, "CompletesAt == OpenedAt + DeltaCP is evaluated normally")

	early := r
	early.CompletesAt = r.OpenedAt.Add(DeltaCP - time.Nanosecond) // still due at Now, but a mapping bug made it early
	out = Decide(completeInput(early))
	assert.Equal(t, RowNone, out.Row, "one nanosecond under OpenedAt + DeltaCP")
	assert.Equal(t, VerdictRefused, out.Verdict)
	assert.Nil(t, out.Effects)

	// The condition is "at least": a kind with a longer delay passes.
	late := r
	late.CompletesAt = r.OpenedAt.Add(DeltaCP + time.Hour)
	in := completeInput(late)
	in.Now = late.CompletesAt
	assert.Equal(t, VerdictApplied, Decide(in).Verdict, "a longer delay completes at its own due instant")
}

func TestComplete_EarlyRowIsRefusedForEveryKindAndState(t *testing.T) {
	for _, kind := range []ReqKind{ReqCredentialRemove, ReqAddressChange, ReqBreakGlass, ReqSocialRecovery, ReqCircleChange} {
		r := dueRemoval()
		r.Kind = kind
		r.CompletesAt = r.OpenedAt.Add(DeltaCP - time.Second)
		for name, mutate := range map[string]func(*Req){
			"plain":       func(*Req) {},
			"held":        func(r *Req) { r.AcceptedAddressNotice = false },
			"at hold cap": func(r *Req) { r.HeldMs = HoldCap.Milliseconds() },
			"open hold":   func(r *Req) { r.HeldSince = r.CompletesAt },
			"unsettled":   func(r *Req) { r.MatureNoticesSettled = false },
		} {
			t.Run(name, func(t *testing.T) {
				rr := r
				mutate(&rr)
				out := Decide(completeInput(rr))
				assert.Equal(t, RowNone, out.Row, "kind %d", kind)
				assert.Equal(t, VerdictRefused, out.Verdict)
				assert.Nil(t, out.Effects, "not even a hold-cap cancel")
			})
		}
	}
}
