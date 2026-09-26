package mfa

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
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

// securityEventRecorder may receive events from concurrent requests; tests read
// events only after every request has returned.
type securityEventRecorder struct {
	mu     sync.Mutex
	events []securityevent.Event
}

func (r *securityEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func requireSerializedEventPrivacy(t *testing.T, events []securityevent.Event) {
	t.Helper()
	require.NotEmpty(t, events)
	serialized, err := json.Marshal(events)
	require.NoError(t, err)
	for _, fixture := range []string{
		"code-fixture", "credential-fixture", "user-fixture", "session-fixture",
		"device-fixture", "token-fixture", "198.51.100.44", "email-fixture@example.test", "raw-error-fixture",
	} {
		require.NotContains(t, string(serialized), fixture)
	}
}

func TestChallengeEventMappingsAreClosedForEverySupportedMethod(t *testing.T) {
	for _, method := range []string{"totp", "backup_code", "webauthn", "unsupported"} {
		t.Run(method, func(t *testing.T) {
			denied := mfaChallengeInvalidEvent(method)
			require.Equal(t, securityevent.EventMFA, denied.EventType)
			require.Equal(t, securityevent.OutcomeDenied, denied.Outcome)
			require.Equal(t, securityevent.ReasonChallengeInvalid, denied.ReasonCode)
			require.Equal(t, securityevent.RouteAuthMFAVerify, denied.RouteTemplate)

			verified := mfaChallengeVerifiedEvent(method)
			require.Equal(t, securityevent.EventMFA, verified.EventType)
			require.Equal(t, securityevent.OutcomeSuccess, verified.Outcome)
			require.Equal(t, securityevent.ReasonChallengeVerified, verified.ReasonCode)
			require.Equal(t, securityevent.RouteAuthMFAVerify, verified.RouteTemplate)
		})
	}
}

type rejectedLoginCompleter struct{}

func (rejectedLoginCompleter) CompleteLogin(c *gin.Context, _ string, _ bool, _ string, _ securityevent.AuthMethod) bool {
	c.Status(http.StatusInternalServerError)
	return false
}

type committedLoginCompleter struct{}

func (committedLoginCompleter) CompleteLogin(c *gin.Context, _ string, _ bool, _ string, _ securityevent.AuthMethod) bool {
	c.Status(http.StatusOK)
	return true
}

type recordingLoginCompleter struct {
	called bool
	calls  int
	method securityevent.AuthMethod
}

func (c *recordingLoginCompleter) CompleteLogin(_ *gin.Context, _ string, _ bool, _ string, method securityevent.AuthMethod) bool {
	c.called = true
	c.calls++
	c.method = method
	return true
}

func TestCompleteVerifyPurposePreservesSignedPrimaryAuthMethod(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })

	for _, test := range []struct {
		name     string
		method   securityevent.AuthMethod
		want     securityevent.AuthMethod
		wantOK   bool
		wantHTTP int
	}{
		{name: "sso", method: securityevent.AuthSSO, want: securityevent.AuthSSO, wantOK: true, wantHTTP: http.StatusOK},
		{name: "legacy empty", method: "", want: securityevent.AuthPassword, wantOK: true, wantHTTP: http.StatusOK},
		{name: "unknown", method: securityevent.AuthMethod("forged-sso"), wantOK: false, wantHTTP: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := NewHandler(nil, redisClient, logger.New("test"), nil, "test", nil, "test")
			completer := &recordingLoginCompleter{}
			h.SetLoginCompleter(completer)
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)
			claims := &ChallengeClaims{UserID: "test-user", Purpose: PurposeLogin, PrimaryAuthMethod: test.method, RegisteredClaims: jwt.RegisteredClaims{ID: "test-challenge-" + test.name}}

			ok := h.completeVerifyPurpose(c.Request.Context(), c, claims, PurposeLogin, false)
			require.Equal(t, test.wantOK, ok)
			require.Equal(t, test.wantOK, completer.called)
			if test.wantOK {
				require.Equal(t, test.want, completer.method)
			}
			if !test.wantOK {
				require.Equal(t, test.wantHTTP, response.Code)
			}
		})
	}
}

func TestCompleteVerifiedLoginEmitsOnlyAfterAuthoritativeCompleterSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })

	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)
		return context, recorder
	}
	claims := &ChallengeClaims{UserID: "test-user", Purpose: PurposeLogin, RegisteredClaims: jwt.RegisteredClaims{ID: "test-challenge-failed"}}

	h := NewHandler(nil, redisClient, logger.New("test"), nil, "test", nil, "test")
	recorder := &securityEventRecorder{}
	h.SetSecurityEvents(recorder)
	h.SetLoginCompleter(rejectedLoginCompleter{})
	context, _ := newContext()
	h.completeVerifiedChallenge(context.Request.Context(), context, claims, PurposeLogin, "totp", false)
	require.Empty(t, recorder.events, "a failed session-mint completion must not produce challenge_verified")

	h.SetLoginCompleter(committedLoginCompleter{})
	context, _ = newContext()
	claims = &ChallengeClaims{UserID: "test-user", Purpose: PurposeLogin, RegisteredClaims: jwt.RegisteredClaims{ID: "test-challenge-success"}}
	h.completeVerifiedChallenge(context.Request.Context(), context, claims, PurposeLogin, "totp", false)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonChallengeVerified,
		AuthMethod: securityevent.AuthTOTP, RouteTemplate: securityevent.RouteAuthMFAVerify,
	}}, recorder.events)
	require.True(t, middleware.NightwatchHandled(context))
}

