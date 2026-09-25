package mfa

// Regression tests for the pre-existing silent failures in handlers.go that
// PR #3433's review deferred to this lockstep companion PR (Q1). Each test
// injects a fault into exactly one Redis command for one key prefix, so the
// branch under test is the only thing that can produce the asserted outcome.
//
// Class A — login MFA Verify. Redis .Val() returns the zero value on error, so
// a failed lockout read reads as "not locked" and a failed attempt INCR is
// never counted: TOTP codes could be guessed without limit during a Redis
// outage. Every arm must fail closed with the response the sibling SetNX
// failure already uses (500 "MFA verification unavailable"), before the code
// is evaluated.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// sfFaultHook fails one Redis command (or any of a "|"-separated set, so an
// oracle need not assume GET+DEL over GETDEL), and only for keys starting with
// prefix. hits counts the faults it injected, so a test can prove its fault
// fired rather than passing on a path that never reached the command.
type sfFaultHook struct {
	cmd    string
	prefix string
	hits   *atomic.Int32
}

func sfFault(cmd, prefix string) (sfFaultHook, *atomic.Int32) {
	hits := new(atomic.Int32)
	return sfFaultHook{cmd: cmd, prefix: prefix, hits: hits}, hits
}

func sfRequireFired(t *testing.T, hits *atomic.Int32) {
	t.Helper()
	require.Positive(t, hits.Load(), "the injected Redis fault never fired, so this test proves nothing")
}

func (sfFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h sfFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if slices.Contains(strings.Split(h.cmd, "|"), cmd.Name()) {
			if args := cmd.Args(); len(args) >= 2 {
				if k, ok := args[1].(string); ok && strings.HasPrefix(k, h.prefix) {
					h.hits.Add(1)
					err := fmt.Errorf("sf: forced %s failure on %s", h.cmd, k)
					cmd.SetErr(err)
					return err
				}
			}
		}
		return next(ctx, cmd)
	}
}

func (sfFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// sfVerifyFixture is one user with confirmed TOTP, a handler over a Redis
// client carrying the given hooks, and a clean (unhooked) client on the same
// server for reading state back.
type sfVerifyFixture struct {
	h      *Handler
	clean  *redis.Client
	userID string
	secret string
}

func sfNewVerifyFixture(t *testing.T, hooks ...redis.Hook) sfVerifyFixture {
	t.Helper()
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	clean := iuNewTestRedis(t)
	hooked := redis.NewClient(&redis.Options{Addr: clean.Options().Addr})
	t.Cleanup(func() { _ = hooked.Close() })
	for _, hk := range hooks {
		hooked.AddHook(hk)
	}
	userID := iuCreateUser(t, db, "sf-password-1")
	secret := iuEnrollTOTP(t, db, kr, userID)
	return sfVerifyFixture{h: iuHandler(db, hooked, kr), clean: clean, userID: userID, secret: secret}
}

// sfWrongCode returns a six-digit code guaranteed to differ from the valid one.
func sfWrongCode(t *testing.T, secret string) string {
	t.Helper()
	if iuTOTPCode(t, secret) == "000000" {
		return "111111"
	}
	return "000000"
}

// sfVerify posts one TOTP verification against a fresh suspicious-refresh
// challenge. That purpose needs no session-mint wiring, and every class-A
// branch sits before the purpose is consulted.
func sfVerify(t *testing.T, f sfVerifyFixture, code string) (int, map[string]interface{}) {
	t.Helper()
	token, _, err := GenerateChallengeToken(f.userID, PurposeSuspiciousRefresh, JWTSecret("test"), "")
	require.NoError(t, err)
	body := fmt.Sprintf(`{"mfa_challenge_token":%q,"method":"totp","code":%q}`, token, code)
	c, w := iuGinContext(http.MethodPost, "/api/v1/auth/mfa/verify", body, "", "")
	f.h.Verify(c)
	return w.Code, iuBody(t, w)
}

func sfAttempts(t *testing.T, f sfVerifyFixture) int {
	t.Helper()
	n, err := f.clean.Get(context.Background(), "mfa_verify_attempts:"+f.userID).Int()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	require.NoError(t, err)
	return n
}

func sfRequireUnavailable(t *testing.T, status int, body map[string]interface{}, property string) {
	t.Helper()
	assert.Equal(t, http.StatusInternalServerError, status, property)
	assert.Equal(t, errMsgMFAVerificationUnavailable, body["error"], property)
}

// Positive control: with no fault a wrong code is refused as invalid and
// counted. Proves the fixture reaches the code check and the counter.
func TestSilentFailureVerifyControl_WrongCodeIsCounted(t *testing.T) {
	f := sfNewVerifyFixture(t)
	status, body := sfVerify(t, f, sfWrongCode(t, f.secret))
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "Invalid MFA code", body["error"])
	assert.Equal(t, 1, sfAttempts(t, f), "a refused code must be counted toward the lockout")
}

