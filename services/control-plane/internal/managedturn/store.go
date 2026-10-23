package managedturn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO_ENABLED=0), POSIX fcntl locking like coturn's libsqlite3
)

// storeBusyTimeoutMS bounds how long a write waits for coturn's readers to
// release their SHARED locks. coturn itself sets no busy timeout, so a read
// that collides with a commit fails that one authentication closed (rule 4).
const storeBusyTimeoutMS = 250

// dbFileMode lets coturn (group 65533 via the setgid db directory) read the
// file through its read-only mount; nobody else can.
const dbFileMode os.FileMode = 0o640

// coturnSchema is the exact list of statements coturn 4.18.0 runs in
// init_sqlite_database (src/apps/relay/dbdrivers/dbd_sqlite.c), in order. The
// text is stored verbatim in sqlite_master, so it is kept byte-for-byte,
// including the two literal tabs in turn_realm_option.
var coturnSchema = []struct{ table, ddl string }{
	{"turnusers_lt", "CREATE TABLE turnusers_lt ( realm varchar(127) default '', name varchar(512), hmackey char(128), PRIMARY KEY (realm,name))"},
	{"turn_secret", "CREATE TABLE turn_secret (realm varchar(127) default '', value varchar(127), primary key (realm,value))"},
	{"allowed_peer_ip", "CREATE TABLE allowed_peer_ip (realm varchar(127) default '', ip_range varchar(256), primary key (realm,ip_range))"},
	{"denied_peer_ip", "CREATE TABLE denied_peer_ip (realm varchar(127) default '', ip_range varchar(256), primary key (realm,ip_range))"},
	{"turn_origin_to_realm", "CREATE TABLE turn_origin_to_realm (origin varchar(127),realm varchar(127),primary key (origin))"},
	{"turn_realm_option", "CREATE TABLE turn_realm_option (realm varchar(127) default '',\topt varchar(32),\tvalue varchar(128),\tprimary key (realm,opt))"},
	{"oauth_key", "CREATE TABLE oauth_key (kid varchar(128),ikm_key varchar(256),timestamp bigint default 0,lifetime integer default 0,as_rs_alg varchar(64) default '',realm varchar(127) default '',primary key (kid))"},
	{"admin_user", "CREATE TABLE admin_user (name varchar(32), realm varchar(127), password varchar(127), primary key (name))"},
}

// Store is coturn's SQLite database, changed in place with journal_mode=DELETE.
//
// The file is never rename-replaced: coturn caches one connection per thread
// and would keep reading the old inode forever. WAL is never used because a
// WAL reader needs a writable -shm file, which coturn's read-only mount lacks.
type Store struct{ db *sql.DB }

// OpenStore opens (creating if absent) the database file at path. The parent
// directory must be private and the file a regular, non-group/other-writable
// file the process can open for writing: a database the writer cannot write
// is refused here rather than discovered after a false success.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("managedturn: resolve database path: %w", err)
	}
	if err := checkPrivateDir(filepath.Dir(abs)); err != nil {
		return nil, err
	}
	if err := ensureDBFile(abs); err != nil {
		return nil, err
	}
	dsn := (&url.URL{
		Scheme: "file",
		Path:   abs,
		RawQuery: url.Values{
			"mode":          {"rw"}, // never let SQLite create the file with its own mode
			"_busy_timeout": {fmt.Sprint(storeBusyTimeoutMS)},
			"_journal_mode": {"DELETE"},
			"_txlock":       {"immediate"},
			// Zero freed pages, so a deleted key does not stay readable by
			// coturn's group in the file's free list.
			"_pragma": {"secure_delete(1)"},
		}.Encode(),
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("managedturn: open database: %w", err)
	}
	// One connection: every statement sees the same pragmas and the writer
	// never contends with itself.
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return nil, errors.Join(fmt.Errorf("managedturn: read journal mode: %w", err), closeDB(db))
	}
	if mode != "delete" {
		return nil, errors.Join(fmt.Errorf("%w: journal mode is not DELETE", ErrInvalidConfig), closeDB(db))
	}
	// The DSN pragma is applied silently; read it back so a driver that
	// ignores it cannot leave deleted keys in the free list unnoticed.
	var secureDelete int
	if err := db.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&secureDelete); err != nil {
		return nil, errors.Join(fmt.Errorf("managedturn: read secure_delete: %w", err), closeDB(db))
	}
	if secureDelete != 1 {
		return nil, errors.Join(fmt.Errorf("%w: secure_delete is not on", ErrInvalidConfig), closeDB(db))
	}
	return &Store{db: db}, nil
}