func TestCredentialDisableDenialsEmitAuthenticationEvents(t *testing.T) {
	passwordHash, err := auth.HashPasswordWithParams("correct", &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	for _, test := range []struct {
		name  string
		call  func(*Handler, *gin.Context)
		body  string
		setup func(*mfaEventDB)
	}{
		{name: "totp", body: `{"password":"wrong","code":"000000"}`, setup: func(s *mfaEventDB) { s.totpExists = true }, call: func(h *Handler, c *gin.Context) { h.TOTPDisable(c) }},
		{name: "webauthn", body: `{"password":"wrong"}`, call: func(h *Handler, c *gin.Context) { h.WebAuthnDeleteCredential(c) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &mfaEventDB{passwordHash: passwordHash}
			if test.setup != nil {
				test.setup(state)
			}
			db := sql.OpenDB(mfaEventConnector{state: state})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			h := NewHandler(db, nil, logger.New("test"), nil, "test", nil, "test")
			recorder := &securityEventRecorder{}
			h.SetSecurityEvents(recorder)
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/disable", strings.NewReader(test.body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("user_id", "user-fixture")
			if test.name == "webauthn" {
				c.Params = gin.Params{{Key: "id", Value: "11111111-2222-3333-4444-555555555555"}}
			}
			test.call(h, c)
			require.Equal(t, http.StatusForbidden, response.Code)
			require.Equal(t, []securityevent.Event{{
				EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied,
				Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials,
				AuthMethod: securityevent.AuthPassword,
			}}, recorder.events)
			require.True(t, middleware.NightwatchHandled(c))
		})
	}
}

func TestVerifySuccessEmitsServerMatchedFactorRatherThanRequestedMethod(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	// The request names backup_code but carries a TOTP code, so the event must
	// report the factor the server matched (totp), not the one requested. A
	// WebAuthn inline token cannot stand in here any more: login never accepts
	// one (#3453 RS11).
	fixture := newRealTOTPFixture(t)
	h := NewHandler(fakeMFADB(t, fixture.state()), redisClient, logger.New("test"), fixture.keyring, "test", nil, "test")
	h.SetLoginCompleter(committedLoginCompleter{})
	recorder := &securityEventRecorder{}
	h.SetSecurityEvents(recorder)
	challenge, _, err := h.GenerateLoginChallenge(context.Background(), "test-user", false, "", securityevent.AuthPassword)
	require.NoError(t, err)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", strings.NewReader(`{"mfa_challenge_token":"`+challenge+`","method":"backup_code","code":"`+fixture.code+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Verify(c)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonChallengeVerified,
		AuthMethod: securityevent.AuthTOTP, RouteTemplate: securityevent.RouteAuthMFAVerify,
	}}, recorder.events)
	require.True(t, middleware.NightwatchHandled(c))
}

func TestCompleteVerifiedChallengeClaimsOnceBeforeLoginMint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })

	h := NewHandler(nil, redisClient, logger.New("test"), nil, "test", nil, "test")
	completer := &recordingLoginCompleter{}
	h.SetLoginCompleter(completer)
	claims := &ChallengeClaims{UserID: "test-user", Purpose: PurposeLogin, RegisteredClaims: jwt.RegisteredClaims{ID: "test-challenge-once"}}

	for i := 0; i < 2; i++ {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)
		h.completeVerifiedChallenge(c.Request.Context(), c, claims, PurposeLogin, "totp", false)
		if i == 1 {
			require.Equal(t, http.StatusUnauthorized, response.Code,
				"a claimed challenge must not invoke the login completer a second time")
		}
	}

	require.True(t, completer.called)
	require.Equal(t, 1, completer.calls)
	used, err := redisClient.Get(context.Background(), "mfa_challenge_used:test-challenge-once").Result()
	require.NoError(t, err)
	require.Equal(t, "1", used)
}

func TestCompleteVerifiedChallengeFailsClosedWhenClaimStoreIsUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })

	h := NewHandler(nil, redisClient, logger.New("test"), nil, "test", nil, "test")
	completer := &recordingLoginCompleter{}
	h.SetLoginCompleter(completer)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)
	claims := &ChallengeClaims{UserID: "test-user", Purpose: PurposeLogin, RegisteredClaims: jwt.RegisteredClaims{ID: "test-challenge-store-failure"}}

	h.completeVerifiedChallenge(c.Request.Context(), c, claims, PurposeLogin, "totp", false)

	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.False(t, completer.called, "a challenge that could not be claimed must never mint a session")
}

func TestParseChallengeTokenClassifiesOnlyExpiredTokensAsExpired(t *testing.T) {
	h := NewHandler(nil, nil, logger.New("test"), nil, "test", nil, "test")
	claims := ChallengeClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    ChallengeTokenIssuer,
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Minute)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		},
		Purpose: PurposeLogin,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key -- deterministic test fixture, not a deployed credential
	signed, err := token.SignedString([]byte("test")) //nolint:gosec // test-only JWT secret
	require.NoError(t, err)

	parsed, purpose, expired := h.parseChallengeToken(signed)
	require.Nil(t, parsed)
	require.Empty(t, purpose)
	require.True(t, expired)

	parsed, purpose, expired = h.parseChallengeToken("not-a-challenge-token")
	require.Nil(t, parsed)
	require.Empty(t, purpose)
	require.False(t, expired)

	validUpgrade := ChallengeClaims{
		UserID: "upgrade-user", Purpose: PurposeMFAUpgrade,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: ChallengeTokenIssuer, ID: "upgrade-without-session",
			IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		},
	}
	upgradeToken := jwt.NewWithClaims(jwt.SigningMethodHS256, validUpgrade)
	// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key -- deterministic test fixture, not a deployed credential
	upgradeSigned, err := upgradeToken.SignedString([]byte("test")) //nolint:gosec // test-only JWT secret
	require.NoError(t, err)
	parsed, purpose, expired = h.parseChallengeToken(upgradeSigned)
	require.Nil(t, parsed)
	require.Empty(t, purpose)
	require.False(t, expired)
}

func TestVerifyEmitsClosedInvalidExpiredAndLockedBranches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })

	invoke := func(t *testing.T, token string) []securityevent.Event {
		t.Helper()
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", strings.NewReader(`{"mfa_challenge_token":"`+token+`","method":"totp","code":"000000"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h := NewHandler(nil, redisClient, logger.New("test"), nil, "test", nil, "test")
		recorder := &securityEventRecorder{}
		h.SetSecurityEvents(recorder)
		h.Verify(c)
		return recorder.events
	}

	invalid := invoke(t, "not-a-challenge-token")
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid,
		RouteTemplate: securityevent.RouteAuthMFAVerify,
	}}, invalid)

	expiredClaims := ChallengeClaims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: ChallengeTokenIssuer, ID: "expired", IssuedAt: jwt.NewNumericDate(time.Now().Add(-2 * time.Minute)), ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
	}, UserID: "expired-user", Purpose: PurposeLogin}
	expiredToken := jwt.NewWithClaims(jwt.SigningMethodHS256, expiredClaims)
	// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key -- deterministic test fixture, not a deployed credential
	expiredSigned, err := expiredToken.SignedString([]byte("test")) //nolint:gosec // test-only JWT secret
	require.NoError(t, err)
	expired := invoke(t, expiredSigned)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeExpired,
		RouteTemplate: securityevent.RouteAuthMFAVerify,
	}}, expired)

	issuer := NewHandler(nil, redisClient, logger.New("test"), nil, "test", nil, "test")
	lockedToken, _, err := issuer.GenerateLoginChallenge(context.Background(), "locked-user", false, "", securityevent.AuthPassword)
	require.NoError(t, err)
	require.NoError(t, redisClient.Set(context.Background(), "mfa_verify_lockout:locked-user", "1", time.Minute).Err())
	locked := invoke(t, lockedToken)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeLocked,
		RouteTemplate: securityevent.RouteAuthMFAVerify,
	}}, locked)
}

