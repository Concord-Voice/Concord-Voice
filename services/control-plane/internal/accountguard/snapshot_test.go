package accountguard

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// ---------------------------------------------------------------------------
// Fixtures
//
// Every test here loads real rows from the scratch database through loadInputTx. The
// fixed clock sits months before the real one on purpose: a loader that read the database
// clock instead of its now argument would see every fixture cancel as far outside the
// window.
// ---------------------------------------------------------------------------

var (
	snapNow    = time.Date(2026, 3, 15, 8, 30, 0, 0, time.UTC)
	snapLegacy = time.Unix(0, 0).UTC() // the 000189 legacy sentinel, 1970-01-01Z
)

// snapKeyID is the key id handed to the loader. It is neither zero nor a value any other
// fixture uses, so a loader that fills CurrentKeyID from anywhere else is caught.
const snapKeyID int16 = 7

// snapStubComment is the comment the brief requires beside the PR3 stubs.
const snapStubComment = "// PR4 builds the budget and the outbox; until then a visible stub, not a silent zero (decide/doc.go H1)."

// snapLockNotAvailable is the SQLSTATE a NOWAIT row lock reports when another
// transaction holds a conflicting lock.
const snapLockNotAvailable = "55P03"

func snapDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	return db
}

type snapTextMarshaler interface{ MarshalText() ([]byte, error) }

// snapSpell returns the database spelling of a decide enum.
func snapSpell(t *testing.T, m snapTextMarshaler) string {
	t.Helper()
	b, err := m.MarshalText()
	require.NoError(t, err)
	return string(b)
}

func snapTimeArg(v time.Time) any {
	if v.IsZero() {
		return nil
	}
	return v
}