// ensureDBFile creates path with dbFileMode if absent, and otherwise proves it
// is a regular file this process can open read-write.
func ensureDBFile(path string) (err error) {
	const flags = os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	f, err := os.OpenFile(path, flags, 0) // #nosec G304 -- configured database path
	if errors.Is(err, fs.ErrNotExist) {
		f, err = os.OpenFile(path, flags|os.O_CREATE|os.O_EXCL, dbFileMode) // #nosec G304 -- configured database path
		if err == nil {
			// Beat the umask: coturn must be able to read the file.
			err = f.Chmod(dbFileMode)
		}
	}
	if errors.Is(err, syscall.ELOOP) {
		return ErrUnsafePermission
	}
	if err != nil {
		openErr := fmt.Errorf("managedturn: open database file for writing: %w", err)
		if f != nil {
			openErr = errors.Join(openErr, closeFile(f))
		}
		return openErr
	}
	defer func() { err = errors.Join(err, closeFile(f)) }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("managedturn: stat database file: %w", err)
	}
	perm := info.Mode().Perm()
	if !info.Mode().IsRegular() || !ownedByEUID(info) || perm&dbRefusedBits != 0 {
		return ErrUnsafePermission
	}
	if perm&0o004 != 0 {
		// Other-readable but otherwise safe: tighten in place (we own the file
		// and hold it open read-write) rather than run with the keys exposed.
		if err := f.Chmod(perm &^ 0o007); err != nil {
			return fmt.Errorf("managedturn: restrict database file mode: %w", err)
		}
	}
	return nil
}

// closeFile closes f, reporting a failed close.
func closeFile(f *os.File) error {
	if err := f.Close(); err != nil {
		return fmt.Errorf("managedturn: close file: %w", err)
	}
	return nil
}

// closeDB closes db on an error path, reporting a failed close as well.
func closeDB(db *sql.DB) error {
	if err := db.Close(); err != nil {
		return fmt.Errorf("managedturn: close database: %w", err)
	}
	return nil
}

// rollback ends tx unless it already committed, joining a failed rollback
// into the caller's error.
func rollback(tx *sql.Tx, err *error) {
	if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
		*err = errors.Join(*err, fmt.Errorf("managedturn: rollback: %w", rbErr))
	}
}

// closeRows closes rows, joining a failed close into the caller's error.
func closeRows(rows *sql.Rows, err *error) {
	if cErr := rows.Close(); cErr != nil {
		*err = errors.Join(*err, fmt.Errorf("managedturn: close rows: %w", cErr))
	}
}

// dbRefusedBits are never acceptable on the database: group write or execute,
// and any write or execute for other users. Other-read is tightened instead.
const dbRefusedBits os.FileMode = 0o033

