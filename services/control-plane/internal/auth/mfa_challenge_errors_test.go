package auth

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// #2450: handleMFAChallenge previously blank-discarded both MFA-method reads. errcheck
// honors an explicit `_`, so the linter never flagged them — the review-only class in
// [internal]rules/backend.md (founding incidents #1142/#1154).
//
// The failure was not cosmetic: on error the response shipped `"methods": []` alongside a
// VALID mfa_challenge_token, so the client rendered an MFA prompt with no selectable
// method, the challenge JTI was burned, and nothing was logged — the user hard-stuck
// mid-login with zero server-side signal.
//
// These tests pin the asymmetry deliberately: loginMethods is load-bearing for the
// response and is FATAL; allMethods only feeds the recovery-only hint and DEGRADES.
//
// In-package (not auth_test) because handleMFAChallenge is unexported; the auth_test
// stubMFAChecker is therefore unreachable and this file carries its own minimal stub.

type mfaMethodsStub struct {
	enabledMethods  []string
	enabledErr      error
	enabledCalls    int
	loginMethods    []string
	loginErr        error
	loginErrAfter   int
	loginCalls      int
	challengeTok    string
	challengeErr    error
	challengeMethod securityevent.AuthMethod
	challengeCalls  int
	upgradeCalls    int
	upgradeErr      error
	upgradeSession  string
}

func (s *mfaMethodsStub) IsEnabled(context.Context, string) bool { return true }
func (s *mfaMethodsStub) GetEnabledMethods(context.Context, string) ([]string, error) {
	s.enabledCalls++
	return s.enabledMethods, s.enabledErr
}
func (s *mfaMethodsStub) GetLoginMethods(context.Context, string) ([]string, error) {
	s.loginCalls++
	if s.loginErr != nil {
		if s.loginErrAfter == 0 || s.loginCalls > s.loginErrAfter {
			return nil, s.loginErr
		}
	}
	return s.loginMethods, nil
}
func (s *mfaMethodsStub) GenerateLoginChallenge(_ context.Context, _ string, _ bool, _ string, primaryAuthMethod securityevent.AuthMethod) (string, string, error) {
	s.challengeCalls++
	s.challengeMethod = primaryAuthMethod
	if s.challengeErr != nil {
		return "", "", s.challengeErr
	}
	return s.challengeTok, "stub-jti", nil
}
func (s *mfaMethodsStub) GenerateUpgradeChallenge(_ context.Context, _, refreshSessionID string) (string, string, error) {
	s.upgradeCalls++
	s.upgradeSession = refreshSessionID
	if s.upgradeErr != nil {
		return "", "", s.upgradeErr
	}
	return s.challengeTok, "stub-jti", nil
}
func (s *mfaMethodsStub) BeginWebAuthnLogin(context.Context, string, string) (interface{}, error) {
	return nil, nil
}
func (s *mfaMethodsStub) GenerateRecoveryToken(string) (string, string, error) {
	return "", "", nil
}
func (s *mfaMethodsStub) ValidateRecoveryToken(string) (*RecoveryClaims, error) { return nil, nil }

func newMFAChallengeRecorder() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	return c, rec
}

// A GetLoginMethods failure must NOT ship a challenge the user cannot answer.
func TestHandleMFAChallengeGetLoginMethodsErrorIsFatal(t *testing.T) {
	stub := &mfaMethodsStub{
		loginErr: errors.New("db unavailable"),
		// A challenge would generate fine — the point is that we never get there.
		challengeTok: "should-not-be-issued",
	}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}

	c, rec := newMFAChallengeRecorder()
	h.handleMFAChallenge(c.Request.Context(), c, "11111111-1111-1111-1111-111111111111", false, "epoch-1", securityevent.AuthPassword)

	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"a GetLoginMethods failure must fail the login, not ship an unanswerable challenge")

	body := rec.Body.String()
	require.NotContains(t, body, "mfa_challenge_token",
		"no challenge token may be issued when the method list could not be read (#2450)")
	require.NotContains(t, body, "should-not-be-issued")
	require.Zero(t, stub.enabledCalls, "the load-bearing login-method lookup must happen before optional enabled methods")
	require.Zero(t, stub.challengeCalls)
}

