package mfa

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// The pre-MFA refresh lock (auth.checkPreMFASessionLock) challenges every
// session older than users.mfa_enabled_at, and consumes the upgrade-bypass key
// for the exact session a verified challenge wrote it for
// (TestPreMFASessionBypassIsSessionBoundAndConsumedOnce). These cases pin the
// other half: which session an enrollment writes that key for.

const enrollUser = "user-fixture"

// enrollConn answers the two reads the shared fake does not: whether MFA was
// ever on for the account, and verify-setup's four-column TOTP row. It also
// models the enable write stamping mfa_enabled_at, so a handler that reads the
// flag after that write, rather than before, sees MFA as already on.
type enrollConn struct {
	mfaEventConn
	neverEnabled bool
	enabledAtSet *bool
	totpActive   *bool
	secretEnc    []byte
	secretNonce  []byte
	keyVersion   int
	// failQuery, when set, fails any read whose SQL contains it. disabledAt
	// records a write that clears mfa_enabled_at. confirmed, when set, models
	// user_mfa_totp.confirmed across requests.
	failQuery  *string
	disabledAt *bool
	confirmed  *bool
	// credentialRemoved records a delete of a WebAuthn credential.
	credentialRemoved *bool
}

// ExecContext records a write only when it succeeded.
func (c enrollConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.mfaEventConn.ExecContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.Contains(query, "mfa_enabled_at = COALESCE"):
		*c.enabledAtSet = true
	case c.disabledAt != nil && strings.Contains(query, "mfa_enabled_at = NULL"):
		*c.disabledAt = true
	case c.confirmed != nil && strings.Contains(query, "SET confirmed = TRUE"):
		*c.confirmed = true
	case c.credentialRemoved != nil && strings.Contains(query, "DELETE FROM user_mfa_webauthn"):
		*c.credentialRemoved = true
	}
	return result, nil
}

func (c enrollConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.failQuery != nil && *c.failQuery != "" && strings.Contains(query, *c.failQuery) {
		return nil, fmt.Errorf("read failed: %s", *c.failQuery)
	}
	switch {
	case c.confirmed != nil && strings.Contains(query, "enabled, confirmed FROM user_mfa_totp"):
		return &mfaEventRows{values: []driver.Value{true, *c.confirmed}}, nil
	case strings.Contains(query, "= ANY(mfa_methods)"):
		return &mfaEventRows{values: []driver.Value{*c.enabledAtSet}}, nil
	case strings.Contains(query, "mfa_enabled_at IS NULL"):
		return &mfaEventRows{values: []driver.Value{c.neverEnabled && !*c.enabledAtSet}}, nil
	case c.totpActive != nil && strings.Contains(query, "enabled AND confirmed"):
		return &mfaEventRows{values: []driver.Value{*c.totpActive}}, nil
	case strings.Contains(query, "key_version, enabled FROM user_mfa_totp"):
		return &mfaEventRows{values: []driver.Value{c.secretEnc, c.secretNonce, int64(c.keyVersion), false}}, nil
	}
	return c.mfaEventConn.QueryContext(ctx, query, args)
}

type enrollConnector struct{ conn enrollConn }

func (c enrollConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (enrollConnector) Driver() driver.Driver                          { return mfaEventDriver{} }

// enrollRequest builds a request authenticated as enrollUser. An empty
// sessionID models a legacy access token minted without a sid claim.
func enrollRequest(path, body, sessionID string) (*gin.Context, *httptest.ResponseRecorder) {
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", enrollUser)
	if sessionID != "" {
		c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"sid": sessionID})
	}
	return c, response
}

func newEnrollmentRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return mini, client
}

// enrollTOTPAs proves the code at verify-setup as verifier, then activates MFA
// at confirm-setup as confirmer.
func enrollTOTPAs(t *testing.T, neverEnabled bool, verifier, confirmer string) *miniredis.Miniredis {
	t.Helper()
	mini, confirm := runTOTPEnrollment(t, &mfaEventDB{}, neverEnabled, verifier, confirmer)
	require.Equal(t, http.StatusOK, confirm.Code, confirm.Body.String())
	return mini
}

func runTOTPEnrollment(t *testing.T, state *mfaEventDB, neverEnabled bool, verifier, confirmer string, faults ...confirmFaults) (*miniredis.Miniredis, *httptest.ResponseRecorder) {
	t.Helper()
	e := newTOTPEnrollment(t, state, neverEnabled)
	e.verify(t, verifier)
	for _, f := range faults {
		e.arm(f)
	}
	return e.mini, e.confirm(confirmer)
}

