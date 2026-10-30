package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plan Task 6: T3, T6, T6c, T8, T9, T10. Vectors cover the table rows; these tests pin the
// fingerprint rules (D19), the boundaries and the order independence the vectors cannot.

func recentCancel(reason CancelReason, canceller Class, keyed, coarse string, keyID int16, at time.Time) Req {
	return Req{
		ID: "old", Kind: ReqBreakGlass, Status: StatusCancelled, OriginClass: C1,
		OpenedAt: at.Add(-time.Hour), CompletesAt: at.Add(71 * time.Hour),
		Fingerprint: rawBytes(keyed), Coarse: rawBytes(coarse), FingerprintKeyID: keyID,
		CancelReason: reason, CancellerClass: canceller, CancelledAt: at,
	}
}

// mailboxReopen builds an OpenBreakGlass by a lone mailbox (C1) against the given recent cancels.
func mailboxReopen(cand Candidate, currentKey int16, recents ...Req) Input {
	in := inputAt(OpenBreakGlass{Candidate: cand}, standardCreds(), fxAddr)
	in.RecentCancels = recents
	in.CurrentKeyID = currentKey
	return in
}

// Review Focus 2: bytes.Equal(nil, nil) is true, so a redacted cancel and a candidate the
// runner failed to fill would otherwise "match" and refuse a legitimate open.
func TestOpen_EmptyFingerprintsNeverMatch(t *testing.T) {
	cancel := recentCancel(CancelProof, C3, "", "", 0, t0.Add(-time.Hour))
	out := Decide(mailboxReopen(Candidate{}, 0, cancel))
	assert.Equal(t, RowT6, out.Row)
	assert.Equal(t, VerdictOpened, out.Verdict)

	// One side empty never matches either.
	cancel = recentCancel(CancelProof, C3, "k", "co", 1, t0.Add(-time.Hour))
	out = Decide(mailboxReopen(Candidate{KeyID: 1}, 1, cancel))
	assert.Equal(t, RowT6, out.Row, "a candidate with no fingerprints")

	cancel = recentCancel(CancelProof, C3, "", "", 1, t0.Add(-time.Hour))
	out = Decide(mailboxReopen(Candidate{Keyed: []byte("k"), Coarse: []byte("co"), KeyID: 1}, 1, cancel))
	assert.Equal(t, RowT6, out.Row, "a cancel with no fingerprints")
}

// C4 mutation: a key-id mismatch must fall back to the coarse fingerprint (spec §7, 11-A).
func TestOpen_CoarseFallbackOnKeyMismatch(t *testing.T) {
	cancel := recentCancel(CancelProof, C3, "keyed-a", "coarse", 1, t0.Add(-time.Hour))
	cand := Candidate{Keyed: []byte("keyed-b"), Coarse: []byte("coarse"), KeyID: 2}
	out := Decide(mailboxReopen(cand, 2, cancel))
	assert.Equal(t, RowT9, out.Row, "coarse values equal, keyed values differ, key ids differ")
	assert.Equal(t, VerdictRefused, out.Verdict)

	// The converse: coarse values differ, keyed values equal, key ids differ: no match.
	cancel = recentCancel(CancelProof, C3, "same", "coarse-a", 1, t0.Add(-time.Hour))
	cand = Candidate{Keyed: []byte("same"), Coarse: []byte("coarse-b"), KeyID: 2}
	out = Decide(mailboxReopen(cand, 2, cancel))
	assert.Equal(t, RowT6, out.Row, "keyed values must not be compared across key ids")
}

// When the key ids agree (cancel, candidate and the current key), the keyed values decide.
func TestOpen_KeyedCompareWhenKeyIDsMatch(t *testing.T) {
	at := t0.Add(-time.Hour)
	cancel := recentCancel(CancelProof, C3, "same", "coarse-a", 1, at)
	out := Decide(mailboxReopen(Candidate{Keyed: []byte("same"), Coarse: []byte("coarse-b"), KeyID: 1}, 1, cancel))
	assert.Equal(t, RowT9, out.Row, "keyed values equal")

	cancel = recentCancel(CancelProof, C3, "keyed-a", "same-coarse", 1, at)
	out = Decide(mailboxReopen(Candidate{Keyed: []byte("keyed-b"), Coarse: []byte("same-coarse"), KeyID: 1}, 1, cancel))
	assert.Equal(t, RowT6, out.Row, "keyed values differ, so equal coarse values do not match")
}

