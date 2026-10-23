package managedturn_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite" // test-only: inspect the on-disk database as coturn would see it

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedturn"
)

// coturnDDL is the exact list of statements coturn 4.18.0 runs in
// init_sqlite_database (src/apps/relay/dbdrivers/dbd_sqlite.c), keyed by table.
// The \t escapes are literal tab characters in the C source. Whitespace
// matters: sqlite_master.sql stores the statement text verbatim, and the
// read-only coturn mount must find the tables exactly as coturn would create.
var coturnDDL = map[string]string{ // #nosec G101 -- coturn 4.18.0 schema DDL, not a credential.
	"turnusers_lt":         "CREATE TABLE turnusers_lt ( realm varchar(127) default '', name varchar(512), hmackey char(128), PRIMARY KEY (realm,name))",
	"turn_secret":          "CREATE TABLE turn_secret (realm varchar(127) default '', value varchar(127), primary key (realm,value))",
	"allowed_peer_ip":      "CREATE TABLE allowed_peer_ip (realm varchar(127) default '', ip_range varchar(256), primary key (realm,ip_range))",
	"denied_peer_ip":       "CREATE TABLE denied_peer_ip (realm varchar(127) default '', ip_range varchar(256), primary key (realm,ip_range))",
	"turn_origin_to_realm": "CREATE TABLE turn_origin_to_realm (origin varchar(127),realm varchar(127),primary key (origin))",
	"turn_realm_option":    "CREATE TABLE turn_realm_option (realm varchar(127) default '',\topt varchar(32),\tvalue varchar(128),\tprimary key (realm,opt))",
	"oauth_key":            "CREATE TABLE oauth_key (kid varchar(128),ikm_key varchar(256),timestamp bigint default 0,lifetime integer default 0,as_rs_alg varchar(64) default '',realm varchar(127) default '',primary key (kid))",
	"admin_user":           "CREATE TABLE admin_user (name varchar(32), realm varchar(127), password varchar(127), primary key (name))",
}

// openRaw opens a direct sqlite handle on path (a stand-in for coturn's own
// libsqlite3 connection). One connection only, so it is never reopened.
func openRaw(t testing.TB, path, mode string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode="+mode)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func schemaOf(t testing.TB, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	out := map[string]string{}
	for rows.Next() {
		var name, ddl string
		require.NoError(t, rows.Scan(&name, &ddl))
		out[name] = ddl
	}
	require.NoError(t, rows.Err())
	return out
}

func countRows(t testing.TB, db *sql.DB, realm string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM turn_secret WHERE realm = ?`, realm).Scan(&n))
	return n
}

func newDBPath(t testing.TB) string {
	t.Helper()
	return filepath.Join(shareableTempDir(t), "turndb")
}

func sortedIDs(keys ...managedturn.Key) []string {
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, k.ID())
	}
	sort.Strings(ids)
	return ids
}

func TestStore_InitSchema_MatchesCoturnDDLExactly(t *testing.T) {
	path := newDBPath(t)
	_ = openInitStore(t, path)

	got := schemaOf(t, openRaw(t, path, "ro"))

	require.Len(t, got, 8, "exactly coturn's eight tables, no extras")
	for name, want := range coturnDDL {
		assert.Equal(t, want, got[name], "table %s must be created byte-for-byte as coturn would", name)
	}

	var extras int
	require.NoError(t, openRaw(t, path, "ro").QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type IN ('view','trigger')`).Scan(&extras))
	assert.Zero(t, extras, "no views or triggers")
}

func TestStore_InitSchema_CoturnReadQueryWorks(t *testing.T) {
	path := newDBPath(t)
	s := openInitStore(t, path)
	k, raw := newKey(t)
	require.NoError(t, s.Insert(context.Background(), testRealm, k))

	// The exact statement coturn 4.18.0 issues per authentication.
	rows, err := openRaw(t, path, "ro").Query(`select value from turn_secret where realm=?`, testRealm)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var values []string
	for rows.Next() {
		var v string
		require.NoError(t, rows.Scan(&v))
		values = append(values, v)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{raw}, values, "coturn must read the raw key under the exact realm")
}