// InitSchema creates coturn 4.18.0's eight tables exactly as coturn would, if
// absent. It never changes rows. Unlike coturn it does not ignore failures: a
// table whose stored DDL differs, or any object coturn would not create, is
// refused so that drift or tampering cannot hide behind a successful exit.
func (s *Store) InitSchema(ctx context.Context) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("managedturn: begin schema transaction: %w", err)
	}
	defer rollback(tx, &err)

	existing, err := schemaObjects(ctx, tx)
	if err != nil {
		return err
	}
	for _, t := range coturnSchema {
		ddl, ok := existing[t.table]
		if ok {
			if ddl != t.ddl {
				return fmt.Errorf("%w: table %s does not match coturn's schema", ErrInvalidConfig, t.table)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, t.ddl); err != nil {
			return fmt.Errorf("managedturn: create table %s: %w", t.table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("managedturn: commit schema: %w", err)
	}
	return nil
}

// schemaObjects returns the DDL of every table, refusing views, triggers,
// stand-alone indexes and tables that coturn does not define.
func schemaObjects(ctx context.Context, tx *sql.Tx) (_ map[string]string, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT type, name, tbl_name, sql FROM sqlite_master`)
	if err != nil {
		return nil, fmt.Errorf("managedturn: read schema: %w", err)
	}
	defer closeRows(rows, &err)
	known := make(map[string]bool, len(coturnSchema))
	for _, t := range coturnSchema {
		known[t.table] = true
	}
	tables := map[string]string{}
	for rows.Next() {
		var typ, name, tbl string
		var ddl sql.NullString
		if err := rows.Scan(&typ, &name, &tbl, &ddl); err != nil {
			return nil, fmt.Errorf("managedturn: scan schema: %w", err)
		}
		switch {
		case typ == "table" && known[name]:
			tables[name] = ddl.String
		case typ == "index" && known[tbl] && !ddl.Valid:
			// sqlite_autoindex_* backing a PRIMARY KEY: created implicitly.
		default:
			return nil, fmt.Errorf("%w: unexpected schema object", ErrInvalidConfig)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("managedturn: read schema: %w", err)
	}
	return tables, nil
}

// KeyIDs returns the sorted IDs of the turn_secret rows for realm. A stored
// value that is not a valid key is an error, never silently skipped.
func (s *Store) KeyIDs(ctx context.Context, realm string) ([]string, error) {
	return keyIDs(ctx, s.db, realm)
}

// queryer is the read surface shared by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func keyIDs(ctx context.Context, q queryer, realm string) (_ []string, err error) {
	// The exact statement coturn issues per authentication.
	rows, err := q.QueryContext(ctx, `select value from turn_secret where realm=?`, realm)
	if err != nil {
		return nil, fmt.Errorf("managedturn: read turn_secret: %w", err)
	}
	defer closeRows(rows, &err)
	ids := []string{}
	for rows.Next() {
		var v sql.RawBytes
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("managedturn: scan turn_secret: %w", err)
		}
		k, err := ParseKey(v)
		if err != nil {
			return nil, fmt.Errorf("%w: database holds an invalid turn_secret value", ErrInvalidKey)
		}
		ids = append(ids, k.ID())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("managedturn: read turn_secret: %w", err)
	}
	slices.Sort(ids)
	return ids, nil
}

// rowCount counts the turn_secret rows for realm.
func (s *Store) rowCount(ctx context.Context, realm string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM turn_secret WHERE realm = ?`, realm).Scan(&n); err != nil {
		return 0, fmt.Errorf("managedturn: count turn_secret: %w", err)
	}
	return n, nil
}

// Insert adds key for realm in one BEGIN IMMEDIATE transaction. Inserting a
// key that is already present is a no-op that still proves the database is
// writable: an INSERT on a read-only database fails even when it would change
// nothing, which a bare BEGIN IMMEDIATE does not.
func (s *Store) Insert(ctx context.Context, realm string, key Key) error {
	return s.insertChecked(ctx, realm, []Key{key}, nil)
}

// insertChecked inserts keys in one BEGIN IMMEDIATE transaction. check, when
// set, sees the sorted row IDs present before the insert and may veto the
// whole transaction. After inserting, every key must read back present.
func (s *Store) insertChecked(ctx context.Context, realm string, keys []Key, check func(before []string) error) (err error) {
	if err := validateRealm(realm); err != nil {
		return err
	}
	if len(keys) == 0 {
		return ErrInvalidKey
	}
	for _, k := range keys {
		if !k.valid() {
			return ErrInvalidKey
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("managedturn: begin write transaction: %w", err)
	}
	defer rollback(tx, &err)

	if check != nil {
		before, err := keyIDs(ctx, tx, realm)
		if err != nil {
			return err
		}
		if err := check(before); err != nil {
			return err
		}
	}
	for _, k := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO turn_secret (realm, value) VALUES (?, ?)`, realm, k.value); err != nil {
			return fmt.Errorf("managedturn: insert turn_secret row: %w", err)
		}
	}
	after, err := keyIDs(ctx, tx, realm)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, found := slices.BinarySearch(after, k.ID()); !found {
			return fmt.Errorf("%w: inserted key did not read back", ErrStoreMismatch)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("managedturn: commit turn_secret: %w", err)
	}
	return nil
}

// deleteRows removes, in one short BEGIN IMMEDIATE transaction, every
// turn_secret row for realm for which drop returns true. drop sees the row's
// key ID, or "" for a stored value that is not a valid key. It returns the
// number of rows removed.
func (s *Store) deleteRows(ctx context.Context, realm string, drop func(id string) bool) (_ int, err error) {
	if err := validateRealm(realm); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("managedturn: begin delete transaction: %w", err)
	}
	defer rollback(tx, &err)

	doomed, err := doomedRows(ctx, tx, realm, drop)
	if err != nil {
		return 0, err
	}
	for _, rowid := range doomed {
		// By rowid, so a row is removed exactly as it was judged, whatever
		// its stored type.
		if _, err := tx.ExecContext(ctx, `DELETE FROM turn_secret WHERE rowid = ? AND realm = ?`, rowid, realm); err != nil {
			return 0, fmt.Errorf("managedturn: delete turn_secret row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("managedturn: commit turn_secret delete: %w", err)
	}
	return len(doomed), nil
}

// doomedRows returns the rowids of realm's rows that drop selects. The rows
// are closed before it returns, so the caller can delete on the same tx.
func doomedRows(ctx context.Context, tx *sql.Tx, realm string, drop func(id string) bool) (_ []int64, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT rowid, value FROM turn_secret WHERE realm = ?`, realm)
	if err != nil {
		return nil, fmt.Errorf("managedturn: read turn_secret: %w", err)
	}
	defer closeRows(rows, &err)
	var doomed []int64
	for rows.Next() {
		var rowid int64
		var v sql.RawBytes
		if err := rows.Scan(&rowid, &v); err != nil {
			return nil, fmt.Errorf("managedturn: scan turn_secret: %w", err)
		}
		id := ""
		if k, perr := ParseKey(v); perr == nil {
			id = k.ID()
		}
		if drop(id) {
			doomed = append(doomed, rowid)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("managedturn: read turn_secret: %w", err)
	}
	return doomed, nil
}

// validateRealm accepts what fits turn_secret.realm: 1–127 printable,
// non-space ASCII bytes. Rows are written under the configured realm exactly.
func validateRealm(realm string) error {
	if realm == "" || len(realm) > MaxKeyLen {
		return fmt.Errorf("%w: realm length", ErrInvalidConfig)
	}
	for i := 0; i < len(realm); i++ {
		if realm[i] < 0x21 || realm[i] > 0x7e {
			return fmt.Errorf("%w: realm characters", ErrInvalidConfig)
		}
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }
