package mfa

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/email"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// These cases pin how the MFA handlers answer a Redis or database read they
// cannot make. A guard that reads past a failure fails open; a status read that
// defaults reports the account as less protected than it is (companion PR to
// #3437).

var errReadFailed = errors.New("read failed")

// failKeyHook fails every Redis command with this lower-case name whose first
// key starts with prefix, and nothing else.
type failKeyHook struct{ name, prefix string }

func (failKeyHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (failKeyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (f failKeyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if args := cmd.Args(); cmd.Name() == f.name && len(args) > 1 {
			if key, ok := args[1].(string); ok && strings.HasPrefix(key, f.prefix) {
				err := errors.New("redis command failed")
				cmd.SetErr(err)
				return err
			}
		}
		return next(ctx, cmd)
	}
}

// scriptedAnswer answers any query containing match with one row, no rows, an
// error, or (rowsErr) a non-nil iteration error surfaced through rows.Err()
// rather than a single malformed row. The first matching answer wins;
// unmatched queries fall through to the shared fake.
type scriptedAnswer struct {
	match   string
	values  []driver.Value
	noRows  bool
	err     error
	rowsErr error
}

// scriptedExecAnswer answers any Exec whose query contains match, either with
// an error from the Exec itself or with a Result whose RowsAffected() fails —
// the shape a driver failure reading a write's affected-row count takes,
// distinct from the write itself failing.
type scriptedExecAnswer struct {
	match           string
	err             error
	rowsAffected    int64
	rowsAffectedErr error
}

type execResult struct {
	rows int64
	err  error
}

func (execResult) LastInsertId() (int64, error)   { return 0, errors.New("not supported") }
func (r execResult) RowsAffected() (int64, error) { return r.rows, r.err }

// erroringRows answers a query with zero rows and a non-nil iteration error,
// so rows.Err() (not a per-row Scan) surfaces it.
type erroringRows struct{ err error }

func (erroringRows) Columns() []string           { return []string{"col"} }
func (erroringRows) Close() error                { return nil }
func (r erroringRows) Next([]driver.Value) error { return r.err }

type scriptedConn struct {
	mfaEventConn
	answers     []scriptedAnswer
	execAnswers []scriptedExecAnswer
}

func (c scriptedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	for _, a := range c.answers {
		if !strings.Contains(query, a.match) {
			continue
		}
		c.state.record(c.id, query)
		if a.err != nil {
			return nil, a.err
		}
		return a.rows(), nil
	}
	return c.mfaEventConn.QueryContext(ctx, query, args)
}

// rows is the result set a successful query answers with. A rowsErr is not a
// query failure: the query succeeds and iteration fails, which is what surfaces
// through rows.Err().
func (a scriptedAnswer) rows() driver.Rows {
	switch {
	case a.rowsErr != nil:
		return erroringRows{err: a.rowsErr}
	case a.noRows:
		return mfaEventNoRows{}
	default:
		return &mfaEventRows{values: a.values}
	}
}

func (c scriptedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	for _, a := range c.execAnswers {
		if !strings.Contains(query, a.match) {
			continue
		}
		c.state.record(c.id, query)
		if a.err != nil {
			return nil, a.err
		}
		return execResult{rows: a.rowsAffected, err: a.rowsAffectedErr}, nil
	}
	return c.mfaEventConn.ExecContext(ctx, query, args)
}

type scriptedConnector struct{ conn scriptedConn }

func (c scriptedConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (scriptedConnector) Driver() driver.Driver                          { return mfaEventDriver{} }

func scriptedHandler(t *testing.T, state *mfaEventDB, environment string, answers ...scriptedAnswer) (*Handler, *redis.Client) {
	t.Helper()
	_, redisClient := newEnrollmentRedis(t)
	db := sql.OpenDB(scriptedConnector{conn: scriptedConn{mfaEventConn: mfaEventConn{state: state, id: 1}, answers: answers}})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return NewHandler(db, redisClient, logger.New("test"), nil, "test", nil, environment), redisClient
}

// scriptedHandlerFull is scriptedHandler with a real keyring (for cases that
// must decrypt a genuine TOTP secret) and Exec fault injection.
func scriptedHandlerFull(t *testing.T, state *mfaEventDB, environment string, keyring *Keyring, queryAnswers []scriptedAnswer, execAnswers []scriptedExecAnswer) (*Handler, *redis.Client) {
	t.Helper()
	_, redisClient := newEnrollmentRedis(t)
	db := sql.OpenDB(scriptedConnector{conn: scriptedConn{mfaEventConn: mfaEventConn{state: state, id: 1}, answers: queryAnswers, execAnswers: execAnswers}})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return NewHandler(db, redisClient, logger.New("test"), keyring, "test", nil, environment), redisClient
}

// testKeyring is a real keyring for tests that must exercise genuine
// encrypt/decrypt (TOTP secret seal/open), not just fault injection.
func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	keyring, err := ParseKeyring("0101010101010101010101010101010101010101010101010101010101010101", 1, "")
	require.NoError(t, err)
	return keyring
}

// realTOTPFixture seals a real TOTP secret and generates a currently-valid
// code for it, for tests that must pass ValidateCode / keyring.Open for real.
type realTOTPFixture struct {
	keyring     *Keyring
	secretEnc   []byte
	secretNonce []byte
	keyVersion  int
	code        string
}

func newRealTOTPFixture(t *testing.T) realTOTPFixture {
	t.Helper()
	keyring := testKeyring(t)
	secret := "JBSWY3DPEHPK3PXP" //nolint:gosec // test-only TOTP seed // pragma: allowlist secret
	secretEnc, secretNonce, keyVersion, err := keyring.Seal([]byte(secret))
	require.NoError(t, err)
	code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	require.NoError(t, err)
	return realTOTPFixture{keyring: keyring, secretEnc: secretEnc, secretNonce: secretNonce, keyVersion: keyVersion, code: code}
}

func correctPasswordHash(t *testing.T) string {
	t.Helper()
	hash, err := auth.HashPasswordWithParams("correct", &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	return hash
}

func loginChallenge(t *testing.T, h *Handler) (token, jti string) {
	t.Helper()
	token, jti, err := h.GenerateLoginChallenge(context.Background(), enrollUser, false, "", securityevent.AuthPassword)
	require.NoError(t, err)
	return token, jti
}

func verifyWith(h *Handler, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Verify(c)
	return response
}

// ── Verify ──────────────────────────────────────────────────────────────────

func TestVerifyFailsClosedWhenItCannotReadTheReuseOrLockoutGuard(t *testing.T) {
	for _, guard := range []struct{ name, prefix string }{
		{name: "reuse", prefix: "mfa_challenge_used:"},
		{name: "lockout", prefix: "mfa_verify_lockout:"},
	} {
		t.Run(guard.name, func(t *testing.T) {
			// The account holds a TOTP factor and the code is right, so the
			// guard is the only thing between this request and a session.
			fixture := newRealTOTPFixture(t)
			h, redisClient := scriptedHandlerFull(t, fixture.state(), "test", fixture.keyring, nil, nil)
			completer := &recordingLoginCompleter{}
			h.SetLoginCompleter(completer)
			token, _ := loginChallenge(t, h)
			redisClient.AddHook(failKeyHook{name: "exists", prefix: guard.prefix})

			response := verifyWith(h, `{"mfa_challenge_token":"`+token+`","method":"totp","code":"`+fixture.code+`"}`)

			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			require.False(t, completer.called, "an unreadable guard must never let a login through")
		})
	}
}

const (
	// inlineFactor is a WebAuthn inline token a step-up test stores for one
	// purpose (storeInlineFactor). It verifies without the database.
	inlineFactor = "webauthn-inline-token-123456"
	// wrongFactor is wrong for an account with no TOTP secret, which is every
	// fixture that uses it.
	wrongFactor = "000000"
)

// storeInlineFactor stores inlineFactor as a token minted for purpose.
func storeInlineFactor(t *testing.T, redisClient *redis.Client, purpose stepup.Purpose) {
	t.Helper()
	require.NoError(t, redisClient.Set(context.Background(), inlineTokenKey(enrollUser, purpose, inlineFactor), "1", time.Minute).Err())
}

// state is an account whose only factor is this fixture's confirmed TOTP.
func (f realTOTPFixture) state() *mfaEventDB {
	return &mfaEventDB{totpSecretEnc: f.secretEnc, totpSecretNonce: f.secretNonce, totpKeyVersion: f.keyVersion, totpEnabled: true, totpConfirmed: true}
}

// wrongCode is a six-digit code outside the window ValidateCode accepts
// around now, so it is wrong for this fixture however the clock falls.
func (f realTOTPFixture) wrongCode(t *testing.T) string {
	t.Helper()
	accepted := map[string]bool{}
	for _, offset := range []time.Duration{-60 * time.Second, -30 * time.Second, 0, 30 * time.Second, 60 * time.Second} {
		code, err := totp.GenerateCodeCustom("JBSWY3DPEHPK3PXP", time.Now().Add(offset), totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		require.NoError(t, err)
		accepted[code] = true
	}
	for i := 0; ; i++ {
		if code := fmt.Sprintf("%06d", i); !accepted[code] {
			return code
		}
	}
}

// verifyFixture is a login challenge whose only factor is a confirmed TOTP.
// Judging a code reads the TOTP secret, so a request that recorded no such read
// judged no code. Login never accepts a WebAuthn inline token (#3453 RS11), so
// this fixture no longer uses one.
type verifyFixture struct {
	h         *Handler
	redis     *redis.Client
	state     *mfaEventDB
	completer *recordingLoginCompleter
	events    *securityEventRecorder
	token     string
	jti       string
	right     string
	wrong     string
}

func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()
	fixture := newRealTOTPFixture(t)
	state := fixture.state()
	// The scripted connector hands out one connection and counts nothing, so
	// concurrent requests do not race on the fake's connection counter.
	h, redisClient := scriptedHandlerFull(t, state, "test", fixture.keyring, nil, nil)
	f := &verifyFixture{h: h, redis: redisClient, state: state, completer: &recordingLoginCompleter{}, events: &securityEventRecorder{}, right: fixture.code, wrong: fixture.wrongCode(t)}
	h.SetLoginCompleter(f.completer)
	h.SetSecurityEvents(f.events)
	f.token, f.jti = loginChallenge(t, h)
	return f
}

func (f *verifyFixture) verify(code string) *httptest.ResponseRecorder {
	return verifyWith(f.h, `{"mfa_challenge_token":"`+f.token+`","method":"totp","code":"`+code+`"}`)
}

func (f *verifyFixture) exists(t *testing.T, key string) bool {
	t.Helper()
	n, err := f.redis.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	return n == 1
}

// judged counts the requests that reached the code check.
func (f *verifyFixture) judged() int {
	return judgedIn(f.state)
}

// judgedIn counts the TOTP secret reads state recorded.
func judgedIn(state *mfaEventDB) int {
	state.mu.Lock()
	defer state.mu.Unlock()
	n := 0
	for _, statement := range state.statements {
		if strings.Contains(statement, "totp_secret_enc") {
			n++
		}
	}
	return n
}

// An attempt that cannot be counted is refused before any code is judged. A
// right code and a wrong one get the same 500, so an outage tells a guesser
// nothing, and the right factor is not spent.
func TestVerifyJudgesNoCodeItCannotCount(t *testing.T) {
	// The count is INCR followed by EXPIRE NX; either failing leaves it unset.
	for _, command := range []string{"incr", "expire"} {
		for _, which := range []string{"right", "wrong"} {
			t.Run(command+"/"+which, func(t *testing.T) {
				f := newVerifyFixture(t)
				f.redis.AddHook(failCommandHook{name: command})
				code := f.right
				if which == "wrong" {
					code = f.wrong
				}

				response := f.verify(code)

				require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
				require.Contains(t, response.Body.String(), errMsgMFAVerificationUnavailable)
				require.False(t, f.completer.called)
				require.Zero(t, f.judged(), "an uncounted request must not have its code judged")
				require.False(t, f.exists(t, "auth_failures:ip:192.0.2.1"), "a code that was never judged is not a failed attempt")
			})
		}
	}
}

// The window starts at the first attempt and nothing extends it: a refused
// attempt renewing the window would let one request every few minutes hold an
// account's MFA locked for as long as the requests keep coming.
func TestVerifyAttemptWindowIsFixedFromTheFirstAttempt(t *testing.T) {
	mini, redisClient := newEnrollmentRedis(t)
	h := NewHandler(fakeMFADB(t, nil), redisClient, logger.New("test"), nil, "test", nil, "test")
	token, _ := loginChallenge(t, h)
	key := "mfa_verify_attempts:" + enrollUser

	require.Equal(t, http.StatusForbidden, verifyWith(h, `{"mfa_challenge_token":"`+token+`","method":"totp","code":"`+wrongFactor+`"}`).Code)
	first := mini.TTL(key)
	require.Positive(t, first, "the first attempt starts the window")

	mini.FastForward(2 * time.Minute)
	require.NoError(t, redisClient.Set(context.Background(), key, failedAttemptLimit, redis.KeepTTL).Err())
	refused := verifyWith(h, `{"mfa_challenge_token":"`+token+`","method":"totp","code":"`+wrongFactor+`"}`)
	require.Equal(t, http.StatusTooManyRequests, refused.Code, refused.Body.String())

	require.Equal(t, first-2*time.Minute, mini.TTL(key), "a refused attempt must not extend the window")
}

// A count left without an expiry (an INCR that landed without its EXPIRE, in
// the design this replaced) gets one on the next attempt instead of locking
// the account for good.
func TestVerifyGivesACountWithoutAnExpiryOne(t *testing.T) {
	mini, redisClient := newEnrollmentRedis(t)
	h := NewHandler(fakeMFADB(t, nil), redisClient, logger.New("test"), nil, "test", nil, "test")
	token, _ := loginChallenge(t, h)
	key := "mfa_verify_attempts:" + enrollUser
	require.NoError(t, redisClient.Set(context.Background(), key, failedAttemptLimit, 0).Err())

	verifyWith(h, `{"mfa_challenge_token":"`+token+`","method":"totp","code":"`+wrongFactor+`"}`)

	require.Equal(t, verifyAttemptWindow, mini.TTL(key))
}

func TestVerifyCountsAWrongCode(t *testing.T) {
	f := newVerifyFixture(t)

	response := f.verify(f.wrong)

	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	attempts, err := f.redis.Get(context.Background(), "mfa_verify_attempts:"+enrollUser).Int()
	require.NoError(t, err)
	require.Equal(t, 1, attempts)
	require.True(t, f.exists(t, "auth_failures:ip:192.0.2.1"), "the per-IP failure counter must see a wrong code")
}

// When the lockout write after the last allowed wrong code fails, that attempt
// fails closed, and the count still refuses the next attempt, even one with
// the right code.
func TestVerifyRefusesPastTheLimitWhenTheLockoutWriteFails(t *testing.T) {
	f := newVerifyFixture(t)
	require.NoError(t, f.redis.Set(context.Background(), "mfa_verify_attempts:"+enrollUser, failedAttemptLimit-1, time.Minute).Err())
	f.redis.AddHook(failKeyHook{name: "set", prefix: "mfa_verify_lockout:"})

	last := f.verify(f.wrong)
	require.Equal(t, http.StatusInternalServerError, last.Code, last.Body.String())
	require.Contains(t, last.Body.String(), errMsgMFAVerificationUnavailable)
	require.False(t, f.exists(t, "mfa_verify_lockout:"+enrollUser))
	require.True(t, f.exists(t, "auth_failures:ip:192.0.2.1"), "the wrong code still counts against the IP")

	judgedBefore := f.judged()
	next := f.verify(f.right)

	require.Equal(t, http.StatusTooManyRequests, next.Code, next.Body.String())
	require.False(t, f.completer.called, "a right code must not sign in past the limit")
	require.Equal(t, judgedBefore, f.judged(), "a refused attempt must not have its code judged")
}

// Guesses sent at once must not each get a code checked. Before the count was
// taken ahead of the check, every request that read a count below the limit
// had its code judged.
func TestVerifyJudgesAtMostTheLimitOfConcurrentGuesses(t *testing.T) {
	f := newVerifyFixture(t)
	// Each judged code holds its TOTP read briefly, so every concurrent request
	// is in flight at once; the fake records one read per judged code.
	f.state.totpReadDelay = 20 * time.Millisecond

	const guesses = 4 * failedAttemptLimit
	codes := make([]int, guesses)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range guesses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = f.verify(f.wrong).Code
		}()
	}
	close(start)
	wg.Wait()

	require.Equal(t, failedAttemptLimit, f.judged(), "only the first %d attempts may have a code judged", failedAttemptLimit)
	statuses := map[int]int{}
	for _, code := range codes {
		statuses[code]++
	}
	require.Equal(t, map[int]int{http.StatusForbidden: failedAttemptLimit, http.StatusTooManyRequests: guesses - failedAttemptLimit}, statuses)
}

