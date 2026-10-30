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

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
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

// Both arms use the same live family: the successful retry cannot be an
// idempotent no-op if the failing commit accidentally persisted its writes.
func TestExecuteRevocationEmitsOnlyAfterDurableUpdate(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	rig.router.DELETE("/api/v1/sessions/:id", func(c *gin.Context) {
		rig.handler.executeRevocation(c.Request.Context(), c, c.GetString("user_id"), c.Param("id"))
	})
	removeFault := installSecurityEventCommitFault(t, rig.db)
	failed := rig.request(http.MethodDelete, "/api/v1/sessions/"+rig.sid, "")
	require.Equal(t, http.StatusServiceUnavailable, failed.Code, failed.Body.String())
	require.JSONEq(t, `{"error":"Authentication temporarily unavailable"}`, failed.Body.String())
	rig.requireFailedCommit(t, failed)
	tracking, err := rig.redis.Exists(context.Background(), revokeTrackingKey(rig.user), simpleAuthWindowKey(rig.user)).Result()
	require.NoError(t, err)
	require.Zero(t, tracking)
	removeFault()
	succeeded := rig.request(http.MethodDelete, "/api/v1/sessions/"+rig.sid, "")
	require.Equal(t, http.StatusOK, succeeded.Code, succeeded.Body.String())
	require.JSONEq(t, `{"message":"Session revoked successfully","session_id":"`+rig.sid+`"}`, succeeded.Body.String())
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonSessionRevoked, AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteSessionDelete}}, rig.recorder.events)
	rig.requireCommittedOwner(t)
	requireSerializedEventPrivacy(t, rig.recorder.events, rig.user, rig.sid, rig.access, rig.refresh)
	members, err := rig.redis.ZRange(context.Background(), revokeTrackingKey(rig.user), 0, -1).Result()
	require.NoError(t, err)
	require.Equal(t, []string{rig.sid}, members)
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
	rig := newSecurityEventFamilyRig(t)
	rig.router.POST("/api/v1/sessions/revoke-all", rig.handler.RevokeAllSessions)
	ctx := context.Background()
	require.NoError(t, rig.redis.ZAdd(ctx, revokeTrackingKey(rig.user), redis.Z{Score: 1, Member: "existing-revoke"}).Err())
	require.NoError(t, rig.redis.Set(ctx, simpleAuthWindowKey(rig.user), "1", 0).Err())
	removeFault := installSecurityEventCommitFault(t, rig.db)
	body := `{"password":"` + privacyPassword + `","include_current":true}`
	failed := rig.request(http.MethodPost, "/api/v1/sessions/revoke-all", body)
	require.Equal(t, http.StatusServiceUnavailable, failed.Code, failed.Body.String())
	require.JSONEq(t, `{"error":"Authentication temporarily unavailable"}`, failed.Body.String())
	rig.requireFailedCommit(t, failed)
	members, err := rig.redis.ZRange(ctx, revokeTrackingKey(rig.user), 0, -1).Result()
	require.NoError(t, err)
	require.Equal(t, []string{"existing-revoke"}, members)
	window, err := rig.redis.Get(ctx, simpleAuthWindowKey(rig.user)).Result()
	require.NoError(t, err)
	require.Equal(t, "1", window)
	removeFault()
	succeeded := rig.request(http.MethodPost, "/api/v1/sessions/revoke-all", body)
	require.Equal(t, http.StatusOK, succeeded.Code, succeeded.Body.String())
	require.JSONEq(t, `{"message":"All sessions revoked successfully","count":1,"include_current":true}`, succeeded.Body.String())
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonSessionsRevoked, AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteSessionsRevokeAll}}, rig.recorder.events)
	rig.requireCommittedOwner(t)
	requireSerializedEventPrivacy(t, rig.recorder.events, rig.user, rig.sid, privacyPassword, rig.access, rig.refresh)
	remaining, err := rig.redis.Exists(ctx, revokeTrackingKey(rig.user), simpleAuthWindowKey(rig.user)).Result()
	require.NoError(t, err)
	require.Zero(t, remaining)
	cookies := succeeded.Result().Cookies()
	require.Len(t, cookies, 1)
	require.Equal(t, "refresh_token", cookies[0].Name)
	require.Equal(t, -1, cookies[0].MaxAge)
}

