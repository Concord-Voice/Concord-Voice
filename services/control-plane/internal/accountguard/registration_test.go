package accountguard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// ---------------------------------------------------------------------------
// Fixtures (Task 11, #3680 PR3: EnrolAtRegistrationTx, H3-b)
//
// Every test INSERTs real rows in a transaction the way the two registration writers do
// (auth/pending.go:481 for the users row, oauth/handler.go for the SSO identity) and leaves
// the maturity columns to their defaults, the 000189 stamp of now() + 72 h. Each test
// asserts that row reads immature BEFORE it calls the function, so a fixture that began
// mature (and a function that stamped nothing) could not pass.
//
// Times compare by instant (regUTC), and the transaction's now() is read from the database:
// the INSERT default and the function's now() are the same instant in one transaction, so
// a stale enrolled_at is told apart by seeding it explicitly where that matters.
// ---------------------------------------------------------------------------

// regImmatureFloor is the least a defaulted maturity may read at: the stamp is 72 h, and an
// hour of slack keeps the check off the clock.
const regImmatureFloor = 71 * time.Hour

func regUTC(v time.Time) time.Time { return v.UTC() }

type regQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// regBegin opens a transaction that is rolled back when the test ends.
func regBegin(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// regInsertUser inserts a users row with the column set auth/pending.go:481 writes and
// every maturity column at its default.
func regInsertUser(t *testing.T, q regQuerier, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := q.ExecContext(context.Background(),
		`INSERT INTO users (id, email, username, password_hash, email_verified, age_verified)
		 VALUES ($1, $2, $3, $4, TRUE, TRUE)`,
		id, email, "reg"+strings.ReplaceAll(id.String(), "-", "")[:16], "x")
	require.NoError(t, err)
	return id
}

// regInsertStaleUser is regInsertUser with both provenance slots seeded to values a stamp
// must replace: the enrolled_at pair and a class in range.
func regInsertStaleUser(t *testing.T, q regQuerier, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := q.ExecContext(context.Background(),
		`INSERT INTO users (id, email, username, password_hash, email_verified, age_verified,
		     password_enrolled_at, password_enrol_class, email_enrolled_at, email_enrol_class)
		 VALUES ($1, $2, $3, $4, TRUE, TRUE, '2001-02-03Z', 2, '2004-05-06Z', 3)`,
		id, email, "reg"+strings.ReplaceAll(id.String(), "-", "")[:16], "x")
	require.NoError(t, err)
	return id
}

// regInsertSSO inserts a user_sso_identities row the way oauth/handler.go does, with the
// maturity columns at their defaults.
func regInsertSSO(t *testing.T, q regQuerier, userID uuid.UUID, provider string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := q.ExecContext(context.Background(),
		`INSERT INTO user_sso_identities (id, user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, $2, $3, $4, 'idp@example.test')`,
		id, userID, provider, "sub-"+id.String())
	require.NoError(t, err)
	return id
}

// regUserRow is the users columns the stamp touches or could disturb.
type regUserRow struct {
	Email         string
	Username      string
	PasswordHash  string
	EmailVerified bool
	PwMatures     time.Time
	EmMatures     time.Time
	PwEnrolled    time.Time
	EmEnrolled    time.Time
	PwClass       sql.NullInt16
	EmClass       sql.NullInt16
	PwVersion     int64
	EmVersion     int64
	EmContested   sql.NullTime
}

func regReadUser(t *testing.T, q regQuerier, id uuid.UUID) regUserRow {
	t.Helper()
	var r regUserRow
	err := q.QueryRowContext(context.Background(),
		`SELECT email, username, password_hash, email_verified,
		        password_matures_at, email_matures_at, password_enrolled_at, email_enrolled_at,
		        password_enrol_class, email_enrol_class, password_version, email_version,
		        email_contested_at
		   FROM users WHERE id = $1`, id,
	).Scan(&r.Email, &r.Username, &r.PasswordHash, &r.EmailVerified,
		&r.PwMatures, &r.EmMatures, &r.PwEnrolled, &r.EmEnrolled,
		&r.PwClass, &r.EmClass, &r.PwVersion, &r.EmVersion, &r.EmContested)
	require.NoError(t, err)
	r.PwMatures, r.EmMatures = regUTC(r.PwMatures), regUTC(r.EmMatures)
	r.PwEnrolled, r.EmEnrolled = regUTC(r.PwEnrolled), regUTC(r.EmEnrolled)
	return r
}

// regSSORow is every column of a user_sso_identities row that the stamp touches or could
// disturb.
type regSSORow struct {
	UserID         uuid.UUID
	Provider       string
	ProviderUserID string
	ProviderEmail  string
	Matures        time.Time
	Contested      sql.NullTime
	Version        int64
	Enrolled       time.Time
	Class          sql.NullInt16
}

func regReadSSO(t *testing.T, q regQuerier, id uuid.UUID) regSSORow {
	t.Helper()
	var r regSSORow
	err := q.QueryRowContext(context.Background(),
		`SELECT user_id, provider, provider_user_id, provider_email, matures_at,
		        maturity_contested_at, material_version, enrolled_at, enrol_class
		   FROM user_sso_identities WHERE id = $1`, id,
	).Scan(&r.UserID, &r.Provider, &r.ProviderUserID, &r.ProviderEmail, &r.Matures,
		&r.Contested, &r.Version, &r.Enrolled, &r.Class)
	require.NoError(t, err)
	r.Matures, r.Enrolled = regUTC(r.Matures), regUTC(r.Enrolled)
	return r
}

// regOtherColumns is the row as JSON minus the six maturity columns, so a test can prove
// the stamp wrote nothing else (an email normalized in place, an updated_at moved).
func regOtherColumns(t *testing.T, q regQuerier, id uuid.UUID) map[string]any {
	t.Helper()
	var raw []byte
	err := q.QueryRowContext(context.Background(),
		`SELECT to_jsonb(u) - $2::text[] FROM users u WHERE id = $1`, id,
		pq.Array([]string{
			"password_matures_at", "email_matures_at",
			"password_enrolled_at", "email_enrolled_at",
			"password_enrol_class", "email_enrol_class",
		}),
	).Scan(&raw)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

func regTxNow(t *testing.T, q regQuerier) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, q.QueryRowContext(context.Background(), `SELECT now()`).Scan(&now))
	return regUTC(now)
}