// An account with MFA on must never be offered no method at all: the login
// paths read an empty list as "no MFA" and sign in on the password alone. A
// disable that removes the last method outside the recovery-only set reaches
// that state, so the restriction lapses there (security review, #3460).
func TestGetLoginMethodsNeverLeavesAnMFAAccountWithNoMethod(t *testing.T) {
	for _, tc := range []struct {
		name, methods, recoveryOnly string
		want                        []string
	}{
		{name: "restriction applies", methods: "{totp,webauthn}", recoveryOnly: "{webauthn}", want: []string{"totp"}},
		{name: "restriction would leave nothing", methods: "{webauthn}", recoveryOnly: "{webauthn}", want: []string{"webauthn"}},
		{name: "every method restricted", methods: "{webauthn,email}", recoveryOnly: "{email,webauthn}", want: []string{"webauthn", "email"}},
		{name: "no restriction", methods: "{totp}", recoveryOnly: "{}", want: []string{"totp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(fakeMFADB(t, &mfaEventDB{mfaMethods: tc.methods, recoveryOnly: tc.recoveryOnly}), nil, logger.New("test"), nil, "test", nil, "test")

			methods, err := h.GetLoginMethods(context.Background(), enrollUser)

			require.NoError(t, err)
			require.Equal(t, tc.want, methods)
		})
	}
}

// A method marked recovery-only unlocks recovery, not sign-in. The login
// response already leaves it out; the server must refuse it too, and refuse it
// exactly as a wrong code is refused (security review, #3460).
func TestVerifyRefusesARecoveryOnlyMethodAtSignIn(t *testing.T) {
	for _, tc := range []struct {
		name         string
		methods      string
		recoveryOnly string
		want         int
	}{
		{name: "restricted", recoveryOnly: "{totp}", want: http.StatusForbidden},
		{name: "another method restricted", recoveryOnly: "{email}", want: http.StatusOK},
		// With no other method left the restriction lapses: refusing here
		// would leave a challenge nothing can answer (see
		// TestGetLoginMethodsNeverLeavesAnMFAAccountWithNoMethod).
		{name: "restriction would leave nothing", methods: "{totp}", recoveryOnly: "{totp}", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVerifyFixture(t)
			f.state.mfaMethods = tc.methods
			f.state.recoveryOnly = tc.recoveryOnly

			response := f.verify(f.right)

			require.Equal(t, tc.want, response.Code, response.Body.String())
			if tc.want == http.StatusForbidden {
				require.Contains(t, response.Body.String(), "Invalid MFA code", "the refusal must not confirm the code was right")
				require.False(t, f.completer.called)
			}
		})
	}
}

