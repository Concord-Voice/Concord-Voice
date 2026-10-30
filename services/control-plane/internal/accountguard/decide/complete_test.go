package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plan Task 9: T11 (parent §6.2 T11 and Holds). Rows are in testdata/vectors.json; these tests
// pin the boundaries and the hold arithmetic. The EffectHold emitted for a held request that is
// not yet due is a plan gap, so no test here puts a held request before its due instant.

func matureAtOpen() []Cred {
	earlier := t0.Add(-100 * time.Hour)
	return []Cred{credMaturing(fxPW, earlier), credMaturing(fxAddr, earlier), credMaturing(fxSSO, earlier), credMaturing(fxTOTP, earlier)}
}

// dueRemoval is a credential_remove whose CompletesAt is now and whose notices were accepted.
func dueRemoval() Req {
	return Req{ID: "r1", Kind: ReqCredentialRemove, Status: StatusPending, OriginClass: C1,
		Origins: []CredRef{fxPW}, Targets: []CredRef{fxSSO},
		OpenedAt: t0.Add(-72 * time.Hour), CompletesAt: t0, AcceptedAddressNotice: true, MatureNoticesSettled: true}
}

// setCompletesAt moves a request's due instant and its opening together, so the row keeps the
// shape every guard row has: it opened exactly DeltaCP before it completes (H3 refuses any row
// that completes sooner).
func setCompletesAt(r *Req, at time.Time) {
	r.CompletesAt = at
	r.OpenedAt = at.Add(-DeltaCP)
}

func completeInput(r Req) Input {
	in := inputAt(Complete{ID: r.ID}, matureAtOpen())
	in.Reqs = []Req{r}
	return in
}

// Parent §6.2 T11: now >= completes_at + held time. At equality it completes; one nanosecond
// earlier it changes nothing.
func TestBoundary_Completion(t *testing.T) {
	heldMs := (10 * time.Hour).Milliseconds()

	t.Run("no held time", func(t *testing.T) {
		r := dueRemoval()
		out := Decide(completeInput(r))
		assert.Equal(t, VerdictApplied, out.Verdict, "Now == CompletesAt completes")
		assert.Len(t, effectsOfKind(out, EffectComplete), 1)

		setCompletesAt(&r, t0.Add(time.Nanosecond))
		out = Decide(completeInput(r))
		assert.Equal(t, VerdictNoChange, out.Verdict)
		assert.Nil(t, out.Effects)
		assert.Nil(t, out.Notices)
	})
	t.Run("with settled held time", func(t *testing.T) {
		r := dueRemoval()
		r.HeldMs = heldMs
		setCompletesAt(&r, t0.Add(-10*time.Hour)) // CompletesAt + held == Now
		out := Decide(completeInput(r))
		assert.Equal(t, VerdictApplied, out.Verdict, "Now == CompletesAt + held completes")

		setCompletesAt(&r, t0.Add(-10*time.Hour+time.Nanosecond))
		out = Decide(completeInput(r))
		assert.Equal(t, VerdictNoChange, out.Verdict, "one nanosecond before CompletesAt + held")
		assert.Empty(t, effectsOfKind(out, EffectComplete))
	})
}

// Parent §6.2 Holds: a request reaching HOLD_CAP is cancelled with no change. accrued ==
// HoldCap cancels; one millisecond less does not (D-plan Task 9 step 3).
func TestBoundary_HoldCap(t *testing.T) {
	capMs := (72 * time.Hour).Milliseconds()
	r := dueRemoval()
	r.AcceptedAddressNotice = false // held

	r.HeldMs = capMs
	out := Decide(completeInput(r))
	require.Equal(t, VerdictApplied, out.Verdict)
	cancels := effectsOfKind(out, EffectCancel)
	require.Len(t, cancels, 1)
	assert.Equal(t, CancelHoldCap, cancels[0].Reason)
	assert.Equal(t, "r1", cancels[0].RequestID)
	assert.Empty(t, effectsOfKind(out, EffectRemove), "a hold-cap cancel changes nothing")
	assert.Len(t, noticesOfKind(out, NoticeChangeCancelled), 1)
	assert.Empty(t, effectsOfKind(out, EffectHold), "no open hold, so nothing to fold")

	// One millisecond under the cap. A reachable row is already due: CompletesAt + HeldMs <= Now
	// (architect ruling 2-Q1), so CompletesAt = Now - HeldMs.
	r.HeldMs = capMs - 1
	setCompletesAt(&r, t0.Add(-time.Duration(r.HeldMs)*time.Millisecond))
	out = Decide(completeInput(r))
	assert.Equal(t, VerdictHeld, out.Verdict)
	assert.Empty(t, effectsOfKind(out, EffectCancel))
	holds := effectsOfKind(out, EffectHold)
	require.Len(t, holds, 1, "a due, held row opens its hold at Now")
	assert.True(t, holds[0].HeldSince.Equal(t0))
}

