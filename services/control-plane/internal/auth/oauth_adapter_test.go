package auth_test

// Tests for the production binding of internal/oauth.AuthAdapter on
// *auth.Handler (oauth_adapter.go). The handler is constructed directly here —
// not via the full SetupTestServer router — to keep the unit-of-test scoped to
// the six adapter wrappers without dragging in the rest of the API surface.
//
// The rig pairs the handler with the same *sql.DB and *redis.Client it was
// built from so tests can stage rows and assert side effects directly.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// adapterRig groups the handler-under-test with the test database and Redis
// it was constructed from, so tests can both invoke adapter methods AND
// inspect/seed the underlying state.
type adapterRig struct {
	Handler *auth.Handler
	DB      *sql.DB
	Redis   *redis.Client
}

// newAdapterRig builds a minimal *auth.Handler bound to the shared test DB
// and Redis. SessionDisconnector is nil — none of the adapter methods route
// through hub.DisconnectUser, so a nil hub is safe.
func newAdapterRig(t *testing.T) *adapterRig {
	t.Helper()

	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(redisCleanup)

	log := logger.New("test")
	h := auth.NewHandler(db, rdb, log, testhelpers.TestJWTSecret, nil)
	return &adapterRig{Handler: h, DB: db, Redis: rdb}
}

// insertAdapterUser writes a users row directly with a known Argon2id hash so
// VerifyPassword can exercise both the success and failure paths. password is
// hashed via the package-level auth.HashPassword to match the production
// stored format.
func insertAdapterUser(t *testing.T, rig *adapterRig, email, username, password string) string {
	t.Helper()
	hash, err := auth.HashPassword(password)
	require.NoError(t, err)

	userID := uuid.New().String()
	_, err = rig.DB.ExecContext(context.Background(),
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, $4, true, true)`,
		userID, email, username, hash,
	)
	require.NoError(t, err)
	return userID
}

// =============================================================================
// IssueAccessAndRefresh
// =============================================================================

// TestIssueAccessAndRefresh_HappyPath verifies the wrapper mints both tokens
// and persists the refresh-token hash in refresh_tokens.
func TestIssueAccessAndRefresh_HappyPath(t *testing.T) {
	rig := newAdapterRig(t)
	userID := insertAdapterUser(t, rig, "issue@example.test", "issueuser", "Password!1234")

	access, refresh, err := rig.Handler.IssueAccessAndRefresh(context.Background(), userID)
	require.NoError(t, err)
	assert.NotEmpty(t, access, "access token must be issued")
	assert.NotEmpty(t, refresh, "refresh token must be issued")

	// Verify the refresh-token row landed in the table.
	var n int
	require.NoError(t, rig.DB.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1`, userID,
	).Scan(&n))
	assert.Equal(t, 1, n, "exactly one refresh_tokens row expected")
}

