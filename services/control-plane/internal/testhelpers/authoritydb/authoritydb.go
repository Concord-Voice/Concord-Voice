// Package authoritydb is the account authority's test harness (#3587 PR-B1,
// DoR §3.2.3). It reads only AUTHORITY_TEST_DATABASE_URL (and the dirty-test
// URL), requires the exact database name, fails rather than skips under
// CI=true, and never falls back to DATABASE_URL. One superuser URL serves
// every role: each pooled connection runs SET SESSION AUTHORIZATION first.
package authoritydb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/migrationrunner"
	"github.com/lib/pq"
)

// The harness's environment variables, database names and roles.
const (
	EnvURL      = "AUTHORITY_TEST_DATABASE_URL"
	EnvDirtyURL = "AUTHORITY_DIRTY_TEST_DATABASE_URL"
	EnvGhostURL = "AUTHORITY_GHOST_DATABASE_URL"
	DBName      = "concord_authority_test"
	DirtyDBName = "concord_authority_dirty_test"
	RoleApp     = "authority_app"
	RoleOwner   = "authority_owner"
	RoleSuper   = "" // the URL's own (superuser) identity
)

// switchStmt is fixed text per role: no identifier is ever interpolated.
var switchStmt = map[string]string{
	RoleApp:   "SET SESSION AUTHORIZATION authority_app",
	RoleOwner: "SET SESSION AUTHORIZATION authority_owner",
	RoleSuper: "",
}

// CheckEnv resolves env through getenv. A missing URL is a skip locally and an
// error under CI=true. A URL naming any database but wantDB is an error whose
// text never quotes the URL (it may carry a password).
func CheckEnv(getenv func(string) string, env, wantDB string) (dsn string, skip bool, err error) {
	raw := getenv(env)
	if raw == "" {
		if getenv("CI") == "true" {
			return "", false, fmt.Errorf("authoritydb: %s is unset under CI=true", env)
		}
		return "", true, nil
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", false, fmt.Errorf("authoritydb: %s does not parse as a URL", env)
	}
	if strings.TrimPrefix(u.Path, "/") != wantDB {
		return "", false, fmt.Errorf("authoritydb: %s must name database %q", env, wantDB)
	}
	return raw, false, nil
}

// URL is CheckEnv over the process environment, as a test helper.
func URL(t testing.TB, env, wantDB string) string {
	t.Helper()
	dsn, skip, err := CheckEnv(os.Getenv, env, wantDB)
	if err != nil {
		t.Fatal(err)
	}
	if skip {
		t.Skipf("%s is unset; export it as the plan's isolated-database section shows", env)
	}
	return dsn
}

type roleConnector struct {
	base *pq.Connector
	stmt string
}

func (c roleConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil || c.stmt == "" {
		return conn, err
	}
	ex, ok := conn.(driver.ExecerContext)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("authoritydb: driver connection cannot execute")
	}
	if _, err := ex.ExecContext(ctx, c.stmt, nil); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("authoritydb: switch role: %w", err)
	}
	return conn, nil
}

func (c roleConnector) Driver() driver.Driver { return c.base.Driver() }

// OpenAs opens a pool whose every connection runs as role. It is closed when
// the test ends.
func OpenAs(t testing.TB, dsn, role string) *sql.DB {
	t.Helper()
	stmt, ok := switchStmt[role]
	if !ok {
		t.Fatalf("authoritydb: unknown role %q", role)
	}
	base, err := pq.NewConnector(dsn)
	if err != nil {
		t.Fatalf("authoritydb: connector: %v", err)
	}
	db := sql.OpenDB(roleConnector{base: base, stmt: stmt})
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("authoritydb: ping as %q: %v", role, err)
	}
	if err := checkDatabase(db, dsn); err != nil {
		t.Fatal(err)
	}
	return db
}

// checkDatabase compares the database the server reports with the one the URL
// path names, which CheckEnv compared with the wanted name. A query key such as
// dbname= or service= makes lib/pq connect elsewhere, so the path alone does
// not say where Setup migrates and truncates (review round 4).
func checkDatabase(db *sql.DB, dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return errors.New("authoritydb: the DSN does not parse as a URL")
	}
	var got string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&got); err != nil {
		return fmt.Errorf("authoritydb: current_database: %w", err)
	}
	if want := strings.TrimPrefix(u.Path, "/"); got != want {
		return fmt.Errorf("authoritydb: connected to database %q, but the URL path names %q; remove the override", got, want)
	}
	return nil
}

