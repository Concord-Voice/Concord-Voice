package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round 3, red-team F1 (architect rulings-3, option A): a synced (backup-eligible) passkey never
// pairs with an anchor into C3 (parent §6.1 C3, §6.9, §6.13). Backup eligibility is read from
// THIS assertion and travels on Proof.DeviceBound; the zero value proves nothing, so a runner
// that forgets it loses C3 for WebAuthn and keeps TOTP + anchor. No stored flag exists.

// TestDecide_SyncedPasskeyPlusAnchorIsNotC3 promotes the red-team PoC: the abuser holds the
// victim's unbound recovery key and a family-synced passkey. Without DeviceBound the pair is C2:
// a break-glass waits delta_cp (T6, not T6c) and the victim's TOTP is a T3 request (not T2).
func TestDecide_SyncedPasskeyPlusAnchorIsNotC3(t *testing.T) {
	victimTOTP := cref(CredTOTP, "victim-totp")
	shared := cref(CredWebAuthn, "family-synced-passkey")
	rk := cref(CredRecoveryKey, "rk")
	creds := []Cred{credMaturing(fxPW, legacyInstant), credMaturing(fxAddr, legacyInstant),
		credMaturing(victimTOTP, legacyInstant), credMaturing(shared, legacyInstant), credMaturing(rk, legacyInstant)}
	abuser := []CredRef{rk, shared}
	cand := Candidate{Keyed: []byte("bg-k"), Coarse: []byte("bg-c"), KeyID: 1}
	synced := Proof{Used: abuser}              // the assertion carried BE=1, so DeviceBound is empty
	bound := proofBound(abuser, shared)        // positive control: the same assertion with BE=0
	input := func(act Action, p Proof) Input { // the account signs in with a password; the budget is clear
		return Input{Now: t0, Act: act, Proof: p, Creds: creds, CurrentKeyID: 1, GuessableBudgetClear: true}
	}

	t.Run("class", func(t *testing.T) {
		assert.Equal(t, C2, ClassOf(synced, creds, t0), "anchor + a passkey whose assertion was BE=1 is C2")
		assert.Equal(t, C3, ClassOf(bound, creds, t0), "positive control: with BE=0 the pair is C3")
	})
	t.Run("T6c immediate break-glass", func(t *testing.T) {
		out := Decide(input(OpenBreakGlass{Reset: true, Candidate: cand}, synced))
		assert.Equal(t, RowT6, out.Row, "must be a delta_cp request the victim's TOTP can cancel")
		assert.Equal(t, VerdictOpened, out.Verdict)
		assert.False(t, out.SetsPassword, "the abuser's password is not set at once")
		assert.Empty(t, effectsOfKind(out, EffectRemove), "the victim's TOTP is not reset at once")
		assert.Equal(t, ClassNone, out.EnrolClass, "an opening writes no credential")

		control := Decide(input(OpenBreakGlass{Reset: true, Candidate: cand}, bound))
		require.Equal(t, RowT6c, control.Row, "positive control: a device-bound key completes at once")
		assert.Contains(t, effectsOfKind(control, EffectRemove), Effect{Kind: EffectRemove, Cred: victimTOTP})
	})
	t.Run("T2 immediate removal of the victim's TOTP", func(t *testing.T) {
		out := Decide(input(Remove{Target: victimTOTP, Candidate: cand}, synced))
		assert.Equal(t, RowT3, out.Row, "C2 <= rank 2 must wait delta_cp")
		assert.Equal(t, VerdictOpened, out.Verdict)

		control := Decide(input(Remove{Target: victimTOTP, Candidate: cand}, bound))
		assert.Equal(t, RowT2, control.Row, "positive control: C3 removes a rank-2 credential at once")
		assert.Equal(t, VerdictApplied, control.Verdict)
	})
}

// TestCovered_WebAuthnNeverPairsIntoC3 pins hold (b)'s strict direction. covered() builds a proof
// from credentials nobody asserted, so its DeviceBound is empty and a WebAuthn is never
// device-bound there. A C3-origin request (a T3 on an anchor target) under a locked guessable
// budget is therefore held when its only non-guessable cover is an anchor and a WebAuthn, even
// though that pair would be C3 if the key were asserted.
func TestCovered_WebAuthnNeverPairsIntoC3(t *testing.T) {
	opened := t0.Add(-DeltaCP)
	creds := []Cred{
		credMaturing(fxPW, opened.Add(-time.Hour)), credMaturing(fxAddr, opened.Add(-time.Hour)),
		credMaturing(fxRK, opened.Add(-time.Hour)), credMaturing(fxWA0, opened.Add(-time.Hour)),
		credMaturing(cref(CredRecoveryKey, "target-rk"), opened.Add(-time.Hour)),
	}
	req := func(oc Class) Req {
		return Req{ID: "r1", Kind: ReqCredentialRemove, Status: StatusPending, OriginClass: oc,
			Origins: []CredRef{fxRK, fxTOTP}, Targets: []CredRef{cref(CredRecoveryKey, "target-rk")},
			OpenedAt: opened, CompletesAt: t0, AcceptedAddressNotice: true, MatureNoticesSettled: true}
	}
	decide := func(r Req, budgetClear bool) Outcome {
		in := inputAt(Complete{ID: "r1"}, creds)
		in.Reqs = []Req{r}
		in.GuessableBudgetClear = budgetClear
		return Decide(in)
	}

	t.Run("C3 origin, budget locked, anchor + WebAuthn: held", func(t *testing.T) {
		out := decide(req(C3), false)
		assert.Equal(t, VerdictHeld, out.Verdict)
		assert.Equal(t, RowT11, out.Row)
		assert.Equal(t, []Effect{{Kind: EffectHold, RequestID: "r1", HeldSince: t0}}, out.Effects)
	})
	t.Run("positive control: the same row with the budget clear completes", func(t *testing.T) {
		out := decide(req(C3), true)
		assert.Equal(t, VerdictApplied, out.Verdict)
		assert.Len(t, effectsOfKind(out, EffectComplete), 1)
	})
	t.Run("positive control: a C2 origin is covered by the same credentials", func(t *testing.T) {
		out := decide(req(C2), false)
		assert.Equal(t, VerdictApplied, out.Verdict, "anchor + WebAuthn is C2, which covers a C2 origin")
	})
}