type securityEventDisconnector struct {
	users    []uuid.UUID
	sessions []string
	families []uuid.UUID
}

func (d *securityEventDisconnector) DisconnectFamilyContext(_ context.Context, _, family uuid.UUID) error {
	d.families = append(d.families, family)
	return nil
}

func (d *securityEventDisconnector) DisconnectUser(user uuid.UUID) { d.users = append(d.users, user) }
func (d *securityEventDisconnector) DisconnectSession(sid string) {
	d.sessions = append(d.sessions, sid)
}

type securityEventFamilyRig struct {
	db                         *sql.DB
	redis                      *redis.Client
	router                     *gin.Engine
	handler                    *Handler
	recorder                   *securityEventRecorder
	disconnector               *securityEventDisconnector
	user, sid, access, refresh string
}

func newSecurityEventFamilyRig(t *testing.T) securityEventFamilyRig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, _ := dbtest.SetupTestDB(t)
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	user := dbtest.CreateUser(t, db).String()
	hash, err := auth.HashPasswordWithParams(privacyPassword, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE users SET password_hash=$2 WHERE id=$1`, user, hash)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO user_keys(user_id,wrapped_private_key,key_derivation_salt,key_version,key_derivation_alg) VALUES($1,$2,$3,1,'argon2id')`, user, []byte{1}, []byte{2})
	require.NoError(t, err)
	const secret = "security-event-family-test-signing-secret"
	minter := auth.NewHandler(db, rdb, logger.New("test"), secret, testDisconnector{})
	access, refresh, sid, err := minter.IssueAccessAndRefresh(context.Background(), user)
	require.NoError(t, err)
	disconnector := &securityEventDisconnector{}
	handler := NewHandler(db, rdb, logger.New("test"), disconnector, nil)
	recorder := &securityEventRecorder{}
	handler.SetSecurityEvents(recorder)
	router := gin.New()
	router.Use(middleware.AuthRequired(secret, rdb, db, nil))
	return securityEventFamilyRig{db: db, redis: rdb, router: router, handler: handler, recorder: recorder, disconnector: disconnector, user: user, sid: sid, access: access, refresh: refresh}
}

func (r securityEventFamilyRig) request(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.access)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: r.refresh, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	response := httptest.NewRecorder()
	r.router.ServeHTTP(response, req)
	return response
}

// A deferred trigger runs at COMMIT after the refresh and family mutations.
// Sequence advancement survives rollback and proves this exact fault was reached.
func installSecurityEventCommitFault(t *testing.T, db *sql.DB) func() {
	t.Helper()
	_, err := db.Exec(`CREATE SEQUENCE security_event_commit_fault_seq;
 CREATE FUNCTION security_event_commit_fault() RETURNS trigger AS $$
 BEGIN
  IF NOT EXISTS (SELECT 1 FROM login_families f JOIN login_family_revocations o ON o.operation_id=f.auth_operation_id
    JOIN refresh_tokens r ON r.user_id=f.user_id JOIN login_family_sids s ON s.sid=r.id AND s.family_id=f.family_id
    WHERE f.family_id=NEW.family_id AND f.terminal_at IS NOT NULL AND o.sealed_at IS NOT NULL AND r.revoked_at IS NOT NULL) THEN
   RAISE EXCEPTION 'commit fault reached before complete family mutation';
  END IF;
  PERFORM nextval('security_event_commit_fault_seq');
  RAISE EXCEPTION 'raw-error-fixture';
 END; $$ LANGUAGE plpgsql;
 CREATE CONSTRAINT TRIGGER security_event_commit_fault AFTER INSERT ON login_family_revocation_targets
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION security_event_commit_fault()`)
	require.NoError(t, err)
	removed := false
	remove := func() {
		if removed {
			return
		}
		_, err := db.Exec(`DROP TRIGGER security_event_commit_fault ON login_family_revocation_targets; DROP FUNCTION security_event_commit_fault(); DROP SEQUENCE security_event_commit_fault_seq`)
		require.NoError(t, err)
		removed = true
	}
	t.Cleanup(remove)
	return remove
}

