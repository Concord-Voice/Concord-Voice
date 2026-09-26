package mfa

// This file pins the INTERNAL-package (package mfa) surface of the
// MFA-settings step-up gate
// ([internal]plans/2026-09-23-mfa-step-up-and-suppress-embeds.md
// Task 2 Steps 1, 2, 5, 6, 7; contract at
// [internal]specs/2026-09-23-mfa-step-up-and-suppress-embeds-design.md
// §4.2/§4.3/§4.5). The gate is NOT implemented yet — Task 3 lands
// settings_stepup.go later in the plan — so every case below is written
// against the EXACT unexported names Task 3 commits to: inlineMFAMethods,
// openMFASettingsTx, verifyMFASettingsStepUpTx, mfaSettingsStepUpKey,
// mfaSettingsStepUpLimit, the gateStage enum, mfaStepUpCredentials, and the
// stepUpLock enum (lockForShare / lockForNoKeyUpdate) — plus the EXISTING
// updateUserMFAFlags, whose fail-open behaviour Task 3 replaces.
//
// Every case is expected to be red today, for the reason stated in its own
// comment: either the package does not compile (a bare `undefined: <name>`
// for a Task-3 symbol — acceptable per the brief, and named at each group),
// or the assertion is wrong against today's (pre-fix) handler behaviour.
// Neither is a typo; both are named.
//
// Helpers here are independent copies, not the ones in
// settings_stepup_gate_test.go — that file is `package mfa_test`, so its
// identifiers (setupTS, testPassword, the url* constants, enrollTOTP, ...)
// are not visible from `package mfa`. Copies below carry an `iu`
// ("internal unit") prefix so they cannot collide with any identifier this
// package's other _test.go files already declare.
// security_event_test.go's securityEventRecorder IS reused directly (same
// package, no copy needed).

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── shared test fixtures ─────────────────────────────────────────────────

const (
	iuPassword    = "IuTestPassword123!"  //nolint:gosec // G101 false positive: test credential // pragma: allowlist secret
	iuBadPassword = "IuWrongPassword999!" //nolint:gosec // G101 false positive: test credential // pragma: allowlist secret
	// iuKeyringHex is a 32-byte AES key, hex-encoded — the same shape (and
	// literal digits) security_event_test.go already uses successfully for
	// ParseKeyring, so its validity is proven, not merely assumed.
	iuKeyringHex = "0101010101010101010101010101010101010101010101010101010101010101"
	// iuTOTPSecret mirrors security_event_test.go's TOTP fixture.
	iuTOTPSecret = "JBSWY3DPEHPK3PXP" //nolint:gosec // test-only TOTP seed // pragma: allowlist secret
)

// iuNewTestDB returns a PRIVATE pool. SetupTestDB still runs for migrations,
// the shared advisory lock and truncate-on-cleanup, but its own pool is not
// used: the first SetupTestDB pool in a test binary keeps one connection
// checked out by the migrate driver, so the pool-safety tests'
// SetMaxOpenConns(1) would leave it zero connections and hang before the gate
// ever ran whenever such a test happened to run first.
func iuNewTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbtest.SetupTestDB(t)
	db, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func iuNewTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	return rdb
}

func iuKeyring(t *testing.T) *Keyring {
	t.Helper()
	kr, err := ParseKeyring(iuKeyringHex, 1, "")
	require.NoError(t, err)
	return kr
}

func iuHandler(db *sql.DB, rdb *redis.Client, kr *Keyring) *Handler {
	return NewHandler(db, rdb, logger.New("test"), kr, "test", nil, "test")
}

// iuCreateUser inserts a user with a REAL Argon2id hash (cheap KDF params —
// test speed only, not a security posture) so stepup.VerifyPasswordFactor's
// auth.VerifyPassword call succeeds/fails for real rather than against a
// placeholder hash. credential_epoch is left NULL: credepoch.MatchEpoch
// admits any token against a NULL/empty stored epoch, which keeps every case
// that is not itself testing the epoch fence unconcerned with it.
func iuCreateUser(t *testing.T, db *sql.DB, password string) string {
	t.Helper()
	hash, err := auth.HashPasswordWithParams(password, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	id := uuid.New().String()
	_, err = db.Exec(`
		INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES ($1, $2, $3, $4, true, true)
	`, id, id+"@iu-stepup.test", "iu"+id[:8], hash)
	require.NoError(t, err)
	return id
}

// iuEnrollTOTP inserts an enabled+confirmed TOTP row directly (bypassing the
// handler) and returns the plaintext secret so a caller can mint a valid
// code with iuTOTPCode.
func iuEnrollTOTP(t *testing.T, db *sql.DB, kr *Keyring, userID string) string {
	t.Helper()
	enc, nonce, keyVersion, err := kr.Seal([]byte(iuTOTPSecret))
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, $4, true, true)
	`, userID, enc, nonce, keyVersion)
	require.NoError(t, err)
	return iuTOTPSecret
}

func iuTOTPCode(t *testing.T, secret string) string {
	t.Helper()
	return iuTOTPCodeAt(t, secret, time.Now())
}

// iuTOTPNextCode mints the code for the step after now. A TOTP code is
// accepted once per step (last_used_step, migration 000158), so a second
// verification for the same user needs a later step; the next one is still
// inside the skew window.
func iuTOTPNextCode(t *testing.T, secret string) string {
	t.Helper()
	return iuTOTPCodeAt(t, secret, time.Now().Add(30*time.Second))
}

func iuTOTPCodeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	return code
}

func iuAddWebAuthnCredential(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
		VALUES ($1, $2, $3, 'Security Key', 'hardware', $4, 0, NOW())
	`, uuid.New().String(), userID, []byte("iu-fake-credential-id-"+userID), []byte("iu-fake-public-key"))
	require.NoError(t, err)
}

// iuGinContext builds a gin context carrying the same "user_id" +
// middleware.JWTClaimsContextKey shape middleware.AuthRequired stamps,
// WITHOUT running AuthRequired. The internal gate functions read those keys
// directly (via middleware.TokenCredentialEpoch), and case 3 below
// specifically needs to drive them without AuthRequired's own 401
// pre-empting the in-transaction check under test.
func iuGinContext(method, path, body, userID, tokenEpoch string) (*gin.Context, *httptest.ResponseRecorder) {
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", userID)
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"cred_epoch": tokenEpoch})
	return c, response
}

func iuBody(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body
}