func TestVerifyFailsClosedWhenItCannotReadTheRecoveryOnlyMethods(t *testing.T) {
	fixture := newRealTOTPFixture(t)
	state := fixture.state()
	h, _ := scriptedHandlerFull(t, state, "test", fixture.keyring,
		[]scriptedAnswer{{match: "recovery_only_methods FROM users", err: errReadFailed}}, nil)
	token, _ := loginChallenge(t, h)

	response := verifyWith(h, `{"mfa_challenge_token":"`+token+`","method":"totp","code":"`+fixture.code+`"}`)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgMFAVerificationUnavailable)
	require.Zero(t, judgedIn(state), "the code must not be judged, and so not spent, before the setting is known")
}

// Judging a code can spend it, so the remember-me state is read first: a read
// failure leaves both the challenge and the factor usable for a retry.
func TestVerifyReadsRememberStateBeforeJudgingTheCode(t *testing.T) {
	f := newVerifyFixture(t)
	f.redis.AddHook(failKeyHook{name: "get", prefix: "mfa_challenge:"})

	response := f.verify(f.right)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgMFAVerificationUnavailable)
	require.False(t, f.completer.called)
	require.Zero(t, f.judged(), "the code must not be judged by a read that failed before judging")
	require.False(t, f.exists(t, "mfa_challenge_used:"+f.jti), "the challenge must stay usable")
	require.False(t, f.exists(t, "mfa_verify_attempts:"+enrollUser), "nothing was attempted")
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded,
		Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable,
		RouteTemplate: securityevent.RouteAuthMFAVerify,
	}}, f.events.events)
}

// A count already at the limit refuses the next attempt even with no lockout
// key, so a lockout write that failed earlier cannot reopen guessing
// (security review, companion to #3437).
func TestVerifyRefusesACountAtTheLimitWhoseLockoutWasNeverWritten(t *testing.T) {
	f := newVerifyFixture(t)
	require.NoError(t, f.redis.Set(context.Background(), "mfa_verify_attempts:"+enrollUser, failedAttemptLimit, time.Minute).Err())

	response := f.verify(f.right)

	require.Equal(t, http.StatusTooManyRequests, response.Code, response.Body.String())
	require.False(t, f.completer.called, "a right code must not sign in past the limit")
	require.Zero(t, f.judged(), "a refused attempt must not have its code judged")
}

func TestWebAuthnLoginReportsAnUnreadableCeremonyAsAnOutage(t *testing.T) {
	_, redisClient := newEnrollmentRedis(t)
	h := NewHandler(fakeMFADB(t, nil), redisClient, logger.New("test"), nil, "test", nil, "test")
	token, jti := loginChallenge(t, h)
	require.NoError(t, redisClient.Set(context.Background(), "mfa_webauthn_session:"+jti, "{}", time.Minute).Err())
	redisClient.AddHook(failKeyHook{name: "getdel", prefix: "mfa_webauthn_session:"})

	response := verifyWith(h, `{"mfa_challenge_token":"`+token+`","method":"webauthn","assertion":{}}`)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgMFAVerificationUnavailable)
}

func TestEmailLoginCodeReportsAnUnreadableCodeAsAnOutage(t *testing.T) {
	_, redisClient := newEnrollmentRedis(t)
	h := NewHandler(fakeMFADB(t, nil), redisClient, logger.New("test"), nil, "test", nil, "test")
	token, jti := loginChallenge(t, h)
	require.NoError(t, redisClient.Set(context.Background(), "mfa_email_login:"+jti, "123456", time.Minute).Err())
	redisClient.AddHook(failKeyHook{name: "get", prefix: "mfa_email_login:"})

	response := verifyWith(h, `{"mfa_challenge_token":"`+token+`","method":"email","code":"123456"}`)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgMFAVerificationUnavailable)
}

// ── Email code delivery ─────────────────────────────────────────────────────

type emailSend struct {
	h     *Handler
	redis *redis.Client
	token string
	jti   string
}

func newEmailSend(t *testing.T, environment string, extra ...scriptedAnswer) *emailSend {
	t.Helper()
	answers := append(extra, scriptedAnswer{match: "SELECT email FROM users", values: []driver.Value{"email-fixture@example.test"}})
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, environment, answers...)
	token, jti := loginChallenge(t, h)
	require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(redisKeyEmailSmsEnabledEmail, enrollUser), "1", 0).Err())
	return &emailSend{h: h, redis: redisClient, token: token, jti: jti}
}

