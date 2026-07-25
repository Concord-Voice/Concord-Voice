package auth

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// #2418: every session mint is bound to the epoch it was AUTHORIZED under, not
// merely the epoch current at mint time. A login authorized before a destructive
// reset must not mint after it, whether it arrived by password or by completing an
// MFA challenge issued pre-reset.

// newRejectPathHandler builds the minimal handler for tests that assert a REFUSAL.
// The epoch check still runs before any token MINTING, so this exercises the guard
// in isolation from token generation.
//
// #2450: it is no longer DB-only. entCache.GetTier moved ABOVE BeginTx because
// calling it under the users-row lock acquired a second pool connection while the
// handler already held one for the tx — a pool-exhaustion deadlock near
// MaxOpenConns, on a lock every destructive reset needs. The deliberate trade is
// that a login destined for refusal now pays one tier lookup: the refusal path is
// rare (it fires only during an epoch race) and the lookup is a Redis GET that
// reads through to the DB only on a miss, so the wasted work is negligible next to
// the availability risk it removes. Production always wires this cache
// (internal/api/router.go), so the nil cache here was a test-only shape.
func newRejectPathHandler(t *testing.T) (*Handler, *sql.DB, uuid.UUID) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, _ := dbtest.SetupTestDB(t)
	rdb := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	return &Handler{
		db:       db,
		log:      logger.NewWithWriter(io.Discard),
		entCache: entitlements.NewCache(rdb, db),
	}, db, userID
}

// newAdmitPathHandler builds a handler that can mint: Redis for the auth-failure
// clear, an entitlement cache for the tier claim, and the user_keys row
// CompleteLogin returns in its response payload (dbtest.CreateUser does not seed it).
func newAdmitPathHandler(t *testing.T) (*Handler, *sql.DB, uuid.UUID) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, _ := dbtest.SetupTestDB(t)
	rdb := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)

	// internal/testhelpers imports internal/auth, so an in-package test cannot use
	// its E2EETestKeys fixture. CompleteLogin only base64-encodes these bytes into
	// its response, so opaque literals are sufficient.
	_, err := db.Exec(
		`INSERT INTO user_keys (user_id, wrapped_private_key, key_derivation_salt, key_version, key_derivation_alg)
		 VALUES ($1, $2, $3, 1, 'argon2id')`,
		userID, []byte("test-wrapped-private-key"), []byte("test-derivation-salt"),
	)
	require.NoError(t, err)

	h := &Handler{
		db:        db,
		redis:     rdb,
		log:       logger.NewWithWriter(io.Discard),
		jwtSecret: "test-jwt-secret-epoch-binding",
		entCache:  entitlements.NewCache(rdb, db),
	}
	return h, db, userID
}

func newLoginContext() (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	return c, rec
}

func TestCompleteLoginRejectsSupersededEpoch(t *testing.T) {
	h, db, userID := newRejectPathHandler(t)

	// The login was authorized under E1; a destructive reset then advanced the
	// durable epoch to E2 before the mint could run.
	_, err := db.Exec(`UPDATE users SET credential_epoch = 'epoch-E2' WHERE id = $1`, userID)
	require.NoError(t, err)

	c, rec := newLoginContext()
	h.CompleteLogin(c, userID.String(), true, "epoch-E1")

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"a login authorized under a superseded epoch must be refused")

	var live int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID,
	).Scan(&live))
	assert.Zero(t, live, "a refused login must leave no live refresh row")
}

// TestCompleteLoginRejectsClaimlessChallengeForRotatedUser locks the fail-closed
// legacy decision (spec §5): a pre-#2418 challenge carries no epoch claim, and for a
// user who has rotated that is refused rather than admitted. Deliberate deviation
// from #2418's stated acceptance criteria; exposure is one 5-minute challenge TTL.
func TestCompleteLoginRejectsClaimlessChallengeForRotatedUser(t *testing.T) {
	h, db, userID := newRejectPathHandler(t)

	_, err := db.Exec(`UPDATE users SET credential_epoch = 'epoch-E1' WHERE id = $1`, userID)
	require.NoError(t, err)

	c, rec := newLoginContext()
	h.CompleteLogin(c, userID.String(), true, "")

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"a claimless legacy challenge must be refused for a rotated user")
}

func TestCompleteLoginAdmitsCurrentEpoch(t *testing.T) {
	h, db, userID := newAdmitPathHandler(t)

	_, err := db.Exec(
		`UPDATE users SET credential_epoch = 'epoch-E1', email_verified = true WHERE id = $1`, userID)
	require.NoError(t, err)

	c, rec := newLoginContext()
	h.CompleteLogin(c, userID.String(), true, "epoch-E1")

	assert.Equal(t, http.StatusOK, rec.Code, "an uncontended login must succeed")

	var live int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID,
	).Scan(&live))
	assert.Equal(t, 1, live, "a successful login mints exactly one live refresh row")
}

// TestCompleteLoginAdmitsNeverRotatedUser locks the MatchEpoch NULL-admits rule: a
// user who has never rotated accepts any expectedEpoch, including "". This is what
// keeps never-rotated accounts and the pre-rollout deploy window working.
func TestCompleteLoginAdmitsNeverRotatedUser(t *testing.T) {
	h, db, userID := newAdmitPathHandler(t)

	_, err := db.Exec(`UPDATE users SET email_verified = true WHERE id = $1`, userID)
	require.NoError(t, err)

	c, rec := newLoginContext()
	h.CompleteLogin(c, userID.String(), true, "")

	assert.Equal(t, http.StatusOK, rec.Code,
		"a never-rotated user (NULL epoch) admits any expected epoch")
}

// TestCompleteLoginRefusesDisabledAccount covers the terminal disabled gate inside
// the locked snapshot — the check now reads `disabled` under FOR NO KEY UPDATE, so a
// disable committing concurrently cannot slip a session past it.
func TestCompleteLoginRefusesDisabledAccount(t *testing.T) {
	h, db, userID := newRejectPathHandler(t)

	_, err := db.Exec(`UPDATE users SET disabled = TRUE WHERE id = $1`, userID)
	require.NoError(t, err)

	c, rec := newLoginContext()
	h.CompleteLogin(c, userID.String(), true, "")

	assert.Equal(t, http.StatusForbidden, rec.Code, "a disabled account must not mint a session")

	var live int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID,
	).Scan(&live))
	assert.Zero(t, live)
}

// TestReadCredentialEpoch covers the helper that backs challenge stamping, including
// its fail-closed behaviour for an unknown user — the SSO and suspicious-refresh
// paths both abort rather than stamping an empty epoch.
func TestReadCredentialEpoch(t *testing.T) {
	h, db, userID := newRejectPathHandler(t)

	epoch, err := h.readCredentialEpoch(context.Background(), userID.String())
	require.NoError(t, err)
	assert.Empty(t, epoch, "a never-rotated user reads an empty epoch")

	_, err = db.Exec(`UPDATE users SET credential_epoch = 'epoch-E1' WHERE id = $1`, userID)
	require.NoError(t, err)
	epoch, err = h.readCredentialEpoch(context.Background(), userID.String())
	require.NoError(t, err)
	assert.Equal(t, "epoch-E1", epoch)

	_, err = h.readCredentialEpoch(context.Background(), uuid.New().String())
	require.Error(t, err, "an unknown user must error, never yield an empty epoch")
}
