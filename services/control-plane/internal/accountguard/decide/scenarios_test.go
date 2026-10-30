package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plan Task 12: rows 1-5 of parent §6.12 ("Mirror checks on the final table") as pure scenarios
// on the world model. Spec §7 places rows 1-5 in C4 and rows 6-11 plus row 4's tail in C6. C4's
// Action sum has no address replace (C5 is the first producer of a displacement), so the mailbox
// legs of rows 1, 2 and 5 are asserted through ClassOf and Rank, and C5 re-runs them end to end.
// Every step runs the global invariants (world.decide).

// scCand is a fingerprint candidate named for the actor who makes the request.
func scCand(actor string) Candidate {
	return Candidate{Keyed: []byte(actor + "-keyed"), Coarse: []byte(actor + "-coarse"), KeyID: 1}
}

// scWorld is an account with the given long-mature credentials and fxAddr as its primary address.
func scWorld(creds ...Cred) *world {
	w := newWorld(creds...)
	w.primaries = []Primary{{Ref: fxAddr, BecamePrimaryAt: t0.Add(-1000 * time.Hour), MaturesAt: t0.Add(-900 * time.Hour)}}
	return w
}

func scRequire(t *testing.T, out Outcome, row Row, verdict Verdict, source Source) {
	t.Helper()
	require.Equal(t, row, out.Row, "row")
	require.Equal(t, verdict, out.Verdict, "verdict")
	require.Equal(t, source, out.Source, "source")
}

func scClass(w *world, used ...CredRef) Class {
	return ClassOf(Proof{Used: used}, w.creds, w.now)
}

// Row 1: an IPV victim with their own TOTP; the abuser has the password and the shared mailbox.
// T1 signs the abuser out and rotates the password; the mailbox (rank 1) is replaced at once
// (T2, C2 > 1); the abuser is cut off immediately.
func TestScenario_Row1_IPVVictimWithTOTP(t *testing.T) {
	t.Run("T1 cuts the abuser off and the mailbox outranks nothing the victim holds", func(t *testing.T) {
		w := scWorld(matureCred(fxPW), matureCred(fxAddr), matureCred(fxTOTP))
		pw := w.passwordRef()

		// The victim's T1 with {pw, totp}: applied, the epoch rotates, a continuation is minted.
		out := w.decide(t, SignOutOthers{}, pw, fxTOTP)
		scRequire(t, out, RowT1, VerdictApplied, SourceUserSession)
		assert.True(t, out.RotateEpoch)
		assert.True(t, out.Continuation)
		assert.Nil(t, out.Effects)

		// The mailbox is rank 1; the victim's proof is C2, so replacing it is immediate (T2).
		cls := scClass(w, pw, fxTOTP)
		require.Equal(t, C2, cls)
		assert.Greater(t, uint8(cls), uint8(CredAddress.Rank()))

		// The runner rotates the password. The abuser's old reference no longer meets the login
		// floor, with or without a second factor, and cannot sign anyone else out either.
		w.apply(t, out)
		w.rotatePassword()
		assert.False(t, LoginFloor(Proof{Used: []CredRef{pw}}, w.creds, false))
		assert.False(t, LoginFloor(Proof{Used: []CredRef{pw, fxTOTP}}, w.creds, false), "a stale password ref meets no knowledge leg")
		scRequire(t, w.decide(t, SignOutOthers{}, pw), RowT1, VerdictRefused, SourceUserSession)
		scRequire(t, w.decide(t, SignOutEverywhere{}, pw), RowT1b, VerdictRefused, SourceUserSession)

		// Positive control: the victim, with the new password and the TOTP, still passes.
		assert.True(t, LoginFloor(Proof{Used: []CredRef{w.passwordRef(), fxTOTP}}, w.creds, false))
		assert.Greater(t, uint8(scClass(w, fxTOTP)), uint8(CredAddress.Rank()), "the TOTP alone still outranks the mailbox")
	})
}