// regRequireImmature is the fixture proof for a users row: both maturities read the default
// stamp, 72 h out, and the row is neither contested nor legacy.
func regRequireImmature(t *testing.T, r regUserRow) {
	t.Helper()
	floor := time.Now().Add(regImmatureFloor)
	require.True(t, r.PwMatures.After(floor), "fixture: the password reads immature (the default stamp), got %v", r.PwMatures)
	require.True(t, r.EmMatures.After(floor), "fixture: the email reads immature (the default stamp), got %v", r.EmMatures)
	require.False(t, r.EmContested.Valid, "fixture: not contested")
	require.Greater(t, r.PwVersion, int64(1), "fixture: the 000189 trigger drew a password version")
	require.Greater(t, r.EmVersion, int64(1), "fixture: the 000189 trigger drew an email version")
}

func regRequireSSOImmature(t *testing.T, r regSSORow) {
	t.Helper()
	require.True(t, r.Matures.After(time.Now().Add(regImmatureFloor)),
		"fixture: the SSO identity reads immature (the default stamp), got %v", r.Matures)
	require.False(t, r.Contested.Valid, "fixture: not contested")
	require.Greater(t, r.Version, int64(1), "fixture: the 000189 trigger drew a material version")
}

// regRequireMatureUser asserts the full users row after a stamp: both slots mature from
// creation, both provenance pairs written together (enrolled_at the transaction's now(),
// enrol_class NULL), and everything else exactly as it was before.
func regRequireMatureUser(t *testing.T, before, after regUserRow, txNow time.Time) {
	t.Helper()
	want := before
	want.PwMatures, want.EmMatures = snapLegacy, snapLegacy
	want.PwEnrolled, want.EmEnrolled = txNow, txNow
	want.PwClass, want.EmClass = sql.NullInt16{}, sql.NullInt16{}
	assert.Equal(t, want, after, "the full users row: mature from creation, provenance as a pair, versions untouched")
}

func regRequireMatureSSO(t *testing.T, before, after regSSORow, txNow time.Time) {
	t.Helper()
	want := before
	want.Matures = snapLegacy
	want.Enrolled = txNow
	want.Class = sql.NullInt16{}
	assert.Equal(t, want, after, "the full SSO identity row: mature from creation, provenance as a pair, version untouched")
}

// regAdvisoryKey is the advisory key the address lock must take for a normalized address.
// It restates the key shape on purpose: the test pins the lock to the key the runner uses.
func regAdvisoryKey(t *testing.T, probe *sql.DB, normalized string) int64 {
	t.Helper()
	var key int64
	require.NoError(t, probe.QueryRow(
		`SELECT hashtextextended('concord.accountguard.address:' || $1, 0)`, normalized).Scan(&key))
	return key
}

// regAsync is a call running on its own goroutine. done closes when it returns, so any
// number of readers (the test body and a cleanup) can wait on it.
type regAsync struct {
	done chan struct{}
	err  error
}

func regGo(f func() error) *regAsync {
	a := &regAsync{done: make(chan struct{})}
	go func() {
		a.err = f()
		close(a.done)
	}()
	return a
}

// regWaitBound bounds every wait on a goroutine, in wall time, so a failing assertion names
// itself instead of hanging until go test's own timeout.
const regWaitBound = 30 * time.Second

// regAwait polls pg_locks until a backend waits on key (blocked) or the call finishes
// first (not blocked, with its error). It sequences on database state, never on a sleep,
// and the deadline names a hang instead of timing the whole run out.
func regAwait(t *testing.T, probe *sql.DB, key int64, a *regAsync) (blocked bool, err error) {
	t.Helper()
	deadline := time.Now().Add(regWaitBound)
	for time.Now().Before(deadline) {
		if testdb.AdvisoryLockWaiterExists(t, probe, key) {
			return true, nil
		}
		select {
		case <-a.done:
			return false, a.err
		default:
		}
	}
	require.FailNow(t, "neither a lock waiter nor a result arrived")
	return false, nil
}

// regReleaseOnExit registers the cleanup that frees a goroutine parked on holder's lock: it
// rolls the holder back first, then waits (bounded) for the goroutine, so a failing
// assertion cannot leave the waiter blocked behind a lock nobody will release. Register it
// AFTER the goroutine starts and AFTER every regBegin: cleanups run last-in first-out, so it
// runs before the waiter's own transaction is rolled back.
func regReleaseOnExit(t *testing.T, holder *sql.Tx, a *regAsync) {
	t.Helper()
	t.Cleanup(func() {
		_ = holder.Rollback()
		select {
		case <-a.done:
		case <-time.After(regWaitBound):
		}
	})
}

func regProbe(t *testing.T) *sql.DB {
	t.Helper()
	probe, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = probe.Close() })
	return probe
}

func regEmail() string { return "reg-" + uuid.NewString()[:8] + "@example.test" }

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestEnrolAtRegistration_StampsMatureWithProvenancePairs(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)

	cases := []struct {
		name   string
		insert func(*testing.T, regQuerier, string) uuid.UUID
		row    string // the email spelling the row stores
		param  string // the email spelling the caller passes
		stale  bool   // the row starts with seeded, stale provenance
	}{
		{name: "default provenance", insert: regInsertUser, row: regEmail(), param: ""},
		{name: "stale provenance is replaced as a pair", insert: regInsertStaleUser, row: regEmail(), param: "", stale: true},
		{name: "a differently cased padded spelling of the same address", insert: regInsertUser,
			row: "Mixed-" + uuid.NewString()[:8] + "@Example.Test", param: ""},
	}
	cases[0].param = cases[0].row
	cases[1].param = cases[1].row
	cases[2].param = "  " + strings.ToLower(cases[2].row) + "  "

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := regBegin(t, db)
			id := tc.insert(t, tx, tc.row)
			before := regReadUser(t, tx, id)
			regRequireImmature(t, before)
			if tc.stale {
				require.True(t, before.PwEnrolled.Equal(time.Date(2001, 2, 3, 0, 0, 0, 0, time.UTC)), "fixture: stale password enrolled_at")
				require.True(t, before.EmEnrolled.Equal(time.Date(2004, 5, 6, 0, 0, 0, 0, time.UTC)), "fixture: stale email enrolled_at")
				require.Equal(t, sql.NullInt16{Int16: 2, Valid: true}, before.PwClass, "fixture: stale password class")
				require.Equal(t, sql.NullInt16{Int16: 3, Valid: true}, before.EmClass, "fixture: stale email class")
			}
			others := regOtherColumns(t, tx, id)

			require.NoError(t, EnrolAtRegistrationTx(ctx, tx, id, tc.param, nil))

			after := regReadUser(t, tx, id)
			regRequireMatureUser(t, before, after, regTxNow(t, tx))
			assert.Equal(t, others, regOtherColumns(t, tx, id), "no other users column is written")
		})
	}
}