func TestSilentFailureVerify_LockoutReadFault_DoesNotEvaluateCode(t *testing.T) {
	hook, hits := sfFault("exists", "mfa_verify_lockout:")
	f := sfNewVerifyFixture(t, hook)
	status, body := sfVerify(t, f, sfWrongCode(t, f.secret))
	sfRequireFired(t, hits)
	sfRequireUnavailable(t, status, body, "a Redis fault on the lockout read must fail closed, not read as 'not locked'")
	assert.Equal(t, 0, sfAttempts(t, f), "a Redis fault on the lockout read must not let a code be evaluated")
}

func TestSilentFailureVerify_UsedPrecheckFault_DoesNotEvaluateCode(t *testing.T) {
	hook, hits := sfFault("exists", "mfa_challenge_used:")
	f := sfNewVerifyFixture(t, hook)
	status, body := sfVerify(t, f, sfWrongCode(t, f.secret))
	sfRequireFired(t, hits)
	sfRequireUnavailable(t, status, body, "a Redis fault on the single-use pre-check must fail closed")
	assert.Equal(t, 0, sfAttempts(t, f), "a Redis fault on the single-use pre-check must not let a code be evaluated")
}

func TestSilentFailureVerify_AttemptIncrFault_FailsClosed(t *testing.T) {
	hook, hits := sfFault("incr", "mfa_verify_attempts:")
	f := sfNewVerifyFixture(t, hook)
	status, body := sfVerify(t, f, sfWrongCode(t, f.secret))
	sfRequireFired(t, hits)
	sfRequireUnavailable(t, status, body, "a wrong code whose attempt could not be counted must fail closed, not answer 'invalid code'")
}

func TestSilentFailureVerify_AttemptExpireFault_FailsClosed(t *testing.T) {
	hook, hits := sfFault("expire", "mfa_verify_attempts:")
	f := sfNewVerifyFixture(t, hook)
	status, body := sfVerify(t, f, sfWrongCode(t, f.secret))
	sfRequireFired(t, hits)
	sfRequireUnavailable(t, status, body, "an attempt counter whose window could not be set must fail closed")
}

// ── Class B — TOTPVerifySetup ──────────────────────────────────────────────
//
// The same shape on the enrolment path: mfa_setup_lockout / mfa_setup_attempts
// guard guessing the code that activates a new TOTP secret.

type sfSetupFixture struct {
	h      *Handler
	clean  *redis.Client
	userID string
	secret string
}

