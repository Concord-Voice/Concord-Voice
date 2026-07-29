package keyrotation_test

import (
	"context"
	"database/sql"
	"errors"
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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
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
const krRotationFingerprint = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

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
	useDefaultDB := redisURL == ""
	if useDefaultDB {
		redisURL = "redis://:" + krTestRedisPassword + "@localhost:6379" //nolint:gosec
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("keyrotation_test: failed to parse redis URL: %v", err)
	}
	if useDefaultDB {
		opts.DB = 1 // matches testhelpers default test-isolation DB index
	}
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
	return keyrotation.NewRotator(db, log, rbac.NewResolver(db, nil, log).CanDistributeChannelKeyTx, websocket.KeyRevocationBroadcaster(hub)), db
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
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`, serverID, ownerID)
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
		 VALUES ($1, $2, 'initial-wrapped-key', 1)
		 ON CONFLICT (channel_id, user_id, key_version) DO NOTHING`,
		channelID, userID,
	)
	require.NoError(t, err)
	for epoch := 2; epoch <= version; epoch++ {
		_, err = tx.Exec(
			`INSERT INTO key_revocations (
				channel_id, revoked_epoch, successor_epoch, reason, revoked_by,
				rotation_distributor_id, rotation_distributor_claimed, rotation_key_fingerprint
			 ) VALUES ($1, $2, $3, 'test_rotation', $4, $4, TRUE, $5)
			 ON CONFLICT (channel_id, revoked_epoch) DO NOTHING`,
			channelID, epoch-1, epoch, userID, krRotationFingerprint,
		)
		require.NoError(t, err)
		_, err = tx.Exec(`SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, krRotationFingerprint)
		require.NoError(t, err)
		_, err = tx.Exec(
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (channel_id, user_id, key_version) DO NOTHING`,
			channelID, userID, "test-wrapped-key", epoch,
		)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
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
	var distributorClaimed bool
	err := db.QueryRow(
		`SELECT revoked_epoch, successor_epoch, reason, rotation_distributor_claimed
		 FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 3`, channelID,
	).Scan(&revokedEpoch, &successorEpoch, &reason, &distributorClaimed)
	require.NoError(t, err, "a key_revocations row should be inserted for the channel")
	assert.Equal(t, 3, revokedEpoch, "revoked_epoch should equal the current max key_version")
	assert.Equal(t, 4, successorEpoch, "successor_epoch should be max+1")
	assert.Equal(t, "temp_access_revoked", reason, "reason should be threaded through")
	assert.False(t, distributorClaimed, "current writers must explicitly leave a new rotation unclaimed")
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

func TestStartManualRotation_RecordsAndBroadcasts(t *testing.T) {
	db := krSetupDB(t)
	var broadcasts []keyrotation.Rotation
	log := logger.New("test")
	r := keyrotation.NewRotator(db, log, rbac.NewResolver(db, nil, log).CanDistributeChannelKeyTx, func(rotation keyrotation.Rotation) {
		broadcasts = append(broadcasts, rotation)
	})
	owner, serverID, channelID := krSeedServerChannel(t, db)
	krSeedEpoch(t, db, channelID, owner, 2)

	rotation, err := r.StartManualRotation(context.Background(), channelID, owner)

	require.NoError(t, err)
	require.NotNil(t, rotation)
	assert.Equal(t, keyrotation.Rotation{
		ChannelID:      channelID,
		ServerID:       serverID,
		RevokedEpoch:   2,
		SuccessorEpoch: 3,
		Reason:         "manual_rotation",
	}, *rotation)
	assert.Equal(t, []keyrotation.Rotation{*rotation}, broadcasts)
}

func TestCompleteInitialKeyDistributionTx(t *testing.T) {
	t.Run("removes the marker", func(t *testing.T) {
		db := krSetupDB(t)
		owner, _, channelID := krSeedServerChannel(t, db)
		_, err := db.Exec(
			`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, channelID, owner,
		)
		require.NoError(t, err)
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				t.Errorf("rollback initial key distribution tx: %v", rollbackErr)
			}
		}()

		require.NoError(t, keyrotation.CompleteInitialKeyDistributionTx(context.Background(), tx, channelID))
		require.NoError(t, tx.Commit())

		var count int
		require.NoError(t, db.QueryRow(
			`SELECT COUNT(*) FROM channel_initial_key_distributions WHERE channel_id = $1`, channelID,
		).Scan(&count))
		assert.Zero(t, count)
	})

	t.Run("returns transaction errors", func(t *testing.T) {
		db := krSetupDB(t)
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())

		err = keyrotation.CompleteInitialKeyDistributionTx(context.Background(), tx, uuid.NewString())

		require.Error(t, err)
		assert.ErrorIs(t, err, sql.ErrTxDone)
	})
}

func TestTriggerForChannel_AdvancesInitialDistributionEpoch(t *testing.T) {
	r, db := newRotator(t)
	owner, _, channelID := krSeedServerChannel(t, db)
	krSeedEpoch(t, db, channelID, owner, 1)
	_, err := db.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`,
		channelID, owner,
	)
	require.NoError(t, err)

	r.TriggerForChannel(channelID, "temp_access_revoked", owner)

	var markerEpoch int
	require.NoError(t, db.QueryRow(
		`SELECT key_version FROM channel_initial_key_distributions WHERE channel_id = $1`, channelID,
	).Scan(&markerEpoch))
	assert.Equal(t, 2, markerEpoch)
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 1 AND successor_epoch = 2`, channelID,
	).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestRevokeChannelKeyEpoch_DeletesIncompleteChannelWhenCreatorRemoved(t *testing.T) {
	db := krSetupDB(t)
	var broadcasts []keyrotation.Rotation
	r := keyrotation.NewRotator(db, logger.New("test"), rbac.NewResolver(db, nil, logger.New("test")).CanDistributeChannelKeyTx, func(rotation keyrotation.Rotation) {
		broadcasts = append(broadcasts, rotation)
	})
	owner, _, channelID := krSeedServerChannel(t, db)
	krSeedEpoch(t, db, channelID, owner, 1)
	_, err := db.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, channelID, owner,
	)
	require.NoError(t, err)

	r.RevokeChannelKeyEpoch(channelID, "member_removal", owner, owner)

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM channels WHERE id = $1`, channelID).Scan(&count))
	assert.Zero(t, count, "an incomplete channel loses its only distributor when the creator is removed")
	require.Len(t, broadcasts, 1)
	assert.Equal(t, []string{channelID}, broadcasts[0].DeletedChannelIDs)
}

func TestRevokeChannelKeyEpoch_DeletesIncompleteChannelWhenCreatorLosesView(t *testing.T) {
	r, db := newRotator(t)
	owner, serverID, channelID := krSeedServerChannel(t, db)
	creatorID := uuid.New().String()
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, 'krcreator@test.concord.chat', 'krcreator', $2, true, true)`, creatorID, krTestPasswordHash)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, creatorID)
	require.NoError(t, err)
	roleID := uuid.New().String()
	_, err = db.Exec(`INSERT INTO roles (id, server_id, name, position, permissions) VALUES ($1, $2, 'creator-viewer', 1, $3)`, roleID, serverID, int64(rbac.PermViewVoiceChannels))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`, serverID, creatorID, roleID)
	require.NoError(t, err)
	krSeedEpoch(t, db, channelID, creatorID, 1)
	_, err = db.Exec(`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, channelID, creatorID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE roles SET permissions = 0 WHERE id = $1`, roleID)
	require.NoError(t, err)

	r.RevokeChannelKeyEpoch(channelID, "member_removal", owner, uuid.New().String())

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM channels WHERE id = $1`, channelID).Scan(&count))
	assert.Zero(t, count, "a fenced creator who loses VIEW cannot complete the distribution")
}

func TestRevokeChannelKeyEpoch_DeletesIncompleteChannelWhenCreatorCheckFails(t *testing.T) {
	db := krSetupDB(t)
	var broadcasts []keyrotation.Rotation
	r := keyrotation.NewRotator(db, logger.New("test"), func(ctx context.Context, tx *sql.Tx, _, _, _ string) (bool, error) {
		_, err := tx.ExecContext(ctx, `SELECT 1 / 0`)
		return false, err
	}, func(rotation keyrotation.Rotation) {
		broadcasts = append(broadcasts, rotation)
	})
	owner, _, channelID := krSeedServerChannel(t, db)
	_, err := db.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, channelID, owner,
	)
	require.NoError(t, err)

	r.RevokeChannelKeyEpoch(channelID, "member_removal", owner, uuid.NewString())

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM channels WHERE id = $1`, channelID).Scan(&count))
	assert.Zero(t, count, "an unverifiable initial distributor must fail closed")
	require.Len(t, broadcasts, 1)
	assert.Equal(t, []string{channelID}, broadcasts[0].DeletedChannelIDs)
}