// confirmFaults fail updateUserMFAFlags' reads during confirm-setup only, after
// verify-setup has succeeded.
type confirmFaults struct {
	failQuery  string // fail the DB read whose SQL contains this
	failExists bool   // fail every Redis EXISTS
}

// failCommandHook fails every Redis command with this lower-case name, and
// nothing else. Arm it after the setup commands that must succeed.
type failCommandHook struct{ name string }

func (failCommandHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (failCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (f failCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == f.name {
			err := errors.New("redis command failed")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

// totpEnrollment is one account's TOTP enrollment against the fake, so a test
// can call confirm-setup more than once.
type totpEnrollment struct {
	h          *Handler
	mini       *miniredis.Miniredis
	redis      *redis.Client
	code       string
	failQuery  *string
	disabledAt *bool
}

func newTOTPEnrollment(t *testing.T, state *mfaEventDB, neverEnabled bool) *totpEnrollment {
	t.Helper()
	mini, redisClient := newEnrollmentRedis(t)
	keyring, err := ParseKeyring("0101010101010101010101010101010101010101010101010101010101010101", 1, "")
	require.NoError(t, err)
	secret := "JBSWY3DPEHPK3PXP" //nolint:gosec // test-only TOTP seed // pragma: allowlist secret
	secretEnc, secretNonce, keyVersion, err := keyring.Seal([]byte(secret))
	require.NoError(t, err)
	code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	require.NoError(t, err)

	e := &totpEnrollment{mini: mini, redis: redisClient, code: code, failQuery: new(string), disabledAt: new(bool)}
	db := sql.OpenDB(enrollConnector{conn: enrollConn{
		mfaEventConn: mfaEventConn{state: state},
		neverEnabled: neverEnabled,
		enabledAtSet: new(bool),
		secretEnc:    secretEnc,
		secretNonce:  secretNonce,
		keyVersion:   keyVersion,
		failQuery:    e.failQuery,
		disabledAt:   e.disabledAt,
		confirmed:    new(bool),
	}})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	e.h = NewHandler(db, redisClient, logger.New("test"), keyring, "test", nil, "test")
	return e
}

func (e *totpEnrollment) verify(t *testing.T, sessionID string) {
	t.Helper()
	c, response := enrollRequest("/api/v1/auth/mfa/totp/verify-setup", `{"code":"`+e.code+`"}`, sessionID)
	e.h.TOTPVerifySetup(c)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func (e *totpEnrollment) arm(f confirmFaults) {
	*e.failQuery = f.failQuery
	if f.failExists {
		e.redis.AddHook(failCommandHook{name: "exists"})
	}
}

// verifyCode calls verify-setup with any code and returns the response.
func (e *totpEnrollment) verifyCode(sessionID, code string) *httptest.ResponseRecorder {
	c, response := enrollRequest("/api/v1/auth/mfa/totp/verify-setup", `{"code":"`+code+`"}`, sessionID)
	e.h.TOTPVerifySetup(c)
	return response
}

func (e *totpEnrollment) confirm(sessionID string) *httptest.ResponseRecorder {
	c, response := enrollRequest("/api/v1/auth/mfa/totp/confirm-setup", "", sessionID)
	e.h.TOTPConfirmSetup(c)
	return response
}

func upgradeGranted(mini *miniredis.Miniredis, sessionID string) bool {
	return mini.Exists(auth.MFAUpgradeBypassKey(enrollUser, sessionID))
}

func noUpgradeGranted(t *testing.T, mini *miniredis.Miniredis) {
	t.Helper()
	for _, key := range mini.Keys() {
		require.False(t, strings.HasPrefix(key, "mfa_upgrade_bypass:"), "unexpected upgrade grant %q", key)
	}
}

func TestTOTPEnrollmentExemptsTheEnrollingSession(t *testing.T) {
	mini := enrollTOTPAs(t, true, "session-enrolling", "session-enrolling")

	require.True(t, upgradeGranted(mini, "session-enrolling"),
		"the session that proved the new factor must not be asked for it again at its next refresh")
	ttl := mini.TTL(auth.MFAUpgradeBypassKey(enrollUser, "session-enrolling"))
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, 30*time.Second, "the grant is for the refresh that follows at once, not a standing exemption")
}

func TestTOTPEnrollmentGrantsNothingWhenAnotherSessionConfirms(t *testing.T) {
	// confirm-setup takes no code. A session that never proved the factor must
	// not collect the exemption by confirming a setup another session verified.
	mini := enrollTOTPAs(t, true, "session-enrolling", "session-other")

	noUpgradeGranted(t, mini)
}

func TestTOTPEnrollmentGrantsNothingWhenMFAWasAlreadyOn(t *testing.T) {
	// A session older than an existing mfa_enabled_at must pass the challenge
	// its age earns it, even when it adds a factor of its own.
	mini := enrollTOTPAs(t, false, "session-enrolling", "session-enrolling")

	noUpgradeGranted(t, mini)
}

func TestTOTPEnrollmentGrantsNothingToATokenWithoutASessionID(t *testing.T) {
	mini := enrollTOTPAs(t, true, "", "")

	noUpgradeGranted(t, mini)
}

func registerWebAuthnAs(t *testing.T, neverEnabled bool, sessionID string) *miniredis.Miniredis {
	t.Helper()
	mini, finish := runWebAuthnRegistration(t, &mfaEventDB{}, neverEnabled, sessionID)
	require.Equal(t, http.StatusOK, finish.Code, finish.Body.String())
	return mini
}

// webauthnFaults arms Redis hooks after the ceremony data is seeded, and can
// record whether finish deleted the credential it inserted.
type webauthnFaults struct {
	hooks             []redis.Hook
	credentialRemoved *bool
}

func runWebAuthnRegistration(t *testing.T, state *mfaEventDB, neverEnabled bool, sessionID string, faults ...webauthnFaults) (*miniredis.Miniredis, *httptest.ResponseRecorder) {
	t.Helper()
	mini, redisClient := newEnrollmentRedis(t)
	webAuthnService, err := NewWebAuthnService("webauthn.io", "test", []string{"https://webauthn.io"})
	require.NoError(t, err)
	sessionJSON, err := json.Marshal(webauthn.SessionData{
		Challenge: webAuthnChallenge, RelyingPartyID: "webauthn.io",
		UserID: []byte(enrollUser), CredParams: webauthn.CredentialParametersDefault(),
	})
	require.NoError(t, err)
	require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(redisKeyWebAuthnReg, enrollUser),
		`{"session":`+strconv.Quote(string(sessionJSON))+`,"credential_name":"credential-fixture","credential_type":"device-fixture"}`, time.Minute).Err())
	var credentialRemoved *bool
	for _, f := range faults {
		for _, hook := range f.hooks {
			redisClient.AddHook(hook)
		}
		credentialRemoved = f.credentialRemoved
	}

	db := sql.OpenDB(enrollConnector{conn: enrollConn{
		mfaEventConn:      mfaEventConn{state: state},
		neverEnabled:      neverEnabled,
		enabledAtSet:      new(bool),
		credentialRemoved: credentialRemoved,
	}})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	h := NewHandler(db, redisClient, logger.New("test"), nil, "test", webAuthnService, "test")

	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/register/finish", webAuthnRegistrationResponse, sessionID)
	h.WebAuthnRegisterFinish(c)
	return mini, response
}

func TestWebAuthnEnrollmentExemptsTheEnrollingSession(t *testing.T) {
	// Registration verifies an attestation from the new key in this request,
	// so the proof and the activation share one session.
	mini := registerWebAuthnAs(t, true, "session-enrolling")

	require.True(t, upgradeGranted(mini, "session-enrolling"))
}

func TestWebAuthnEnrollmentGrantsNothingWhenMFAWasAlreadyOn(t *testing.T) {
	mini := registerWebAuthnAs(t, false, "session-enrolling")

	noUpgradeGranted(t, mini)
}

func TestWebAuthnEnrollmentGrantsNothingToATokenWithoutASessionID(t *testing.T) {
	mini := registerWebAuthnAs(t, true, "")

	noUpgradeGranted(t, mini)
}

// An enable that cannot write users.mfa_enabled_at leaves login and the
// pre-MFA session lock unenforced, so it must not report MFA as on (security
// review, PR #3437). Exec order in these flows: TOTP is verify-setup's UPDATE,
// confirm-setup's UPDATE, then the flags UPDATE; WebAuthn is the credential
// INSERT, then the flags UPDATE.
func TestTOTPEnrollmentFailsWhenTheMFAFlagsCannotBeWritten(t *testing.T) {
	mini, confirm := runTOTPEnrollment(t, &mfaEventDB{failExecAt: 3}, true, "session-enrolling", "session-enrolling")

	require.Equal(t, http.StatusInternalServerError, confirm.Code, confirm.Body.String())
	noUpgradeGranted(t, mini)
}

func TestWebAuthnEnrollmentFailsWhenTheMFAFlagsCannotBeWritten(t *testing.T) {
	removed := new(bool)
	mini, finish := runWebAuthnRegistration(t, &mfaEventDB{failExecAt: 2}, true, "session-enrolling", webauthnFaults{credentialRemoved: removed})

	require.Equal(t, http.StatusInternalServerError, finish.Code, finish.Body.String())
	noUpgradeGranted(t, mini)
	// The key is taken back out, so a retry registers it again instead of
	// adding a second credential beside one login does not know MFA is on for
	// (silent-failure review, PR #3437).
	require.True(t, *removed, "a failed activation must not leave the credential registered")
}

// finish reads and deletes the ceremony data in one step, so a failed delete
// cannot leave it replayable until its TTL runs out (security review, PR #3437).
func TestWebAuthnRegisterFinishConsumesTheCeremonyWhenADeleteFails(t *testing.T) {
	mini, finish := runWebAuthnRegistration(t, &mfaEventDB{}, true, "session-enrolling", webauthnFaults{hooks: []redis.Hook{failCommandHook{name: "del"}}})

	require.Equal(t, http.StatusOK, finish.Code, finish.Body.String())
	require.False(t, mini.Exists(fmt.Sprintf(redisKeyWebAuthnReg, enrollUser)), "the ceremony data must not survive a finish")
}

// updateUserMFAFlags reads every method before it writes the flags. A read that
// fails must fail the activation. Counting that method as absent can take the
// disable branch, which clears mfa_enabled_at on an account that already had
// MFA on (security review, PR #3437).
func TestTOTPEnrollmentFailsWhenAnMFAMethodCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		faults confirmFaults
	}{
		{"TOTP row", confirmFaults{failQuery: "enabled AND confirmed"}},
		{"WebAuthn count", confirmFaults{failQuery: "COUNT(*) FROM user_mfa_webauthn"}},
		{"Email/SMS flag", confirmFaults{failExists: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTOTPEnrollment(t, &mfaEventDB{}, false)
			e.verify(t, "session-enrolling")
			e.arm(tc.faults)

			confirm := e.confirm("session-enrolling")

			require.Equal(t, http.StatusInternalServerError, confirm.Code, confirm.Body.String())
			require.False(t, *e.disabledAt, "a failed read must not clear mfa_enabled_at")
			noUpgradeGranted(t, e.mini)
		})
	}
}

