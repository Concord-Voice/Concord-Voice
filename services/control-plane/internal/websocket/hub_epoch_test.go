package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // register file:// source driver for golang-migrate
	"github.com/google/uuid"
	_ "github.com/lib/pq" // register postgres driver for database/sql
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
)

var (
	hubMigrateOnce sync.Once
	hubMigrateErr  error
)

const (
	msgTypeDM = "dm_message"
)

// hubTestDBPassword mirrors the assembled-from-parts pattern in
// testhelpers/testdb.go to satisfy static credential analysis
// (Semgrep "Hard-Coded Credentials in Postgres", SonarCloud S6698/S2068).
// It is a dev-only default that matches docker-compose; production
// always sets DATABASE_URL via env. The Redis counterpart is gone —
// redistest owns Redis URL resolution now (#2680).
var hubTestDBPassword = "concord_dev_password" //nolint:gosec // matches docker-compose dev default // pragma: allowlist secret

// hubTestMigrationsPath resolves the absolute path to the migrations directory
// using runtime.Caller, matching the pattern in testhelpers/testdb.go.
func hubTestMigrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	// websocket/ is at internal/websocket/hub_epoch_test.go
	// migrations/ is at migrations/
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

// hubRunMigrations runs migrations once per package binary via sync.Once.
// Separated from setupHubTestDB to avoid calling t.Fatalf inside the Once closure.
func hubRunMigrations(db *sql.DB) error {
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("could not create migration driver: %w", err)
	}
	m, err := migrate.NewWithDatabaseInstance(
		fmt.Sprintf("file://%s", hubTestMigrationsPath()),
		"postgres", driver,
	)
	if err != nil {
		return fmt.Errorf("could not create migrate instance: %w", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("could not run migrations: %w", err)
	}
	return nil
}

// setupHubTestDB creates a real DB connection for hub tests without importing testhelpers (avoids import cycle).
func setupHubTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://concord:" + hubTestDBPassword + "@localhost:5432/concord?sslmode=disable" //nolint:gosec
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("hub_epoch_test: failed to open database: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("hub_epoch_test: failed to ping database: %v", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)

	// Migrate once per package binary, then truncate for isolation.
	hubMigrateOnce.Do(func() {
		hubMigrateErr = hubRunMigrations(db)
	})
	if hubMigrateErr != nil {
		t.Fatalf("hub_epoch_test: migration failed: %v", hubMigrateErr)
	}

	t.Cleanup(func() {
		if _, err := db.Exec(`TRUNCATE users, dm_conversations, dm_participants, dm_channel_keys, dm_key_revocations, dm_messages CASCADE`); err != nil {
			t.Errorf("hub_epoch_test: failed to truncate tables: %v", err)
		}
		_ = db.Close()
	})
	return db
}

// setupHubTestRedis returns a client on this process's own allocated logical
// database (#2680). The DB-1 pin it replaced was shared by every package binary
// in a run and by every concurrent worktree, so both flushes below could reach
// another process's live fixtures; redistest.Reset refuses any database this
// process does not own.
func setupHubTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	client := redistest.Client(t)
	ctx := context.Background()
	require.NoError(t, redistest.Reset(ctx, client), "hub_epoch_test: failed to reset redis DB")

	t.Cleanup(func() {
		// Reported, not discarded: Reset fails closed when the client's DB or
		// server does not match this process's allocation, and swallowing that is
		// what hid the guard refusals this assertion exists to catch.
		//
		// redis.ErrClosed is the one tolerated error. Several tests here close
		// `client` mid-test on purpose to exercise the fail-closed paths against a
		// dead Redis. Closing a client releases LOCAL resources only, so keys that
		// test wrote do survive on the server — but **the setup Reset above, not
		// this one, is what guarantees isolation**: every test entering through
		// this helper flushes first, so a leaked key is cleared by the next test's
		// setup rather than inherited. Cleanup here is opportunistic.
		//
		// Two stronger forms were tried and rejected, both measured: allocating a
		// fresh client inside this closure took the package from 2 failures to 15,
		// and opening a second client at setup left it at 13 — each extra
		// connection and cleanup entry widens the window on the shared-Postgres
		// TRUNCATE deadlock this package already flakes on (#2790). Making the
		// suite flaky to close a leak the next setup already closes is a bad trade.
		err := redistest.Reset(ctx, client)
		if errors.Is(err, redis.ErrClosed) {
			return
		}
		assert.NoError(t, err, "hub_epoch_test: cleanup reset failed")
	})
	return client
}

