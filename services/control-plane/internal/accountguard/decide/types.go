package decide

import (
	"errors"
	"time"
)

// Enumerations. Every enum except Source is a uint8 with a text form (MarshalText and
// UnmarshalText); the text is the database and wire spelling PR3 maps. Unknown text is an
// error. The zero value of an enum whose zero has no meaning (CredKind, ReqKind, ReqStatus,
// EffectKind, NoticeKind, Rank) has no text either, so a zero value cannot be serialized as
// a valid one.

// Class is the strength of a proof set (parent §6.1). The zero value is ClassNone, so an
// unset class satisfies no threshold.
type Class uint8

// The proof classes. Their numbers equal the ranks they outrank (T2: c > rank(X)).
const (
	ClassNone Class = iota
	C1
	C2
	C3
)

func classNames() []string { return []string{"none", "c1", "c2", "c3"} }

// MarshalText returns the class's spelling ("none", "c1", "c2" or "c3").
func (c Class) MarshalText() ([]byte, error) {
	return enumText(c, classNames(), ClassNone, "class")
}

// UnmarshalText parses a class spelling.
func (c *Class) UnmarshalText(text []byte) error {
	return enumParse(c, text, classNames(), ClassNone, "class")
}

// Rank is a credential kind's rank as a target (parent §6.1): K is 1, P is 2, A is 3.
type Rank uint8

// The ranks. RankInvalid is the rank of an unknown kind.
const (
	RankInvalid Rank = iota
	Rank1
	Rank2
	Rank3
)

func rankNames() []string { return []string{"", "rank1", "rank2", "rank3"} }

// MarshalText returns the rank's spelling ("rank1", "rank2" or "rank3").
func (r Rank) MarshalText() ([]byte, error) { return enumText(r, rankNames(), Rank1, "rank") }

// UnmarshalText parses a rank spelling.
func (r *Rank) UnmarshalText(text []byte) error {
	return enumParse(r, text, rankNames(), Rank1, "rank")
}

// CredKind is the kind of a credential (parent §6.1). CredAddress is the primary address;
// a backup address is CredBackupAddress.
type CredKind uint8

// The credential kinds.
const (
	CredUnknown CredKind = iota
	CredPassword
	CredAddress
	CredBackupAddress
	CredSSO
	CredEmailMFA
	CredTOTP
	CredWebAuthn
	CredBackupCodeSet
	CredRecoveryKey
	CredRecoveryDevice
)

func credKindNames() []string {
	return []string{
		"", "password", "address", "backup_address", "sso", "email_mfa", "totp", // pragma: allowlist secret
		"webauthn", "backup_code_set", "recovery_key", "recovery_device",
	}
}

// MarshalText returns the kind's spelling.
func (k CredKind) MarshalText() ([]byte, error) {
	return enumText(k, credKindNames(), CredPassword, "credential kind")
}

// UnmarshalText parses a kind spelling.
func (k *CredKind) UnmarshalText(text []byte) error {
	return enumParse(k, text, credKindNames(), CredPassword, "credential kind")
}

// ReqKind is the kind of a pending request. It holds all six parent kinds so that the
// target-fence rule of every kind is pinned now (mutation #39), although C4's database
// admits only the first three.
type ReqKind uint8

// The request kinds.
const (
	ReqUnknown ReqKind = iota
	ReqCredentialRemove
	ReqAddressChange
	ReqBreakGlass
	ReqSocialRecovery
	ReqCircleChange
	ReqRecoveryKey
)

func reqKindNames() []string {
	return []string{"", "credential_remove", "address_change", "break_glass", "social_recovery", "circle_change", "recovery_key"}
}

// MarshalText returns the kind's spelling.
func (k ReqKind) MarshalText() ([]byte, error) {
	return enumText(k, reqKindNames(), ReqCredentialRemove, "request kind")
}

// UnmarshalText parses a kind spelling.
func (k *ReqKind) UnmarshalText(text []byte) error {
	return enumParse(k, text, reqKindNames(), ReqCredentialRemove, "request kind")
}

// ReqStatus is a request's state (parent §6.3).
type ReqStatus uint8

// The request states.
const (
	StatusUnknown ReqStatus = iota
	StatusPending
	StatusDisputed
	StatusCompleted
	StatusCancelled
)