func snapBytesArg(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func snapClassArg(c decide.Class) any {
	if c == decide.ClassNone {
		return nil
	}
	return int(c)
}

func snapUUIDArg(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// snapMaturity is the provenance every credential row carries. A zero contestedAt is
// stored as NULL, and a ClassNone enrolClass is stored as NULL (the schema admits only
// 1 to 3).
type snapMaturity struct {
	maturesAt   time.Time
	contestedAt time.Time
	enrolledAt  time.Time
	enrolClass  decide.Class
}

func snapLegacyMaturity() snapMaturity {
	return snapMaturity{maturesAt: snapLegacy, enrolledAt: snapLegacy}
}

// cred is the decide.Cred the loader must produce for a row with this provenance.
func (m snapMaturity) cred(ref decide.CredRef, paper bool) decide.Cred {
	return decide.Cred{
		Ref:         ref,
		MaturesAt:   m.maturesAt,
		ContestedAt: m.contestedAt,
		Paper:       paper,
		EnrolledAt:  m.enrolledAt,
		EnrolClass:  m.enrolClass,
	}
}

// snapUser is one users row. insert fills pwRef and addrRef from the versions the
// 000189 trigger drew.
type snapUser struct {
	id            uuid.UUID
	email         string
	verified      bool
	passwordHash  string
	loginDisabled bool
	trustSSO      bool
	password      snapMaturity
	address       snapMaturity

	pwRef   decide.CredRef
	addrRef decide.CredRef
}

// snapAddressID is the CredRef ID the loader gives an address credential: lower(btrim(email)),
// the same identity a reservation and a displaced primary carry (ruling 9). It normalizes on
// purpose, so a fixture with a padded or mixed-case email states the expected ID honestly.
// TestLoadInput_AddressIDIsTheNormalizedEmail also pins one literal.
func snapAddressID(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func newSnapUser() snapUser {
	id := uuid.New()
	return snapUser{
		id:           id,
		email:        "snap-" + id.String()[:8] + "@example.test",
		verified:     true,
		passwordHash: "x",
		password:     snapLegacyMaturity(),
		address:      snapLegacyMaturity(),
	}
}

func (u *snapUser) insert(t *testing.T, db *sql.DB) {
	t.Helper()
	var pwVersion, emVersion int64
	err := db.QueryRow(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified,
		     password_login_disabled, trust_sso_security,
		     password_matures_at, password_enrolled_at, password_enrol_class,
		     email_matures_at, email_contested_at, email_enrolled_at, email_enrol_class)
		 VALUES ($1, $2, $3, $4, true, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 RETURNING password_version, email_version`,
		u.id, u.email, "snap"+u.id.String()[:8], u.passwordHash, u.verified, u.loginDisabled, u.trustSSO,
		u.password.maturesAt, u.password.enrolledAt, snapClassArg(u.password.enrolClass),
		u.address.maturesAt, snapTimeArg(u.address.contestedAt), u.address.enrolledAt, snapClassArg(u.address.enrolClass),
	).Scan(&pwVersion, &emVersion)
	require.NoError(t, err)
	u.pwRef = decide.CredRef{Kind: decide.CredPassword, ID: u.id.String(), Version: pwVersion}
	u.addrRef = decide.CredRef{Kind: decide.CredAddress, ID: snapAddressID(u.email), Version: emVersion}
}

// primary is the one current primary the loader must produce for a verified address.
func (u snapUser) primary() decide.Primary {
	return decide.Primary{
		Ref:             u.addrRef,
		BecamePrimaryAt: u.address.enrolledAt,
		MaturesAt:       u.address.maturesAt,
		ContestedAt:     u.address.contestedAt,
	}
}

// baseInput is the Input the loader must produce for this user alone: the password (when
// the hash is non-empty), the address and its primary (when verified), and the stubs.
func (u snapUser) baseInput() decide.Input {
	in := decide.Input{Now: snapNow, GuessableBudgetClear: true, CurrentKeyID: snapKeyID}
	if u.passwordHash != "" {
		in.Creds = append(in.Creds, u.password.cred(u.pwRef, false))
	}
	if u.verified {
		in.Creds = append(in.Creds, u.address.cred(u.addrRef, false))
		in.Primaries = []decide.Primary{u.primary()}
	}
	return in
}

func snapAddTOTP(t *testing.T, db *sql.DB, userID uuid.UUID, enabled, confirmed bool, m snapMaturity) decide.CredRef {
	t.Helper()
	var version int64
	err := db.QueryRow(
		`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed,
		     matures_at, maturity_contested_at, enrolled_at, enrol_class)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING material_version`,
		userID, []byte("enc"), []byte("nonce"), enabled, confirmed,
		m.maturesAt, snapTimeArg(m.contestedAt), m.enrolledAt, snapClassArg(m.enrolClass),
	).Scan(&version)
	require.NoError(t, err)
	return decide.CredRef{Kind: decide.CredTOTP, ID: userID.String(), Version: version}
}

func snapAddWebAuthn(t *testing.T, db *sql.DB, userID uuid.UUID, m snapMaturity) decide.CredRef {
	t.Helper()
	id := uuid.New()
	var version int64
	err := db.QueryRow(
		`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, public_key,
		     matures_at, maturity_contested_at, enrolled_at, enrol_class)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING material_version`,
		id, userID, id[:], []byte("pub"),
		m.maturesAt, snapTimeArg(m.contestedAt), m.enrolledAt, snapClassArg(m.enrolClass),
	).Scan(&version)
	require.NoError(t, err)
	return decide.CredRef{Kind: decide.CredWebAuthn, ID: id.String(), Version: version}
}

func snapAddSSO(t *testing.T, db *sql.DB, userID uuid.UUID, m snapMaturity) decide.CredRef {
	t.Helper()
	id := uuid.New()
	var version int64
	err := db.QueryRow(
		`INSERT INTO user_sso_identities (id, user_id, provider, provider_user_id, provider_email,
		     matures_at, maturity_contested_at, enrolled_at, enrol_class)
		 VALUES ($1, $2, 'google', $3, 'idp@example.test', $4, $5, $6, $7)
		 RETURNING material_version`,
		id, userID, "sub-"+id.String(),
		m.maturesAt, snapTimeArg(m.contestedAt), m.enrolledAt, snapClassArg(m.enrolClass),
	).Scan(&version)
	require.NoError(t, err)
	return decide.CredRef{Kind: decide.CredSSO, ID: id.String(), Version: version}
}

func snapAddRecovery(t *testing.T, db *sql.DB, userID uuid.UUID, m snapMaturity) decide.CredRef {
	t.Helper()
	var version int64
	err := db.QueryRow(
		`INSERT INTO user_recovery_keys (user_id, recovery_wrapped_private_key, recovery_key_salt,
		     matures_at, maturity_contested_at, enrolled_at, enrol_class)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING material_version`,
		userID, []byte("wrapped"), []byte("salt"),
		m.maturesAt, snapTimeArg(m.contestedAt), m.enrolledAt, snapClassArg(m.enrolClass),
	).Scan(&version)
	require.NoError(t, err)
	return decide.CredRef{Kind: decide.CredRecoveryKey, ID: userID.String(), Version: version}
}

// snapAccount is a user with one credential of every loadable kind.
type snapAccount struct {
	user                snapUser
	totp, web, sso, rec decide.CredRef
	totpM, webM, ssoM   snapMaturity
	recM                snapMaturity
}

// snapBuild inserts a user and one confirmed TOTP, WebAuthn, SSO link and recovery key,
// each with its own provenance. u must already carry its password and address provenance.
func snapBuild(t *testing.T, db *sql.DB, u snapUser, totpM, webM, ssoM, recM snapMaturity) snapAccount {
	t.Helper()
	u.insert(t, db)
	return snapAccount{
		user:  u,
		totp:  snapAddTOTP(t, db, u.id, true, true, totpM),
		web:   snapAddWebAuthn(t, db, u.id, webM),
		sso:   snapAddSSO(t, db, u.id, ssoM),
		rec:   snapAddRecovery(t, db, u.id, recM),
		totpM: totpM, webM: webM, ssoM: ssoM, recM: recM,
	}
}

func snapBuildLegacy(t *testing.T, db *sql.DB) snapAccount {
	t.Helper()
	m := snapLegacyMaturity()
	return snapBuild(t, db, newSnapUser(), m, m, m, m)
}

// wantInput is the Input the loader must produce for the account alone.
func (a snapAccount) wantInput() decide.Input {
	in := a.user.baseInput()
	in.Creds = append(in.Creds,
		a.totpM.cred(a.totp, false),
		a.webM.cred(a.web, false),
		a.ssoM.cred(a.sso, false),
		a.recM.cred(a.rec, true),
	)
	return in
}

// ---------------------------------------------------------------------------
// Request fixtures
// ---------------------------------------------------------------------------

// snapReq is one account_pending_requests row. Zero values are stored as NULL where the
// column is nullable: a zero target kind is no target, a zero time is NULL, an empty
// fingerprint is NULL, a zero fpKeyID is NULL.
type snapReq struct {
	id             uuid.UUID
	kind           decide.ReqKind
	status         decide.ReqStatus
	target         decide.CredRef
	resetFactors   bool
	originClass    decide.Class
	fingerprint    []byte
	coarse         []byte
	fpKeyID        int16
	disputeGroup   uuid.UUID
	openedAt       time.Time
	completesAt    time.Time
	heldMs         int64
	heldSince      time.Time
	disputedAt     time.Time
	completedAt    time.Time
	cancelledAt    time.Time
	cancelReason   decide.CancelReason
	cancellerClass decide.Class
	origins        []decide.CredRef
}

// snapOpenReq is a pending request of the given kind (target is ignored by a break-glass,
// which carries none).
func snapOpenReq(kind decide.ReqKind, target decide.CredRef) snapReq {
	r := snapReq{
		id:          uuid.New(),
		kind:        kind,
		status:      decide.StatusPending,
		target:      target,
		originClass: decide.C2,
		openedAt:    snapNow.Add(-24 * time.Hour),
		completesAt: snapNow.Add(48 * time.Hour),
	}
	if kind == decide.ReqBreakGlass {
		r.target = decide.CredRef{}
	}
	return r
}

func snapDisputedReq(target decide.CredRef) snapReq {
	r := snapOpenReq(decide.ReqCredentialRemove, target)
	r.status = decide.StatusDisputed
	r.disputedAt = snapNow.Add(-2 * time.Hour)
	r.disputeGroup = uuid.New()
	return r
}

func snapCompletedReq(target decide.CredRef) snapReq {
	r := snapOpenReq(decide.ReqCredentialRemove, target)
	r.status = decide.StatusCompleted
	r.completedAt = snapNow.Add(-time.Hour)
	return r
}

// snapCancelledReq is a credential_remove cancelled at the given instant. A proof or
// dispute_resolved cancel keeps its fingerprints and records a canceller class; every
// other reason must have neither (the schema's retention and class checks).
func snapCancelledReq(reason decide.CancelReason, at time.Time, target decide.CredRef) snapReq {
	r := snapOpenReq(decide.ReqCredentialRemove, target)
	r.status = decide.StatusCancelled
	r.cancelReason = reason
	r.cancelledAt = at
	if reason == decide.CancelProof || reason == decide.CancelDisputeResolved {
		r.cancellerClass = decide.C2
		r.fingerprint = []byte("fp-" + r.id.String())
		r.coarse = []byte("coarse-" + r.id.String())
		r.fpKeyID = snapKeyID
	}
	return r
}

func snapInsertReq(t *testing.T, db *sql.DB, userID uuid.UUID, r snapReq) {
	t.Helper()
	var targetKind, targetID, targetVersion any
	if r.target.Kind != decide.CredUnknown {
		targetKind, targetID, targetVersion = snapSpell(t, r.target.Kind), r.target.ID, r.target.Version
	}
	var reason, keyID any
	if r.cancelReason != decide.CancelNone {
		reason = snapSpell(t, r.cancelReason)
	}
	if r.fpKeyID != 0 {
		keyID = r.fpKeyID
	}
	_, err := db.Exec(
		`INSERT INTO account_pending_requests (
		     id, user_id, kind, target_kind, target_id, target_material_version, reset_factors,
		     status, origin_class, origin_fingerprint, request_fingerprint, coarse_fingerprint,
		     fingerprint_key_id, dispute_group_id, opened_at, completes_at, held_ms, held_since,
		     disputed_at, completed_at, cancelled_at, cancel_reason, canceller_class)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
		     $19, $20, $21, $22, $23)`,
		r.id, userID, snapSpell(t, r.kind), targetKind, targetID, targetVersion, r.resetFactors,
		snapSpell(t, r.status), int(r.originClass), bytes.Repeat([]byte{7}, 32),
		snapBytesArg(r.fingerprint), snapBytesArg(r.coarse),
		keyID, snapUUIDArg(r.disputeGroup), r.openedAt, r.completesAt, r.heldMs, snapTimeArg(r.heldSince),
		snapTimeArg(r.disputedAt), snapTimeArg(r.completedAt), snapTimeArg(r.cancelledAt), reason,
		snapClassArg(r.cancellerClass),
	)
	require.NoError(t, err)
	for _, o := range r.origins {
		snapInsertOriginRaw(t, db, r.id, snapSpell(t, o.Kind), o.ID, o.Version)
	}
}

func snapInsertOriginRaw(t *testing.T, db *sql.DB, reqID uuid.UUID, kind, id string, version int64) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO account_pending_request_origins (request_id, credential_kind, credential_id, material_version)
		 VALUES ($1, $2, $3, $4)`,
		reqID, kind, id, version,
	)
	require.NoError(t, err)
}

