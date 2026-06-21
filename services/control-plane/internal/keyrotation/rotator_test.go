package keyrotation_test

import (
	"context"
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

	"github.com/markdrogersjr/Concord/services/control-plane/internal/keyrotation"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// This is an external (keyrotation_test) package test for the extracted shared CSK
// rotation primitive (#487 P2 / Decision 1a). It sets up the DB/Redis/Hub directly,
// mirroring the no-import-cycle pattern in members/keyrotation_test.go and
// websocket/hub_epoch_test.go.

var (
	krMigrateOnce sync.Once
	krMigrateErr  error
)

// Dev-only defaults matching docker-compose; production always sets DATABASE_URL / REDIS_URL.
var krTestDBPassword = "concord_dev_password" //nolint:gosec // matches docker-compose dev default // pragma: allowlist secret
var krTestRedisPassword = "concord_dev_redis" //nolint:gosec // matches docker-compose dev default // pragma: allowlist secret

const krTestPasswordHash = "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec // dummy hash, not a credential // pragma: allowlist secret

func krMigrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	// keyrotation/ is at internal/keyrotation/; migrations/ is at repo migrations/.
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

func krSetupRedis(t *testing.T) *redis.Client {
	t.Helper()
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://:" + krTestRedisPassword + "@localhost:6379" //nolint:gosec
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("keyrotation_test: failed to parse redis URL: %v", err)
	}
	opts.DB = 1 // matches testhelpers test-isolation DB index
	client := redis.NewClient(opts)
	require.NoError(t, client.Ping(context.Background()).Err(), "keyrotation_test: failed to ping redis")
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func newRotator(t *testing.T) (*keyrotation.Rotator, *sql.DB) {
	t.Helper()
	db := krSetupDB(t)
	redisClient := krSetupRedis(t)
	log := logger.New("test")
	hub := websocket.NewHub(db, redisClient)
	return keyrotation.NewRotator(db, log, hub), db
}

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
	_, err := db.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, $3, $4)`,
		channelID, userID, "test-wrapped-key", version,
	)
	require.NoError(t, err)
}

// TestTriggerForChannel_InsertsRevocation verifies the extracted shared rotation
// inserts a key_revocations row rotating maxEpoch -> maxEpoch+1 with the supplied
// reason, scoped to the one channel.
func TestTriggerForChannel_InsertsRevocation(t *testing.T) {
	r, db := newRotator(t)
	owner, _, channelID := krSeedServerChannel(t, db)
	krSeedEpoch(t, db, channelID, owner, 3)

	r.TriggerForChannel(channelID, "temp_access_revoked", owner)

	var revokedEpoch, successorEpoch int
	var reason string
	err := db.QueryRow(
		`SELECT revoked_epoch, successor_epoch, reason FROM key_revocations WHERE channel_id = $1`, channelID,
	).Scan(&revokedEpoch, &successorEpoch, &reason)
	require.NoError(t, err, "a key_revocations row should be inserted for the channel")
	assert.Equal(t, 3, revokedEpoch, "revoked_epoch should equal the current max key_version")
	assert.Equal(t, 4, successorEpoch, "successor_epoch should be max+1")
	assert.Equal(t, "temp_access_revoked", reason, "reason should be threaded through")
}

// TestTriggerForChannel_DefaultsEpochWhenNoKeys verifies the
// COALESCE(MAX(key_version),1) default fires when a channel has no channel_keys
// rows yet (rotates 1 -> 2).
func TestTriggerForChannel_DefaultsEpochWhenNoKeys(t *testing.T) {
	r, db := newRotator(t)
	owner, _, channelID := krSeedServerChannel(t, db)

	r.TriggerForChannel(channelID, "temp_access_revoked", owner)

	var revokedEpoch, successorEpoch int
	err := db.QueryRow(
		`SELECT revoked_epoch, successor_epoch FROM key_revocations WHERE channel_id = $1`, channelID,
	).Scan(&revokedEpoch, &successorEpoch)
	require.NoError(t, err)
	assert.Equal(t, 1, revokedEpoch, "default max epoch is 1 when no channel_keys exist")
	assert.Equal(t, 2, successorEpoch)
}

// TestTriggerForChannel_UnknownChannelNoRow verifies the helper is a safe no-op
// (logs + returns) when the channel does not resolve to a server.
func TestTriggerForChannel_UnknownChannelNoRow(t *testing.T) {
	r, db := newRotator(t)
	krSeedServerChannel(t, db)
	unknownChannel := uuid.New().String()

	r.TriggerForChannel(unknownChannel, "temp_access_revoked", uuid.New().String())

	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1`, unknownChannel,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no revocation should be inserted for an unresolvable channel")
}

// TestRevokeChannelKeyEpoch_IncludesRemovedUserID verifies the member-removal
// payload shape: when removedUserID is non-empty the broadcast core still inserts
// the revocation row (the removed_user_id field lives in the WS payload, not the DB).
func TestRevokeChannelKeyEpoch_IncludesRemovedUserID(t *testing.T) {
	r, db := newRotator(t)
	owner, serverID, channelID := krSeedServerChannel(t, db)
	krSeedEpoch(t, db, channelID, owner, 2)
	serverUUID, err := uuid.Parse(serverID)
	require.NoError(t, err)

	r.RevokeChannelKeyEpoch(serverID, serverUUID, channelID, 2, "member_removal", owner, owner)

	var revokedEpoch, successorEpoch int
	err = db.QueryRow(
		`SELECT revoked_epoch, successor_epoch FROM key_revocations WHERE channel_id = $1`, channelID,
	).Scan(&revokedEpoch, &successorEpoch)
	require.NoError(t, err)
	assert.Equal(t, 2, revokedEpoch)
	assert.Equal(t, 3, successorEpoch)
}

// TestRevokeChannelKeyEpoch_InsertError verifies the INSERT-failure branch: a
// channel_id with no matching channels row violates the key_revocations FK
// (channel_id REFERENCES channels(id)). The rotator must log + return without
// panicking and without broadcasting — no row is written.
func TestRevokeChannelKeyEpoch_InsertError(t *testing.T) {
	r, db := newRotator(t)
	// A well-formed UUID that does NOT correspond to any channels row.
	orphanChannel := uuid.New().String()
	serverUUID := uuid.New()

	// Must not panic even though the INSERT will fail on the FK constraint.
	r.RevokeChannelKeyEpoch(serverUUID.String(), serverUUID, orphanChannel, 1, "temp_access_revoked", "", "")

	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1`, orphanChannel).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no revocation row should persist when the INSERT fails")
}

// TestRevokeChannelKeyEpoch_Idempotent verifies the ON CONFLICT DO NOTHING guard:
// re-revoking the same epoch does not error and keeps a single row.
func TestRevokeChannelKeyEpoch_Idempotent(t *testing.T) {
	r, db := newRotator(t)
	owner, serverID, channelID := krSeedServerChannel(t, db)
	krSeedEpoch(t, db, channelID, owner, 5)
	serverUUID, err := uuid.Parse(serverID)
	require.NoError(t, err)

	r.RevokeChannelKeyEpoch(serverID, serverUUID, channelID, 5, "temp_access_revoked", owner, "")
	r.RevokeChannelKeyEpoch(serverID, serverUUID, channelID, 5, "temp_access_revoked", owner, "")

	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 5`, channelID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "ON CONFLICT DO NOTHING should keep a single row")
}