func reqStatusNames() []string { return []string{"", "pending", "disputed", "completed", "cancelled"} }

// MarshalText returns the status's spelling.
func (s ReqStatus) MarshalText() ([]byte, error) {
	return enumText(s, reqStatusNames(), StatusPending, "request status")
}

// UnmarshalText parses a status spelling.
func (s *ReqStatus) UnmarshalText(text []byte) error {
	return enumParse(s, text, reqStatusNames(), StatusPending, "request status")
}

// CancelReason is why a request was cancelled. CancelNone, spelled "", is a request that
// was not cancelled.
type CancelReason uint8

// The cancel reasons (child spec §6.2).
const (
	CancelNone CancelReason = iota
	CancelProof
	CancelDisputeResolved
	CancelSuperseded
	CancelHoldCap
	CancelDisputeExpired
)

func cancelReasonNames() []string {
	return []string{"", "proof", "dispute_resolved", "superseded", "hold_cap", "dispute_expired"}
}

// MarshalText returns the reason's spelling.
func (r CancelReason) MarshalText() ([]byte, error) {
	return enumText(r, cancelReasonNames(), CancelNone, "cancel reason")
}

// UnmarshalText parses a reason spelling.
func (r *CancelReason) UnmarshalText(text []byte) error {
	return enumParse(r, text, cancelReasonNames(), CancelNone, "cancel reason")
}

// Source is the auth-change source an outcome is written under (DoR D-250). In C4, Decide
// emits only SourceUserSession and SourceBreakGlass.
type Source string

// The six sources.
const (
	SourceUserSession  Source = "user_session"
	SourceLockdown     Source = "lockdown"
	SourceBreakGlass   Source = "break_glass"
	SourceRecoveryKey  Source = "recovery_key"
	SourceOperator     Source = "operator"
	SourceUnattributed Source = "unattributed"
)

// Row is the T-table row an outcome applies (parent §6.2). RowSweep is the sweeper's Expire.
// RowNone means the table has no row for the input (architect ruling 8).
type Row uint8

// The rows.
const (
	RowNone Row = iota
	RowT1
	RowT1b
	RowT2
	RowT2b
	RowT3
	RowT4
	RowT5
	RowT6
	RowT6c
	RowT7
	RowT8
	RowT9
	RowT10
	RowT11
	RowT12
	RowSweep
)

func rowNames() []string {
	return []string{"none", "t1", "t1b", "t2", "t2b", "t3", "t4", "t5", "t6", "t6c", "t7", "t8", "t9", "t10", "t11", "t12", "sweep"}
}

// MarshalText returns the row's spelling ("none", "t1" ... "t12", "sweep").
func (r Row) MarshalText() ([]byte, error) { return enumText(r, rowNames(), RowNone, "row") }

// UnmarshalText parses a row spelling.
func (r *Row) UnmarshalText(text []byte) error {
	return enumParse(r, text, rowNames(), RowNone, "row")
}

// Verdict is what an outcome did. The zero value is VerdictRefused, so an unset verdict
// fails closed.
type Verdict uint8

// The verdicts.
const (
	VerdictRefused Verdict = iota
	VerdictApplied
	VerdictOpened
	VerdictDisputed
	VerdictHeld
	VerdictNoChange
)

func verdictNames() []string {
	return []string{"refused", "applied", "opened", "disputed", "held", "no_change"}
}

// MarshalText returns the verdict's spelling.
func (v Verdict) MarshalText() ([]byte, error) {
	return enumText(v, verdictNames(), VerdictRefused, "verdict")
}

// UnmarshalText parses a verdict spelling.
func (v *Verdict) UnmarshalText(text []byte) error {
	return enumParse(v, text, verdictNames(), VerdictRefused, "verdict")
}

// EffectKind is the kind of one write the runner applies (child spec §7, plus plan D4's
// EffectHold and EffectReturnToPending).
type EffectKind uint8

// The effect kinds.
const (
	EffectUnknown EffectKind = iota
	EffectRemove
	EffectContest
	EffectOpen
	EffectCancel
	EffectDispute
	EffectReturnToPending
	EffectComplete
	EffectRestorePrimary
	EffectReleaseReservation
	EffectHold
	EffectResyncMFAFlags
)

