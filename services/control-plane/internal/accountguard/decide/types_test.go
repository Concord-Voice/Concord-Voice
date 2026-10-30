package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plan Task 3: enums with text round-trip, and the snapshot, outcome and Action types.
//
// Text spellings are pinned only where the plan lists them (Class, CredKind, ReqKind,
// ReqStatus, CancelReason, Row, NoticeKind, and the zero Verdict "refused"). Verdict and
// EffectKind non-zero spellings are deliberately NOT pinned: only round-trip, uniqueness
// and the zero values the plan specifies.

type textCase[T any] struct {
	v    T
	text string
}

type marshalable interface {
	comparable
	MarshalText() ([]byte, error)
}

type unmarshalable[T any] interface {
	*T
	UnmarshalText([]byte) error
}

// checkTextTable asserts MarshalText(v) == text and UnmarshalText(text) == v for every case.
func checkTextTable[T marshalable, P unmarshalable[T]](t *testing.T, cases []textCase[T]) {
	t.Helper()
	for _, c := range cases {
		b, err := c.v.MarshalText()
		require.NoError(t, err, "marshal %q", c.text)
		assert.Equal(t, c.text, string(b))

		var got T
		require.NoError(t, P(&got).UnmarshalText([]byte(c.text)), "unmarshal %q", c.text)
		assert.Equal(t, c.v, got, "round trip of %q", c.text)
	}
}

// checkRoundTripUnique asserts every value marshals to a distinct non-empty text that
// unmarshals back to the same value, without pinning the spelling.
func checkRoundTripUnique[T marshalable, P unmarshalable[T]](t *testing.T, vals []T) {
	t.Helper()
	seen := map[string]T{}
	for _, v := range vals {
		b, err := v.MarshalText()
		require.NoError(t, err)
		text := string(b)
		require.NotEmpty(t, text)
		prev, dup := seen[text]
		require.False(t, dup, "text %q is shared by %v and %v", text, prev, v)
		seen[text] = v

		var got T
		require.NoError(t, P(&got).UnmarshalText(b))
		assert.Equal(t, v, got)
	}
}

func checkUnknownTextIsError[T any, P unmarshalable[T]](t *testing.T) {
	t.Helper()
	var v T
	assert.Error(t, P(&v).UnmarshalText([]byte("bogus")))
}