// Row 2: IPV on a no-MFA account. Access is cut at once (T1). Replacing the mailbox is a T3
// request the abuser cannot veto. The victim enrols TOTP (T4); the mailbox (C1) cannot contest
// it (T5 needs >= 2). A passive abuser holds nothing after delta_cp; a contesting one forces
// `disputed` (F3).
func TestScenario_Row2_IPVNoMFA(t *testing.T) {
	t.Run("the victim enrols a TOTP the shared mailbox cannot contest", func(t *testing.T) {
		w := scWorld(matureCred(fxPW), matureCred(fxAddr))
		pw := w.passwordRef()

		// Access is cut at once. (The runner's rotation of the password is not modelled here, so
		// the password's class below is the one the plan records for the enrolment.)
		out := w.decide(t, SignOutOthers{}, pw)
		scRequire(t, out, RowT1, VerdictApplied, SourceUserSession)
		assert.True(t, out.RotateEpoch)
		w.apply(t, out)

		// The victim's class is C1, not above the mailbox's rank 1: replacing the mailbox would be
		// a T3 request, not an immediate T2.
		cls := scClass(w, pw)
		require.Equal(t, C1, cls)
		assert.LessOrEqual(t, uint8(cls), uint8(CredAddress.Rank()))

		// T4: a TOTP needs only the login floor.
		victimTOTP := cref(CredTOTP, "victim-totp")
		out = w.decide(t, Enrol{Ref: victimTOTP}, pw)
		scRequire(t, out, RowT4, VerdictApplied, SourceUserSession)
		require.Len(t, noticesOfKind(out, NoticeCredentialAdded), 1)
		assert.Equal(t, victimTOTP, noticesOfKind(out, NoticeCredentialAdded)[0].Subject)
		w.enrol(matureCred(victimTOTP), cls)

		// The abuser's mailbox (C1) cannot contest the TOTP: T5 needs at least rank 2.
		out = w.decide(t, Contest{}, fxAddr)
		scRequire(t, out, RowT5, VerdictRefused, SourceUserSession)
		assert.False(t, out.RotateEpoch)

		// At exactly delta_cp the TOTP has not yet matured (D7: strictly before); a minute later it has.
		w.advance(DeltaCP)
		require.Equal(t, ClassNone, scClass(w, victimTOTP), "matures exactly now: does not count")
		w.advance(time.Minute)
		require.Equal(t, C2, scClass(w, victimTOTP))

		// The abuser opens a break-glass with the mailbox; the victim's TOTP cancels it at C2.
		out = w.decide(t, OpenBreakGlass{Candidate: scCand("abuser")}, fxAddr)
		scRequire(t, out, RowT6, VerdictOpened, SourceBreakGlass)
		w.apply(t, out)
		require.Len(t, w.reqs, 1)
		id := w.reqs[0].ID

		out = w.decide(t, Cancel{IDs: []string{id}}, victimTOTP)
		scRequire(t, out, RowT7, VerdictApplied, SourceUserSession)
		cancels := effectsOfKind(out, EffectCancel)
		require.Len(t, cancels, 1)
		assert.Equal(t, id, cancels[0].RequestID)
		assert.Equal(t, CancelProof, cancels[0].Reason)
		assert.Equal(t, C2, cancels[0].CancellerClass)
		w.apply(t, out)

		// The abuser's reopen is refused by T9: a stronger class cancelled the same fingerprint.
		out = w.decide(t, OpenBreakGlass{Candidate: scCand("abuser")}, fxAddr)
		scRequire(t, out, RowT9, VerdictRefused, SourceBreakGlass)
		assert.Nil(t, out.Notices)
	})
}