func sfNewSetupFixture(t *testing.T, hooks ...redis.Hook) sfSetupFixture {
	t.Helper()
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	clean := iuNewTestRedis(t)
	hooked := redis.NewClient(&redis.Options{Addr: clean.Options().Addr})
	t.Cleanup(func() { _ = hooked.Close() })
	for _, hk := range hooks {
		hooked.AddHook(hk)
	}
	userID := iuCreateUser(t, db, "sf-password-1")
	enc, nonce, keyVersion, err := kr.Seal([]byte(iuTOTPSecret))
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, $4, false, false)
	`, userID, enc, nonce, keyVersion)
	require.NoError(t, err)
	return sfSetupFixture{h: iuHandler(db, hooked, kr), clean: clean, userID: userID, secret: iuTOTPSecret}
}

func sfVerifySetup(t *testing.T, f sfSetupFixture, code string) (int, map[string]interface{}) {
	t.Helper()
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/verify-setup", fmt.Sprintf(`{"code":%q}`, code), f.userID, "")
	f.h.TOTPVerifySetup(c)
	return w.Code, iuBody(t, w)
}

func sfSetupAttempts(t *testing.T, f sfSetupFixture) int {
	t.Helper()
	n, err := f.clean.Get(context.Background(), "mfa_setup_attempts:"+f.userID).Int()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	require.NoError(t, err)
	return n
}

func TestSilentFailureSetupControl_WrongCodeIsCounted(t *testing.T) {
	f := sfNewSetupFixture(t)
	status, body := sfVerifySetup(t, f, sfWrongCode(t, f.secret))
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "Invalid code", body["error"])
	assert.Equal(t, 1, sfSetupAttempts(t, f), "a refused setup code must be counted toward the lockout")
}

func TestSilentFailureSetup_LockoutReadFault_DoesNotEvaluateCode(t *testing.T) {
	hook, hits := sfFault("exists", "mfa_setup_lockout:")
	f := sfNewSetupFixture(t, hook)
	status, _ := sfVerifySetup(t, f, sfWrongCode(t, f.secret))
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, status, "a Redis fault on the setup lockout read must fail closed")
	assert.Equal(t, 0, sfSetupAttempts(t, f), "a Redis fault on the setup lockout read must not let a code be evaluated")
}

func TestSilentFailureSetup_AttemptIncrFault_FailsClosed(t *testing.T) {
	hook, hits := sfFault("incr", "mfa_setup_attempts:")
	f := sfNewSetupFixture(t, hook)
	status, _ := sfVerifySetup(t, f, sfWrongCode(t, f.secret))
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, status, "a wrong setup code whose attempt could not be counted must fail closed")
}

func TestSilentFailureSetup_AttemptExpireFault_FailsClosed(t *testing.T) {
	hook, hits := sfFault("expire", "mfa_setup_attempts:")
	f := sfNewSetupFixture(t, hook)
	status, _ := sfVerifySetup(t, f, sfWrongCode(t, f.secret))
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, status, "a setup attempt counter whose window could not be set must fail closed")
}

func TestSilentFailureSetup_LockoutSetFault_FailsClosedAtThreshold(t *testing.T) {
	hook, hits := sfFault("set", "mfa_setup_lockout:")
	f := sfNewSetupFixture(t, hook)
	require.NoError(t, f.clean.Set(context.Background(), "mfa_setup_attempts:"+f.userID, 4, 0).Err())
	status, _ := sfVerifySetup(t, f, sfWrongCode(t, f.secret))
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, status, "the setup attempt that reaches the threshold must fail closed when the lockout cannot be written")
}

// ── SQL fault injection ─────────────────────────────────────────────────────
//
// sfFaultDB returns a *sql.DB over the test database whose connections fail
// exactly the statements containing match, on the pool and inside
// transactions alike. It wraps lib/pq's own connector, so every other
// statement runs for real — no production seam is needed to reach a branch
// that only a failing query can reach.

type sfFaultConnector struct {
	base  driver.Connector
	match string
	hits  *atomic.Int32
	// rowsOnly runs a matching Exec for real and fails only its row count.
	rowsOnly bool
}

func (c sfFaultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &sfFaultConn{Conn: conn, match: c.match, hits: c.hits, rowsOnly: c.rowsOnly}, nil
}

func (c sfFaultConnector) Driver() driver.Driver { return c.base.Driver() }

type sfFaultConn struct {
	driver.Conn
	match    string
	hits     *atomic.Int32
	rowsOnly bool
}

// sfRowsErrResult is a completed write whose row count cannot be read. The
// driver contract allows it, and a handler that reads it as zero rows reports
// "not found" for a write that ran.
type sfRowsErrResult struct{ driver.Result }

func (sfRowsErrResult) RowsAffected() (int64, error) {
	return 0, errors.New("sf: forced RowsAffected failure")
}

func (c *sfFaultConn) fault(query string) error {
	if !c.rowsOnly && strings.Contains(query, c.match) {
		c.hits.Add(1)
		return errors.New("sf: forced statement failure")
	}
	return nil
}

func (c *sfFaultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.fault(query); err != nil {
		return nil, err
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *sfFaultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.fault(query); err != nil {
		return nil, err
	}
	res, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err == nil && c.rowsOnly && strings.Contains(query, c.match) {
		c.hits.Add(1)
		return sfRowsErrResult{res}, nil
	}
	return res, err
}

func (c *sfFaultConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := c.fault(query); err != nil {
		return nil, err
	}
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c *sfFaultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *sfFaultConn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}

func sfFaultDB(t *testing.T, match string) (*sql.DB, *atomic.Int32) {
	return sfOpenFaultDB(t, match, false)
}

// sfRowsFaultDB runs every statement for real; a matching Exec reports that
// its row count could not be read.
func sfRowsFaultDB(t *testing.T, match string) (*sql.DB, *atomic.Int32) {
	return sfOpenFaultDB(t, match, true)
}

func sfOpenFaultDB(t *testing.T, match string, rowsOnly bool) (*sql.DB, *atomic.Int32) {
	t.Helper()
	dbtest.SetupTestDB(t)
	base, err := pq.NewConnector(dbtest.DatabaseURL())
	require.NoError(t, err)
	hits := new(atomic.Int32)
	db := sql.OpenDB(sfFaultConnector{base: base, match: match, hits: hits, rowsOnly: rowsOnly})
	t.Cleanup(func() { _ = db.Close() })
	return db, hits
}

// ── Class C — TOTPSetup ─────────────────────────────────────────────────────
//
// The re-enrol guard reads `confirmed` and proceeds when the read FAILS. The
// upsert that follows resets enabled and confirmed to FALSE, so a transient
// read error during a legitimate re-enrol silently disables a working TOTP.

func TestSilentFailureTOTPSetup_ConfirmedReadFault_KeepsTheExistingFactor(t *testing.T) {
	db, hits := sfFaultDB(t, "SELECT confirmed FROM user_mfa_totp WHERE user_id = $1")
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	secret := iuEnrollTOTP(t, db, kr, userID)
	h := iuHandler(db, iuNewTestRedis(t), kr)

	body := fmt.Sprintf(`{"password":%q,"mfa_code":%q}`, iuPassword, iuTOTPCode(t, secret))
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/setup", body, userID, "")
	h.TOTPSetup(c)

	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "an unreadable re-enrol guard must fail closed, not proceed to overwrite the factor")
	var enabled, confirmed bool
	require.NoError(t, db.QueryRow(`SELECT enabled, confirmed FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(&enabled, &confirmed))
	assert.True(t, enabled && confirmed, "a confirmed TOTP must survive a failed re-enrol guard read")
}