func (e *emailSend) send() *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/email/send", strings.NewReader(`{"mfa_challenge_token":"`+e.token+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	e.h.SendEmailMFACode(c)
	return response
}

func (e *emailSend) exists(t *testing.T, key string) bool {
	t.Helper()
	n, err := e.redis.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	return n > 0
}

func TestSendEmailMFACodeSendsOnceAndStoresTheCode(t *testing.T) {
	e := newEmailSend(t, "test")

	require.Equal(t, http.StatusOK, e.send().Code)
	require.True(t, e.exists(t, "mfa_email_login:"+e.jti))
	require.Equal(t, http.StatusTooManyRequests, e.send().Code, "one code per challenge")
}

func TestSendEmailMFACodeFailsWhenItCannotReadWhetherEmailMFAIsOn(t *testing.T) {
	e := newEmailSend(t, "test")
	e.redis.AddHook(failKeyHook{name: "exists", prefix: "mfa_emailsms_enabled:"})

	response := e.send()

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
}

// An unreadable send limit must not become an unlimited one. The limit is
// claimed with SETNX, which go-redis sends as SET ... NX.
func TestSendEmailMFACodeSendsNothingWhenTheSendLimitIsUnreachable(t *testing.T) {
	e := newEmailSend(t, "test")
	e.redis.AddHook(failKeyHook{name: "set", prefix: "mfa_email_sent:"})

	response := e.send()

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.False(t, e.exists(t, "mfa_email_login:"+e.jti), "no code may be issued past an unreachable limit")
}

// Without email delivery the development fallback logs the code. Anything but
// an explicit development or test environment must refuse instead, including
// an unrecognised one.
func TestSendEmailMFACodeRefusesOutsideDevelopmentWithoutEmailDelivery(t *testing.T) {
	for _, environment := range []string{"production", "staging", ""} {
		t.Run(fmt.Sprintf("%q", environment), func(t *testing.T) {
			e := newEmailSend(t, environment)

			response := e.send()

			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), errMsgEmailNotConfigured)
			require.False(t, e.exists(t, "mfa_email_login:"+e.jti))
			require.False(t, e.exists(t, "mfa_email_sent:"+e.jti))
		})
	}
}

// SMS has no provider, so its setup code can only reach the user as dev_codes
// in the response. Anywhere but an explicit development or test environment
// that would make SMS a factor proving nothing, so it cannot be enrolled.
func TestEmailSmsSetupRefusesSMSOutsideDevelopment(t *testing.T) {
	const refusal = "SMS MFA is not yet available"
	for _, tc := range []struct {
		environment string
		refused     bool
	}{
		{environment: "production", refused: true},
		{environment: "staging", refused: true},
		{environment: "", refused: true},
		{environment: "Development", refused: true},
		{environment: "development", refused: false},
		{environment: "test", refused: false},
	} {
		t.Run(fmt.Sprintf("%q", tc.environment), func(t *testing.T) {
			h, _ := scriptedHandler(t, &mfaEventDB{}, tc.environment)
			c, response := enrollRequest("/api/v1/mfa/email-sms/setup", `{"password":"x","methods":["sms"]}`, "session-a")

			h.EmailSmsSetup(c)

			if tc.refused {
				require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
				require.Contains(t, response.Body.String(), refusal)
				return
			}
			require.NotContains(t, response.Body.String(), refusal)
		})
	}
}

// An inbox marked recovery-only must not receive a sign-in code: whoever holds
// the password and the inbox would otherwise sign in with them.
func TestSendEmailMFACodeRefusesARecoveryOnlyEmail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra scriptedAnswer
		want  int
	}{
		{name: "recovery only", extra: scriptedAnswer{match: "recovery_only_methods FROM users", values: []driver.Value{[]byte("{totp,email}"), []byte("{email}")}}, want: http.StatusBadRequest},
		{name: "unreadable", extra: scriptedAnswer{match: "recovery_only_methods FROM users", err: errReadFailed}, want: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmailSend(t, "test", tc.extra)

			response := e.send()

			require.Equal(t, tc.want, response.Code, response.Body.String())
			require.False(t, e.exists(t, "mfa_email_login:"+e.jti))
			require.False(t, e.exists(t, "mfa_email_sent:"+e.jti))
		})
	}
}

// loggingEmailService is the email service production wiring builds when SMTP
// is unset, with the handler's own environment: it exists, and in development
// or test logs every code it is asked to send.
func loggingEmailService(environment string) *email.Service {
	return email.NewService(&config.Config{Environment: environment}, logger.New("test"))
}

// An unsent setup code is discarded, but only while it is still this
// request's: a newer setup request may already have stored and sent its own
// (security review, PR #3460).
func TestSendEmailSmsSetupEmailDiscardsOnlyItsOwnCode(t *testing.T) {
	for _, tc := range []struct {
		name, stored string
		kept         bool
	}{
		{name: "its own code", stored: "123456", kept: false},
		{name: "a newer request's code", stored: "999999", kept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mini, redisClient := newEnrollmentRedis(t)
			key := fmt.Sprintf(redisKeyEmailSmsSetup, enrollUser, "email")
			require.NoError(t, mini.Set(key, tc.stored))
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			unreachable := email.NewService(&config.Config{SMTPHost: "127.0.0.1", SMTPPort: 1}, logger.New("test"))
			h := &Handler{emailSvc: unreachable, redis: redisClient, log: logger.New("test")}

			require.False(t, h.sendEmailSmsSetupEmail(c, enrollUser, "user@example.com", map[string]string{"email": "123456"}))

			require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			require.Equal(t, tc.kept, mini.Exists(key))
		})
	}
}

// The email service logs codes whenever SMTP is unset, whatever the
// environment. A service that exists is therefore not one that delivers, and
// outside development a code must not go to it (security review, #3460).
func TestSendEmailMFACodeRefusesAnEmailServiceThatOnlyLogs(t *testing.T) {
	for _, tc := range []struct {
		environment string
		refused     bool
	}{
		{environment: "staging", refused: true},
		{environment: "production", refused: true},
		{environment: "", refused: true},
		{environment: "development", refused: false},
	} {
		t.Run(fmt.Sprintf("%q", tc.environment), func(t *testing.T) {
			e := newEmailSend(t, tc.environment)
			e.h.SetEmailService(loggingEmailService(tc.environment))

			response := e.send()

			if !tc.refused {
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				return
			}
			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), errMsgEmailNotConfigured)
			require.False(t, e.exists(t, "mfa_email_login:"+e.jti), "no code may exist that only a log holds")
			require.False(t, e.exists(t, "mfa_email_sent:"+e.jti))
		})
	}
}

func TestEmailSmsSetupRefusesAnEmailServiceThatOnlyLogs(t *testing.T) {
	for _, tc := range []struct {
		environment string
		refused     bool
	}{
		{environment: "staging", refused: true},
		{environment: "development", refused: false},
	} {
		t.Run(tc.environment, func(t *testing.T) {
			hash := correctPasswordHash(t)
			// The step-up's subject read: this password, and a confirmed TOTP,
			// which is the Standard factor email/SMS setup requires.
			h, redisClient := scriptedHandler(t, &mfaEventDB{passwordHash: hash}, tc.environment,
				scriptedAnswer{match: "FROM users u WHERE u.id", values: []driver.Value{hash, true, false}},
				scriptedAnswer{match: "SELECT email FROM users", values: []driver.Value{"email-fixture@example.test"}})
			h.SetEmailService(loggingEmailService(tc.environment))
			// The step-up factor: an inline WebAuthn token minted for this
			// route verifies without the database.
			storeInlineFactor(t, redisClient, stepup.PurposeEmailSmsSetup)
			c, response := enrollRequest("/api/v1/mfa/email-sms/setup",
				`{"password":"correct","mfa_code":"`+inlineFactor+`","methods":["email"]}`, "session-a")

			h.EmailSmsSetup(c)

			if !tc.refused {
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				return
			}
			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), errMsgEmailNotConfigured)
			n, err := redisClient.Exists(context.Background(), fmt.Sprintf(redisKeyEmailSmsSetup, enrollUser, "email")).Result()
			require.NoError(t, err)
			require.Zero(t, n, "no setup code may be stored for a send that is refused")
		})
	}
}

func TestSendEmailMFACodeReleasesTheLimitWhenTheCodeCannotBeStored(t *testing.T) {
	e := newEmailSend(t, "test")
	e.redis.AddHook(failKeyHook{name: "set", prefix: "mfa_email_login:"})

	response := e.send()

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.False(t, e.exists(t, "mfa_email_sent:"+e.jti), "the user must be able to ask for another code")
}

// A verified MFA-upgrade challenge is spent by writing the bypass the refresh
// reads. Answering 200 when that write failed would send the client to a
// refresh that asks for the factor again, with its challenge already used.
func TestMFAUpgradeReportsABypassItCouldNotWrite(t *testing.T) {
	claims := &ChallengeClaims{UserID: enrollUser, RefreshSessionID: "session-a"}
	bypassKey := auth.MFAUpgradeBypassKey(enrollUser, "session-a")
	for _, tc := range []struct {
		name   string
		fail   bool
		status int
	}{
		{name: "written", status: http.StatusOK},
		{name: "unwritable", fail: true, status: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
			if tc.fail {
				redisClient.AddHook(failKeyHook{name: "set", prefix: "mfa_upgrade_bypass:"})
			}
			c, response := enrollRequest("/api/v1/auth/mfa/verify", "", "")

			completed := h.completeVerifyPurpose(context.Background(), c, claims, PurposeMFAUpgrade, false)

			require.Equal(t, tc.status, response.Code, response.Body.String())
			require.Equal(t, !tc.fail, completed)
			n, err := redisClient.Exists(context.Background(), bypassKey).Result()
			require.NoError(t, err)
			require.Equal(t, !tc.fail, n == 1)
			if tc.fail {
				require.Contains(t, response.Body.String(), errMsgMFAVerificationUnavailable)
			}
		})
	}
}

// ── MFA state reads used by step-up ─────────────────────────────────────────

func TestIsEnabledRequiresAFactorWhenItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer scriptedAnswer
		want   bool
	}{
		// IsEnabled asks whether a step-up needs an MFA leg: whether the account
		// holds an inline factor (confirmed TOTP or a security key).
		{name: "off", answer: scriptedAnswer{match: "WHERE user_id = $1 AND enabled AND confirmed", values: []driver.Value{false, false}}, want: false},
		{name: "on", answer: scriptedAnswer{match: "WHERE user_id = $1 AND enabled AND confirmed", values: []driver.Value{true, false}}, want: true},
		{name: "unreadable", answer: scriptedAnswer{match: "WHERE user_id = $1 AND enabled AND confirmed", err: errReadFailed}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := scriptedHandler(t, &mfaEventDB{}, "test", tc.answer)
			require.Equal(t, tc.want, h.IsEnabled(context.Background(), enrollUser))
		})
	}
}

// ── Status reads ────────────────────────────────────────────────────────────

func statusAnswers(fault *scriptedAnswer) []scriptedAnswer {
	answers := []scriptedAnswer{
		{match: "FROM user_mfa_totp", noRows: true},
		{match: "recovery_only_methods, recovery_hardened, backup_email", values: []driver.Value{false, []byte("{}"), []byte("{}"), false, nil}},
	}
	if fault != nil {
		answers = append([]scriptedAnswer{*fault}, answers...)
	}
	return answers
}

func runGetStatus(t *testing.T, fault *scriptedAnswer, hooks ...redis.Hook) *httptest.ResponseRecorder {
	t.Helper()
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test", statusAnswers(fault)...)
	for _, hook := range hooks {
		redisClient.AddHook(hook)
	}
	c, response := enrollRequest("/api/v1/auth/mfa/status", "", "session-a")
	h.GetStatus(c)
	return response
}

func TestGetStatusFailsRatherThanReportAReadItCouldNotMake(t *testing.T) {
	require.Equal(t, http.StatusOK, runGetStatus(t, nil).Code, "control: every read succeeds")

	for _, tc := range []struct {
		name  string
		fault *scriptedAnswer
		hook  redis.Hook
	}{
		{name: "totp", fault: &scriptedAnswer{match: "FROM user_mfa_totp", err: errReadFailed}},
		{name: "webauthn", fault: &scriptedAnswer{match: "COUNT(*) FROM user_mfa_webauthn", err: errReadFailed}},
		{name: "account flags", fault: &scriptedAnswer{match: "recovery_only_methods, recovery_hardened", err: errReadFailed}},
		{name: "email and sms", hook: failKeyHook{name: "exists", prefix: "mfa_emailsms_enabled:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hooks []redis.Hook
			if tc.hook != nil {
				hooks = append(hooks, tc.hook)
			}
			response := runGetStatus(t, tc.fault, hooks...)
			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
		})
	}
}

func TestEmailSmsStatusFailsWhenItCannotReadTheFlags(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
	redisClient.AddHook(failKeyHook{name: "exists", prefix: "mfa_emailsms_enabled:"})
	c, response := enrollRequest("/api/v1/auth/mfa/email-sms/status", "", "session-a")

	h.EmailSmsStatus(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
}

func TestGetBackupEmailFailsWhenItCannotRead(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: "SELECT backup_email FROM users", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/backup-email", "", "session-a")

	h.GetBackupEmail(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
}

// ── Disable and delete paths ────────────────────────────────────────────────

func TestTOTPDisableReportsAReadItCouldNotMake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  *mfaEventDB
		answer scriptedAnswer
		body   string
	}{
		// Unreadable, the enrollment used to read as absent, and the handler
		// answered "disabled" without disabling anything.
		// No space after EXISTS: this is TOTPDisable's own probe, not the
		// step-up's factor read ("SELECT EXISTS (").
		{name: "enrollment", state: &mfaEventDB{}, answer: scriptedAnswer{match: "SELECT EXISTS(SELECT 1 FROM user_mfa_totp", err: errReadFailed}, body: errMsgFailedDisableMFA},
		// A failed password lookup is not a wrong password.
		{name: "password", state: &mfaEventDB{totpExists: true}, answer: scriptedAnswer{match: "credential_epoch", err: errReadFailed}, body: stepup.ErrMsgVerificationFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := scriptedHandler(t, tc.state, "test", tc.answer)
			c, response := enrollRequest("/api/v1/auth/mfa/totp/disable", `{"password":"correct","code":"000000"}`, "session-a")

			h.TOTPDisable(c)

			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), tc.body)
		})
	}
}

const credentialFixtureID = "11111111-2222-3333-4444-555555555555"

func deleteWebAuthnCredential(t *testing.T, state *mfaEventDB, answers ...scriptedAnswer) (*httptest.ResponseRecorder, *securityEventRecorder) {
	t.Helper()
	return deleteWebAuthnCredentialID(t, credentialFixtureID, state, answers...)
}

func deleteWebAuthnCredentialID(t *testing.T, id string, state *mfaEventDB, answers ...scriptedAnswer) (*httptest.ResponseRecorder, *securityEventRecorder) {
	t.Helper()
	state.passwordHash = correctPasswordHash(t)
	h, _ := scriptedHandler(t, state, "test", answers...)
	recorder := &securityEventRecorder{}
	h.SetSecurityEvents(recorder)
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/credentials/"+id, `{"password":"correct"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.WebAuthnDeleteCredential(c)
	return response, recorder
}