// ── Case 1: inlineMFAMethods table ───────────────────────────────────────
//
// UNDEFINED TODAY: `inlineMFAMethods` does not exist until Task 3 lands
// (settings_stepup.go). `go vet` reports `undefined: inlineMFAMethods` for
// this whole test — the acceptable, named reason it is red right now, not a
// typo.
func TestInlineMFAMethods_TOTPWebAuthnMatrix(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	ctx := context.Background()

	cases := []struct {
		name              string
		totpRow           bool
		totpEnabled       bool
		totpConfirmed     bool
		webauthn          bool
		staleUsersMethods []string // written to users.mfa_methods to prove it is never read
		want              []string
	}{
		{name: "no totp row, no webauthn", want: []string{}},
		{name: "totp row present but disabled and unconfirmed", totpRow: true, want: []string{}},
		{name: "totp enabled only (not confirmed)", totpRow: true, totpEnabled: true, want: []string{}},
		{name: "totp confirmed only (not enabled)", totpRow: true, totpConfirmed: true, want: []string{}},
		{name: "totp enabled and confirmed", totpRow: true, totpEnabled: true, totpConfirmed: true, want: []string{"totp"}},
		{name: "webauthn present, no totp row", webauthn: true, want: []string{"webauthn"}},
		{name: "totp enabled+confirmed and webauthn present", totpRow: true, totpEnabled: true, totpConfirmed: true, webauthn: true, want: []string{"totp", "webauthn"}},
		{
			name:              "stale users.mfa_methods lists email/sms but no inline factor exists",
			staleUsersMethods: []string{"email", "sms"},
			want:              []string{},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			userID := iuCreateUser(t, db, iuPassword)
			if tc.totpRow {
				enc, nonce, keyVersion, err := kr.Seal([]byte(iuTOTPSecret))
				require.NoError(t, err)
				_, err = db.Exec(`
					INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
					VALUES ($1, $2, $3, $4, $5, $6)
				`, userID, enc, nonce, keyVersion, tc.totpEnabled, tc.totpConfirmed)
				require.NoError(t, err)
			}
			if tc.webauthn {
				iuAddWebAuthnCredential(t, db, userID)
			}
			if tc.staleUsersMethods != nil {
				_, err := db.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = $1 WHERE id = $2`, pq.Array(tc.staleUsersMethods), userID)
				require.NoError(t, err)
			}

			// The predicate moved to internal/stepup (one P1 reader for every
			// gate); this matrix pins it against the real schema.
			got, err := stepup.InlineMFAMethods(ctx, db, userID)

			require.NoError(t, err)
			assert.NotNil(t, got, "InlineMFAMethods must never return a nil slice")
			assert.ElementsMatch(t, tc.want, got, "InlineMFAMethods(%s)", tc.name)
			for _, forbidden := range []string{"email", "sms"} {
				assert.NotContains(t, got, forbidden, "InlineMFAMethods must never offer %q", forbidden)
			}
		})
	}
}

// ── Case 2: events by stage ───────────────────────────────────────────────
//
// UNDEFINED TODAY: the budget subtests reference `mfaSettingsStepUpKey` and
// `mfaSettingsStepUpLimit` (Task 3), so the whole file — and therefore this
// whole test — fails to compile today (`undefined: mfaSettingsStepUpKey`).
// Considered in isolation, the non-budget subtests (wrong password / wrong
// code / epoch mismatch / DeleteRecoveryKey wrong password) reference only
// EXISTING names and would compile; they are red on their ASSERTIONS,
// because neither EmailSmsDisable nor DeleteRecoveryKey's pre-fix code path
// produces the wanted body/event pair on this route today. Each subtest
// states its own today-vs-wanted delta.
func TestStepUpSettingsEvents_ByStage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	rdb := iuNewTestRedis(t)
	kr := iuKeyring(t)

	t.Run("EmailSmsDisable wrong password emits ReasonInvalidCredentials", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		h := iuHandler(db, rdb, kr)
		recorder := &securityEventRecorder{}
		h.SetSecurityEvents(recorder)
		c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/email-sms/disable",
			fmt.Sprintf(`{"password":%q}`, iuBadPassword), userID, "")

		h.EmailSmsDisable(c)

		// TODAY: EmailSmsDisable checks no credential at all, so this
		// succeeds (200) and emits ReasonFactorDisabled — not the 403 +
		// ReasonInvalidCredentials the gate must produce.
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, map[string]interface{}{"error": "Invalid password"}, iuBody(t, w))
		require.Len(t, recorder.events, 1)
		assert.Equal(t, securityevent.EventAuthentication, recorder.events[0].EventType)
		assert.Equal(t, securityevent.OutcomeDenied, recorder.events[0].Outcome)
		assert.Equal(t, securityevent.ReasonInvalidCredentials, recorder.events[0].ReasonCode)
		assert.Equal(t, securityevent.AuthPassword, recorder.events[0].AuthMethod)
	})

	t.Run("EmailSmsDisable wrong TOTP code emits ReasonChallengeInvalid", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		iuEnrollTOTP(t, db, kr, userID)
		h := iuHandler(db, rdb, kr)
		recorder := &securityEventRecorder{}
		h.SetSecurityEvents(recorder)
		c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/email-sms/disable",
			fmt.Sprintf(`{"password":%q,"mfa_code":"000000"}`, iuPassword), userID, "")

		h.EmailSmsDisable(c)

		// TODAY: the MFA code is never read by EmailSmsDisable, so this
		// still succeeds (200) with ReasonFactorDisabled.
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, map[string]interface{}{"error": "Invalid MFA code"}, iuBody(t, w))
		require.Len(t, recorder.events, 1)
		assert.Equal(t, securityevent.EventMFA, recorder.events[0].EventType)
		assert.Equal(t, securityevent.ReasonChallengeInvalid, recorder.events[0].ReasonCode)
	})

	t.Run("EmailSmsDisable rotated credential epoch emits ReasonCredentialEpochMismatch", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		_, err := db.Exec(`UPDATE users SET credential_epoch = gen_random_uuid()::text WHERE id = $1`, userID)
		require.NoError(t, err)
		h := iuHandler(db, rdb, kr)
		recorder := &securityEventRecorder{}
		h.SetSecurityEvents(recorder)
		c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/email-sms/disable",
			fmt.Sprintf(`{"password":%q}`, iuPassword), userID, "stale-client-epoch")

		h.EmailSmsDisable(c)

		// TODAY: no epoch fence exists on this route at all, so this still
		// succeeds (200).
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, map[string]interface{}{"error": "Authentication required"}, iuBody(t, w))
		require.Len(t, recorder.events, 1)
		assert.Equal(t, securityevent.EventCredentialEpoch, recorder.events[0].EventType)
		assert.Equal(t, securityevent.ReasonCredentialEpochMismatch, recorder.events[0].ReasonCode)
		assert.Equal(t, securityevent.SeverityHigh, recorder.events[0].Severity)
	})

	t.Run("6th credentialed attempt emits ReasonRateLimitExceeded", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		h := iuHandler(db, rdb, kr)
		recorder := &securityEventRecorder{}
		h.SetSecurityEvents(recorder)

		var w *httptest.ResponseRecorder
		for i := 0; i < mfaSettingsStepUpLimit+1; i++ {
			var c *gin.Context
			// A WRONG password, matching its HTTP twin
			// (TestStepUpSettings_Budget "6th credentialed attempt returns
			// 429" in settings_stepup_gate_test.go). Every success clears
			// the budget (verified by the "verified commit clears the
			// budget" case), so a loop of CORRECT-password attempts can
			// never accumulate past the limit — only credentialed refusals
			// do.
			c, w = iuGinContext(http.MethodPost, "/api/v1/mfa/email-sms/disable",
				fmt.Sprintf(`{"password":%q}`, iuBadPassword), userID, "")
			recorder.events = nil
			h.EmailSmsDisable(c)
		}

		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.Equal(t, map[string]interface{}{"error": "Too many verification attempts"}, iuBody(t, w))
		require.Len(t, recorder.events, 1)
		assert.Equal(t, securityevent.EventSecurityControl, recorder.events[0].EventType)
		assert.Equal(t, securityevent.OutcomeDenied, recorder.events[0].Outcome)
		assert.Equal(t, securityevent.ReasonRateLimitExceeded, recorder.events[0].ReasonCode)
	})

	t.Run("budget backend failure emits ReasonRateLimitBackendUnavailable", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		faultRedis := iuNewTestRedis(t)
		faultRedis.AddHook(iuBudgetIncrFaultHook{key: mfaSettingsStepUpKey(userID)})
		h := iuHandler(db, faultRedis, kr)
		recorder := &securityEventRecorder{}
		h.SetSecurityEvents(recorder)
		c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/email-sms/disable",
			fmt.Sprintf(`{"password":%q}`, iuPassword), userID, "")

		h.EmailSmsDisable(c)

		// An unevaluable budget is a server fault, not the user's doing: 503
		// with its own body, never the 429 "too many attempts" that locked the
		// form for a window that was not running (F7).
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, map[string]interface{}{"error": stepup.ErrMsgBudgetUnavailable}, iuBody(t, w))
		require.Len(t, recorder.events, 1)
		assert.Equal(t, securityevent.EventSecurityControl, recorder.events[0].EventType)
		assert.Equal(t, securityevent.OutcomeDegraded, recorder.events[0].Outcome)
		assert.Equal(t, securityevent.ReasonRateLimitBackendUnavailable, recorder.events[0].ReasonCode)
	})

	t.Run("DeleteRecoveryKey wrong password emits ReasonInvalidCredentials", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		_, err := db.Exec(`
			INSERT INTO user_recovery_keys (user_id, recovery_wrapped_private_key, recovery_key_salt)
			VALUES ($1, $2, $3)
		`, userID, []byte("iu-seed-recovery-key-32-bytes-x"), []byte("iu-seed-salt-16b"))
		require.NoError(t, err)
		h := iuHandler(db, rdb, kr)
		recorder := &securityEventRecorder{}
		h.SetSecurityEvents(recorder)
		c, w := iuGinContext(http.MethodDelete, "/api/v1/mfa/recovery-key",
			fmt.Sprintf(`{"password":%q}`, iuBadPassword), userID, "")

		h.DeleteRecoveryKey(c)

		// TODAY: DeleteRecoveryKey already checks the password (off-seam,
		// via requirePasswordAndMFA), but with the OLD body ("Incorrect
		// password") and NO event emission at all.
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, map[string]interface{}{"error": "Invalid password"}, iuBody(t, w))
		require.Len(t, recorder.events, 1)
		assert.Equal(t, securityevent.ReasonInvalidCredentials, recorder.events[0].ReasonCode)
	})
}

type iuBudgetIncrFaultHook struct{ key string }

func (iuBudgetIncrFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h iuBudgetIncrFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "incr" {
			if args := cmd.Args(); len(args) >= 2 {
				if k, ok := args[1].(string); ok && k == h.key {
					return errors.New("iu: forced step-up budget incr failure")
				}
			}
		}
		return next(ctx, cmd)
	}
}

func (iuBudgetIncrFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// ── Case 3: in-transaction epoch fence ───────────────────────────────────
//
// UNDEFINED TODAY: `openMFASettingsTx`, `mfaStepUpCredentials` and
// `lockForNoKeyUpdate` do not exist until Task 3 lands — `go vet` reports
// `undefined: openMFASettingsTx` for this whole test.
//
// This case exists so a later mutation that deletes the in-tx
// credepoch.MatchEpoch check inside openMFASettingsTx turns red. The HTTP
// level cannot see that mutation: middleware.AuthRequired's own 401 on a
// rotated epoch fires before any handler runs, so
// settings_stepup_gate_test.go's "rotated credential epoch" row is
// deliberately blind to it (see that file's header). This test drives
// openMFASettingsTx directly, WITHOUT AuthRequired, so the epoch check
// inside that exact function is what is under test.
func TestOpenMFASettingsTx_EpochFence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	rdb := iuNewTestRedis(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	_, err := db.Exec(`UPDATE users SET credential_epoch = 'server-side-epoch' WHERE id = $1`, userID)
	require.NoError(t, err)
	// A concrete guarded-state witness: openMFASettingsTx itself performs no
	// write, so this column is expected to survive a refusal untouched —
	// making "guarded state unchanged" a real assertion rather than a
	// tautology.
	_, err = db.Exec(`UPDATE users SET backup_email = NULL WHERE id = $1`, userID)
	require.NoError(t, err)

	h := iuHandler(db, rdb, kr)
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/email-sms/disable", "", userID, "stale-client-epoch")

	tx, _, ok := h.openMFASettingsTx(c, userID, mfaStepUpCredentials{Password: iuPassword}, lockForNoKeyUpdate)

	assert.False(t, ok, "a mismatched credential epoch must refuse")
	assert.Nil(t, tx)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, map[string]interface{}{"error": "Authentication required"}, iuBody(t, w))

	var backupEmail sql.NullString
	require.NoError(t, db.QueryRow(`SELECT backup_email FROM users WHERE id = $1`, userID).Scan(&backupEmail))
	assert.False(t, backupEmail.Valid, "the guarded row must be unchanged by a refused open")
}

// ── Case 4: pool safety (no read/write under the row lock) ───────────────
//
// UNDEFINED TODAY: `openMFASettingsTx`, `verifyMFASettingsStepUpTx`,
// `mfaStepUpCredentials`, `lockForNoKeyUpdate` and `lockForShare` do not
// exist until Task 3 lands — `go vet` reports `undefined: openMFASettingsTx`
// for this whole test.
//
// Once Task 3 lands per spec (nothing touches h.db between the lock and
// commit), this test becomes a REGRESSION guard: a version that reads the
// pool while the row lock is held cannot proceed on a 1-connection pool (the
// reading call blocks waiting for the connection the open transaction is
// holding), and the watchdog fails it in seconds rather than hanging to
// go test's own 10-minute panic.
func TestOpenAndVerifyMFASettingsStepUpTx_PoolSafety(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	db.SetMaxOpenConns(1)
	rdb := iuNewTestRedis(t)
	kr := iuKeyring(t)
	h := iuHandler(db, rdb, kr)

	plainUser := iuCreateUser(t, db, iuPassword)
	totpUser := iuCreateUser(t, db, iuPassword)
	totpSecret := iuEnrollTOTP(t, db, kr, totpUser)

	type step struct {
		name   string
		userID string
		lock   stepUpLock
		creds  func() mfaStepUpCredentials
		// wantMissingCode marks the missing-code step (I3): the refusal must
		// offer the preloaded P1 set, and building it must not read the pool
		// (the old fallback read users.mfa_methods through h.db while this
		// transaction held the only connection).
		wantMissingCode bool
	}
	steps := []step{
		{name: "FOR NO KEY UPDATE, password only (EmailSmsDisable/SetBackupEmail shape)", userID: plainUser, lock: lockForNoKeyUpdate,
			creds: func() mfaStepUpCredentials { return mfaStepUpCredentials{Password: iuPassword} }},
		{name: "FOR SHARE, password only (StoreRecoveryKey/DeleteRecoveryKey shape)", userID: plainUser, lock: lockForShare,
			creds: func() mfaStepUpCredentials { return mfaStepUpCredentials{Password: iuPassword} }},
		{name: "FOR NO KEY UPDATE, TOTP leg", userID: totpUser, lock: lockForNoKeyUpdate,
			creds: func() mfaStepUpCredentials {
				return mfaStepUpCredentials{Password: iuPassword, MFACode: iuTOTPCode(t, totpSecret)}
			}},
		// The step above committed its code's step, so this one uses the next.
		{name: "FOR SHARE, TOTP leg", userID: totpUser, lock: lockForShare,
			creds: func() mfaStepUpCredentials {
				return mfaStepUpCredentials{Password: iuPassword, MFACode: iuTOTPNextCode(t, totpSecret)}
			}},
		{name: "FOR NO KEY UPDATE, TOTP enrolled, code missing", userID: totpUser, lock: lockForNoKeyUpdate,
			creds:           func() mfaStepUpCredentials { return mfaStepUpCredentials{Password: iuPassword} },
			wantMissingCode: true},
	}

	for _, s := range steps {
		s := s
		t.Run(s.name, func(t *testing.T) {
			done := make(chan struct{})
			var (
				stepErr *stepup.Error
				stage   gateStage
				ok      bool
			)
			go func() {
				defer close(done)
				c, _ := iuGinContext(http.MethodPost, "/x", "", s.userID, "")
				var tx *sql.Tx
				var subj stepup.Subject
				tx, subj, ok = h.openMFASettingsTx(c, s.userID, s.creds(), s.lock)
				if !ok {
					return
				}
				defer func() { _ = tx.Rollback() }()
				stepErr, stage = h.verifyMFASettingsStepUpTx(c.Request.Context(), tx, s.userID, subj, s.creds(), stepup.Copy{ //nolint:gosec // G101 false positive: user-facing refusal copy, not credentials
					NoFactors:          "no factors",
					CredentialRequired: "credential required",
				})
				if stepErr == nil {
					_ = tx.Commit()
				}
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("watchdog: a pool read under the row lock hung for 10s")
			}
			assert.True(t, ok, "%s: openMFASettingsTx must accept a valid password", s.name)
			if s.wantMissingCode {
				require.NotNil(t, stepErr, "%s: a TOTP account with no code must be refused", s.name)
				assert.Equal(t, http.StatusForbidden, stepErr.Status)
				assert.Equal(t, stageMFAMissing, stage)
				assert.Equal(t, []string{"totp"}, stepErr.Body["methods"])
				return
			}
			assert.Nil(t, stepErr, "%s: verifyMFASettingsStepUpTx must accept valid credentials (stage %v)", s.name, stage)
		})
	}
}

// iuSeedRecoveryKey inserts a recovery key directly, bypassing the handler,
// so a test can drive the StoreRecoveryKey OVERWRITE branch (K1 already
// exists) rather than the first-time insert branch.
func iuSeedRecoveryKey(t *testing.T, db *sql.DB, userID string, key, salt []byte) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO user_recovery_keys (user_id, recovery_wrapped_private_key, recovery_key_salt)
		VALUES ($1, $2, $3)
	`, userID, key, salt)
	require.NoError(t, err)
}

// TestHandlers_PoolSafety_GreenButProspective is the HANDLER-level twin of
// TestOpenAndVerifyMFASettingsStepUpTx_PoolSafety above. That test only
// drives the gate's own primitives; it cannot see a self-deadlock the design
// panel identified in the HANDLERS themselves — any h.db (pool) read placed
// between openMFASettingsTx and Commit inside a handler body (an
// IsEnabled/methods lookup, a recovery-key existence check run on the pool
// instead of the tx) hangs once the transaction holds the only connection.
// Only a test that drives the actual handler bodies can see that.
//
// GREEN today, and that is expected, not a finding — the same shape as
// TestStepUpSettings_Step5NoDeadlock in settings_stepup_gate_test.go. The
// pre-fix handlers take no row lock at all, so nothing can hang; the
// password/mfa_code fields these requests send are silently ignored by
// today's handlers (none of the four checks a credential yet), so every
// happy path below succeeds trivially. Its value is PROSPECTIVE: once Task 4
// wires these four handlers onto openMFASettingsTx, any h.db read it adds
// between the lock and Commit will hang on this 1-connection pool, and the
// watchdog fails it in seconds rather than hitting go test's 10-minute
// panic. It uses ONLY existing handler method names (EmailSmsDisable,
// SetBackupEmail, StoreRecoveryKey, DeleteRecoveryKey) mounted on a bare gin
// engine, so it adds no new undefined name and compiles today.
func TestHandlers_PoolSafety_GreenButProspective(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	db.SetMaxOpenConns(1)
	rdb := iuNewTestRedis(t)
	kr := iuKeyring(t)
	h := iuHandler(db, rdb, kr)

	plainUser := iuCreateUser(t, db, iuPassword)
	totpUser := iuCreateUser(t, db, iuPassword)
	totpSecret := iuEnrollTOTP(t, db, kr, totpUser)
	iuSeedRecoveryKey(t, db, totpUser,
		[]byte("iu-seed-K1-recovery-key-32-byte"), []byte("iu-seed-K1-salt-16b"))
	// factorUser exercises the two factor-deleting handlers, which since B1 run
	// their step-up, delete, last-factor check and flag write on one locked
	// transaction: TOTP goes first (the security key keeps an inline factor),
	// then the key.
	factorUser := iuCreateUser(t, db, iuPassword)
	factorSecret := iuEnrollTOTP(t, db, kr, factorUser)
	iuAddWebAuthnCredential(t, db, factorUser)
	factorCredentialID := iuWebAuthnCredentialID(t, db, factorUser)

	// iuMountHandler bypasses AuthRequired exactly like case 3's
	// iuGinContext (same "user_id" + middleware.JWTClaimsContextKey shape),
	// but through a real gin.Engine + ServeHTTP so the request travels the
	// same binding/routing path production traffic does, not a direct Go
	// call into the handler.
	iuMountHandler := func(userID, method, path string, handler gin.HandlerFunc) *gin.Engine {
		router := gin.New()
		router.Handle(method, path, func(c *gin.Context) {
			c.Set("user_id", userID)
			c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"cred_epoch": ""})
			handler(c)
		})
		return router
	}

	steps := []struct {
		name    string
		userID  string
		method  string
		path    string
		route   string // gin pattern when it differs from path
		body    string
		handler gin.HandlerFunc
	}{
		{
			name: "EmailSmsDisable, password + valid TOTP code", userID: totpUser,
			method: http.MethodPost, path: "/email-sms/disable",
			body:    fmt.Sprintf(`{"password":%q,"mfa_code":%q}`, iuPassword, iuTOTPCode(t, totpSecret)),
			handler: h.EmailSmsDisable,
		},
		{
			name: "SetBackupEmail, password only (no inline factor)", userID: plainUser,
			method: http.MethodPut, path: "/backup-email",
			body:    fmt.Sprintf(`{"email":"pool-safety@example.com","password":%q}`, iuPassword),
			handler: h.SetBackupEmail,
		},
		{
			// EmailSmsDisable above spent totpUser's current step, so this
			// verification uses the next one.
			name: "StoreRecoveryKey overwrite (seeded K1), password + valid TOTP code", userID: totpUser,
			method: http.MethodPut, path: "/recovery-key",
			body: fmt.Sprintf(`{"recovery_wrapped_private_key":%q,"recovery_key_salt":%q,"password":%q,"mfa_code":%q}`,
				base64.StdEncoding.EncodeToString([]byte("iu-overwrite-recovery-key-32-by")),
				base64.StdEncoding.EncodeToString([]byte("iu-overwrite-salt-16b")),
				iuPassword, iuTOTPNextCode(t, totpSecret)),
			handler: h.StoreRecoveryKey,
		},
		{
			name: "DeleteRecoveryKey, password only", userID: plainUser,
			method: http.MethodDelete, path: "/recovery-key",
			body:    fmt.Sprintf(`{"password":%q}`, iuPassword),
			handler: h.DeleteRecoveryKey,
		},
		{
			name: "TOTPDisable, password + valid TOTP code", userID: factorUser,
			method: http.MethodPost, path: "/totp/disable",
			body:    fmt.Sprintf(`{"password":%q,"code":%q}`, iuPassword, iuTOTPCode(t, factorSecret)),
			handler: h.TOTPDisable,
		},
		{
			name: "WebAuthnDeleteCredential, password", userID: factorUser,
			method: http.MethodDelete, path: "/webauthn/credentials/" + factorCredentialID, route: "/webauthn/credentials/:id",
			body:    fmt.Sprintf(`{"password":%q}`, iuPassword),
			handler: h.WebAuthnDeleteCredential,
		},
	}

	for _, s := range steps {
		s := s
		t.Run(s.name, func(t *testing.T) {
			route := s.path
			if s.route != "" {
				route = s.route
			}
			router := iuMountHandler(s.userID, s.method, route, s.handler)
			done := make(chan struct{})
			var w *httptest.ResponseRecorder
			go func() {
				defer close(done)
				req := httptest.NewRequest(s.method, s.path, strings.NewReader(s.body))
				req.Header.Set("Content-Type", "application/json")
				w = httptest.NewRecorder()
				router.ServeHTTP(w, req)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("watchdog: request did not complete within 10s")
			}
			require.NotNil(t, w, "%s: handler never responded", s.name)
			assert.Equal(t, http.StatusOK, w.Code, "%s: body %s", s.name, w.Body.String())
		})
	}
}

