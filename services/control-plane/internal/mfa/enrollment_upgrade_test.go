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

// enrollConn answers the reads the shared fake does not: whether MFA was ever
// on for the account, verify-setup's four-column TOTP row, and the inline
// factors the B1 flag sync reads inside its transaction. It models the flag
// write stamping or clearing mfa_enabled_at, so a handler that reads the flag
// after that write, rather than before, sees MFA as already on. It also models
// rollback: B1 commits a factor and its flags together, so a write the
// transaction undoes must not be counted.
type enrollConn struct {
	mfaEventConn
	neverEnabled bool
	enabledAtSet *bool
	secretEnc    []byte
	secretNonce  []byte
	keyVersion   int
	// failQuery, when set, fails any read whose SQL contains it. disabledAt
	// records a flag write that clears mfa_enabled_at. confirmed, when set,
	// models user_mfa_totp.confirmed across requests.
	failQuery  *string
	disabledAt *bool
	confirmed  *bool
	// degradedWrite records a flag write that carried the row's email/SMS
	// entries forward because their state could not be read.
	degradedWrite *bool
	// credentialStored records a WebAuthn credential INSERT that was not
	// rolled back.
	credentialStored *bool
	// tx makes a write inside a transaction undoable on rollback.
	tx *enrollTxLog
}

// enrollTxLog is one transaction's undo log. A write applies at once, so later
// statements in the same transaction see it, and is undone on rollback.
type enrollTxLog struct {
	open bool
	undo []func()
}

type enrollTx struct{ log *enrollTxLog }

func (t enrollTx) Commit() error {
	t.log.open, t.log.undo = false, nil
	return nil
}

func (t enrollTx) Rollback() error {
	for i := len(t.log.undo) - 1; i >= 0; i-- {
		t.log.undo[i]()
	}
	t.log.open, t.log.undo = false, nil
	return nil
}

func (c enrollConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.tx == nil {
		return c.mfaEventConn.BeginTx(ctx, opts)
	}
	c.tx.open, c.tx.undo = true, nil
	return enrollTx{log: c.tx}, nil
}

// set records a successful write's effect, undoably while a transaction is open.
func (c enrollConn) set(p *bool) {
	if p == nil {
		return
	}
	prev := *p
	*p = true
	if c.tx != nil && c.tx.open {
		c.tx.undo = append(c.tx.undo, func() { *p = prev })
	}
}

func isSet(p *bool) bool { return p != nil && *p }

