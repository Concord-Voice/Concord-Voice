package members

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // register file:// source driver for golang-migrate
	"github.com/google/uuid"
	_ "github.com/lib/pq" // register postgres driver for database/sql
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// This is an internal (package members) test because triggerKeyRevocationForChannel
// is unexported. It cannot use internal/testhelpers (testhelpers -> api -> members
// import cycle), so it sets up the DB/Redis/Hub directly, mirroring the no-import-cycle
// pattern in internal/websocket/hub_epoch_test.go.

var (
	krMigrateOnce sync.Once
	krMigrateErr  error
)

// krTestDBPassword mirrors the assembled-from-parts pattern in
// testhelpers/testdb.go to satisfy static credential analysis (S6698/S2068). It is a
// dev-only default matching docker-compose; production always sets DATABASE_URL.
// The Redis counterpart is gone — redistest owns Redis URL resolution (#2680).
var krTestDBPassword = "concord_dev_password" //nolint:gosec // matches docker-compose dev default // pragma: allowlist secret

func krMigrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	// members/ is at internal/members/keyrotation_test.go; migrations/ is at repo migrations/.
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

func krRunMigrations(db *sql.DB) error {
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("could not create migration driver: %w", err)
	}
	m, err := migrate.NewWithDatabaseInstance(
		fmt.Sprintf("file://%s", krMigrationsPath()), "postgres", driver,
	)
	if err != nil {
		return fmt.Errorf("could not create migrate instance: %w", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("could not run migrations: %w", err)
	}
	return nil
}

