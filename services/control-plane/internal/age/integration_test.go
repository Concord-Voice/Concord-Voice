package age

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // file:// source driver
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// DB/Redis setup is self-contained (NOT via testhelpers) to avoid the import cycle
// age <- testhelpers <- api <- age. Mirrors internal/websocket/hub_epoch_test.go.
const ageITDBPassword = "concord_dev_password" //nolint:gosec // docker-compose dev default // pragma: allowlist secret
const ageITRedisPassword = "concord_dev_redis" //nolint:gosec // docker-compose dev default // pragma: allowlist secret

var (
	ageMigrateOnce sync.Once
	ageMigrateErr  error
)

func ageMigrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

func ageRunMigrations(db *sql.DB) error {
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("migration driver: %w", err)
	}
	m, err := migrate.NewWithDatabaseInstance(
		fmt.Sprintf("file://%s", ageMigrationsPath()), "postgres", driver)
	if err != nil {
		return fmt.Errorf("migrate instance: %w", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// RSA-4096 keygen is slow; generate one keypair for the whole integration package.
var (
	itKeyOnce sync.Once
	itKey     *rsa.PrivateKey
)

func sharedTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	itKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 4096)
		require.NoError(t, err)
		itKey = k
	})
	return itKey
}

type recordingHub struct {
	mu           sync.Mutex
	disconnected []uuid.UUID
}

func (r *recordingHub) DisconnectUser(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disconnected = append(r.disconnected, id)
}

func (r *recordingHub) wasDisconnected(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.disconnected {
		if d.String() == id {
			return true
		}
	}
	return false
}

type ageITEnv struct {
	h      *Handler
	db     *sql.DB
	rdb    *redis.Client
	hub    *recordingHub
	userID string
	key    *rsa.PrivateKey
}

// setupAgeIT provisions a real DB + Redis, a verified user, and a public_keys row
// holding the shared test key's SPKI (key_version 1).
func setupAgeIT(t *testing.T, withKey bool) (*ageITEnv, func()) {
	t.Helper()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://concord:" + ageITDBPassword + "@localhost:5432/concord?sslmode=disable"
	}
	// nosemgrep: go.secrets.pg.pg-hardcoded-secret.pg-hardcoded-secret -- test-only docker-compose dev default, not a real credential (mirrors hub_epoch_test.go)
	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err)
	require.NoError(t, db.Ping(), "age integration tests require PostgreSQL")
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	ageMigrateOnce.Do(func() { ageMigrateErr = ageRunMigrations(db) })
	require.NoError(t, ageMigrateErr)

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://:" + ageITRedisPassword + "@localhost:6379"
	}
	opts, err := redis.ParseURL(redisURL)
	require.NoError(t, err)
	rdb := redis.NewClient(opts)
	require.NoError(t, rdb.Ping(t.Context()).Err(), "age integration tests require Redis")

	key := sharedTestKey(t)
	userID := uuid.New().String()

	email := fmt.Sprintf("age-%s@test.local", userID)
	username := fmt.Sprintf("u%s", userID[:8])
	// nosemgrep: go.lang.security.audit.sqli.gosql-sqli.gosql-sqli -- fully parameterized ($1..$4); email/username are test-controlled UUID-derived values passed as bound params, not interpolated SQL
	_, err = db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, $4, true, true)`,
		userID, email, username, "x")
	require.NoError(t, err)

	if withKey {
		spki, mErr := x509.MarshalPKIXPublicKey(&key.PublicKey)
		require.NoError(t, mErr)
		_, err = db.Exec(
			`INSERT INTO public_keys (user_id, public_key, key_version) VALUES ($1, $2, 1)`,
			userID, spki)
		require.NoError(t, err)
	}

	hub := &recordingHub{}
	env := &ageITEnv{
		h:  NewHandler(db, rdb, hub, logger.New("test")),
		db: db, rdb: rdb, hub: hub, userID: userID, key: key,
	}
	cleanup := func() {
		_, _ = db.Exec(`TRUNCATE users, public_keys, refresh_tokens, age_verification_records CASCADE`)
		_ = rdb.Del(context.Background(), "user_disabled:"+userID).Err()
		_ = rdb.Close()
		_ = db.Close()
	}
	return env, cleanup
}

func (e *ageITEnv) freshClaim() Claim {
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	return Claim{
		CanonicalVersion: 1, UserID: e.userID, ValidAge: true, NSFWAuth: false,
		JurisdictionObligation: 1, Nonce: hex.EncodeToString(nonce),
		Timestamp: time.Now().Unix(), KeyVersion: 1, ClientVersion: "0.2.0",
	}
}

// signBody produces the request JSON for a claim, signed with the env's key. If
// tamperAfterSign mutates the claim, the signature no longer matches (invalid-sig case).
func (e *ageITEnv) signBody(t *testing.T, c Claim, tamperAfterSign func(*Claim)) string {
	t.Helper()
	msg, err := c.CanonicalBytes()
	require.NoError(t, err)
	h := sha256.Sum256(msg)
	sig, err := rsa.SignPSS(rand.Reader, e.key, crypto.SHA256, h[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	require.NoError(t, err)
	if tamperAfterSign != nil {
		tamperAfterSign(&c)
	}
	body := map[string]any{
		"canonical_version":       c.CanonicalVersion,
		"valid_age":               c.ValidAge,
		"nsfw_auth":               c.NSFWAuth,
		"jurisdiction_obligation": c.JurisdictionObligation,
		"nonce":                   c.Nonce,
		"timestamp":               c.Timestamp,
		"key_version":             c.KeyVersion,
		"client_version":          c.ClientVersion,
		"signature":               base64.StdEncoding.EncodeToString(sig),
	}
	b, err := json.Marshal(body)
	require.NoError(t, err)
	return string(b)
}

func (e *ageITEnv) submit(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/age/claim", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", e.userID)
	e.h.SubmitClaim(c)
	return w
}

func (e *ageITEnv) errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		ErrorCode string `json:"error_code"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.ErrorCode
}