func TestStore_InitSchema_IdempotentAndNeverDeletesRows(t *testing.T) {
	ctx := context.Background()
	path := newDBPath(t)
	s := openInitStore(t, path)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	require.NoError(t, s.Insert(ctx, testRealm, k1))
	require.NoError(t, s.Insert(ctx, testRealm, k2))
	before := schemaOf(t, openRaw(t, path, "ro"))

	require.NoError(t, s.InitSchema(ctx))
	require.NoError(t, s.InitSchema(ctx))

	ids, err := s.KeyIDs(ctx, testRealm)
	require.NoError(t, err)
	assert.Equal(t, sortedIDs(k1, k2), ids, "InitSchema must never change rows")
	assert.Equal(t, before, schemaOf(t, openRaw(t, path, "ro")), "schema unchanged on repeat")

	// A fresh store handle over the same file behaves the same.
	s2, err := managedturn.OpenStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	require.NoError(t, s2.InitSchema(ctx))
	ids, err = s2.KeyIDs(ctx, testRealm)
	require.NoError(t, err)
	assert.Equal(t, sortedIDs(k1, k2), ids)
}

func TestStore_OpenStore_CreatesAbsentFile(t *testing.T) {
	path := newDBPath(t)
	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err), "fixture must start without a database")

	_ = openInitStore(t, path)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
	assert.Zero(t, info.Mode().Perm()&0o027, "database must not be group-writable or world-accessible, got %v", info.Mode().Perm())
}

func TestStore_JournalModeDeleteNotWAL(t *testing.T) {
	ctx := context.Background()
	path := newDBPath(t)
	s := openInitStore(t, path)
	k, _ := newKey(t)
	require.NoError(t, s.Insert(ctx, testRealm, k))

	// SQLite header bytes 18 and 19 are the file-format write/read versions:
	// 1 is the rollback-journal format, 2 is WAL. WAL is persistent, so a
	// store that ever switched modes would be visible here.
	header := make([]byte, 100)
	f, err := os.Open(path) // #nosec G304 -- test temp-dir path.
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()
	_, err = f.ReadAt(header, 0)
	require.NoError(t, err)
	assert.Equal(t, byte(1), header[18], "write format must be legacy rollback-journal, not WAL")
	assert.Equal(t, byte(1), header[19], "read format must be legacy rollback-journal, not WAL")

	var mode string
	require.NoError(t, openRaw(t, path, "ro").QueryRow(`PRAGMA journal_mode`).Scan(&mode))
	assert.Equal(t, "delete", mode)

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"turndb"}, names, "no -wal, -shm or leftover -journal beside the database")
}

func TestStore_InsertThenKeyIDs_RoundTrip(t *testing.T) {
	ctx := context.Background()
	path := newDBPath(t)
	s := openInitStore(t, path)

	empty, err := s.KeyIDs(ctx, testRealm)
	require.NoError(t, err)
	assert.Empty(t, empty)

	k1, raw1 := newKey(t)
	k2, _ := newKey(t)
	require.NoError(t, s.Insert(ctx, testRealm, k1))
	ids, err := s.KeyIDs(ctx, testRealm)
	require.NoError(t, err)
	assert.Equal(t, []string{k1.ID()}, ids)

	require.NoError(t, s.Insert(ctx, testRealm, k2))
	ids, err = s.KeyIDs(ctx, testRealm)
	require.NoError(t, err)
	assert.Equal(t, sortedIDs(k1, k2), ids, "IDs are returned sorted")
	assert.True(t, sort.StringsAreSorted(ids))

	// The row is what coturn reads: the raw key under the exact realm.
	db := openRaw(t, path, "ro")
	var gotRealm, gotValue string
	require.NoError(t, db.QueryRow(`SELECT realm, value FROM turn_secret WHERE value = ?`, raw1).Scan(&gotRealm, &gotValue))
	assert.Equal(t, testRealm, gotRealm)
	assert.Equal(t, raw1, gotValue)
	assert.Equal(t, 2, countRows(t, db, testRealm))
}