func effectKindNames() []string {
	return []string{
		"", "remove", "contest", "open", "cancel", "dispute", "return_to_pending", "complete",
		"restore_primary", "release_reservation", "hold", "resync_mfa_flags",
	}
}

// MarshalText returns the effect kind's spelling.
func (k EffectKind) MarshalText() ([]byte, error) {
	return enumText(k, effectKindNames(), EffectRemove, "effect kind")
}

// UnmarshalText parses an effect kind spelling.
func (k *EffectKind) UnmarshalText(text []byte) error {
	return enumParse(k, text, effectKindNames(), EffectRemove, "effect kind")
}

// NoticeKind is a SendSecurityNotice kind (child spec §9), numbered 1-9 in that order
// (architect ruling 14).
type NoticeKind uint8

// The notice kinds.
const (
	NoticeUnknown NoticeKind = iota
	NoticeCredentialAdded
	NoticeCredentialRemoved
	NoticeChangeRequested
	NoticeChangeCancelled
	NoticeChangeCompleted
	NoticeChangeDisputed
	NoticeEnrolmentContested
	NoticeSignedOutEverywhere
	NoticePasswordChanged
)

func noticeKindNames() []string {
	return []string{
		"", "credential_added", "credential_removed", "change_requested", "change_cancelled",
		"change_completed", "change_disputed", "enrolment_contested", "signed_out_everywhere",
		"password_changed", // pragma: allowlist secret
	}
}

// MarshalText returns the notice kind's spelling.
func (k NoticeKind) MarshalText() ([]byte, error) {
	return enumText(k, noticeKindNames(), NoticeCredentialAdded, "notice kind")
}

// UnmarshalText parses a notice kind spelling.
func (k *NoticeKind) UnmarshalText(text []byte) error {
	return enumParse(k, text, noticeKindNames(), NoticeCredentialAdded, "notice kind")
}

// enumText returns names[v]. A value below first (the meaningless zero of most enums) or
// past the table has no text.
func enumText[E ~uint8](v E, names []string, first E, what string) ([]byte, error) {
	if v < first || int(v) >= len(names) {
		return nil, errors.New("decide: " + what + " value has no text")
	}
	return []byte(names[v]), nil
}

// enumParse sets *dst to the value from first onward whose text is text.
func enumParse[E ~uint8](dst *E, text []byte, names []string, first E, what string) error {
	for v := first; int(v) < len(names); v++ {
		if names[v] == string(text) {
			*dst = v
			return nil
		}
	}
	return errors.New("decide: unknown " + what + " text")
}

// Snapshot types. Decide reads them and never writes through them.

// CredRef names one credential at one material version. ID is opaque: canonical UUID text,
// or a runner key for a credential with no row of its own. Set membership, origin sets and
// proof matching compare all three fields; target-fence, reservation and reset lookups
// compare the (Kind, ID) slot only (plan D6).
type CredRef struct {
	Kind    CredKind
	ID      string
	Version int64
}

// Cred is one live credential (architect ruling 7: verified addresses, confirmed TOTP,
// registered WebAuthn, and the rest of the account's live credentials, as they stood before
// the action).
type Cred struct {
	Ref CredRef
	// MaturesAt is when the credential starts to count toward a class. A zero value counts
	// for nothing.
	MaturesAt time.Time
	// ContestedAt is zero when the credential is uncontested. A contested credential never
	// counts and never signs in.
	ContestedAt time.Time
	// Paper marks a credential that proves like a backup code.
	Paper bool
	// EnrolledAt is the 000189 enrolled_at (legacy 1970-01-01Z). A zero value makes the
	// credential uncontestable (plan D20).
	EnrolledAt time.Time
	// EnrolClass is the 000189 enrol_class; NULL (unknown) is ClassNone (plan D20).
	EnrolClass Class
}

// Proof is the set of credentials the actor proved, each already verified by the runner. A
// WebAuthn ref is present only when this assertion carried user verification.
type Proof struct {
	Used []CredRef
	// DeviceBound lists the WebAuthn refs of Used whose THIS assertion's authenticator data
	// carried BE=0 (red-team F1). It is read from the assertion and from nowhere else, so its
	// zero value proves nothing: a WebAuthn absent from it is a synced passkey and never pairs
	// with an anchor into C3. A ref absent from Used, or of another kind, is ignored.
	DeviceBound []CredRef
}