func TestEnrolAtRegistration_StampsSSOIdentityMatureWithProvenancePair(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	tx := regBegin(t, db)

	email := regEmail()
	id := regInsertUser(t, tx, email)
	sso := regInsertSSO(t, tx, id, "google")
	// A second identity of the same user: only the named one is stamped.
	other := regInsertSSO(t, tx, id, "github")

	userBefore := regReadUser(t, tx, id)
	ssoBefore := regReadSSO(t, tx, sso)
	otherBefore := regReadSSO(t, tx, other)
	regRequireImmature(t, userBefore)
	regRequireSSOImmature(t, ssoBefore)
	regRequireSSOImmature(t, otherBefore)

	require.NoError(t, EnrolAtRegistrationTx(ctx, tx, id, email, &sso))

	txNow := regTxNow(t, tx)
	regRequireMatureUser(t, userBefore, regReadUser(t, tx, id), txNow)
	regRequireMatureSSO(t, ssoBefore, regReadSSO(t, tx, sso), txNow)
	assert.Equal(t, otherBefore, regReadSSO(t, tx, other), "a sibling identity is not stamped")
}

// The 000189 triggers re-stamp a maturity the writer left equal to OLD whenever a material
// column moves. The registration stamp must stand after the triggers, and a maturity-only
// write must not move a version. The row is inserted, stamped and committed in ONE
// transaction, as a registration does, then re-read on a second connection, so a deferred
// or after-commit effect would show too.
func TestEnrolAtRegistration_MaturitySurvivesTheMaterialTriggers(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	probe := regProbe(t)

	email := regEmail()
	tx := regBegin(t, db)
	id := regInsertUser(t, tx, email)
	sso := regInsertSSO(t, tx, id, "google")
	userBefore := regReadUser(t, tx, id)
	ssoBefore := regReadSSO(t, tx, sso)
	regRequireImmature(t, userBefore)
	regRequireSSOImmature(t, ssoBefore)

	require.NoError(t, EnrolAtRegistrationTx(ctx, tx, id, email, &sso))
	txNow := regTxNow(t, tx)
	require.NoError(t, tx.Commit())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id) })

	userAfter := regReadUser(t, probe, id)
	ssoAfter := regReadSSO(t, probe, sso)
	assert.True(t, userAfter.PwMatures.Equal(snapLegacy), "password maturity stands after the triggers")
	assert.True(t, userAfter.EmMatures.Equal(snapLegacy), "email maturity stands after the triggers")
	assert.True(t, ssoAfter.Matures.Equal(snapLegacy), "SSO maturity stands after the trigger")
	assert.Equal(t, userBefore.PwVersion, userAfter.PwVersion, "password_version is unchanged by a maturity-only write")
	assert.Equal(t, userBefore.EmVersion, userAfter.EmVersion, "email_version is unchanged by a maturity-only write")
	assert.Equal(t, ssoBefore.Version, ssoAfter.Version, "material_version is unchanged by a maturity-only write")
	regRequireMatureUser(t, userBefore, userAfter, txNow)
	regRequireMatureSSO(t, ssoBefore, ssoAfter, txNow)
}

// Ruling 13 (I1): the stamp matures only a row THIS transaction inserted. An account that
// already exists, committed by an earlier transaction, is a contract breach and stays
// immature, so no later flow can zero the 72 h window of an existing account.
func TestEnrolAtRegistration_ExistingAccountIsContract(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	probe := regProbe(t)

	for _, withSSO := range []bool{false, true} {
		name := "users row only"
		if withSSO {
			name = "users row and its SSO identity"
		}
		t.Run(name, func(t *testing.T) {
			email := regEmail()
			setup := regBegin(t, db)
			id := regInsertUser(t, setup, email)
			sso := regInsertSSO(t, setup, id, "google")
			require.NoError(t, setup.Commit())
			t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id) })

			userBefore := regReadUser(t, probe, id)
			ssoBefore := regReadSSO(t, probe, sso)
			regRequireImmature(t, userBefore)
			regRequireSSOImmature(t, ssoBefore)

			tx := regBegin(t, db)
			var ssoArg *uuid.UUID
			if withSSO {
				ssoArg = &sso
			}
			err := EnrolAtRegistrationTx(ctx, tx, id, email, ssoArg)

			require.ErrorIs(t, err, ErrContract)
			status, _ := HTTPStatus(err)
			assert.Equal(t, 500, status)
			assert.NotContains(t, err.Error(), email, "the error names no address")
			assert.NotContains(t, err.Error(), id.String(), "the error names no id")
			require.NoError(t, tx.Rollback())
			assert.Equal(t, userBefore, regReadUser(t, probe, id), "the existing account stays immature")
			assert.Equal(t, ssoBefore, regReadSSO(t, probe, sso), "the existing identity stays immature")
		})
	}
}