// An id that is not a UUID names no key. It is a 404 like any other miss, not
// the 500 the database's parse error would produce, and no delete is tried.
func TestWebAuthnDeleteAnswersAMalformedIDAsNotFound(t *testing.T) {
	state := &mfaEventDB{}
	response, _ := deleteWebAuthnCredentialID(t, "not-a-uuid", state)

	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgKeyNotFound)
	for _, statement := range state.statements {
		require.NotContains(t, statement, "DELETE", "a malformed id must not reach the database")
	}
}

// The client hands remaining_credential_ids to the authenticator as every key
// still accepted. An empty list after a failed read would hide every key the
// user still has, so the field is left out instead.
func TestWebAuthnDeleteOmitsTheRemainingKeysWhenItCannotListThem(t *testing.T) {
	response, _ := deleteWebAuthnCredential(t, &mfaEventDB{})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"remaining_credential_ids":[]`, "control: a readable empty list is sent")

	response, _ = deleteWebAuthnCredential(t, &mfaEventDB{},
		scriptedAnswer{match: "SELECT credential_id FROM user_mfa_webauthn", err: errReadFailed})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NotContains(t, response.Body.String(), "remaining_credential_ids")
}

// The recovery-only update runs in the disable's transaction before the Redis
// delete, so its failure leaves both methods on and the answer says so.
func TestEmailSmsDisableReportsARecoveryOnlyWriteItCouldNotMake(t *testing.T) {
	// Exec 1 is the recovery-only update.
	h, redisClient := scriptedHandler(t, &mfaEventDB{failFirstExec: true, passwordHash: correctPasswordHash(t)}, "test")
	emailOn := fmt.Sprintf(redisKeyEmailSmsEnabled, enrollUser, "email")
	require.NoError(t, redisClient.Set(context.Background(), emailOn, "1", 0).Err())
	c, response := enrollRequest("/api/v1/auth/mfa/email-sms", `{"password":"correct"}`, "session-a")

	h.EmailSmsDisable(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedDisableEmailSms)
	require.Equal(t, int64(1), redisClient.Exists(context.Background(), emailOn).Val(), "a disable that failed must leave the method on")
}

// ── Credential listing and setup writes ─────────────────────────────────────

func TestWebAuthnListCredentialsFailsOnARowItCannotRead(t *testing.T) {
	// One column where five are scanned.
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: "SELECT id, credential_name", values: []driver.Value{"one-column"}})
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/credentials", "", "session-a")

	h.WebAuthnListCredentials(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
}

func TestBuildWebAuthnUserFailsOnACredentialItCannotRead(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: "SELECT credential_id, public_key", values: []driver.Value{"one-column"}})

	_, err := h.buildWebAuthnUser(context.Background(), enrollUser)

	require.Error(t, err)
}

// SetRecoveryOnly answers with the value its own write left. It used to re-read
// the flag and report false when that read failed.
func TestSetRecoveryOnlyReportsTheValueItsWriteLeft(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "test",
		scriptedAnswer{match: "SELECT mfa_enabled FROM users", values: []driver.Value{false}},
		scriptedAnswer{match: "SELECT mfa_methods FROM users", values: []driver.Value{[]byte("{totp,email}")}},
		scriptedAnswer{match: "RETURNING recovery_hardened", values: []driver.Value{true}},
		scriptedAnswer{match: "SELECT recovery_hardened FROM users", err: errReadFailed},
	)
	c, response := enrollRequest("/api/v1/auth/mfa/recovery-only", `{"methods":["email"],"password":"correct"}`, "session-a")

	h.SetRecoveryOnly(c)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"recovery_hardened":true`)
}

func TestEmailSmsSetupCodesFailWhenOneCannotBeStored(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
	redisClient.AddHook(failKeyHook{name: "set", prefix: "mfa_emailsms_setup:"})

	_, err := h.generateAndStoreEmailSmsCodes(context.Background(), enrollUser, []string{"email"})

	require.Error(t, err, "an unstored code can never be verified, so none may be sent")
}

// The stored session is not JSON, so a handler that read it would answer
// errMsgInvalidSessionData. Only the read-fault arm answers with its own body,
// and a handler that lost that arm would read an empty value and fail the
// decode instead, which is also a 500.
func TestWebAuthnCeremoniesReportAnUnreadableSessionAsAnOutage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
		call   func(*Handler, *gin.Context)
		body   string
	}{
		{name: "register finish", prefix: "webauthn_reg:", call: func(h *Handler, c *gin.Context) { h.WebAuthnRegisterFinish(c) }, body: "Failed to complete registration"},
		{name: "inline verify finish", prefix: "mfa_inline_purpose_session:", call: func(h *Handler, c *gin.Context) { h.WebAuthnVerifyInlineFinish(c) }, body: errMsgFailedVerify},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
			require.NoError(t, redisClient.Set(context.Background(), tc.prefix+enrollUser, "not json", time.Minute).Err())
			redisClient.AddHook(failKeyHook{name: "getdel", prefix: tc.prefix})
			c, response := enrollRequest("/api/v1/auth/mfa/webauthn", "{}", "session-a")

			tc.call(h, c)

			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), tc.body)
			require.NotContains(t, response.Body.String(), errMsgInvalidSessionData)
		})
	}
}

// ── Recovery request responses ──────────────────────────────────────────────

func TestRespondToRecoveryRequestAnswersAMalformedIDAsNotFound(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test")
	c, response := enrollRequest("/api/v1/auth/mfa/recovery/requests/not-a-uuid", `{"action":"approve"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}

	h.RespondToRecoveryRequest(c)

	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Recovery request not found")
}

// A generic read failure (not sql.ErrNoRows) on the ownership/status lookup
// must answer 500, not 404 — a 404 here would tell a guesser their id was
// simply wrong rather than that the server could not check it.
func TestRespondToRecoveryRequestFailsWhenItCannotReadTheRequest(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "SELECT user_id, status FROM recovery_requests", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/recovery/requests/"+credentialFixtureID, `{"action":"approve","encrypted_payload":"AAAA","responder_public_key":"AAAA"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: credentialFixtureID}}

	h.RespondToRecoveryRequest(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedRespondRecovery)
}

// executeRecoveryResponse's write failing (approve branch) must answer 500
// rather than silently claim the response landed.
func TestRespondToRecoveryRequestFailsWhenTheApprovalWriteFails(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{failFirstExec: true}, "test",
		scriptedAnswer{match: "SELECT user_id, status FROM recovery_requests", values: []driver.Value{enrollUser, "pending"}})
	c, response := enrollRequest("/api/v1/auth/mfa/recovery/requests/"+credentialFixtureID, `{"action":"approve","encrypted_payload":"AAAA","responder_public_key":"AAAA"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: credentialFixtureID}}

	h.RespondToRecoveryRequest(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedRespondRecovery)
}

// A write whose RowsAffected() itself cannot be read must answer 500, exactly
// as a write that failed outright — the caller cannot tell the response
// landed.
func TestRespondToRecoveryRequestFailsWhenItCannotReadTheWriteResult(t *testing.T) {
	h, _ := scriptedHandlerFull(t, &mfaEventDB{}, "test", nil,
		[]scriptedAnswer{{match: "SELECT user_id, status FROM recovery_requests", values: []driver.Value{enrollUser, "pending"}}},
		[]scriptedExecAnswer{{match: "status = 'rejected'", rowsAffectedErr: errReadFailed}})
	c, response := enrollRequest("/api/v1/auth/mfa/recovery/requests/"+credentialFixtureID, `{"action":"reject"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: credentialFixtureID}}

	h.RespondToRecoveryRequest(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedRespondRecovery)
}

// A concurrent response landing between the read and the write leaves this
// write matching no row; the caller must be told to retry, not congratulated.
func TestRespondToRecoveryRequestAnswersARaceAsAlreadyResponded(t *testing.T) {
	h, _ := scriptedHandlerFull(t, &mfaEventDB{}, "test", nil,
		[]scriptedAnswer{{match: "SELECT user_id, status FROM recovery_requests", values: []driver.Value{enrollUser, "pending"}}},
		[]scriptedExecAnswer{{match: "status = 'rejected'", rowsAffected: 0}})
	c, response := enrollRequest("/api/v1/auth/mfa/recovery/requests/"+credentialFixtureID, `{"action":"reject"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: credentialFixtureID}}

	h.RespondToRecoveryRequest(c)

	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Request already responded to")
}

// ── Social recovery responses ───────────────────────────────────────────────

func TestRespondToSocialRecoveryFailsWhenItCannotReadTheRequest(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "rr.status, rr.circle_id, rc.threshold_k, rr.expires_at", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/recovery/social/"+credentialFixtureID, `{"encrypted_share":"AAAA"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: credentialFixtureID}}

	h.RespondToSocialRecovery(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedSubmitResponse)
}

func TestRespondToSocialRecoveryFailsWhenItCannotInsertTheShare(t *testing.T) {
	future := time.Now().Add(time.Hour)
	h, _ := scriptedHandlerFull(t, &mfaEventDB{failFirstExec: true}, "test", nil,
		[]scriptedAnswer{{match: "rr.status, rr.circle_id, rc.threshold_k, rr.expires_at",
			values: []driver.Value{"pending", credentialFixtureID, int64(2), future}}},
		nil)
	c, response := enrollRequest("/api/v1/auth/mfa/recovery/social/"+credentialFixtureID, `{"encrypted_share":"AAAA"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: credentialFixtureID}}

	h.RespondToSocialRecovery(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedSubmitResponse)
}

// A response whose contact already answered inserts nothing (ON CONFLICT DO
// NOTHING) and must be refused as a duplicate, not counted a second time.
func TestRespondToSocialRecoveryRefusesADuplicateResponse(t *testing.T) {
	future := time.Now().Add(time.Hour)
	h, _ := scriptedHandlerFull(t, &mfaEventDB{}, "test", nil,
		[]scriptedAnswer{{match: "rr.status, rr.circle_id, rc.threshold_k, rr.expires_at",
			values: []driver.Value{"pending", credentialFixtureID, int64(2), future}}},
		[]scriptedExecAnswer{{match: "recovery_circle_responses", rowsAffected: 0}})
	c, response := enrollRequest("/api/v1/auth/mfa/recovery/social/"+credentialFixtureID, `{"encrypted_share":"AAAA"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: credentialFixtureID}}

	h.RespondToSocialRecovery(c)

	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "already responded")
}