func snapSortedRefs(refs []decide.CredRef) []decide.CredRef {
	out := slices.Clone(refs)
	slices.SortFunc(out, compareRefs)
	return out
}

// wantOpen is the Req the loader must produce for an open (pending or disputed) row.
func (r snapReq) wantOpen() decide.Req {
	req := decide.Req{
		ID:                   r.id.String(),
		Kind:                 r.kind,
		Status:               r.status,
		OriginClass:          r.originClass,
		Origins:              snapSortedRefs(r.origins),
		OpenedAt:             r.openedAt,
		CompletesAt:          r.completesAt,
		HeldMs:               r.heldMs,
		HeldSince:            r.heldSince,
		DisputedAt:           r.disputedAt,
		Fingerprint:          r.fingerprint,
		Coarse:               r.coarse,
		FingerprintKeyID:     r.fpKeyID,
		ResetFactors:         r.resetFactors,
		MatureNoticesSettled: true,
		// Ruling 10: PR4 builds the outbox; until then every loaded request reads true.
		AcceptedAddressNotice: true,
	}
	if r.target.Kind != decide.CredUnknown {
		req.Targets = []decide.CredRef{r.target}
	}
	if r.disputeGroup != uuid.Nil {
		req.DisputeGroup = r.disputeGroup.String()
	}
	return req
}

// wantCancel is the part of a recent cancel that decide reads (T9, T10): which request, its
// kind, why and when it was cancelled, who cancelled it, and its fingerprints.
func (r snapReq) wantCancel() decide.Req {
	return decide.Req{
		ID:               r.id.String(),
		Kind:             r.kind,
		Status:           decide.StatusCancelled,
		CancelReason:     r.cancelReason,
		CancellerClass:   r.cancellerClass,
		CancelledAt:      r.cancelledAt,
		Fingerprint:      r.fingerprint,
		Coarse:           r.coarse,
		FingerprintKeyID: r.fpKeyID,
	}
}

// ---------------------------------------------------------------------------
// Comparison
//
// The brief and rulings pin no order for Creds, Primaries, Reservations, Reqs or
// RecentCancels, no nil-versus-empty choice, and nothing about a recent cancel beyond the
// fields decide reads. canon removes exactly those degrees of freedom from both sides, so
// a test fails on a value, never on a choice the spec leaves open. Origins keep the order
// the loader produced: that order is pinned.
// ---------------------------------------------------------------------------

func snapUTC(v time.Time) time.Time {
	if v.IsZero() {
		return time.Time{}
	}
	return v.UTC()
}

func snapNilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

func snapCanonReq(r decide.Req) decide.Req {
	r.OpenedAt = snapUTC(r.OpenedAt)
	r.CompletesAt = snapUTC(r.CompletesAt)
	r.HeldSince = snapUTC(r.HeldSince)
	r.DisputedAt = snapUTC(r.DisputedAt)
	r.CancelledAt = snapUTC(r.CancelledAt)
	r.Origins = snapNilIfEmpty(r.Origins)
	r.Targets = snapNilIfEmpty(r.Targets)
	r.Fingerprint = snapNilIfEmpty(r.Fingerprint)
	r.Coarse = snapNilIfEmpty(r.Coarse)
	return r
}

func snapCanonCancel(r decide.Req) decide.Req {
	return decide.Req{
		ID:               r.ID,
		Kind:             r.Kind,
		Status:           r.Status,
		CancelReason:     r.CancelReason,
		CancellerClass:   r.CancellerClass,
		CancelledAt:      snapUTC(r.CancelledAt),
		Fingerprint:      snapNilIfEmpty(r.Fingerprint),
		Coarse:           snapNilIfEmpty(r.Coarse),
		FingerprintKeyID: r.FingerprintKeyID,
	}
}

func snapCanon(in decide.Input) decide.Input {
	in.Now = snapUTC(in.Now)

	creds := slices.Clone(in.Creds)
	for i := range creds {
		creds[i].MaturesAt = snapUTC(creds[i].MaturesAt)
		creds[i].ContestedAt = snapUTC(creds[i].ContestedAt)
		creds[i].EnrolledAt = snapUTC(creds[i].EnrolledAt)
	}
	slices.SortFunc(creds, func(a, b decide.Cred) int { return compareRefs(a.Ref, b.Ref) })
	in.Creds = snapNilIfEmpty(creds)

	primaries := slices.Clone(in.Primaries)
	for i := range primaries {
		primaries[i].BecamePrimaryAt = snapUTC(primaries[i].BecamePrimaryAt)
		primaries[i].MaturesAt = snapUTC(primaries[i].MaturesAt)
		primaries[i].ContestedAt = snapUTC(primaries[i].ContestedAt)
		primaries[i].DisplacedAt = snapUTC(primaries[i].DisplacedAt)
	}
	slices.SortFunc(primaries, func(a, b decide.Primary) int { return compareRefs(a.Ref, b.Ref) })
	in.Primaries = snapNilIfEmpty(primaries)

	reservations := slices.Clone(in.Reservations)
	for i := range reservations {
		reservations[i].DisplacedAt = snapUTC(reservations[i].DisplacedAt)
		reservations[i].FloorUntil = snapUTC(reservations[i].FloorUntil)
	}
	slices.SortFunc(reservations, func(a, b decide.Reservation) int { return compareRefs(a.AddressRef, b.AddressRef) })
	in.Reservations = snapNilIfEmpty(reservations)

	reqs := make([]decide.Req, 0, len(in.Reqs))
	for _, r := range in.Reqs {
		reqs = append(reqs, snapCanonReq(r))
	}
	slices.SortFunc(reqs, func(a, b decide.Req) int { return strings.Compare(a.ID, b.ID) })
	in.Reqs = snapNilIfEmpty(reqs)

	cancels := make([]decide.Req, 0, len(in.RecentCancels))
	for _, r := range in.RecentCancels {
		cancels = append(cancels, snapCanonCancel(r))
	}
	slices.SortFunc(cancels, func(a, b decide.Req) int { return strings.Compare(a.ID, b.ID) })
	in.RecentCancels = snapNilIfEmpty(cancels)

	return in
}

func snapAssertInput(t *testing.T, want, got decide.Input) {
	t.Helper()
	assert.Equal(t, snapCanon(want), snapCanon(got))
}

// snapLoad runs loadInputTx in a transaction that is rolled back afterwards.
func snapLoad(t *testing.T, db *sql.DB, userID uuid.UUID) decide.Input {
	t.Helper()
	return snapLoadAt(t, db, userID, snapNow)
}

// snapLoadAt is snapLoad with an explicit now.
func snapLoadAt(t *testing.T, db *sql.DB, userID uuid.UUID, now time.Time) decide.Input {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	got, err := loadInputTx(context.Background(), tx, userID, snapKeyID, now)
	require.NoError(t, err)
	return got
}