// Row 3: naive credential stuffing. The stuffer's new password is immature, so it cannot cancel
// the owner's mailbox break-glass. The owner wins at delta_cp.
func TestScenario_Row3_NaiveStuffing(t *testing.T) {
	t.Run("the stuffer's fresh credentials cannot veto the owner's break-glass", func(t *testing.T) {
		w := scWorld(matureCred(fxPW), matureCred(fxAddr))
		oldPW := w.passwordRef()

		// The stuffer signs the owner out and rotates the password; the new one is immature.
		out := w.decide(t, SignOutOthers{}, oldPW)
		scRequire(t, out, RowT1, VerdictApplied, SourceUserSession)
		w.apply(t, out)
		w.rotatePassword()
		newPW := w.passwordRef()
		require.EqualValues(t, 2, newPW.Version)

		// It also digs in with a TOTP of its own, enrolled on the strength of the immature password
		// (the login floor ignores maturity), so the recorded class is none.
		stufferTOTP := cref(CredTOTP, "stuffer-totp")
		out = w.decide(t, Enrol{Ref: stufferTOTP}, newPW)
		scRequire(t, out, RowT4, VerdictApplied, SourceUserSession)
		recorded := scClass(w, newPW)
		require.Equal(t, ClassNone, recorded)
		w.enrol(matureCred(stufferTOTP), recorded)

		// The owner's break-glass with the mailbox is opened.
		out = w.decide(t, OpenBreakGlass{Candidate: scCand("owner")}, fxAddr)
		scRequire(t, out, RowT6, VerdictOpened, SourceBreakGlass)
		w.apply(t, out)
		require.Len(t, w.reqs, 1)
		id := w.reqs[0].ID
		require.Equal(t, StatusPending, w.reqs[0].Status)

		// The stuffer's T7 with the new password (and with its new TOTP) is refused: both matured after the open.
		out = w.decide(t, Cancel{IDs: []string{id}}, newPW)
		scRequire(t, out, RowT7, VerdictRefused, SourceUserSession)
		out = w.decide(t, Cancel{IDs: []string{id}}, newPW, stufferTOTP)
		scRequire(t, out, RowT7, VerdictRefused, SourceUserSession)
		assert.Nil(t, out.Effects)

		// One nanosecond before delta_cp nothing happens; at delta_cp the owner wins.
		w.advance(DeltaCP - time.Nanosecond)
		w.accept(id)
		out = w.decide(t, Complete{ID: id})
		scRequire(t, out, RowT11, VerdictNoChange, SourceBreakGlass)
		assert.Nil(t, out.Effects)

		w.advance(time.Nanosecond)
		out = w.decide(t, Complete{ID: id})
		scRequire(t, out, RowT11, VerdictApplied, SourceBreakGlass)
		assert.True(t, out.RotateEpoch)
		assert.True(t, out.SetsPassword)
		require.Len(t, effectsOfKind(out, EffectComplete), 1)
		removes := effectsOfKind(out, EffectRemove)
		require.Len(t, removes, 1, "only the stuffer's immature credential goes; the password and the mailbox stay")
		assert.Equal(t, stufferTOTP, removes[0].Cred)
		assert.Len(t, noticesOfKind(out, NoticeChangeCompleted), 1)
		assert.Len(t, noticesOfKind(out, NoticePasswordChanged), 1)
		removed := noticesOfKind(out, NoticeCredentialRemoved)
		require.Len(t, removed, 1)
		assert.Equal(t, stufferTOTP, removed[0].Subject)
	})
}

