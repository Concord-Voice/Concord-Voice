package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plan Task 7: T5 and T1b's 3-A contest set (parent §6.2 T5; spec §3.0 LEAD-B 3-A; D20).
// Rows are in testdata/vectors.json; these tests pin D20's unknown-class and unset-instant rules.

// phisherCred returns an immature credential enrolled an hour before t0 at the given class.
func phisherCred(r CredRef, enrolClass Class) Cred {
	c := credMaturing(r, t0.Add(70*time.Hour))
	c.EnrolledAt = t0.Add(-time.Hour)
	c.EnrolClass = enrolClass
	return c
}

// ownerCreds are the account's real, long-mature credentials; the TOTP matured before the
// phisher's enrolment.
func ownerCreds() []Cred {
	earlier := t0.Add(-3 * time.Hour)
	return []Cred{
		credMaturing(fxPW, earlier), credMaturing(fxAddr, earlier),
		credMaturing(fxTOTP, earlier), credMaturing(fxRK, earlier),
	}
}

func contestedRefs(o Outcome) []CredRef {
	contests := effectsOfKind(o, EffectContest)
	refs := make([]CredRef, 0, len(contests))
	for _, e := range contests {
		refs = append(refs, e.Cred)
	}
	return refs
}

// D20: an unknown enrolment class (NULL in the database, ClassNone here) maps to the rank floor.
func TestContest_UnrecordedEnrolClassUsesRankFloor(t *testing.T) {
	phisher := cref(CredWebAuthn, "phisher")
	creds := append(ownerCreds(), phisherCred(phisher, ClassNone))
	out := Decide(inputAt(Contest{}, creds, fxTOTP))
	require.Equal(t, RowT5, out.Row)
	require.Equal(t, VerdictApplied, out.Verdict)
	assert.Equal(t, []CredRef{phisher}, contestedRefs(out), "a C2 proof meets the rank-2 floor")
	assert.True(t, out.RotateEpoch, "T5 rotates the epoch (#40)")

	// The floor is real: a lone mailbox (C1) does not meet rank 2.
	out = Decide(inputAt(Contest{}, creds, fxAddr))
	assert.Equal(t, VerdictRefused, out.Verdict)
	assert.Empty(t, contestedRefs(out))
}

// D20: a zero EnrolledAt means no column value reached Decide; nothing counts, so the
// credential cannot be contested (fails closed).
func TestContest_UnsetEnrolledAtIsUncontestable(t *testing.T) {
	phisher := phisherCred(cref(CredWebAuthn, "phisher"), C2)
	phisher.EnrolledAt = time.Time{}
	creds := append(ownerCreds(), phisher)
	out := Decide(inputAt(Contest{}, creds, fxRK, fxTOTP)) // even a C3 proof
	assert.Equal(t, RowT5, out.Row)
	assert.Equal(t, VerdictRefused, out.Verdict)
	assert.Nil(t, out.Effects)
	assert.False(t, out.RotateEpoch)
}

// Parent §6.2 T5: "never below rank(X) for a K or P target". A K credential's floor is C1.
func TestContest_KnowledgeTargetIsContestedAtC1(t *testing.T) {
	newPW := cref(CredPassword, "pw-new")
	creds := append(ownerCreds(), phisherCred(newPW, ClassNone))
	out := Decide(inputAt(Contest{}, creds, fxAddr))
	require.Equal(t, VerdictApplied, out.Verdict)
	assert.Equal(t, []CredRef{newPW}, contestedRefs(out))
}

// An already contested credential is not contested again, and a mature one is not contestable.
func TestContest_OnlyUncontestedImmatureCredentialsAreInTheSet(t *testing.T) {
	already := phisherCred(cref(CredWebAuthn, "already"), C2)
	already.ContestedAt = t0.Add(-30 * time.Minute)
	mature := matureCred(cref(CredWebAuthn, "mature"))
	mature.EnrolledAt = t0.Add(-time.Hour)
	mature.EnrolClass = C2
	creds := append(ownerCreds(), already, mature)
	out := Decide(inputAt(Contest{}, creds, fxTOTP))
	assert.Equal(t, VerdictRefused, out.Verdict)
	assert.Empty(t, contestedRefs(out))
}

// The set is per credential: one the proof outranks is contested, one enrolled at a higher
// class is not.
func TestContest_ContestsEachCredentialAgainstItsOwnThreshold(t *testing.T) {
	low := cref(CredWebAuthn, "low")
	high := cref(CredTOTP, "high")
	creds := append(ownerCreds(), phisherCred(low, C2), phisherCred(high, C3))
	out := Decide(inputAt(Contest{}, creds, fxTOTP))
	require.Equal(t, VerdictApplied, out.Verdict)
	assert.Equal(t, []CredRef{low}, contestedRefs(out))
	notices := noticesOfKind(out, NoticeEnrolmentContested)
	require.Len(t, notices, 1, "one enrolment_contested per contested credential")
	assert.Equal(t, low, notices[0].Subject)
	assert.True(t, out.PermGenBump)
	assert.Len(t, effectsOfKind(out, EffectResyncMFAFlags), 1)
}