// ── Case 5: the flag sync fails closed where it must, and only there ──────
//
// syncMFAFlagsTx replaced updateUserMFAFlags (B1). Two properties, pulling in
// opposite directions:
//   - An inline-factor read error writes NOTHING: a failed read must never be
//     mistaken for an absent factor and written back as mfa_enabled = FALSE.
//     TOTP and WebAuthn are now one statement, so one case covers both.
//   - A Redis EXISTS error writes ANYWAY. Aborting there is what B1 fixed (a
//     committed factor was left unlisted); the downgrade guarantee the old
//     "Redis Exists error writes nothing" case pinned — email/SMS the row
//     lists survive an unreadable store — now comes from the degraded
//     statement instead of from skipping the write.

type iuFlagsFaultConn struct {
	failInline bool
	execCalls  *int32
}

func (iuFlagsFaultConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (iuFlagsFaultConn) Close() error                        { return nil }
func (iuFlagsFaultConn) Begin() (driver.Tx, error)           { return iuNoopTx{}, nil }

// BeginTx lets the sync run on a transaction from this fake, as it does in
// production.
func (iuFlagsFaultConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return iuNoopTx{}, nil
}

type iuNoopTx struct{}

func (iuNoopTx) Commit() error   { return nil }
func (iuNoopTx) Rollback() error { return nil }

func (c iuFlagsFaultConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	atomic.AddInt32(c.execCalls, 1)
	return driver.RowsAffected(1), nil
}

func (c iuFlagsFaultConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "user_mfa_totp"):
		if c.failInline {
			return nil, errors.New("iu: forced inline factor read failure")
		}
		return &iuOneRow{values: []driver.Value{false, false}}, nil
	default:
		return &iuOneRow{values: []driver.Value{int64(0)}}, nil
	}
}