// Empty keyed values on both sides compare coarse values.
func TestOpen_EmptyKeyedFallsBackToCoarse(t *testing.T) {
	cancel := recentCancel(CancelProof, C3, "", "coarse", 1, t0.Add(-time.Hour))
	out := Decide(mailboxReopen(Candidate{Coarse: []byte("coarse"), KeyID: 1}, 1, cancel))
	assert.Equal(t, RowT9, out.Row)

	cancel = recentCancel(CancelProof, C3, "", "coarse", 1, t0.Add(-time.Hour))
	out = Decide(mailboxReopen(Candidate{Coarse: nil, KeyID: 1}, 1, cancel))
	assert.Equal(t, RowT6, out.Row, "one-sided empty coarse never matches")
}

// Parent §6.2 T9: within N_cancel_cp. A cancel exactly NCancelCP old is outside the window.
func TestBoundary_CancelExactlyNCancelCPOldIsIgnored(t *testing.T) {
	const window = 30 * 24 * time.Hour
	cand := Candidate{Keyed: []byte("k"), Coarse: []byte("c"), KeyID: 1}

	old := recentCancel(CancelProof, C3, "k", "c", 1, t0.Add(-window))
	out := Decide(mailboxReopen(cand, 1, old))
	assert.Equal(t, RowT6, out.Row, "exactly NCancelCP old")
	assert.Equal(t, VerdictOpened, out.Verdict)

	inside := recentCancel(CancelProof, C3, "k", "c", 1, t0.Add(-window+time.Nanosecond))
	out = Decide(mailboxReopen(cand, 1, inside))
	assert.Equal(t, RowT9, out.Row, "one nanosecond inside the window")
}

// A recovery_key request is outside T7-T12 (parent §6.2): it never triggers T9 or T10.
func TestOpen_RecoveryKeyCancelNeverTriggersT9(t *testing.T) {
	cancel := recentCancel(CancelProof, C3, "k", "c", 1, t0.Add(-time.Hour))
	cancel.Kind = ReqRecoveryKey
	out := Decide(mailboxReopen(Candidate{Keyed: []byte("k"), Coarse: []byte("c"), KeyID: 1}, 1, cancel))
	assert.Equal(t, RowT6, out.Row)
}

// A refused T9 emits no notice and no effect (plan Task 6: "Emit no notice").
func TestOpen_T9EmitsNothing(t *testing.T) {
	cancel := recentCancel(CancelProof, C2, "k", "c", 1, t0.Add(-time.Hour))
	out := Decide(mailboxReopen(Candidate{Keyed: []byte("k"), Coarse: []byte("c"), KeyID: 1}, 1, cancel))
	assert.Equal(t, RowT9, out.Row)
	assert.Equal(t, VerdictRefused, out.Verdict)
	assert.Nil(t, out.Effects)
	assert.Nil(t, out.Notices)
	assert.False(t, out.RotateEpoch || out.SetsPassword)
}

// D14: order of checks. A recent cancel and a conflicting row both present: T9 wins over T8.
// The vectors cover this; here, the reversed pair of rows must not change the outcome.
func TestOpen_OutputIndependentOfOrder(t *testing.T) {
	cand := Candidate{Keyed: []byte("k"), Coarse: []byte("c"), KeyID: 1}
	conflicting := func(id string, origin CredRef) Req {
		return Req{ID: id, Kind: ReqBreakGlass, Status: StatusPending, OriginClass: C1,
			Origins: []CredRef{origin}, OpenedAt: t0.Add(-time.Hour), CompletesAt: t0.Add(71 * time.Hour),
			AcceptedAddressNotice: true, MatureNoticesSettled: true}
	}
	reqs := []Req{conflicting("r3", fxSSO), conflicting("r1", fxPW), conflicting("r2", fxTOTP)}
	unrelated := recentCancel(CancelProof, C3, "other", "other", 1, t0.Add(-time.Hour))
	unrelated.ID = "u1"
	unrelated2 := recentCancel(CancelHoldCap, C3, "k", "c", 1, t0.Add(-time.Hour))
	unrelated2.ID = "u2"
	recents := []Req{unrelated, unrelated2}

	build := func(rs, cs []Req) Input {
		in := mailboxReopen(cand, 1, cs...)
		in.Reqs = rs
		return in
	}
	base := Decide(build(reqs, recents))
	require.Equal(t, RowT8, base.Row)
	assert.Len(t, effectsOfKind(base, EffectDispute), 3)

	rev := func(rs []Req) []Req {
		out := make([]Req, len(rs))
		for i := range rs {
			out[len(rs)-1-i] = rs[i]
		}
		return out
	}
	assert.Equal(t, base, Decide(build(rev(reqs), recents)))
	assert.Equal(t, base, Decide(build(reqs, rev(recents))))
	assert.Equal(t, base, Decide(build(rev(reqs), rev(recents))))
}