func TestEnums_TextRoundTrip(t *testing.T) {
	t.Run("Class", func(t *testing.T) {
		checkTextTable[Class](t, []textCase[Class]{
			{ClassNone, "none"}, {C1, "c1"}, {C2, "c2"}, {C3, "c3"},
		})
	})
	t.Run("CredKind", func(t *testing.T) {
		checkTextTable[CredKind](t, []textCase[CredKind]{
			{CredPassword, "password"},
			{CredAddress, "address"},
			{CredBackupAddress, "backup_address"},
			{CredSSO, "sso"},
			{CredEmailMFA, "email_mfa"},
			{CredTOTP, "totp"},
			{CredWebAuthn, "webauthn"},
			{CredBackupCodeSet, "backup_code_set"},
			{CredRecoveryKey, "recovery_key"},
			{CredRecoveryDevice, "recovery_device"},
		})
	})
	t.Run("ReqKind", func(t *testing.T) {
		checkTextTable[ReqKind](t, []textCase[ReqKind]{
			{ReqCredentialRemove, "credential_remove"},
			{ReqAddressChange, "address_change"},
			{ReqBreakGlass, "break_glass"},
			{ReqSocialRecovery, "social_recovery"},
			{ReqCircleChange, "circle_change"},
			{ReqRecoveryKey, "recovery_key"},
		})
	})
	t.Run("ReqStatus", func(t *testing.T) {
		checkTextTable[ReqStatus](t, []textCase[ReqStatus]{
			{StatusPending, "pending"},
			{StatusDisputed, "disputed"},
			{StatusCompleted, "completed"},
			{StatusCancelled, "cancelled"},
		})
	})
	t.Run("CancelReason", func(t *testing.T) {
		checkTextTable[CancelReason](t, []textCase[CancelReason]{
			{CancelNone, ""},
			{CancelProof, "proof"},
			{CancelDisputeResolved, "dispute_resolved"},
			{CancelSuperseded, "superseded"},
			{CancelHoldCap, "hold_cap"},
			{CancelDisputeExpired, "dispute_expired"},
		})
	})
	t.Run("Row", func(t *testing.T) {
		checkTextTable[Row](t, []textCase[Row]{
			{RowNone, "none"},
			{RowT1, "t1"}, {RowT1b, "t1b"}, {RowT2, "t2"}, {RowT2b, "t2b"}, {RowT3, "t3"},
			{RowT4, "t4"}, {RowT5, "t5"}, {RowT6, "t6"}, {RowT6c, "t6c"}, {RowT7, "t7"},
			{RowT8, "t8"}, {RowT9, "t9"}, {RowT10, "t10"}, {RowT11, "t11"}, {RowT12, "t12"},
			{RowSweep, "sweep"},
		})
	})
	t.Run("NoticeKind", func(t *testing.T) {
		// Spec §9 "SendSecurityNotice kinds (closed, C4 only)", in order.
		checkTextTable[NoticeKind](t, []textCase[NoticeKind]{
			{NoticeCredentialAdded, "credential_added"},
			{NoticeCredentialRemoved, "credential_removed"},
			{NoticeChangeRequested, "change_requested"},
			{NoticeChangeCancelled, "change_cancelled"},
			{NoticeChangeCompleted, "change_completed"},
			{NoticeChangeDisputed, "change_disputed"},
			{NoticeEnrolmentContested, "enrolment_contested"},
			{NoticeSignedOutEverywhere, "signed_out_everywhere"},
			{NoticePasswordChanged, "password_changed"},
		})
	})
}

// Verdict and EffectKind spellings are unpinned (plan lists only the zero Verdict).
func TestEnums_VerdictAndEffectKindRoundTripAndUniqueness(t *testing.T) {
	t.Run("Verdict", func(t *testing.T) {
		checkRoundTripUnique[Verdict](t, []Verdict{
			VerdictRefused, VerdictApplied, VerdictOpened, VerdictDisputed, VerdictHeld, VerdictNoChange,
		})
	})
	t.Run("EffectKind", func(t *testing.T) {
		checkRoundTripUnique[EffectKind](t, []EffectKind{
			EffectRemove, EffectContest, EffectOpen, EffectCancel, EffectDispute, EffectReturnToPending,
			EffectComplete, EffectRestorePrimary, EffectReleaseReservation, EffectHold, EffectResyncMFAFlags,
		})
	})
}

func TestEnums_UnknownTextIsError(t *testing.T) {
	t.Run("Class", checkUnknownTextIsError[Class])
	t.Run("CredKind", checkUnknownTextIsError[CredKind])
	t.Run("ReqKind", checkUnknownTextIsError[ReqKind])
	t.Run("ReqStatus", checkUnknownTextIsError[ReqStatus])
	t.Run("CancelReason", checkUnknownTextIsError[CancelReason])
	t.Run("Row", checkUnknownTextIsError[Row])
	t.Run("Verdict", checkUnknownTextIsError[Verdict])
	t.Run("EffectKind", checkUnknownTextIsError[EffectKind])
	t.Run("NoticeKind", checkUnknownTextIsError[NoticeKind])
}