// ExecContext records a write only when it succeeded.
func (c enrollConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.mfaEventConn.ExecContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	switch {
	// The B1 flag write (mfaFlagsExactSQL / mfaFlagsDegradedSQL): a non-empty
	// method list stamps mfa_enabled_at, an empty one clears it.
	case strings.Contains(query, "COALESCE(mfa_enabled_at, NOW())"):
		if strings.Contains(query, "m IN ('email', 'sms')") {
			c.set(c.degradedWrite)
		}
		if len(args) > 0 && fmt.Sprint(args[0].Value) != "{}" {
			c.set(c.enabledAtSet)
		} else {
			c.set(c.disabledAt)
		}
	case strings.Contains(query, "SET confirmed = TRUE"):
		c.set(c.confirmed)
	case strings.Contains(query, "INSERT INTO user_mfa_webauthn"):
		c.set(c.credentialStored)
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
	// stepup.InlineMFAMethods, read by the B1 flag sync inside its transaction:
	// TOTP counts once confirm-setup has confirmed it, WebAuthn once its
	// credential is stored. stepup.LoadSubject's read carries the same TOTP
	// predicate and is left to the shared fake.
	case strings.Contains(query, "AND enabled AND confirmed") && !strings.Contains(query, "FROM users u"):
		return &mfaEventRows{values: []driver.Value{isSet(c.confirmed), isSet(c.credentialStored)}}, nil
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

// confirmFaults fail the flag sync's reads during confirm-setup only, after
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
	h             *Handler
	mini          *miniredis.Miniredis
	redis         *redis.Client
	code          string
	failQuery     *string
	disabledAt    *bool
	degradedWrite *bool
	confirmed     *bool
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

	e := &totpEnrollment{mini: mini, redis: redisClient, code: code, failQuery: new(string), disabledAt: new(bool), degradedWrite: new(bool), confirmed: new(bool)}
	db := sql.OpenDB(enrollConnector{conn: enrollConn{
		mfaEventConn:  mfaEventConn{state: state},
		neverEnabled:  neverEnabled,
		enabledAtSet:  new(bool),
		secretEnc:     secretEnc,
		secretNonce:   secretNonce,
		keyVersion:    keyVersion,
		failQuery:     e.failQuery,
		disabledAt:    e.disabledAt,
		confirmed:     e.confirmed,
		degradedWrite: e.degradedWrite,
		tx:            new(enrollTxLog),
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
// observe whether the credential finish inserted survived its transaction.
type webauthnFaults struct {
	hooks            []redis.Hook
	credentialStored *bool
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
	// Always tracked: the flag sync's inline read lists WebAuthn only once the
	// credential is stored.
	credentialStored := new(bool)
	for _, f := range faults {
		for _, hook := range f.hooks {
			redisClient.AddHook(hook)
		}
		if f.credentialStored != nil {
			credentialStored = f.credentialStored
		}
	}

	db := sql.OpenDB(enrollConnector{conn: enrollConn{
		mfaEventConn:     mfaEventConn{state: state},
		neverEnabled:     neverEnabled,
		enabledAtSet:     new(bool),
		credentialStored: credentialStored,
		tx:               new(enrollTxLog),
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
	stored := new(bool)
	mini, finish := runWebAuthnRegistration(t, &mfaEventDB{failExecAt: 2}, true, "session-enrolling", webauthnFaults{credentialStored: stored})

	require.Equal(t, http.StatusInternalServerError, finish.Code, finish.Body.String())
	noUpgradeGranted(t, mini)
	// The credential and the flags commit together (B1), so the failed flag
	// write rolls the key back out and a retry registers it again, instead of
	// adding a second credential beside one login does not know MFA is on for
	// (silent-failure review, PR #3437).
	require.False(t, *stored, "a failed activation must not leave the credential registered")
}

// Control for the case above: the same flow with a working flag write keeps
// the credential, so the case above is observing the rollback, not a fixture
// that never stores anything.
func TestWebAuthnEnrollmentKeepsTheCredentialWhenTheFlagsAreWritten(t *testing.T) {
	stored := new(bool)
	_, finish := runWebAuthnRegistration(t, &mfaEventDB{}, true, "session-enrolling", webauthnFaults{credentialStored: stored})

	require.Equal(t, http.StatusOK, finish.Code, finish.Body.String())
	require.True(t, *stored)
}

// finish reads and deletes the ceremony data in one step, so a failed delete
// cannot leave it replayable until its TTL runs out (security review, PR #3437).
func TestWebAuthnRegisterFinishConsumesTheCeremonyWhenADeleteFails(t *testing.T) {
	mini, finish := runWebAuthnRegistration(t, &mfaEventDB{}, true, "session-enrolling", webauthnFaults{hooks: []redis.Hook{failCommandHook{name: "del"}}})

	require.Equal(t, http.StatusOK, finish.Code, finish.Body.String())
	require.False(t, mini.Exists(fmt.Sprintf(redisKeyWebAuthnReg, enrollUser)), "the ceremony data must not survive a finish")
}

// A factor read that fails must fail the activation: counting a factor it could
// not read as absent can take the disable branch, which clears mfa_enabled_at
// on an account that already had MFA on (security review, PR #3437). The B1
// flag sync reads both inline factors in ONE statement inside the transaction
// that confirms TOTP, so the failure rolls the confirm back with it.
func TestTOTPEnrollmentFailsWhenTheInlineFactorsCannotBeRead(t *testing.T) {
	e := newTOTPEnrollment(t, &mfaEventDB{}, false)
	e.verify(t, "session-enrolling")
	e.arm(confirmFaults{failQuery: "AND enabled AND confirmed"})

	confirm := e.confirm("session-enrolling")

	require.Equal(t, http.StatusInternalServerError, confirm.Code, confirm.Body.String())
	require.False(t, *e.disabledAt, "a failed read must not clear mfa_enabled_at")
	noUpgradeGranted(t, e.mini)
}

// An unreadable email/SMS state does NOT fail the activation. PR #3437 failed
// it; the B1 flag sync instead writes the TOTP half exactly and carries the
// row's own email/SMS entries forward (mfaFlagsDegradedSQL). That never drops
// a factor that is on and never clears mfa_enabled_at, which is the harm #3437
// guarded against, and it does not turn a Redis blip into a failed enable.
func TestTOTPEnrollmentCarriesEmailSmsForwardWhenTheirStateCannotBeRead(t *testing.T) {
	e := newTOTPEnrollment(t, &mfaEventDB{}, true)
	e.verify(t, "session-enrolling")
	e.arm(confirmFaults{failExists: true})

	confirm := e.confirm("session-enrolling")

	require.Equal(t, http.StatusOK, confirm.Code, confirm.Body.String())
	require.True(t, *e.degradedWrite, "an unreadable email/SMS state must take the carry-forward write")
	require.False(t, *e.disabledAt, "a failed read must not clear mfa_enabled_at")
	require.True(t, upgradeGranted(e.mini, "session-enrolling"))
}

// A confirm whose flag write fails is rolled back with it (B1), so the retry is
// an ordinary first confirm. It must finish the activation, including the
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

// Before B1 a confirm could commit confirmed = TRUE and then lose its flag
// write, leaving a row confirmed while the flags lack TOTP. confirm-setup heals
// it: the flags are written, it answers 200 and grants as a first confirm
// would, and only a completed activation answers 409.
func TestTOTPConfirmHealsARowConfirmedWithoutItsFlags(t *testing.T) {
	e := newTOTPEnrollment(t, &mfaEventDB{}, true)
	e.verify(t, "session-enrolling")
	*e.confirmed = true // the pre-B1 confirm whose flag write was lost

	heal := e.confirm("session-enrolling")

	require.Equal(t, http.StatusOK, heal.Code, heal.Body.String())
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
		tx:           new(enrollTxLog),
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
		tx:           new(enrollTxLog),
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