// confirm-setup commits confirmed = TRUE before it writes the account flags. A
// retry after that write failed must finish the activation, including the
// grant, instead of answering 409 (code review, PR #3437).
func TestTOTPConfirmRetryFinishesAnActivationWhoseFlagsWriteFailed(t *testing.T) {
	e := newTOTPEnrollment(t, &mfaEventDB{failExecAt: 3}, true)
	e.verify(t, "session-enrolling")
	first := e.confirm("session-enrolling")
	require.Equal(t, http.StatusInternalServerError, first.Code, first.Body.String())

	retry := e.confirm("session-enrolling")

	require.Equal(t, http.StatusOK, retry.Code, retry.Body.String())
	require.True(t, upgradeGranted(e.mini, "session-enrolling"))
	require.Equal(t, http.StatusConflict, e.confirm("session-enrolling").Code, "a completed activation still answers 409")
}

// A re-setup replaces the secret, so a session that verified the OLD secret
// must not match at confirm-setup for the new one (security review, PR #3437).
func TestTOTPSetupDiscardsAnEarlierVerifyingSession(t *testing.T) {
	h, mini, _ := newTOTPReSetup(t)

	response := runTOTPSetup(h)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.False(t, mini.Exists(fmt.Sprintf(redisKeyTOTPSetupSession, enrollUser)), "the record of who verified the replaced secret must not survive")
}

