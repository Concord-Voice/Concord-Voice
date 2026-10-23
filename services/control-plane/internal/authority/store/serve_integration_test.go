package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

func TestServeLockIsExclusiveAndReleases(t *testing.T) {
	f := authoritydb.Setup(t)
	ctx := context.Background()
	l, err := AcquireServeLock(ctx, f.App)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release) // Release is idempotent; a failure below leaves no lock held
	if l2, err := AcquireServeLock(ctx, f.Owner); !errors.Is(err, ErrServeLockHeld) {
		if l2 != nil {
			l2.Release()
		}
		t.Fatalf("second acquire: %v, want ErrServeLockHeld (D-171)", err)
	}
	if err := l.Ping(ctx); err != nil {
		t.Fatalf("ping while held: %v", err)
	}
	pid := backendPID(ctx, t, l.conn)
	l.Release()
	// Release discards the session rather than pooling it, so the lock cannot
	// outlive it even if the unlock failed on a live session.
	waitSessionGone(ctx, t, f.Super, pid)
	l2, err := AcquireServeLock(ctx, f.App) // the pool the released session came from
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	l2.Release()
}

// D-171: serve exits when it loses the lock's connection; Ping is how it knows.
func TestServeLockPingFailsWhenItsSessionDies(t *testing.T) {
	f := authoritydb.Setup(t)
	ctx := context.Background()
	l, err := AcquireServeLock(ctx, f.App)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	pid := backendPID(ctx, t, l.conn)
	var gone bool // the 5 s timeout waits until the backend has exited
	if err := f.Super.QueryRowContext(ctx, `SELECT pg_terminate_backend($1, 5000)`, pid).Scan(&gone); err != nil || !gone {
		t.Fatalf("terminate the lock's session: %v %v", gone, err)
	}
	if err := l.Ping(ctx); err == nil {
		t.Fatal("ping after the session was terminated: nil, want an error")
	}
}