// A row inserted under a savepoint carries a subtransaction id, so it is not provably this
// transaction's own and fails closed. Neither registration caller uses a savepoint.
func TestEnrolAtRegistration_SavepointInsertedRowsAreContract(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)

	t.Run("a users row inserted under a savepoint", func(t *testing.T) {
		tx := regBegin(t, db)
		email := regEmail()
		_, err := tx.ExecContext(ctx, `SAVEPOINT reg_sp`)
		require.NoError(t, err)
		id := regInsertUser(t, tx, email)
		_, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT reg_sp`)
		require.NoError(t, err)
		before := regReadUser(t, tx, id)
		regRequireImmature(t, before)

		err = EnrolAtRegistrationTx(ctx, tx, id, email, nil)

		require.ErrorIs(t, err, ErrContract)
		assert.Equal(t, before, regReadUser(t, tx, id), "nothing written")
	})

	t.Run("an SSO identity inserted under a savepoint", func(t *testing.T) {
		tx := regBegin(t, db)
		email := regEmail()
		id := regInsertUser(t, tx, email)
		_, err := tx.ExecContext(ctx, `SAVEPOINT reg_sp`)
		require.NoError(t, err)
		sso := regInsertSSO(t, tx, id, "google")
		_, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT reg_sp`)
		require.NoError(t, err)
		userBefore := regReadUser(t, tx, id)
		ssoBefore := regReadSSO(t, tx, sso)
		regRequireImmature(t, userBefore)
		regRequireSSOImmature(t, ssoBefore)

		err = EnrolAtRegistrationTx(ctx, tx, id, email, &sso)

		require.ErrorIs(t, err, ErrContract, "the users row is this transaction's own; the identity is not provably so")
		assert.Equal(t, ssoBefore, regReadSSO(t, tx, sso), "the identity is not stamped")
	})
}

func TestEnrolAtRegistration_ActivatedRefuses(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	tx := regBegin(t, db)

	email := regEmail()
	id := regInsertUser(t, tx, email)
	sso := regInsertSSO(t, tx, id, "google")
	userBefore := regReadUser(t, tx, id)
	ssoBefore := regReadSSO(t, tx, sso)
	regRequireImmature(t, userBefore)
	regRequireSSOImmature(t, ssoBefore)
	others := regOtherColumns(t, tx, id)

	// Transaction-local, so it rolls back with the test and the singleton is never left set.
	_, err := tx.ExecContext(ctx, `UPDATE accountguard_state SET activated_at = now()`)
	require.NoError(t, err)

	err = EnrolAtRegistrationTx(ctx, tx, id, email, &sso)

	require.ErrorIs(t, err, ErrActivated)
	status, _ := HTTPStatus(err)
	assert.Equal(t, 503, status)
	assert.Equal(t, userBefore, regReadUser(t, tx, id), "nothing written to users")
	assert.Equal(t, ssoBefore, regReadSSO(t, tx, sso), "nothing written to the SSO identity")
	assert.Equal(t, others, regOtherColumns(t, tx, id))
	var held int
	require.NoError(t, tx.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()`).Scan(&held))
	assert.Zero(t, held, "the marker is read before the address lock")
}

func TestEnrolAtRegistration_MissingMarkerRowIsContract(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	tx := regBegin(t, db)

	email := regEmail()
	id := regInsertUser(t, tx, email)
	before := regReadUser(t, tx, id)
	regRequireImmature(t, before)

	// Transaction-local, rolled back with the test.
	_, err := tx.ExecContext(ctx, `DELETE FROM accountguard_state`)
	require.NoError(t, err)

	err = EnrolAtRegistrationTx(ctx, tx, id, email, nil)

	require.ErrorIs(t, err, ErrContract, "a missing marker row is never read as not activated")
	assert.Equal(t, before, regReadUser(t, tx, id), "nothing written")
}

func TestEnrolAtRegistration_TakesAddressLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), regWaitBound)
	t.Cleanup(cancel)
	db := snapDB(t)
	probe := regProbe(t)

	// Two accounts whose emails are one normalized address in two spellings: the schema's
	// uniqueness is on the raw text, so both rows exist, and only the normalization makes
	// them contend.
	normalized := regEmail()
	spelledB := "  " + strings.ToUpper(normalized) + "  "
	key := regAdvisoryKey(t, probe, normalized)

	tx1 := regBegin(t, db)
	tx2 := regBegin(t, db)
	tx3 := regBegin(t, db)
	idA := regInsertUser(t, tx1, normalized)
	idB := regInsertUser(t, tx2, spelledB)
	other := regEmail()
	idC := regInsertUser(t, tx3, other)
	beforeB := regReadUser(t, tx2, idB)
	regRequireImmature(t, regReadUser(t, tx1, idA))
	regRequireImmature(t, beforeB)
	require.False(t, testdb.AdvisoryLockWaiterExists(t, probe, key), "fixture: nobody waits yet")

	require.NoError(t, EnrolAtRegistrationTx(ctx, tx1, idA, normalized, nil))

	// The lock is the one the runner takes for the normalized address, held to the end of
	// the transaction.
	classID, objID := testdb.AdvisoryKeyHalves(key)
	var held int
	require.NoError(t, tx1.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_locks
		  WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted
		    AND classid::bigint = $1 AND objid::bigint = $2`, classID, objID).Scan(&held))
	require.Equal(t, 1, held, "the call holds the advisory lock of the normalized address")

	// Control: an unrelated address does not wait on it.
	asyncC := regGo(func() error { return EnrolAtRegistrationTx(ctx, tx3, idC, other, nil) })
	regReleaseOnExit(t, tx1, asyncC)
	blockedC, errC := regAwait(t, probe, key, asyncC)
	require.False(t, blockedC, "a different address does not block")
	require.NoError(t, errC)

	// The second spelling of the same address waits for the first transaction.
	asyncB := regGo(func() error { return EnrolAtRegistrationTx(ctx, tx2, idB, spelledB, nil) })
	regReleaseOnExit(t, tx1, asyncB)
	blocked, _ := regAwait(t, probe, key, asyncB)
	require.True(t, blocked, "the second spelling must wait on the first")

	require.NoError(t, tx1.Rollback())
	<-asyncB.done
	require.NoError(t, asyncB.err)
	regRequireMatureUser(t, beforeB, regReadUser(t, tx2, idB), regTxNow(t, tx2))
}