type iuFlagsFaultConnector struct{ conn iuFlagsFaultConn }

func (c iuFlagsFaultConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (iuFlagsFaultConnector) Driver() driver.Driver                          { return iuFlagsFaultDriver{} }

type iuFlagsFaultDriver struct{}

func (iuFlagsFaultDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("connector required")
}

type iuOneRow struct {
	values []driver.Value
	sent   bool
}

func (r *iuOneRow) Columns() []string { return make([]string, len(r.values)) }
func (*iuOneRow) Close() error        { return nil }
func (r *iuOneRow) Next(dest []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	copy(dest, r.values)
	r.sent = true
	return nil
}

func TestSyncMFAFlags_FailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("inline factor read error writes nothing", func(t *testing.T) {
		var execCalls int32
		db := sql.OpenDB(iuFlagsFaultConnector{conn: iuFlagsFaultConn{failInline: true, execCalls: &execCalls}})
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })

		err = syncMFAFlagsTx(ctx, tx, "iu-fault-user-a", emailSmsState{known: true})

		assert.Error(t, err, "an inline factor read error must be reported, not swallowed")
		assert.Zero(t, atomic.LoadInt32(&execCalls), "a failed factor read must write nothing")
	})

	t.Run("control: a readable factor state does write", func(t *testing.T) {
		// Proves the case above fails for its named reason: with the read
		// healthy, the same fake reaches the UPDATE.
		var execCalls int32
		db := sql.OpenDB(iuFlagsFaultConnector{conn: iuFlagsFaultConn{execCalls: &execCalls}})
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })

		require.NoError(t, syncMFAFlagsTx(ctx, tx, "iu-fault-user-b", emailSmsState{known: true}))
		assert.Equal(t, int32(1), atomic.LoadInt32(&execCalls))
	})

	t.Run("Redis Exists error still writes, keeping the email/SMS factors the row lists", func(t *testing.T) {
		db := iuNewTestDB(t)
		kr := iuKeyring(t)
		userID := iuCreateUser(t, db, iuPassword)
		iuSetFlags(t, db, userID, []string{"email"})
		h := iuHandler(db, iuExistsFaultRedis(t), kr)

		es := h.readEmailSmsForSync(ctx, userID)
		require.False(t, es.known, "fixture guard: the EXISTS fault must leave the state unknown")
		require.NoError(t, h.withMFAFactorWriteTx(ctx, userID, es, nil),
			"an unreadable email/SMS state must not block the flag write")

		enabled, methods, enabledAtSet := iuFlags(t, db, userID)
		assert.True(t, enabled, "a failed Redis read must not downgrade mfa_enabled")
		assert.Equal(t, []string{"email"}, methods, "a failed Redis read must not drop a listed email factor")
		assert.True(t, enabledAtSet)
	})
}

