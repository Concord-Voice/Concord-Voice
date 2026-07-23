package credepoch

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unit tests cover the pure-cache half of the state machine (no DB hit occurs
// when the cache answers) plus Begin/Commit/Rollback transitions and epoch
// generation. DB read-through, Redis-transport fallback, and GuardTx locking
// need real Postgres and live in the integration suite
// (internal/auth/credential_epoch_integration_test.go, Task 8 of the plan).

type nopLogger struct{}

func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// panicQuerier asserts the DB is never touched on cache-answered paths.
type panicQuerier struct{ t *testing.T }

func (p panicQuerier) QueryRowContext(context.Context, string, ...any) *sql.Row {
	p.t.Fatal("unexpected DB access on a cache-answered path")
	return nil
}

func newFence(t *testing.T) (*Fence, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(panicQuerier{t: t}, rdb, nopLogger{}), mr
}

func TestCheck_CacheActive(t *testing.T) {
	f, mr := newFence(t)
	require.NoError(t, mr.Set(Key("u1"), "active:epochA"))

	t.Run("matching claim admits", func(t *testing.T) {
		assert.NoError(t, f.Check(context.Background(), "u1", "epochA"))
	})
	t.Run("missing claim fails closed after first rotation", func(t *testing.T) {
		assert.ErrorIs(t, f.Check(context.Background(), "u1", ""), ErrEpochMismatch)
	})
	t.Run("mismatched claim fails closed", func(t *testing.T) {
		assert.ErrorIs(t, f.Check(context.Background(), "u1", "epochB"), ErrEpochMismatch)
	})
}

func TestCheck_CacheNone_AdmitsAnyClaim(t *testing.T) {
	f, mr := newFence(t)
	require.NoError(t, mr.Set(Key("u1"), "none"))
	assert.NoError(t, f.Check(context.Background(), "u1", ""))
	// A claim-bearing token against "none" (post-down-migration state) admits.
	assert.NoError(t, f.Check(context.Background(), "u1", "stale"))
}

func TestCheck_CacheBlocked_FailsClosed(t *testing.T) {
	f, mr := newFence(t)
	require.NoError(t, mr.Set(Key("u1"), "blocked:op123"))
	assert.ErrorIs(t, f.Check(context.Background(), "u1", "anything"), ErrBlocked)
	assert.ErrorIs(t, f.Check(context.Background(), "u1", ""), ErrBlocked)
}

func TestBegin_PublishesBlockedMarkerWithTTL(t *testing.T) {
	f, mr := newFence(t)
	op, err := f.Begin(context.Background(), "u1")
	require.NoError(t, err)
	require.NotNil(t, op)

	val, err := mr.Get(Key("u1"))
	require.NoError(t, err)
	assert.Regexp(t, `^blocked:[0-9a-f]{32}$`, val)
	ttl := mr.TTL(Key("u1"))
	assert.Greater(t, ttl, time.Duration(0), "blocked marker must expire (crash reconcile)")
	assert.LessOrEqual(t, ttl, blockedTTL)
}

func TestOp_CommitPublishesActiveEpoch(t *testing.T) {
	f, mr := newFence(t)
	op, err := f.Begin(context.Background(), "u1")
	require.NoError(t, err)

	op.Commit(context.Background())
	val, err := mr.Get(Key("u1"))
	require.NoError(t, err)
	assert.Equal(t, "active:"+op.NewEpochValue(), val)

	// The published epoch now admits exactly itself.
	assert.NoError(t, f.Check(context.Background(), "u1", op.NewEpochValue()))
	assert.ErrorIs(t, f.Check(context.Background(), "u1", "old"), ErrEpochMismatch)
}

func TestOp_RollbackDeletesMarker(t *testing.T) {
	f, mr := newFence(t)
	op, err := f.Begin(context.Background(), "u1")
	require.NoError(t, err)

	op.Rollback(context.Background())
	assert.False(t, mr.Exists(Key("u1")), "rollback must clear the key so read-through restores truth")
}

func TestMatchEpoch(t *testing.T) {
	t.Run("never-rotated (NULL) admits any token", func(t *testing.T) {
		assert.NoError(t, MatchEpoch(sql.NullString{Valid: false}, "anything"))
		assert.NoError(t, MatchEpoch(sql.NullString{Valid: false}, ""))
	})
	t.Run("empty stored epoch admits", func(t *testing.T) {
		assert.NoError(t, MatchEpoch(sql.NullString{Valid: true, String: ""}, "tok"))
	})
	t.Run("matching token admits", func(t *testing.T) {
		assert.NoError(t, MatchEpoch(sql.NullString{Valid: true, String: "epochA"}, "epochA"))
	})
	t.Run("mismatched token fails closed", func(t *testing.T) {
		assert.ErrorIs(t, MatchEpoch(sql.NullString{Valid: true, String: "epochA"}, "epochB"), ErrEpochMismatch)
	})
	t.Run("absent claim after rotation fails closed", func(t *testing.T) {
		assert.ErrorIs(t, MatchEpoch(sql.NullString{Valid: true, String: "epochA"}, ""), ErrEpochMismatch)
	})
}

func TestOp_CommitPublishesActiveWhenMarkerOwned(t *testing.T) {
	// Normal path: our blocked marker is current, so Commit transitions it to
	// active:<newEpoch> (the CAS 'set' branch).
	f, mr := newFence(t)
	op, err := f.Begin(context.Background(), "u1")
	require.NoError(t, err)

	op.Commit(context.Background())
	val, err := mr.Get(Key("u1"))
	require.NoError(t, err)
	assert.Equal(t, "active:"+op.NewEpochValue(), val)
}

func TestOp_CommitClearsStaleNonBlockedValue(t *testing.T) {
	// #2201 review (A): if at Commit time the cache holds a stale, non-blocked
	// value (e.g. a pre-rotation active: because Begin's publish was lost), Commit
	// DELs it — the CAS 'del' branch — so the stale value can't keep admitting the
	// just-revoked token; the next request reads through to the DB's new epoch.
	f, mr := newFence(t)
	op, err := f.Begin(context.Background(), "u1")
	require.NoError(t, err)
	require.NoError(t, mr.Set(Key("u1"), "active:staleEpoch")) // simulate lost marker

	op.Commit(context.Background())
	assert.False(t, mr.Exists(Key("u1")), "a stale non-blocked value must be cleared to force read-through")
}

func TestOp_RollbackPreservesNewerBlockedMarker(t *testing.T) {
	// #2201 review (925 / CodeRabbit CWE-362): op1 rolls back while op2's newer
	// blocked marker occupies the key. rollbackScript DELs only its OWN
	// blocked:<opID>; deleting op2's marker would reopen authentication-level
	// admission (read-through hits the pre-op2 durable epoch) during op2's
	// in-flight window.
	f, mr := newFence(t)
	op1, err := f.Begin(context.Background(), "u1")
	require.NoError(t, err)
	op2, err := f.Begin(context.Background(), "u1") // overwrites the marker with blocked:op2
	require.NoError(t, err)

	op1.Rollback(context.Background())
	val, err := mr.Get(Key("u1"))
	require.NoError(t, err)
	assert.Equal(t, "blocked:"+op2.opID, val, "op1.Rollback must leave op2's blocked marker (stays fail-closed)")
}

func TestOp_CommitDoesNotClobberNewerBlockedMarker(t *testing.T) {
	// #2201 review (925): two destructive flows overlap for one user. op1's Commit
	// must NOT overwrite op2's still-in-flight blocked marker (the CAS 'skip'
	// branch) — doing so would admit tokens under op1's epoch during op2's window.
	f, mr := newFence(t)
	op1, err := f.Begin(context.Background(), "u1")
	require.NoError(t, err)
	op2, err := f.Begin(context.Background(), "u1") // overwrites the marker with blocked:op2
	require.NoError(t, err)

	op1.Commit(context.Background())
	val, err := mr.Get(Key("u1"))
	require.NoError(t, err)
	assert.Equal(t, "blocked:"+op2.opID, val, "op1.Commit must leave op2's blocked marker (stays fail-closed)")
}

func TestOp_CommitToleratesRedisOutage(t *testing.T) {
	// A total Redis outage fails both the active: publish and the fallback DEL;
	// Commit must not panic — the blocked-marker TTL + DB read-through remain the
	// backstop. Exercises the Set-error → DEL fail-path (#2201 review A).
	f, mr := newFence(t)
	op, err := f.Begin(context.Background(), "u1")
	require.NoError(t, err)
	mr.Close() // every subsequent redis op errors

	assert.NotPanics(t, func() { op.Commit(context.Background()) })
}

func TestOp_CommitSurvivesCanceledRequestContext(t *testing.T) {
	// The post-commit publish must not be lost because the HTTP request context
	// was canceled mid-flight (context.WithoutCancel — age-handler template).
	f, mr := newFence(t)
	op, err := f.Begin(context.Background(), "u1")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op.Commit(ctx)

	val, err := mr.Get(Key("u1"))
	require.NoError(t, err)
	assert.Equal(t, "active:"+op.NewEpochValue(), val)
}

func TestNewEpoch_ShapeAndUniqueness(t *testing.T) {
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		e, err := NewEpoch()
		require.NoError(t, err)
		assert.Regexp(t, hex32, e)
		assert.False(t, seen[e], "epochs must be unique")
		seen[e] = true
	}
}

func TestKey_Format(t *testing.T) {
	assert.Equal(t, "cred_epoch:abc", Key("abc"))
}