// Candidate is the opening action's request fingerprint: the keyed HMAC under key KeyID and
// the unkeyed coarse fingerprint (child spec §6.2, LEAD-D 11-A).
type Candidate struct {
	Keyed  []byte
	Coarse []byte
	KeyID  int16
}

// Req is one row of account_pending_requests: an open or disputed request (Input.Reqs) or a
// recent proof or dispute_resolved cancel (Input.RecentCancels).
type Req struct {
	ID          string
	Kind        ReqKind
	Status      ReqStatus
	OriginClass Class
	Origins     []CredRef
	// Targets carries each target's recorded target_material_version in its Version (plan
	// D1). Decide never reads it on a break-glass row (architect ruling 4).
	Targets     []CredRef
	OpenedAt    time.Time
	CompletesAt time.Time
	// HeldMs is the settled held time in milliseconds; HeldSince is set while a hold is
	// open. An open hold counts as HeldMs + (Now - HeldSince).
	HeldMs           int64
	HeldSince        time.Time
	DisputedAt       time.Time
	DisputeGroup     string
	Fingerprint      []byte
	Coarse           []byte
	FingerprintKeyID int16
	CancelReason     CancelReason
	CancellerClass   Class
	CancelledAt      time.Time
	// AcceptedAddressNotice is true once a provider accepted one of the request's address
	// notices. MatureNoticesSettled is true only when every notice to a mature address is
	// accepted or has failed permanently; its zero value holds the request (parent §6.2 Holds
	// (a); H1: a missed load fails closed).
	AcceptedAddressNotice bool
	MatureNoticesSettled  bool
	ResetFactors          bool
}

// Primary is one row of the account's primary-address chain: the current primary has a zero
// DisplacedAt, and each earlier one records when it was displaced (child spec §4 input 1).
type Primary struct {
	Ref             CredRef
	BecamePrimaryAt time.Time
	MaturesAt       time.Time
	ContestedAt     time.Time
	DisplacedAt     time.Time
}

// Reservation keeps a displaced primary reserved to the account (child spec §6.2).
type Reservation struct {
	AddressRef  CredRef
	DisplacedAt time.Time
	FloorUntil  time.Time
}

// Action is the closed sum of what Decide decides. Only this package's types implement it,
// and Decide accepts them by value: any other type, a pointer to an action included, is
// refused.
type Action interface{ isAction() }

// SignOutOthers signs out every other session and rotates the password (T1).
type SignOutOthers struct{}

// SignOutEverywhere signs out every session, sessionless (T1b).
type SignOutEverywhere struct{}

// Remove removes or replaces a mature credential (T2 or T3).
type Remove struct {
	Target    CredRef
	Candidate Candidate
}

// SelfRemoveBackup is a backup address removing itself with a code sent to it (T2b).
type SelfRemoveBackup struct {
	Target CredRef
}

// Enrol enrols a new credential (T4). Ref is absent from Input.Creds (architect ruling 7).
type Enrol struct {
	Ref CredRef
}

// Contest contests the immature enrolments the proof outranks (T5).
type Contest struct{}

// OpenBreakGlass opens a break-glass (T6), or completes it at once at C3 (T6c). Reset also
// resets every possession factor outside the proof.
type OpenBreakGlass struct {
	Reset     bool
	Candidate Candidate
}

// Cancel cancels each named request the proof may cancel (T7).
type Cancel struct {
	IDs []string
}

// Complete is the sweeper's completion check of one request (T11).
type Complete struct {
	ID string
}

// Resolve resolves a dispute group (T12). Chosen is the member returned to pending, or ""
// to cancel every member.
type Resolve struct {
	Group  string
	Chosen string
}

// Expire is the sweeper's pass over dispute expiry, reservation release and contested
// credentials past Δ_cp.
type Expire struct{}

func (SignOutOthers) isAction() {
	// Marker method: seals Action.
}

func (SignOutEverywhere) isAction() {
	// Marker method: seals Action.
}

func (Remove) isAction() {
	// Marker method: seals Action.
}

