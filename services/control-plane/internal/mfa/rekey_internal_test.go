package mfa

// Internal-package (package mfa) pins for resealRowTx, rekeyRow and
// fetchPendingBatch. rekey_test.go is package mfa_test and cannot reach the
// unexported functions. Most arms need no database: a cancelled context fails
// BeginTx before any connection is dialed, and a stub driver whose connection
// rejects or answers statements on demand reaches the failure arms
// deterministically. The CAS pin at the end runs against a real database.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errRekeyStubExec     = errors.New("stub exec failure")
	errRekeyStubRollback = errors.New("stub rollback failure")
	errRekeyStubClose    = errors.New("stub rows close failure")
)

// rekeyStub is the state one stub database shares across its connection,
// transaction and rows.
type rekeyStub struct {
	execErr     error // returned by every Exec; nil lets it succeed
	rollbackErr error // returned by Tx.Rollback
	closeErr    error // returned by Rows.Close
	// What a query returns: the columns, then the data rows (none by default).
	columns []string
	data    [][]driver.Value

	rolledBack bool
	lastQuery  string
	lastArgs   []driver.NamedValue
}

func (s *rekeyStub) open(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(rekeyStubConnector{s})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type rekeyStubConnector struct{ s *rekeyStub }

func (c rekeyStubConnector) Connect(context.Context) (driver.Conn, error) {
	return rekeyStubConn(c), nil
}
func (rekeyStubConnector) Driver() driver.Driver { return nil }

type rekeyStubConn struct{ s *rekeyStub }

func (rekeyStubConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (rekeyStubConn) Close() error                        { return nil }
func (c rekeyStubConn) Begin() (driver.Tx, error)         { return rekeyStubTx(c), nil }
func (c rekeyStubConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return rekeyStubTx(c), nil
}
func (c rekeyStubConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	if c.s.execErr != nil {
		return nil, c.s.execErr
	}
	return driver.ResultNoRows, nil
}
func (c rekeyStubConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.s.lastQuery, c.s.lastArgs = query, args
	return &rekeyStubRows{s: c.s}, nil
}

type rekeyStubTx struct{ s *rekeyStub }

func (rekeyStubTx) Commit() error { return nil }
func (t rekeyStubTx) Rollback() error {
	t.s.rolledBack = true
	return t.s.rollbackErr
}

type rekeyStubRows struct {
	s   *rekeyStub
	idx int
}

func (r *rekeyStubRows) Columns() []string { return r.s.columns }
func (r *rekeyStubRows) Close() error      { return r.s.closeErr }
func (r *rekeyStubRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.s.data) {
		return io.EOF
	}
	copy(dest, r.s.data[r.idx])
	r.idx++
	return nil
}

func TestRekey_ResealRowTx_BeginFailureReturnsZeroAndWrapsCause(t *testing.T) {
	s := &rekeyStub{}
	db := s.open(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	n, err := resealRowTx(ctx, db, pendingRow{userID: "u", version: 1}, []byte("enc"), []byte("nonce"), 2)

	require.Error(t, err)
	assert.Zero(t, n)
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, "begin row transaction")
	assert.False(t, s.rolledBack, "no transaction may have been opened")
}

func TestRekey_ResealRowTx_SetConfigFailureReturnsZeroAndRollsBack(t *testing.T) {
	s := &rekeyStub{execErr: errRekeyStubExec}
	db := s.open(t)

	n, err := resealRowTx(context.Background(), db, pendingRow{userID: "u", version: 1}, []byte("enc"), []byte("nonce"), 2)

	require.Error(t, err)
	assert.Zero(t, n)
	assert.ErrorIs(t, err, errRekeyStubExec)
	assert.ErrorContains(t, err, "set rewrap flag")
	assert.True(t, s.rolledBack, "the deferred rollback must discard the doomed transaction")
	assert.NotContains(t, err.Error(), "rollback row transaction", "a clean rollback adds nothing to the error")
}

// A rollback that itself fails is joined to the error that caused it: both
// sentinels stay reachable and the text names the rollback.
func TestRekey_ResealRowTx_RollbackFailureIsJoinedToTheCause(t *testing.T) {
	s := &rekeyStub{execErr: errRekeyStubExec, rollbackErr: errRekeyStubRollback}
	db := s.open(t)

	n, err := resealRowTx(context.Background(), db, pendingRow{userID: "u", version: 1}, []byte("enc"), []byte("nonce"), 2)

	require.Error(t, err)
	assert.Zero(t, n)
	assert.ErrorIs(t, err, errRekeyStubExec, "the original failure must stay reachable")
	assert.ErrorIs(t, err, errRekeyStubRollback, "the rollback failure must be joined, not dropped")
	assert.ErrorContains(t, err, "set rewrap flag")
	assert.ErrorContains(t, err, "rollback row transaction")
}

// sql.ErrTxDone from the deferred rollback means the transaction already ended;
// it is not a failure and must not be joined.
func TestRekey_ResealRowTx_TxDoneRollbackIsNotJoined(t *testing.T) {
	s := &rekeyStub{execErr: errRekeyStubExec, rollbackErr: sql.ErrTxDone}
	db := s.open(t)

	_, err := resealRowTx(context.Background(), db, pendingRow{userID: "u", version: 1}, []byte("enc"), []byte("nonce"), 2)

	require.Error(t, err)
	assert.ErrorIs(t, err, errRekeyStubExec)
	assert.NotContains(t, err.Error(), "rollback row transaction")
}

// The CAS lost (no row came back): the row is skipped, nothing is an error, the
// transaction is rolled back, and the statement carried the material_version the
// batch read as its seventh argument.
func TestRekey_ResealRowTx_CASLossReturnsZeroWithoutError(t *testing.T) {
	s := &rekeyStub{columns: []string{"material_version"}}
	db := s.open(t)
	r := pendingRow{userID: "u", version: 1, enc: []byte("old"), materialVersion: 41}

	n, err := resealRowTx(context.Background(), db, r, []byte("enc"), []byte("nonce"), 2)

	require.NoError(t, err)
	assert.Zero(t, n)
	assert.True(t, s.rolledBack, "an uncommitted transaction must be rolled back")
	assert.Contains(t, s.lastQuery, "material_version = $7")
	require.Len(t, s.lastArgs, 7)
	assert.Equal(t, int64(41), s.lastArgs[6].Value, "the CAS must pin the material_version the batch read")
}

func TestRekey_ResealRowTx_VersionChangedAbortsBeforeCommit(t *testing.T) {
	// The row came back with a different material_version: the trigger did not
	// apply the rewrap exemption.
	s := &rekeyStub{columns: []string{"material_version"}, data: [][]driver.Value{{int64(99)}}}
	db := s.open(t)
	r := pendingRow{userID: "u", version: 1, materialVersion: 41}

	n, err := resealRowTx(context.Background(), db, r, []byte("enc"), []byte("nonce"), 2)

	require.Error(t, err)
	assert.Zero(t, n)
	assert.ErrorContains(t, err, "rewrap exemption not applied")
	assert.True(t, s.rolledBack, "the transaction must be rolled back, never committed")
}

func testRekeyRing(t *testing.T) (oldRing, activeRing *Keyring) {
	t.Helper()
	const k1 = "0101010101010101010101010101010101010101010101010101010101010101"
	const k2 = "0202020202020202020202020202020202020202020202020202020202020202"
	var err error
	oldRing, err = ParseKeyring(k1, 1, "")
	require.NoError(t, err)
	activeRing, err = ParseKeyring(k2, 2, "1:"+k1)
	require.NoError(t, err)
	return oldRing, activeRing
}

func sealedPending(t *testing.T, oldRing *Keyring, userID string, materialVersion int64) pendingRow {
	t.Helper()
	enc, nonce, ver, err := oldRing.Seal([]byte("JBSWY3DPEHPK3PXP"))
	require.NoError(t, err)
	return pendingRow{userID: userID, enc: enc, nonce: nonce, version: ver, materialVersion: materialVersion}
}

// An infrastructure failure comes back as rekeyAborted (never as a re-sealed
// row), with no per-row failure record, and the error names the row.
func TestRekey_RekeyRow_InfrastructureFailureIsAbortedAndNamesTheRow(t *testing.T) {
	oldRing, activeRing := testRekeyRing(t)
	s := &rekeyStub{execErr: errRekeyStubExec}
	db := s.open(t)

	outcome, failure, err := rekeyRow(context.Background(), db, activeRing, sealedPending(t, oldRing, "user-7", 5))

	assert.Equal(t, rekeyAborted, outcome)
	assert.NotEqual(t, rekeyDone, outcome)
	assert.Nil(t, failure)
	require.Error(t, err)
	assert.ErrorIs(t, err, errRekeyStubExec)
	assert.ErrorContains(t, err, "reseal user user-7 (key_version 1)")
}

func TestRekey_RekeyRow_CASLossIsSkipped(t *testing.T) {
	oldRing, activeRing := testRekeyRing(t)
	s := &rekeyStub{columns: []string{"material_version"}}
	db := s.open(t)

	outcome, failure, err := rekeyRow(context.Background(), db, activeRing, sealedPending(t, oldRing, "user-8", 5))

	require.NoError(t, err)
	assert.Nil(t, failure)
	assert.Equal(t, rekeySkipped, outcome)
}

func TestRekey_RekeyRow_UndecryptableRowIsAFailureNotAnAbort(t *testing.T) {
	_, activeRing := testRekeyRing(t)
	s := &rekeyStub{}
	db := s.open(t)

	outcome, failure, err := rekeyRow(context.Background(), db, activeRing,
		pendingRow{userID: "user-9", enc: []byte("garbage"), nonce: []byte("nonce"), version: 9})

	require.NoError(t, err)
	assert.Equal(t, rekeyFailedRow, outcome)
	require.NotNil(t, failure)
	assert.Equal(t, "user-9", failure.UserID)
}

// A failure to close the result set is returned with the error that preceded it
// (here a scan error: the stub returns four columns where five are scanned), and
// the batch is dropped.
func TestRekey_FetchPendingBatch_CloseFailureIsJoinedAndDropsTheBatch(t *testing.T) {
	s := &rekeyStub{
		closeErr: errRekeyStubClose,
		columns:  []string{"user_id", "totp_secret_enc", "totp_secret_nonce", "key_version"},
		data:     [][]driver.Value{{"u", []byte("e"), []byte("n"), int64(1)}},
	}
	db := s.open(t)

	batch, err := fetchPendingBatch(context.Background(), db, 2, zeroUUID, 10)

	assert.Nil(t, batch)
	require.Error(t, err)
	assert.ErrorIs(t, err, errRekeyStubClose)
	assert.ErrorContains(t, err, "close pending rows")
	assert.ErrorContains(t, err, "scan pending row")
}

// The CAS pins material_version, against a real database: a confirmation that
// lands between the batch read and the write moves material_version, and the
// stale write must lose without touching the row. With the right version the
// write wins and, being a flagged rewrap, leaves material_version alone.
func TestRekey_ResealRowTx_CASPinsMaterialVersionAgainstADatabase(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	id := uuid.New()
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash) VALUES ($1, $2, $3, 'x')`,
		id, "rekey-cas-"+id.String()+"@example.test", "rc"+id.String()[:8])
	require.NoError(t, err)
	t.Cleanup(func() {
		_, delErr := db.Exec(`DELETE FROM users WHERE id = $1`, id)
		assert.NoError(t, delErr)
	})
	enc := []byte("sealed-under-v1")
	_, err = db.Exec(`
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, 1, FALSE, FALSE)`, id, enc, []byte("nonce-v1"))
	require.NoError(t, err)

	readRow := func() (keyVersion int, materialVersion int64) {
		t.Helper()
		require.NoError(t, db.QueryRow(`SELECT key_version, material_version FROM user_mfa_totp WHERE user_id = $1`, id).Scan(&keyVersion, &materialVersion))
		return keyVersion, materialVersion
	}
	_, batchRead := readRow()

	// A concurrent confirmation: enabled false -> true is a material change.
	_, err = db.Exec(`UPDATE user_mfa_totp SET enabled = TRUE, confirmed = TRUE WHERE user_id = $1`, id)
	require.NoError(t, err)
	_, current := readRow()
	require.NotEqual(t, batchRead, current, "the confirmation must have moved material_version")

	stale := pendingRow{userID: id.String(), enc: enc, version: 1, materialVersion: batchRead}
	n, err := resealRowTx(context.Background(), db, stale, []byte("sealed-under-v2"), []byte("nonce-v2"), 2)
	require.NoError(t, err)
	assert.Zero(t, n, "a write pinned to a stale material_version must lose the CAS")
	keyVersion, materialVersion := readRow()
	assert.Equal(t, 1, keyVersion, "the lost CAS must leave the row alone")
	assert.Equal(t, current, materialVersion)

	fresh := pendingRow{userID: id.String(), enc: enc, version: 1, materialVersion: current}
	n, err = resealRowTx(context.Background(), db, fresh, []byte("sealed-under-v2"), []byte("nonce-v2"), 2)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	keyVersion, materialVersion = readRow()
	assert.Equal(t, 2, keyVersion)
	assert.Equal(t, current, materialVersion, "a flagged rewrap must not move material_version")
}