func TestStore_RealmIsolation(t *testing.T) {
	ctx := context.Background()
	s := openInitStore(t, newDBPath(t))
	kA, _ := newKey(t)
	kB, _ := newKey(t)
	realmA, realmB := "realm-a.test.invalid", "realm-b.test.invalid"

	require.NoError(t, s.Insert(ctx, realmA, kA))
	require.NoError(t, s.Insert(ctx, realmB, kB))

	idsA, err := s.KeyIDs(ctx, realmA)
	require.NoError(t, err)
	idsB, err := s.KeyIDs(ctx, realmB)
	require.NoError(t, err)
	idsNone, err := s.KeyIDs(ctx, "realm-c.test.invalid")
	require.NoError(t, err)

	assert.Equal(t, []string{kA.ID()}, idsA)
	assert.Equal(t, []string{kB.ID()}, idsB)
	assert.Empty(t, idsNone)
}

func TestStore_RealmMatchIsExact(t *testing.T) {
	ctx := context.Background()
	s := openInitStore(t, newDBPath(t))
	k, _ := newKey(t)
	require.NoError(t, s.Insert(ctx, testRealm, k))

	for _, other := range []string{"TURN.TEST.INVALID", testRealm + " ", " " + testRealm, "test.invalid"} {
		ids, err := s.KeyIDs(ctx, other)
		require.NoError(t, err)
		assert.Empty(t, ids, "realm %q must not match %q", other, testRealm)
	}
}

func TestStore_DuplicateInsertDoesNotDuplicateRow(t *testing.T) {
	ctx := context.Background()
	s := openInitStore(t, newDBPath(t))
	k, _ := newKey(t)
	require.NoError(t, s.Insert(ctx, testRealm, k))

	_ = s.Insert(ctx, testRealm, k) // refusing or ignoring are both acceptable

	ids, err := s.KeyIDs(ctx, testRealm)
	require.NoError(t, err)
	assert.Equal(t, []string{k.ID()}, ids, "a repeated key is never stored twice")
}

func TestStore_InsertRejectsZeroKey(t *testing.T) {
	ctx := context.Background()
	s := openInitStore(t, newDBPath(t))

	err := s.Insert(ctx, testRealm, managedturn.Key{})
	requireRejected(t, err, managedturn.ErrInvalidKey)

	ids, kerr := s.KeyIDs(ctx, testRealm)
	require.NoError(t, kerr)
	assert.Empty(t, ids, "an empty key must never become a turn_secret row")
}

func inodeOf(t testing.TB, path string) (dev string, ino uint64) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok, "expected a unix stat")
	return fmt.Sprint(st.Dev), uint64(st.Ino) //nolint:unconvert // inode field widths differ per platform
}

func TestStore_NeverRenameReplacesDatabase(t *testing.T) {
	ctx := context.Background()
	path := newDBPath(t)
	s := openInitStore(t, path)
	dev0, ino0 := inodeOf(t, path)

	for i := 0; i < 3; i++ {
		k, _ := newKey(t)
		require.NoError(t, s.Insert(ctx, testRealm, k))
		dev, ino := inodeOf(t, path)
		assert.Equal(t, dev0, dev)
		assert.Equal(t, ino0, ino, "insert %d replaced the database file; coturn's cached connections would keep the old inode", i)
	}
	require.NoError(t, s.InitSchema(ctx))
	_, ino := inodeOf(t, path)
	assert.Equal(t, ino0, ino, "InitSchema must also leave the inode alone")
}

