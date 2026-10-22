package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// dirtySetup is authoritydb.DirtySetup under authoritydb.Setup's advisory
// lock as well. The store's tests GRANT and REVOKE authority_owner on
// authority_app, which is cluster-wide, holding Setup's lock alone (ruling 6),
// and CI runs packages two at a time; holding that lock here keeps this
// package's tests out of that window, so none can see the app role with owner
// membership, today or after a test here connects as authority_app. Setup's
// lock is taken before DirtySetup's, the order any test holding both must use.
func dirtySetup(t *testing.T) (owner, super *sql.DB) {
	t.Helper()
	authoritydb.Setup(t)
	return authoritydb.DirtySetup(t)
}

// B2-D2: migrate builds the schema, writes meta once, refuses another realm,
// and refuses while the serve lock is held (D-171).
func TestMigrateDB(t *testing.T) {
	owner, super := dirtySetup(t)
	ctx := context.Background()
	realm := uuid.NewString()
	if err := migrateDB(ctx, owner, realm); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := store.CheckSchema(ctx, owner); err != nil {
		t.Fatalf("schema after migrate: %v", err)
	}
	var got string
	if err := owner.QueryRow(`SELECT realm_id FROM authority.meta WHERE id = 1`).Scan(&got); err != nil || got != realm {
		t.Fatalf("meta realm %q, %v", got, err)
	}
	if err := migrateDB(ctx, owner, realm); err != nil {
		t.Fatalf("second migrate, same realm: %v", err)
	}
	if err := migrateDB(ctx, owner, uuid.NewString()); reason(err) != "trust" {
		t.Fatalf("another realm: %v, want reason trust", err)
	}
	lock, err := store.AcquireServeLock(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := migrateDB(ctx, owner, realm); reason(err) != "lock" {
		t.Fatalf("lock held: %v, want reason lock", err)
	}
}

// returnsWithin runs f and reports whether it returned within wait, and with
// what.
func returnsWithin(wait time.Duration, f func() error) (returned bool, err error) {
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return true, err
	case <-time.After(wait):
		return false, nil
	}
}

// shortDeadline makes the per-step deadline 300 ms for one test.
func shortDeadline(t *testing.T) {
	t.Helper()
	old := store.RequestDeadlineTx
	store.RequestDeadlineTx = 300 * time.Millisecond
	t.Cleanup(func() { store.RequestDeadlineTx = old })
}

// Carry 4 (M2), the lock step: with every connection of the pool held,
// AcquireServeLock waits for one. The step's own deadline ends the wait, and
// an expired deadline keeps the step's reason, config (the lock is not
// held; it was never asked about). The caller's context outlasts the
// window, so a step with no deadline of its own is still waiting at 2 s.
func TestMigrateDBBoundsTheLockStep(t *testing.T) {
	owner, _ := dirtySetup(t)
	shortDeadline(t)
	owner.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	held, err := owner.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	returned, err := returnsWithin(2*time.Second, func() error { return migrateDB(ctx, owner, uuid.NewString()) })
	if !returned {
		t.Fatal("migrateDB is still waiting for a connection after 2 s: the lock step has no deadline of its own")
	}
	if reason(err) != "config" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a lock step that timed out: %v (reason %q), want reason config over the deadline", err, reason(err))
	}
}

// Carry 4 (M2), the meta step: a transaction holding authority.meta
// ACCESS EXCLUSIVE blocks EnsureMeta's insert. The step's own deadline ends
// the wait with reason=config. The migration before it, which takes no
// context and which no deadline bounds, has already run: this is the
// already-migrated second pass, which finds nothing to apply.
func TestMigrateDBBoundsTheMetaStep(t *testing.T) {
	owner, super := dirtySetup(t)
	realm := uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := migrateDB(ctx, owner, realm); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	tx, err := super.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(ctx, `LOCK TABLE authority.meta IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	shortDeadline(t)
	returned, err := returnsWithin(2*time.Second, func() error { return migrateDB(ctx, owner, realm) })
	if !returned {
		t.Fatal("migrateDB is still waiting on authority.meta after 2 s: the meta step has no deadline of its own")
	}
	if reason(err) != "config" {
		t.Fatalf("a meta step that timed out: %v (reason %q), want config", err, reason(err))
	}
}

// Carry C1 of the Task 11 review, against the real driver: an object the first
// migration creates already exists, so golang-migrate fails with a
// database.Error whose text carries the whole migration's SQL. The exit line
// carries the cause's message and nothing of the SQL. (A dropped connection
// mid-migration, the OpError shape, cannot be forced from a test; the shape
// rows in TestExitTextOfMigrationErrors stand in for it.)
func TestMigrateDBFailureCarriesNoSQL(t *testing.T) {
	owner, _ := dirtySetup(t)
	if _, err := owner.Exec(`CREATE TABLE authority.meta (id smallint)`); err != nil {
		t.Fatal(err)
	}
	err := migrateDB(context.Background(), owner, uuid.NewString())
	if reason(err) != "config" {
		t.Fatalf("a migration that fails: %v (reason %q), want config", err, reason(err))
	}
	for _, s := range []string{"CREATE TABLE authority.meta (", "Account authority, migration 000001"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("control: the raw error does not carry %q, so this test cannot see the SQL leak", s)
		}
	}
	var buf bytes.Buffer
	exitCode(logger.NewJSON(&buf).Logger, err)
	m := exitLine(t, 1, buf.String(), "config")
	if got := errorOf(t, m); got != `database: pq: relation "meta" already exists` {
		t.Fatalf("exit line error %q", got)
	}
	if strings.Contains(buf.String(), "CREATE") || strings.Contains(buf.String(), "Account authority") {
		t.Fatalf("the exit line carries the migration's SQL: %.200q", buf.String())
	}
}