// buildMFAChallengeResponse is the SHARED builder behind the suspicious-refresh and
// MFA-upgrade challenges. Testing it directly is what stops the #2450 defect from
// reappearing on a third caller: handleMFAChallenge was fixed by hand-rolling its own
// response rather than routing through this helper, which is precisely why one copy got
// fixed and two paths stayed broken.
func TestBuildMFAChallengeResponseLoginMethodsErrorReturnsError(t *testing.T) {
	stub := &mfaMethodsStub{loginErr: errors.New("db unavailable")}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}

	resp, err := h.buildMFAChallengeResponse(context.Background(),
		"suspicious_session_mfa", "Session verification required", "tok", "user-1", "jti-1")

	require.Error(t, err, "a GetLoginMethods failure must surface, not yield a partial response")
	require.Nil(t, resp, "no half-built response may escape — an empty method list beside a "+
		"valid challenge token is the hard-stuck bug (#2450)")
	require.Zero(t, stub.enabledCalls, "the builder must reject a failed login-method read before its optional enabled-method read")
}

// Recovery-only factors are valid for recovery, but cannot answer a login challenge.
// The direct password caller must fall through to CompleteLogin rather than minting an
// unanswerable methods:[] response.
func TestHandleMFAChallengeRecoveryOnlyMethodsFallThrough(t *testing.T) {
	stub := &mfaMethodsStub{
		enabledMethods: []string{"backup_code"},
		challengeTok:   "must-not-be-issued",
	}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}
	c, rec := newMFAChallengeRecorder()

	handled := h.handleMFAChallenge(c.Request.Context(), c, "user-1", false, "epoch-1", securityevent.AuthPassword)

	require.False(t, handled, "the Login caller must proceed to CompleteLogin when no login MFA method exists")
	require.Empty(t, rec.Body.String())
	require.Equal(t, 1, stub.loginCalls)
	require.Zero(t, stub.enabledCalls)
	require.Zero(t, stub.challengeCalls)
}

func TestBuildMFAChallengeResponseRejectsRecoveryOnlyMethods(t *testing.T) {
	stub := &mfaMethodsStub{enabledMethods: []string{"backup_code"}}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}

	resp, err := h.buildMFAChallengeResponse(context.Background(), "mfa_upgrade_required", "Verify", "tok", "user-1", "jti-1")

	require.Error(t, err)
	require.Nil(t, resp)
	require.Equal(t, 1, stub.loginCalls)
	require.Zero(t, stub.enabledCalls, "no recovery-only hint is needed after rejecting the response")
}

func TestBuildMFAChallengeResponseSucceedsWithMethods(t *testing.T) {
	stub := &mfaMethodsStub{
		loginMethods:   []string{"totp"},
		enabledMethods: []string{"totp", "recovery_code"},
	}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}

	resp, err := h.buildMFAChallengeResponse(context.Background(),
		"mfa_upgrade_required", "Verify your identity", "tok-2", "user-2", "jti-2")

	require.NoError(t, err)
	require.Equal(t, "tok-2", resp["mfa_challenge_token"])
	require.Equal(t, []string{"totp"}, resp["methods"])
	require.Equal(t, []string{"recovery_code"}, resp["recovery_only_methods"],
		"a method enabled but not login-eligible must surface as recovery-only")
}

// A GetEnabledMethods failure only costs the recovery-only hint; the login proceeds.
func TestHandleMFAChallengeGetEnabledMethodsErrorDegradesHint(t *testing.T) {
	stub := &mfaMethodsStub{
		enabledErr:   errors.New("db unavailable"),
		loginMethods: []string{"totp"},
		challengeTok: "challenge-token-abc",
	}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}

	c, rec := newMFAChallengeRecorder()
	h.handleMFAChallenge(c.Request.Context(), c, "22222222-2222-2222-2222-222222222222", false, "epoch-1", securityevent.AuthPassword)

	require.Equal(t, http.StatusOK, rec.Code,
		"an allMethods failure must not block a login that still has a usable method list")

	body := rec.Body.String()
	require.Contains(t, body, "challenge-token-abc", "the challenge must still be issued")
	require.Contains(t, body, "totp", "the selectable method list must still be present")
	require.NotContains(t, body, "recovery_only_methods",
		"the recovery-only hint degrades to absent when the superset read fails")
}