// If TOTPSetup cannot clear that record, it stops. Carrying on would let the
// session that verified the old secret match at confirm-setup for the new one
// (security review, PR #3437).
func TestTOTPSetupFailsWhenTheEarlierVerifyingSessionCannotBeCleared(t *testing.T) {
	h, _, redisClient := newTOTPReSetup(t)
	redisClient.AddHook(failCommandHook{name: "del"})

	response := runTOTPSetup(h)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
}

// newTOTPReSetup is an account whose earlier TOTP setup was verified by
// "session-old", about to start a new setup.
func newTOTPReSetup(t *testing.T) (*Handler, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mini, redisClient := newEnrollmentRedis(t)
	require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(redisKeyTOTPSetupSession, enrollUser), "session-old", time.Minute).Err())

	passwordHash, err := auth.HashPasswordWithParams("credential-fixture", &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	keyring, err := ParseKeyring("0101010101010101010101010101010101010101010101010101010101010101", 1, "")
	require.NoError(t, err)
	db := sql.OpenDB(enrollConnector{conn: enrollConn{
		mfaEventConn: mfaEventConn{state: &mfaEventDB{passwordHash: passwordHash}},
		enabledAtSet: new(bool),
		totpActive:   new(bool),
	}})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return NewHandler(db, redisClient, logger.New("test"), keyring, "test", nil, "test"), mini, redisClient
}

