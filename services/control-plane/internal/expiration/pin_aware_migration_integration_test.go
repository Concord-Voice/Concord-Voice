//go:build integration

package expiration_test

import (
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// pinAwareExpiryUp is migration 000168 (#3459). Renumber here if it moves at
// merge time.
const pinAwareExpiryUp uint = 168

// TestPinAwareExpiryMigration_MetadataOnlyAndRoundTrip pins AC12 for 000168:
// the migration is metadata-only (no table rewrite), existing timers keep
// expiring pinned messages, the NOT VALID CHECK still enforces new writes, and
// up -> down -> up leaves pg_constraint, pg_attribute (surviving columns) and
// pg_index exactly as they were.
func TestPinAwareExpiryMigration_MetadataOnlyAndRoundTrip(t *testing.T) {
	db, m := newPinAwareMigrationDB(t)
	require.NoError(t, m.Migrate(pinAwarePriorVersion(t, pinAwareExpiryUp)), "migrate to the pre-000168 schema")

	owner, other := pinAwareUser(t, db), pinAwareUser(t, db)
	serverID := pinAwareServer(t, db, owner)
	channelID := pinAwareChannel(t, db, serverID, 86400, 4)
	bareChannelID := pinAwareChannel(t, db, serverID, 0, 0)
	conversationID := pinAwareConversation(t, db, owner, other, 3600, 2)

	future := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond)
	pinnedMsg := pinAwareMessage(t, db, channelID, owner, true, &future)
	plainMsg := pinAwareMessage(t, db, channelID, owner, false, &future)
	pinnedDM := pinAwareDMMessage(t, db, conversationID, owner, true, &future)

	nodesBefore := pinAwareFilenodes(t, db)
	schemaBefore := pinAwareSchema(t, db)

	require.NoError(t, m.Migrate(pinAwareExpiryUp), "000168 must apply")
	require.Equal(t, nodesBefore, pinAwareFilenodes(t, db), "000168 must be metadata-only: no table may be rewritten")
	schemaAfterUp := pinAwareSchema(t, db)
	require.NotEqual(t, schemaBefore, schemaAfterUp)

	// Existing timers keep expiring pins; nothing was stamped, moved or held.
	for _, id := range []string{channelID, bareChannelID} {
		require.True(t, pinAwareChannelInclude(t, db, id), "a pre-000168 channel must read include=TRUE")
	}
	require.True(t, pinAwareConversationInclude(t, db, conversationID), "a pre-000168 conversation must read include=TRUE")
	for _, id := range []string{pinnedMsg, plainMsg} {
		expires, held := pinAwareStamps(t, db, "messages", id)
		pinAwareRequireStamp(t, expires, &future, "expires_at must be untouched by a first apply")
		pinAwareRequireStamp(t, held, nil, "held_expires_at must start NULL")
	}
	expires, held := pinAwareStamps(t, db, "dm_messages", pinnedDM)
	pinAwareRequireStamp(t, expires, &future, "dm expires_at must be untouched by a first apply")
	pinAwareRequireStamp(t, held, nil, "dm held_expires_at must start NULL")
	require.Zero(t, pinAwareEvidenceRows(t, db), "a first apply creates the evidence tables empty")

	// The held column is never indexed (C-DDL), and the CHECK is permanently
	// NOT VALID yet enforced for every new write.
	var heldIndexes int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_indexes
		WHERE tablename IN ('messages', 'dm_messages') AND indexdef LIKE '%held_expires_at%'`).Scan(&heldIndexes))
	require.Zero(t, heldIndexes, "held_expires_at must carry no index")
	for _, c := range []struct{ table, name string }{
		{"messages", "messages_held_expires_at_shape_check"},
		{"dm_messages", "dm_messages_held_expires_at_shape_check"},
	} {
		var validated bool
		require.NoError(t, db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conrelid = $1::regclass AND conname = $2`,
			c.table, c.name).Scan(&validated))
		require.False(t, validated, "%s must stay NOT VALID", c.name)
	}
	_, err := db.Exec(`UPDATE messages SET held_expires_at = $2 WHERE id = $1`, plainMsg, future)
	pinAwareRequireCheckViolation(t, err, "an unpinned message may not carry a held stamp")
	_, err = db.Exec(`UPDATE messages SET held_expires_at = $2 WHERE id = $1`, pinnedMsg, future)
	pinAwareRequireCheckViolation(t, err, "a pinned message may not carry both expires_at and held_expires_at")
	_, err = db.Exec(`UPDATE messages SET held_expires_at = expires_at, expires_at = NULL WHERE id = $1`, pinnedMsg)
	require.NoError(t, err, "a pinned message with a NULL expires_at may hold a stamp")
	_, err = db.Exec(`UPDATE dm_messages SET held_expires_at = $2 WHERE id = $1`, pinnedDM, future)
	pinAwareRequireCheckViolation(t, err, "a pinned DM message may not carry both expires_at and held_expires_at")
	_, err = db.Exec(`UPDATE messages SET held_expires_at = NULL, expires_at = $2 WHERE id = $1`, pinnedMsg, future)
	require.NoError(t, err)

	// down -> up restores the schema exactly.
	require.NoError(t, m.Steps(-1), "000168 down must apply")
	require.Equal(t, schemaBefore, pinAwareSchema(t, db), "down leaves pg_constraint, pg_attribute and pg_index as they were")
	require.NoError(t, m.Steps(1), "000168 re-applies")
	require.Equal(t, schemaAfterUp, pinAwareSchema(t, db), "up after down restores the same schema")
	require.True(t, pinAwareChannelInclude(t, db, channelID))
	require.Zero(t, pinAwareEvidenceRows(t, db), "an up consumes the evidence a down wrote")
}