// Row 4: prepared credential stuffing, on a legacy account with no MFA. `disputed` (T8). While it
// runs, the stuffer enrols a possession factor (T4), which the owner's mailbox cannot contest
// (T5 needs >= 2). At expiry that factor is mature and cancels every break-glass the owner
// reopens (T7, then T9). The takeover sticks, the accepted row-4 outcome; row 4's tail is C6's.
func TestScenario_Row4_PreparedStuffingLegacyNoMFA(t *testing.T) {
	t.Run("a dispute buys the stuffer time to mature a factor the mailbox cannot contest", func(t *testing.T) {
		w := scWorld(matureCred(fxPW), matureCred(fxAddr))
		pw := w.passwordRef()

		// The owner's break-glass with the mailbox is opened.
		out := w.decide(t, OpenBreakGlass{Candidate: scCand("owner")}, fxAddr)
		scRequire(t, out, RowT6, VerdictOpened, SourceBreakGlass)
		w.apply(t, out)
		require.Len(t, w.reqs, 1)
		ownerReq := w.reqs[0].ID

		// The stuffer, with the still-mature password, opens its own: T8, both disputed.
		out = w.decide(t, OpenBreakGlass{Candidate: scCand("stuffer")}, pw)
		scRequire(t, out, RowT8, VerdictDisputed, SourceBreakGlass)
		opens := effectsOfKind(out, EffectOpen)
		require.Len(t, opens, 1)
		assert.Equal(t, StatusDisputed, opens[0].Open.Status)
		disputes := effectsOfKind(out, EffectDispute)
		require.Len(t, disputes, 1)
		assert.Equal(t, ownerReq, disputes[0].RequestID)
		assert.Len(t, noticesOfKind(out, NoticeChangeDisputed), 2, "the opener and the owner's row")
		assert.Empty(t, noticesOfKind(out, NoticeChangeRequested))
		w.apply(t, out)
		require.Len(t, w.reqs, 2)
		for _, r := range w.reqs {
			require.Equal(t, StatusDisputed, r.Status)
			require.NotEmpty(t, r.DisputeGroup)
		}
		require.Equal(t, w.reqs[0].DisputeGroup, w.reqs[1].DisputeGroup, "one dispute group")

		// While the dispute runs, the stuffer enrols a TOTP with its password (T4).
		stufferTOTP := cref(CredTOTP, "stuffer-totp")
		out = w.decide(t, Enrol{Ref: stufferTOTP}, pw)
		scRequire(t, out, RowT4, VerdictApplied, SourceUserSession)
		cls := scClass(w, pw)
		require.Equal(t, C1, cls)
		w.enrol(matureCred(stufferTOTP), cls)

		// The owner's mailbox (C1) cannot contest it: T5 needs at least rank 2.
		out = w.decide(t, Contest{}, fxAddr)
		scRequire(t, out, RowT5, VerdictRefused, SourceUserSession)

		// Nothing expires one nanosecond early; at exactly the dispute expiry both rows are cancelled.
		w.advance(DisputeExpiry - time.Nanosecond)
		out = w.decide(t, Expire{})
		scRequire(t, out, RowSweep, VerdictNoChange, SourceUserSession)
		w.advance(time.Nanosecond)
		out = w.decide(t, Expire{})
		scRequire(t, out, RowSweep, VerdictApplied, SourceUserSession)
		cancels := effectsOfKind(out, EffectCancel)
		require.Len(t, cancels, 2)
		for _, c := range cancels {
			assert.Equal(t, CancelDisputeExpired, c.Reason)
			assert.Equal(t, ClassNone, c.CancellerClass)
		}
		assert.Len(t, noticesOfKind(out, NoticeChangeCancelled), 2)
		w.apply(t, out)
		require.Empty(t, w.reqs)

		// The owner reopens: opened (an expired dispute is no T9 trigger).
		out = w.decide(t, OpenBreakGlass{Candidate: scCand("owner")}, fxAddr)
		scRequire(t, out, RowT6, VerdictOpened, SourceBreakGlass)
		w.apply(t, out)
		require.Len(t, w.reqs, 1)
		reopened := w.reqs[0].ID

		// The stuffer's TOTP matured during the dispute: it cancels the reopened request at C2.
		out = w.decide(t, Cancel{IDs: []string{reopened}}, stufferTOTP)
		scRequire(t, out, RowT7, VerdictApplied, SourceUserSession)
		cancels = effectsOfKind(out, EffectCancel)
		require.Len(t, cancels, 1)
		assert.Equal(t, CancelProof, cancels[0].Reason)
		assert.Equal(t, C2, cancels[0].CancellerClass)
		w.apply(t, out)

		// The owner reopens again: T9 refuses (a C2 canceller beats the mailbox's C1). It sticks.
		out = w.decide(t, OpenBreakGlass{Candidate: scCand("owner")}, fxAddr)
		scRequire(t, out, RowT9, VerdictRefused, SourceBreakGlass)
		assert.Nil(t, out.Effects)
		assert.Nil(t, out.Notices)
	})
}

