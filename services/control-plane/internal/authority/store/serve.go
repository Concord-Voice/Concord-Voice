package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

// This file is the store's surface for PR-B2's serve, migrate and tick
// (B2-D3). Every statement serve runs outside B1's request paths lives here.

// ServeLockKey is the session advisory lock serve holds for its lifetime, and
// migrate, restore-begin and reconcile-complete try (D-171). "AUTHSRV".
const ServeLockKey int64 = 0x41555448535256

// ErrServeLockHeld means a serve, migrate or owner verb on this node holds it.
var ErrServeLockHeld = errors.New("authority store: the serve lock is held")

// ServeLock is ServeLockKey held on its own connection.
type ServeLock struct{ conn *sql.Conn }

// AcquireServeLock tries ServeLockKey on a dedicated connection and never
// waits (pg_try_advisory_lock).
func AcquireServeLock(ctx context.Context, db *sql.DB) (*ServeLock, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, ServeLockKey).Scan(&ok); err != nil {
		discard(conn) // the server may have granted the lock before the error reached us
		return nil, err
	}
	if !ok {
		_ = conn.Close()
		return nil, ErrServeLockHeld
	}
	return &ServeLock{conn: conn}, nil
}

// Ping fails once the lock's session is gone, and with it the lock (D-171).
func (l *ServeLock) Ping(ctx context.Context) error { return l.conn.PingContext(ctx) }

// Release unlocks, then discards the session rather than pooling it. A
// session-level lock dies with its session, so the lock is gone even when the
// unlock fails on a session that is still alive; Close alone would pool that
// session still holding ServeLockKey. Release is idempotent.
func (l *ServeLock) Release() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = l.conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, ServeLockKey)
	discard(l.conn)
}

// discard closes conn's session instead of returning it to the pool:
// database/sql closes a connection whose Raw callback reports ErrBadConn.
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// ErrWrongRole means serve's connection is not authority_app, or can act as owner.
var ErrWrongRole = errors.New("authority store: serve must connect as authority_app, not a superuser or an owner member")

// CheckAppRole is serve's startup role check, the DoR's checkRole seam
// (DoR:1419, :1652): current_user is authority_app, not a superuser, and not
// a member of authority_owner. session_user must be the same role: a session
// whose login switched to authority_app through SET ROLE (a startup option,
// PGOPTIONS, ALTER ROLE ... SET role) can RESET ROLE back to that login.
// The check proves the privilege envelope too, not only the identity
// (CWE-269): no BYPASSRLS; no membership of pg_write_all_data,
// which writes every table, so a misprovisioned role could lift the release
// gate (meta.replication_required) or delete the ledgers; none of
// pg_execute_server_program or pg_write_server_files, which reach the
// database host; and session_replication_role is origin, since replica,
// set by ALTER ROLE or DATABASE ... SET or a GRANT SET ON PARAMETER, stops
// the guard triggers firing. The setting is read on the session checked;
// the pool's other sessions take the same role and database settings.
func CheckAppRole(ctx context.Context, conn *sql.Conn) error {
	var ok bool
	if err := conn.QueryRowContext(ctx, `SELECT current_user = 'authority_app' AND NOT r.rolsuper
		AND NOT r.rolbypassrls
		AND session_user = current_user
		AND NOT pg_has_role(current_user, 'authority_owner', 'MEMBER')
		AND NOT pg_has_role(current_user, 'pg_write_all_data', 'MEMBER')
		AND NOT pg_has_role(current_user, 'pg_execute_server_program', 'MEMBER')
		AND NOT pg_has_role(current_user, 'pg_write_server_files', 'MEMBER')
		AND current_setting('session_replication_role') = 'origin'
		FROM pg_roles r WHERE r.rolname = current_user`).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrWrongRole
	}
	return nil
}

// Leader reports whether this node's database is out of recovery, through
// the same read as step 1a. serve's role poll calls it every ROLE_POLL_MS.
func (s *Store) Leader(ctx context.Context) (bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	in, err := s.inRecovery(ctx, conn)
	if err != nil {
		return false, err
	}
	return !in, nil
}

// Revoked reads revoked_keys uncached (D-207). serve calls it for the CP's
// TLS pin at every handshake and before every request. It matches key_digest
// alone, as isRevoked does: a row of any kind revokes the digest for every
// caller, since the table is insert-only and a misfiled row stays. So the
// digest's form is the kind's: for cp_tls it is the pin, ac.TLSPin(point),
// SHA-384 of the 120-byte SPKI DER, which serve passes here (tlspin.Pin); for
// cak and operator it is ac.KeyDigest(point), SHA-384 of the 97-byte point. A
// cp_tls row written as KeyDigest never matches, and the key stays admitted.
func (s *Store) Revoked(ctx context.Context, digest []byte) (bool, error) {
	var r bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM authority.revoked_keys
		WHERE key_digest = $1)`, digest).Scan(&r)
	return r, err
}

// NoteNonClient503 records a 503 serve made itself, a panic or an expired
// deadline, for the tick's D-147 leg (B1-D31, B2-D7).
func (s *Store) NoteNonClient503() { s.nonClient503.Store(true) }

// PurgeDue is the purge leg's test (§3.2.4): last_purge_at is NULL, or
// db_now is 24 h past it.
func (s *Store) PurgeDue(ctx context.Context) (bool, error) {
	now, err := s.dbNow(ctx, s.db)
	if err != nil {
		return false, err
	}
	var last sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT last_purge_at FROM authority.meta WHERE id = 1`).Scan(&last); err != nil {
		return false, err
	}
	return purgeDue(msOfNull(last), now), nil
}

// ErrRealmMismatch means meta names another realm than the caller's.
var ErrRealmMismatch = errors.New("authority store: meta names another realm")

// EnsureMeta writes the meta singleton for realm, or confirms it (B2-D2). It
// runs as authority_owner: authority_app may not INSERT meta. last_tick_at
// starts at the insert; replication_required keeps its default, true. The
// realms compare as UUIDs, so a spelling such as upper case is the same realm.
func EnsureMeta(ctx context.Context, owner *sql.DB, realm string) error {
	if _, err := owner.ExecContext(ctx, `INSERT INTO authority.meta (id, realm_id, last_tick_at)
		VALUES (1, $1, clock_timestamp()) ON CONFLICT (id) DO NOTHING`, realm); err != nil {
		return err
	}
	var same bool
	var got string
	if err := owner.QueryRowContext(ctx, `SELECT realm_id = $1::uuid, realm_id::text FROM authority.meta WHERE id = 1`,
		realm).Scan(&same, &got); err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("%w: %s", ErrRealmMismatch, got)
	}
	return nil
}