func TestTOTPConfirmSetupEmitsFactorEnabledOnlyAfterWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/totp/confirm-setup", nil)
		c.Set("user_id", "user")
		return c, response
	}

	failureDB := sql.OpenDB(mfaEventConnector{state: &mfaEventDB{failFirstExec: true}})
	t.Cleanup(func() { require.NoError(t, failureDB.Close()) })
	failureHandler := NewHandler(failureDB, redisClient, logger.New("test"), nil, "test", nil, "test")
	failureRecorder := &securityEventRecorder{}
	failureHandler.SetSecurityEvents(failureRecorder)
	failureContext, failureResponse := newContext()
	failureHandler.TOTPConfirmSetup(failureContext)
	require.Equal(t, http.StatusInternalServerError, failureResponse.Code)
	require.Empty(t, failureRecorder.events, "a failed TOTP activation write must not emit factor_enabled")

	successDB := sql.OpenDB(mfaEventConnector{state: &mfaEventDB{}})
	t.Cleanup(func() { require.NoError(t, successDB.Close()) })
	successHandler := NewHandler(successDB, redisClient, logger.New("test"), nil, "test", nil, "test")
	successRecorder := &securityEventRecorder{}
	successHandler.SetSecurityEvents(successRecorder)
	successContext, successResponse := newContext()
	successHandler.TOTPConfirmSetup(successContext)
	require.Equal(t, http.StatusOK, successResponse.Code)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorEnabled,
		AuthMethod: securityevent.AuthTOTP,
	}}, successRecorder.events)
}