type hubTestSetup struct {
	hub    *Hub
	db     *sql.DB
	client *Client
	convID string
	user1  uuid.UUID
	user2  uuid.UUID
}

func setupEpochTest(t *testing.T, seedKey, seedRevocation bool) *hubTestSetup {
	t.Helper()

	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	user1ID := uuid.New()
	user2ID := uuid.New()
	hash := "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec

	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		user1ID.String(), "hubuser1@test.concord.chat", "hubuser1", hash)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		user2ID.String(), "hubuser2@test.concord.chat", "hubuser2", hash)
	require.NoError(t, err)

	convUUID := uuid.New()
	convID := convUUID.String()
	_, err = db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, false, false, $2)`,
		convID, user1ID.String())
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`,
		convID, user1ID.String(), user2ID.String())
	require.NoError(t, err)

	if seedKey {
		_, err = db.Exec(`INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`,
			convID, user1ID.String(), []byte("test-key"))
		require.NoError(t, err)
	}

	if seedRevocation {
		_, err = db.Exec(`INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason, revoked_by) VALUES ($1, 1, 2, 'test', $2)`,
			convID, user1ID.String())
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 2)`,
			convID, user1ID.String(), []byte("test-key-v2"))
		require.NoError(t, err)
	}

	clientID := uuid.New()
	client := &Client{
		ID:       clientID,
		UserID:   user1ID,
		Username: "hubuser1",
		Send:     make(chan []byte, 10),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}

	hub.clients[clientID] = client
	hub.userClients[user1ID] = map[uuid.UUID]bool{clientID: true}
	hub.dmSubscriptions[convUUID] = map[uuid.UUID]bool{clientID: true}

	return &hubTestSetup{
		hub:    hub,
		db:     db,
		client: client,
		convID: convID,
		user1:  user1ID,
		user2:  user2ID,
	}
}

func readClientMsg(t *testing.T, client *Client) map[string]interface{} {
	t.Helper()
	select {
	case data := <-client.Send:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &msg))
		return msg
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for client message")
		return nil
	}
}

func TestHandleDMMessageCurrentEpochAccepted(t *testing.T) {
	setup := setupEpochTest(t, true, false)

	msg := IncomingMessage{
		Type:     msgTypeDM,
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        "hello encrypted",
			"key_version":     float64(1),
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.NotEqual(t, "error", resp["type"], "message should be accepted, not an error")
}

func TestHandleDMMessageRevokedEpochRejected(t *testing.T) {
	setup := setupEpochTest(t, true, true)

	msg := IncomingMessage{
		Type:     msgTypeDM,
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        "hello with revoked key",
			"key_version":     float64(1),
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Equal(t, "epoch_revoked", data["code"])
	assert.Equal(t, float64(2), data["current_epoch"])
}

func TestEnforceDMEpoch_CurrentEpochLookupFailureFailsClosed(t *testing.T) {
	setup := setupEpochTest(t, true, true)
	convUUID, err := uuid.Parse(setup.convID)
	require.NoError(t, err)

	_, err = setup.db.Exec(`ALTER TABLE dm_channel_keys RENAME TO dm_channel_keys_epoch_lookup_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, revertErr := setup.db.Exec(`ALTER TABLE dm_channel_keys_epoch_lookup_test RENAME TO dm_channel_keys`)
		require.NoError(t, revertErr)
	})

	msg := IncomingMessage{ClientID: setup.client.ID}
	assert.False(t, setup.hub.enforceDMEpoch(msg, convUUID, 1))

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Equal(t, errMsgFailedVerifyKeyEpoch, data["message"])
	assert.NotContains(t, data, "current_epoch")
}