func TestHandleMFAChallengeEmitsChallengeRequiredOnlyAfterIssuance(t *testing.T) {
	for _, test := range []struct {
		name         string
		challengeErr error
		wantEvents   []securityevent.Event
	}{
		{
			name: "issued",
			wantEvents: []securityevent.Event{{
				EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess,
				Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonChallengeRequired,
				AuthMethod: securityevent.AuthPassword, RouteTemplate: securityevent.RouteAuthLogin,
			}},
		},
		{name: "issuer failure", challengeErr: errors.New("issuer unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &mfaMethodsStub{enabledMethods: []string{"totp"}, loginMethods: []string{"totp"}, challengeTok: "issued-token", challengeErr: test.challengeErr}
			h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}
			recorder := &securityEventRecorder{}
			h.SetSecurityEvents(recorder)
			c, response := newMFAChallengeRecorder()

			h.handleMFAChallenge(c.Request.Context(), c, "33333333-3333-3333-3333-333333333333", false, "epoch-1", securityevent.AuthPassword)

			if test.challengeErr != nil {
				require.Equal(t, http.StatusInternalServerError, response.Code)
				require.False(t, middleware.NightwatchHandled(c))
			} else {
				require.True(t, middleware.NightwatchHandled(c))
			}
			require.Equal(t, test.wantEvents, recorder.events)
			require.Equal(t, securityevent.AuthPassword, stub.challengeMethod)
		})
	}
}

// A same-IP, different-machine refresh is suspicious enough that every MFA
// dependency failure must block rotation. Returning false here would let the
// caller mint a fresh session without satisfying the challenge.
func TestSuspiciousMachineMFAFailuresFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	epochDB := sql.OpenDB(suspiciousEpochConnector{})
	t.Cleanup(func() { require.NoError(t, epochDB.Close()) })

	for _, test := range []struct {
		name    string
		db      *sql.DB
		checker *mfaMethodsStub
	}{
		{
			name:    "enabled method lookup error",
			checker: &mfaMethodsStub{loginMethods: []string{"totp"}, enabledErr: errors.New("MFA methods unavailable")},
		},
		{
			name: "challenge issuance error",
			db:   epochDB,
			checker: &mfaMethodsStub{
				enabledMethods: []string{"totp"},
				loginMethods:   []string{"totp"},
				challengeErr:   errors.New("challenge issuer unavailable"),
			},
		},
		{
			name: "challenge response error",
			db:   epochDB,
			checker: &mfaMethodsStub{
				enabledMethods: []string{"totp"},
				loginMethods:   []string{"totp"},
				challengeTok:   "challenge-token",
				loginErr:       errors.New("MFA methods unavailable"),
				loginErrAfter:  1,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := &Handler{db: test.db, mfaChecker: test.checker, log: logger.NewWithWriter(io.Discard)}
			recorder := &securityEventRecorder{}
			h.SetSecurityEvents(recorder)
			c, response := newMFAChallengeRecorder()

			blocked := h.handleSuspiciousMachineID(c, models.RefreshToken{
				UserID: "user-fixture", MachineID: "stored-machine", RememberMe: true,
			}, "new-machine")

			require.True(t, blocked, "dependency failures must prevent refresh-token rotation")
			require.Equal(t, http.StatusInternalServerError, response.Code)
			require.Equal(t, []securityevent.Event{{
				EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded,
				Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable,
				AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteAuthRefresh,
			}}, recorder.events)
			require.True(t, middleware.NightwatchHandled(c))
		})
	}
}

func TestSuspiciousMachineRecoveryOnlyMethodsAllowRefresh(t *testing.T) {
	stub := &mfaMethodsStub{enabledMethods: []string{"backup_code"}, challengeTok: "must-not-be-issued"}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}
	c, rec := newMFAChallengeRecorder()

	blocked := h.handleSuspiciousMachineID(c, models.RefreshToken{UserID: "user-fixture", MachineID: "stored-machine"}, "new-machine")

	require.False(t, blocked, "the refresh caller must proceed when only recovery factors exist")
	require.Empty(t, rec.Body.String())
	require.Equal(t, 1, stub.loginCalls)
	require.Zero(t, stub.enabledCalls)
	require.Zero(t, stub.challengeCalls)
}

