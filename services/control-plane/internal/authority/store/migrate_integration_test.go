package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

func TestEmbeddedMigrationsMatchTheDirectory(t *testing.T) {
	ents, err := fs.ReadDir(MigrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != len(disk) || len(ents) != 2 {
		t.Fatalf("embedded %d files, directory %d, want 2 each", len(ents), len(disk))
	}
	for _, e := range ents {
		emb, _ := fs.ReadFile(MigrationFiles, "migrations/"+e.Name())
		dsk, err := os.ReadFile(filepath.Join("migrations", e.Name()))
		if err != nil || string(emb) != string(dsk) {
			t.Fatalf("%s differs between the embed and the directory", e.Name())
		}
	}
}

func TestMigrateLeavesACleanSchemaAndIsIdempotent(t *testing.T) {
	f := authoritydb.Setup(t)
	if err := CheckSchema(context.Background(), f.App); err != nil {
		t.Fatalf("CheckSchema after Setup: %v", err)
	}
	owner, _ := authoritydb.DirtySetup(t)
	for i := 0; i < 2; i++ {
		if err := Migrate(owner, authoritydb.MigrationsSourceURL()); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}
}

func TestDirtySchemaIsRefusedByMigrateAndCheckSchema(t *testing.T) {
	owner, super := authoritydb.DirtySetup(t)
	if err := Migrate(owner, authoritydb.MigrationsSourceURL()); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if _, err := super.Exec(`UPDATE authority.schema_migrations SET dirty = true`); err != nil {
		t.Fatal(err)
	}
	err := Migrate(owner, authoritydb.MigrationsSourceURL())
	if err == nil || !strings.Contains(err.Error(), "is dirty") {
		t.Fatalf("Migrate over a dirty schema = %v, want the migrationrunner refusal", err)
	}
	if err := CheckSchema(context.Background(), owner); !errors.Is(err, ErrSchemaDirty) {
		t.Fatalf("CheckSchema(dirty) = %v, want ErrSchemaDirty", err)
	}
}

func TestSkewedSchemaIsRefusedInBothDirections(t *testing.T) {
	owner, super := authoritydb.DirtySetup(t)
	if err := Migrate(owner, authoritydb.MigrationsSourceURL()); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(`UPDATE authority.schema_migrations SET version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(context.Background(), owner); !errors.Is(err, ErrSchemaSkew) {
		t.Fatalf("CheckSchema(ahead) = %v, want ErrSchemaSkew", err)
	}
	// golang-migrate cannot find the recorded version 2 in the source, so Up
	// errors. If a future golang-migrate returned ErrNoChange here instead,
	// CheckSchema is still the refusal serve relies on; record it and keep this
	// assertion's intent by asserting CheckSchema only.
	if err := Migrate(owner, authoritydb.MigrationsSourceURL()); err == nil {
		t.Fatal("Migrate accepted a schema ahead of the code")
	}
	if _, err := super.Exec(`DELETE FROM authority.schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(context.Background(), owner); !errors.Is(err, ErrSchemaSkew) {
		t.Fatalf("CheckSchema(no version) = %v, want ErrSchemaSkew", err)
	}
}

func TestDownMigrationRaisesWhileAnyAuthorityRowExistsAndRoundTripsWhenEmpty(t *testing.T) {
	owner, super := authoritydb.DirtySetup(t)
	if err := Migrate(owner, authoritydb.MigrationsSourceURL()); err != nil {
		t.Fatal(err)
	}
	drv, err := postgres.WithInstance(owner, &postgres.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithDatabaseInstance(authoritydb.MigrationsSourceURL(), "postgres", drv)
	if err != nil {
		t.Fatal(err)
	}
	refused := func(what string) {
		t.Helper()
		if err := m.Down(); err == nil || !strings.Contains(err.Error(), "authority rows exist") {
			t.Fatalf("Down with %s = %v, want the RAISE", what, err)
		}
		// The failed Down rolled back as one implicit transaction, but
		// golang-migrate had already recorded its target first: version -1
		// (none), dirty. Restore version 1, or the next Down is a no-op.
		mustExec(t, super, `UPDATE authority.schema_migrations SET version = $1, dirty = false`, SchemaVersion)
	}
	seedOneEvent(t, super)
	refused("an event")
	mustExec(t, super, `BEGIN; SET LOCAL session_replication_role = replica;
		TRUNCATE authority.account_events, authority.device_events, authority.delegations; COMMIT`)
	// B1-D27: a recorded refusal leaves a head and a ledger row and no event.
	insertAbsentHead(t, super, testAcct)
	mustExec(t, super, `INSERT INTO authority.request_ledger (account_id, request_id, intent_digest, code, cp_evidence)
		VALUES ($1, gen_random_uuid(), $2, 'stale_seq', '\x00')`, testAcct, d48(4))
	refused("a recorded refusal and no event")
	mustExec(t, super, `TRUNCATE authority.request_ledger, authority.account_heads`)
	if err := m.Down(); err != nil {
		t.Fatalf("Down with no event: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
}

// A row committed while Down runs must still refuse it. Down locks the tables
// before its check, so it waits for the open insert and then sees the row.
// Without the lock the check passes, DROP waits, and the row is dropped.
func TestDownMigrationRefusesARowCommittedWhileItRuns(t *testing.T) {
	owner, super := authoritydb.DirtySetup(t)
	if err := Migrate(owner, authoritydb.MigrationsSourceURL()); err != nil {
		t.Fatal(err)
	}
	drv, err := postgres.WithInstance(owner, &postgres.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithDatabaseInstance(authoritydb.MigrationsSourceURL(), "postgres", drv)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := super.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO authority.account_heads (account_id, state, seq, recovery_gen, susp_operator,
		susp_user, root_revoked, legacy_pin_revoked) VALUES ($1, 'absent', 0, 0, false, false, false, false)`, testAcct); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Down() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := super.QueryRow(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Down never waited on the open insert")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "authority rows exist") {
		t.Fatalf("Down racing a committed insert = %v, want the RAISE", err)
	}
}