// Advisory-lock keys: one test at a time per database, across packages (CI runs
// two package binaries at once).
const (
	lockKey      int64 = 0x415554484f5231 // "AUTHOR1"
	dirtyLockKey int64 = 0x415554484f5232 // "AUTHOR2"
)

var (
	migrateMu   sync.Mutex
	migratedDSN = map[string]error{}
)

func migrateOnceFor(dsn string) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()
	if err, done := migratedDSN[dsn]; done {
		return err
	}
	base, err := pq.NewConnector(dsn)
	if err == nil {
		mdb := sql.OpenDB(roleConnector{base: base, stmt: switchStmt[RoleOwner]})
		mdb.SetMaxOpenConns(1) // migrationrunner pins one connection for the process
		err = migrationrunner.Run(mdb, MigrationsSourceURL())
	}
	migratedDSN[dsn] = err
	return err
}

// MigrationsSourceURL is the file:// URL of the store's migrations directory.
// The harness does not import package store, so store's own tests can import it.
func MigrationsSourceURL() string {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "authority", "store", "migrations")
	abs, _ := filepath.Abs(dir)
	return "file://" + abs
}

// Fixture is one test's view of concord_authority_test, under three roles.
type Fixture struct{ App, Owner, Super *sql.DB }

func lock(t testing.TB, super *sql.DB, key int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conn, err := super.Conn(ctx)
	if err != nil {
		t.Fatalf("authoritydb: lock conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		_ = conn.Close()
		t.Fatalf("authoritydb: advisory lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		_ = conn.Close()
	})
}

// Setup takes the database lock, migrates once per process as authority_owner,
// and empties every authority table. Tests in a package run serially.
func Setup(t testing.TB) *Fixture { return SetupAt(t, EnvURL) }

// SetupAt is Setup against the database env names (concord_authority_test in
// every cluster: the shared one, or the dedicated ghost container).
func SetupAt(t testing.TB, env string) *Fixture {
	t.Helper()
	dsn := URL(t, env, DBName)
	f := &Fixture{App: OpenAs(t, dsn, RoleApp), Owner: OpenAs(t, dsn, RoleOwner), Super: OpenAs(t, dsn, RoleSuper)}
	lock(t, f.Super, lockKey)
	if err := migrateOnceFor(dsn); err != nil {
		t.Fatalf("authoritydb: migrate: %v", err)
	}
	// The schema refuses TRUNCATE on its never-deleted tables, for the owner
	// too. A replica session skips those triggers; SET LOCAL keeps it to this
	// transaction, so the seed inserts that follow still run every guard.
	if _, err := f.Super.Exec(`BEGIN; SET LOCAL session_replication_role = replica;
		TRUNCATE authority.account_events, authority.device_events, authority.request_ledger,
		authority.key_bindings, authority.revoked_keys, authority.legacy_population, authority.account_heads,
		authority.successions, authority.delegations, authority.meta RESTART IDENTITY; COMMIT`); err != nil {
		t.Fatalf("authoritydb: reset: %v", err)
	}
	return f
}

// SeedMeta inserts the meta singleton as authority_owner (B1-D12). The test
// cluster has no standby, so replication_required is false, as the dev
// bootstrap and the self-host installer set it (DoR §3.2.3).
func (f *Fixture) SeedMeta(t testing.TB, realmID string) {
	t.Helper()
	if _, err := f.Owner.Exec(`INSERT INTO authority.meta (id, realm_id, last_tick_at, replication_required)
		VALUES (1, $1, clock_timestamp(), false)`, realmID); err != nil {
		t.Fatalf("authoritydb: seed meta: %v", err)
	}
}

// DirtySetup gives a test an empty `authority` schema on the dirty-test database.
func DirtySetup(t testing.TB) (owner, super *sql.DB) {
	t.Helper()
	dsn := URL(t, EnvDirtyURL, DirtyDBName)
	super = OpenAs(t, dsn, RoleSuper)
	lock(t, super, dirtyLockKey)
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS authority CASCADE`,
		`CREATE SCHEMA authority AUTHORIZATION authority_owner`,
	} {
		if _, err := super.Exec(q); err != nil {
			t.Fatalf("authoritydb: dirty reset: %v", err)
		}
	}
	// OpenAs's pool of 8, not 1: every Migrate pins a connection for the life
	// of the process, and the dirty tests call it more than once on one pool.
	owner = OpenAs(t, dsn, RoleOwner)
	return owner, super
}