func TestSuspiciousMachineChallengeEmitsSuccessEvent(t *testing.T) {
	epochDB := sql.OpenDB(suspiciousEpochConnector{})
	t.Cleanup(func() { require.NoError(t, epochDB.Close()) })
	stub := &mfaMethodsStub{loginMethods: []string{"totp"}, enabledMethods: []string{"totp"}, challengeTok: "challenge-token"}
	h := &Handler{db: epochDB, mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}
	recorder := &securityEventRecorder{}
	h.SetSecurityEvents(recorder)
	c, response := newMFAChallengeRecorder()

	require.True(t, h.handleSuspiciousMachineID(c, models.RefreshToken{UserID: "user-fixture", MachineID: "stored-machine"}, "new-machine"))
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonChallengeRequired,
		AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteAuthRefresh,
	}}, recorder.events)
	require.True(t, middleware.NightwatchHandled(c))
}

func TestPreMFASessionRecoveryOnlyMethodsNeedNoUpgrade(t *testing.T) {
	stub := &mfaMethodsStub{enabledMethods: []string{"backup_code"}}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}
	c, rec := newMFAChallengeRecorder()

	blocked := h.checkPreMFASessionLock(c, models.RefreshToken{UserID: "user-fixture"})

	require.False(t, blocked, "a pre-MFA session cannot complete an upgrade challenge without a login MFA method")
	require.Empty(t, rec.Body.String())
	require.Equal(t, 1, stub.loginCalls)
	require.Zero(t, stub.enabledCalls)
	require.Zero(t, stub.upgradeCalls)
}

func TestPreMFASessionDependencyFailuresBlockRefresh(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })

	missingDB := sql.OpenDB(noRowsConnector{})
	t.Cleanup(func() { require.NoError(t, missingDB.Close()) })
	enabledAt := time.Now().UTC()
	enabledDB := sql.OpenDB(mfaEnabledAtConnector{enabledAt: enabledAt})
	t.Cleanup(func() { require.NoError(t, enabledDB.Close()) })

	for _, test := range []struct {
		name    string
		db      *sql.DB
		checker *mfaMethodsStub
	}{
		{name: "login method lookup", checker: &mfaMethodsStub{loginErr: errors.New("method store unavailable")}},
		{name: "enablement timestamp lookup", db: missingDB, checker: &mfaMethodsStub{loginMethods: []string{"totp"}}},
		{name: "challenge issuance", db: enabledDB, checker: &mfaMethodsStub{loginMethods: []string{"totp"}, upgradeErr: errors.New("challenge store unavailable")}},
		{name: "challenge response", db: enabledDB, checker: &mfaMethodsStub{loginMethods: []string{"totp"}, loginErr: errors.New("method store unavailable"), loginErrAfter: 1, challengeTok: "challenge-token"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := &Handler{db: test.db, redis: rdb, mfaChecker: test.checker, log: logger.NewWithWriter(io.Discard)}
			recorder := &securityEventRecorder{}
			h.SetSecurityEvents(recorder)
			c, response := newMFAChallengeRecorder()

			blocked := h.checkPreMFASessionLock(c, models.RefreshToken{UserID: "user-fixture", CreatedAt: enabledAt.Add(-time.Hour)})

			require.True(t, blocked, "dependency failures must prevent refresh-token rotation")
			require.Equal(t, http.StatusInternalServerError, response.Code)
			require.Equal(t, []securityevent.Event{{
				EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded,
				Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable,
				AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteAuthRefresh,
			}}, recorder.events)
			require.True(t, middleware.NightwatchHandled(c))
		})
	}
}