// TestEmailSmsDisableEmitsOnlyAfterFlagsAndDeleteCommit: since B1 the flag
// write is the second statement of EmailSmsDisable's own transaction (after
// the recovery-only update), not a post-commit best-effort sync — so a failed
// flag write is a 500 that rolls everything back and emits nothing. Before B1
// this case (then "...EvenIfFlagSyncFails") pinned a 200 over stale flags.
func TestEmailSmsDisableEmitsOnlyAfterFlagsAndDeleteCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })

	// The step-up gate now requires credentials on every call this test
	// makes, so every fixture needs a real password hash and every request
	// carries the matching password. pragma: allowlist secret -- test credential
	passwordHash, err := auth.HashPasswordWithParams("EmailSmsDisableEventTest1!", &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		// pragma: allowlist secret -- test credential, matches passwordHash above
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/email-sms",
			strings.NewReader(`{"password":"EmailSmsDisableEventTest1!"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("user_id", "user")
		return c, response
	}

	// Exec 2 is the in-transaction flag write.
	failureDB := sql.OpenDB(mfaEventConnector{state: &mfaEventDB{failExecAt: 2, passwordHash: passwordHash}})
	t.Cleanup(func() { require.NoError(t, failureDB.Close()) })
	failureHandler := NewHandler(failureDB, redisClient, logger.New("test"), nil, "test", nil, "test")
	failureRecorder := &securityEventRecorder{}
	failureHandler.SetSecurityEvents(failureRecorder)
	failureContext, failureResponse := newContext()
	failureHandler.EmailSmsDisable(failureContext)
	require.Equal(t, http.StatusInternalServerError, failureResponse.Code, "body: %s", failureResponse.Body.String())
	require.Empty(t, failureRecorder.events, "a failed flag write must not emit factor_disabled")

	successDB := sql.OpenDB(mfaEventConnector{state: &mfaEventDB{passwordHash: passwordHash}})
	t.Cleanup(func() { require.NoError(t, successDB.Close()) })
	successHandler := NewHandler(successDB, redisClient, logger.New("test"), nil, "test", nil, "test")
	successRecorder := &securityEventRecorder{}
	successHandler.SetSecurityEvents(successRecorder)
	successContext, successResponse := newContext()
	successHandler.EmailSmsDisable(successContext)
	require.Equal(t, http.StatusOK, successResponse.Code, "body: %s", successResponse.Body.String())
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorDisabled,
	}}, successRecorder.events)

	// The authoritative delete is the Redis Del of the four email/SMS flag
	// keys, issued from INSIDE the write transaction while the users-row
	// lock is held — not the step-up budget's Redis INCR, which runs before
	// BeginTx and must stay healthy for this request to ever reach the
	// delete at all. A fully-unreachable Redis client can no longer isolate
	// that specific failure: it would fail the budget check first, with its
	// own distinct 429 ReasonRateLimitBackendUnavailable event (covered in
	// settings_stepup_internal_test.go). A DEL-only ProcessHook against a
	// live miniredis is what lets every other call on this request succeed
	// while only the authoritative delete fails.
	delFailRedis := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, delFailRedis.Close()) })
	delFailRedis.AddHook(mfaEventDelFaultHook{})
	downDB := sql.OpenDB(mfaEventConnector{state: &mfaEventDB{passwordHash: passwordHash}})
	t.Cleanup(func() { require.NoError(t, downDB.Close()) })
	downHandler := NewHandler(downDB, delFailRedis, logger.New("test"), nil, "test", nil, "test")
	downRecorder := &securityEventRecorder{}
	downHandler.SetSecurityEvents(downRecorder)
	downContext, downResponse := newContext()
	downHandler.EmailSmsDisable(downContext)
	require.Equal(t, http.StatusServiceUnavailable, downResponse.Code, "body: %s", downResponse.Body.String())
	require.Empty(t, downRecorder.events, "a failed authoritative Redis delete must not emit factor_disabled")
}

// mfaEventDelFaultHook fails every Redis DEL command and passes everything
// else through untouched — in particular the step-up budget's INCR/TTL/
// EXPIRE calls, which must stay healthy so the request reaches the
// authoritative delete this hook exists to fail.
type mfaEventDelFaultHook struct{}

func (mfaEventDelFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (mfaEventDelFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() != "del" {
			return next(ctx, cmd)
		}
		err := errors.New("simulated redis DEL failure")
		cmd.SetErr(err)
		return err
	}
}

func (mfaEventDelFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestEmailSmsVerifyEmitsFactorEnabledOnlyAfterFlagWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/email-sms/verify", strings.NewReader(`{"codes":{"email":"123456"}}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("user_id", "user")
		return c, response
	}
	run := func(t *testing.T, state *mfaEventDB) (*httptest.ResponseRecorder, *securityEventRecorder) {
		t.Helper()
		db := sql.OpenDB(mfaEventConnector{state: state})
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(redisKeyEmailSmsSetup, "user", "email"), "123456", time.Minute).Err())
		h := NewHandler(db, redisClient, logger.New("test"), nil, "test", nil, "test")
		recorder := &securityEventRecorder{}
		h.SetSecurityEvents(recorder)
		c, response := newContext()
		h.EmailSmsVerify(c)
		return response, recorder
	}
	failureResponse, failureRecorder := run(t, &mfaEventDB{failFirstExec: true})
	require.Equal(t, http.StatusInternalServerError, failureResponse.Code)
	require.Empty(t, failureRecorder.events, "a failed MFA flag write must not emit factor_enabled")
	successResponse, successRecorder := run(t, &mfaEventDB{})
	require.Equal(t, http.StatusOK, successResponse.Code)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorEnabled}}, successRecorder.events)
}