type iuExistsFaultHook struct{}

func (iuExistsFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (iuExistsFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "exists" {
			return errors.New("iu: forced exists failure")
		}
		return next(ctx, cmd)
	}
}

func (iuExistsFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// ── Case 6: flag sync under a Redis EXISTS fault (B1 / C1) ────────────────
//
// The email/SMS half of users.mfa_methods lives in Redis. Before B1 a failed
// EXISTS aborted the whole flag sync AFTER the factor had already been
// written, so an enrolled factor was never listed (login and refresh skipped
// the challenge while the user was told MFA was on) and a removed one stayed
// listed (login demanded a factor the account no longer had). Each case below
// drives a real handler against a real database with every EXISTS failing,
// and asserts the flags the handler left behind.

// iuFlags reads the denormalized MFA flags straight from the row.
func iuFlags(t *testing.T, db *sql.DB, userID string) (enabled bool, methods []string, enabledAtSet bool) {
	t.Helper()
	var at sql.NullTime
	require.NoError(t, db.QueryRow(`SELECT mfa_enabled, mfa_methods, mfa_enabled_at FROM users WHERE id = $1`, userID).
		Scan(&enabled, pq.Array(&methods), &at))
	return enabled, methods, at.Valid
}

// iuSetFlags seeds the denormalized flags, as a prior successful sync would
// have left them.
func iuSetFlags(t *testing.T, db *sql.DB, userID string, methods []string) {
	t.Helper()
	_, err := db.Exec(`UPDATE users SET mfa_methods = $1::text[], mfa_enabled = cardinality($1::text[]) > 0,
		mfa_enabled_at = CASE WHEN cardinality($1::text[]) > 0 THEN NOW() END WHERE id = $2`, pq.Array(methods), userID)
	require.NoError(t, err)
}

func iuExistsFaultRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := iuNewTestRedis(t)
	rdb.AddHook(iuExistsFaultHook{})
	return rdb
}