func snapCredByKind(in decide.Input, kind decide.CredKind) []decide.Cred {
	var out []decide.Cred
	for _, c := range in.Creds {
		if c.Ref.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

func TestLoadInput_LegacyAccountReadsMature(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)

	got := snapLoad(t, db, acct.user.id)

	snapAssertInput(t, acct.wantInput(), got)
	// The sentinel is a real instant, not a zero: it must arrive as 1970, never as the
	// zero time.Time (which would count for nothing) and never as now.
	require.Len(t, got.Creds, 6)
	for _, c := range got.Creds {
		assert.True(t, c.MaturesAt.Equal(snapLegacy), "%v matures at %v", c.Ref.Kind, c.MaturesAt)
		assert.True(t, c.EnrolledAt.Equal(snapLegacy), "%v enrolled at %v", c.Ref.Kind, c.EnrolledAt)
		assert.False(t, c.MaturesAt.IsZero())
		assert.False(t, c.EnrolledAt.IsZero())
	}
	require.Len(t, got.Primaries, 1)
	assert.True(t, got.Primaries[0].BecamePrimaryAt.Equal(snapLegacy))
	assert.True(t, got.Primaries[0].DisplacedAt.IsZero())
	assert.Empty(t, got.Reqs)
	assert.Empty(t, got.RecentCancels)
	assert.Empty(t, got.Reservations)
}

func TestLoadInput_InputCarriesNowAndKeyIDAndLeavesActionAndProofToTheRunner(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)

	got := snapLoad(t, db, acct.user.id)

	assert.True(t, got.Now.Equal(snapNow), "Now must be the now argument, got %v", got.Now)
	assert.Equal(t, snapKeyID, got.CurrentKeyID)
	assert.Nil(t, got.Act)
	assert.Equal(t, decide.Proof{}, got.Proof)
}

func TestLoadInput_NullEnrolClassIsNone(t *testing.T) {
	db := snapDB(t)
	// Every class column is NULL (the legacy default): all six credentials read ClassNone.
	acct := snapBuildLegacy(t, db)

	got := snapLoad(t, db, acct.user.id)

	snapAssertInput(t, acct.wantInput(), got)
	require.Len(t, got.Creds, 6)
	for _, c := range got.Creds {
		assert.Equal(t, decide.ClassNone, c.EnrolClass, "%v", c.Ref.Kind)
	}
}

// Each provenance column, with a non-zero and a zero value side by side: a column the
// loader dropped or crossed with its neighbour shows as a mismatch on exactly that
// credential.
func TestLoadInput_MaturityContestAndEnrolmentRoundTrip(t *testing.T) {
	db := snapDB(t)
	u := newSnapUser()
	u.password = snapMaturity{ // pragma: allowlist secret
		maturesAt:  snapNow.Add(-96 * time.Hour),
		enrolledAt: snapNow.Add(-200 * time.Hour),
		enrolClass: decide.C3,
	}
	u.address = snapMaturity{
		maturesAt:   snapNow.Add(30 * time.Hour),
		contestedAt: snapNow.Add(-5 * time.Hour),
		enrolledAt:  snapNow.Add(-40 * time.Hour),
		enrolClass:  decide.C1,
	}
	totpM := snapMaturity{
		maturesAt:  snapNow.Add(-72 * time.Hour),
		enrolledAt: snapNow.Add(-150 * time.Hour),
		enrolClass: decide.C2,
	}
	webM := snapMaturity{
		maturesAt:   snapNow.Add(10 * time.Hour),
		contestedAt: snapNow.Add(-3 * time.Hour),
		enrolledAt:  snapNow.Add(-20 * time.Hour),
		enrolClass:  decide.C3,
	}
	ssoM := snapMaturity{
		maturesAt:  snapNow.Add(-1 * time.Hour),
		enrolledAt: snapNow.Add(-90 * time.Hour),
		enrolClass: decide.C1,
	}
	recM := snapMaturity{
		maturesAt:   snapNow.Add(-500 * time.Hour),
		contestedAt: snapNow.Add(-7 * time.Hour),
		enrolledAt:  snapNow.Add(-600 * time.Hour),
		enrolClass:  decide.C2,
	}
	acct := snapBuild(t, db, u, totpM, webM, ssoM, recM)

	got := snapLoad(t, db, acct.user.id)

	snapAssertInput(t, acct.wantInput(), got)
	require.Len(t, got.Primaries, 1)
	assert.True(t, got.Primaries[0].ContestedAt.Equal(u.address.contestedAt), "the primary carries the address contest")
	assert.True(t, got.Primaries[0].MaturesAt.Equal(u.address.maturesAt))
	assert.True(t, got.Primaries[0].BecamePrimaryAt.Equal(u.address.enrolledAt))
}

func TestLoadInput_VersionsAreTheRowMaterialVersions(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)

	got := snapLoad(t, db, acct.user.id)

	versions := map[decide.CredKind]int64{}
	for _, c := range got.Creds {
		versions[c.Ref.Kind] = c.Ref.Version
	}
	assert.Equal(t, acct.user.pwRef.Version, versions[decide.CredPassword], "password: users.password_version")
	assert.Equal(t, acct.user.addrRef.Version, versions[decide.CredAddress], "address: users.email_version")
	assert.Equal(t, acct.totp.Version, versions[decide.CredTOTP])
	assert.Equal(t, acct.web.Version, versions[decide.CredWebAuthn])
	assert.Equal(t, acct.sso.Version, versions[decide.CredSSO])
	assert.Equal(t, acct.rec.Version, versions[decide.CredRecoveryKey])
	// The six draws come from one sequence, so no two are equal: a loader that read the
	// wrong column for one credential would repeat another's version.
	seen := map[int64]decide.CredKind{}
	for kind, v := range versions {
		other, dup := seen[v]
		assert.False(t, dup, "%v and %v share version %d", kind, other, v)
		seen[v] = kind
	}
}

func TestLoadInput_CredentialIDs(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)

	got := snapLoad(t, db, acct.user.id)

	ids := map[decide.CredKind]string{}
	for _, c := range got.Creds {
		ids[c.Ref.Kind] = c.Ref.ID
	}
	uid := acct.user.id.String()
	assert.Equal(t, uid, ids[decide.CredPassword], "password: the user UUID")
	assert.Equal(t, uid, ids[decide.CredTOTP], "TOTP: the user UUID (the row is keyed by user_id)")
	assert.Equal(t, uid, ids[decide.CredRecoveryKey], "recovery key: the user UUID (keyed by user_id)")
	assert.Equal(t, acct.web.ID, ids[decide.CredWebAuthn], "WebAuthn: the row id")
	assert.Equal(t, acct.sso.ID, ids[decide.CredSSO], "SSO: the row id")
	assert.NotEqual(t, uid, acct.web.ID)
}

func TestLoadInput_RecoveryKeyIsPaper(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)

	got := snapLoad(t, db, acct.user.id)

	snapAssertInput(t, acct.wantInput(), got)
	require.Len(t, got.Creds, 6)
	for _, c := range got.Creds {
		assert.Equal(t, c.Ref.Kind == decide.CredRecoveryKey, c.Paper, "Paper on %v", c.Ref.Kind)
	}
}

func TestLoadInput_UnconfirmedTOTPNotLoaded(t *testing.T) {
	cases := []struct {
		name               string
		enabled, confirmed bool
		loaded             bool
	}{
		{"neither enabled nor confirmed", false, false, false},
		{"enabled but not confirmed", true, false, false},
		{"confirmed but not enabled", false, true, false},
		{"enabled and confirmed", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := snapDB(t)
			u := newSnapUser()
			u.insert(t, db)
			totp := snapAddTOTP(t, db, u.id, tc.enabled, tc.confirmed, snapLegacyMaturity())

			got := snapLoad(t, db, u.id)

			want := u.baseInput()
			if tc.loaded {
				want.Creds = append(want.Creds, snapLegacyMaturity().cred(totp, false))
			}
			snapAssertInput(t, want, got)
			assert.Equal(t, tc.loaded, len(snapCredByKind(got, decide.CredTOTP)) == 1)
		})
	}
}

func TestLoadInput_UnverifiedAddressIsNotACredential(t *testing.T) {
	db := snapDB(t)
	u := newSnapUser()
	u.verified = false
	u.insert(t, db)

	got := snapLoad(t, db, u.id)

	assert.Empty(t, snapCredByKind(got, decide.CredAddress))
	assert.Empty(t, got.Primaries, "an unverified address is not a primary either (ruling 20)")
	assert.Equal(t, []decide.CredKind{decide.CredPassword}, func() []decide.CredKind {
		kinds := make([]decide.CredKind, 0, len(got.Creds))
		for _, c := range got.Creds {
			kinds = append(kinds, c.Ref.Kind)
		}
		return kinds
	}())
}