// TestIssueAccessAndRefresh_UnknownUser_Errors verifies the wrapper surfaces
// a sql.ErrNoRows-rooted error when the user does not exist.
func TestIssueAccessAndRefresh_UnknownUser_Errors(t *testing.T) {
	rig := newAdapterRig(t)
	missing := uuid.New().String()

	_, _, err := rig.Handler.IssueAccessAndRefresh(context.Background(), missing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// =============================================================================
// VerifyPassword (4 paths: lookup miss, lockout, bad password, good password)
// =============================================================================

// TestVerifyPassword_GoodPassword_Succeeds verifies the happy path: a correct
// password returns nil and clears the failure counter.
func TestVerifyPassword_GoodPassword_Succeeds(t *testing.T) {
	rig := newAdapterRig(t)
	const password = "CorrectHorseBatteryStaple1!" // pragma: allowlist secret
	userID := insertAdapterUser(t, rig, "verify@example.test", "verifyuser", password)

	// Pre-seed a failure counter so we can verify the success path clears it.
	require.NoError(t, rig.Redis.Set(
		context.Background(), "login_attempts:verify@example.test", "3", 0,
	).Err())

	err := rig.Handler.VerifyPassword(context.Background(), userID, password)
	require.NoError(t, err)

	// Counter should be cleared (Get returns redis.Nil when key is absent).
	_, redisErr := rig.Redis.Get(
		context.Background(), "login_attempts:verify@example.test",
	).Result()
	assert.True(t, errors.Is(redisErr, redis.Nil),
		"successful verify must clear the per-email login_attempts counter")
}

// TestVerifyPassword_WrongPassword_IncrementsCounter verifies the bad-password
// path increments the per-email lockout counter and returns
// invalid_credentials.
func TestVerifyPassword_WrongPassword_IncrementsCounter(t *testing.T) {
	rig := newAdapterRig(t)
	const goodPassword = "CorrectHorseBatteryStaple1!" // pragma: allowlist secret
	const wrongPassword = "WrongHorseBatteryStaple1!"  // pragma: allowlist secret
	userID := insertAdapterUser(t, rig, "wrong@example.test", "wronguser", goodPassword)

	err := rig.Handler.VerifyPassword(context.Background(), userID, wrongPassword)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_credentials")
	// Sentinel must NOT match here — only threshold-trip returns ErrAccountLocked.
	assert.False(t, errors.Is(err, auth.ErrAccountLocked),
		"single bad password must not return ErrAccountLocked")

	// Counter should be 1 after one failure.
	v, err := rig.Redis.Get(
		context.Background(), "login_attempts:wrong@example.test",
	).Result()
	require.NoError(t, err)
	assert.Equal(t, "1", v)
}

// TestVerifyPassword_PreflightLockout_ReturnsSentinel verifies the load-bearing
// security invariant: when the lockout key is set (the shared /login counter
// has tripped), VerifyPassword short-circuits with auth.ErrAccountLocked
// BEFORE consulting the Argon2id hash. CompleteLink relies on this for its
// 423 Locked response.
func TestVerifyPassword_PreflightLockout_ReturnsSentinel(t *testing.T) {
	rig := newAdapterRig(t)
	const password = "CorrectHorseBatteryStaple1!" // pragma: allowlist secret
	userID := insertAdapterUser(t, rig, "locked@example.test", "lockeduser", password)

	// Simulate the /login lockout having tripped — set the lockout key.
	require.NoError(t, rig.Redis.Set(
		context.Background(), "login_lockout:locked@example.test", "1", 0,
	).Err())

	// Even with the CORRECT password, lockout pre-flight wins.
	err := rig.Handler.VerifyPassword(context.Background(), userID, password)
	require.Error(t, err)
	assert.True(t, errors.Is(err, auth.ErrAccountLocked),
		"locked account must return auth.ErrAccountLocked sentinel")
}

// TestVerifyPassword_UnknownUser_ReturnsInvalidCredentials verifies the
// lookup-miss path returns invalid_credentials (not a leaky "user not found"
// error). This avoids a user-enumeration oracle.
func TestVerifyPassword_UnknownUser_ReturnsInvalidCredentials(t *testing.T) {
	rig := newAdapterRig(t)
	missing := uuid.New().String()

	err := rig.Handler.VerifyPassword(context.Background(), missing, "anything")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_credentials")
	assert.False(t, errors.Is(err, auth.ErrAccountLocked))
}

// =============================================================================
// IssueMFAChallenge — exercises the production binding against a stubbed
// MFAChecker. The wrapper checks IsEnabled before issuing a challenge, then
// hydrates methods + recovery-only methods + WebAuthn options for the
// renderer's MFAChallengeModal.
// =============================================================================

// stubMFAChecker is the hand-rolled MFAChecker for adapter tests. Each method
// is driven by a public field so individual tests can choose:
//   - IsEnabledResult        — pre-flight gate (false ⇒ mfaEnabled=false)
//   - LoginMethodsResult/Err — methods array surfaced to the renderer
//   - EnabledMethodsResult   — superset used to compute recovery-only diff
//   - WebAuthnOptions        — non-nil when "webauthn" is in LoginMethodsResult
//   - GenerateChallengeErr   — drives the error path on the actual issuance
type stubMFAChecker struct {
	IsEnabledResult      bool
	LoginMethodsResult   []string
	LoginMethodsErr      error
	EnabledMethodsResult []string
	EnabledMethodsErr    error
	WebAuthnOptions      interface{}
	WebAuthnErr          error
	GenerateChallengeJTI string
	GenerateChallengeTok string
	GenerateChallengeErr error
	RecoveryTokenStub    string
	RecoveryTokenJTI     string
}

func (s *stubMFAChecker) IsEnabled(_ context.Context, _ string) bool { return s.IsEnabledResult }
func (s *stubMFAChecker) GetEnabledMethods(_ context.Context, _ string) ([]string, error) {
	return s.EnabledMethodsResult, s.EnabledMethodsErr
}
func (s *stubMFAChecker) GetLoginMethods(_ context.Context, _ string) ([]string, error) {
	return s.LoginMethodsResult, s.LoginMethodsErr
}
func (s *stubMFAChecker) GenerateLoginChallenge(_ context.Context, _ string, _ bool) (string, string, error) {
	if s.GenerateChallengeErr != nil {
		return "", "", s.GenerateChallengeErr
	}
	tok := s.GenerateChallengeTok
	if tok == "" {
		tok = "stub-mfa-token"
	}
	jti := s.GenerateChallengeJTI
	if jti == "" {
		jti = "stub-jti"
	}
	return tok, jti, nil
}
func (s *stubMFAChecker) GenerateUpgradeChallenge(_ context.Context, _ string, _ bool) (string, string, error) {
	return "", "", nil
}
func (s *stubMFAChecker) BeginWebAuthnLogin(_ context.Context, _ string, _ string) (interface{}, error) {
	return s.WebAuthnOptions, s.WebAuthnErr
}
func (s *stubMFAChecker) GenerateRecoveryToken(_ string) (string, string, error) {
	return s.RecoveryTokenStub, s.RecoveryTokenJTI, nil
}
func (s *stubMFAChecker) ValidateRecoveryToken(_ string) (*auth.RecoveryClaims, error) {
	return nil, nil //nolint:nilnil // deliberate stub
}

// TestIssueMFAChallenge_NotEnrolled_ReturnsMFAEnabledFalse verifies the
// IsEnabled pre-flight gate: a user with no MFA enrolled must surface
// mfaEnabled=false (and zero-valued challenge / methods) rather than serving
// an unverifiable challenge token. respondExistingSSO uses this to decide to
// fall through to direct token issuance.
func TestIssueMFAChallenge_NotEnrolled_ReturnsMFAEnabledFalse(t *testing.T) {
	rig := newAdapterRig(t)
	rig.Handler.SetMFAChecker(&stubMFAChecker{IsEnabledResult: false})

	tok, methods, recoveryOnly, webauthn, mfaEnabled, err := rig.Handler.IssueMFAChallenge(
		context.Background(), uuid.New().String(),
	)
	require.NoError(t, err)
	assert.False(t, mfaEnabled, "no enrolled MFA must yield mfaEnabled=false")
	assert.Empty(t, tok)
	assert.Empty(t, methods)
	assert.Empty(t, recoveryOnly)
	assert.Nil(t, webauthn)
}

// TestIssueMFAChallenge_TOTPEnrolled_HydratesMethods verifies the happy-path:
// a TOTP-enrolled user gets a challenge token plus the methods + recovery-only
// arrays that drive the modal layout. WebAuthn options remain nil because
// "webauthn" is not in loginMethods.
func TestIssueMFAChallenge_TOTPEnrolled_HydratesMethods(t *testing.T) {
	rig := newAdapterRig(t)
	rig.Handler.SetMFAChecker(&stubMFAChecker{
		IsEnabledResult:      true,
		EnabledMethodsResult: []string{"totp", "backup_code"},
		LoginMethodsResult:   []string{"totp"},
		GenerateChallengeTok: "ch-totp",
	})

	tok, methods, recoveryOnly, webauthn, mfaEnabled, err := rig.Handler.IssueMFAChallenge(
		context.Background(), uuid.New().String(),
	)
	require.NoError(t, err)
	assert.True(t, mfaEnabled)
	assert.Equal(t, "ch-totp", tok)
	assert.Equal(t, []string{"totp"}, methods)
	assert.Equal(t, []string{"backup_code"}, recoveryOnly,
		"backup_code is enrolled but excluded from login methods → recovery-only")
	assert.Nil(t, webauthn, "no webauthn in loginMethods ⇒ options not populated")
}

// TestIssueMFAChallenge_GenerateError_PropagatesError verifies the
// GenerateLoginChallenge failure branch surfaces a wrapped error so the
// caller can map it to mfa_challenge_failed.
func TestIssueMFAChallenge_GenerateError_PropagatesError(t *testing.T) {
	rig := newAdapterRig(t)
	rig.Handler.SetMFAChecker(&stubMFAChecker{
		IsEnabledResult:      true,
		LoginMethodsResult:   []string{"totp"},
		EnabledMethodsResult: []string{"totp"},
		GenerateChallengeErr: errors.New("redis unavailable"),
	})

	_, _, _, _, _, err := rig.Handler.IssueMFAChallenge(
		context.Background(), uuid.New().String(),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "generate login challenge")
}

// =============================================================================
// HashPassword
// =============================================================================

// TestHashPassword_HappyPath verifies the wrapper produces an Argon2id-format
// hash that the package-level VerifyPassword recognises.
func TestHashPassword_HappyPath(t *testing.T) {
	rig := newAdapterRig(t)
	const password = "Test-Password-123!" // pragma: allowlist secret

	hash, err := rig.Handler.HashPassword(context.Background(), password)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(hash, "$argon2id$"),
		"hash must use the argon2id format, got %q", hash)

	valid, err := auth.VerifyPassword(password, hash)
	require.NoError(t, err)
	assert.True(t, valid, "hashed password must verify against itself")
}

// =============================================================================
// ValidateUsername
// =============================================================================

// TestValidateUsername_ValidPasses verifies a well-formed username passes
// through the wrapper.
func TestValidateUsername_ValidPasses(t *testing.T) {
	rig := newAdapterRig(t)
	require.NoError(t, rig.Handler.ValidateUsername("alice123"))
}

// TestValidateUsername_RejectsInvalid verifies the wrapper delegates to the
// package-level ValidateUsername — empty and whitespace-bearing usernames are
// rejected.
func TestValidateUsername_RejectsInvalid(t *testing.T) {
	rig := newAdapterRig(t)
	require.Error(t, rig.Handler.ValidateUsername(""))
	require.Error(t, rig.Handler.ValidateUsername("alice bob"))
}

// =============================================================================
// ValidatePasswordStrength
// =============================================================================

// TestValidatePasswordStrength_StrongPasses verifies a 12+ char, multi-class
// password passes the wrapper.
func TestValidatePasswordStrength_StrongPasses(t *testing.T) {
	rig := newAdapterRig(t)
	require.NoError(t, rig.Handler.ValidatePasswordStrength("Strong-Pass-1234!"))
}

// TestValidatePasswordStrength_RejectsWeak verifies the wrapper delegates to
// ValidatePasswordStrength — short or single-class passwords fail.
func TestValidatePasswordStrength_RejectsWeak(t *testing.T) {
	rig := newAdapterRig(t)
	require.Error(t, rig.Handler.ValidatePasswordStrength("short"))
	require.Error(t, rig.Handler.ValidatePasswordStrength("alllowercase1234"))
}

// TestNormalizeUsername_LowercasesViaAdapter verifies the wrapper delegates to
// the package-level NormalizeUsername (lowercase fold) so the SSO path stores
// usernames identically to the password path (#1931).
func TestNormalizeUsername_LowercasesViaAdapter(t *testing.T) {
	rig := newAdapterRig(t)
	assert.Equal(t, "mixedcase", rig.Handler.NormalizeUsername("MixedCase"))
	assert.Equal(t, "alllower", rig.Handler.NormalizeUsername("alllower"))
}