func krSetupDB(t *testing.T) *sql.DB {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://concord:" + krTestDBPassword + "@localhost:5432/concord?sslmode=disable" //nolint:gosec
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("keyrotation_test: failed to open database: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("keyrotation_test: failed to ping database: %v", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)

	krMigrateOnce.Do(func() { krMigrateErr = krRunMigrations(db) })
	if krMigrateErr != nil {
		t.Fatalf("keyrotation_test: migration failed: %v", krMigrateErr)
	}

	t.Cleanup(func() {
		if _, err := db.Exec(`TRUNCATE users, servers, server_members, channels, channel_keys, key_revocations CASCADE`); err != nil {
			t.Errorf("keyrotation_test: failed to truncate tables: %v", err)
		}
		_ = db.Close()
	})
	return db
}

// krSetupRedis returns a client on this process's own allocated logical database
// (#2680). The DB-1 pin it replaced was the same index every other package
// binary and every concurrent worktree used, so a sibling package's flush could
// erase these fixtures mid-test.
func krSetupRedis(t *testing.T) *redis.Client {
	t.Helper()
	return redistest.Client(t)
}

// newKeyRotationHandler wires a members.Handler to the integration DB/Redis/Hub.
func newKeyRotationHandler(t *testing.T) (*Handler, *sql.DB) {
	t.Helper()
	db := krSetupDB(t)
	redisClient := krSetupRedis(t)
	log := logger.New("test")
	hub := websocket.NewHub(db, redisClient)
	cache := rbac.NewPermissionCache(redisClient)
	resolver := rbac.NewResolver(db, cache, log)
	audit := rbac.NewAuditWriter(db, log)
	h := NewHandler(db, log, redisClient, hub, resolver, audit)
	return h, db
}

const krTestPasswordHash = "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec // dummy hash, not a credential // pragma: allowlist secret
const krRotationFingerprint = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// krSeedServerChannel inserts a user (owner), server, and channel, returning their IDs.
// The DB is truncated between tests (see krSetupDB cleanup), so fixed literal
// username/email values cannot collide across the sequential package tests. Using
// literals (mirroring hub_epoch_test.go's "hubuser1") keeps every value bound to a
// $-placeholder with no computed-string source flowing into the SQL call.
func krSeedServerChannel(t *testing.T, db *sql.DB) (ownerID, serverID, channelID string) {
	t.Helper()
	ownerID = uuid.New().String()
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, 'kruser@test.concord.chat', 'kruser', $2, true, true)`,
		ownerID, krTestPasswordHash)
	require.NoError(t, err)

	serverID = uuid.New().String()
	_, err = db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'KR Test Server', $2)`,
		serverID, ownerID)
	require.NoError(t, err)

	channelID = uuid.New().String()
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'voice-room', 'voice')`,
		channelID, serverID)
	require.NoError(t, err)
	return ownerID, serverID, channelID
}

func krSeedEpoch(t *testing.T, db *sql.DB, channelID, userID string, version int) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, 'initial-wrapped-key', 1)`,
		channelID, userID,
	)
	require.NoError(t, err)
	for epoch := 2; epoch <= version; epoch++ {
		_, err = tx.Exec(
			`INSERT INTO key_revocations (
				channel_id, revoked_epoch, successor_epoch, reason, revoked_by,
				rotation_distributor_id, rotation_distributor_claimed, rotation_key_fingerprint
			 ) VALUES ($1, $2, $3, 'test_rotation', $4, $4, TRUE, $5)`,
			channelID, epoch-1, epoch, userID, krRotationFingerprint,
		)
		require.NoError(t, err)
		_, err = tx.Exec(`SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, krRotationFingerprint)
		require.NoError(t, err)
		_, err = tx.Exec(
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'test-wrapped-key', $3)`,
			channelID, userID, epoch,
		)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

// TestTriggerKeyRevocationForChannel_InsertsRevocation verifies the extracted
// per-channel CSK rotation (#487 P2) inserts a key_revocations row rotating
// maxEpoch -> maxEpoch+1 with the supplied reason, scoped to the one channel.
func TestTriggerKeyRevocationForChannel_InsertsRevocation(t *testing.T) {
	h, db := newKeyRotationHandler(t)
	owner, _, channelID := krSeedServerChannel(t, db)
	krSeedEpoch(t, db, channelID, owner, 3)

	h.triggerKeyRevocationForChannel(channelID, "temp_access_revoked", owner)

	var revokedEpoch, successorEpoch int
	var reason string
	err := db.QueryRow(
		`SELECT revoked_epoch, successor_epoch, reason
		 FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 3`, channelID,
	).Scan(&revokedEpoch, &successorEpoch, &reason)
	require.NoError(t, err, "a key_revocations row should be inserted for the channel")
	assert.Equal(t, 3, revokedEpoch, "revoked_epoch should equal the current max key_version")
	assert.Equal(t, 4, successorEpoch, "successor_epoch should be max+1")
	assert.Equal(t, "temp_access_revoked", reason, "reason should be threaded through")
}

// TestTriggerKeyRevocationForChannel_DefaultsEpochWhenNoKeys verifies the
// COALESCE(MAX(key_version),1) default fires when a channel has no channel_keys
// rows yet (rotates 1 -> 2), mirroring the server-wide path's behavior.
func TestTriggerKeyRevocationForChannel_DefaultsEpochWhenNoKeys(t *testing.T) {
	h, db := newKeyRotationHandler(t)
	owner, _, channelID := krSeedServerChannel(t, db)
	// No channel_keys seeded.

	h.triggerKeyRevocationForChannel(channelID, "temp_access_revoked", owner)

	var revokedEpoch, successorEpoch int
	err := db.QueryRow(
		`SELECT revoked_epoch, successor_epoch FROM key_revocations WHERE channel_id = $1`, channelID,
	).Scan(&revokedEpoch, &successorEpoch)
	require.NoError(t, err)
	assert.Equal(t, 1, revokedEpoch, "default max epoch is 1 when no channel_keys exist")
	assert.Equal(t, 2, successorEpoch)
}

// TestTriggerKeyRevocationForChannel_UnknownChannelNoRow verifies the helper is a
// safe no-op (logs + returns) when the channel does not resolve to a server.
func TestTriggerKeyRevocationForChannel_UnknownChannelNoRow(t *testing.T) {
	h, db := newKeyRotationHandler(t)
	owner, _, _ := krSeedServerChannel(t, db)
	unknownChannel := uuid.New().String()

	h.triggerKeyRevocationForChannel(unknownChannel, "temp_access_revoked", owner)

	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1`, unknownChannel,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no revocation should be inserted for an unresolvable channel")
}