func TestLoadInput_EmptyPasswordHashIsNotAPasswordCredential(t *testing.T) {
	db := snapDB(t)
	u := newSnapUser()
	u.passwordHash = ""
	u.insert(t, db)

	got := snapLoad(t, db, u.id)

	assert.Empty(t, snapCredByKind(got, decide.CredPassword))
	snapAssertInput(t, u.baseInput(), got)
}

func TestLoadInput_SSOLoginFromLoginModeColumn(t *testing.T) {
	cases := []struct {
		name          string
		loginDisabled bool
		trustSSO      bool
		linkSSO       bool
		want          bool
	}{
		{"password login disabled, SSO linked, real hash still stored", true, false, true, true},
		{"password login disabled and trusted", true, true, true, true},
		{"password login enabled with an SSO link is a password account", false, false, true, false},
		{"password login enabled, SSO trusted, still a password account", false, true, true, false},
		{"password login enabled, nothing linked", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := snapDB(t)
			u := newSnapUser()
			u.loginDisabled = tc.loginDisabled
			u.trustSSO = tc.trustSSO
			u.insert(t, db)
			want := u.baseInput()
			want.SSOLogin = tc.want
			if tc.linkSSO {
				sso := snapAddSSO(t, db, u.id, snapLegacyMaturity())
				want.Creds = append(want.Creds, snapLegacyMaturity().cred(sso, false))
			}

			got := snapLoad(t, db, u.id)

			snapAssertInput(t, want, got)
			assert.Equal(t, tc.want, got.SSOLogin)
			assert.Len(t, snapCredByKind(got, decide.CredPassword), 1,
				"an SSO account stores a real hash; the password credential is still loaded")
		})
	}
}

func TestLoadInput_ReservationsLoadedForTheUserOnly(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)
	other := newSnapUser()
	other.insert(t, db)

	type reservation struct {
		address, owner         string
		displacedAt, floorUtil time.Time
		requestID              any
	}
	mine := []reservation{
		{"old-one@example.test", acct.user.id.String(), snapNow.Add(-100 * time.Hour), snapNow.Add(-28 * time.Hour), uuid.New()},
		{"old-two@example.test", acct.user.id.String(), snapNow.Add(-40 * time.Hour), snapNow.Add(32 * time.Hour), nil},
	}
	theirs := reservation{"theirs@example.test", other.id.String(), snapNow.Add(-10 * time.Hour), snapNow.Add(62 * time.Hour), nil}
	for _, r := range append(slices.Clone(mine), theirs) {
		_, err := db.Exec(
			`INSERT INTO account_address_reservations (address, user_id, request_id, reason, displaced_at, floor_until)
			 VALUES ($1, $2, $3, 'displaced_primary', $4, $5)`,
			r.address, r.owner, r.requestID, r.displacedAt, r.floorUtil,
		)
		require.NoError(t, err)
	}

	got := snapLoad(t, db, acct.user.id)

	want := acct.wantInput()
	for _, r := range mine {
		want.Reservations = append(want.Reservations, decide.Reservation{
			AddressRef:  decide.CredRef{Kind: decide.CredAddress, ID: snapAddressID(r.address)},
			DisplacedAt: r.displacedAt,
			FloorUntil:  r.floorUtil,
		})
	}
	snapAssertInput(t, want, got)
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// Each of HeldSince, ResetFactors and the recent cancels round-trips both a non-zero and
// a zero value: decide cannot tell a dropped load from a real zero (round-2 Q6).
func TestLoadInput_HeldSinceResetFactorsRecentCancelsLoaded(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)
	uid := acct.user.id

	held := snapOpenReq(decide.ReqBreakGlass, decide.CredRef{})
	held.resetFactors = true
	held.originClass = decide.C3
	held.heldMs = 90_000
	held.heldSince = snapNow.Add(-3 * time.Hour)
	held.fingerprint = []byte("fp-request-a")
	held.coarse = []byte("coarse-a")
	held.fpKeyID = snapKeyID
	held.origins = []decide.CredRef{acct.user.pwRef}

	unheld := snapOpenReq(decide.ReqCredentialRemove, acct.totp)
	unheld.originClass = decide.C1
	unheld.coarse = []byte("coarse-b")
	unheld.origins = []decide.CredRef{acct.user.pwRef, acct.web}

	disputed := snapDisputedReq(acct.sso)
	disputed.originClass = decide.C2
	disputed.origins = []decide.CredRef{acct.rec}

	cancelProof := snapCancelledReq(decide.CancelProof, snapNow.Add(-2*time.Hour), acct.web)
	cancelProof.cancellerClass = decide.C3
	cancelResolved := snapCancelledReq(decide.CancelDisputeResolved, snapNow.Add(-29*24*time.Hour), acct.web)
	cancelResolved.kind = decide.ReqBreakGlass
	cancelResolved.target = decide.CredRef{}
	cancelResolved.cancellerClass = decide.C1
	cancelResolved.fingerprint = nil // no keyed fingerprint: a zero must arrive as a zero
	cancelResolved.fpKeyID = 0

	for _, r := range []snapReq{held, unheld, disputed, cancelProof, cancelResolved} {
		snapInsertReq(t, db, uid, r)
	}

	got := snapLoad(t, db, uid)

	want := acct.wantInput()
	want.Reqs = []decide.Req{held.wantOpen(), unheld.wantOpen(), disputed.wantOpen()}
	want.RecentCancels = []decide.Req{cancelProof.wantCancel(), cancelResolved.wantCancel()}
	snapAssertInput(t, want, got)

	byID := map[string]decide.Req{}
	for _, r := range got.Reqs {
		byID[r.ID] = r
	}
	require.Len(t, byID, 3)
	assert.True(t, byID[held.id.String()].HeldSince.Equal(held.heldSince), "an open hold keeps its start")
	assert.Equal(t, int64(90_000), byID[held.id.String()].HeldMs)
	assert.True(t, byID[held.id.String()].ResetFactors)
	assert.True(t, byID[unheld.id.String()].HeldSince.IsZero(), "a request with no hold reads a zero HeldSince")
	assert.False(t, byID[unheld.id.String()].ResetFactors)
	assert.Equal(t, snapKeyID, byID[held.id.String()].FingerprintKeyID)
	assert.Zero(t, byID[unheld.id.String()].FingerprintKeyID)
}

func TestLoadInput_RequestReadsAreBoundedToTheUserAndOpenStatus(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)
	other := snapBuildLegacy(t, db)
	uid := acct.user.id

	open := snapOpenReq(decide.ReqCredentialRemove, acct.totp)
	open.origins = []decide.CredRef{acct.user.pwRef}
	completed := snapCompletedReq(acct.web)
	superseded := snapCancelledReq(decide.CancelSuperseded, snapNow.Add(-time.Hour), acct.web)
	holdCap := snapCancelledReq(decide.CancelHoldCap, snapNow.Add(-time.Hour), acct.sso)
	expired := snapCancelledReq(decide.CancelDisputeExpired, snapNow.Add(-time.Hour), acct.sso)
	recent := snapCancelledReq(decide.CancelProof, snapNow.Add(-time.Hour), acct.sso)
	otherOpen := snapOpenReq(decide.ReqCredentialRemove, other.totp)
	otherCancel := snapCancelledReq(decide.CancelProof, snapNow.Add(-time.Hour), other.web)

	for _, r := range []snapReq{open, completed, superseded, holdCap, expired, recent} {
		snapInsertReq(t, db, uid, r)
	}
	for _, r := range []snapReq{otherOpen, otherCancel} {
		snapInsertReq(t, db, other.user.id, r)
	}

	got := snapLoad(t, db, uid)

	want := acct.wantInput()
	want.Reqs = []decide.Req{open.wantOpen()}
	want.RecentCancels = []decide.Req{recent.wantCancel()}
	snapAssertInput(t, want, got)
}