// A completing response (shares_received reaches the threshold) must mark the
// request complete; a failure doing so must not silently drop the completion.
func TestRespondToSocialRecoveryFailsWhenItCannotMarkTheRequestComplete(t *testing.T) {
	future := time.Now().Add(time.Hour)
	h, _ := scriptedHandlerFull(t, &mfaEventDB{failExecAt: 2}, "test", nil,
		[]scriptedAnswer{
			{match: "rr.status, rr.circle_id, rc.threshold_k, rr.expires_at",
				values: []driver.Value{"pending", credentialFixtureID, int64(1), future}},
			{match: "SET shares_received = shares_received + 1", values: []driver.Value{int64(1)}},
		},
		nil)
	c, response := enrollRequest("/api/v1/auth/mfa/recovery/social/"+credentialFixtureID, `{"encrypted_share":"AAAA"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: credentialFixtureID}}

	h.RespondToSocialRecovery(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedSubmitResponse)
}

// end of social recovery response tests

// ── WebAuthn registration ────────────────────────────────────────────────────

func TestWebAuthnRegisterBeginFailsWhenItCannotCountExistingCredentials(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "test",
		scriptedAnswer{match: "SELECT COUNT(*) FROM user_mfa_webauthn WHERE user_id = $1", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/register/begin", `{"password":"correct"}`, "session-a")

	h.WebAuthnRegisterBegin(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedStartReg)
}

func TestWebAuthnRegisterBeginFailsWhenItCannotBuildTheUser(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "test",
		scriptedAnswer{match: "COALESCE(display_name", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/register/begin", `{"password":"correct"}`, "session-a")

	h.WebAuthnRegisterBegin(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedStartReg)
}

// Without a stored session, finish can never succeed, so a Redis failure
// storing it must refuse the ceremony rather than hand out options.
func TestWebAuthnRegisterBeginFailsWhenItCannotStoreTheSession(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "test")
	h.webauthn, _ = NewWebAuthnService("webauthn.io", "test", []string{"https://webauthn.io"})
	redisClient.AddHook(failKeyHook{name: "set", prefix: "webauthn_reg:"})
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/register/begin", `{"password":"correct"}`, "session-a")

	h.WebAuthnRegisterBegin(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedStartReg)
}

// The stored ceremony metadata and session are both attacker-unreachable
// Redis values, but a corrupted one (e.g. a partial write) must not panic —
// it must be reported as an outage.
func TestWebAuthnRegisterFinishFailsOnUndecodableMetadataOrSession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "metadata", value: "not json"},
		{name: "session", value: `{"session":"not json","credential_name":"x","credential_type":"hardware"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
			require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(redisKeyWebAuthnReg, enrollUser), tc.value, time.Minute).Err())
			c, response := enrollRequest("/api/v1/auth/mfa/webauthn/register/finish", "{}", "session-a")

			h.WebAuthnRegisterFinish(c)

			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), errMsgInvalidSessionData)
		})
	}
}

func TestWebAuthnRegisterFinishFailsWhenItCannotBuildTheUser(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: "COALESCE(display_name", err: errReadFailed})
	require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(redisKeyWebAuthnReg, enrollUser),
		`{"session":"{}","credential_name":"x","credential_type":"hardware"}`, time.Minute).Err())
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/register/finish", "{}", "session-a")

	h.WebAuthnRegisterFinish(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Failed to complete registration")
}

// A request body that cannot satisfy the WebAuthn attestation contract must
// be refused as a verification failure, not accepted or crashed on.
func TestWebAuthnRegisterFinishRefusesAMalformedAttestation(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
	h.webauthn, _ = NewWebAuthnService("webauthn.io", "test", []string{"https://webauthn.io"})
	require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(redisKeyWebAuthnReg, enrollUser),
		`{"session":"{}","credential_name":"x","credential_type":"hardware"}`, time.Minute).Err())
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/register/finish", "{}", "session-a")

	h.WebAuthnRegisterFinish(c)

	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Registration verification failed")
}

// ── WebAuthn credential listing and deletion ────────────────────────────────

func TestWebAuthnListCredentialsFailsWhenTheQueryFails(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "SELECT id, credential_name", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/credentials", "", "session-a")

	h.WebAuthnListCredentials(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedListKeys)
}

func TestWebAuthnListCredentialsFailsWhenIterationFails(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "SELECT id, credential_name", rowsErr: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/credentials", "", "session-a")

	h.WebAuthnListCredentials(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedListKeys)
}

func TestWebAuthnDeleteCredentialFailsWhenPasswordCannotBeVerified(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: "password_hash", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/credentials/"+credentialFixtureID, `{"password":"correct"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: credentialFixtureID}}

	h.WebAuthnDeleteCredential(c)

	// The password is read with the row lock; a failed read is not a wrong password.
	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), stepup.ErrMsgVerificationFailed)
}

func TestWebAuthnDeleteCredentialFailsWhenItCannotReadTheDeleteResult(t *testing.T) {
	h, _ := scriptedHandlerFull(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "test", nil, nil,
		[]scriptedExecAnswer{{match: "DELETE FROM user_mfa_webauthn WHERE id = $1 AND user_id = $2", rowsAffectedErr: errReadFailed}})
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/credentials/"+credentialFixtureID, `{"password":"correct"}`, "session-a")
	c.Params = gin.Params{{Key: "id", Value: credentialFixtureID}}

	h.WebAuthnDeleteCredential(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedDeleteCredential)
}

// buildWebAuthnUser's own credential-listing query failing mid-iteration must
// surface as an error, not a silently short exclusion list (a key missing
// from it can be registered a second time).
func TestBuildWebAuthnUserFailsWhenCredentialIterationFails(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "SELECT credential_id, public_key", rowsErr: errReadFailed})

	_, err := h.buildWebAuthnUser(context.Background(), enrollUser)

	require.Error(t, err)
	require.Contains(t, err.Error(), "list credentials")
}

func TestRemainingCredentialIDsFailsWhenIterationFails(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "SELECT credential_id FROM user_mfa_webauthn", rowsErr: errReadFailed})

	_, err := h.remainingCredentialIDs(context.Background(), enrollUser)

	require.Error(t, err)
	require.Contains(t, err.Error(), "iterate credential IDs")
}

// ── WebAuthn inline verify (protected-operation step-up) ───────────────────

// inlineBeginBody names a purpose, which begin requires before any other work.
const inlineBeginBody = `{"purpose":"mfa_settings.totp_setup"}`

// inlineStoredSession is an empty ceremony that carries a purpose: finish
// refuses a session with none before reaching the step under test.
const inlineStoredSession = `{"purpose":"mfa_settings.totp_setup"}`

func TestWebAuthnVerifyInlineBeginFailsWhenItCannotBuildTheUser(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: "COALESCE(display_name", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/verify/begin", inlineBeginBody, "session-a")

	h.WebAuthnVerifyInlineBegin(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedStartVerification)
}

// Without a stored session, finish can never succeed, so a Redis failure
// storing it must refuse the ceremony.
func TestWebAuthnVerifyInlineBeginFailsWhenItCannotStoreTheSession(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "SELECT credential_id, public_key", values: []driver.Value{
			[]byte("cred-id"), []byte("pub-key"), []byte("aaguid"), int64(0), []byte("{usb}"),
		}})
	h.webauthn, _ = NewWebAuthnService("webauthn.io", "test", []string{"https://webauthn.io"})
	redisClient.AddHook(failKeyHook{name: "set", prefix: "mfa_inline_purpose_session:"})
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/verify/begin", inlineBeginBody, "session-a")

	h.WebAuthnVerifyInlineBegin(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedStartVerification)
}

func TestWebAuthnVerifyInlineFinishFailsOnUndecodableSession(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
	require.NoError(t, redisClient.Set(context.Background(), inlineSessionKey(enrollUser), "not json", time.Minute).Err())
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/verify/finish", "{}", "session-a")

	h.WebAuthnVerifyInlineFinish(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgInvalidSessionData)
}

func TestWebAuthnVerifyInlineFinishFailsWhenItCannotBuildTheUser(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: "COALESCE(display_name", err: errReadFailed})
	require.NoError(t, redisClient.Set(context.Background(), inlineSessionKey(enrollUser), inlineStoredSession, time.Minute).Err())
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/verify/finish", "{}", "session-a")

	h.WebAuthnVerifyInlineFinish(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedVerify)
}

// A malformed assertion must be refused as a failed verification, not accepted.
func TestWebAuthnVerifyInlineFinishRefusesAMalformedAssertion(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
	h.webauthn, _ = NewWebAuthnService("webauthn.io", "test", []string{"https://webauthn.io"})
	require.NoError(t, redisClient.Set(context.Background(), inlineSessionKey(enrollUser), inlineStoredSession, time.Minute).Err())
	c, response := enrollRequest("/api/v1/auth/mfa/webauthn/verify/finish", "{}", "session-a")

	h.WebAuthnVerifyInlineFinish(c)

	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Verification failed. Try again.")
}

// ── BeginWebAuthnLogin ───────────────────────────────────────────────────────