// ── Class G — SetRecoveryOnly ───────────────────────────────────────────────
//
// The response re-reads recovery_hardened and discards the read error, so a
// failed read reports recovery_hardened=false for an account where it is true.

func TestSilentFailureSetRecoveryOnly_HardenedReadFault_NeverReportsFalseForTrue(t *testing.T) {
	db, _ := sfFaultDB(t, "SELECT recovery_hardened FROM users WHERE id = $1")
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	secret := iuEnrollTOTP(t, db, kr, userID)
	iuSetFlags(t, db, userID, []string{"totp", "email"})
	h := iuHandler(db, iuNewTestRedis(t), kr)

	// Marking email recovery-only writes recovery_hardened = TRUE, then re-reads it.
	body := fmt.Sprintf(`{"methods":["email"],"password":%q,"mfa_code":%q}`, iuPassword, iuTOTPCode(t, secret))
	c, w := iuGinContext(http.MethodPut, "/api/v1/mfa/recovery-only", body, userID, "")
	h.SetRecoveryOnly(c)

	// No fault-fired precondition here: a fix that returns the value from the
	// UPDATE (RETURNING) removes the read altogether, which is correct. The two
	// acceptable outcomes are a 200 carrying the TRUE value, or a fail-closed
	// 500. Anything else means the request never reached the write, and the
	// test would pass while checking nothing.
	require.Contains(t, []int{http.StatusOK, http.StatusInternalServerError}, w.Code,
		"the request must reach the post-write read; got %d %s", w.Code, w.Body.String())
	if w.Code == http.StatusOK {
		assert.Equal(t, true, iuBody(t, w)["recovery_hardened"],
			"a 200 must never report recovery_hardened=false for an account where it is true")
	}
}

// Found while proving the class-G test reached its read: clearing the
// recovery-only list sends `methods: []`, filterValidRecoveryOnly returns a nil
// slice, pq.Array(nil) binds SQL NULL, and the NOT NULL column refuses it — so
// clearing recovery-only methods has always answered 500.
func TestSilentFailureSetRecoveryOnly_ClearingTheListSucceeds(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	secret := iuEnrollTOTP(t, db, kr, userID)
	iuSetFlags(t, db, userID, []string{"totp", "email"})
	_, err := db.Exec(`UPDATE users SET recovery_only_methods = ARRAY['email'] WHERE id = $1`, userID)
	require.NoError(t, err)
	h := iuHandler(db, iuNewTestRedis(t), kr)

	body := fmt.Sprintf(`{"methods":[],"password":%q,"mfa_code":%q}`, iuPassword, iuTOTPCode(t, secret))
	c, w := iuGinContext(http.MethodPut, "/api/v1/mfa/recovery-only", body, userID, "")
	h.SetRecoveryOnly(c)

	require.Equal(t, http.StatusOK, w.Code, "clearing the recovery-only list must succeed: %s", w.Body.String())
	var stored []string
	require.NoError(t, db.QueryRow(`SELECT recovery_only_methods FROM users WHERE id = $1`, userID).Scan(pq.Array(&stored)))
	assert.Empty(t, stored, "clearing must store an empty list, never NULL")
}

// ── Class E — challenges and sessions written at issue time ─────────────────
//
// Each of these stores the state a later step needs, ignores the write error,
// and hands the client a challenge that can never complete.

func sfHooked(t *testing.T, hooks ...redis.Hook) *redis.Client {
	t.Helper()
	clean := iuNewTestRedis(t)
	hooked := redis.NewClient(&redis.Options{Addr: clean.Options().Addr})
	t.Cleanup(func() { _ = hooked.Close() })
	for _, hk := range hooks {
		hooked.AddHook(hk)
	}
	return hooked
}

func sfWebAuthnHandler(t *testing.T, db *sql.DB, rdb *redis.Client, kr *Keyring) *Handler {
	t.Helper()
	svc, err := NewWebAuthnService("webauthn.io", "test", []string{"https://webauthn.io"})
	require.NoError(t, err)
	return NewHandler(db, rdb, logger.New("test"), kr, "test", svc, "test")
}

func TestSilentFailureBeginWebAuthnLogin_SessionWriteFault_ReturnsError(t *testing.T) {
	hook, hits := sfFault("set", "mfa_webauthn_session:")
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	iuAddWebAuthnCredential(t, db, userID)
	h := sfWebAuthnHandler(t, db, sfHooked(t, hook), kr)

	options, err := h.BeginWebAuthnLogin(context.Background(), userID, "sf-jti")
	sfRequireFired(t, hits)
	assert.Error(t, err, "a login challenge whose session could not be stored must not be handed out")
	assert.Nil(t, options, "no assertion options may be returned when the session was not stored")
}