// TestEnums_ZeroMarshalsAsUnknownOrNone: a zero CredKind, ReqKind, ReqStatus or EffectKind
// cannot be serialized as valid; zero Class and Row are "none"; zero Verdict is "refused"
// (fail-closed zero). Plan Task 3.
func TestEnums_ZeroMarshalsAsUnknownOrNone(t *testing.T) {
	_, err := CredKind(0).MarshalText()
	assert.Error(t, err, "CredUnknown")
	_, err = ReqKind(0).MarshalText()
	assert.Error(t, err, "ReqUnknown")
	_, err = ReqStatus(0).MarshalText()
	assert.Error(t, err, "StatusUnknown")
	_, err = EffectKind(0).MarshalText()
	assert.Error(t, err, "EffectUnknown")

	b, err := Class(0).MarshalText()
	require.NoError(t, err)
	assert.Equal(t, "none", string(b))
	b, err = Row(0).MarshalText()
	require.NoError(t, err)
	assert.Equal(t, "none", string(b))
	b, err = Verdict(0).MarshalText()
	require.NoError(t, err)
	assert.Equal(t, "refused", string(b))
}

// TestEnums_ZeroValuesAreTheNamedZeros pins the zero constants the plan declares.
func TestEnums_ZeroValuesAreTheNamedZeros(t *testing.T) {
	assert.Equal(t, Class(0), ClassNone)
	assert.Equal(t, Rank(0), RankInvalid)
	assert.Equal(t, CredKind(0), CredUnknown)
	assert.Equal(t, ReqKind(0), ReqUnknown)
	assert.Equal(t, ReqStatus(0), StatusUnknown)
	assert.Equal(t, CancelReason(0), CancelNone)
	assert.Equal(t, Row(0), RowNone)
	assert.Equal(t, Verdict(0), VerdictRefused)
	assert.Equal(t, EffectKind(0), EffectUnknown)
	assert.Equal(t, NoticeKind(0), NoticeUnknown)
}

// TestEnums_ClassAndRankNumbering: "ClassNone=0, C1=1, C2=2, C3=3" and "RankInvalid=0,
// Rank1=1, ...". T2's `c > rank(target)` and T5's `class whose number equals Rank` rely on it.
func TestEnums_ClassAndRankNumbering(t *testing.T) {
	assert.Equal(t, uint8(1), uint8(C1))
	assert.Equal(t, uint8(2), uint8(C2))
	assert.Equal(t, uint8(3), uint8(C3))
	assert.Equal(t, uint8(1), uint8(Rank1))
	assert.Equal(t, uint8(2), uint8(Rank2))
	assert.Equal(t, uint8(3), uint8(Rank3))
	assert.Less(t, C1, C2)
	assert.Less(t, C2, C3)
}

// TestEnums_NoticeKindNumbering (architect ruling 14): values 1-9 in child §9 order, NoticeUnknown = 0.
func TestEnums_NoticeKindNumbering(t *testing.T) {
	in := []NoticeKind{
		NoticeCredentialAdded, NoticeCredentialRemoved, NoticeChangeRequested, NoticeChangeCancelled,
		NoticeChangeCompleted, NoticeChangeDisputed, NoticeEnrolmentContested, NoticeSignedOutEverywhere,
		NoticePasswordChanged,
	}
	for i, k := range in {
		assert.Equal(t, uint8(i+1), uint8(k))
	}
	assert.Equal(t, uint8(0), uint8(NoticeUnknown))
}

// TestSource_ExactlySixValues pins the six sources (plan Task 3; DoR D-250).
func TestSource_ExactlySixValues(t *testing.T) {
	got := []Source{SourceUserSession, SourceLockdown, SourceBreakGlass, SourceRecoveryKey, SourceOperator, SourceUnattributed}
	want := []Source{"user_session", "lockdown", "break_glass", "recovery_key", "operator", "unattributed"}
	assert.Equal(t, want, got)
}

// TestActions_ElevenVariantsImplementAction: the Action sum is closed over these eleven types
// (spec §7 "Action, a closed sum"), all with value receivers.
func TestActions_ElevenVariantsImplementAction(t *testing.T) {
	acts := []Action{
		SignOutOthers{}, SignOutEverywhere{}, Remove{}, SelfRemoveBackup{}, Enrol{}, Contest{},
		OpenBreakGlass{}, Cancel{}, Complete{}, Resolve{}, Expire{},
	}
	assert.Len(t, acts, 11)
}