func iuWebAuthnCredentialID(t *testing.T, db *sql.DB, userID string) string {
	t.Helper()
	var id string
	require.NoError(t, db.QueryRow(`SELECT id FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&id))
	return id
}

func TestFlagSync_RedisExistsFault_TOTPConfirmListsTheFactor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	enc, nonce, keyVersion, err := kr.Seal([]byte(iuTOTPSecret))
	require.NoError(t, err)
	// Code verified, backup codes not yet acknowledged: the state
	// TOTPVerifySetup leaves behind.
	_, err = db.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, $4, TRUE, FALSE)`, userID, enc, nonce, keyVersion)
	require.NoError(t, err)

	h := iuHandler(db, iuExistsFaultRedis(t), kr)
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/confirm-setup", "", userID, "")
	h.TOTPConfirmSetup(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	enabled, methods, enabledAtSet := iuFlags(t, db, userID)
	assert.True(t, enabled, "a 200 'MFA is now active' must leave mfa_enabled set")
	assert.Equal(t, []string{"totp"}, methods)
	assert.True(t, enabledAtSet, "mfa_enabled_at gates the challenge of pre-existing sessions")
}

func TestFlagSync_RedisExistsFault_WebAuthnRegisterListsTheFactor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	rdb := iuExistsFaultRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	svc, err := NewWebAuthnService("webauthn.io", "test", []string{"https://webauthn.io"})
	require.NoError(t, err)
	sessionJSON, err := json.Marshal(webauthn.SessionData{
		Challenge: webAuthnChallenge, RelyingPartyID: "webauthn.io",
		UserID: []byte(userID), CredParams: webauthn.CredentialParametersDefault(),
	})
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), fmt.Sprintf(redisKeyWebAuthnReg, userID),
		`{"session":`+strconv.Quote(string(sessionJSON))+`,"credential_name":"Key","credential_type":"hardware"}`, time.Minute).Err())

	h := NewHandler(db, rdb, logger.New("test"), iuKeyring(t), "test", svc, "test")
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/register/finish", webAuthnRegistrationResponse, userID, "")
	h.WebAuthnRegisterFinish(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	enabled, methods, enabledAtSet := iuFlags(t, db, userID)
	assert.True(t, enabled, "a registered security key must be listed even when email/SMS state is unreadable")
	assert.Equal(t, []string{"webauthn"}, methods)
	assert.True(t, enabledAtSet)
}