// Without the stored session, the assertion can never be verified, so a Redis
// failure storing it must refuse to hand out options.
func TestBeginWebAuthnLoginFailsWhenItCannotStoreTheSession(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "SELECT credential_id, public_key", values: []driver.Value{
			[]byte("cred-id"), []byte("pub-key"), []byte("aaguid"), int64(0), []byte("{usb}"),
		}})
	h.webauthn, _ = NewWebAuthnService("webauthn.io", "test", []string{"https://webauthn.io"})
	redisClient.AddHook(failKeyHook{name: "set", prefix: "mfa_webauthn_session:"})

	_, err := h.BeginWebAuthnLogin(context.Background(), enrollUser, "jti-fixture")

	require.Error(t, err)
	require.Contains(t, err.Error(), "store webauthn login session")
}

// ── TOTPSetup ────────────────────────────────────────────────────────────────

// An unreadable pre-existing-enrollment check must stop setup: reading it as
// absent would let the upsert replace an active TOTP and turn MFA off.
func TestTOTPSetupFailsWhenItCannotCheckExistingEnrollment(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "test",
		scriptedAnswer{match: "SELECT confirmed FROM user_mfa_totp", err: errReadFailed})
	storeInlineFactor(t, redisClient, stepup.PurposeTOTPSetup)
	c, response := enrollRequest("/api/v1/auth/mfa/totp/setup", `{"password":"correct","mfa_code":"`+inlineFactor+`"}`, "session-a")

	h.TOTPSetup(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Failed to start TOTP setup")
}

func TestTOTPSetupFailsWhenItCannotReadTheAccountEmail(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "test",
		scriptedAnswer{match: "SELECT confirmed FROM user_mfa_totp", noRows: true},
		scriptedAnswer{match: "SELECT email FROM users", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/totp/setup", `{"password":"correct"}`, "session-a")

	h.TOTPSetup(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Failed to fetch user")
}

func TestTOTPSetupFailsWhenItCannotStoreTheSecret(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t), failFirstExec: true}, "test",
		scriptedAnswer{match: "SELECT confirmed FROM user_mfa_totp", noRows: true},
		scriptedAnswer{match: "SELECT email FROM users", values: []driver.Value{"fixture@example.test"}})
	h.keyring = testKeyring(t)
	c, response := enrollRequest("/api/v1/auth/mfa/totp/setup", `{"password":"correct"}`, "session-a")

	h.TOTPSetup(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedStoreTOTPSeed)
}

// ── TOTPVerifySetup ──────────────────────────────────────────────────────────

func TestTOTPVerifySetupRefusesPastTheSetupLockout(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
	require.NoError(t, redisClient.Set(context.Background(), "mfa_setup_lockout:"+enrollUser, "1", time.Minute).Err())
	c, response := enrollRequest("/api/v1/auth/mfa/totp/verify-setup", `{"code":"123456"}`, "session-a")

	h.TOTPVerifySetup(c)

	require.Equal(t, http.StatusTooManyRequests, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgTooManyAttempts)
}

func TestTOTPVerifySetupFailsOnAGenericReadError(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "key_version, enabled FROM user_mfa_totp", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/totp/verify-setup", `{"code":"123456"}`, "session-a")

	h.TOTPVerifySetup(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedVerifyCode)
}

// A sealed secret that cannot be decrypted (corrupt ciphertext, not merely a
// wrong guess) must answer 500 with the generic verify-code message, not
// treat the request as a wrong code.
func TestTOTPVerifySetupFailsWhenTheSecretCannotBeDecrypted(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "key_version, enabled FROM user_mfa_totp", values: []driver.Value{
			[]byte("not-a-real-ciphertext-000000000"), []byte("bad-nonce-12"), int64(1), false,
		}})
	h.keyring = testKeyring(t)
	c, response := enrollRequest("/api/v1/auth/mfa/totp/verify-setup", `{"code":"123456"}`, "session-a")

	h.TOTPVerifySetup(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedVerifyCode)
}

func TestTOTPVerifySetupFailsWhenItCannotStoreTheVerifiedState(t *testing.T) {
	fx := newRealTOTPFixture(t)
	h, _ := scriptedHandlerFull(t, &mfaEventDB{failFirstExec: true}, "test", fx.keyring,
		[]scriptedAnswer{{match: "key_version, enabled FROM user_mfa_totp", values: []driver.Value{fx.secretEnc, fx.secretNonce, int64(fx.keyVersion), false}}},
		nil)
	c, response := enrollRequest("/api/v1/auth/mfa/totp/verify-setup", `{"code":"`+fx.code+`"}`, "session-a")

	h.TOTPVerifySetup(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Failed to complete verification")
}

// The attempt-count reset and the setup-session record are both best-effort:
// a Redis failure writing either must not fail a request that already
// verified the code and stored the backup codes.
func TestTOTPVerifySetupSucceedsDespiteBestEffortRedisFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hook   redis.Hook
		logMsg string
	}{
		{name: "attempt reset", hook: failKeyHook{name: "del", prefix: "mfa_setup_attempts:"}, logMsg: "Failed to clear MFA setup attempt counter"},
		{name: "setup session", hook: failKeyHook{name: "set", prefix: "mfa_totp_setup_session:"}, logMsg: "Failed to record the TOTP setup session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			fx := newRealTOTPFixture(t)
			db := sql.OpenDB(scriptedConnector{conn: scriptedConn{
				mfaEventConn: mfaEventConn{state: &mfaEventDB{}, id: 1},
				answers:      []scriptedAnswer{{match: "key_version, enabled FROM user_mfa_totp", values: []driver.Value{fx.secretEnc, fx.secretNonce, int64(fx.keyVersion), false}}},
			}})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			_, redisClient := newEnrollmentRedis(t)
			redisClient.AddHook(tc.hook)
			h := NewHandler(db, redisClient, logger.NewWithWriter(&buf), fx.keyring, "test", nil, "test")
			c, response := enrollRequest("/api/v1/auth/mfa/totp/verify-setup", `{"code":"`+fx.code+`"}`, "session-a")

			h.TOTPVerifySetup(c)

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "backup_codes")
			require.Contains(t, buf.String(), tc.logMsg)
		})
	}
}

// A wrong verify-setup code must count against the setup lockout; a count
// that cannot be written must fail the request rather than silently omit the
// limit.
func TestTOTPVerifySetupFailsWhenTheFailedAttemptCannotBeCounted(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "key_version, enabled FROM user_mfa_totp", values: []driver.Value{[]byte("x"), []byte("y"), int64(1), false}})
	h.keyring = testKeyring(t)
	redisClient.AddHook(failKeyHook{name: "incr", prefix: "mfa_setup_attempts:"})
	c, response := enrollRequest("/api/v1/auth/mfa/totp/verify-setup", `{"code":"000000"}`, "session-a")

	h.TOTPVerifySetup(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.NotContains(t, response.Body.String(), "Invalid code", "a request that never learned whether the count was written must not judge the code")
}

// ── TOTPConfirmSetup ─────────────────────────────────────────────────────────

func TestTOTPConfirmSetupFailsOnAGenericReadError(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "SELECT enabled, confirmed FROM user_mfa_totp", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/totp/confirm-setup", "", "session-a")

	h.TOTPConfirmSetup(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedActivateMFA)
}

// A read failure fetching the proving session must still activate MFA — the
// flags write already succeeded — but must be logged, since it means the
// enrolling session keeps the pre-MFA challenge it should have been exempted
// from.
func TestTOTPConfirmSetupLogsWhenTheProvingSessionCannotBeRead(t *testing.T) {
	var buf bytes.Buffer
	db := sql.OpenDB(scriptedConnector{conn: scriptedConn{
		mfaEventConn: mfaEventConn{state: &mfaEventDB{}, id: 1},
		answers:      []scriptedAnswer{{match: "SELECT enabled, confirmed FROM user_mfa_totp", values: []driver.Value{true, false}}},
	}})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, redisClient := newEnrollmentRedis(t)
	redisClient.AddHook(failKeyHook{name: "getdel", prefix: "mfa_totp_setup_session:"})
	h := NewHandler(db, redisClient, logger.NewWithWriter(&buf), nil, "test", nil, "test")
	c, response := enrollRequest("/api/v1/auth/mfa/totp/confirm-setup", "", "session-a")

	h.TOTPConfirmSetup(c)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "MFA is now active")
	require.Contains(t, buf.String(), "Failed to read the TOTP setup session")
}

// ── RegenerateBackupCodes ────────────────────────────────────────────────────

func TestRegenerateBackupCodesFailsWhenPasswordCannotBeVerified(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: "password_hash", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/totp/backup-codes", `{"password":"correct","code":"000000"}`, "session-a")

	h.RegenerateBackupCodes(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedVerifyLoginFactor)
}

func TestRegenerateBackupCodesFailsOnAGenericTOTPReadError(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "test",
		scriptedAnswer{match: "totp_secret_enc", err: errReadFailed})
	c, response := enrollRequest("/api/v1/auth/mfa/totp/backup-codes", `{"password":"correct","code":"000000"}`, "session-a")

	h.RegenerateBackupCodes(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgFailedBackupCodes)
}