func TestSilentFailureWebAuthnVerifyInlineBegin_SessionWriteFault_FailsClosed(t *testing.T) {
	hook, hits := sfFault("set", "mfa_inline_session:")
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	iuAddWebAuthnCredential(t, db, userID)
	h := sfWebAuthnHandler(t, db, sfHooked(t, hook), kr)

	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/verify-inline/begin", `{}`, userID, "")
	h.WebAuthnVerifyInlineBegin(c)
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "an inline challenge whose session could not be stored must fail closed, not return options")
}

func TestSilentFailureWebAuthnRegisterBegin_SessionWriteFault_FailsClosed(t *testing.T) {
	hook, hits := sfFault("set", "webauthn_reg:")
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	secret := iuEnrollTOTP(t, db, kr, userID)
	h := sfWebAuthnHandler(t, db, sfHooked(t, hook), kr)

	body := fmt.Sprintf(`{"credential_name":"Key","credential_type":"hardware","password":%q,"mfa_code":%q}`, iuPassword, iuTOTPCode(t, secret))
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/register/begin", body, userID, "")
	h.WebAuthnRegisterBegin(c)
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "a registration whose session could not be stored must fail closed, not return options")
}

func TestSilentFailureEmailSmsSetupCodes_WriteFault_ReturnsError(t *testing.T) {
	hook, hits := sfFault("set", "mfa_emailsms_setup:")
	h := iuHandler(iuNewTestDB(t), sfHooked(t, hook), iuKeyring(t))

	codes, err := h.generateAndStoreEmailSmsCodes(context.Background(), "00000000-0000-0000-0000-000000000001", []string{"email"})
	sfRequireFired(t, hits)
	assert.Error(t, err, "a setup code that could not be stored must not be sent to the user")
	assert.Nil(t, codes, "no codes may be returned when they were not stored")
}

// ── Class F — SendEmailMFACode ──────────────────────────────────────────────
//
// One email per challenge is enforced by an EXISTS read that fails open, and
// the "is email MFA enabled" read turns a Redis fault into a false "not
// enabled" answer.

func sfEmailFixture(t *testing.T, hooks ...redis.Hook) (*Handler, string, string) {
	t.Helper()
	db := iuNewTestDB(t)
	rdb := sfHooked(t, hooks...)
	userID := iuCreateUser(t, db, iuPassword)
	require.NoError(t, rdb.Set(context.Background(), fmt.Sprintf(redisKeyEmailSmsEnabledEmail, userID), "1", 0).Err())
	token, _, err := GenerateChallengeToken(userID, PurposeSuspiciousRefresh, JWTSecret("test"), "")
	require.NoError(t, err)
	return iuHandler(db, rdb, iuKeyring(t)), userID, token
}

func sfSendEmailCode(h *Handler, token string) *httptest.ResponseRecorder {
	c, w := iuGinContext(http.MethodPost, "/api/v1/auth/mfa/email/send", fmt.Sprintf(`{"mfa_challenge_token":%q}`, token), "", "")
	h.SendEmailMFACode(c)
	return w
}

func TestSilentFailureSendEmailCodeControl_Sends(t *testing.T) {
	h, _, token := sfEmailFixture(t)
	assert.Equal(t, http.StatusOK, sfSendEmailCode(h, token).Code, "with no fault the email code is sent")
}

func TestSilentFailureSendEmailCode_SentMarkerReadFault_DoesNotResend(t *testing.T) {
	// The marker may be read by EXISTS or reserved by SET NX; fault either.
	hook, hits := sfFault("exists|set|setnx", "mfa_email_sent:")
	h, _, token := sfEmailFixture(t, hook)
	w := sfSendEmailCode(h, token)
	sfRequireFired(t, hits)
	assert.GreaterOrEqual(t, w.Code, 500, "an unreadable one-email-per-challenge marker must fail closed, not send again")
}

func TestSilentFailureSendEmailCode_EnabledReadFault_IsNotReportedAsDisabled(t *testing.T) {
	hook, hits := sfFault("exists", "mfa_emailsms_enabled:")
	h, _, token := sfEmailFixture(t, hook)
	w := sfSendEmailCode(h, token)
	sfRequireFired(t, hits)
	assert.GreaterOrEqual(t, w.Code, 500, "an unreadable 'email MFA enabled' flag is a server fault, not 'not enabled'")
}

// ── Class D — WebAuthnVerifyInlineFinish ────────────────────────────────────
//
// After a genuinely verified assertion the handler (D1) discards the sign_count
// update, which is what cloned-authenticator detection compares against;
// (D2) reads the session with GET and deletes it with an unchecked DEL, so a
// failed delete leaves a single-use ceremony reusable; (D3) stores the inline
// token with an unchecked SET, returning a token the server will never accept.
// Reaching any of them needs a real signed assertion, so sfAuthenticator is a
// minimal software authenticator: a P-256 key stored as the credential's COSE
// public key, signing over the session's real challenge.