func TestWebAuthnDeleteEmitsFactorDisabledOnlyAfterDelete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	passwordHash, err := auth.HashPasswordWithParams("correct", &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Params = gin.Params{{Key: "id", Value: "11111111-2222-3333-4444-555555555555"}}
		c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/auth/mfa/webauthn/credential", strings.NewReader(`{"password":"correct"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("user_id", "user")
		return c, response
	}
	run := func(t *testing.T, state *mfaEventDB) (*httptest.ResponseRecorder, *securityEventRecorder) {
		t.Helper()
		state.passwordHash = passwordHash // pragma: allowlist secret -- test-only password hash
		db := sql.OpenDB(mfaEventConnector{state: state})
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		h := NewHandler(db, redisClient, logger.New("test"), nil, "test", nil, "test")
		recorder := &securityEventRecorder{}
		h.SetSecurityEvents(recorder)
		c, response := newContext()
		h.WebAuthnDeleteCredential(c)
		return response, recorder
	}
	failureResponse, failureRecorder := run(t, &mfaEventDB{failFirstExec: true})
	require.Equal(t, http.StatusInternalServerError, failureResponse.Code)
	require.Empty(t, failureRecorder.events, "a failed WebAuthn delete must not emit factor_disabled")
	successResponse, successRecorder := run(t, &mfaEventDB{})
	require.Equal(t, http.StatusOK, successResponse.Code)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorDisabled, AuthMethod: securityevent.AuthWebAuthn}}, successRecorder.events)
}

func TestTOTPDisableEmitsFactorDisabledOnlyAfterDelete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })

	passwordHash, err := auth.HashPasswordWithParams("credential-fixture", &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	keyring, err := ParseKeyring("0101010101010101010101010101010101010101010101010101010101010101", 1, "")
	require.NoError(t, err)
	secret := "JBSWY3DPEHPK3PXP" //nolint:gosec // test-only TOTP seed // pragma: allowlist secret
	secretEnc, secretNonce, keyVersion, err := keyring.Seal([]byte(secret))
	require.NoError(t, err)
	code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	require.NoError(t, err)

	newContext := func(requestCode string) (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/totp/disable", strings.NewReader(`{"password":"credential-fixture","code":"`+requestCode+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("user_id", "user-fixture")
		return c, response
	}
	run := func(t *testing.T, state *mfaEventDB, requestCode string) (*httptest.ResponseRecorder, *securityEventRecorder, *gin.Context) {
		t.Helper()
		state.passwordHash = passwordHash   // pragma: allowlist secret -- test-only password hash
		state.totpSecretEnc = secretEnc     // pragma: allowlist secret -- test-only sealed TOTP seed
		state.totpSecretNonce = secretNonce // pragma: allowlist secret -- test-only sealed TOTP seed
		state.totpKeyVersion = keyVersion
		state.totpExists = true
		state.totpEnabled = true
		state.totpConfirmed = true
		db := sql.OpenDB(mfaEventConnector{state: state})
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		h := NewHandler(db, redisClient, logger.New("test"), keyring, "test", nil, "test")
		recorder := &securityEventRecorder{}
		h.SetSecurityEvents(recorder)
		c, response := newContext(requestCode)
		h.TOTPDisable(c)
		return response, recorder, c
	}

	failureResponse, failureRecorder, _ := run(t, &mfaEventDB{failFirstExec: true}, code)
	require.Equal(t, http.StatusInternalServerError, failureResponse.Code)
	require.Empty(t, failureRecorder.events, "a failed TOTP delete must not emit factor_disabled")

	successResponse, successRecorder, _ := run(t, &mfaEventDB{}, code)
	require.Equal(t, http.StatusOK, successResponse.Code)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorDisabled,
		AuthMethod: securityevent.AuthTOTP,
	}}, successRecorder.events)
	requireSerializedEventPrivacy(t, successRecorder.events)

	invalidResponse, invalidRecorder, invalidContext := run(t, &mfaEventDB{}, "99999")
	require.Equal(t, http.StatusForbidden, invalidResponse.Code)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid,
	}}, invalidRecorder.events)

	require.True(t, middleware.NightwatchHandled(invalidContext))
}

func TestVerifyTOTPOrBackupEmitsDependencyEventOnVerificationFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := sql.OpenDB(mfaEventConnector{state: &mfaEventDB{}})
	h := NewHandler(db, nil, logger.New("test"), nil, "test", nil, "test")
	recorder := &securityEventRecorder{}
	h.SetSecurityEvents(recorder)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)

	require.NoError(t, db.Close())
	valid, method, responded := h.verifyTOTPOrBackup(c.Request.Context(), c, "123456", "user-fixture")
	require.False(t, valid)
	require.Empty(t, method)
	require.True(t, responded)
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded,
		Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable,
		RouteTemplate: securityevent.RouteAuthMFAVerify,
	}}, recorder.events)
	require.True(t, middleware.NightwatchHandled(c))
}

// TestStepUpGateFailsClosedOnSubjectReadFailure is C2. The request carries the
// CORRECT password and no MFA code, so a gate that read a failed subject or
// factor lookup as "no MFA" would admit it on the password alone. Every arm
// must answer 500 "Verification failed", write nothing, and emit nothing (a
// fault is not a denied credential). The control arm proves the fixture
// reaches the success path when nothing fails, so each fault arm fails for
// its own reason.
func TestStepUpGateFailsClosedOnSubjectReadFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const credential = "GateFailClosedFixture1!" // pragma: allowlist secret -- test credential
	passwordHash, err := auth.HashPasswordWithParams(credential, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })

	emailSmsDisable := func(h *Handler, c *gin.Context) { h.EmailSmsDisable(c) }
	totpSetup := func(h *Handler, c *gin.Context) { h.TOTPSetup(c) }
	for _, tc := range []struct {
		name       string
		state      *mfaEventDB // nil: nothing fails
		call       func(*Handler, *gin.Context)
		wantStatus int
	}{
		{name: "control: nothing fails", call: emailSmsDisable, wantStatus: http.StatusOK},
		{name: "in-transaction gate: users-row lock read fails", state: &mfaEventDB{failLock: true}, call: emailSmsDisable, wantStatus: http.StatusInternalServerError},
		{name: "in-transaction gate: P1 factor read fails", state: &mfaEventDB{failInline: true}, call: emailSmsDisable, wantStatus: http.StatusInternalServerError},
		{name: "pool gate: subject read fails", state: &mfaEventDB{failInline: true}, call: totpSetup, wantStatus: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := tc.state
			if state == nil {
				state = &mfaEventDB{}
			}
			state.passwordHash = passwordHash // pragma: allowlist secret -- test-only password hash
			db := sql.OpenDB(mfaEventConnector{state: state})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			h := NewHandler(db, redisClient, logger.New("test"), nil, "test", nil, "test")
			recorder := &securityEventRecorder{}
			h.SetSecurityEvents(recorder)
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/mfa/step-up", strings.NewReader(`{"password":"`+credential+`"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("user_id", "user-fixture-"+strconv.Itoa(len(tc.name)))

			tc.call(h, c)

			require.Equal(t, tc.wantStatus, response.Code, "body: %s", response.Body.String())
			if tc.wantStatus == http.StatusOK {
				return
			}
			require.JSONEq(t, `{"error":"Verification failed"}`, response.Body.String())
			require.Zero(t, state.execCalls, "a failed subject read must write nothing")
			require.Empty(t, recorder.events, "a server fault is not a denied credential and emits no event")
		})
	}
}