func TestStore_SecondReadOnlyConnectionSeesInsertWithoutReopening(t *testing.T) {
	ctx := context.Background()
	path := newDBPath(t)
	s := openInitStore(t, path)
	reader := openRaw(t, path, "ro") // simulates coturn: one cached read-only connection

	require.Equal(t, 0, countRows(t, reader, testRealm))

	k1, _ := newKey(t)
	require.NoError(t, s.Insert(ctx, testRealm, k1))
	assert.Equal(t, 1, countRows(t, reader, testRealm), "the same open connection sees the first insert")

	k2, _ := newKey(t)
	require.NoError(t, s.Insert(ctx, testRealm, k2))
	assert.Equal(t, 2, countRows(t, reader, testRealm), "and the second, with no reopen")
}

func TestStore_ReopenSeesPersistedRows(t *testing.T) {
	ctx := context.Background()
	path := newDBPath(t)
	s, err := managedturn.OpenStore(ctx, path)
	require.NoError(t, err)
	require.NoError(t, s.InitSchema(ctx))
	k, _ := newKey(t)
	require.NoError(t, s.Insert(ctx, testRealm, k))
	require.NoError(t, s.Close())

	s2 := openInitStore(t, path)
	ids, err := s2.KeyIDs(ctx, testRealm)
	require.NoError(t, err)
	assert.Equal(t, []string{k.ID()}, ids)
}

func TestStore_InsertOnReadOnlyDatabaseFails(t *testing.T) {
	ctx := context.Background()
	path := newDBPath(t)
	s, err := managedturn.OpenStore(ctx, path)
	require.NoError(t, err)
	require.NoError(t, s.InitSchema(ctx))
	k1, _ := newKey(t)
	require.NoError(t, s.Insert(ctx, testRealm, k1))
	require.NoError(t, s.Close())

	env := makeStoreReadOnly(t, path)

	ro, openErr := managedturn.OpenStore(ctx, path)
	if openErr != nil {
		// Refusing to open a database it cannot write is fail-closed too.
		requireRejected(t, openErr)
		env.Restore()
	} else {
		k2, _ := newKey(t)
		insertErr := ro.Insert(ctx, testRealm, k2)
		_ = ro.Close()
		env.Restore()
		requireRejected(t, insertErr)
	}

	check := openInitStore(t, path)
	ids, err := check.KeyIDs(ctx, testRealm)
	require.NoError(t, err)
	assert.Equal(t, []string{k1.ID()}, ids, "a refused insert leaves the rows untouched")
}

func TestStore_InsertFailsWhileDatabaseIsLocked(t *testing.T) {
	ctx := context.Background()
	path := newDBPath(t)
	s := openInitStore(t, path)
	k1, _ := newKey(t)
	require.NoError(t, s.Insert(ctx, testRealm, k1))

	blocker := openRaw(t, path, "rwc")
	conn, err := blocker.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.ExecContext(ctx, `BEGIN EXCLUSIVE`)
	require.NoError(t, err)

	k2, _ := newKey(t)
	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	insertErr := s.Insert(short, testRealm, k2)
	elapsed := time.Since(start)

	requireRejected(t, insertErr)
	assert.Less(t, elapsed, 15*time.Second, "a contended insert must fail promptly, not hang")

	_, err = conn.ExecContext(ctx, `ROLLBACK`)
	require.NoError(t, err)

	ids, err := s.KeyIDs(ctx, testRealm)
	require.NoError(t, err)
	assert.Equal(t, []string{k1.ID()}, ids, "the failed insert left no row")

	require.NoError(t, s.Insert(ctx, testRealm, k2), "the store recovers once the lock is gone")
	ids, err = s.KeyIDs(ctx, testRealm)
	require.NoError(t, err)
	assert.Equal(t, sortedIDs(k1, k2), ids)
}

func TestStore_Close(t *testing.T) {
	s, err := managedturn.OpenStore(context.Background(), newDBPath(t))
	require.NoError(t, err)
	require.NoError(t, s.Close())
}