// Plan Task 9 step 3: accrued = HeldMs + (Now - HeldSince) when HeldSince is set.
func TestComplete_HeldTimeAccruesFromHeldSince(t *testing.T) {
	r := dueRemoval()
	r.AcceptedAddressNotice = false
	r.HeldMs = 0
	r.HeldSince = t0.Add(-72 * time.Hour)
	out := Decide(completeInput(r))
	require.Equal(t, VerdictApplied, out.Verdict)
	cancels := effectsOfKind(out, EffectCancel)
	require.Len(t, cancels, 1)
	assert.Equal(t, CancelHoldCap, cancels[0].Reason)
	// Ruling 1 corollary: a row leaving pending with an open hold also folds it.
	holds := effectsOfKind(out, EffectHold)
	require.Len(t, holds, 1)
	assert.Equal(t, "r1", holds[0].RequestID)
	assert.Equal(t, (72 * time.Hour).Milliseconds(), holds[0].HeldMs)
	assert.True(t, holds[0].HeldSince.IsZero())
	cancelNotices := noticesOfKind(out, NoticeChangeCancelled)
	require.Len(t, cancelNotices, 1)
	assert.Equal(t, "r1", cancelNotices[0].RequestID)

	r.HeldSince = t0.Add(-72*time.Hour + time.Millisecond)
	out = Decide(completeInput(r))
	assert.Empty(t, effectsOfKind(out, EffectCancel), "one millisecond short of the cap")

	// Settled and open time add: 40 h settled + 32 h open reaches the cap.
	r.HeldMs = (40 * time.Hour).Milliseconds()
	r.HeldSince = t0.Add(-32 * time.Hour)
	out = Decide(completeInput(r))
	assert.Len(t, effectsOfKind(out, EffectCancel), 1)
}

// Architect ruling 1: a hold is measured from the due instant, not from the moment the
// notice went unaccepted. With the budget locked and no covering credential, HeldSince =
// CompletesAt: one nanosecond before CompletesAt + HoldCap the request is held with no effect,
// and at CompletesAt + HoldCap it is cancelled as hold_cap with the hold folded.
func TestBoundary_HoldCapMeasuredFromDue(t *testing.T) {
	completesAt := t0.Add(-5 * time.Hour)
	r := dueRemoval()
	setCompletesAt(&r, completesAt)
	r.HeldSince = completesAt

	at := func(now time.Time) Outcome {
		in := completeInput(r)
		in.Now = now
		in.GuessableBudgetClear = false // uncovered: no WebAuthn or anchor credential
		return Decide(in)
	}

	out := at(completesAt.Add(72*time.Hour - time.Nanosecond))
	assert.Equal(t, RowT11, out.Row)
	assert.Equal(t, VerdictHeld, out.Verdict)
	assert.Nil(t, out.Effects, "an open hold already exists, and the cap is not yet reached")
	assert.Nil(t, out.Notices)

	out = at(completesAt.Add(72 * time.Hour))
	require.Equal(t, VerdictApplied, out.Verdict)
	cancels := effectsOfKind(out, EffectCancel)
	require.Len(t, cancels, 1)
	assert.Equal(t, CancelHoldCap, cancels[0].Reason)
	holds := effectsOfKind(out, EffectHold)
	require.Len(t, holds, 1)
	assert.Equal(t, int64(259200000), holds[0].HeldMs)
	assert.True(t, holds[0].HeldSince.IsZero())
	assert.Len(t, noticesOfKind(out, NoticeChangeCancelled), 1)
}

// Architect ruling 1, steps 3-5: the cap check runs before the due check, and a request that is
// not yet due changes nothing -- it opens no hold, even if a notice is unaccepted.
func TestComplete_NotYetDueOpensNoHold(t *testing.T) {
	r := dueRemoval()
	r.AcceptedAddressNotice = false
	setCompletesAt(&r, t0.Add(time.Hour))
	out := Decide(completeInput(r))
	assert.Equal(t, RowT11, out.Row)
	assert.Equal(t, VerdictNoChange, out.Verdict)
	assert.Nil(t, out.Effects)
	assert.Nil(t, out.Notices)

	// The cap check still comes first: an accrued hold at the cap cancels even though not due.
	r.HeldMs = (72 * time.Hour).Milliseconds()
	out = Decide(completeInput(r))
	assert.Equal(t, VerdictApplied, out.Verdict)
	assert.Len(t, effectsOfKind(out, EffectCancel), 1)
}