// The window is the half-open interval (now - NCancelCP, now]: a cancel exactly NCancelCP
// old is outside it (decide.reopenWindow), so the loader's > must not be >=.
func TestLoadInput_CancelsOlderThan30dExcluded(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)
	uid := acct.user.id
	window := decide.NCancelCP

	cases := []struct {
		name   string
		reason decide.CancelReason
		at     time.Time
		loaded bool
	}{
		{"proof one microsecond inside", decide.CancelProof, snapNow.Add(-window + time.Microsecond), true},
		{"proof exactly on the boundary", decide.CancelProof, snapNow.Add(-window), false},
		{"proof one microsecond outside", decide.CancelProof, snapNow.Add(-window - time.Microsecond), false},
		{"proof long ago", decide.CancelProof, snapNow.Add(-90 * 24 * time.Hour), false},
		{"resolved one microsecond inside", decide.CancelDisputeResolved, snapNow.Add(-window + time.Microsecond), true},
		{"resolved exactly on the boundary", decide.CancelDisputeResolved, snapNow.Add(-window), false},
		{"resolved just outside", decide.CancelDisputeResolved, snapNow.Add(-window - time.Second), false},
		{"proof a minute ago", decide.CancelProof, snapNow.Add(-time.Minute), true},
	}
	want := acct.wantInput()
	for _, tc := range cases {
		r := snapCancelledReq(tc.reason, tc.at, acct.web)
		snapInsertReq(t, db, uid, r)
		if tc.loaded {
			want.RecentCancels = append(want.RecentCancels, r.wantCancel())
		}
	}

	got := snapLoad(t, db, uid)

	snapAssertInput(t, want, got)
	assert.Len(t, got.RecentCancels, 3)
}

func TestLoadInput_OriginsInCanonicalOrder(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)
	uid := acct.user.id

	// Canonical: kind by number (password 1 < address 2 < sso 4 < totp 6 < webauthn 7 <
	// recovery key 9), then ID byte for byte ('Z' 0x5a < '_' 0x5f < 'b' 0x62 < 'l' 0x6c, which
	// a locale collation does not do), then version numerically (2 < 10).
	canonical := []decide.CredRef{
		{Kind: decide.CredPassword, ID: "Zeta", Version: 4},
		{Kind: decide.CredPassword, ID: "a_b", Version: 1},
		{Kind: decide.CredPassword, ID: "ab", Version: 1},
		{Kind: decide.CredPassword, ID: "alpha", Version: 2},
		{Kind: decide.CredPassword, ID: "alpha", Version: 10},
		{Kind: decide.CredAddress, ID: "zed@example.test", Version: 3},
		{Kind: decide.CredSSO, ID: "sso-row", Version: 1},
		{Kind: decide.CredTOTP, ID: "totp-row", Version: 9},
		{Kind: decide.CredWebAuthn, ID: "Web-row", Version: 3},
		{Kind: decide.CredWebAuthn, ID: "web-row", Version: 3},
		{Kind: decide.CredRecoveryKey, ID: "rec-row", Version: 1},
	}
	// Inserted in a scrambled order, so heap order cannot pass for the right order.
	scrambled := make([]decide.CredRef, len(canonical))
	for i, j := range []int{7, 2, 10, 0, 5, 9, 3, 1, 8, 4, 6} {
		scrambled[i] = canonical[j]
	}
	first := snapOpenReq(decide.ReqCredentialRemove, acct.totp)
	first.origins = scrambled
	second := snapOpenReq(decide.ReqCredentialRemove, acct.web)
	second.status = decide.StatusDisputed
	second.disputedAt = snapNow.Add(-time.Hour)
	second.disputeGroup = uuid.New()
	second.origins = []decide.CredRef{
		{Kind: decide.CredTOTP, ID: "other-totp", Version: 2},
		{Kind: decide.CredPassword, ID: "other-pw", Version: 8},
	}
	snapInsertReq(t, db, uid, first)
	snapInsertReq(t, db, uid, second)

	got := snapLoad(t, db, uid)

	byID := map[string]decide.Req{}
	for _, r := range got.Reqs {
		byID[r.ID] = r
	}
	require.Len(t, byID, 2)
	assert.Equal(t, canonical, byID[first.id.String()].Origins, "origins in kind, ID, version order")
	assert.Equal(t, []decide.CredRef{
		{Kind: decide.CredPassword, ID: "other-pw", Version: 8},
		{Kind: decide.CredTOTP, ID: "other-totp", Version: 2},
	}, byID[second.id.String()].Origins, "each request keeps only its own origins")

	want := acct.wantInput()
	want.Reqs = []decide.Req{first.wantOpen(), second.wantOpen()}
	snapAssertInput(t, want, got)
}

func TestLoadInput_StubsAreExplicitTrue(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)
	empty := newSnapUser()
	empty.insert(t, db)

	held := snapOpenReq(decide.ReqBreakGlass, decide.CredRef{})
	held.heldMs = 5
	held.heldSince = snapNow.Add(-time.Hour)
	unheld := snapOpenReq(decide.ReqCredentialRemove, acct.totp)
	disputed := snapDisputedReq(acct.web)
	for _, r := range []snapReq{held, unheld, disputed} {
		snapInsertReq(t, db, acct.user.id, r)
	}

	got := snapLoad(t, db, acct.user.id)

	assert.True(t, got.GuessableBudgetClear)
	require.Len(t, got.Reqs, 3)
	for _, r := range got.Reqs {
		assert.True(t, r.MatureNoticesSettled, "request %s", r.ID)
		assert.True(t, r.AcceptedAddressNotice, "request %s: ruling 10 stub", r.ID)
	}
	// The budget stub is a constant, not a function of the requests: an account with none
	// reads true too.
	assert.True(t, snapLoad(t, db, empty.id).GuessableBudgetClear)
}

// The stubs must stay visible: the brief requires the comment verbatim beside them.
func TestLoadInput_StubCommentIsPresentInSource(t *testing.T) {
	src, err := os.ReadFile("snapshot.go")
	require.NoError(t, err)
	// Once beside GuessableBudgetClear and once beside the per-request flags (ruling 21).
	assert.Equal(t, 2, strings.Count(string(src), snapStubComment))
}

// ---------------------------------------------------------------------------
// All or nothing (ruling 5)
// ---------------------------------------------------------------------------