const webAuthnCredentialID = "6Jry73M_WVWDoXLsGxRsBVVHpPWDpNy1ETGXUEvJLdTAn5Ew6nDGU6W8iO3ZkcLEqr-CBwvx0p2WAxzt8RiwQQ" // #nosec G101 -- public WebAuthn test fixture // pragma: allowlist secret

const webAuthnAttestation = "o2NmbXRkbm9uZWdhdHRTdG10oGhhdXRoRGF0YVjEdKbqkhPJnC90siSSsyDPQCYqlMGpUKA5fyklC2CEHvBBAAAAAAAAAAAAAAAAAAAAAAAAAAAAQOia8u9zP1lVg6Fy7BsUbAVVR6T1g6TctRExl1BLyS3UwJ-RMOpwxlOlvIjt2ZHCxKq_ggcL8dKdlgMc7fEYsEGlAQIDJiABIVgg--n_QvZithDycYmnifk6vMHiwBP6kugn2PlsnvkrcSgiWCBAlBYm2B-rMtQlp5MxGTLoGDHoktxb0p364Hy2BH9U2Q" // pragma: allowlist secret -- public WebAuthn test fixture

const webAuthnClientData = "eyJjaGFsbG" +
	"VuZ2UiOiJzVnQ0U2NjZU16cUZTbmZBcThoZ0x6Ymx2bzNmYTRfYUZWRWNJRVNISUowIiwib3JpZ2luIjoiaHR0cHM6Ly93ZWJhdXRobi5pbyIsInR5cGUiOiJ3ZWJhdXRobi5jcmVhdGUifQ" // pragma: allowlist secret -- public WebAuthn test fixture

const webAuthnChallenge = "sVt4ScceMz" +
	"qFSnfAq8hgLzblvo3fa4_aFVEcIESHIJ0" // pragma: allowlist secret -- public WebAuthn test fixture

var webAuthnRegistrationResponse = fmt.Sprintf(`{"id":%q,"rawId":%q,"response":{"attestationObject":%q,"clientDataJSON":%q},"type":"public-key"}`, webAuthnCredentialID, webAuthnCredentialID, webAuthnAttestation, webAuthnClientData)