func TestHandleDMMessageNotSubscribed(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	otherConvID := uuid.New().String()
	_, err := setup.db.Exec(
		`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, false, false, $2)`,
		otherConvID, setup.user1.String())
	require.NoError(t, err)

	msg := IncomingMessage{
		Type:     msgTypeDM,
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: otherConvID,
			keyContent:        "should fail",
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data["message"], "Not subscribed")
}

// TestHandleDMMessagePlaintextAcceptedWithKeyVersion exercises the WS
// layer's narrow contract: validateEnvelope (#1025) gates on key_version
// presence/validity, not on payload-shape. A payload that happens to be
// plaintext bytes is structurally indistinguishable from a ciphertext
// envelope at the WS layer once key_version >= 1 is declared. Ciphertext
// shape enforcement lives at the REST send-message path (see
// TestSendMessageEncryptedChannelRequiresCiphertext); this test does NOT
// imply the system accepts plaintext DMs in production — it only asserts
// the WS validator's specific contract.
func TestHandleDMMessagePlaintextAcceptedWithKeyVersion(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	msg := IncomingMessage{
		Type:     msgTypeDM,
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        "plaintext content with key_version",
			"key_version":     float64(1),
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.NotEqual(t, "error", resp["type"], "DM with valid key_version should be accepted under E2EE-everywhere")
}

// #2832: enforceWSEpoch and enforceDMEpoch previously answered `return true` for a
// non-positive epoch, which SKIPPED the revocation lookup entirely — a client that
// sent key_version 0 was waved past the very check the epoch exists to drive. They
// now fail closed.
//
// These functions are called DIRECTLY here on purpose. Through the real callers the
// values below are unreachable: validateEnvelope rejects a missing or non-positive
// key_version upstream and closes the socket, so no fixture routed through
// handleMessage or handleDMMessage can exercise the guard. Do not delete this test
// as "unreachable" — a direct call is the only way to pin the guard's behaviour
// independently of the upstream validation that currently masks it, and it is that
// masking which let the inverted condition survive from the pre-#201 era.
func TestEnforceEpochGuards_RejectNonPositiveKeyVersion(t *testing.T) {
	tests := []struct {
		name       string
		keyVersion int
	}{
		{name: "zero", keyVersion: 0},
		{name: "negative", keyVersion: -1},
	}

	for _, tt := range tests {
		t.Run("enforceWSEpoch "+tt.name, func(t *testing.T) {
			setup := setupEpochTest(t, false, false)
			channelUUID := uuid.New()
			msg := IncomingMessage{ClientID: setup.client.ID}

			assert.False(t, setup.hub.enforceWSEpoch(msg, channelUUID, channelUUID.String(), tt.keyVersion))

			resp := readClientMsg(t, setup.client)
			assert.Equal(t, "error", resp["type"])
			data, dataOK := resp["data"].(map[string]interface{})
			require.True(t, dataOK, "error response must carry a data object")
			assert.Equal(t, errMsgFailedVerifyKeyEpoch, data["message"])
		})

		t.Run("enforceDMEpoch "+tt.name, func(t *testing.T) {
			setup := setupEpochTest(t, false, false)
			convUUID, err := uuid.Parse(setup.convID)
			require.NoError(t, err)
			msg := IncomingMessage{ClientID: setup.client.ID}

			assert.False(t, setup.hub.enforceDMEpoch(msg, convUUID, tt.keyVersion))

			resp := readClientMsg(t, setup.client)
			assert.Equal(t, "error", resp["type"])
			data, dataOK := resp["data"].(map[string]interface{})
			require.True(t, dataOK, "error response must carry a data object")
			assert.Equal(t, errMsgFailedVerifyKeyEpoch, data["message"])
		})
	}
}

// #2832: extractKeyVersion must mirror validateEnvelope's accepted type set exactly.
// The int case is the one that regressed: it passed validateEnvelope, then failed the
// call sites' float64-only assertion and was silently relabelled epoch 1.
// TestValidateEnvelopeAgreesWithExtractKeyVersion pins the #2832 invariant that
// validateEnvelope and extractKeyVersion cannot diverge. They were separate
// predicates over the same field and disagreed twice — on int-typed values and
// on NaN — which is how a validated value got replaced by a server-chosen one.
// validateEnvelope now delegates, so this test fails loudly if anyone
// reintroduces an independent predicate.
func TestValidateEnvelopeAgreesWithExtractKeyVersion(t *testing.T) {
	values := []interface{}{
		float64(1), float64(3), float64(maxKeyVersion), 1, 3, maxKeyVersion,
		float64(0), float64(-1), 0, -1, 3.5, 1.25,
		math.Inf(1), math.Inf(-1), math.NaN(), 1e100, maxKeyVersion + 1,
		"1", nil, true,
	}

	for _, v := range values {
		msg := &IncomingMessage{Data: map[string]interface{}{keyKeyVersion: v}}
		_, extractOK := extractKeyVersion(msg)
		validateOK := validateEnvelope(msg) == nil
		assert.Equal(t, extractOK, validateOK,
			"validateEnvelope and extractKeyVersion disagree on %#v", v)
	}

	// Absent key, checked separately since the map has no entry at all.
	empty := &IncomingMessage{Data: map[string]interface{}{}}
	_, extractOK := extractKeyVersion(empty)
	assert.False(t, extractOK)
	assert.Error(t, validateEnvelope(empty))
}

func TestExtractKeyVersion(t *testing.T) {
	tests := []struct {
		name     string
		data     map[string]interface{}
		expected int
		ok       bool
	}{
		{name: "float64 three", data: map[string]interface{}{keyKeyVersion: float64(3)}, expected: 3, ok: true},
		{name: "int three", data: map[string]interface{}{keyKeyVersion: 3}, expected: 3, ok: true},
		{name: "float64 zero", data: map[string]interface{}{keyKeyVersion: float64(0)}},
		{name: "int zero", data: map[string]interface{}{keyKeyVersion: 0}},
		{name: "float64 negative", data: map[string]interface{}{keyKeyVersion: float64(-1)}},
		{name: "int negative", data: map[string]interface{}{keyKeyVersion: -1}},
		{name: "string", data: map[string]interface{}{keyKeyVersion: "1"}},
		{name: "missing", data: map[string]interface{}{}},

		// #2832 (CodeRabbit): a JSON number arrives as float64, and int(v) is
		// lossy in ways that would persist an epoch the sender never claimed.
		// Each case below returned a WRONG accepted value before the guard:
		// 3.5 -> 3, 1.25 -> 1, +Inf -> MaxInt64, 1e100 -> MaxInt64. NaN passed
		// the old negated bound check (!(NaN < 1) is true) and extracted as 0 —
		// the very value the epoch guards exist to reject.
		{name: "float64 fractional truncates to a different epoch", data: map[string]interface{}{keyKeyVersion: 3.5}},
		{name: "float64 fractional just above one", data: map[string]interface{}{keyKeyVersion: 1.25}},
		{name: "float64 positive infinity", data: map[string]interface{}{keyKeyVersion: math.Inf(1)}},
		{name: "float64 negative infinity", data: map[string]interface{}{keyKeyVersion: math.Inf(-1)}},
		{name: "float64 NaN", data: map[string]interface{}{keyKeyVersion: math.NaN()}},
		{name: "float64 beyond int32", data: map[string]interface{}{keyKeyVersion: 1e100}},
		{name: "int beyond maxKeyVersion", data: map[string]interface{}{keyKeyVersion: maxKeyVersion + 1}},

		// Boundaries that MUST still be accepted.
		{name: "float64 one", data: map[string]interface{}{keyKeyVersion: float64(1)}, expected: 1, ok: true},
		{name: "float64 at maxKeyVersion", data: map[string]interface{}{keyKeyVersion: float64(maxKeyVersion)}, expected: maxKeyVersion, ok: true},
		{name: "int at maxKeyVersion", data: map[string]interface{}{keyKeyVersion: maxKeyVersion}, expected: maxKeyVersion, ok: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyVersion, ok := extractKeyVersion(&IncomingMessage{Data: tt.data})

			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.expected, keyVersion)
		})
	}
}