// Spec §7: "Candidate ... carried by every opening action". The opening carries it so the runner
// can store the request fingerprint -- except T6c's completed row, whose Candidate is zero
// (architect ruling 15a).
func TestOpen_OpeningCarriesCandidate(t *testing.T) {
	cand := Candidate{Keyed: []byte("keyed"), Coarse: []byte("coarse"), KeyID: 7}
	creds := standardCreds()

	t.Run("T3", func(t *testing.T) {
		out := Decide(inputAt(Remove{Target: fxTOTP, Candidate: cand}, creds, fxPW, fxTOTP))
		opens := effectsOfKind(out, EffectOpen)
		require.Len(t, opens, 1)
		assert.Equal(t, cand, opens[0].Open.Candidate)
	})
	t.Run("T6", func(t *testing.T) {
		out := Decide(inputAt(OpenBreakGlass{Candidate: cand}, creds, fxAddr))
		opens := effectsOfKind(out, EffectOpen)
		require.Len(t, opens, 1)
		assert.Equal(t, cand, opens[0].Open.Candidate)
	})
	t.Run("T6c", func(t *testing.T) {
		out := Decide(inputAt(OpenBreakGlass{Candidate: cand}, creds, fxRK, fxTOTP))
		opens := effectsOfKind(out, EffectOpen)
		require.Len(t, opens, 1)
		assert.Equal(t, StatusCompleted, opens[0].Open.Status)
		assert.Equal(t, Candidate{}, opens[0].Open.Candidate, "ruling 15a: a completed row carries no fingerprint (child §6.2 CHECK)")
	})
}

// Plan Task 6 "Fields of the opening": Origins are the distinct Proof.Used; OpenedAt is Now;
// CompletesAt is Now + DeltaCP (72 h, parent §6.4); a repeated ref appears once.
func TestOpen_OpeningFields(t *testing.T) {
	out := Decide(inputAt(Remove{Target: fxTOTP}, standardCreds(), fxTOTP, fxPW, fxPW, fxTOTP))
	require.Equal(t, RowT3, out.Row)
	opens := effectsOfKind(out, EffectOpen)
	require.Len(t, opens, 1)
	op := opens[0].Open
	assert.Equal(t, ReqCredentialRemove, op.Kind)
	assert.Equal(t, StatusPending, op.Status)
	assert.Equal(t, C2, op.OriginClass)
	assert.ElementsMatch(t, []CredRef{fxPW, fxTOTP}, op.Origins, "distinct proof refs")
	assert.Equal(t, []CredRef{fxTOTP}, op.Targets)
	assert.True(t, op.OpenedAt.Equal(t0))
	assert.True(t, op.CompletesAt.Equal(t0.Add(72*time.Hour)))
	assert.True(t, op.DisputedAt.IsZero(), "a pending opening has no DisputedAt")
	assert.Empty(t, opens[0].RequestID, "D12: new IDs are the runner's")
}

// Parent §6.5 / #52: every request notice carries every reserved primary, sorted.
func TestOpen_RequestNoticeCarriesEveryReservation(t *testing.T) {
	in := inputAt(OpenBreakGlass{}, standardCreds(), fxAddr)
	a, b := cref(CredAddress, "old-a"), cref(CredAddress, "old-b")
	in.Reservations = []Reservation{
		{AddressRef: b, DisplacedAt: t0.Add(-48 * time.Hour), FloorUntil: t0.Add(time.Hour)},
		{AddressRef: a, DisplacedAt: t0.Add(-24 * time.Hour), FloorUntil: t0.Add(time.Hour)},
	}
	out := Decide(in)
	notices := noticesOfKind(out, NoticeChangeRequested)
	require.Len(t, notices, 1)
	assert.ElementsMatch(t, []CredRef{a, b}, notices[0].ReservedTo)
	assert.Empty(t, notices[0].RequestID, "D12: an empty RequestID refers to the request the outcome opens")
}