type sfAuthenticator struct {
	key    *ecdsa.PrivateKey
	credID []byte
	count  uint32
}

func sfNewAuthenticator(t *testing.T, db *sql.DB, userID string) *sfAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := key.PublicKey.ECDH()
	require.NoError(t, err)
	point := pub.Bytes() // 0x04 || X || Y
	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
		Curve:         int64(webauthncose.P256),
		XCoord:        point[1:33],
		YCoord:        point[33:65],
	})
	require.NoError(t, err)
	credID := []byte("sf-credential-" + userID)
	_, err = db.Exec(`
		INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
		VALUES ($1, $2, $3, 'Software Key', 'hardware', $4, 0, NOW())
	`, uuid.New().String(), userID, credID, cose)
	require.NoError(t, err)
	return &sfAuthenticator{key: key, credID: credID}
}

// assertion signs a WebAuthn get() response over challenge for the webauthn.io
// relying party, with user-present and user-verified set.
func (a *sfAuthenticator) assertion(t *testing.T, challenge string) string {
	t.Helper()
	a.count++
	clientData, err := json.Marshal(map[string]any{
		"type": "webauthn.get", "challenge": challenge, "origin": "https://webauthn.io", "crossOrigin": false,
	})
	require.NoError(t, err)
	rpID := sha256.Sum256([]byte("webauthn.io"))
	authData := binary.BigEndian.AppendUint32(append(rpID[:], 0x05), a.count)
	clientHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	require.NoError(t, err)
	b64 := base64.RawURLEncoding.EncodeToString
	return fmt.Sprintf(`{"id":%q,"rawId":%q,"type":"public-key","response":{"authenticatorData":%q,"clientDataJSON":%q,"signature":%q}}`,
		b64(a.credID), b64(a.credID), b64(authData), b64(clientData), b64(sig))
}

// sfInlineVerify runs a real inline ceremony: begin, read the stored session's
// challenge through the unhooked client, sign it, finish.
func sfInlineVerify(t *testing.T, h *Handler, clean *redis.Client, auth *sfAuthenticator, userID string) (int, map[string]interface{}) {
	t.Helper()
	c1, w1 := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/verify-inline/begin", `{}`, userID, "")
	h.WebAuthnVerifyInlineBegin(c1)
	require.Equal(t, http.StatusOK, w1.Code, "inline begin must succeed: %s", w1.Body.String())
	raw, err := clean.Get(context.Background(), "mfa_inline_session:"+userID).Bytes()
	require.NoError(t, err)
	var session webauthn.SessionData
	require.NoError(t, json.Unmarshal(raw, &session))

	c2, w2 := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/verify-inline/finish", auth.assertion(t, session.Challenge), userID, "")
	h.WebAuthnVerifyInlineFinish(c2)
	return w2.Code, iuBody(t, w2)
}

func sfInlineFixture(t *testing.T, db *sql.DB, hooks ...redis.Hook) (*Handler, *redis.Client, *sfAuthenticator, string) {
	t.Helper()
	clean := iuNewTestRedis(t)
	hooked := redis.NewClient(&redis.Options{Addr: clean.Options().Addr})
	t.Cleanup(func() { _ = hooked.Close() })
	for _, hk := range hooks {
		hooked.AddHook(hk)
	}
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	return sfWebAuthnHandler(t, db, hooked, iuKeyring(t)), clean, auth, userID
}