// TestPinAwareExpiryMigration_DownUpRestoresKeepPins pins C-DOWN and restore
// steps (a)-(d): down -> up neither relabels a keep-pins context nor drops a
// held stamp, a revision that moved in between blocks the relabel and the
// re-arm of that context's held stamps, a pin made
// while rolled back is normalised into held, a stamp already due is never
// moved, and the consumed evidence is deleted.
func TestPinAwareExpiryMigration_DownUpRestoresKeepPins(t *testing.T) {
	db, m := newPinAwareMigrationDB(t)
	require.NoError(t, m.Migrate(pinAwareExpiryUp), "000168 must apply")

	owner, other := pinAwareUser(t, db), pinAwareUser(t, db)
	serverID := pinAwareServer(t, db, owner)
	now := time.Now().UTC().Truncate(time.Microsecond)
	heldAt := func(hours int) time.Time { return now.Add(time.Duration(hours) * time.Hour) }

	// K1: keeps pins, revision will not move. K2: keeps pins, revision WILL
	// move. N: expires pins (control). DK: a keep-pins conversation.
	k1 := pinAwareChannel(t, db, serverID, 86400, 7)
	k2 := pinAwareChannel(t, db, serverID, 86400, 3)
	k3 := pinAwareChannel(t, db, serverID, 86400, 9)
	n := pinAwareChannel(t, db, serverID, 86400, 1)
	dk := pinAwareConversation(t, db, owner, other, 86400, 5)
	for _, id := range []string{k1, k2, k3} {
		_, err := db.Exec(`UPDATE channels SET expiration_include_pinned = FALSE WHERE id = $1`, id)
		require.NoError(t, err)
	}
	_, err := db.Exec(`UPDATE dm_conversations SET expiration_include_pinned = FALSE WHERE id = $1`, dk)
	require.NoError(t, err)

	h1, h2, h3, h4 := heldAt(10), heldAt(20), heldAt(30), heldAt(40)
	u1Stamp, nStamp := heldAt(50), heldAt(60)
	held1 := pinAwareHeldMessage(t, db, "messages", k1, owner, h1)
	held2 := pinAwareHeldMessage(t, db, "messages", k2, owner, h2)
	held3 := pinAwareHeldMessage(t, db, "dm_messages", dk, owner, h3)
	held4 := pinAwareHeldMessage(t, db, "messages", k1, owner, h4)
	unpinned := pinAwareMessage(t, db, k1, owner, false, &u1Stamp)
	controlPin := pinAwareMessage(t, db, n, owner, true, &nStamp)
	// H5's held stamp is already past: re-arming it would delete the message.
	h5 := now.Add(-2 * time.Hour)
	held5 := pinAwareHeldMessage(t, db, "messages", k3, owner, h5)

	require.NoError(t, m.Steps(-1), "000168 down must apply")
	var heldColumns int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_name IN ('channels', 'dm_conversations', 'messages', 'dm_messages')
		  AND column_name IN ('held_expires_at', 'expiration_include_pinned')`).Scan(&heldColumns))
	require.Zero(t, heldColumns, "down drops all four columns")

	// The down wrote exactly the K contexts and the held stamps.
	require.Equal(t, 4, pinAwareCount(t, db, `SELECT count(*) FROM expiration_keep_pins_evidence`))
	require.Equal(t, 5, pinAwareCount(t, db, `SELECT count(*) FROM expiration_held_stamps_evidence`))
	for _, e := range []struct {
		kind, id string
		revision int64
	}{{"channel", k1, 7}, {"channel", k2, 3}, {"conversation", dk, 5}} {
		var revision int64
		require.NoError(t, db.QueryRow(`SELECT expiration_revision FROM expiration_keep_pins_evidence
			WHERE context_kind = $1 AND context_id = $2`, e.kind, e.id).Scan(&revision), "%s %s must be recorded", e.kind, e.id)
		require.Equal(t, e.revision, revision)
	}
	var controlRecorded int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM expiration_keep_pins_evidence WHERE context_id = $1`, n).Scan(&controlRecorded))
	require.Zero(t, controlRecorded, "a context that expires pins is not recorded")

	// What the OLD binary does while rolled back.
	_, err = db.Exec(`UPDATE channels SET expiration_revision = 4 WHERE id = $1`, k2)
	require.NoError(t, err, "a user changed K2's policy while rolled back")
	// K3: the old binary clears the timer (clear_pending), then sets a new one
	// with new_only. Each advances the revision; neither stamps existing rows.
	_, err = db.Exec(`UPDATE channels SET expiration_window_seconds = NULL, expiration_revision = 10 WHERE id = $1`, k3)
	require.NoError(t, err, "a user cleared K3's timer while rolled back")
	_, err = db.Exec(`UPDATE channels SET expiration_window_seconds = 3600, expiration_revision = 11 WHERE id = $1`, k3)
	require.NoError(t, err, "a user set a new K3 timer, new_only, while rolled back")
	pinFuture := heldAt(70)
	pinPast := now.Add(-48 * time.Hour)
	pinDMFuture := heldAt(80)
	lateFuture := pinAwareMessage(t, db, k1, owner, true, &pinFuture)
	latePast := pinAwareMessage(t, db, k1, owner, true, &pinPast)
	lateDM := pinAwareDMMessage(t, db, dk, owner, true, &pinDMFuture)
	_, err = db.Exec(`UPDATE messages SET pinned_at = NULL WHERE id = $1`, held4)
	require.NoError(t, err, "a held pin was unpinned while rolled back")

	require.NoError(t, m.Steps(1), "000168 re-applies")

	// (a) relabel only where the revision is unchanged.
	require.False(t, pinAwareChannelInclude(t, db, k1), "K1 must still keep pins")
	require.False(t, pinAwareConversationInclude(t, db, dk), "the keep-pins conversation must still keep pins")
	require.True(t, pinAwareChannelInclude(t, db, k2), "K2's revision moved, so it must NOT be relabelled")
	require.True(t, pinAwareChannelInclude(t, db, n))

	// (b) held stamps return to held where the context still keeps pins ...
	expires, held := pinAwareStamps(t, db, "messages", held1)
	pinAwareRequireStamp(t, expires, nil, "H1 stays unscheduled")
	pinAwareRequireStamp(t, held, &h1, "H1 must get its held stamp back")
	expires, held = pinAwareStamps(t, db, "dm_messages", held3)
	pinAwareRequireStamp(t, expires, nil, "H3 stays unscheduled")
	pinAwareRequireStamp(t, held, &h3, "H3 (DM) must get its held stamp back")
	// ... are dropped where the policy moved while rolled back, so the old
	// binary's action stands (Codex review of #3459) ...
	expires, held = pinAwareStamps(t, db, "messages", held2)
	pinAwareRequireStamp(t, expires, nil, "H2's policy moved while rolled back, so its stamp is not re-armed")
	pinAwareRequireStamp(t, held, nil, "H2 must not stay held")
	expires, held = pinAwareStamps(t, db, "messages", held5)
	pinAwareRequireStamp(t, expires, nil, "a timer cleared and set new_only while rolled back must not resurrect H5's stamp")
	pinAwareRequireStamp(t, held, nil, "H5 must not stay held")
	// ... and re-arm where the deferral's condition ended during the rollback.
	expires, held = pinAwareStamps(t, db, "messages", held4)
	pinAwareRequireStamp(t, expires, &h4, "H4 was unpinned while rolled back, so its stamp is re-armed")
	pinAwareRequireStamp(t, held, nil, "H4 must not stay held")

	// (c) pins made while rolled back: a future stamp moves into held, a due
	// stamp is never moved (C-NORES), an expiring context is untouched.
	expires, held = pinAwareStamps(t, db, "messages", lateFuture)
	pinAwareRequireStamp(t, expires, nil, "a future stamp on a pin in a K context must move out of expires_at")
	pinAwareRequireStamp(t, held, &pinFuture, "a future stamp on a pin in a K context must move into held")
	expires, held = pinAwareStamps(t, db, "dm_messages", lateDM)
	pinAwareRequireStamp(t, expires, nil, "the same normalisation applies to a DM pin")
	pinAwareRequireStamp(t, held, &pinDMFuture, "the same normalisation applies to a DM pin")
	expires, held = pinAwareStamps(t, db, "messages", latePast)
	pinAwareRequireStamp(t, expires, &pinPast, "a past stamp must never be moved")
	pinAwareRequireStamp(t, held, nil, "a past stamp must never be held")
	expires, held = pinAwareStamps(t, db, "messages", unpinned)
	pinAwareRequireStamp(t, expires, &u1Stamp, "an unpinned message is never touched")
	pinAwareRequireStamp(t, held, nil, "an unpinned message is never held")
	expires, held = pinAwareStamps(t, db, "messages", controlPin)
	pinAwareRequireStamp(t, expires, &nStamp, "a pin in a context that expires pins is never touched")
	pinAwareRequireStamp(t, held, nil, "a pin in a context that expires pins is never held")

	// (d) consumed evidence is deleted.
	require.Zero(t, pinAwareEvidenceRows(t, db), "the up must delete the evidence it consumed")
}