func TestRegenerateBackupCodesFailsWhenItCannotStoreTheNewCodes(t *testing.T) {
	fx := newRealTOTPFixture(t)
	state := &mfaEventDB{
		passwordHash: correctPasswordHash(t), failFirstExec: true,
		totpSecretEnc: fx.secretEnc, totpSecretNonce: fx.secretNonce, totpKeyVersion: fx.keyVersion,
		totpEnabled: true, totpConfirmed: true,
	}
	h, _ := scriptedHandlerFull(t, state, "test", fx.keyring, nil, nil)
	c, response := enrollRequest("/api/v1/auth/mfa/totp/backup-codes", `{"password":"correct","code":"`+fx.code+`"}`, "session-a")

	h.RegenerateBackupCodes(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Failed to store backup codes")
}

// ── GetStatus self-heal / readEmailSmsEnabled ───────────────────────────────

// GetStatus's self-heal must fail closed on a re-read after a successful
// resync — a stale answer here would report a factor as off (or on) when the
// resync just changed it.
func TestGetStatusFailsWhenTheReReadAfterResyncFails(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test",
		scriptedAnswer{match: "FROM user_mfa_totp", noRows: true},
		scriptedAnswer{match: "recovery_only_methods, recovery_hardened, backup_email",
			values: []driver.Value{false, []byte("{totp}"), []byte("{}"), false, nil}},
		scriptedAnswer{match: "SELECT mfa_enabled, mfa_methods FROM users", err: errReadFailed},
	)
	c, response := enrollRequest("/api/v1/auth/mfa/status", "", "session-a")

	h.GetStatus(c)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Failed to load MFA status")
}

func TestReadEmailSmsEnabledFailsWhenTheSMSFlagCannotBeRead(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
	redisClient.AddHook(failKeyHook{name: "exists", prefix: "mfa_emailsms_enabled:" + enrollUser + ":sms"})

	_, _, err := h.readEmailSmsEnabled(context.Background(), enrollUser)

	require.Error(t, err)
	require.Contains(t, err.Error(), "read SMS MFA state")
}

// ── completeVerifiedChallenge / readRememberMe ──────────────────────────────

// The attempt-count reset after a verified challenge is best-effort: a Redis
// failure resetting it must not stop a login that already succeeded.
func TestCompleteVerifiedChallengeLogsWhenTheAttemptCountCannotBeReset(t *testing.T) {
	var buf bytes.Buffer
	f := newVerifyFixture(t)
	f.h.log = logger.NewWithWriter(&buf)
	f.redis.AddHook(failKeyHook{name: "del", prefix: "mfa_verify_attempts:"})

	response := f.verify(f.right)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.True(t, f.completer.called, "the login must still complete despite the best-effort failure")
	require.Contains(t, buf.String(), "Failed to clear MFA verification attempt counter")
}

func TestReadRememberMeFailsOnAGenericReadError(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
	redisClient.AddHook(failKeyHook{name: "get", prefix: "mfa_challenge:"})
	claims := &ChallengeClaims{UserID: enrollUser, RegisteredClaims: jwt.RegisteredClaims{ID: "jti-fixture"}}

	_, err := h.readRememberMe(context.Background(), claims, PurposeLogin)

	require.Error(t, err)
	require.Contains(t, err.Error(), "read MFA challenge remember state")
}

// ── verifyWebAuthnChallenge / verifyEmailCode ───────────────────────────────

func TestVerifyWebAuthnChallengeFailsOnUndecodableSession(t *testing.T) {
	f := newVerifyFixture(t)
	require.NoError(t, f.redis.Set(context.Background(), "mfa_webauthn_session:"+f.jti, "not json", time.Minute).Err())

	response := verifyWith(f.h, `{"mfa_challenge_token":"`+f.token+`","method":"webauthn","assertion":{}}`)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Invalid WebAuthn session")
}

// The keys are read before the ceremony is spent, so a database failure leaves
// the ceremony for a retry (security review, PR #3460).
func TestVerifyWebAuthnChallengeFailsWhenItCannotBuildTheUser(t *testing.T) {
	var logs bytes.Buffer
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: "COALESCE(display_name", err: errReadFailed})
	h.log = logger.NewWithWriter(&logs)
	token, jti := loginChallenge(t, h)
	ceremony := "mfa_webauthn_session:" + jti
	require.NoError(t, redisClient.Set(context.Background(), ceremony, "{}", time.Minute).Err())

	response := verifyWith(h, `{"mfa_challenge_token":"`+token+`","method":"webauthn","assertion":{}}`)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errMsgMFAVerificationUnavailable)
	// Any earlier outage answers the same 500 without touching the ceremony.
	require.Contains(t, logs.String(), "fetch user", "the failure must be the key read")
	require.Equal(t, int64(1), redisClient.Exists(context.Background(), ceremony).Val(), "a failed database read must not spend the ceremony")
}

// Clearing a spent email code is best-effort: a Redis failure clearing it must
// not undo a verification that already succeeded.
func TestVerifyEmailCodeLogsWhenItCannotClearTheSpentCode(t *testing.T) {
	var buf bytes.Buffer
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
	h.SetLoginCompleter(&recordingLoginCompleter{})
	h.log = logger.NewWithWriter(&buf)
	token, jti := loginChallenge(t, h)
	require.NoError(t, redisClient.Set(context.Background(), "mfa_email_login:"+jti, "123456", time.Minute).Err())
	redisClient.AddHook(failKeyHook{name: "del", prefix: "mfa_email_login:"})

	response := verifyWith(h, `{"mfa_challenge_token":"`+token+`","method":"email","code":"123456"}`)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, buf.String(), "Failed to clear MFA email code")
}

// ── recordCodeFailure ────────────────────────────────────────────────────────

func TestRecordCodeFailureFailsWhenTheCountCannotBeWritten(t *testing.T) {
	_, redisClient := newEnrollmentRedis(t)
	redisClient.AddHook(failKeyHook{name: "incr", prefix: "mfa_x_attempts:"})
	h := NewHandler(nil, redisClient, logger.New("test"), nil, "test", nil, "test")

	err := h.recordCodeFailure(context.Background(), enrollUser, "mfa_x_attempts:"+enrollUser, "mfa_x_lockout:"+enrollUser)

	require.Error(t, err)
	require.Contains(t, err.Error(), "count failed attempt")
}

// A count with no window would never expire, so it fails closed too.
func TestRecordCodeFailureFailsWhenTheExpiryCannotBeWritten(t *testing.T) {
	_, redisClient := newEnrollmentRedis(t)
	redisClient.AddHook(failKeyHook{name: "expire", prefix: "mfa_x_attempts:"})
	h := NewHandler(nil, redisClient, logger.New("test"), nil, "test", nil, "test")

	err := h.recordCodeFailure(context.Background(), enrollUser, "mfa_x_attempts:"+enrollUser, "mfa_x_lockout:"+enrollUser)

	require.Error(t, err)
	require.Contains(t, err.Error(), "set attempt window")
}

// A lockout write that fails at the limit must keep the count, so the next
// wrong code tries the lockout again, and must report the failure so the
// caller does not believe the limit engaged.
func TestRecordCodeFailureFailsWhenTheLockoutCannotBeWrittenAtTheLimit(t *testing.T) {
	_, redisClient := newEnrollmentRedis(t)
	require.NoError(t, redisClient.Set(context.Background(), "mfa_x_attempts:"+enrollUser, failedAttemptLimit-1, time.Minute).Err())
	redisClient.AddHook(failKeyHook{name: "set", prefix: "mfa_x_lockout:"})
	h := NewHandler(nil, redisClient, logger.New("test"), nil, "test", nil, "test")

	err := h.recordCodeFailure(context.Background(), enrollUser, "mfa_x_attempts:"+enrollUser, "mfa_x_lockout:"+enrollUser)

	require.Error(t, err)
	require.Contains(t, err.Error(), "arm lockout")
	n, err := redisClient.Exists(context.Background(), "mfa_x_attempts:"+enrollUser).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "the count must survive a failed lockout write")
}

// Resetting the count after a successful lockout is best-effort.
func TestRecordCodeFailureLogsWhenTheCountResetCannotBeWrittenAtTheLimit(t *testing.T) {
	var buf bytes.Buffer
	_, redisClient := newEnrollmentRedis(t)
	require.NoError(t, redisClient.Set(context.Background(), "mfa_x_attempts:"+enrollUser, failedAttemptLimit-1, time.Minute).Err())
	redisClient.AddHook(failKeyHook{name: "del", prefix: "mfa_x_attempts:"})
	h := NewHandler(nil, redisClient, logger.NewWithWriter(&buf), nil, "test", nil, "test")

	err := h.recordCodeFailure(context.Background(), enrollUser, "mfa_x_attempts:"+enrollUser, "mfa_x_lockout:"+enrollUser)

	require.NoError(t, err, "the lockout was written; a failed count reset must not fail the request")
	require.Contains(t, buf.String(), "Failed to reset MFA attempt counter after lockout")
}

// ── completeVerifyPurpose ────────────────────────────────────────────────────

func TestCompleteVerifyPurposeFailsWithNoLoginCompleterWired(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test")
	claims := &ChallengeClaims{UserID: enrollUser, RegisteredClaims: jwt.RegisteredClaims{ID: "jti-fixture"}}
	c, response := enrollRequest("/api/v1/auth/mfa/verify", "", "")

	ok := h.completeVerifyPurpose(context.Background(), c, claims, PurposeLogin, false)

	require.False(t, ok)
	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Login completion not configured")
}

// Clearing the remember-me key after login completes is best-effort: the
// challenge is already claimed, so a Redis failure here must not undo a
// completed login.
func TestCompleteVerifyPurposeLogsWhenTheRememberKeyCannotBeCleared(t *testing.T) {
	var buf bytes.Buffer
	_, redisClient := newEnrollmentRedis(t)
	redisClient.AddHook(failKeyHook{name: "del", prefix: "mfa_challenge:"})
	h := NewHandler(nil, redisClient, logger.NewWithWriter(&buf), nil, "test", nil, "test")
	completer := &recordingLoginCompleter{}
	h.SetLoginCompleter(completer)
	claims := &ChallengeClaims{UserID: enrollUser, RegisteredClaims: jwt.RegisteredClaims{ID: "jti-fixture"}}
	c, response := enrollRequest("/api/v1/auth/mfa/verify", "", "")

	ok := h.completeVerifyPurpose(context.Background(), c, claims, PurposeLogin, false)

	require.True(t, ok)
	require.True(t, completer.called)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, buf.String(), "Failed to clear MFA challenge remember-me state")
}