// Ruling 14 (M1): lockAddressesTx orders by the NORMALIZED key. Two spellings of one
// address plus a second address lock exactly two keys, the smaller normalized key first,
// even when the raw spellings sort the other way round (a space sorts before every letter
// in byte order). Observed through a second transaction that holds the larger key: the
// locker must already hold the smaller one while it waits on the larger.
func TestLockAddressesTx_OrdersByNormalizedKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), regWaitBound)
	t.Cleanup(cancel)
	db := snapDB(t)
	probe := regProbe(t)

	suffix := "-" + uuid.NewString()[:8] + "@example.test"
	small, large := "a"+suffix, "b"+suffix // normalized, small < large
	addrs := []string{" " + strings.ToUpper(large) + " ", large, small}
	rawFirst := slices.Min(addrs)
	require.Equal(t, addrs[0], rawFirst, "fixture: the raw order puts a spelling of the larger address first")
	require.Equal(t, large, strings.ToLower(strings.TrimSpace(addrs[0])), "fixture: that spelling is the larger address")

	smallClass, smallObj := testdb.AdvisoryKeyHalves(regAdvisoryKey(t, probe, small))
	largeKey := regAdvisoryKey(t, probe, large)
	largeClass, largeObj := testdb.AdvisoryKeyHalves(largeKey)

	locker := regBegin(t, db)
	holder, err := probe.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, largeKey)
	require.NoError(t, err)

	a := regGo(func() error { return lockAddressesTx(ctx, locker, addrs) })
	regReleaseOnExit(t, holder, a)
	blocked, _ := regAwait(t, probe, largeKey, a)
	require.True(t, blocked, "the locker waits on the larger key")

	// While it waits it already holds the smaller key, and nothing else.
	var heldSmall, heldAll int
	require.NoError(t, probe.QueryRowContext(ctx,
		`SELECT count(*) FILTER (WHERE classid::bigint = $1 AND objid::bigint = $2), count(*)
		   FROM pg_locks
		  WHERE locktype = 'advisory' AND granted
		    AND pid = (SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
		                AND classid::bigint = $3 AND objid::bigint = $4 LIMIT 1)`,
		smallClass, smallObj, largeClass, largeObj,
	).Scan(&heldSmall, &heldAll))
	assert.Equal(t, 1, heldSmall, "the smaller normalized key is taken first")
	assert.Equal(t, 1, heldAll, "and it is the only key held while waiting")

	require.NoError(t, holder.Rollback())
	<-a.done
	require.NoError(t, a.err)

	var total int
	require.NoError(t, locker.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted`).Scan(&total))
	assert.Equal(t, 2, total, "exactly two keys: the two spellings of one address are one lock")
}

func TestEnrolAtRegistration_ClaimsNoBarrier(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	tx := regBegin(t, db)

	email := regEmail()
	id := regInsertUser(t, tx, email)
	regRequireImmature(t, regReadUser(t, tx, id))

	require.NoError(t, EnrolAtRegistrationTx(ctx, tx, id, email, nil))

	// R5g: the stamp claims no loginfamily authority, so the caller can still take the
	// barrier once on this transaction (a second claim would be refused).
	_, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
}

func TestEnrolAtRegistration_LeavesLockTimeoutToTheCaller(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	tx := regBegin(t, db)

	email := regEmail()
	id := regInsertUser(t, tx, email)
	_, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '7s'`)
	require.NoError(t, err)

	require.NoError(t, EnrolAtRegistrationTx(ctx, tx, id, email, nil))

	var got string
	require.NoError(t, tx.QueryRowContext(ctx, `SHOW lock_timeout`).Scan(&got))
	assert.Equal(t, "7s", got, "the caller owns the transaction's timeouts")
}

func TestEnrolAtRegistration_MismatchedEmailIsContract(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	tx := regBegin(t, db)

	email := regEmail()
	id := regInsertUser(t, tx, email)
	sso := regInsertSSO(t, tx, id, "google")
	userBefore := regReadUser(t, tx, id)
	ssoBefore := regReadSSO(t, tx, sso)
	regRequireImmature(t, userBefore)
	regRequireSSOImmature(t, ssoBefore)
	others := regOtherColumns(t, tx, id)

	foreign := regEmail()
	err := EnrolAtRegistrationTx(ctx, tx, id, foreign, &sso)

	require.ErrorIs(t, err, ErrContract)
	status, _ := HTTPStatus(err)
	assert.Equal(t, 500, status)
	assert.NotContains(t, err.Error(), foreign, "the error names no address")
	assert.NotContains(t, err.Error(), email, "the error names no address")
	assert.Equal(t, userBefore, regReadUser(t, tx, id), "nothing written to users")
	assert.Equal(t, ssoBefore, regReadSSO(t, tx, sso), "nothing written to the SSO identity")
	assert.Equal(t, others, regOtherColumns(t, tx, id))
}

func TestEnrolAtRegistration_UnknownUserIsContract(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	tx := regBegin(t, db)

	email := regEmail()
	id := regInsertUser(t, tx, email)
	before := regReadUser(t, tx, id)
	regRequireImmature(t, before)

	err := EnrolAtRegistrationTx(ctx, tx, uuid.New(), email, nil)

	require.ErrorIs(t, err, ErrContract)
	assert.Equal(t, before, regReadUser(t, tx, id), "another account's row is not stamped")
}

