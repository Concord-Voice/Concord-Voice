package credepoch

import (
	"context"
	"database/sql"
	"io"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Package-local coverage for the DB half of the fence: read-through +
// back-fill, the Redis-transport-error fallback, GuardTx, and RotateTx.
// (Go coverage is per-package: the cross-package integration suites exercise
// these paths but do not count toward credepoch.go's own coverage.)

func newDBFence(t *testing.T) (*Fence, *miniredis.Miniredis, *sql.DB, string) {
	t.Helper()
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := dbtest.CreateUser(t, db)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(db, rdb, logger.NewWithWriter(io.Discard)), mr, db, userID.String()
}

func deadRedisFence(t *testing.T, db RowQuerier) *Fence {
	t.Helper()
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // nothing listens
	t.Cleanup(func() { _ = dead.Close() })
	return New(db, dead, logger.NewWithWriter(io.Discard))
}

func TestCheck_ReadThroughAndBackfill(t *testing.T) {
	f, mr, db, uid := newDBFence(t)
	ctx := context.Background()

	t.Run("DB NULL caches none and admits", func(t *testing.T) {
		require.NoError(t, f.Check(ctx, uid, ""))
		val, err := mr.Get(Key(uid))
		require.NoError(t, err)
		assert.Equal(t, "none", val)
	})

	t.Run("DB epoch caches active and enforces", func(t *testing.T) {
		_, err := db.Exec(`UPDATE users SET credential_epoch = 'dbepochX' WHERE id = $1`, uid)
		require.NoError(t, err)
		mr.Del(Key(uid))

		require.NoError(t, f.Check(ctx, uid, "dbepochX"))
		val, err := mr.Get(Key(uid))
		require.NoError(t, err)
		assert.Equal(t, "active:dbepochX", val)

		assert.ErrorIs(t, f.Check(ctx, uid, "stale"), ErrEpochMismatch)
		assert.ErrorIs(t, f.Check(ctx, uid, ""), ErrEpochMismatch)
	})

	t.Run("unrecognized cache value re-derives from the DB", func(t *testing.T) {
		require.NoError(t, mr.Set(Key(uid), "garbage-value"))
		require.NoError(t, f.Check(ctx, uid, "dbepochX"))
	})

	t.Run("unknown user has no epoch marker", func(t *testing.T) {
		require.NoError(t, f.Check(ctx, "00000000-0000-0000-0000-00000000dead", ""))
	})
}

func TestCheck_RedisTransportErrorFallsBackToDB(t *testing.T) {
	_, _, db, uid := newDBFence(t)
	f := deadRedisFence(t, db)
	ctx := context.Background()

	_, err := db.Exec(`UPDATE users SET credential_epoch = 'fallbackE' WHERE id = $1`, uid)
	require.NoError(t, err)

	assert.NoError(t, f.Check(ctx, uid, "fallbackE"))
	assert.ErrorIs(t, f.Check(ctx, uid, "stale"), ErrEpochMismatch)
}

// brokenDB always errors — the both-stores-down fail-closed case.
type brokenDB struct{ db *sql.DB }

func (b brokenDB) QueryRowContext(ctx context.Context, _ string, args ...any) *sql.Row {
	return b.db.QueryRowContext(ctx, `SELECT credential_epoch FROM no_such_table WHERE id = $1`, args...)
}

func TestCheck_BothStoresDownFailsClosed(t *testing.T) {
	_, _, db, uid := newDBFence(t)
	f := deadRedisFence(t, brokenDB{db: db})
	assert.ErrorIs(t, f.Check(context.Background(), uid, "anything"), ErrUnavailable)
}

func TestCheck_ReadThroughDBErrorFailsClosed(t *testing.T) {
	// Redis answers (miss), DB errors → ErrUnavailable.
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	uid := dbtest.CreateUser(t, db)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	f := New(brokenDB{db: db}, rdb, logger.NewWithWriter(io.Discard))
	assert.ErrorIs(t, f.Check(context.Background(), uid.String(), ""), ErrUnavailable)
}

func TestGuardTx_LifecycleAgainstRealRows(t *testing.T) {
	_, _, db, uid := newDBFence(t)
	ctx := context.Background()

	t.Run("no epoch marker admits any claim", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		assert.NoError(t, GuardTx(ctx, tx, uid, ""))
		assert.NoError(t, GuardTx(ctx, tx, uid, "whatever"))
	})

	t.Run("after rotation only the matching claim passes", func(t *testing.T) {
		_, err := db.Exec(`UPDATE users SET credential_epoch = 'guardE' WHERE id = $1`, uid)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		assert.NoError(t, GuardTx(ctx, tx, uid, "guardE"))
		assert.ErrorIs(t, GuardTx(ctx, tx, uid, ""), ErrEpochMismatch)
		assert.ErrorIs(t, GuardTx(ctx, tx, uid, "old"), ErrEpochMismatch)
	})

	t.Run("guard read error propagates", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		assert.Error(t, GuardTx(ctx, brokenDB{db: db}, uid, "guardE"))
	})
}

func TestOp_RotateTxStampsDurableEpoch(t *testing.T) {
	f, mr, db, uid := newDBFence(t)
	ctx := context.Background()

	op, err := f.Begin(ctx, uid)
	require.NoError(t, err)
	val, err := mr.Get(Key(uid))
	require.NoError(t, err)
	assert.Contains(t, val, "blocked:")

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, op.RotateTx(ctx, tx))
	require.NoError(t, tx.Commit())

	var stored sql.NullString
	require.NoError(t, db.QueryRow(`SELECT credential_epoch FROM users WHERE id = $1`, uid).Scan(&stored))
	require.True(t, stored.Valid)
	assert.Equal(t, op.NewEpochValue(), stored.String)

	op.Commit(ctx)
	require.NoError(t, f.Check(ctx, uid, op.NewEpochValue()))
	assert.ErrorIs(t, f.Check(ctx, uid, "pre-rotation"), ErrEpochMismatch)
}

func TestOp_RotateTxUnknownUserErrors(t *testing.T) {
	f, _, db, _ := newDBFence(t)
	ctx := context.Background()
	op, err := f.Begin(ctx, "00000000-0000-0000-0000-00000000beef")
	require.NoError(t, err)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	assert.Error(t, op.RotateTx(ctx, tx), "rotating a missing user row must error (RETURNING id scans nothing)")
}

func TestBeginTolleratesDeadRedisAndOpsAreSafe(t *testing.T) {
	// Begin/Commit/Rollback against a dead Redis log-and-tolerate: an outage
	// must not block account recovery (the durable fence still applies).
	_, _, db, uid := newDBFence(t)
	f := deadRedisFence(t, db)
	ctx := context.Background()

	op, err := f.Begin(ctx, uid)
	require.NoError(t, err, "Begin must tolerate a Redis outage")
	op.Commit(ctx)   // must not panic
	op.Rollback(ctx) // must not panic

	// And the backfill path with a dead Redis inside authoritativeEpoch is
	// unreachable (transport error → direct DB read), covered above.
	_ = time.Now()
}