func TestFlagSync_RedisExistsFault_TOTPDisableLeavesNoStaleFactor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	secret := iuEnrollTOTP(t, db, kr, userID)
	iuSetFlags(t, db, userID, []string{"totp"})

	h := iuHandler(db, iuExistsFaultRedis(t), kr)
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/disable",
		fmt.Sprintf(`{"password":%q,"code":%q}`, iuPassword, iuTOTPCode(t, secret)), userID, "")
	h.TOTPDisable(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	enabled, methods, enabledAtSet := iuFlags(t, db, userID)
	assert.False(t, enabled, "login must not demand a factor the account no longer holds")
	assert.Empty(t, methods)
	assert.False(t, enabledAtSet)
}

func TestFlagSync_RedisExistsFault_WebAuthnDeleteLeavesNoStaleFactor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	iuEnrollTOTP(t, db, kr, userID)
	iuAddWebAuthnCredential(t, db, userID)
	iuSetFlags(t, db, userID, []string{"totp", "webauthn"})
	credentialID := iuWebAuthnCredentialID(t, db, userID)

	h := iuHandler(db, iuExistsFaultRedis(t), kr)
	c, w := iuGinContext(http.MethodDelete, "/api/v1/mfa/webauthn/credentials/"+credentialID,
		fmt.Sprintf(`{"password":%q}`, iuPassword), userID, "")
	c.Params = gin.Params{{Key: "id", Value: credentialID}}
	h.WebAuthnDeleteCredential(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	enabled, methods, _ := iuFlags(t, db, userID)
	assert.True(t, enabled)
	assert.Equal(t, []string{"totp"}, methods, "the deleted key must not stay listed")
}

// ── Case 7: EmailSmsDisable rolls back on a real database (C3) ────────────
//
// The recovery-only update and the flag write run on the transaction; the
// Redis DEL of the four keys runs last, under the lock. When the DEL fails the
// deferred rollback must undo BOTH users writes — asserted against the real
// row, which the fake-driver event test cannot see.
func TestEmailSmsDisable_RedisDeleteFailureRollsBackTheRow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	iuSetFlags(t, db, userID, []string{"email"})
	_, err := db.Exec(`UPDATE users SET recovery_only_methods = '{email}' WHERE id = $1`, userID)
	require.NoError(t, err)

	rdb := iuNewTestRedis(t)
	emailKey := fmt.Sprintf(redisKeyEmailSmsEnabled, userID, "email")
	require.NoError(t, rdb.Set(context.Background(), emailKey, "1", 0).Err())
	rdb.AddHook(mfaEventDelFaultHook{})
	h := iuHandler(db, rdb, kr)
	recorder := &securityEventRecorder{}
	h.SetSecurityEvents(recorder)

	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/email-sms/disable",
		fmt.Sprintf(`{"password":%q}`, iuPassword), userID, "")
	h.EmailSmsDisable(c)

	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, map[string]interface{}{"error": "MFA service temporarily unavailable"}, iuBody(t, w))
	enabled, methods, enabledAtSet := iuFlags(t, db, userID)
	assert.True(t, enabled, "the rolled-back flag write must leave MFA on")
	assert.Equal(t, []string{"email"}, methods)
	assert.True(t, enabledAtSet)
	var recoveryOnly []string
	require.NoError(t, db.QueryRow(`SELECT recovery_only_methods FROM users WHERE id = $1`, userID).Scan(pq.Array(&recoveryOnly)))
	assert.Equal(t, []string{"email"}, recoveryOnly, "the rolled-back recovery-only update must leave the row unchanged")
	assert.Equal(t, int64(1), rdb.Exists(context.Background(), emailKey).Val(), "the email factor must still be on")
	assert.Empty(t, recorder.events, "nothing was disabled, so factor_disabled must not be emitted")
}

// ── Case 8: D1 — the last inline factor while email/SMS is on (409) ───────

func iuWantInlineFactorRequired(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Equal(t, map[string]interface{}{
		"error":                  "Turn off email and text-message codes before removing your last authenticator app or security key.",
		"inline_factor_required": true,
	}, iuBody(t, w))
}

func iuCountRows(t *testing.T, db *sql.DB, table, userID string) int {
	t.Helper()
	queries := map[string]string{
		"user_mfa_totp":     `SELECT count(*) FROM user_mfa_totp WHERE user_id = $1`,
		"user_mfa_webauthn": `SELECT count(*) FROM user_mfa_webauthn WHERE user_id = $1`,
	}
	query, ok := queries[table]
	require.True(t, ok, "unknown table %q", table)
	var n int
	require.NoError(t, db.QueryRow(query, userID).Scan(&n))
	return n
}

func TestD1_LastInlineFactorWhileEmailOrSmsIsOn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	ctx := context.Background()

	disableTOTP := func(t *testing.T, h *Handler, userID, secret string) *httptest.ResponseRecorder {
		t.Helper()
		c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/disable",
			fmt.Sprintf(`{"password":%q,"code":%q}`, iuPassword, iuTOTPCode(t, secret)), userID, "")
		h.TOTPDisable(c)
		return w
	}
	deleteKey := func(t *testing.T, h *Handler, userID string) *httptest.ResponseRecorder {
		t.Helper()
		credentialID := iuWebAuthnCredentialID(t, db, userID)
		c, w := iuGinContext(http.MethodDelete, "/api/v1/mfa/webauthn/credentials/"+credentialID,
			fmt.Sprintf(`{"password":%q}`, iuPassword), userID, "")
		c.Params = gin.Params{{Key: "id", Value: credentialID}}
		h.WebAuthnDeleteCredential(c)
		return w
	}

	t.Run("TOTPDisable of the only inline factor with email on is 409, nothing removed", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		secret := iuEnrollTOTP(t, db, kr, userID)
		iuSetFlags(t, db, userID, []string{"totp", "email"})
		rdb := iuNewTestRedis(t)
		require.NoError(t, rdb.Set(ctx, fmt.Sprintf(redisKeyEmailSmsEnabled, userID, "email"), "1", 0).Err())

		w := disableTOTP(t, iuHandler(db, rdb, kr), userID, secret)

		iuWantInlineFactorRequired(t, w)
		assert.Equal(t, 1, iuCountRows(t, db, "user_mfa_totp", userID), "the refused delete must roll back")
		_, methods, _ := iuFlags(t, db, userID)
		assert.Equal(t, []string{"totp", "email"}, methods)
	})

	t.Run("WebAuthn delete of the last key with SMS on is 409, key kept", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		iuAddWebAuthnCredential(t, db, userID)
		iuSetFlags(t, db, userID, []string{"webauthn", "sms"})
		rdb := iuNewTestRedis(t)
		require.NoError(t, rdb.Set(ctx, fmt.Sprintf(redisKeyEmailSmsEnabled, userID, "sms"), "1", 0).Err())

		w := deleteKey(t, iuHandler(db, rdb, kr), userID)

		iuWantInlineFactorRequired(t, w)
		assert.Equal(t, 1, iuCountRows(t, db, "user_mfa_webauthn", userID))
	})

	t.Run("unreadable email/SMS state falls back to the row's listing: 409", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		secret := iuEnrollTOTP(t, db, kr, userID)
		iuSetFlags(t, db, userID, []string{"totp", "email"})

		w := disableTOTP(t, iuHandler(db, iuExistsFaultRedis(t), kr), userID, secret)

		iuWantInlineFactorRequired(t, w)
		assert.Equal(t, 1, iuCountRows(t, db, "user_mfa_totp", userID))
	})

	t.Run("another inline factor remains: allowed, email kept in the flags", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		secret := iuEnrollTOTP(t, db, kr, userID)
		iuAddWebAuthnCredential(t, db, userID)
		iuSetFlags(t, db, userID, []string{"totp", "webauthn", "email"})
		rdb := iuNewTestRedis(t)
		require.NoError(t, rdb.Set(ctx, fmt.Sprintf(redisKeyEmailSmsEnabled, userID, "email"), "1", 0).Err())

		w := disableTOTP(t, iuHandler(db, rdb, kr), userID, secret)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Zero(t, iuCountRows(t, db, "user_mfa_totp", userID))
		_, methods, _ := iuFlags(t, db, userID)
		assert.Equal(t, []string{"webauthn", "email"}, methods)
	})

	t.Run("email and SMS off: removing the last factor is allowed", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		iuAddWebAuthnCredential(t, db, userID)
		iuSetFlags(t, db, userID, []string{"webauthn"})

		w := deleteKey(t, iuHandler(db, iuNewTestRedis(t), kr), userID)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Zero(t, iuCountRows(t, db, "user_mfa_webauthn", userID))
		enabled, methods, _ := iuFlags(t, db, userID)
		assert.False(t, enabled)
		assert.Empty(t, methods)
	})
}