func TestEnrolAtRegistration_ForeignSSOIdentityIsContract(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)

	cases := []struct {
		name string
		pick func(owned, foreign uuid.UUID) uuid.UUID
	}{
		{name: "an identity of another user", pick: func(_, foreign uuid.UUID) uuid.UUID { return foreign }},
		{name: "an identity that does not exist", pick: func(_, _ uuid.UUID) uuid.UUID { return uuid.New() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := regBegin(t, db)
			email := regEmail()
			id := regInsertUser(t, tx, email)
			ownedSSO := regInsertSSO(t, tx, id, "google")
			stranger := regInsertUser(t, tx, regEmail())
			foreignSSO := regInsertSSO(t, tx, stranger, "google")

			userBefore := regReadUser(t, tx, id)
			ownedBefore := regReadSSO(t, tx, ownedSSO)
			foreignBefore := regReadSSO(t, tx, foreignSSO)
			regRequireImmature(t, userBefore)
			regRequireSSOImmature(t, ownedBefore)
			regRequireSSOImmature(t, foreignBefore)

			// The caller's own rollback point, taken after its INSERTs: any error leaves the
			// transaction unusable for commit, and the caller rolls back to here (or all the
			// way). The users stamp may have run before the identity step failed.
			_, err := tx.ExecContext(ctx, `SAVEPOINT caller_rollback`)
			require.NoError(t, err)

			target := tc.pick(ownedSSO, foreignSSO)
			err = EnrolAtRegistrationTx(ctx, tx, id, email, &target)
			require.ErrorIs(t, err, ErrContract)

			_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT caller_rollback`)
			require.NoError(t, err)
			assert.Equal(t, userBefore, regReadUser(t, tx, id), "after the caller rolls back, the users row is unchanged and immature")
			assert.Equal(t, foreignBefore, regReadSSO(t, tx, foreignSSO), "another user's identity is never stamped")
			assert.Equal(t, ownedBefore, regReadSSO(t, tx, ownedSSO), "the user's own other identity is not stamped")
		})
	}
}

func TestEnrolAtRegistration_NoSSOLeavesIdentitiesUntouched(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	tx := regBegin(t, db)

	email := regEmail()
	id := regInsertUser(t, tx, email)
	sso := regInsertSSO(t, tx, id, "google")
	userBefore := regReadUser(t, tx, id)
	ssoBefore := regReadSSO(t, tx, sso)
	regRequireImmature(t, userBefore)
	regRequireSSOImmature(t, ssoBefore)

	require.NoError(t, EnrolAtRegistrationTx(ctx, tx, id, email, nil))

	regRequireMatureUser(t, userBefore, regReadUser(t, tx, id), regTxNow(t, tx))
	assert.Equal(t, ssoBefore, regReadSSO(t, tx, sso), "no identity id given, no identity written")
}

// A driver error is wrapped with withhold (runner ruling 31): the message carries a code,
// never the driver's text, which can name the statement or a value.
func TestEnrolAtRegistration_DriverErrorIsWithheld(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	tx := regBegin(t, db)

	email := regEmail()
	id := regInsertUser(t, tx, email)
	require.NoError(t, tx.Rollback())

	err := EnrolAtRegistrationTx(ctx, tx, id, email, nil)

	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrContract), "a dead transaction is not a contract breach")
	assert.False(t, errors.Is(err, ErrActivated))
	var w *withheld
	require.ErrorAs(t, err, &w, "the driver error is wrapped with withhold")
	assert.Contains(t, err.Error(), "cause withheld")
	assert.NotContains(t, err.Error(), email)
}

// Ruling 17 (M4): a driver error from either stamp is withheld. A test-only BEFORE UPDATE
// trigger, created in the test's own transaction and rolled back with it, raises an
// exception whose message carries a marker; the marker must not reach err.Error(), while
// the cause stays reachable for the caller to classify.
func TestEnrolAtRegistration_StampDriverErrorsAreWithheld(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)

	cases := []struct {
		name  string
		table string
		step  string
	}{
		{name: "the users stamp", table: "users", step: "stamp credentials"},
		{name: "the identity stamp", table: "user_sso_identities", step: "stamp sso identity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := regBegin(t, db)
			email := regEmail()
			id := regInsertUser(t, tx, email)
			sso := regInsertSSO(t, tx, id, "google")
			regRequireImmature(t, regReadUser(t, tx, id))
			regRequireSSOImmature(t, regReadSSO(t, tx, sso))

			tag := strings.ReplaceAll(uuid.NewString(), "-", "")
			marker := "REGMARKER" + tag
			fn := "reg_marker_fn_" + tag
			_, err := tx.ExecContext(ctx, "CREATE FUNCTION "+fn+"() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN RAISE EXCEPTION '"+marker+"'; END $f$")
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, "CREATE TRIGGER reg_marker_trg BEFORE UPDATE ON "+tc.table+" FOR EACH ROW EXECUTE FUNCTION "+fn+"()") //nolint:gosec // DDL cannot be parameterized; table is a literal from the case list, fn a uuid-suffixed name
			require.NoError(t, err)

			err = EnrolAtRegistrationTx(ctx, tx, id, email, &sso)

			require.Error(t, err)
			assert.False(t, errors.Is(err, ErrContract), "a driver failure is not a contract breach")
			assert.NotContains(t, err.Error(), marker, "the driver's text never reaches the error")
			assert.NotContains(t, err.Error(), email)
			assert.Contains(t, err.Error(), tc.step, "the fixed step name is kept")
			assert.Contains(t, err.Error(), "cause withheld")
			var w *withheld
			require.ErrorAs(t, err, &w)
			var pqErr *pq.Error
			require.ErrorAs(t, err, &pqErr, "control: the trigger really raised, and the cause is still reachable")
			assert.Contains(t, pqErr.Message, marker)
		})
	}
}

// ---------------------------------------------------------------------------
// Red-team fix round (#3680 PR3): RT-5 fresh-row rule, RT-3 address normalizer
// ---------------------------------------------------------------------------

// RT-5 (CWE-697): an UPDATE gives the new row version the updating transaction's xid, so
// xmin alone also matches an existing account the caller merely touched. Fresh means INSERTED
// by this transaction, proven by xmin and created_at together. Each case commits an account,
// touches it in the caller's transaction, and expects ErrContract with every stamp unchanged.
func TestEnrolAtRegistration_ExistingAccountTheTransactionTouchedIsContract(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	probe := regProbe(t)

	cases := []struct {
		name  string
		touch func(tx *sql.Tx, id uuid.UUID, email, username string) error
	}{
		{"a non-material touch of the users row", func(tx *sql.Tx, id uuid.UUID, _, _ string) error {
			_, err := tx.ExecContext(ctx, `UPDATE users SET email_verified = email_verified WHERE id = $1`, id)
			return err
		}},
		{"a password change, which the 000189 trigger re-stamps immature", func(tx *sql.Tx, id uuid.UUID, _, _ string) error {
			_, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = 'changed-in-tx' WHERE id = $1`, id) // pragma: allowlist secret
			return err
		}},
		{"an INSERT ... ON CONFLICT DO UPDATE that a caller takes for an insert", func(tx *sql.Tx, id uuid.UUID, email, username string) error {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO users (id, email, username, password_hash, email_verified, age_verified)
				 VALUES ($1, $2, $3, 'x', TRUE, TRUE)
				 ON CONFLICT (id) DO UPDATE SET email_verified = EXCLUDED.email_verified`, id, email, username)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			email := regEmail()
			setup := regBegin(t, db)
			id := regInsertUser(t, setup, email)
			sso := regInsertSSO(t, setup, id, "google")
			require.NoError(t, setup.Commit())
			t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id) })

			userBefore := regReadUser(t, probe, id)
			ssoBefore := regReadSSO(t, probe, sso)
			regRequireImmature(t, userBefore)
			regRequireSSOImmature(t, ssoBefore)

			tx := regBegin(t, db)
			require.NoError(t, tc.touch(tx, id, email, userBefore.Username))
			_, err := tx.ExecContext(ctx, `UPDATE user_sso_identities SET last_used_at = now() WHERE id = $1`, sso)
			require.NoError(t, err)

			err = EnrolAtRegistrationTx(ctx, tx, id, email, &sso)

			require.ErrorIs(t, err, ErrContract, "an existing committed account passes the fresh-row rule")
			status, _ := HTTPStatus(err)
			assert.Equal(t, 500, status)
			assert.NotContains(t, err.Error(), email, "the error names no address")
			assert.NotContains(t, err.Error(), id.String(), "the error names no id")
			require.NoError(t, tx.Rollback())
			after := regReadUser(t, probe, id)
			assert.True(t, after.PwMatures.After(time.Now().Add(regImmatureFloor)), "the password stays immature, got %v", after.PwMatures)
			assert.True(t, after.EmMatures.After(time.Now().Add(regImmatureFloor)), "the email stays immature, got %v", after.EmMatures)
			assert.Equal(t, ssoBefore, regReadSSO(t, probe, sso), "the existing identity stays immature")
		})
	}
}

// RT-5, SSO half: the users row is this transaction's own insert, so the users stamp is
// legitimate and only the identity statement can refuse. The identity is an existing committed
// one of another account that the caller's transaction re-pointed at the new user.
func TestEnrolAtRegistration_ExistingSSOIdentityTheTransactionTouchedIsContract(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	probe := regProbe(t)

	setup := regBegin(t, db)
	owner := regInsertUser(t, setup, regEmail())
	sso := regInsertSSO(t, setup, owner, "google")
	require.NoError(t, setup.Commit())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, owner) })
	ssoBefore := regReadSSO(t, probe, sso)
	regRequireSSOImmature(t, ssoBefore)

	tx := regBegin(t, db)
	email := regEmail()
	id := regInsertUser(t, tx, email)
	regRequireImmature(t, regReadUser(t, tx, id))
	_, err := tx.ExecContext(ctx, `UPDATE user_sso_identities SET user_id = $1 WHERE id = $2`, id, sso)
	require.NoError(t, err)

	err = EnrolAtRegistrationTx(ctx, tx, id, email, &sso)

	require.ErrorIs(t, err, ErrContract, "an existing identity the transaction only touched passes the fresh-row rule")
	assert.ErrorContains(t, err, "stamp sso identity", "the identity statement is what refused")
	require.NoError(t, tx.Rollback())
	assert.Equal(t, ssoBefore, regReadSSO(t, probe, sso), "the existing identity stays as it was")
}

// RT-5 companion (legitimate path): users.created_at is TIMESTAMP (no zone) and
// user_sso_identities.created_at is TIMESTAMPTZ, so a created_at comparison written in the wrong
// type breaks under a non-UTC session zone. A registration must still stamp under any zone.
func TestEnrolAtRegistration_StampsUnderANonUTCSessionZone(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)

	for _, zone := range []string{"America/New_York", "Asia/Kolkata", "Pacific/Kiritimati", "Pacific/Pago_Pago"} {
		t.Run(zone, func(t *testing.T) {
			tx := regBegin(t, db)
			// set_config(..., true) is SET LOCAL: it ends with the transaction.
			_, err := tx.ExecContext(ctx, `SELECT set_config('timezone', $1, true)`, zone)
			require.NoError(t, err)
			var got string
			require.NoError(t, tx.QueryRowContext(ctx, `SHOW timezone`).Scan(&got))
			require.Equal(t, zone, got, "fixture: the session zone is not UTC")

			email := regEmail()
			id := regInsertUser(t, tx, email)
			sso := regInsertSSO(t, tx, id, "google")
			userBefore := regReadUser(t, tx, id)
			ssoBefore := regReadSSO(t, tx, sso)
			regRequireImmature(t, userBefore)
			regRequireSSOImmature(t, ssoBefore)

			require.NoError(t, EnrolAtRegistrationTx(ctx, tx, id, email, &sso))

			txNow := regTxNow(t, tx)
			regRequireMatureUser(t, userBefore, regReadUser(t, tx, id), txNow)
			regRequireMatureSSO(t, ssoBefore, regReadSSO(t, tx, sso), txNow)
		})
	}
}

// regUnicodeSpellings are spellings of base that the application's normalizer
// (strings.ToLower(strings.TrimSpace(...))) folds to base and PostgreSQL's one-argument btrim
// does not: NBSP, tab, IDEOGRAPHIC SPACE and LF. The runes are built, never escaped in source.
func regUnicodeSpellings(base string) []struct{ name, spelling string } {
	return []struct{ name, spelling string }{
		{"trailing no-break space U+00A0", base + string(rune(0x00A0))},
		{"leading tab", "\t" + base},
		{"trailing ideographic space U+3000", base + string(rune(0x3000))},
		{"trailing line feed", base + "\n"},
		{"upper-cased with padding of two kinds", "\t" + strings.ToUpper(base) + string(rune(0x00A0))},
	}
}

// regLockCtx bounds a lock call in the RT-3 tests: an advisory-lock wait ends with the context,
// so a spelling that collides with a live holder fails the test in seconds instead of hanging.
func regLockCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// regAdvisoryHeld counts the advisory locks this backend holds, and how many of them are key.
func regAdvisoryHeld(t *testing.T, tx *sql.Tx, key int64) (total, matching int) {
	t.Helper()
	classID, objID := testdb.AdvisoryKeyHalves(key)
	require.NoError(t, tx.QueryRowContext(context.Background(),
		`SELECT count(*), count(*) FILTER (WHERE classid::bigint = $1 AND objid::bigint = $2)
		   FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted`,
		classID, objID).Scan(&total, &matching))
	return total, matching
}

// RT-3 (CWE-1289): the address lock key must fold an address the way the application stores and
// resolves it, so every spelling of one address takes ONE advisory key.
func TestLockAddressTx_UnicodeWhitespaceSpellingsTakeTheCanonicalKey(t *testing.T) {
	ctx := context.Background()
	db := snapDB(t)
	probe := regProbe(t)
	base := regEmail()
	key := regAdvisoryKey(t, probe, base)

	// Control: the canonical spelling takes exactly that key.
	ctl := regBegin(t, db)
	require.NoError(t, lockAddressTx(ctx, ctl, base))
	total, matching := regAdvisoryHeld(t, ctl, key)
	require.Equal(t, 1, total, "control: one key")
	require.Equal(t, 1, matching, "control: the canonical key")
	// End the holder before the subtests: once every spelling folds to the canonical key, a live
	// holder would make each subtest wait on it. Every lock call below is bounded as well, so a
	// regression that makes spellings collide while a holder lives fails fast and never hangs.
	require.NoError(t, ctl.Rollback())

	for _, x := range regUnicodeSpellings(base) {
		t.Run(x.name, func(t *testing.T) {
			lockCtx := regLockCtx(t)
			tx := regBegin(t, db)
			require.NoError(t, lockAddressTx(lockCtx, tx, x.spelling))
			total, matching := regAdvisoryHeld(t, tx, key)
			assert.Equal(t, 1, total, "one advisory key")
			assert.Equal(t, 1, matching, "and it is the canonical address's key")
		})
	}
}

func TestLockAddressesTx_UnicodeWhitespaceSpellingsAreOneLock(t *testing.T) {
	db := snapDB(t)
	probe := regProbe(t)
	base := regEmail()
	other := regEmail()
	baseKey := regAdvisoryKey(t, probe, base)
	otherKey := regAdvisoryKey(t, probe, other)

	variants := regUnicodeSpellings(base)
	spellings := make([]string, 0, len(variants)+1)
	spellings = append(spellings, base)
	for _, x := range variants {
		spellings = append(spellings, x.spelling)
	}

	t.Run("every spelling of one address is one key", func(t *testing.T) {
		tx := regBegin(t, db)
		require.NoError(t, lockAddressesTx(regLockCtx(t), tx, spellings))
		total, matching := regAdvisoryHeld(t, tx, baseKey)
		assert.Equal(t, 1, total, "count in pg_locks: one advisory key")
		assert.Equal(t, 1, matching)
	})

	t.Run("a second address still takes its own key", func(t *testing.T) {
		tx := regBegin(t, db)
		require.NoError(t, lockAddressesTx(regLockCtx(t), tx, append(slices.Clone(spellings), other)))
		total, matching := regAdvisoryHeld(t, tx, baseKey)
		assert.Equal(t, 2, total, "the address and the second address: two keys")
		assert.Equal(t, 1, matching)
		_, matchingOther := regAdvisoryHeld(t, tx, otherKey)
		assert.Equal(t, 1, matchingOther, "the second address's own key")
	})
}

// Legitimate path, green before and after RT-3: two ASCII addresses are two keys.
func TestLockAddressesTx_DistinctAddressesTakeTheirOwnKeys(t *testing.T) {
	db := snapDB(t)
	probe := regProbe(t)
	first, second := regEmail(), regEmail()

	tx := regBegin(t, db)
	require.NoError(t, lockAddressesTx(regLockCtx(t), tx, []string{first, "  " + strings.ToUpper(second) + " ", first}))

	total, matchingFirst := regAdvisoryHeld(t, tx, regAdvisoryKey(t, probe, first))
	_, matchingSecond := regAdvisoryHeld(t, tx, regAdvisoryKey(t, probe, second))
	assert.Equal(t, 2, total)
	assert.Equal(t, 1, matchingFirst)
	assert.Equal(t, 1, matchingSecond)
}

// RT-3, registration guard: the stored email is the normalized address, and the caller may pass
// any spelling the application folds to it, so the guard accepts each Unicode-whitespace
// spelling of the stored address and the lock it takes is the canonical key.
func TestEnrolAtRegistration_AcceptsUnicodeWhitespaceSpellingOfTheStoredAddress(t *testing.T) {
	db := snapDB(t)
	probe := regProbe(t)

	for i, x := range regUnicodeSpellings("") {
		t.Run(x.name, func(t *testing.T) {
			stored := regEmail()
			spelling := regUnicodeSpellings(stored)[i].spelling
			require.Equal(t, stored, strings.ToLower(strings.TrimSpace(spelling)), "fixture: the application folds it to the stored address")

			tx := regBegin(t, db)
			id := regInsertUser(t, tx, stored)
			before := regReadUser(t, tx, id)
			regRequireImmature(t, before)

			require.NoError(t, EnrolAtRegistrationTx(regLockCtx(t), tx, id, spelling, nil), "the guard accepts that spelling of the stored address")

			regRequireMatureUser(t, before, regReadUser(t, tx, id), regTxNow(t, tx))
			total, matching := regAdvisoryHeld(t, tx, regAdvisoryKey(t, probe, stored))
			assert.Equal(t, 1, total)
			assert.Equal(t, 1, matching, "the lock is the canonical address's key")
		})
	}
}