func (SelfRemoveBackup) isAction() {
	// Marker method: seals Action.
}

func (Enrol) isAction() {
	// Marker method: seals Action.
}

func (Contest) isAction() {
	// Marker method: seals Action.
}

func (OpenBreakGlass) isAction() {
	// Marker method: seals Action.
}

func (Cancel) isAction() {
	// Marker method: seals Action.
}

func (Complete) isAction() {
	// Marker method: seals Action.
}

func (Resolve) isAction() {
	// Marker method: seals Action.
}

func (Expire) isAction() {
	// Marker method: seals Action.
}

// Input is the snapshot Decide decides over. The runner loads it under its locks.
type Input struct {
	// Now is the database clock, and Decide's only clock.
	Now   time.Time
	Act   Action
	Proof Proof
	// Creds is the pre-action snapshot of the account's live credentials (architect
	// ruling 7).
	Creds        []Cred
	Primaries    []Primary
	Reservations []Reservation
	// Reqs holds the account's pending and disputed requests; RecentCancels its requests
	// cancelled by proof or by T12 within NCancelCP.
	Reqs          []Req
	RecentCancels []Req
	// GuessableBudgetClear is true only when the runner read the account's guessable-proof
	// cancel budgets and none is locked. Its zero value evaluates hold (b) (H1: a missed load
	// fails closed).
	GuessableBudgetClear bool
	// CurrentKeyID is the key id the runner's fingerprint HMAC uses now.
	CurrentKeyID int16
	// SSOLogin reports whether the account signs in with SSO rather than a password (plan D2).
	// Its zero value is a password account, so a missed load never lets an SSO assertion meet
	// a password account's login floor (H1; parent §4 IdP row).
	SSOLogin bool
}

// Opening is the request an EffectOpen inserts.
type Opening struct {
	Kind   ReqKind
	Status ReqStatus
	// Targets is nil for a break-glass (architect ruling 4). Targets and Origins are in
	// canonical order: Kind, then ID byte-wise, then Version, with no duplicate (ruling 6).
	Targets      []CredRef
	Origins      []CredRef
	OriginClass  Class
	ResetFactors bool
	OpenedAt     time.Time
	CompletesAt  time.Time
	DisputedAt   time.Time
	CompletedAt  time.Time
	// Candidate is the opening action's fingerprint; zero on a completed (T6c) row.
	Candidate Candidate
}

// Effect is one write the runner applies. RequestID names the account_pending_requests row
// the effect writes, and is "" on every effect that writes no request row and on EffectOpen
// (the runner assigns the new ID). Group "" means one new dispute group shared by every
// effect of the outcome (plan D12).
type Effect struct {
	Kind           EffectKind
	RequestID      string
	Cred           CredRef
	Open           Opening
	Reason         CancelReason
	CancellerClass Class
	Group          string
	HeldMs         int64
	HeldSince      time.Time
}

// Notice is one security notice. RequestID is the request the notice belongs to; inside an
// outcome that opens a request, "" names that request, and anywhere else "" means none
// (plan D12). ReservedTo lists the reserved primaries that also receive a request's notices.
type Notice struct {
	Kind       NoticeKind
	RequestID  string
	Subject    CredRef
	ReservedTo []CredRef
}

// Outcome is Decide's result. Effects and Notices are sorted canonically and nil when empty.
type Outcome struct {
	Row     Row
	Verdict Verdict
	Effects []Effect
	Source  Source
	// RotateEpoch rotates the credential epoch and revokes every session; SetsPassword
	// writes the request's pending password; Continuation mints the actor a continuation
	// session.
	RotateEpoch  bool
	SetsPassword bool
	Continuation bool
	Notices      []Notice
	// PermGenBump is true exactly when the effects contain a contest or a remove.
	PermGenBump bool
	// EnrolClass is the class the runner writes, with enrolled_at, on the credential the
	// outcome writes (red-team F2): ClassOf the proof for T1's password and T4's credential,
	// C3 for T6c's password, and the origin class for a T11 completion of a break-glass (its
	// password) or an address_change (its address). It is ClassNone on every other outcome,
	// because a removal, a cancel, a hold or a refusal writes no credential.
	EnrolClass Class
}