// newPinAwareMigrationDB creates a throwaway database so the round trip never
// moves the shared test database's schema version. Same shape as
// runIsolatedMigrationRoundTrip in migration_integration_test.go.
func newPinAwareMigrationDB(t *testing.T) (*sql.DB, *migrate.Migrate) {
	t.Helper()
	baseURL := testdb.DatabaseURL()
	parsed, err := url.Parse(baseURL)
	require.NoError(t, err)
	databaseName := "concord_pin_aware_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedName := pq.QuoteIdentifier(databaseName)
	adminDB, err := sql.Open("postgres", baseURL)
	require.NoError(t, err)
	t.Cleanup(func() {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- UUID-derived database name is safely quoted with pq.QuoteIdentifier.
		if _, cleanupErr := adminDB.Exec(`DROP DATABASE IF EXISTS ` + quotedName + ` WITH (FORCE)`); cleanupErr != nil {
			t.Errorf("drop disposable database: %v", cleanupErr)
		}
		if cleanupErr := adminDB.Close(); cleanupErr != nil {
			t.Errorf("close disposable database admin connection: %v", cleanupErr)
		}
	})
	require.NoError(t, adminDB.Ping())
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- UUID-derived database name is safely quoted with pq.QuoteIdentifier.
	_, err = adminDB.Exec(`CREATE DATABASE ` + quotedName)
	require.NoError(t, err)

	parsed.Path = "/" + databaseName
	db, err := sql.Open("postgres", parsed.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		if cleanupErr := db.Close(); cleanupErr != nil {
			t.Errorf("close disposable migration database: %v", cleanupErr)
		}
	})
	require.NoError(t, db.Ping())

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	migrationDir := filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithDatabaseInstance("file://"+migrationDir, "postgres", driver)
	require.NoError(t, err)
	t.Cleanup(func() {
		sourceErr, databaseErr := m.Close()
		if sourceErr != nil {
			t.Errorf("close disposable migration source: %v", sourceErr)
		}
		if databaseErr != nil {
			t.Errorf("close disposable migration database driver: %v", databaseErr)
		}
	})
	return db, m
}