// A C3 break-glass with nothing in the way completes at once (T6c): Status completed,
// OpenedAt == CompletedAt == Now, the epoch rotates and a password is set.
func TestOpen_T6cOpensACompletedRow(t *testing.T) {
	out := Decide(inputAt(OpenBreakGlass{Reset: false}, standardCreds(), fxRK, fxTOTP))
	require.Equal(t, RowT6c, out.Row)
	assert.Equal(t, VerdictApplied, out.Verdict)
	assert.Equal(t, SourceBreakGlass, out.Source)
	assert.True(t, out.RotateEpoch)
	assert.True(t, out.SetsPassword)
	opens := effectsOfKind(out, EffectOpen)
	require.Len(t, opens, 1)
	assert.Equal(t, StatusCompleted, opens[0].Open.Status)
	assert.True(t, opens[0].Open.OpenedAt.Equal(t0))
	assert.True(t, opens[0].Open.CompletedAt.Equal(t0))
	assert.True(t, opens[0].Open.CompletesAt.Equal(t0), "ruling 5: nothing waits")
	assert.True(t, opens[0].Open.DisputedAt.IsZero())
	assert.Nil(t, opens[0].Open.Targets, "ruling 4: a break-glass opening has no Targets")
	assert.Len(t, noticesOfKind(out, NoticeChangeCompleted), 1)
	assert.Len(t, noticesOfKind(out, NoticePasswordChanged), 1)
}

// Architect ruling 2: keyed fingerprints are compared only when the cancel's key id, the
// candidate's key id and CurrentKeyID are all equal. A stale shared key falls back to coarse.
func TestOpen_SharedStaleKeyFallsBackToCoarse(t *testing.T) {
	at := t0.Add(-time.Hour)

	// Cancel and candidate share key 1, but the current key is 2: equal keyed values are not enough.
	cancel := recentCancel(CancelProof, C3, "same-keyed", "coarse-a", 1, at)
	out := Decide(mailboxReopen(Candidate{Keyed: []byte("same-keyed"), Coarse: []byte("coarse-b"), KeyID: 1}, 2, cancel))
	assert.Equal(t, RowT6, out.Row, "equal keyed values under a stale key, coarse values differ: no match")

	// Equal coarse values match even though the keyed values differ.
	cancel = recentCancel(CancelProof, C3, "keyed-a", "coarse", 1, at)
	out = Decide(mailboxReopen(Candidate{Keyed: []byte("keyed-b"), Coarse: []byte("coarse"), KeyID: 1}, 2, cancel))
	assert.Equal(t, RowT9, out.Row, "differing keyed values under a stale key, coarse values equal: match")
}

// Architect ruling A2: Opening.Origins holds only the Proof.Used refs that match a credential
// exactly and count at Now, so an immature, stale or unknown ref cannot change the origin set.
func TestOpen_OriginsHoldOnlyCountedRefs(t *testing.T) {
	immature := credMaturing(fxSSO, t0.Add(time.Hour))
	creds := []Cred{matureCred(fxPW), matureCred(fxAddr), matureCred(fxTOTP), immature}
	stale := CredRef{Kind: CredPassword, ID: "pw", Version: 9}
	ghost := cref(CredTOTP, "ghost")

	out := Decide(inputAt(Remove{Target: fxTOTP}, creds, fxPW, fxTOTP, fxSSO, stale, ghost))
	require.Equal(t, RowT3, out.Row)
	opens := effectsOfKind(out, EffectOpen)
	require.Len(t, opens, 1)
	assert.Equal(t, []CredRef{fxPW, fxTOTP}, opens[0].Open.Origins)

	// Adding an uncounted ref never turns a duplicate open into a dispute.
	existing := Req{ID: "r1", Kind: ReqBreakGlass, Status: StatusPending, OriginClass: C1,
		Origins: []CredRef{fxAddr}, OpenedAt: t0.Add(-time.Hour), CompletesAt: t0.Add(71 * time.Hour)}
	in := inputAt(OpenBreakGlass{}, creds, fxAddr, fxSSO)
	in.Reqs = []Req{existing}
	out = Decide(in)
	assert.Equal(t, VerdictRefused, out.Verdict, "the immature ref leaves the origin set equal to the existing row's")
	assert.Equal(t, RowNone, out.Row)
	assert.Nil(t, out.Effects)
}