// ── Case 9: the token-only first store is idempotent (F2) ─────────────────

func iuRecoveryKeyBody(key, salt string, extra string) string {
	return fmt.Sprintf(`{"recovery_wrapped_private_key":%q,"recovery_key_salt":%q%s}`,
		base64.StdEncoding.EncodeToString([]byte(key)), base64.StdEncoding.EncodeToString([]byte(salt)), extra)
}

func TestStoreRecoveryKey_RepeatedFirstStoreIsIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	ctx := context.Background()
	const key, salt = "F2-first-store-recovery-key-32b", "F2-first-salt-16"

	put := func(t *testing.T, h *Handler, userID, body string) *httptest.ResponseRecorder {
		t.Helper()
		c, w := iuGinContext(http.MethodPut, "/api/v1/mfa/recovery-key", body, userID, "")
		h.StoreRecoveryKey(c)
		return w
	}
	storedKey := func(t *testing.T, userID string) string {
		t.Helper()
		var b []byte
		require.NoError(t, db.QueryRow(`SELECT recovery_wrapped_private_key FROM user_recovery_keys WHERE user_id = $1`, userID).Scan(&b))
		return string(b)
	}

	t.Run("identical bytes, no credentials: 200, nothing written, budget untouched", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		rdb := iuNewTestRedis(t)
		h := iuHandler(db, rdb, kr)
		require.Equal(t, http.StatusOK, put(t, h, userID, iuRecoveryKeyBody(key, salt, "")).Code)
		var before time.Time
		require.NoError(t, db.QueryRow(`SELECT updated_at FROM user_recovery_keys WHERE user_id = $1`, userID).Scan(&before))
		require.NoError(t, rdb.Set(ctx, mfaSettingsStepUpKey(userID), "3", 0).Err())

		w := put(t, h, userID, iuRecoveryKeyBody(key, salt, ""))

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, map[string]interface{}{"message": "Recovery key stored"}, iuBody(t, w))
		var after time.Time
		require.NoError(t, db.QueryRow(`SELECT updated_at FROM user_recovery_keys WHERE user_id = $1`, userID).Scan(&after))
		assert.True(t, before.Equal(after), "an idempotent retry must not rewrite the row")
		assert.Equal(t, "3", rdb.Get(ctx, mfaSettingsStepUpKey(userID)).Val(), "nothing was verified, so the budget must not be cleared")
	})

	t.Run("different bytes, no credentials: the step-up refusal, key unchanged", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		h := iuHandler(db, iuNewTestRedis(t), kr)
		require.Equal(t, http.StatusOK, put(t, h, userID, iuRecoveryKeyBody(key, salt, "")).Code)

		w := put(t, h, userID, iuRecoveryKeyBody("F2-a-different-recovery-key-32b", salt, ""))

		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		assert.Equal(t, true, iuBody(t, w)["password_required"])
		assert.Equal(t, key, storedKey(t, userID))
	})

	t.Run("same key, prefs added where the row has NULL: mismatch, not a match", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		h := iuHandler(db, iuNewTestRedis(t), kr)
		require.Equal(t, http.StatusOK, put(t, h, userID, iuRecoveryKeyBody(key, salt, "")).Code)

		prefs := fmt.Sprintf(`,"recovery_wrapped_prefs_key":%q,"recovery_prefs_key_salt":%q`,
			base64.StdEncoding.EncodeToString([]byte("prefs-key")), base64.StdEncoding.EncodeToString([]byte("prefs-salt")))
		w := put(t, h, userID, iuRecoveryKeyBody(key, salt, prefs))

		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		var wrappedPrefs []byte
		require.NoError(t, db.QueryRow(`SELECT recovery_wrapped_prefs_key FROM user_recovery_keys WHERE user_id = $1`, userID).Scan(&wrappedPrefs))
		assert.Nil(t, wrappedPrefs, "the NULL prefs column must stay NULL")
	})

	t.Run("no stored row: not a match", func(t *testing.T) {
		userID := iuCreateUser(t, db, iuPassword)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })

		same, err := recoveryKeyMatchesTx(ctx, tx, userID, recoveryKeyMaterial{wrappedKey: []byte(key), keySalt: []byte(salt)})

		require.NoError(t, err)
		assert.False(t, same)
	})
}

func TestSameRecoveryColumn(t *testing.T) {
	cases := []struct {
		name string
		a, b []byte
		want int
	}{
		{"both NULL", nil, nil, 1},
		{"equal bytes", []byte("abc"), []byte("abc"), 1},
		{"NULL vs empty", nil, []byte{}, 0},
		{"empty vs NULL", []byte{}, nil, 0},
		{"NULL vs present", nil, []byte("abc"), 0},
		{"different length", []byte("abc"), []byte("abcd"), 0},
		{"same length, different byte", []byte("abc"), []byte("abd"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sameRecoveryColumn(tc.a, tc.b))
		})
	}
}