func runTOTPSetup(h *Handler) *httptest.ResponseRecorder {
	c, response := enrollRequest("/api/v1/auth/mfa/totp/setup", `{"password":"credential-fixture"}`, "session-new")
	h.TOTPSetup(c)
	return response
}

// verify-setup's attempt limit must hold while Redis fails. The lockout is read
// before the code is checked, so an unreadable lockout stops the request rather
// than checking codes with no limit (silent-failure review, PR #3437).
func TestTOTPVerifySetupFailsClosedWhenTheLockoutCannotBeRead(t *testing.T) {
	e := newTOTPEnrollment(t, &mfaEventDB{}, true)
	e.redis.AddHook(failCommandHook{name: "exists"})

	response := e.verifyCode("session-enrolling", e.code)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.False(t, e.mini.Exists(fmt.Sprintf(redisKeyTOTPSetupSession, enrollUser)), "a request stopped by the limit must record no proving session")
}

// runEmailVerification checks a pending email code as sessionID. The code is
// checked in the same request that activates the method, as with WebAuthn, so
// the proof and the activation share one session (red-team, PR #3437).
func runEmailVerification(t *testing.T, state *mfaEventDB, neverEnabled bool, sessionID string) (*miniredis.Miniredis, *httptest.ResponseRecorder) {
	t.Helper()
	mini, redisClient := newEnrollmentRedis(t)
	require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(redisKeyEmailSmsSetup, enrollUser, "email"), "123456", time.Minute).Err())
	db := sql.OpenDB(enrollConnector{conn: enrollConn{
		mfaEventConn: mfaEventConn{state: state},
		neverEnabled: neverEnabled,
		enabledAtSet: new(bool),
	}})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	h := NewHandler(db, redisClient, logger.New("test"), nil, "test", nil, "test")

	c, response := enrollRequest("/api/v1/auth/mfa/email-sms/verify", `{"codes":{"email":"123456"}}`, sessionID)
	h.EmailSmsVerify(c)
	return mini, response
}

func TestEmailEnrollmentExemptsTheEnrollingSession(t *testing.T) {
	mini, verify := runEmailVerification(t, &mfaEventDB{}, true, "session-enrolling")

	require.Equal(t, http.StatusOK, verify.Code, verify.Body.String())
	require.True(t, upgradeGranted(mini, "session-enrolling"))
}

func TestEmailEnrollmentGrantsNothingWhenMFAWasAlreadyOn(t *testing.T) {
	mini, verify := runEmailVerification(t, &mfaEventDB{}, false, "session-enrolling")

	require.Equal(t, http.StatusOK, verify.Code, verify.Body.String())
	noUpgradeGranted(t, mini)
}

func TestEmailEnrollmentGrantsNothingToATokenWithoutASessionID(t *testing.T) {
	mini, verify := runEmailVerification(t, &mfaEventDB{}, true, "")

	require.Equal(t, http.StatusOK, verify.Code, verify.Body.String())
	noUpgradeGranted(t, mini)
}

func TestEmailEnrollmentGrantsNothingWhenTheMFAFlagsCannotBeWritten(t *testing.T) {
	mini, verify := runEmailVerification(t, &mfaEventDB{failExecAt: 1}, true, "session-enrolling")

	require.Equal(t, http.StatusInternalServerError, verify.Code, verify.Body.String())
	noUpgradeGranted(t, mini)
}