func TestWebAuthnRegisterFinishEmitsFactorEnabledOnlyAfterCredentialInsert(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	webAuthnService, err := NewWebAuthnService("webauthn.io", "test", []string{"https://webauthn.io"})
	require.NoError(t, err)

	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/webauthn/register/finish", strings.NewReader(webAuthnRegistrationResponse))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("user_id", "user-fixture")
		return c, response
	}
	run := func(t *testing.T, state *mfaEventDB) (*httptest.ResponseRecorder, *securityEventRecorder) {
		t.Helper()
		db := sql.OpenDB(mfaEventConnector{state: state})
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		sessionJSON, err := json.Marshal(webauthn.SessionData{
			Challenge: webAuthnChallenge, RelyingPartyID: "webauthn.io",
			UserID: []byte("user-fixture"), CredParams: webauthn.CredentialParametersDefault(),
		})
		require.NoError(t, err)
		require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(redisKeyWebAuthnReg, "user-fixture"), `{"session":`+strconv.Quote(string(sessionJSON))+`,"credential_name":"credential-fixture","credential_type":"device-fixture"}`, time.Minute).Err())
		h := NewHandler(db, redisClient, logger.New("test"), nil, "test", webAuthnService, "test")
		recorder := &securityEventRecorder{}
		h.SetSecurityEvents(recorder)
		c, response := newContext()
		h.WebAuthnRegisterFinish(c)
		return response, recorder
	}

	failureResponse, failureRecorder := run(t, &mfaEventDB{failFirstExec: true})
	require.Equal(t, http.StatusInternalServerError, failureResponse.Code)
	require.Empty(t, failureRecorder.events, "a failed WebAuthn credential insert must not emit factor_enabled")

	successResponse, successRecorder := run(t, &mfaEventDB{})
	require.Equal(t, http.StatusOK, successResponse.Code)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorEnabled,
		AuthMethod: securityevent.AuthWebAuthn,
	}}, successRecorder.events)
	requireSerializedEventPrivacy(t, successRecorder.events)
}

type mfaEventDB struct {
	failFirstExec bool
	failExecAt    int
	// failLock fails the step-up's first-statement users-row lock read;
	// failInline fails the P1 factor read (and LoadSubject's combined read).
	failLock        bool
	failInline      bool
	execCalls       int
	passwordHash    string
	totpSecretEnc   []byte
	totpSecretNonce []byte
	totpKeyVersion  int
	totpExists      bool
	totpEnabled     bool
	totpConfirmed   bool
	// totpReadDelay holds each read of the TOTP secret, so concurrent
	// requests that reach the code check are all in flight at once.
	totpReadDelay time.Duration
	// recoveryOnly is the user's recovery_only_methods, as a Postgres array
	// literal; empty means none.
	recoveryOnly string
	// mfaMethods is the user's mfa_methods, as a Postgres array literal; empty
	// means several factors, so a restriction on one of them still applies.
	mfaMethods string
	// deleteMatchesNothing makes every DELETE report zero rows.
	deleteMatchesNothing bool
	// statements logs every statement as "<conn> <sql>", plus "<conn> BEGIN"
	// and "<conn> COMMIT", so a test can see which connection ran what.
	statements  []string
	connsOpened int
	mu          sync.Mutex
}

func (s *mfaEventDB) record(conn int, statement string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statements = append(s.statements, fmt.Sprintf("%d %s", conn, statement))
}