func TestPreMFASessionBypassIsSessionBoundAndConsumedOnce(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	const sessionID = "session-fixture"
	require.NoError(t, rdb.Set(context.Background(), MFAUpgradeBypassKey("user-fixture", sessionID), "1", time.Minute).Err())

	enabledAt := time.Now().UTC()
	db := sql.OpenDB(mfaEnabledAtConnector{enabledAt: enabledAt})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	stub := &mfaMethodsStub{loginMethods: []string{"totp"}, enabledMethods: []string{"totp"}, challengeTok: "challenge-token"}
	h := &Handler{db: db, redis: rdb, mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}
	token := models.RefreshToken{ID: sessionID, UserID: "user-fixture", CreatedAt: enabledAt.Add(-time.Hour)}

	attacker, attackerResponse := newMFAChallengeRecorder()
	attackerToken := token
	attackerToken.ID = "other-session"
	require.True(t, h.checkPreMFASessionLock(attacker, attackerToken))
	require.Equal(t, http.StatusForbidden, attackerResponse.Code)

	first, firstResponse := newMFAChallengeRecorder()
	require.False(t, h.checkPreMFASessionLock(first, token))
	require.Empty(t, firstResponse.Body.String())

	second, secondResponse := newMFAChallengeRecorder()
	secondRecorder := &securityEventRecorder{}
	h.SetSecurityEvents(secondRecorder)
	require.True(t, h.checkPreMFASessionLock(second, token))
	require.Equal(t, http.StatusForbidden, secondResponse.Code)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonChallengeRequired,
		AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteAuthRefresh,
	}}, secondRecorder.events)
	require.True(t, middleware.NightwatchHandled(second))
	require.Equal(t, 2, stub.upgradeCalls, "the wrong session and the second use must each require MFA")
	require.Equal(t, sessionID, stub.upgradeSession)
}

type mfaEnabledAtConnector struct{ enabledAt time.Time }

func (c mfaEnabledAtConnector) Connect(context.Context) (driver.Conn, error) {
	return mfaEnabledAtConn(c), nil
}
func (c mfaEnabledAtConnector) Driver() driver.Driver { return mfaEnabledAtDriver(c) }

type mfaEnabledAtDriver mfaEnabledAtConnector

func (d mfaEnabledAtDriver) Open(string) (driver.Conn, error) {
	return mfaEnabledAtConn(d), nil
}

type mfaEnabledAtConn struct{ enabledAt time.Time }

func (c mfaEnabledAtConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c mfaEnabledAtConn) Close() error                        { return nil }
func (c mfaEnabledAtConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c mfaEnabledAtConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &mfaEnabledAtRows{enabledAt: c.enabledAt}, nil
}

type mfaEnabledAtRows struct {
	enabledAt time.Time
	returned  bool
}

func (r *mfaEnabledAtRows) Columns() []string { return []string{"mfa_enabled_at"} }
func (r *mfaEnabledAtRows) Close() error      { return nil }
func (r *mfaEnabledAtRows) Next(dest []driver.Value) error {
	if r.returned {
		return io.EOF
	}
	r.returned = true
	dest[0] = r.enabledAt
	return nil
}

// suspiciousEpochConnector returns the one credential-epoch row required by
// the suspicious-refresh path without depending on a PostgreSQL fixture.
type suspiciousEpochConnector struct{}

func (suspiciousEpochConnector) Connect(context.Context) (driver.Conn, error) {
	return suspiciousEpochConn{}, nil
}
func (suspiciousEpochConnector) Driver() driver.Driver { return suspiciousEpochDriver{} }

type suspiciousEpochDriver struct{}

func (suspiciousEpochDriver) Open(string) (driver.Conn, error) { return suspiciousEpochConn{}, nil }

type suspiciousEpochConn struct{}

func (suspiciousEpochConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (suspiciousEpochConn) Close() error                        { return nil }
func (suspiciousEpochConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (suspiciousEpochConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &suspiciousEpochRows{}, nil
}

type suspiciousEpochRows struct{ returned bool }

func (r *suspiciousEpochRows) Columns() []string { return []string{"credential_epoch"} }
func (r *suspiciousEpochRows) Close() error      { return nil }
func (r *suspiciousEpochRows) Next(dest []driver.Value) error {
	if r.returned {
		return io.EOF
	}
	r.returned = true
	dest[0] = "epoch-fixture"
	return nil
}