// TestTypes_FieldsMatchTheInterfacesBlock builds each struct with every field named in the
// plan's Interfaces block, so a missing or renamed field fails compilation or this test.
func TestTypes_FieldsMatchTheInterfacesBlock(t *testing.T) {
	ref := CredRef{Kind: CredTOTP, ID: "x", Version: 3}
	when := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	c := Cred{Ref: ref, MaturesAt: when, ContestedAt: when, Paper: true, EnrolledAt: when, EnrolClass: C2}
	assert.Equal(t, ref, c.Ref)
	assert.Equal(t, C2, c.EnrolClass)

	cand := Candidate{Keyed: []byte("k"), Coarse: []byte("c"), KeyID: 7}
	r := Req{
		ID: "r", Kind: ReqBreakGlass, Status: StatusPending, OriginClass: C1,
		Origins: []CredRef{ref}, Targets: []CredRef{ref},
		OpenedAt: when, CompletesAt: when, HeldMs: 5, HeldSince: when,
		DisputedAt: when, DisputeGroup: "g",
		Fingerprint: []byte("f"), Coarse: []byte("c"), FingerprintKeyID: 2,
		CancelReason: CancelProof, CancellerClass: C3, CancelledAt: when,
		AcceptedAddressNotice: true, MatureNoticesSettled: true, ResetFactors: true,
	}
	assert.Equal(t, "g", r.DisputeGroup)

	p := Primary{Ref: ref, BecamePrimaryAt: when, MaturesAt: when, ContestedAt: when, DisplacedAt: when}
	res := Reservation{AddressRef: ref, DisplacedAt: when, FloorUntil: when}
	in := Input{
		Now: when, Act: Remove{Target: ref, Candidate: cand}, Proof: Proof{Used: []CredRef{ref}, DeviceBound: []CredRef{ref}},
		Creds: []Cred{c}, Primaries: []Primary{p}, Reservations: []Reservation{res},
		Reqs: []Req{r}, RecentCancels: []Req{r},
		GuessableBudgetClear: true, CurrentKeyID: 2, SSOLogin: true,
	}
	assert.True(t, in.SSOLogin)
	assert.True(t, in.GuessableBudgetClear)

	op := Opening{
		Kind: ReqBreakGlass, Status: StatusCompleted, Targets: []CredRef{ref}, Origins: []CredRef{ref},
		OriginClass: C3, ResetFactors: true,
		OpenedAt: when, CompletesAt: when, DisputedAt: when, CompletedAt: when, Candidate: cand,
	}
	e := Effect{
		Kind: EffectOpen, RequestID: "r", Cred: ref, Open: op, Reason: CancelProof, CancellerClass: C1,
		Group: "g", HeldMs: 1, HeldSince: when,
	}
	n := Notice{Kind: NoticeChangeRequested, RequestID: "r", Subject: ref, ReservedTo: []CredRef{ref}}
	o := Outcome{
		Row: RowT6c, Verdict: VerdictApplied, Effects: []Effect{e}, Source: SourceBreakGlass,
		RotateEpoch: true, SetsPassword: true, Continuation: true, Notices: []Notice{n}, PermGenBump: true, EnrolClass: C2,
	}
	assert.Equal(t, RowT6c, o.Row)

	// Actions carry the fields the plan lists.
	_ = OpenBreakGlass{Reset: true, Candidate: cand}
	_ = Cancel{IDs: []string{"a"}}
	_ = Complete{ID: "a"}
	_ = Resolve{Group: "g", Chosen: "r"}
	_ = SelfRemoveBackup{Target: ref}
	_ = Enrol{Ref: ref}
}