// pinAwarePriorVersion returns the highest migration version below v, read from
// the migrations directory so no neighbour's number is hard-coded.
func pinAwarePriorVersion(t *testing.T, v uint) uint {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	ups, err := filepath.Glob(filepath.Join(filepath.Dir(filename), "..", "..", "migrations", "*.up.sql"))
	require.NoError(t, err)
	var prior uint
	for _, path := range ups {
		n, convErr := strconv.ParseUint(strings.SplitN(filepath.Base(path), "_", 2)[0], 10, 64)
		require.NoError(t, convErr)
		if uint(n) < v && uint(n) > prior {
			prior = uint(n)
		}
	}
	require.NotZero(t, prior)
	return prior
}

func pinAwareUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES ($1, $2 || '@test.local', 'u_' || left($2, 8), 'x', true, true)`, id, id)
	require.NoError(t, err)
	return id
}

func pinAwareServer(t *testing.T, db *sql.DB, ownerID string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'pin aware migration server', $2)`, id, ownerID)
	require.NoError(t, err)
	return id
}

// pinAwareChannel inserts a text channel. A zero window leaves expiry off.
func pinAwareChannel(t *testing.T, db *sql.DB, serverID string, window int, revision int64) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'pin aware channel', 'text')`, id, serverID)
	require.NoError(t, err)
	if window > 0 {
		_, err = db.Exec(`UPDATE channels SET expiration_window_seconds = $2, expiration_revision = $3 WHERE id = $1`, id, window, revision)
		require.NoError(t, err)
	}
	return id
}

// pinAwareConversation inserts a personal DM conversation with two participants.
func pinAwareConversation(t *testing.T, db *sql.DB, owner, other string, window int, revision int64) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, false, true, $2)`, id, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, id, owner, other)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE dm_conversations SET expiration_window_seconds = $2, expiration_revision = $3 WHERE id = $1`, id, window, revision)
	require.NoError(t, err)
	return id
}

// pinAwareMessage inserts a channel message using only columns that exist
// before 000168. A pinned message gets pinned_at = now.
func pinAwareMessage(t *testing.T, db *sql.DB, channelID, userID string, pinned bool, expires *time.Time) string {
	t.Helper()
	id := uuid.NewString()
	var pinnedAt *time.Time
	if pinned {
		now := time.Now().UTC()
		pinnedAt = &now
	}
	_, err := db.Exec(`INSERT INTO messages (id, channel_id, user_id, content, created_at, pinned_at, expires_at)
		VALUES ($1, $2, $3, 'ciphertext', now(), $4, $5)`, id, channelID, userID, pinnedAt, expires)
	require.NoError(t, err)
	return id
}

func pinAwareDMMessage(t *testing.T, db *sql.DB, conversationID, userID string, pinned bool, expires *time.Time) string {
	t.Helper()
	id := uuid.NewString()
	var pinnedAt *time.Time
	if pinned {
		now := time.Now().UTC()
		pinnedAt = &now
	}
	_, err := db.Exec(`INSERT INTO dm_messages (id, conversation_id, user_id, content, type, created_at, pinned_at, expires_at)
		VALUES ($1, $2, $3, 'ciphertext', 'user', now(), $4, $5)`, id, conversationID, userID, pinnedAt, expires)
	require.NoError(t, err)
	return id
}

// pinAwareHeldMessage inserts a pinned message that holds a stamp. It needs
// the 000168 schema. table is "messages" or "dm_messages".
func pinAwareHeldMessage(t *testing.T, db *sql.DB, table, parentID, userID string, held time.Time) string {
	t.Helper()
	var id string
	switch table {
	case "messages":
		id = pinAwareMessage(t, db, parentID, userID, true, nil)
		_, err := db.Exec(`UPDATE messages SET held_expires_at = $2 WHERE id = $1`, id, held)
		require.NoError(t, err)
	case "dm_messages":
		id = pinAwareDMMessage(t, db, parentID, userID, true, nil)
		_, err := db.Exec(`UPDATE dm_messages SET held_expires_at = $2 WHERE id = $1`, id, held)
		require.NoError(t, err)
	default:
		t.Fatalf("unsupported table %q", table)
	}
	return id
}

// pinAwareStamps returns expires_at and held_expires_at for a message. It reads
// held_expires_at, so it needs the 000168 schema.
func pinAwareStamps(t *testing.T, db *sql.DB, table, id string) (expires, held sql.NullTime) {
	t.Helper()
	var row *sql.Row
	switch table {
	case "messages":
		row = db.QueryRow(`SELECT expires_at, held_expires_at FROM messages WHERE id = $1`, id)
	case "dm_messages":
		row = db.QueryRow(`SELECT expires_at, held_expires_at FROM dm_messages WHERE id = $1`, id)
	default:
		t.Fatalf("unsupported table %q", table)
	}
	require.NoError(t, row.Scan(&expires, &held))
	return expires, held
}

func pinAwareRequireStamp(t *testing.T, got sql.NullTime, want *time.Time, msg string) {
	t.Helper()
	if want == nil {
		require.False(t, got.Valid, msg)
		return
	}
	require.True(t, got.Valid, msg)
	require.True(t, want.Equal(got.Time), "%s: want %s, got %s", msg, want.Format(time.RFC3339Nano), got.Time.Format(time.RFC3339Nano))
}

func pinAwareRequireCheckViolation(t *testing.T, err error, msg string) {
	t.Helper()
	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr), "%s: want a check violation, got %v", msg, err)
	require.Equal(t, pq.ErrorCode("23514"), pqErr.Code, msg)
}

func pinAwareChannelInclude(t *testing.T, db *sql.DB, id string) bool {
	t.Helper()
	var include bool
	require.NoError(t, db.QueryRow(`SELECT expiration_include_pinned FROM channels WHERE id = $1`, id).Scan(&include))
	return include
}

func pinAwareConversationInclude(t *testing.T, db *sql.DB, id string) bool {
	t.Helper()
	var include bool
	require.NoError(t, db.QueryRow(`SELECT expiration_include_pinned FROM dm_conversations WHERE id = $1`, id).Scan(&include))
	return include
}

func pinAwareCount(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query).Scan(&n))
	return n
}

// pinAwareEvidenceRows counts the rows in both rollback-evidence tables.
func pinAwareEvidenceRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	return pinAwareCount(t, db, `SELECT (SELECT count(*) FROM expiration_keep_pins_evidence)
		+ (SELECT count(*) FROM expiration_held_stamps_evidence)`)
}

// pinAwareFilenodes returns pg_relation_filenode for the four tables 000168
// alters. An ALTER that rewrites a table assigns it a new filenode.
func pinAwareFilenodes(t *testing.T, db *sql.DB) string {
	t.Helper()
	var nodes string
	require.NoError(t, db.QueryRow(`SELECT string_agg(c.relname || '=' || pg_relation_filenode(c.oid)::text, ',' ORDER BY c.relname)
		FROM pg_class c
		WHERE c.relkind = 'r' AND c.relname IN ('messages', 'dm_messages', 'channels', 'dm_conversations')`).Scan(&nodes))
	return nodes
}

// pinAwareSchema returns a comparable snapshot of the four altered tables:
// every constraint (definition and convalidated), every surviving column
// (type, nullability, default; attnum is excluded because a dropped and
// re-added column legitimately gets a new slot) and every index.
func pinAwareSchema(t *testing.T, db *sql.DB) string {
	t.Helper()
	var constraints, attributes, indexes string
	require.NoError(t, db.QueryRow(`
		SELECT COALESCE(string_agg(c.relname || '.' || k.conname || ' ' || pg_get_constraintdef(k.oid) || ' validated=' || k.convalidated,
			E'\n' ORDER BY c.relname, k.conname), '')
		FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid
		WHERE c.relkind = 'r' AND c.relname IN ('messages', 'dm_messages', 'channels', 'dm_conversations')`).Scan(&constraints))
	require.NoError(t, db.QueryRow(`
		SELECT COALESCE(string_agg(c.relname || '.' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
			|| ' notnull=' || a.attnotnull || ' default=' || COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
			E'\n' ORDER BY c.relname, a.attname), '')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE c.relkind = 'r' AND c.relname IN ('messages', 'dm_messages', 'channels', 'dm_conversations')
		  AND a.attnum > 0 AND NOT a.attisdropped`).Scan(&attributes))
	require.NoError(t, db.QueryRow(`
		SELECT COALESCE(string_agg(tablename || '.' || indexname || ' ' || indexdef, E'\n' ORDER BY tablename, indexname), '')
		FROM pg_indexes
		WHERE tablename IN ('messages', 'dm_messages', 'channels', 'dm_conversations')`).Scan(&indexes))
	return constraints + "\n--\n" + attributes + "\n--\n" + indexes
}