// Row 5: a real-time TOTP phish. The mailbox goes at once. Removing the owner's TOTP waits (T3),
// and the TOTP vetoes its own removal (a P is not an address or SSO link). T1b with the TOTP
// signs the phisher out everywhere; without the next code the phisher cannot sign in again. The
// owner's break-glass on the TOTP alone completes at delta_cp.
func TestScenario_Row5_RealtimeTOTPPhish(t *testing.T) {
	t.Run("the owner's TOTP vetoes its own removal and T1b evicts the phisher's new factor", func(t *testing.T) {
		w := scWorld(matureCred(fxPW), matureCred(fxAddr), matureCred(fxTOTP))
		pw := w.passwordRef()

		// {pw, totp code} is C2, above the mailbox's rank 1: the mailbox goes at once (class level).
		cls := scClass(w, pw, fxTOTP)
		require.Equal(t, C2, cls)
		assert.Greater(t, uint8(cls), uint8(CredAddress.Rank()))

		// The phisher cannot remove the TOTP at once: the class equals its rank, so T3 opens a request.
		out := w.decide(t, Remove{Target: fxTOTP, Candidate: scCand("phisher")}, pw, fxTOTP)
		scRequire(t, out, RowT3, VerdictOpened, SourceUserSession)
		opens := effectsOfKind(out, EffectOpen)
		require.Len(t, opens, 1)
		assert.Equal(t, ReqCredentialRemove, opens[0].Open.Kind)
		assert.Equal(t, []CredRef{fxTOTP}, opens[0].Open.Targets)
		assert.Equal(t, C2, opens[0].Open.OriginClass)
		w.apply(t, out)
		require.Len(t, w.reqs, 1)
		removal := w.reqs[0].ID

		// The owner's TOTP cancels it: a P never vetoes itself away.
		out = w.decide(t, Cancel{IDs: []string{removal}}, fxTOTP)
		scRequire(t, out, RowT7, VerdictApplied, SourceUserSession)
		cancels := effectsOfKind(out, EffectCancel)
		require.Len(t, cancels, 1)
		assert.Equal(t, C2, cancels[0].CancellerClass)
		w.apply(t, out)

		// The phisher enrols a WebAuthn key of its own (T4 with {pw, totp}).
		out = w.decide(t, Enrol{Ref: fxWA0}, pw, fxTOTP)
		scRequire(t, out, RowT4, VerdictApplied, SourceUserSession)
		w.enrol(matureCred(fxWA0), scClass(w, pw, fxTOTP))

		// The owner's T1b with the TOTP: the epoch rotates, and the outcome contests the new key (3-A).
		out = w.decide(t, SignOutEverywhere{}, fxTOTP)
		scRequire(t, out, RowT1b, VerdictApplied, SourceUserSession)
		assert.True(t, out.RotateEpoch)
		contests := effectsOfKind(out, EffectContest)
		require.Len(t, contests, 1)
		assert.Equal(t, fxWA0, contests[0].Cred)
		assert.Len(t, noticesOfKind(out, NoticeSignedOutEverywhere), 1)
		require.Len(t, noticesOfKind(out, NoticeEnrolmentContested), 1)
		assert.Equal(t, fxWA0, noticesOfKind(out, NoticeEnrolmentContested)[0].Subject)
		w.apply(t, out)

		// Without the next code the phisher cannot get back in: a password alone meets no MFA leg.
		assert.False(t, LoginFloor(Proof{Used: []CredRef{pw}}, w.creds, false))
		scRequire(t, w.decide(t, SignOutEverywhere{}, pw), RowT1b, VerdictRefused, SourceUserSession)

		// The owner's break-glass on the TOTP alone: opened, then complete at delta_cp.
		out = w.decide(t, OpenBreakGlass{Candidate: scCand("owner")}, fxTOTP)
		scRequire(t, out, RowT6, VerdictOpened, SourceBreakGlass)
		w.apply(t, out)
		require.Len(t, w.reqs, 1, "the cancelled removal is no longer open")
		id := w.reqs[0].ID

		w.advance(DeltaCP)
		w.accept(id)
		out = w.decide(t, Complete{ID: id})
		scRequire(t, out, RowT11, VerdictApplied, SourceBreakGlass)
		assert.True(t, out.RotateEpoch)
		assert.True(t, out.SetsPassword)
		require.Len(t, effectsOfKind(out, EffectComplete), 1)
		removes := effectsOfKind(out, EffectRemove)
		require.Len(t, removes, 1, "the contested key goes; the owner's credentials stay")
		assert.Equal(t, fxWA0, removes[0].Cred)
	})
}