func TestLoadInput_AnyFailureReturnsZeroInputAndLeaksNoUserData(t *testing.T) {
	type failure struct {
		name string
		// setup returns the transaction and context to load under, and every string the
		// error must not contain. The account is fully loaded by every earlier query, so a
		// partly built Input would be visible.
		setup func(t *testing.T, db *sql.DB, acct snapAccount) (context.Context, *sql.Tx, []string, error)
	}
	begin := func(t *testing.T, db *sql.DB) *sql.Tx {
		t.Helper()
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		return tx
	}
	failures := []failure{
		{"transaction already finished", func(t *testing.T, db *sql.DB, _ snapAccount) (context.Context, *sql.Tx, []string, error) {
			tx := begin(t, db)
			require.NoError(t, tx.Rollback())
			return context.Background(), tx, nil, sql.ErrTxDone
		}},
		{"context cancelled", func(t *testing.T, db *sql.DB, _ snapAccount) (context.Context, *sql.Tx, []string, error) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, begin(t, db), nil, context.Canceled
		}},
		{"origin of an unknown credential kind", func(t *testing.T, db *sql.DB, acct snapAccount) (context.Context, *sql.Tx, []string, error) {
			r := snapOpenReq(decide.ReqCredentialRemove, acct.totp)
			snapInsertReq(t, db, acct.user.id, r)
			snapInsertOriginRaw(t, db, r.id, "bogus_kind", "origin-row-id", 3)
			return context.Background(), begin(t, db), []string{r.id.String(), "bogus_kind", "origin-row-id"}, nil
		}},
		{"target of an unknown credential kind", func(t *testing.T, db *sql.DB, acct snapAccount) (context.Context, *sql.Tx, []string, error) {
			r := snapOpenReq(decide.ReqCredentialRemove, acct.totp)
			snapInsertReq(t, db, acct.user.id, r)
			_, err := db.Exec(`UPDATE account_pending_requests SET target_kind = 'bogus_kind' WHERE id = $1`, r.id)
			require.NoError(t, err)
			return context.Background(), begin(t, db), []string{r.id.String(), "bogus_kind"}, nil
		}},
		{"no such account", func(t *testing.T, db *sql.DB, _ snapAccount) (context.Context, *sql.Tx, []string, error) {
			return context.Background(), begin(t, db), nil, nil
		}},
	}
	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			db := snapDB(t)
			acct := snapBuildLegacy(t, db)
			ctx, tx, secrets, wantIs := f.setup(t, db, acct)
			userID := acct.user.id
			if f.name == "no such account" {
				userID = uuid.New()
			}

			got, err := loadInputTx(ctx, tx, userID, snapKeyID, snapNow)

			require.Error(t, err)
			assert.Equal(t, decide.Input{}, got, "a failed load must not return a partly built snapshot")
			if wantIs != nil {
				assert.ErrorIs(t, err, wantIs, "the cause stays wrapped for the caller")
			}
			msg := err.Error()
			assert.NotEmpty(t, msg)
			for _, leaked := range append(secrets, userID.String(), acct.user.email) {
				assert.NotContains(t, msg, leaked, "the error names the operation only")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Locks (ruling 4, ruling 8)
// ---------------------------------------------------------------------------

// snapLock is one row the loader locks, with the statement that reaches it.
type snapLock struct {
	name  string
	table string
	where string
	args  []any
}

func (l snapLock) query(mode string) string {
	return "SELECT 1 FROM " + l.table + " WHERE " + l.where + " " + mode
}

// snapLockHeld reports whether another connection's NOWAIT lock on the row is refused,
// which is to say whether some open transaction holds a conflicting lock on it. The probe
// is rolled back at once, so it never holds a row any longer than the statement.
func snapLockHeld(t *testing.T, pool *sql.DB, query string, args ...any) bool {
	t.Helper()
	tx, err := pool.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var one int
	err = tx.QueryRow(query, args...).Scan(&one)
	var pgErr *pq.Error
	if errors.As(err, &pgErr) && string(pgErr.Code) == snapLockNotAvailable {
		return true
	}
	require.NoError(t, err, "the probe must find its row")
	return false
}

// snapWaitBlocked returns once a backend is waiting on a lock in a statement that names
// table, and fails the test when the loader finishes first (it never took the lock the
// blocker holds) or nothing waits within ten seconds.
func snapWaitBlocked(t *testing.T, db *sql.DB, table string, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("the loader finished without waiting on the %s row: it never locked it", table)
		default:
		}
		var waiting int
		require.NoError(t, db.QueryRow(
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'
			   AND pid <> pg_backend_pid() AND query LIKE '%' || $1 || '%'`, table,
		).Scan(&waiting))
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend waited on a %s lock within 10s", table)
}

// snapLockFixture is an account with two pending requests, a completed request, an
// unrelated user's open request, and one factor row of every locked table.
type snapLockFixture struct {
	acct            snapAccount
	lowReq, highReq snapReq
	completed       snapReq
	otherOpen       snapReq
	locks           []snapLock
}

func snapBuildLockFixture(t *testing.T, db *sql.DB) snapLockFixture {
	t.Helper()
	acct := snapBuildLegacy(t, db)
	other := snapBuildLegacy(t, db)
	uid := acct.user.id

	low := snapOpenReq(decide.ReqCredentialRemove, acct.totp)
	high := snapOpenReq(decide.ReqCredentialRemove, acct.web)
	if low.id.String() > high.id.String() {
		low.id, high.id = high.id, low.id
	}
	completed := snapCompletedReq(acct.sso)
	otherOpen := snapOpenReq(decide.ReqCredentialRemove, other.totp)
	// The higher id goes in first, so heap order is the reverse of id order and a loader
	// without ORDER BY id locks them the wrong way round.
	snapInsertReq(t, db, uid, high)
	snapInsertReq(t, db, uid, low)
	snapInsertReq(t, db, uid, completed)
	snapInsertReq(t, db, other.user.id, otherOpen)

	return snapLockFixture{
		acct: acct, lowReq: low, highReq: high, completed: completed, otherOpen: otherOpen,
		locks: []snapLock{
			{"lower request id", "account_pending_requests", "id = $1", []any{low.id}},
			{"higher request id", "account_pending_requests", "id = $1", []any{high.id}},
			{"user_mfa_totp", "user_mfa_totp", "user_id = $1", []any{uid}},
			{"user_mfa_webauthn", "user_mfa_webauthn", "id = $1", []any{uuid.MustParse(acct.web.ID)}},
			{"user_sso_identities", "user_sso_identities", "id = $1", []any{uuid.MustParse(acct.sso.ID)}},
			{"user_recovery_keys", "user_recovery_keys", "user_id = $1", []any{uid}},
		},
	}
}

// The order is requests (by id), then user_mfa_totp, user_mfa_webauthn,
// user_sso_identities, user_recovery_keys. For each position k a second transaction holds
// that row, so the loader parks on it: every row before k must by then be locked by the
// loader, and every row after k must not yet be.
func TestLoadInput_LocksRequestsThenFactorTablesInFixedOrder(t *testing.T) {
	db := snapDB(t)
	loaderPool := extraPool(t)
	probePool := extraPool(t)
	fx := snapBuildLockFixture(t, db)
	uid := fx.acct.user.id
	usersRow := snapLock{"users", "users", "id = $1", []any{uid}}

	for k, blockAt := range fx.locks {
		t.Run("loader parked on "+blockAt.name, func(t *testing.T) {
			blocker, err := db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			var one int
			require.NoError(t, blocker.QueryRow(blockAt.query("FOR UPDATE"), blockAt.args...).Scan(&one))

			loaderTx, err := loaderPool.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			done := make(chan struct{})
			var loadErr error
			go func() {
				defer close(done)
				_, loadErr = loadInputTx(ctx, loaderTx, uid, snapKeyID, snapNow)
			}()
			defer func() {
				_ = blocker.Rollback()
				<-done
				cancel()
				_ = loaderTx.Rollback()
			}()

			snapWaitBlocked(t, db, blockAt.table, done)
			for j, probed := range fx.locks {
				assert.Equal(t, j <= k,
					snapLockHeld(t, probePool, probed.query("FOR NO KEY UPDATE NOWAIT"), probed.args...),
					"while the loader waits on %s, lock on %s", blockAt.name, probed.name)
			}
			assert.False(t, snapLockHeld(t, probePool, usersRow.query("FOR UPDATE NOWAIT"), usersRow.args...),
				"the loader never locks the users row")

			require.NoError(t, blocker.Rollback())
			<-done
			require.NoError(t, loadErr)
		})
	}
}

// What the loader holds when it returns, and in what mode. Requests are locked FOR UPDATE
// (a KEY SHARE probe conflicts only with that), the factor rows FOR NO KEY UPDATE (a KEY
// SHARE probe passes, a NO KEY UPDATE probe does not). Nothing else is locked: not the
// users row, not a closed request, not another user's request.
func TestLoadInput_LockModesAndScopeAfterLoad(t *testing.T) {
	db := snapDB(t)
	loaderPool := extraPool(t)
	probePool := extraPool(t)
	fx := snapBuildLockFixture(t, db)
	uid := fx.acct.user.id

	loaderTx, err := loaderPool.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = loaderTx.Rollback() }()
	got, err := loadInputTx(context.Background(), loaderTx, uid, snapKeyID, snapNow)
	require.NoError(t, err)
	require.Len(t, got.Reqs, 2)

	for j, l := range fx.locks {
		isRequest := j < 2
		assert.True(t, snapLockHeld(t, probePool, l.query("FOR NO KEY UPDATE NOWAIT"), l.args...), "%s is locked", l.name)
		assert.Equal(t, isRequest, snapLockHeld(t, probePool, l.query("FOR KEY SHARE NOWAIT"), l.args...),
			"%s: FOR UPDATE for a request, FOR NO KEY UPDATE for a factor row", l.name)
	}

	unlocked := []snapLock{
		{"users row", "users", "id = $1", []any{uid}},
		{"completed request", "account_pending_requests", "id = $1", []any{fx.completed.id}},
		{"another user's request", "account_pending_requests", "id = $1", []any{fx.otherOpen.id}},
		{"another user's totp", "user_mfa_totp", "user_id = $1", []any{fx.otherOpen.target.ID}},
	}
	for _, l := range unlocked {
		assert.False(t, snapLockHeld(t, probePool, l.query("FOR UPDATE NOWAIT"), l.args...), "%s stays unlocked", l.name)
	}
}

// Ruling 13: the TOTP row is locked whatever its state, then filtered in Go. A confirm is an
// UPDATE of that row, so it must queue behind the loader; a lock taken only on a confirmed,
// enabled row leaves the unconfirmed row free, and the credential can go live between the
// snapshot and the decision. The loaded Input still omits the credential.
func TestLoadInput_LocksAnUnconfirmedTOTPRowAndStillOmitsIt(t *testing.T) {
	cases := []struct {
		name               string
		enabled, confirmed bool
	}{
		{"neither enabled nor confirmed", false, false},
		{"enabled but not confirmed", true, false},
		{"confirmed but not enabled", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := snapDB(t)
			loaderPool := extraPool(t)
			probePool := extraPool(t)
			u := newSnapUser()
			u.insert(t, db)
			snapAddTOTP(t, db, u.id, tc.enabled, tc.confirmed, snapLegacyMaturity())

			loaderTx, err := loaderPool.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			defer func() { _ = loaderTx.Rollback() }()
			got, err := loadInputTx(context.Background(), loaderTx, u.id, snapKeyID, snapNow)
			require.NoError(t, err)

			snapAssertInput(t, u.baseInput(), got)
			assert.Empty(t, snapCredByKind(got, decide.CredTOTP), "an unconfirmed or disabled TOTP is never loaded")

			totp := snapLock{"user_mfa_totp", "user_mfa_totp", "user_id = $1", []any{u.id}}
			assert.True(t, snapLockHeld(t, probePool, totp.query("FOR NO KEY UPDATE NOWAIT"), totp.args...),
				"the loader holds the row whatever its state")
			assert.False(t, snapLockHeld(t, probePool, totp.query("FOR KEY SHARE NOWAIT"), totp.args...),
				"the mode is FOR NO KEY UPDATE, not FOR UPDATE")

			// The write that would make the credential live queues behind the loader.
			confirm, err := probePool.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			defer func() { _ = confirm.Rollback() }()
			_, err = confirm.Exec(`SET LOCAL lock_timeout = '300ms'`)
			require.NoError(t, err)
			_, err = confirm.Exec(`UPDATE user_mfa_totp SET enabled = true, confirmed = true WHERE user_id = $1`, u.id)
			var pgErr *pq.Error
			require.ErrorAs(t, err, &pgErr, "the confirm must not complete while the loader's transaction is open")
			assert.Equal(t, snapLockNotAvailable, string(pgErr.Code))
		})
	}
}

// Ruling 9 and 16: the address credential's ID is lower(btrim(users.email)), whatever spelling
// the column holds, and a reservation under that text occupies the same slot.
func TestLoadInput_AddressIDIsTheNormalizedEmail(t *testing.T) {
	db := snapDB(t)
	u := newSnapUser()
	u.email = " Mixed.Case@Example.TEST"
	u.insert(t, db)
	var stored string
	require.NoError(t, db.QueryRow(`SELECT email FROM users WHERE id = $1`, u.id).Scan(&stored))
	require.Equal(t, " Mixed.Case@Example.TEST", stored, "the fixture must store the raw spelling")
	_, err := db.Exec(
		`INSERT INTO account_address_reservations (address, user_id, reason, displaced_at, floor_until)
		 VALUES ($1, $2, 'displaced_primary', $3, $4)`,
		"mixed.case@example.test", u.id, snapNow.Add(-10*time.Hour), snapNow.Add(62*time.Hour),
	)
	require.NoError(t, err)

	got := snapLoad(t, db, u.id)

	addrs := snapCredByKind(got, decide.CredAddress)
	require.Len(t, addrs, 1)
	assert.Equal(t, "mixed.case@example.test", addrs[0].Ref.ID)
	assert.Equal(t, u.addrRef.Version, addrs[0].Ref.Version)
	require.Len(t, got.Primaries, 1)
	assert.Equal(t, "mixed.case@example.test", got.Primaries[0].Ref.ID)
	require.Len(t, got.Reservations, 1)
	res := got.Reservations[0].AddressRef
	assert.Equal(t, addrs[0].Ref.Kind, res.Kind, "same slot: kind")
	assert.Equal(t, addrs[0].Ref.ID, res.ID, "same slot: ID")
	assert.Zero(t, res.Version)

	want := u.baseInput()
	want.Reservations = []decide.Reservation{{
		AddressRef:  decide.CredRef{Kind: decide.CredAddress, ID: "mixed.case@example.test"},
		DisplacedAt: snapNow.Add(-10 * time.Hour),
		FloorUntil:  snapNow.Add(62 * time.Hour),
	}}
	snapAssertInput(t, want, got)
}

// Ruling 18: cancelled_at is a microsecond column, and PostgreSQL ROUNDS a finer parameter.
// With now carrying 700 ns, an untruncated bound reaches the server as the next microsecond,
// which would drop the cancel one microsecond inside the window. The bound is truncated.
func TestLoadInput_CancelWindowTruncatesANanosecondNow(t *testing.T) {
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)
	uid := acct.user.id
	now := snapNow.Add(700 * time.Nanosecond)
	boundary := snapNow.Add(-decide.NCancelCP) // (now - NCancelCP) truncated to the microsecond

	onBoundary := snapCancelledReq(decide.CancelProof, boundary, acct.web)
	inside := snapCancelledReq(decide.CancelProof, boundary.Add(time.Microsecond), acct.web)
	outside := snapCancelledReq(decide.CancelDisputeResolved, boundary.Add(-time.Microsecond), acct.web)
	for _, r := range []snapReq{onBoundary, inside, outside} {
		snapInsertReq(t, db, uid, r)
	}

	got := snapLoadAt(t, db, uid, now)

	want := acct.wantInput()
	want.Now = now
	want.RecentCancels = []decide.Req{inside.wantCancel()}
	snapAssertInput(t, want, got)
	assert.True(t, got.Now.Equal(now), "Now keeps its nanoseconds")
}
