package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plan Task 8: T7, T12, dispute expiry and reservation release (parent §6.2 T7/T12; spec §4
// input 1). Rows are in testdata/vectors.json; these tests pin the boundaries and the
// malformed-snapshot behaviour.

const thirtyDays = 30 * 24 * time.Hour

func disputedReq(id string, disputedAt time.Time) Req {
	return Req{ID: id, Kind: ReqBreakGlass, Status: StatusDisputed, OriginClass: C1,
		Origins: []CredRef{fxAddr}, OpenedAt: disputedAt.Add(-time.Hour), CompletesAt: disputedAt.Add(71 * time.Hour),
		DisputedAt: disputedAt, DisputeGroup: "g"}
}

// Parent §6.2 T12: an unresolved disputed request expires after NCancelCP. The boundary:
// !Now.Before(DisputedAt + NCancelCP), so exactly 30 d expires and 1 ns earlier does not.
func TestBoundary_DisputeExpiresAtExactly30d(t *testing.T) {
	out := Decide(Input{Now: t0, Act: Expire{}, Creds: credsOf(fxPW), Reqs: []Req{disputedReq("d", t0.Add(-thirtyDays))}})
	require.Equal(t, VerdictApplied, out.Verdict)
	assert.Equal(t, RowSweep, out.Row)
	cancels := effectsOfKind(out, EffectCancel)
	require.Len(t, cancels, 1)
	assert.Equal(t, "d", cancels[0].RequestID)
	assert.Equal(t, CancelDisputeExpired, cancels[0].Reason)
	assert.Equal(t, ClassNone, cancels[0].CancellerClass, "ruling 13: only proof and dispute_resolved record a class")
	notices := noticesOfKind(out, NoticeChangeCancelled)
	require.Len(t, notices, 1, "ruling 9: one change_cancelled per expired row")
	assert.Equal(t, "d", notices[0].RequestID)
	assert.Equal(t, CredRef{}, notices[0].Subject)
	assert.Nil(t, notices[0].ReservedTo)

	out = Decide(Input{Now: t0, Act: Expire{}, Creds: credsOf(fxPW), Reqs: []Req{disputedReq("d", t0.Add(-thirtyDays+time.Nanosecond))}})
	assert.Equal(t, VerdictNoChange, out.Verdict)
	assert.Equal(t, RowSweep, out.Row)
	assert.Nil(t, out.Effects)
	assert.Nil(t, out.Notices)
}

// Architect ruling A3 / parent T5: the sweeper deletes a contested credential after delta_cp.
// ContestedAt + 72 h <= Now deletes (exactly at the boundary too); one nanosecond earlier keeps
// it. The password and the primary address are never deleted (D9). The deletion carries a
// credential_removed notice with no request and no reservations.
func TestBoundary_ContestedCredentialDeletedAtExactlyDeltaCP(t *testing.T) {
	const deltaCP = 72 * time.Hour
	contestedAt := func(c Cred, at time.Time) Cred {
		c.ContestedAt = at
		return c
	}
	phisher := credMaturing(fxWA0, t0.Add(70*time.Hour))

	out := Decide(Input{Now: t0, Act: Expire{}, Creds: []Cred{matureCred(fxPW), contestedAt(phisher, t0.Add(-deltaCP))}})
	require.Equal(t, VerdictApplied, out.Verdict)
	assert.Equal(t, RowSweep, out.Row)
	removes := effectsOfKind(out, EffectRemove)
	require.Len(t, removes, 1)
	assert.Equal(t, fxWA0, removes[0].Cred)
	assert.Empty(t, removes[0].RequestID)
	assert.Len(t, effectsOfKind(out, EffectResyncMFAFlags), 1, "D17")
	assert.True(t, out.PermGenBump, "D17")
	notices := noticesOfKind(out, NoticeCredentialRemoved)
	require.Len(t, notices, 1)
	assert.Equal(t, fxWA0, notices[0].Subject)
	assert.Empty(t, notices[0].RequestID)
	assert.Nil(t, notices[0].ReservedTo)

	out = Decide(Input{Now: t0, Act: Expire{}, Creds: []Cred{contestedAt(phisher, t0.Add(-deltaCP+time.Nanosecond))}})
	assert.Equal(t, VerdictNoChange, out.Verdict, "one nanosecond short of delta_cp")
	assert.Nil(t, out.Effects)

	out = Decide(Input{Now: t0, Act: Expire{}, Creds: []Cred{
		contestedAt(matureCred(fxPW), t0.Add(-10*deltaCP)), contestedAt(matureCred(fxAddr), t0.Add(-10*deltaCP)),
	}})
	assert.Equal(t, VerdictNoChange, out.Verdict, "the password and the primary address are never deleted")
	assert.Nil(t, out.Effects)
}

// Only disputed rows expire: an old pending row is T11's business.
func TestExpire_LeavesPendingAndRecoveryKeyRowsAlone(t *testing.T) {
	pending := disputedReq("p", t0.Add(-2*thirtyDays))
	pending.Status = StatusPending
	rk := disputedReq("rk", t0.Add(-2*thirtyDays))
	rk.Kind = ReqRecoveryKey
	out := Decide(Input{Now: t0, Act: Expire{}, Creds: credsOf(fxPW), Reqs: []Req{pending, rk}})
	assert.Equal(t, VerdictNoChange, out.Verdict)
	assert.Nil(t, out.Effects)
}

func releaseInput(floorUntil time.Time, primaries []Primary, reqs ...Req) Input {
	return Input{
		Now: t0, Act: Expire{}, Creds: credsOf(fxPW, fxAddr), Primaries: primaries, Reqs: reqs,
		Reservations: []Reservation{{AddressRef: cref(CredAddress, "old"), DisplacedAt: t0.Add(-200 * time.Hour), FloorUntil: floorUntil}},
	}
}

func matureCurrentPrimary() []Primary {
	return []Primary{{Ref: fxAddr, BecamePrimaryAt: t0.Add(-200 * time.Hour), MaturesAt: t0.Add(-100 * time.Hour)}}
}

// Spec §4 input 1: now >= FloorUntil. Exactly at FloorUntil releases; 1 ns before holds.
func TestBoundary_ReservationReleasesAtFloorUntil(t *testing.T) {
	out := Decide(releaseInput(t0, matureCurrentPrimary()))
	require.Equal(t, VerdictApplied, out.Verdict)
	releases := effectsOfKind(out, EffectReleaseReservation)
	require.Len(t, releases, 1)
	assert.Equal(t, cref(CredAddress, "old"), releases[0].Cred)

	out = Decide(releaseInput(t0.Add(time.Nanosecond), matureCurrentPrimary()))
	assert.Equal(t, VerdictNoChange, out.Verdict)
	assert.Empty(t, effectsOfKind(out, EffectReleaseReservation))
}

// Spec §4 input 1: the current primary is mature (MaturesAt.Before(Now)), so a primary maturing
// exactly now is still immature.
func TestBoundary_PrimaryMaturingExactlyNowHoldsTheReservation(t *testing.T) {
	cur := matureCurrentPrimary()
	cur[0].MaturesAt = t0
	out := Decide(releaseInput(t0.Add(-time.Hour), cur))
	assert.Equal(t, VerdictNoChange, out.Verdict)

	cur[0].MaturesAt = t0.Add(-time.Nanosecond)
	out = Decide(releaseInput(t0.Add(-time.Hour), cur))
	assert.Equal(t, VerdictApplied, out.Verdict)
}

// currentPrimary needs exactly one row with a zero DisplacedAt (plan Task 8): zero or two
// current primaries release nothing.
func TestResolve_NoCurrentPrimaryReleasesNothing(t *testing.T) {
	displaced := Primary{Ref: fxAddr, BecamePrimaryAt: t0.Add(-300 * time.Hour), MaturesAt: t0.Add(-200 * time.Hour),
		DisplacedAt: t0.Add(-100 * time.Hour)}

	t.Run("no primaries", func(t *testing.T) {
		out := Decide(releaseInput(t0.Add(-time.Hour), nil))
		assert.Equal(t, VerdictNoChange, out.Verdict)
		assert.Empty(t, effectsOfKind(out, EffectReleaseReservation))
	})
	t.Run("only a displaced primary", func(t *testing.T) {
		out := Decide(releaseInput(t0.Add(-time.Hour), []Primary{displaced}))
		assert.Equal(t, VerdictNoChange, out.Verdict)
	})
	t.Run("two current primaries", func(t *testing.T) {
		two := append(matureCurrentPrimary(), Primary{Ref: cref(CredAddress, "other"),
			BecamePrimaryAt: t0.Add(-200 * time.Hour), MaturesAt: t0.Add(-100 * time.Hour)})
		out := Decide(releaseInput(t0.Add(-time.Hour), two))
		assert.Equal(t, VerdictNoChange, out.Verdict)
	})
	t.Run("positive control: one current primary", func(t *testing.T) {
		out := Decide(releaseInput(t0.Add(-time.Hour), append(matureCurrentPrimary(), displaced)))
		assert.Equal(t, VerdictApplied, out.Verdict)
	})
}

// Round 3, red-team F5 (architect rulings-3, D7): the release rule and restoreTarget use one
// predicate, !primaryCountsAt(current, OpenedAt). A primary maturing exactly at OpenedAt did not
// count at OpenedAt, so a break-glass opened at that instant still needs the displaced primary
// reserved: it holds the reservation. One nanosecond later the primary counted, and it does not.
func TestBoundary_BreakGlassOpenedExactlyAtPrimaryMaturityHolds(t *testing.T) {
	cur := matureCurrentPrimary()
	bg := Req{ID: "bg", Kind: ReqBreakGlass, Status: StatusPending, OriginClass: C1, Origins: []CredRef{fxPW},
		OpenedAt: cur[0].MaturesAt, CompletesAt: cur[0].MaturesAt.Add(DeltaCP)}
	out := Decide(releaseInput(t0.Add(-time.Hour), cur, bg))
	assert.Equal(t, VerdictNoChange, out.Verdict, "opened at the maturity instant")
	assert.Empty(t, effectsOfKind(out, EffectReleaseReservation))

	bg.OpenedAt = cur[0].MaturesAt.Add(time.Nanosecond)
	bg.CompletesAt = bg.OpenedAt.Add(DeltaCP)
	out = Decide(releaseInput(t0.Add(-time.Hour), cur, bg))
	assert.Equal(t, VerdictApplied, out.Verdict, "opened one nanosecond after it")

	bg.OpenedAt = cur[0].MaturesAt.Add(-time.Nanosecond)
	bg.CompletesAt = bg.OpenedAt.Add(DeltaCP)
	out = Decide(releaseInput(t0.Add(-time.Hour), cur, bg))
	assert.Equal(t, VerdictNoChange, out.Verdict, "opened one nanosecond before it")

	bg.OpenedAt = cur[0].MaturesAt
	bg.CompletesAt = bg.OpenedAt.Add(DeltaCP)
	bg.Status = StatusDisputed
	bg.DisputedAt, bg.DisputeGroup = bg.OpenedAt, "g"
	out = Decide(releaseInput(t0.Add(-time.Hour), cur, bg))
	assert.Equal(t, VerdictNoChange, out.Verdict, "a disputed break-glass opened at the instant holds it too")

	bg.Status = StatusCancelled
	out = Decide(releaseInput(t0.Add(-time.Hour), cur, bg))
	assert.Equal(t, VerdictApplied, out.Verdict, "a cancelled break-glass no longer holds it")
}

// TestDecide_BreakGlassOpenedAtPrimaryMaturityKeepsItsRestore promotes the red-team PoC end to
// end. The attacker's address matures at the instant the owner's break-glass opens. The sweep
// must keep the owner's reservation, and the completion must restore the owner's primary
// (D7: the attacker's address was not mature at OpenedAt).
func TestDecide_BreakGlassOpenedAtPrimaryMaturityKeepsItsRestore(t *testing.T) {
	owner := cref(CredAddress, "owner-old")
	attacker := cref(CredAddress, "attacker-new")
	displaced := t0.Add(-200 * time.Hour)
	matures := displaced.Add(DeltaCP) // the attacker's address matures here
	prims := []Primary{
		{Ref: owner, BecamePrimaryAt: t0.Add(-1000 * time.Hour), MaturesAt: t0.Add(-900 * time.Hour), DisplacedAt: displaced},
		{Ref: attacker, BecamePrimaryAt: displaced, MaturesAt: matures},
	}
	res := []Reservation{{AddressRef: owner, DisplacedAt: displaced, FloorUntil: displaced.Add(DeltaCP)}}
	bg := Req{ID: "bg", Kind: ReqBreakGlass, Status: StatusPending, OriginClass: C2, Origins: []CredRef{fxTOTP},
		OpenedAt: matures, CompletesAt: matures.Add(DeltaCP), AcceptedAddressNotice: true, MatureNoticesSettled: true}
	creds := []Cred{credMaturing(fxPW, legacyInstant), credMaturing(fxTOTP, legacyInstant)}

	completeAt := matures.Add(DeltaCP + time.Minute)
	control := Decide(Input{Now: completeAt, Act: Complete{ID: "bg"}, Creds: creds, Primaries: prims,
		Reservations: res, Reqs: []Req{bg}, GuessableBudgetClear: true})
	require.Equal(t, []Effect{{Kind: EffectRestorePrimary, Cred: owner}}, effectsOfKind(control, EffectRestorePrimary),
		"positive control: with the reservation held, T11 restores the owner's primary")

	sweep := Decide(Input{Now: matures.Add(time.Hour), Act: Expire{}, Creds: creds, Primaries: prims,
		Reservations: res, Reqs: []Req{bg}})
	assert.Empty(t, effectsOfKind(sweep, EffectReleaseReservation),
		"the sweep must hold the reservation the open break-glass will restore")
	assert.Equal(t, VerdictNoChange, sweep.Verdict)

	// A break-glass opened one nanosecond later never needed it: the attacker's address counted.
	later := bg
	later.OpenedAt = matures.Add(time.Nanosecond)
	later.CompletesAt = later.OpenedAt.Add(DeltaCP)
	sweep = Decide(Input{Now: matures.Add(time.Hour), Act: Expire{}, Creds: creds, Primaries: prims,
		Reservations: res, Reqs: []Req{later}})
	assert.Len(t, effectsOfKind(sweep, EffectReleaseReservation), 1, "positive control: opened after maturity releases")
}

// ---- T7 -------------------------------------------------------------------------------

func pendingSSORemoval(id string) Req {
	return Req{ID: id, Kind: ReqCredentialRemove, Status: StatusPending, OriginClass: C1,
		Origins: []CredRef{fxPW}, Targets: []CredRef{fxSSO},
		OpenedAt: t0.Add(-time.Hour), CompletesAt: t0.Add(71 * time.Hour), AcceptedAddressNotice: true, MatureNoticesSettled: true}
}

func cancelCreds() []Cred {
	return []Cred{credMaturing(fxPW, t0.Add(-2*time.Hour)), matureCred(fxAddr), matureCred(fxSSO)}
}

// Parent §6.2 T7 applies to pending and disputed rows only; recovery_key rows are the authority's.
func TestCancel_OnlyPendingOrDisputedNonRecoveryKeyRows(t *testing.T) {
	ok := pendingSSORemoval("ok")
	disputed := pendingSSORemoval("disp")
	disputed.Status = StatusDisputed
	disputed.DisputedAt = t0.Add(-30 * time.Minute)
	done := pendingSSORemoval("done")
	done.Status = StatusCompleted
	gone := pendingSSORemoval("gone")
	gone.Status = StatusCancelled
	rk := pendingSSORemoval("rk")
	rk.Kind = ReqRecoveryKey

	in := inputAt(Cancel{IDs: []string{"ok", "disp", "done", "gone", "rk", "missing"}}, cancelCreds(), fxPW)
	in.Reqs = []Req{ok, disputed, done, gone, rk}
	out := Decide(in)
	require.Equal(t, RowT7, out.Row)
	require.Equal(t, VerdictApplied, out.Verdict)
	cancels := effectsOfKind(out, EffectCancel)
	cancelled := make([]string, 0, len(cancels))
	for _, e := range cancels {
		cancelled = append(cancelled, e.RequestID)
		assert.Equal(t, CancelProof, e.Reason)
		assert.Equal(t, C1, e.CancellerClass)
	}
	assert.ElementsMatch(t, []string{"ok", "disp"}, cancelled)
	assert.Len(t, noticesOfKind(out, NoticeChangeCancelled), 2, "one change_cancelled per cancel")
	assert.False(t, out.RotateEpoch, "T7 rotates nothing")
}

// A cancel that fails for every ID is a refusal; unknown IDs alone are not an error (D8).
func TestCancel_NothingCancellableIsRefused(t *testing.T) {
	in := inputAt(Cancel{IDs: []string{"a", "b"}}, cancelCreds(), fxPW)
	in.Reqs = []Req{pendingSSORemoval("x")}
	out := Decide(in)
	assert.Equal(t, VerdictRefused, out.Verdict)
	assert.Equal(t, RowNone, out.Row, "ruling 8: no ID names a pending or disputed non-recovery_key row")
	assert.Nil(t, out.Effects)
	assert.Nil(t, out.Notices)
}

// The canceller class is the class the proof reached at the request's opened_at, not a
// ceiling: a C2 proof against a C1-origin request records C2.
func TestCancel_RecordsTheCancellerClass(t *testing.T) {
	creds := []Cred{credMaturing(fxPW, t0.Add(-2*time.Hour)), credMaturing(fxTOTP, t0.Add(-2*time.Hour)), matureCred(fxSSO)}
	in := inputAt(Cancel{IDs: []string{"r"}}, creds, fxPW, fxTOTP)
	in.Reqs = []Req{pendingSSORemoval("r")}
	out := Decide(in)
	require.Equal(t, VerdictApplied, out.Verdict)
	cancels := effectsOfKind(out, EffectCancel)
	require.Len(t, cancels, 1)
	assert.Equal(t, C2, cancels[0].CancellerClass)
}

// Parent §6.1 maturity at the reference instant: a credential maturing exactly at opened_at
// does not count (D7).
func TestBoundary_CancelCredentialMaturingExactlyAtOpenedAt(t *testing.T) {
	r := pendingSSORemoval("r")
	creds := []Cred{credMaturing(fxPW, r.OpenedAt), matureCred(fxSSO)}
	in := inputAt(Cancel{IDs: []string{"r"}}, creds, fxPW)
	in.Reqs = []Req{r}
	refused := Decide(in)
	assert.Equal(t, VerdictRefused, refused.Verdict)
	assert.Equal(t, RowT7, refused.Row, "ruling 8: the request exists, so the T7 condition was evaluated and failed")

	creds[0] = credMaturing(fxPW, r.OpenedAt.Add(-time.Nanosecond))
	in = inputAt(Cancel{IDs: []string{"r"}}, creds, fxPW)
	in.Reqs = []Req{r}
	assert.Equal(t, VerdictApplied, Decide(in).Verdict)
}

// ---- T12 ------------------------------------------------------------------------------

func t12Creds() []Cred {
	return []Cred{credMaturing(fxTOTP, t0.Add(-2*time.Hour)), credMaturing(fxRK, t0.Add(-2*time.Hour)), matureCred(fxPW)}
}

func twoDisputed() []Req {
	return []Req{disputedReq("r1", t0.Add(-time.Hour)), disputedReq("r2", t0.Add(-time.Hour))}
}

// Mutation #32: T12 never completes anything, whatever the passage of time.
func TestResolve_NeverCompletes(t *testing.T) {
	reqs := twoDisputed()
	for i := range reqs {
		reqs[i].CompletesAt = t0.Add(-48 * time.Hour) // long past due
	}
	in := inputAt(Resolve{Group: "g", Chosen: "r1"}, t12Creds(), fxRK, fxTOTP)
	in.Reqs = reqs
	out := Decide(in)
	require.Equal(t, VerdictApplied, out.Verdict)
	assert.Empty(t, effectsOfKind(out, EffectComplete))
	assert.Len(t, effectsOfKind(out, EffectReturnToPending), 1)
	cancels := effectsOfKind(out, EffectCancel)
	require.Len(t, cancels, 1)
	assert.Equal(t, CancelDisputeResolved, cancels[0].Reason)
	assert.Equal(t, C3, cancels[0].CancellerClass)
	// Architect ruling 10: T12 never rotates; any rotation happens at the chosen row's T11.
	assert.False(t, out.RotateEpoch)
	assert.False(t, out.SetsPassword)
	assert.False(t, out.Continuation)
	// Ruling 9: each loser gets one change_cancelled, the chosen row none.
	notices := noticesOfKind(out, NoticeChangeCancelled)
	require.Len(t, notices, 1)
	assert.Equal(t, "r2", notices[0].RequestID)
	assert.Len(t, out.Notices, 1)
}

// D22: maturity at the earliest DisputedAt in the group. A proof credential that matured after
// the earliest dispute does not count even if it predates the later one.
func TestResolve_MaturityAtTheEarliestDisputedAtBoundary(t *testing.T) {
	early := t0.Add(-3 * time.Hour)
	reqs := []Req{disputedReq("r1", t0.Add(-time.Hour)), disputedReq("r2", early)}

	creds := []Cred{credMaturing(fxTOTP, early), credMaturing(fxRK, early.Add(-time.Hour))}
	in := inputAt(Resolve{Group: "g", Chosen: "r1"}, creds, fxRK, fxTOTP)
	in.Reqs = reqs
	refused := Decide(in)
	assert.Equal(t, VerdictRefused, refused.Verdict, "TOTP matured exactly at the earliest DisputedAt")
	assert.Equal(t, RowT12, refused.Row, "ruling 8: the class test failed")

	creds[0] = credMaturing(fxTOTP, early.Add(-time.Nanosecond))
	in = inputAt(Resolve{Group: "g", Chosen: "r1"}, creds, fxRK, fxTOTP)
	in.Reqs = reqs
	assert.Equal(t, VerdictApplied, Decide(in).Verdict, "one nanosecond earlier")
}

// A pending row that happens to carry the group name is not a member.
func TestResolve_OnlyDisputedRowsAreMembers(t *testing.T) {
	reqs := twoDisputed()
	pending := disputedReq("r3", t0.Add(-time.Hour))
	pending.Status = StatusPending
	reqs = append(reqs, pending)
	in := inputAt(Resolve{Group: "g", Chosen: "r3"}, t12Creds(), fxRK, fxTOTP)
	in.Reqs = reqs
	refused := Decide(in)
	assert.Equal(t, VerdictRefused, refused.Verdict, "r3 is pending, so it is not a member")
	assert.Equal(t, RowNone, refused.Row, "ruling 8: Chosen outside the group")
}