func (r securityEventFamilyRig) requireFailedCommit(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	var called bool
	require.NoError(t, r.db.QueryRow(`SELECT is_called FROM security_event_commit_fault_seq`).Scan(&called))
	require.True(t, called, "must reach deferred commit fault after family and refresh mutations")
	require.Empty(t, r.recorder.events)
	require.Empty(t, r.disconnector.users)
	require.Empty(t, r.disconnector.sessions)
	require.Empty(t, r.disconnector.families, "a failed commit must not fence any family")
	require.Empty(t, response.Header().Values("Set-Cookie"))
	var count int
	for _, query := range []string{
		`SELECT count(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NOT NULL`,
		`SELECT count(*) FROM login_families WHERE user_id=$1 AND (terminal_at IS NOT NULL OR auth_operation_id IS NOT NULL OR epoch_generation IS NOT NULL)`,
		`SELECT count(*) FROM login_family_revocations WHERE user_id=$1`,
		`SELECT count(*) FROM login_family_revocation_targets WHERE user_id=$1`,
	} {
		require.NoError(t, r.db.QueryRow(query, r.user).Scan(&count))
		require.Zero(t, count, query)
	}
}

func (r securityEventFamilyRig) requireCommittedOwner(t *testing.T) {
	t.Helper()
	var count int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM refresh_tokens r
 JOIN login_family_sids s ON s.sid=r.id AND s.user_id=r.user_id
 JOIN login_families f ON f.family_id=s.family_id AND f.user_id=s.user_id
 JOIN login_family_revocation_targets t ON t.family_id=f.family_id AND t.user_id=f.user_id AND t.operation_id=f.auth_operation_id
 JOIN login_family_revocations o ON o.operation_id=t.operation_id AND o.user_id=t.user_id
 WHERE r.id=$1 AND r.user_id=$2 AND r.revoked_at IS NOT NULL AND f.terminal_at IS NOT NULL
 AND f.epoch_generation IS NULL AND o.sealed_at IS NOT NULL`, r.sid, r.user).Scan(&count))
	require.Equal(t, 1, count, "committed refresh revocation must have its exact sealed durable family owner")
}

type revokeMFAVerifier struct{ enabled bool }

func (v revokeMFAVerifier) IsEnabled(context.Context, string) bool { return v.enabled }
func (revokeMFAVerifier) VerifyCode(context.Context, string, stepup.Purpose, string) (bool, error) {
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
		name   string
		action revokeAction
	}{
		{name: "delete", action: revokeSessionAction},
		{name: "revoke all", action: revokeAllSessionsAction},
		{name: "revocation mode", action: revocationModeSetAction},
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
			require.True(t, passwordHandler.authenticateForRevoke(context.Background(), passwordContext, privacyUserID, "wrong", "", test.action))
			require.Equal(t, http.StatusForbidden, passwordResponse.Code)
			require.Equal(t, []securityevent.Event{{
				EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied,
				Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials,
				AuthMethod: securityevent.AuthPassword, RouteTemplate: test.action.route,
			}}, passwordRecorder.events)
			require.True(t, middleware.NightwatchHandled(passwordContext))

			mfaHandler := NewHandler(db, nil, logger.New("test"), testDisconnector{}, revokeMFAVerifier{enabled: true})
			mfaRecorder := &securityEventRecorder{}
			mfaHandler.SetSecurityEvents(mfaRecorder)
			mfaContext, mfaResponse := newContext()
			require.True(t, mfaHandler.authenticateForRevoke(context.Background(), mfaContext, privacyUserID, "", "bad-code", test.action))
			require.Equal(t, http.StatusForbidden, mfaResponse.Code)
			require.Equal(t, []securityevent.Event{{
				EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied,
				Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid,
				RouteTemplate: test.action.route,
			}}, mfaRecorder.events)
			require.True(t, middleware.NightwatchHandled(mfaContext))

			noCredentialContext, noCredentialResponse := newContext()
			require.True(t, mfaHandler.authenticateForRevoke(context.Background(), noCredentialContext, privacyUserID, "", "", test.action))
			require.Equal(t, http.StatusForbidden, noCredentialResponse.Code)
			require.False(t, middleware.NightwatchHandled(noCredentialContext))
		})
	}
}

type testDisconnector struct{}

func (testDisconnector) DisconnectUser(uuid.UUID) {}
func (testDisconnector) DisconnectSession(string) {}
func (testDisconnector) DisconnectFamilyContext(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

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