// Positive control: the software authenticator completes a real ceremony, and
// the sign count it presented is recorded.
func TestSilentFailureInlineFinishControl_IssuesTokenAndRecordsSignCount(t *testing.T) {
	db := iuNewTestDB(t)
	h, clean, auth, userID := sfInlineFixture(t, db)
	status, body := sfInlineVerify(t, h, clean, auth, userID)
	require.Equal(t, http.StatusOK, status, "the software authenticator must complete a real ceremony: %v", body)
	assert.NotEmpty(t, body["mfa_token"])
	var signCount int64
	require.NoError(t, db.QueryRow(`SELECT sign_count FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&signCount))
	assert.EqualValues(t, 1, signCount, "a completed ceremony must record the presented sign count")
}

func TestSilentFailureInlineFinish_SignCountWriteFault_IssuesNoToken(t *testing.T) {
	db, hits := sfFaultDB(t, "UPDATE user_mfa_webauthn SET sign_count")
	h, clean, auth, userID := sfInlineFixture(t, db)
	status, body := sfInlineVerify(t, h, clean, auth, userID)
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, status, "an assertion whose sign count could not be recorded must fail closed")
	assert.Nil(t, body["mfa_token"], "no inline token may be issued when cloned-key detection could not be updated")
}

func TestSilentFailureInlineFinish_SessionConsumeFault_IssuesNoToken(t *testing.T) {
	hook, hits := sfFault("del|getdel", "mfa_inline_session:")
	db := iuNewTestDB(t)
	h, clean, auth, userID := sfInlineFixture(t, db, hook)
	status, body := sfInlineVerify(t, h, clean, auth, userID)
	sfRequireFired(t, hits)
	assert.NotEqual(t, http.StatusOK, status, "a single-use session that could not be consumed must not complete")
	assert.Nil(t, body["mfa_token"], "no inline token may be issued from a session that is still reusable")
}

func TestSilentFailureInlineFinish_TokenWriteFault_FailsClosed(t *testing.T) {
	hook, hits := sfFault("set", "mfa_inline_token:")
	db := iuNewTestDB(t)
	h, clean, auth, userID := sfInlineFixture(t, db, hook)
	status, body := sfInlineVerify(t, h, clean, auth, userID)
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, status, "a token that could not be stored must not be returned")
	assert.Nil(t, body["mfa_token"], "the client must not receive a token the server will never accept")
}

func TestSilentFailureVerify_LockoutSetFault_FailsClosedAtThreshold(t *testing.T) {
	hook, hits := sfFault("set", "mfa_verify_lockout:")
	f := sfNewVerifyFixture(t, hook)
	require.NoError(t, f.clean.Set(context.Background(), "mfa_verify_attempts:"+f.userID, 4, 0).Err())
	status, body := sfVerify(t, f, sfWrongCode(t, f.secret))
	sfRequireFired(t, hits)
	sfRequireUnavailable(t, status, body, "the attempt that reaches the threshold must fail closed when the lockout cannot be written")
}

// ── Round 2 — found by the class A–H fix, outside its brief ─────────────────
//
// I: login WebAuthn verification has the inline flow's two defects (class D):
// a failed sign-count write is logged and the assertion still verifies, and the
// session is read with GET and deleted with a best-effort DEL. J: a send whose
// one-email-per-challenge marker could not be written still answers 200, so the
// next request sends again. K: three handlers discard RowsAffected's error and
// read it as zero rows, answering "not found" or "already responded" for a
// write that ran.

// sfLoginWebAuthnVerify runs a real login verification: a challenge token,
// BeginWebAuthnLogin for its jti, the stored session's challenge signed by the
// software authenticator, then Verify.
func sfLoginWebAuthnVerify(t *testing.T, h *Handler, clean *redis.Client, auth *sfAuthenticator, userID string) (int, map[string]interface{}) {
	t.Helper()
	token, jti, err := GenerateChallengeToken(userID, PurposeSuspiciousRefresh, JWTSecret("test"), "")
	require.NoError(t, err)
	_, err = h.BeginWebAuthnLogin(context.Background(), userID, jti)
	require.NoError(t, err)
	raw, err := clean.Get(context.Background(), "mfa_webauthn_session:"+jti).Bytes()
	require.NoError(t, err)
	var session webauthn.SessionData
	require.NoError(t, json.Unmarshal(raw, &session))
	body := fmt.Sprintf(`{"mfa_challenge_token":%q,"method":"webauthn","assertion":%s}`, token, auth.assertion(t, session.Challenge))
	c, w := iuGinContext(http.MethodPost, "/api/v1/auth/mfa/verify", body, "", "")
	h.Verify(c)
	return w.Code, iuBody(t, w)
}

// Positive control: the software authenticator completes a login verification
// and its sign count is recorded, so the fault cases below reach the write.
func TestSilentFailureLoginWebAuthnControl_VerifiesAndRecordsSignCount(t *testing.T) {
	db := iuNewTestDB(t)
	h, clean, auth, userID := sfInlineFixture(t, db)
	status, body := sfLoginWebAuthnVerify(t, h, clean, auth, userID)
	require.Equal(t, http.StatusOK, status, "the software authenticator must verify a login: %v", body)
	var signCount int64
	require.NoError(t, db.QueryRow(`SELECT sign_count FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&signCount))
	assert.EqualValues(t, 1, signCount, "a verified login must record the presented sign count")
}

func TestSilentFailureLoginWebAuthn_SignCountWriteFault_DoesNotVerify(t *testing.T) {
	db, hits := sfFaultDB(t, "UPDATE user_mfa_webauthn SET sign_count")
	h, clean, auth, userID := sfInlineFixture(t, db)
	status, body := sfLoginWebAuthnVerify(t, h, clean, auth, userID)
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, status, "a login whose sign count could not be recorded must fail closed")
	assert.NotEqual(t, true, body["verified"], "cloned-key detection was not updated, so the login must not verify")
}

func TestSilentFailureLoginWebAuthn_SessionConsumeFault_DoesNotVerify(t *testing.T) {
	hook, hits := sfFault("del|getdel", "mfa_webauthn_session:")
	db := iuNewTestDB(t)
	h, clean, auth, userID := sfInlineFixture(t, db, hook)
	status, body := sfLoginWebAuthnVerify(t, h, clean, auth, userID)
	sfRequireFired(t, hits)
	assert.NotEqual(t, http.StatusOK, status, "a single-use session that could not be consumed must not verify")
	assert.NotEqual(t, true, body["verified"])
}