func (e *ageITEnv) userDisabled(t *testing.T) bool {
	t.Helper()
	var disabled bool
	require.NoError(t, e.db.QueryRow(`SELECT disabled FROM users WHERE id = $1`, e.userID).Scan(&disabled))
	return disabled
}

func TestAgeIT_ValidAge_StoresActive(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	w := env.submit(t, env.signBody(t, env.freshClaim(), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var validAge bool
	require.NoError(t, env.db.QueryRow(
		`SELECT valid_age FROM age_verification_records WHERE user_id = $1`, env.userID).Scan(&validAge))
	assert.True(t, validAge)
	assert.False(t, env.userDisabled(t), "valid_age=true must not disable")
}

func TestAgeIT_InvalidAge_DisablesTerminally(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	// Seed a live refresh token to prove the disable tx revokes it.
	_, err := env.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me)
		 VALUES ($1, $2, $3, 'dev', '1.2.3.4', 'ua', NOW() + INTERVAL '30 days', false)`,
		uuid.New().String(), env.userID, "test-refresh-token-hash")
	require.NoError(t, err)

	c := env.freshClaim()
	c.ValidAge = false
	w := env.submit(t, env.signBody(t, c, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// account terminally disabled
	var reason string
	require.NoError(t, env.db.QueryRow(
		`SELECT disabled_reason FROM users WHERE id = $1 AND disabled = TRUE`, env.userID).Scan(&reason))
	assert.Equal(t, "age_verification", reason)

	// session revoked
	var revoked sql.NullTime
	require.NoError(t, env.db.QueryRow(
		`SELECT revoked_at FROM refresh_tokens WHERE user_id = $1`, env.userID).Scan(&revoked))
	assert.True(t, revoked.Valid, "refresh token must be revoked")

	// denylist key set synchronously before the 200
	n, rerr := env.rdb.Exists(t.Context(), "user_disabled:"+env.userID).Result()
	require.NoError(t, rerr)
	assert.Equal(t, int64(1), n, "denylist key must be set")

	// live sessions kicked
	assert.True(t, env.hub.wasDisconnected(env.userID), "DisconnectUser must be called")
}

func TestAgeIT_Downgrade_TrueThenFalse_Disables(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	require.Equal(t, http.StatusOK, env.submit(t, env.signBody(t, env.freshClaim(), nil)).Code)
	require.False(t, env.userDisabled(t))

	c := env.freshClaim()
	c.ValidAge = false
	require.Equal(t, http.StatusOK, env.submit(t, env.signBody(t, c, nil)).Code)
	assert.True(t, env.userDisabled(t), "downgrade true->false must disable")
}

func TestAgeIT_DisabledUser_Resubmit_403(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	c := env.freshClaim()
	c.ValidAge = false
	require.Equal(t, http.StatusOK, env.submit(t, env.signBody(t, c, nil)).Code)

	// A disabled user can never write another claim — 403 before signature verify.
	w := env.submit(t, env.signBody(t, env.freshClaim(), nil))
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "account_disabled", env.errorCode(t, w))
}

func TestAgeIT_ReplayedNonce_409(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	body := env.signBody(t, env.freshClaim(), nil)
	require.Equal(t, http.StatusOK, env.submit(t, body).Code)

	// Exact same body (same nonce) → single-use nonce rejects.
	w := env.submit(t, body)
	require.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "replayed_nonce", env.errorCode(t, w))
}

func TestAgeIT_NoSigningKey_422(t *testing.T) {
	env, cleanup := setupAgeIT(t, false) // no public_keys row
	defer cleanup()

	w := env.submit(t, env.signBody(t, env.freshClaim(), nil))
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Equal(t, "no_signing_key", env.errorCode(t, w))
}

func TestAgeIT_StaleKeyVersion_422(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	c := env.freshClaim()
	c.KeyVersion = 2 // stored is 1
	w := env.submit(t, env.signBody(t, c, nil))
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Equal(t, "stale_key_version", env.errorCode(t, w))
}

func TestAgeIT_InvalidSignature_422(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	// Flip valid_age AFTER signing → reconstructed bytes differ → verify fails.
	body := env.signBody(t, env.freshClaim(), func(c *Claim) { c.ValidAge = false })
	w := env.submit(t, body)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Equal(t, "invalid_signature", env.errorCode(t, w))
}

func TestAgeIT_StaleTimestamp_422(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	c := env.freshClaim()
	c.Timestamp = time.Now().Unix() - 301 // just past the -300s window
	w := env.submit(t, env.signBody(t, c, nil))
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Equal(t, "stale_timestamp", env.errorCode(t, w))
}

// TestAgeIT_DisabledInDB_NoDenylistKey_PersistGuard_403 proves the atomic persist
// backstop (Fix #2): even if the step-2 Redis denylist fast-path MISSES a disabled
// account (key absent — the cancellation window or a Redis flush before rebuild), the
// `WHERE NOT EXISTS (disabled=TRUE)` guard on the UPSERT refuses the write → 403.
func TestAgeIT_DisabledInDB_NoDenylistKey_PersistGuard_403(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	// Disable in the DB source of truth WITHOUT setting the Redis denylist key.
	_, err := env.db.Exec(`UPDATE users SET disabled=TRUE, disabled_reason='age_verification' WHERE id=$1`, env.userID)
	require.NoError(t, err)
	n, _ := env.rdb.Exists(t.Context(), "user_disabled:"+env.userID).Result()
	require.Equal(t, int64(0), n, "precondition: denylist key absent (step-2 fast-path will miss)")

	w := env.submit(t, env.signBody(t, env.freshClaim(), nil))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, "account_disabled", env.errorCode(t, w))

	// And no row was written (the guard made the UPSERT a no-op).
	var count int
	require.NoError(t, env.db.QueryRow(
		`SELECT COUNT(*) FROM age_verification_records WHERE user_id=$1`, env.userID).Scan(&count))
	assert.Equal(t, 0, count, "disabled account must not write/overwrite its record")
}

// TestAgeIT_LastChange_MatchesPersistedValue proves Fix #5: the 200 response echoes the
// DB-persisted last_change (via RETURNING), not a separate h.now() clock.
func TestAgeIT_LastChange_MatchesPersistedValue(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	w := env.submit(t, env.signBody(t, env.freshClaim(), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		LastChange time.Time `json:"last_change"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	var stored time.Time
	require.NoError(t, env.db.QueryRow(
		`SELECT last_change FROM age_verification_records WHERE user_id=$1`, env.userID).Scan(&stored))
	assert.WithinDuration(t, stored, resp.LastChange, time.Millisecond,
		"response last_change must equal the persisted DB value")
}

func TestAgeIT_MalformedBody_400(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	w := env.submit(t, `{"nonce": 12345}`) // wrong type / missing fields
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "malformed", env.errorCode(t, w))
}

// RebuildDisabledDenylist must repopulate user_disabled:<id> from users.disabled=TRUE
// after a Redis flush — the cross-process recovery path.
func TestAgeIT_RebuildDenylist_AfterFlush(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	c := env.freshClaim()
	c.ValidAge = false
	require.Equal(t, http.StatusOK, env.submit(t, env.signBody(t, c, nil)).Code)

	// Simulate a Redis flush of the denylist key.
	require.NoError(t, env.rdb.Del(t.Context(), "user_disabled:"+env.userID).Err())
	n, _ := env.rdb.Exists(t.Context(), "user_disabled:"+env.userID).Result()
	require.Equal(t, int64(0), n, "precondition: key flushed")

	require.NoError(t, middleware.RebuildDisabledDenylist(t.Context(), env.db, env.rdb))

	n, err := env.rdb.Exists(t.Context(), "user_disabled:"+env.userID).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "rebuild must restore the denylist key from users.disabled")
}