// Architect ruling 3: every notice that belongs to a request carries its ID; ReservedTo is every
// reservation; Subject is zero on change_* and the removed credential on credential_removed.
func TestComplete_NoticesCarryRequestIDAndReservations(t *testing.T) {
	reserved := []CredRef{cref(CredAddress, "res-b"), cref(CredAddress, "res-a")}
	in := completeInput(dueRemoval())
	in.Reservations = []Reservation{
		{AddressRef: reserved[0], DisplacedAt: t0.Add(-48 * time.Hour), FloorUntil: t0.Add(time.Hour)},
		{AddressRef: reserved[1], DisplacedAt: t0.Add(-24 * time.Hour), FloorUntil: t0.Add(time.Hour)},
	}
	out := Decide(in)
	require.Equal(t, VerdictApplied, out.Verdict)
	completed := noticesOfKind(out, NoticeChangeCompleted)
	require.Len(t, completed, 1, "every T11 completion emits one change_completed")
	removed := noticesOfKind(out, NoticeCredentialRemoved)
	require.Len(t, removed, 1)
	for _, n := range []Notice{completed[0], removed[0]} {
		assert.Equal(t, "r1", n.RequestID)
		assert.Equal(t, []CredRef{cref(CredAddress, "res-a"), cref(CredAddress, "res-b")}, n.ReservedTo, "sorted")
	}
	assert.Equal(t, CredRef{}, completed[0].Subject)
	assert.Equal(t, fxSSO, removed[0].Subject)
}

// Architect ruling 3: one credential_removed per EffectRemove, break-glass removals included.
func TestComplete_BreakGlassEmitsOneCredentialRemovedPerRemoval(t *testing.T) {
	bg := dueRemoval()
	bg.Kind = ReqBreakGlass
	bg.Targets = []CredRef{fxPW}
	bg.Origins = []CredRef{fxPW}
	creds := append(matureAtOpen(), credMaturing(fxWA1, t0.Add(-10*time.Hour)), credMaturing(cref(CredWebAuthn, "wa1b"), t0.Add(-5*time.Hour)))
	in := inputAt(Complete{ID: bg.ID}, creds)
	in.Reqs = []Req{bg}
	out := Decide(in)
	require.Equal(t, VerdictApplied, out.Verdict)
	removes := effectsOfKind(out, EffectRemove)
	require.Len(t, removes, 2)
	notices := noticesOfKind(out, NoticeCredentialRemoved)
	require.Len(t, notices, 2)
	removedRefs := make([]CredRef, 0, len(removes))
	subjects := make([]CredRef, 0, len(notices))
	for _, e := range removes {
		removedRefs = append(removedRefs, e.Cred)
		assert.Empty(t, e.RequestID, "ruling 11: a removal carries no request ID")
	}
	for _, n := range notices {
		subjects = append(subjects, n.Subject)
		assert.Equal(t, "r1", n.RequestID)
	}
	assert.ElementsMatch(t, removedRefs, subjects)
	assert.Len(t, noticesOfKind(out, NoticeChangeCompleted), 1)
	assert.Len(t, noticesOfKind(out, NoticePasswordChanged), 1)
}

// Plan Task 9 step 4: a held request with no open hold starts one at Now.
func TestComplete_HeldRequestStartsAHoldAtNow(t *testing.T) {
	r := dueRemoval()
	r.MatureNoticesSettled = false
	r.HeldMs = 1234
	// Reachable state (after a T12 return): the row is due, CompletesAt + HeldMs <= Now
	// (architect ruling 2-Q1).
	setCompletesAt(&r, t0.Add(-1234*time.Millisecond))
	out := Decide(completeInput(r))
	require.Equal(t, VerdictHeld, out.Verdict)
	holds := effectsOfKind(out, EffectHold)
	require.Len(t, holds, 1)
	assert.Equal(t, "r1", holds[0].RequestID)
	assert.Equal(t, int64(1234), holds[0].HeldMs)
	assert.True(t, holds[0].HeldSince.Equal(t0))
	assert.Empty(t, effectsOfKind(out, EffectComplete))
}

