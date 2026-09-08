package sessions

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type securityEventRecorder struct{ events []securityevent.Event }

const (
	privacyUserID    = "user-7e4c92"
	privacySessionID = "session-6f4ab1"
	privacyPassword  = "correct-horse-battery-staple" // pragma: allowlist secret -- deterministic test-only password fixture
)

func (r *securityEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
}

func requireSerializedEventPrivacy(t *testing.T, events []securityevent.Event, sensitiveValues ...string) {
	t.Helper()
	require.NotEmpty(t, events)
	serialized, err := json.Marshal(events)
	require.NoError(t, err)
	for _, fixture := range append(sensitiveValues, []string{
		"198.51.100.44", "email-fixture@example.test", "raw-error-fixture",
	}...) {
		require.NotContains(t, string(serialized), fixture)
	}
}

func TestExecuteRevocationEmitsOnlyAfterDurableUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })

	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(response)
		ginContext.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/session", nil)
		return ginContext, response
	}

	failureDB := sql.OpenDB(execConnector{err: errors.New("commit failed")})
	t.Cleanup(func() { require.NoError(t, failureDB.Close()) })
	failureHandler := NewHandler(failureDB, redisClient, logger.New("test"), testDisconnector{}, nil)
	failureRecorder := &securityEventRecorder{}
	failureHandler.SetSecurityEvents(failureRecorder)
	failureContext, failureResponse := newContext()
	failureHandler.executeRevocation(context.Background(), failureContext, privacyUserID, privacySessionID, false)
	require.Equal(t, http.StatusInternalServerError, failureResponse.Code)
	require.Empty(t, failureRecorder.events)

	successDB := sql.OpenDB(execConnector{rows: 1})
	t.Cleanup(func() { require.NoError(t, successDB.Close()) })
	successHandler := NewHandler(successDB, redisClient, logger.New("test"), testDisconnector{}, nil)
	successRecorder := &securityEventRecorder{}
	successHandler.SetSecurityEvents(successRecorder)
	successContext, successResponse := newContext()
	successHandler.executeRevocation(context.Background(), successContext, privacyUserID, privacySessionID, false)
	require.Equal(t, http.StatusOK, successResponse.Code)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonSessionRevoked, AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteSessionDelete}}, successRecorder.events)
	requireSerializedEventPrivacy(t, successRecorder.events, privacyUserID, privacySessionID)
}

func TestUpdateRevocationModeEmitsOnlyAfterDurableUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	passwordHash, err := auth.HashPasswordWithParams(privacyPassword, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)

	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(response)
		ginContext.Request = httptest.NewRequest(http.MethodPut, "/api/v1/sessions/revocation-mode", strings.NewReader(`{"mode":"secure","password":"`+privacyPassword+`"}`))
		ginContext.Request.Header.Set("Content-Type", "application/json")
		ginContext.Set("user_id", privacyUserID)
		return ginContext, response
	}

	failureDB := sql.OpenDB(execConnector{err: errors.New("update failed"), passwordHash: passwordHash})
	t.Cleanup(func() { require.NoError(t, failureDB.Close()) })
	failureHandler := NewHandler(failureDB, redisClient, logger.New("test"), testDisconnector{}, nil)
	failureRecorder := &securityEventRecorder{}
	failureHandler.SetSecurityEvents(failureRecorder)
	failureContext, failureResponse := newContext()
	failureHandler.UpdateRevocationMode(failureContext)
	require.Equal(t, http.StatusInternalServerError, failureResponse.Code)
	require.Empty(t, failureRecorder.events)

	successDB := sql.OpenDB(execConnector{rows: 1, passwordHash: passwordHash})
	t.Cleanup(func() { require.NoError(t, successDB.Close()) })
	successHandler := NewHandler(successDB, redisClient, logger.New("test"), testDisconnector{}, nil)
	successRecorder := &securityEventRecorder{}
	successHandler.SetSecurityEvents(successRecorder)
	successContext, successResponse := newContext()
	successHandler.UpdateRevocationMode(successContext)
	require.Equal(t, http.StatusOK, successResponse.Code)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonRevocationModeChanged, AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteSessionsRevocationMode}}, successRecorder.events)
	requireSerializedEventPrivacy(t, successRecorder.events, privacyUserID, privacyPassword)
}

func TestRevokeAllSessionsEmitsOnlyAfterCommittedBulkRevoke(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	passwordHash, err := auth.HashPasswordWithParams(privacyPassword, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)

	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/revoke-all", strings.NewReader(`{"password":"`+privacyPassword+`","include_current":true}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("user_id", privacyUserID)
		return c, response
	}

	failureDB := sql.OpenDB(execConnector{passwordHash: passwordHash, rows: 1, commitErr: errors.New("bulk revoke commit failed")})
	t.Cleanup(func() { require.NoError(t, failureDB.Close()) })
	failureHandler := NewHandler(failureDB, redisClient, logger.New("test"), testDisconnector{}, nil)
	failureRecorder := &securityEventRecorder{}
	failureHandler.SetSecurityEvents(failureRecorder)
	failureContext, failureResponse := newContext()
	failureHandler.RevokeAllSessions(failureContext)
	require.Equal(t, http.StatusInternalServerError, failureResponse.Code)
	require.Empty(t, failureRecorder.events, "a failed bulk-revoke commit must not emit sessions_revoked")

	successDB := sql.OpenDB(execConnector{passwordHash: passwordHash, rows: 1})
	t.Cleanup(func() { require.NoError(t, successDB.Close()) })
	successHandler := NewHandler(successDB, redisClient, logger.New("test"), testDisconnector{}, nil)
	successRecorder := &securityEventRecorder{}
	successHandler.SetSecurityEvents(successRecorder)
	successContext, successResponse := newContext()
	successHandler.RevokeAllSessions(successContext)
	require.Equal(t, http.StatusOK, successResponse.Code)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonSessionsRevoked,
		AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteSessionsRevokeAll,
	}}, successRecorder.events)
	requireSerializedEventPrivacy(t, successRecorder.events, privacyUserID, privacyPassword)
}