// Architect ruling 9: a T8 or T10 opening notifies the opener with one change_disputed (and no
// change_requested), and each pending conflicting row that moves to disputed with one more,
// carrying that row's ID. A row already disputed that only changes group gets nothing.
func TestOpen_DisputedOpeningNotices(t *testing.T) {
	pending := Req{ID: "p1", Kind: ReqBreakGlass, Status: StatusPending, OriginClass: C1, Origins: []CredRef{fxPW},
		OpenedAt: t0.Add(-time.Hour), CompletesAt: t0.Add(71 * time.Hour)}
	already := Req{ID: "d1", Kind: ReqBreakGlass, Status: StatusDisputed, OriginClass: C1, Origins: []CredRef{fxSSO},
		OpenedAt: t0.Add(-3 * time.Hour), DisputedAt: t0.Add(-time.Hour), DisputeGroup: "ga"}
	in := inputAt(OpenBreakGlass{}, standardCreds(), fxAddr)
	in.Reqs = []Req{pending, already}
	out := Decide(in)
	require.Equal(t, RowT8, out.Row)
	assert.Empty(t, noticesOfKind(out, NoticeChangeRequested))
	disputed := noticesOfKind(out, NoticeChangeDisputed)
	require.Len(t, disputed, 2)
	assert.Equal(t, "", disputed[0].RequestID, "the opener's notice names the request the outcome opens")
	assert.Equal(t, "p1", disputed[1].RequestID)
	assert.Equal(t, CredRef{}, disputed[0].Subject)

	// The opener joins ga, the pending row moves to ga, and the disputed row needs no effect.
	opens := effectsOfKind(out, EffectOpen)
	require.Len(t, opens, 1)
	assert.Equal(t, "ga", opens[0].Group)
	disputes := effectsOfKind(out, EffectDispute)
	require.Len(t, disputes, 1)
	assert.Equal(t, "p1", disputes[0].RequestID)
	assert.Equal(t, "ga", disputes[0].Group)
}

// Architect ruling 12: group union. Every member of every group that holds a conflicting row
// moves into the smallest such group; a row already in it gets no effect.
func TestOpen_GroupUnionMovesEveryMemberOfEveryTouchedGroup(t *testing.T) {
	mk := func(id, group string, origin CredRef) Req {
		return Req{ID: id, Kind: ReqBreakGlass, Status: StatusDisputed, OriginClass: C1, Origins: []CredRef{origin},
			OpenedAt: t0.Add(-4 * time.Hour), DisputedAt: t0.Add(-time.Hour), DisputeGroup: group}
	}
	in := inputAt(OpenBreakGlass{}, standardCreds(), fxAddr)
	in.Reqs = []Req{mk("a1", "gb", fxPW), mk("a2", "ga", fxSSO), mk("a3", "gb", fxTOTP), mk("z9", "gz", fxRK)}
	out := Decide(in)
	require.Equal(t, RowT8, out.Row)
	opens := effectsOfKind(out, EffectOpen)
	require.Len(t, opens, 1)
	assert.Equal(t, "ga", opens[0].Group, "the smallest group id among the conflicting rows")
	moved := map[string]string{}
	for _, e := range effectsOfKind(out, EffectDispute) {
		moved[e.RequestID] = e.Group
	}
	assert.Equal(t, map[string]string{"a1": "ga", "a3": "ga", "z9": "ga"}, moved,
		"every row outside ga moves; a2 is already in ga and gets no effect")
}

// A T3 request opened at an equal class never rotates the epoch or sets a password.
func TestOpen_T3AndT6RotateNothing(t *testing.T) {
	for _, in := range []Input{
		inputAt(Remove{Target: fxTOTP}, standardCreds(), fxPW, fxTOTP),
		inputAt(OpenBreakGlass{}, standardCreds(), fxAddr),
	} {
		out := Decide(in)
		assert.Equal(t, VerdictOpened, out.Verdict)
		assert.False(t, out.RotateEpoch)
		assert.False(t, out.SetsPassword)
		assert.False(t, out.Continuation)
	}
}