// Architect ruling 2-Q1: a held row waits for CompletesAt + accrued, one definition of "due" for
// the held and the unheld branch. CompletesAt = Now with HeldMs > 0 and no open hold is reachable
// only after a backward DB-clock step; it must change nothing and open no hold. This kills the
// CompletesAt-only mutant (hold opened before CompletesAt + HeldMs).
func TestComplete_HeldRowWaitsForSettledHeldTime(t *testing.T) {
	r := dueRemoval()
	r.MatureNoticesSettled = false
	r.HeldMs = 1234
	setCompletesAt(&r, t0)
	out := Decide(completeInput(r))
	assert.Equal(t, RowT11, out.Row)
	assert.Equal(t, VerdictNoChange, out.Verdict)
	assert.Nil(t, out.Effects)
	assert.Nil(t, out.Notices)
}

// A request that is neither pending nor of a completable kind is refused with no effects.
func TestComplete_NonPendingRefused(t *testing.T) {
	for _, status := range []ReqStatus{StatusDisputed, StatusCompleted, StatusCancelled, StatusUnknown} {
		r := dueRemoval()
		r.Status = status
		out := Decide(completeInput(r))
		assert.Equal(t, VerdictRefused, out.Verdict, "status %d", status)
		assert.Equal(t, RowNone, out.Row, "status %d: ruling 8, Complete refuses with RowNone", status)
		assert.Nil(t, out.Effects, "status %d", status)
		assert.Nil(t, out.Notices, "status %d", status)
	}
}

// A refused Complete on a break-glass row still carries break_glass (D11), so the runner never
// branches before SourceTx.
func TestComplete_RefusedBreakGlassCarriesBreakGlassSource(t *testing.T) {
	r := dueRemoval()
	r.Kind = ReqBreakGlass
	r.Status = StatusCompleted
	out := Decide(completeInput(r))
	assert.Equal(t, VerdictRefused, out.Verdict)
	assert.Equal(t, SourceBreakGlass, out.Source)
}

// Parent §6.2: recovery_key rows are outside T7-T12 and both holds; the authority completes them.
func TestComplete_RecoveryKeyRefused(t *testing.T) {
	r := dueRemoval()
	r.Kind = ReqRecoveryKey
	out := Decide(completeInput(r))
	assert.Equal(t, VerdictRefused, out.Verdict)
	assert.Nil(t, out.Effects)
}

// D15: a completed T3 removal rotates nothing and sets no password; only a break-glass does.
func TestComplete_OnlyBreakGlassRotatesAndSetsAPassword(t *testing.T) {
	out := Decide(completeInput(dueRemoval()))
	require.Equal(t, VerdictApplied, out.Verdict)
	assert.False(t, out.RotateEpoch)
	assert.False(t, out.SetsPassword)
	assert.Equal(t, SourceUserSession, out.Source)

	bg := dueRemoval()
	bg.Kind = ReqBreakGlass
	bg.Targets = []CredRef{fxPW}
	out = Decide(completeInput(bg))
	require.Equal(t, VerdictApplied, out.Verdict)
	assert.True(t, out.RotateEpoch)
	assert.True(t, out.SetsPassword)
	assert.Equal(t, SourceBreakGlass, out.Source)
	assert.Len(t, noticesOfKind(out, NoticePasswordChanged), 1)
}

// Plan Task 9 step 7: the completed removal targets the CURRENT credential at the recorded slot.
func TestComplete_RemovesTheCurrentTarget(t *testing.T) {
	out := Decide(completeInput(dueRemoval()))
	removes := effectsOfKind(out, EffectRemove)
	require.Len(t, removes, 1)
	assert.Equal(t, fxSSO, removes[0].Cred)
	assert.True(t, out.PermGenBump)
	assert.Len(t, effectsOfKind(out, EffectResyncMFAFlags), 1)
}