type revokeMFAVerifier struct{ enabled bool }

func (v revokeMFAVerifier) IsEnabled(context.Context, string) bool { return v.enabled }
func (revokeMFAVerifier) VerifyCode(context.Context, string, string) (bool, error) {
	return false, nil
}
func (revokeMFAVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	return []string{"totp"}, nil
}

func TestAuthenticateForRevokeEmitsCredentialSpecificRefusalEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	passwordHash, err := auth.HashPasswordWithParams(privacyPassword, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)

	for _, test := range []struct {
		name  string
		route securityevent.RouteTemplate
	}{
		{name: "delete", route: securityevent.RouteSessionDelete},
		{name: "revoke all", route: securityevent.RouteSessionsRevokeAll},
		{name: "revocation mode", route: securityevent.RouteSessionsRevocationMode},
	} {
		t.Run(test.name, func(t *testing.T) {
			newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
				response := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(response)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
				return c, response
			}

			db := sql.OpenDB(execConnector{passwordHash: passwordHash})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			passwordHandler := NewHandler(db, nil, logger.New("test"), testDisconnector{}, revokeMFAVerifier{})
			passwordRecorder := &securityEventRecorder{}
			passwordHandler.SetSecurityEvents(passwordRecorder)
			passwordContext, passwordResponse := newContext()
			require.True(t, passwordHandler.authenticateForRevoke(context.Background(), passwordContext, privacyUserID, "wrong", "", "revoke", test.route))
			require.Equal(t, http.StatusForbidden, passwordResponse.Code)
			require.Equal(t, []securityevent.Event{{
				EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied,
				Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials,
				AuthMethod: securityevent.AuthPassword, RouteTemplate: test.route,
			}}, passwordRecorder.events)
			require.True(t, middleware.NightwatchHandled(passwordContext))

			mfaHandler := NewHandler(db, nil, logger.New("test"), testDisconnector{}, revokeMFAVerifier{enabled: true})
			mfaRecorder := &securityEventRecorder{}
			mfaHandler.SetSecurityEvents(mfaRecorder)
			mfaContext, mfaResponse := newContext()
			require.True(t, mfaHandler.authenticateForRevoke(context.Background(), mfaContext, privacyUserID, "", "bad-code", "revoke", test.route))
			require.Equal(t, http.StatusForbidden, mfaResponse.Code)
			require.Equal(t, []securityevent.Event{{
				EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied,
				Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid,
				RouteTemplate: test.route,
			}}, mfaRecorder.events)
			require.True(t, middleware.NightwatchHandled(mfaContext))

			noCredentialContext, noCredentialResponse := newContext()
			require.True(t, mfaHandler.authenticateForRevoke(context.Background(), noCredentialContext, privacyUserID, "", "", "revoke", test.route))
			require.Equal(t, http.StatusForbidden, noCredentialResponse.Code)
			require.False(t, middleware.NightwatchHandled(noCredentialContext))
		})
	}
}

type testDisconnector struct{}

func (testDisconnector) DisconnectUser(uuid.UUID) {}
func (testDisconnector) DisconnectSession(string) {}

type execConnector struct {
	err          error
	commitErr    error
	rows         int64
	passwordHash string
}

func (c execConnector) Connect(context.Context) (driver.Conn, error) {
	return execConn{connector: c}, nil
}
func (c execConnector) Driver() driver.Driver { return execDriver{connector: c} }

type execDriver struct{ connector execConnector }

func (d execDriver) Open(string) (driver.Conn, error) { return execConn(d), nil }

type execConn struct{ connector execConnector }

func (c execConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c execConn) Close() error                        { return nil }
func (c execConn) Begin() (driver.Tx, error)           { return execTx(c), nil }
func (c execConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	if c.connector.err != nil {
		return nil, c.connector.err
	}
	return driver.RowsAffected(c.connector.rows), nil
}

func (c execConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "credential_epoch") {
		return &oneSessionValueRow{value: nil}, nil
	}
	if c.connector.passwordHash == "" {
		return noSessionRows{}, nil
	}
	return &oneSessionStringRow{value: c.connector.passwordHash}, nil
}

type execTx struct{ connector execConnector }

func (tx execTx) Commit() error { return tx.connector.commitErr }
func (execTx) Rollback() error  { return nil }

type noSessionRows struct{}

func (noSessionRows) Columns() []string         { return []string{"password_hash"} }
func (noSessionRows) Close() error              { return nil }
func (noSessionRows) Next([]driver.Value) error { return io.EOF }

type oneSessionStringRow struct {
	value string
	sent  bool
}

type oneSessionValueRow struct {
	value any
	sent  bool
}

func (r oneSessionValueRow) Columns() []string { return []string{"credential_epoch"} }
func (oneSessionValueRow) Close() error        { return nil }
func (r *oneSessionValueRow) Next(dest []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	dest[0] = r.value
	r.sent = true
	return nil
}

func (r oneSessionStringRow) Columns() []string { return []string{"password_hash"} }
func (r oneSessionStringRow) Close() error      { return nil }
func (r *oneSessionStringRow) Next(dest []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	dest[0] = r.value
	r.sent = true
	return nil
}