// fakeMFADB is a database for a handler test that reaches the user's MFA reads
// but needs no real rows. A nil state answers as an account with no factors.
func fakeMFADB(t *testing.T, state *mfaEventDB) *sql.DB {
	t.Helper()
	if state == nil {
		state = &mfaEventDB{}
	}
	db := sql.OpenDB(mfaEventConnector{state: state})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func (s *mfaEventDB) mfaMethodsArray() string {
	if s.mfaMethods == "" {
		return "{totp,webauthn,email}"
	}
	return s.mfaMethods
}

func (s *mfaEventDB) recoveryOnlyArray() string {
	if s.recoveryOnly == "" {
		return "{}"
	}
	return s.recoveryOnly
}

type mfaEventConnector struct{ state *mfaEventDB }

func (c mfaEventConnector) Connect(context.Context) (driver.Conn, error) {
	c.state.connsOpened++
	return mfaEventConn{state: c.state, id: c.state.connsOpened}, nil
}
func (mfaEventConnector) Driver() driver.Driver { return mfaEventDriver{} }

type mfaEventDriver struct{}

func (mfaEventDriver) Open(string) (driver.Conn, error) { return nil, errors.New("connector required") }

type mfaEventConn struct {
	state *mfaEventDB
	id    int
}

func (mfaEventConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (mfaEventConn) Close() error                        { return nil }
func (mfaEventConn) Begin() (driver.Tx, error)           { return mfaEventTx{}, nil }

// BeginTx makes this fake driver satisfy driver.ConnBeginTx. Without it,
// database/sql's ctxDriverBegin refuses any non-default isolation level
// outright ("driver does not support non-default isolation level") before
// ever falling back to Begin() — and the MFA-settings step-up gate always
// requests sql.LevelReadCommitted. The isolation/read-only options are
// otherwise unused by this fake; it always begins a no-op transaction.
func (mfaEventConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return mfaEventTx{}, nil
}

// mfaEventTx is a no-op driver.Tx: Commit/Rollback do nothing beyond letting
// database/sql mark the *sql.Tx done. Statements issued on the *sql.Tx route
// back through the same mfaEventConn (ExecContext/QueryContext), so the
// shared *mfaEventDB state is still exercised identically whether a call
// happens inside or outside a transaction.
type mfaEventTx struct{}

func (mfaEventTx) Commit() error   { return nil }
func (mfaEventTx) Rollback() error { return nil }

func (c mfaEventConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.state.record(c.id, query)
	c.state.execCalls++
	if (c.state.failFirstExec && c.state.execCalls == 1) || c.state.failExecAt == c.state.execCalls {
		return nil, errors.New("activation write failed")
	}
	if c.state.deleteMatchesNothing && strings.HasPrefix(strings.TrimSpace(query), "DELETE") {
		return driver.RowsAffected(0), nil
	}
	return driver.RowsAffected(1), nil
}
func (c mfaEventConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.state.record(c.id, query)
	switch {
	// The MFA-settings step-up gate's first-statement users-row lock
	// (settings_stepup.go stepUpLockForShareSQL / stepUpLockForNoKeyUpdateSQL)
	// selects credential_epoch and password_hash together. Matched ahead of
	// the single-column "password_hash" case below because that query text
	// also contains "password_hash". A nil epoch scans into an invalid
	// sql.NullString, which credepoch.MatchEpoch treats as "never rotated" —
	// the same meaning a NULL column carries in production.
	case strings.Contains(query, "credential_epoch"):
		if c.state.failLock {
			return nil, errors.New("raw-error-fixture: forced users-row lock failure")
		}
		return &mfaEventRows{values: []driver.Value{nil, c.state.passwordHash}}, nil
	// stepup.LoadSubject's combined read (hash + both P1 EXISTS) — matched
	// ahead of the two cases below, whose substrings it also contains.
	case strings.Contains(query, "FROM users u WHERE u.id"):
		if c.state.failInline {
			return nil, errors.New("raw-error-fixture: forced subject read failure")
		}
		return &mfaEventRows{values: []driver.Value{c.state.passwordHash, false, false}}, nil
	// stepup.InlineMFAMethods' two-column EXISTS query also contains
	// "SELECT EXISTS", so it must be matched ahead of the single-column case
	// below (TOTPDisable's existence probe). None of this file's fixtures need
	// an inline-verifiable factor, so both columns are false.
	case strings.Contains(query, "AND enabled AND confirmed"):
		if c.state.failInline {
			return nil, errors.New("raw-error-fixture: forced inline factor read failure")
		}
		return &mfaEventRows{values: []driver.Value{false, false}}, nil
	case strings.Contains(query, "SELECT EXISTS"):
		return &mfaEventRows{values: []driver.Value{c.state.totpExists}}, nil
	case strings.Contains(query, "password_hash"):
		return &mfaEventRows{values: []driver.Value{c.state.passwordHash}}, nil
	case strings.Contains(query, "totp_secret_enc"):
		time.Sleep(c.state.totpReadDelay)
		return &mfaEventRows{values: []driver.Value{c.state.totpSecretEnc, c.state.totpSecretNonce, int64(c.state.totpKeyVersion), c.state.totpEnabled, c.state.totpConfirmed}}, nil
	case strings.Contains(query, "backup_codes_hash"):
		return &mfaEventRows{values: []driver.Value{[]byte("{}"), []byte("{}")}}, nil
	case strings.Contains(query, "COALESCE(display_name"):
		return &mfaEventRows{values: []driver.Value{"email-fixture@example.test", "user-fixture", "User Fixture"}}, nil
	case strings.Contains(query, "FROM user_mfa_webauthn") && !strings.Contains(query, "COUNT"):
		return mfaEventNoRows{}, nil
	case strings.Contains(query, "enabled, confirmed"):
		return &mfaEventRows{values: []driver.Value{true, false}}, nil
	case strings.Contains(query, "enabled AND confirmed"):
		return &mfaEventRows{values: []driver.Value{true}}, nil
	case strings.Contains(query, "recovery_only_methods FROM users"):
		return &mfaEventRows{values: []driver.Value{[]byte(c.state.mfaMethodsArray()), []byte(c.state.recoveryOnlyArray())}}, nil
	default:
		return &mfaEventRows{values: []driver.Value{int64(0)}}, nil
	}
}

type mfaEventRows struct {
	values []driver.Value
	sent   bool
}

type mfaEventNoRows struct{}

func (mfaEventNoRows) Columns() []string         { return []string{"credential_id"} }
func (mfaEventNoRows) Close() error              { return nil }
func (mfaEventNoRows) Next([]driver.Value) error { return io.EOF }

func (r *mfaEventRows) Columns() []string { return make([]string, len(r.values)) }
func (*mfaEventRows) Close() error        { return nil }
func (r *mfaEventRows) Next(dest []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	copy(dest, r.values)
	r.sent = true
	return nil
}