// An owner verb holding the lock learns its session died through Watch: the
// context stays live while pings answer, is cancelled with ErrServeLockLost
// once the session is terminated, and the lock is by then free for another
// holder, which is what the cancellation guards against. stop is no loss.
func TestServeLockWatchCancelsWhenItsSessionDies(t *testing.T) {
	f := authoritydb.Setup(t)
	ctx := context.Background()
	l, err := AcquireServeLock(ctx, f.Owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	pid := backendPID(ctx, t, l.conn)
	wctx, stop := l.Watch(ctx)
	defer stop()
	time.Sleep(ServeLockWatchInterval * 3 / 2) // at least one ping answered
	if wctx.Err() != nil {
		t.Fatalf("the watch cancelled a live session: %v", context.Cause(wctx))
	}
	var gone bool
	if err := f.Super.QueryRowContext(ctx, `SELECT pg_terminate_backend($1, 5000)`, pid).Scan(&gone); err != nil || !gone {
		t.Fatalf("terminate the lock's session: %v %v", gone, err)
	}
	select {
	case <-wctx.Done():
	case <-time.After(ServeLockWatchInterval + RolePollInterval + time.Second):
		t.Fatal("the watch did not cancel its context after the lock's session died")
	}
	if !errors.Is(context.Cause(wctx), ErrServeLockLost) {
		t.Fatalf("cause = %v, want ErrServeLockLost", context.Cause(wctx))
	}
	cut := errors.New("statement cut short")
	if err := lockLost(wctx, cut); !errors.Is(err, ErrServeLockLost) || !errors.Is(err, cut) {
		t.Fatalf("lockLost = %v, want the work's error joined with ErrServeLockLost", err)
	}
	l2, err := AcquireServeLock(ctx, f.App)
	if err != nil {
		t.Fatalf("acquire after the session died = %v, want the lock free", err)
	}
	defer l2.Release()

	w2, stop2 := l2.Watch(ctx)
	stop2()
	if errors.Is(context.Cause(w2), ErrServeLockLost) || lockLost(w2, cut) != cut {
		t.Fatalf("stop reads as a lost lock: cause %v", context.Cause(w2))
	}
}

func backendPID(ctx context.Context, t *testing.T, conn *sql.Conn) int {
	t.Helper()
	var pid int
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

// waitSessionGone waits up to 5 s for the backend pid to leave pg_stat_activity.
func waitSessionGone(ctx context.Context, t *testing.T, super *sql.DB, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var alive bool
		if err := super.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`,
			pid).Scan(&alive); err != nil {
			t.Fatal(err)
		}
		if !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %d is still alive 5 s after Release: it was pooled, not discarded", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCheckAppRole(t *testing.T) {
	f := authoritydb.Setup(t)
	ctx := context.Background()
	check := func(db *sql.DB) error {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		return CheckAppRole(ctx, conn)
	}
	// A grant left by a killed run would refuse authority_app below for the
	// wrong reason; REVOKE of a membership not held only warns.
	mustExec(t, f.Super, `REVOKE authority_owner FROM authority_app`)
	if err := check(f.App); err != nil {
		t.Fatalf("authority_app: %v", err)
	}
	if err := check(f.Owner); !errors.Is(err, ErrWrongRole) {
		t.Fatalf("authority_owner: %v, want ErrWrongRole", err)
	}
	if err := check(f.Super); !errors.Is(err, ErrWrongRole) {
		t.Fatalf("superuser: %v, want ErrWrongRole", err)
	}
	// Neither a superuser nor an owner member: only the current_user clause
	// refuses it, so this case alone kills mutation 3. The harness opens no
	// pool as authority_backup, so one superuser session assumes it.
	if err := checkOnDiscardedSession(ctx, t, f.Super, `SET SESSION AUTHORIZATION authority_backup`); !errors.Is(err, ErrWrongRole) {
		t.Fatalf("authority_backup: %v, want ErrWrongRole", err)
	}
	mustExec(t, f.Super, `GRANT authority_owner TO authority_app`)
	t.Cleanup(func() { mustExec(t, f.Super, `REVOKE authority_owner FROM authority_app`) })
	if err := check(f.App); !errors.Is(err, ErrWrongRole) {
		t.Fatalf("authority_app as a member of authority_owner: %v, want ErrWrongRole", err)
	}
}

// CWE-269: CheckAppRole proves authority_app's privilege
// envelope, not only its identity. Each case widens the role one way, which
// only its own clause refuses, and puts it back; the plain role passes
// between them, so each refusal is that clause's.
func TestCheckAppRoleRefusesAWiderEnvelope(t *testing.T) {
	f := authoritydb.Setup(t)
	ctx := context.Background()
	check := func() error {
		conn, err := f.App.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		return CheckAppRole(ctx, conn)
	}
	mustExec(t, f.Super, `REVOKE authority_owner FROM authority_app`)
	for _, c := range []struct{ name, widen, restore string }{
		{"a member of pg_write_all_data", `GRANT pg_write_all_data TO authority_app`, `REVOKE pg_write_all_data FROM authority_app`},
		{"a member of pg_execute_server_program", `GRANT pg_execute_server_program TO authority_app`, `REVOKE pg_execute_server_program FROM authority_app`},
		{"a member of pg_write_server_files", `GRANT pg_write_server_files TO authority_app`, `REVOKE pg_write_server_files FROM authority_app`},
		{"BYPASSRLS", `ALTER ROLE authority_app BYPASSRLS`, `ALTER ROLE authority_app NOBYPASSRLS`},
	} {
		mustExec(t, f.Super, c.restore) // a run killed mid-case leaves nothing behind for the next
		if err := check(); err != nil {
			t.Fatalf("%s: the plain role was refused: %v", c.name, err)
		}
		mustExec(t, f.Super, c.widen)
		err := check()
		mustExec(t, f.Super, c.restore)
		if !errors.Is(err, ErrWrongRole) {
			t.Errorf("%s: %v, want ErrWrongRole", c.name, err)
		}
	}
	// session_replication_role is a session setting: one superuser session
	// sets it, then becomes authority_app, as ALTER ROLE ... SET would make
	// every login. The control is the same session without the setting.
	if err := checkOnDiscardedSession(ctx, t, f.Super, `SET SESSION AUTHORIZATION authority_app`); err != nil {
		t.Fatalf("control: authority_app by SET SESSION AUTHORIZATION: %v", err)
	}
	if err := checkOnDiscardedSession(ctx, t, f.Super,
		`SET session_replication_role = replica; SET SESSION AUTHORIZATION authority_app`); !errors.Is(err, ErrWrongRole) {
		t.Fatalf("session_replication_role = replica: %v, want ErrWrongRole", err)
	}
}

// A superuser login switched to authority_app by SET ROLE, as a startup
// option, PGOPTIONS or ALTER ROLE ... SET role would switch it, passes every
// current_user clause yet can RESET ROLE. Only session_user = current_user
// refuses it.
func TestCheckAppRoleRefusesALoginSwitchedBySetRole(t *testing.T) {
	f := authoritydb.Setup(t)
	ctx := context.Background()
	mustExec(t, f.Super, `REVOKE authority_owner FROM authority_app`) // owner membership would refuse it for another reason
	if err := checkOnDiscardedSession(ctx, t, f.Super, `SET ROLE authority_app`); !errors.Is(err, ErrWrongRole) {
		t.Fatalf("a superuser login under SET ROLE authority_app: %v, want ErrWrongRole", err)
	}
}

// checkOnDiscardedSession runs CheckAppRole on one superuser session after
// assume, a fixed statement that switches the session's identity. The session
// is then discarded, never pooled, so the switch cannot reach another caller.
func checkOnDiscardedSession(ctx context.Context, t *testing.T, super *sql.DB, assume string) error {
	t.Helper()
	conn, err := super.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer discard(conn)
	if _, err := conn.ExecContext(ctx, assume); err != nil {
		t.Fatalf("%s: %v", assume, err)
	}
	return CheckAppRole(ctx, conn)
}

func TestLeaderReadsRecoveryThroughTheHook(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	ctx := context.Background()
	var inRec bool
	var hookErr error
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{InRecovery: func(context.Context, *sql.Conn) (bool, error) {
		return inRec, hookErr
	}})
	if ok, err := s.Leader(ctx); err != nil || !ok {
		t.Fatalf("out of recovery: %v %v, want leader", ok, err)
	}
	inRec = true
	if ok, err := s.Leader(ctx); err != nil || ok {
		t.Fatalf("in recovery: %v %v, want standby", ok, err)
	}
	inRec, hookErr = false, errors.New("boom")
	if ok, err := s.Leader(ctx); err == nil || ok {
		t.Fatalf("hook error: %v %v, want (false, err)", ok, err)
	}
	s.hooks.InRecovery = nil // the real pg_is_in_recovery(): the test cluster is a primary
	if ok, err := s.Leader(ctx); err != nil || !ok {
		t.Fatalf("real read: %v %v, want leader", ok, err)
	}
}

// D-207: revoked_keys is read uncached, and on the digest alone. Its key is
// key_digest and it is insert-only, so a row filed under the wrong kind can
// never be corrected: a row of any kind revokes that digest for every caller.
func TestRevokedReadsTheTableUncached(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	ctx := context.Background()
	digest := func(b byte) []byte {
		d := make([]byte, 48)
		d[0] = b
		return d
	}
	pin := digest(7)
	if got, err := s.Revoked(ctx, pin); err != nil || got {
		t.Fatalf("before revoke-key: %v %v", got, err)
	}
	mustExec(t, f.Owner, `INSERT INTO authority.revoked_keys (key_digest, kind) VALUES ($1, 'cp_tls')`, pin)
	if got, err := s.Revoked(ctx, pin); err != nil || !got {
		t.Fatalf("after revoke-key: %v %v, want true with no restart (D-207)", got, err)
	}
	for _, c := range []struct {
		kind string
		d    []byte
	}{{"cak", digest(8)}, {"operator", digest(9)}} {
		mustExec(t, f.Owner, `INSERT INTO authority.revoked_keys (key_digest, kind) VALUES ($1, $2)`, c.d, c.kind)
		if got, err := s.Revoked(ctx, c.d); err != nil || !got {
			t.Fatalf("a row filed under %s: %v %v, want revoked whatever its kind", c.kind, got, err)
		}
	}
	if got, err := s.Revoked(ctx, digest(10)); err != nil || got {
		t.Fatalf("a digest never revoked: %v %v, want false", got, err)
	}
}

func TestNoteNonClient503SetsTheTickLeg(t *testing.T) {
	var s Store
	s.NoteNonClient503()
	if !s.nonClient503.Load() {
		t.Fatal("the D-147 leg is not set")
	}
}

func TestPurgeDueReadsMeta(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	ctx := context.Background()
	for _, c := range []struct {
		set string
		due bool
	}{
		{`UPDATE authority.meta SET last_purge_at = NULL`, true},
		{`UPDATE authority.meta SET last_purge_at = clock_timestamp()`, false},
		{`UPDATE authority.meta SET last_purge_at = clock_timestamp() - interval '23 hours 59 minutes'`, false},
		{`UPDATE authority.meta SET last_purge_at = clock_timestamp() - interval '24 hours 1 minute'`, true},
	} {
		mustExec(t, f.Owner, c.set)
		if got, err := s.PurgeDue(ctx); err != nil || got != c.due {
			t.Fatalf("%s: %v %v, want %v", c.set, got, err, c.due)
		}
	}
}

func TestEnsureMetaInsertsOnceAndRefusesAnotherRealm(t *testing.T) {
	f := authoritydb.Setup(t)
	ctx := context.Background()
	realm := uuid.NewString()
	// A non-canonical spelling names the same realm: it inserts, then confirms.
	if err := EnsureMeta(ctx, f.Owner, strings.ToUpper(realm)); err != nil {
		t.Fatalf("first, in upper case: %v", err)
	}
	var required bool
	if err := f.Owner.QueryRow(`SELECT replication_required FROM authority.meta`).Scan(&required); err != nil || !required {
		t.Fatalf("replication_required: %v %v, want the default true (B2-D2)", required, err)
	}
	if err := EnsureMeta(ctx, f.Owner, realm); err != nil {
		t.Fatalf("same realm again, canonical: %v", err)
	}
	if err := EnsureMeta(ctx, f.Owner, uuid.NewString()); !errors.Is(err, ErrRealmMismatch) {
		t.Fatalf("another realm: %v, want ErrRealmMismatch", err)
	}
	// The same realm, so only the missing INSERT privilege can refuse it.
	err := EnsureMeta(ctx, f.App, realm)
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || pqErr.Code != "42501" || errors.Is(err, ErrRealmMismatch) {
		t.Fatalf("authority_app: %v, want permission denied, SQLSTATE 42501 (000001 grants it no INSERT on meta)", err)
	}
}