func TestSilentFailureSendEmailCode_SentMarkerWriteFault_DoesNotSucceed(t *testing.T) {
	hook, hits := sfFault("set|setnx", "mfa_email_sent:")
	h, _, token := sfEmailFixture(t, hook)
	w := sfSendEmailCode(h, token)
	sfRequireFired(t, hits)
	assert.GreaterOrEqual(t, w.Code, 500, "a send whose one-email-per-challenge marker cannot be written must not succeed, or the next request sends again")
}

func sfRemoveTrustedDevice(t *testing.T, db *sql.DB) *httptest.ResponseRecorder {
	t.Helper()
	userID := iuCreateUser(t, db, iuPassword)
	h := iuHandler(db, iuNewTestRedis(t), iuKeyring(t))
	c, w := iuGinContext(http.MethodDelete, "/api/v1/mfa/trusted-devices/x", fmt.Sprintf(`{"password":%q}`, iuPassword), userID, "")
	c.Params = gin.Params{{Key: "id", Value: uuid.New().String()}}
	h.RemoveTrustedDevice(c)
	return w
}

// Positive control: an unknown device is "not found", so the fault case below
// is observing the row count and not an earlier refusal.
func TestSilentFailureRemoveTrustedDeviceControl_UnknownDeviceIsNotFound(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, sfRemoveTrustedDevice(t, iuNewTestDB(t)).Code)
}

func TestSilentFailureRemoveTrustedDevice_RowCountFault_IsNotReportedAsNotFound(t *testing.T) {
	db, hits := sfRowsFaultDB(t, "DELETE FROM trusted_recovery_devices")
	w := sfRemoveTrustedDevice(t, db)
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "a delete whose row count is unknown must not be answered as 'not found'")
}

func sfDeleteRecoveryCircle(t *testing.T, db *sql.DB) *httptest.ResponseRecorder {
	t.Helper()
	userID := iuCreateUser(t, db, iuPassword)
	h := iuHandler(db, iuNewTestRedis(t), iuKeyring(t))
	c, w := iuGinContext(http.MethodDelete, "/api/v1/mfa/recovery-circle", fmt.Sprintf(`{"password":%q}`, iuPassword), userID, "")
	h.DeleteRecoveryCircle(c)
	return w
}

func TestSilentFailureDeleteRecoveryCircleControl_NoCircleIsNotFound(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, sfDeleteRecoveryCircle(t, iuNewTestDB(t)).Code)
}

func TestSilentFailureDeleteRecoveryCircle_RowCountFault_IsNotReportedAsNotFound(t *testing.T) {
	db, hits := sfRowsFaultDB(t, "DELETE FROM recovery_circles")
	w := sfDeleteRecoveryCircle(t, db)
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "a delete whose row count is unknown must not be answered as 'not found'")
}

// sfRecoveryRequest seeds a circle owner, a contact and a pending request, and
// returns the request id and the contact.
func sfRecoveryRequest(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	owner := iuCreateUser(t, db, iuPassword)
	contact := iuCreateUser(t, db, iuPassword)
	var circleID, requestID string
	require.NoError(t, db.QueryRow(
		`INSERT INTO recovery_circles (user_id, threshold_k, total_shares_n) VALUES ($1, 2, 2) RETURNING id`, owner,
	).Scan(&circleID))
	require.NoError(t, db.QueryRow(`
		INSERT INTO recovery_circle_requests (circle_id, user_id, recovery_token_jti, ephemeral_public_key, expires_at)
		VALUES ($1, $2, 'sf-jti', '\x00', NOW() + INTERVAL '1 hour') RETURNING id`, circleID, owner,
	).Scan(&requestID))
	return requestID, contact
}

func TestSilentFailureSocialRecoveryResponseControl_IsRecorded(t *testing.T) {
	db := iuNewTestDB(t)
	requestID, contact := sfRecoveryRequest(t, db)
	h := iuHandler(db, iuNewTestRedis(t), iuKeyring(t))
	received, msg, status := h.executeSocialRecoveryResponse(context.Background(), requestID, contact, []byte("share"), 2)
	assert.Equal(t, 0, status, msg)
	assert.Equal(t, 1, received)
}

func TestSilentFailureSocialRecoveryResponse_RowCountFault_IsNotReportedAsDuplicate(t *testing.T) {
	db, hits := sfRowsFaultDB(t, "INSERT INTO recovery_circle_responses")
	requestID, contact := sfRecoveryRequest(t, db)
	h := iuHandler(db, iuNewTestRedis(t), iuKeyring(t))
	_, msg, status := h.executeSocialRecoveryResponse(context.Background(), requestID, contact, []byte("share"), 2)
	sfRequireFired(t, hits)
	assert.Equal(t, http.StatusInternalServerError, status, "an insert whose row count is unknown must not be answered as 'already responded': %s", msg)
}