// The target fence (parent §6.2 T11; mutation #33): every non-break-glass, non-recovery_key kind
// is cancelled as superseded when a target changed version; a break-glass is exempt (#38).
func TestComplete_TargetFenceByKind(t *testing.T) {
	bumped := matureAtOpen()
	bumped[2] = credMaturing(CredRef{Kind: CredSSO, ID: "sso", Version: 2}, t0.Add(-100*time.Hour))

	for _, kind := range []ReqKind{ReqCredentialRemove, ReqAddressChange, ReqSocialRecovery, ReqCircleChange} {
		r := dueRemoval()
		r.Kind = kind
		in := inputAt(Complete{ID: r.ID}, bumped)
		in.Reqs = []Req{r}
		out := Decide(in)
		cancels := effectsOfKind(out, EffectCancel)
		require.Len(t, cancels, 1, "kind %d", kind)
		assert.Equal(t, CancelSuperseded, cancels[0].Reason, "kind %d", kind)
		assert.Empty(t, effectsOfKind(out, EffectComplete), "kind %d", kind)
	}

	r := dueRemoval()
	r.Kind = ReqBreakGlass
	in := inputAt(Complete{ID: r.ID}, bumped)
	in.Reqs = []Req{r}
	out := Decide(in)
	assert.Len(t, effectsOfKind(out, EffectComplete), 1, "a break-glass is exempt from the fence")
	assert.Empty(t, effectsOfKind(out, EffectCancel))
}

// Plan Task 9 step 6 precedes completion: a superseded request is cancelled even when it is not
// held, and a held-but-capped one is cancelled as hold_cap, not superseded (step 3 first).
func TestComplete_HoldCapPrecedesTheFence(t *testing.T) {
	bumped := matureAtOpen()
	bumped[2] = credMaturing(CredRef{Kind: CredSSO, ID: "sso", Version: 2}, t0.Add(-100*time.Hour))
	r := dueRemoval()
	r.AcceptedAddressNotice = false
	r.HeldMs = (72 * time.Hour).Milliseconds()
	in := inputAt(Complete{ID: r.ID}, bumped)
	in.Reqs = []Req{r}
	cancels := effectsOfKind(Decide(in), EffectCancel)
	require.Len(t, cancels, 1)
	assert.Equal(t, CancelHoldCap, cancels[0].Reason)
}

// Mutation #21 / spec §4 input 1: the restore target is the newest primary that counted at
// opened_at, still reserved, and not the current one.
func TestComplete_RestoreTargetIsTheNewestReservedPrimaryMatureAtOpen(t *testing.T) {
	bg := dueRemoval()
	bg.Kind = ReqBreakGlass
	bg.Targets = []CredRef{fxPW}
	older := cref(CredAddress, "older")
	newer := cref(CredAddress, "newer")
	immatureAtOpen := cref(CredAddress, "immature-at-open")
	current := cref(CredAddress, "current")

	in := completeInput(bg)
	in.Primaries = []Primary{
		{Ref: older, BecamePrimaryAt: t0.Add(-400 * time.Hour), MaturesAt: t0.Add(-300 * time.Hour), DisplacedAt: t0.Add(-200 * time.Hour)},
		{Ref: newer, BecamePrimaryAt: t0.Add(-200 * time.Hour), MaturesAt: t0.Add(-150 * time.Hour), DisplacedAt: t0.Add(-60 * time.Hour)},
		{Ref: immatureAtOpen, BecamePrimaryAt: t0.Add(-60 * time.Hour), MaturesAt: t0.Add(-30 * time.Hour), DisplacedAt: t0.Add(-20 * time.Hour)},
		{Ref: current, BecamePrimaryAt: t0.Add(-20 * time.Hour), MaturesAt: t0.Add(52 * time.Hour)},
	}
	in.Reservations = []Reservation{
		{AddressRef: older, DisplacedAt: t0.Add(-200 * time.Hour), FloorUntil: t0.Add(time.Hour)},
		{AddressRef: newer, DisplacedAt: t0.Add(-60 * time.Hour), FloorUntil: t0.Add(time.Hour)},
		{AddressRef: immatureAtOpen, DisplacedAt: t0.Add(-20 * time.Hour), FloorUntil: t0.Add(time.Hour)},
	}
	out := Decide(in)
	require.Equal(t, VerdictApplied, out.Verdict)
	restores := effectsOfKind(out, EffectRestorePrimary)
	require.Len(t, restores, 1)
	assert.Equal(t, newer, restores[0].Cred, "newest primary mature at opened_at; the later one was immature then")
	assert.Len(t, effectsOfKind(out, EffectResyncMFAFlags), 1)

	// Without a reservation for it, a primary is not a candidate.
	in.Reservations = in.Reservations[:1]
	restores = effectsOfKind(Decide(in), EffectRestorePrimary)
	require.Len(t, restores, 1)
	assert.Equal(t, older, restores[0].Cred)

	// With nothing reserved, nothing is restored.
	in.Reservations = nil
	assert.Empty(t, effectsOfKind(Decide(in), EffectRestorePrimary))
}