// Architect ruling 3: enrolment_contested carries the contested credential as Subject, no request
// ID and no reservations, even when a reservation exists.
func TestContest_NoticeCarriesNoRequestOrReservation(t *testing.T) {
	phisher := cref(CredWebAuthn, "phisher")
	creds := append(ownerCreds(), phisherCred(phisher, C2))
	in := inputAt(Contest{}, creds, fxTOTP)
	in.Reservations = []Reservation{{AddressRef: cref(CredAddress, "old"), DisplacedAt: t0.Add(-48 * time.Hour), FloorUntil: t0.Add(time.Hour)}}
	out := Decide(in)
	notices := noticesOfKind(out, NoticeEnrolmentContested)
	require.Len(t, notices, 1)
	assert.Equal(t, phisher, notices[0].Subject)
	assert.Empty(t, notices[0].RequestID)
	assert.Nil(t, notices[0].ReservedTo)
	for _, e := range effectsOfKind(out, EffectContest) {
		assert.Empty(t, e.RequestID, "ruling 11: a contest writes no request row")
	}
}

// T5 never touches a request or completes anything.
func TestContest_ChangesNoRequest(t *testing.T) {
	phisher := cref(CredWebAuthn, "phisher")
	creds := append(ownerCreds(), phisherCred(phisher, C2))
	in := inputAt(Contest{}, creds, fxTOTP)
	in.Reqs = []Req{{ID: "r1", Kind: ReqCredentialRemove, Status: StatusPending, OriginClass: C1,
		Targets: []CredRef{fxSSO}, OpenedAt: t0.Add(-time.Hour), CompletesAt: t0.Add(71 * time.Hour)}}
	out := Decide(in)
	require.Equal(t, VerdictApplied, out.Verdict)
	kinds := effectKindsSet(out)
	for _, k := range []EffectKind{EffectCancel, EffectDispute, EffectReturnToPending, EffectComplete, EffectOpen} {
		assert.False(t, kinds[k], "T5 must not emit effect kind %d", k)
	}
}

// 3-A: T1b contests only possession factors and anchors; with a possession factor in the
// proof, an immature address enrolled at C1 is left alone.
func TestT1bContestSet_IsPossessionAndAnchorsOnly(t *testing.T) {
	phisherWA := cref(CredWebAuthn, "phisher")
	phisherAddr := cref(CredBackupAddress, "phisher-addr")
	creds := append(ownerCreds(), phisherCred(phisherWA, C2), phisherCred(phisherAddr, C1))
	out := Decide(inputAt(SignOutEverywhere{}, creds, fxTOTP))
	require.Equal(t, RowT1b, out.Row)
	require.Equal(t, VerdictApplied, out.Verdict)
	assert.Equal(t, []CredRef{phisherWA}, contestedRefs(out))
	assert.True(t, out.RotateEpoch)
	assert.Len(t, noticesOfKind(out, NoticeSignedOutEverywhere), 1)

	// The same snapshot under Contest{} DOES include the address: /guard/contest covers it.
	out = Decide(inputAt(Contest{}, creds, fxTOTP))
	assert.ElementsMatch(t, []CredRef{phisherWA, phisherAddr}, contestedRefs(out))
}

// Architect ruling 2-Q6: a zero MaturesAt is a missing value, not an immature credential. T5 is
// admissible only against an immature enrolment, so a missing maturity must not admit the
// contest (that would kill a possibly mature factor at equal class). The awaiting-notice
// sentinel (2999) is non-zero and stays contestable. Covers T5 and T1b's 3-A set.
func TestContest_ZeroMaturesAtIsNotContestable(t *testing.T) {
	phisher := cref(CredWebAuthn, "phisher")
	zero := phisherCred(phisher, C2)
	zero.MaturesAt = time.Time{}
	awaiting := phisherCred(phisher, C2)
	awaiting.MaturesAt = awaitingInstant

	t.Run("T5", func(t *testing.T) {
		out := Decide(inputAt(Contest{}, append(ownerCreds(), zero), fxTOTP))
		assert.Equal(t, RowT5, out.Row)
		assert.Equal(t, VerdictRefused, out.Verdict)
		assert.Empty(t, contestedRefs(out))
		assert.Nil(t, out.Effects)
		assert.False(t, out.RotateEpoch)

		out = Decide(inputAt(Contest{}, append(ownerCreds(), awaiting), fxTOTP))
		require.Equal(t, VerdictApplied, out.Verdict, "positive control: the awaiting sentinel is contestable")
		assert.Equal(t, []CredRef{phisher}, contestedRefs(out))
	})
	t.Run("T1b", func(t *testing.T) {
		out := Decide(inputAt(SignOutEverywhere{}, append(ownerCreds(), zero), fxTOTP))
		require.Equal(t, RowT1b, out.Row)
		require.Equal(t, VerdictApplied, out.Verdict, "T1b itself still signs out everywhere")
		assert.Empty(t, contestedRefs(out), "a zero MaturesAt is not in the 3-A set")
		assert.Empty(t, noticesOfKind(out, NoticeEnrolmentContested))

		out = Decide(inputAt(SignOutEverywhere{}, append(ownerCreds(), awaiting), fxTOTP))
		assert.Equal(t, []CredRef{phisher}, contestedRefs(out), "positive control: the awaiting sentinel is contested")
	})
}