func TestRevokeChannelKeyEpoch_MissingCreatorCheckerRollsBack(t *testing.T) {
	db := krSetupDB(t)
	var broadcasts []keyrotation.Rotation
	r := keyrotation.NewRotator(db, logger.New("test"), nil, func(rotation keyrotation.Rotation) {
		broadcasts = append(broadcasts, rotation)
	})
	owner, _, channelID := krSeedServerChannel(t, db)
	_, err := db.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, channelID, owner,
	)
	require.NoError(t, err)

	r.RevokeChannelKeyEpoch(channelID, "member_removal", owner, uuid.NewString())

	var channelCount, revocationCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM channels WHERE id = $1`, channelID).Scan(&channelCount))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1`, channelID).Scan(&revocationCount))
	assert.Equal(t, 1, channelCount, "a configuration error must roll back instead of mutating the channel")
	assert.Zero(t, revocationCount)
	assert.Empty(t, broadcasts)
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
	owner, _, channelID := krSeedServerChannel(t, db)
	krSeedEpoch(t, db, channelID, owner, 2)

	r.RevokeChannelKeyEpoch(channelID, "member_removal", owner, owner)

	var revokedEpoch, successorEpoch int
	err := db.QueryRow(
		`SELECT revoked_epoch, successor_epoch FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 2`, channelID,
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
	// Must not panic even though the INSERT will fail on the FK constraint.
	r.RevokeChannelKeyEpoch(orphanChannel, "temp_access_revoked", "", "")

	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1`, orphanChannel).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no revocation row should persist when the INSERT fails")
}

// TestRevokeChannelKeyEpoch_Idempotent verifies the ON CONFLICT DO NOTHING guard:
// re-revoking the same epoch does not error and keeps a single row.
func TestRevokeChannelKeyEpoch_Idempotent(t *testing.T) {
	r, db := newRotator(t)
	owner, _, channelID := krSeedServerChannel(t, db)
	krSeedEpoch(t, db, channelID, owner, 5)

	r.RevokeChannelKeyEpoch(channelID, "temp_access_revoked", owner, "")
	r.RevokeChannelKeyEpoch(channelID, "temp_access_revoked", owner, "")

	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 5`, channelID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "ON CONFLICT DO NOTHING should keep a single row")
}
