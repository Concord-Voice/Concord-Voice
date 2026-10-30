package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type eventRecorder struct {
	events       []securityevent.Event
	correlations []string
}

func (r *eventRecorder) Emit(ctx context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
	r.correlations = append(r.correlations, securityevent.CorrelationFromContext(ctx))
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

func TestNightwatchObserverUsesFullPathAndServerCorrelation(t *testing.T) {
	recorder := &eventRecorder{}
	router := gin.New()
	router.Use(nightwatchSecurityObserver(recorder))
	router.POST("/api/v1/auth/login", func(c *gin.Context) {
		middleware.MarkNightwatchVerdict(c, middleware.NightwatchVerdict{
			EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied,
			Severity: securityevent.SeverityMedium, Reason: securityevent.ReasonInvalidCredentials,
			AuthMethod: securityevent.AuthPassword,
		})
		c.Status(http.StatusUnauthorized)
	})
	router.POST("/api/v1/auth/logout", func(c *gin.Context) {
		recorder.Emit(c.Request.Context(), securityevent.Event{
			EventType: securityevent.EventSession, Outcome: securityevent.OutcomeDenied,
			Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonCredentialEpochMismatch,
		})
		middleware.MarkNightwatchHandled(c)
		c.Status(http.StatusUnauthorized)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login?token=attacker", nil)
	req.Header.Set("X-Request-ID", "attacker-controlled")
	router.ServeHTTP(httptest.NewRecorder(), req)
	require.Len(t, recorder.events, 1)
	require.Equal(t, securityevent.RouteAuthLogin, recorder.events[0].RouteTemplate)
	require.NotEqual(t, "attacker-controlled", recorder.correlations[0])

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/unmatched/raw/secret", nil))
	require.Len(t, recorder.events, 1)

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil))
	require.Len(t, recorder.events, 2)
}

func TestNightwatchObserverCountsOneControlDecisionOrFallback(t *testing.T) {
	recorder := &eventRecorder{}
	router := gin.New()
	router.Use(nightwatchSecurityObserver(recorder))
	router.POST("/api/v1/auth/login", func(c *gin.Context) {
		c.Status(http.StatusUnauthorized)
	})
	router.POST("/api/v1/auth/refresh", func(c *gin.Context) {
		middleware.MarkNightwatchHandled(c)
		c.Status(http.StatusUnauthorized)
	})
	router.POST("/api/v1/auth/mfa/verify", func(c *gin.Context) {
		middleware.MarkNightwatchVerdict(c, middleware.NightwatchVerdict{
			EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied,
			Severity: securityevent.SeverityMedium, Reason: securityevent.ReasonRateLimitExceeded,
		})
		c.Status(http.StatusTooManyRequests)
	})

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil))
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials,
		RouteTemplate: securityevent.RouteAuthLogin,
	}}, recorder.events)

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil))
	require.Len(t, recorder.events, 1, "a domain event marker must suppress the coarse fallback")

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil))
	require.Len(t, recorder.events, 2, "one middleware verdict must produce one control event")
	require.Equal(t, securityevent.ReasonRateLimitExceeded, recorder.events[1].ReasonCode)
	require.Equal(t, securityevent.RouteAuthMFAVerify, recorder.events[1].RouteTemplate)
	requireSerializedEventPrivacy(t, recorder.events)
}

func TestNightwatchObserverDoesNotTreatNonCanonicalIdentityObservationAsTerminal(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })

	userID := dbtest.CreateUser(t, db)
	var epoch string
	require.NoError(t, db.QueryRow(`SELECT COALESCE(credential_epoch,'') FROM users WHERE id=$1`, userID).Scan(&epoch))
	sid := uuid.New()
	prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: userID, SID: sid, CredentialEpoch: epoch}, time.Now())
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := tx.Rollback()
		if err != sql.ErrTxDone {
			require.NoError(t, err)
		}
	})
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{userID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(userID)
	require.NoError(t, err)
	family, err := loginfamily.CreateFamilyTx(t.Context(), tx, user, epoch)
	require.NoError(t, err)
	require.NoError(t, loginfamily.BindSIDTx(t.Context(), tx, family, sid, prepared.ExpiresAt()))
	require.NoError(t, tx.Commit())
	const testJWTSecret = "observer-family-test-secret"
	canonicalToken, err := prepared.Sign(testJWTSecret)
	require.NoError(t, err)
	token, err := jwt.Parse(canonicalToken, func(*jwt.Token) (any, error) { return []byte(testJWTSecret), nil }, jwt.WithValidMethods([]string{"HS256"}))
	require.NoError(t, err)
	claims, ok := token.Claims.(jwt.MapClaims)
	require.True(t, ok)
	// Keep every signed authority claim, including the exact committed expiry;
	// vary only the legacy identity spelling observed by AuthRequired.
	claims["user_id"] = "{" + userID.String() + "}"
	encoded, err := token.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)

	recorder := &eventRecorder{}
	router := gin.New()
	router.Use(nightwatchSecurityObserver(recorder))
	handlerCalls := 0
	router.POST("/api/v1/servers/:id/bans/:user_id", middleware.AuthRequired(testJWTSecret, redisClient, db, nil), func(c *gin.Context) {
		handlerCalls++
		require.Equal(t, userID.String(), c.GetString("user_id"))
		if c.Query("terminal") == "1" {
			middleware.MarkNightwatchVerdict(c, middleware.NightwatchVerdict{
				EventType: securityevent.EventPrivilegedAction, Outcome: securityevent.OutcomeDenied,
				Severity: securityevent.SeverityMedium, Reason: securityevent.ReasonAuthorizationDenied,
			})
		}
		c.Status(http.StatusForbidden)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers/server-1/bans/user-1", nil)
	req.Header.Set("Authorization", "Bearer "+encoded)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Equal(t, 1, handlerCalls, "identity observation must reach the privileged handler")

	require.Len(t, recorder.events, 2,
		"a noncanonical identity observation must be emitted before the downstream privileged fallback")
	require.Equal(t, securityevent.EventAuthentication, recorder.events[0].EventType)
	require.Equal(t, securityevent.ReasonIdentityClaimNonCanonical, recorder.events[0].ReasonCode)
	require.Equal(t, securityevent.EventPrivilegedAction, recorder.events[1].EventType)
	require.Equal(t, securityevent.ReasonPrivilegedRouteDenied, recorder.events[1].ReasonCode,
		"identity normalization must not suppress the allowlisted privileged-route fallback")

	terminalReq := httptest.NewRequest(http.MethodPost, "/api/v1/servers/server-1/bans/user-1?terminal=1", nil)
	terminalReq.Header.Set("Authorization", "Bearer "+encoded)
	terminalResponse := httptest.NewRecorder()
	router.ServeHTTP(terminalResponse, terminalReq)
	require.Equal(t, http.StatusForbidden, terminalResponse.Code)
	require.Equal(t, 2, handlerCalls, "terminal verdict must originate in the privileged handler")

	require.Len(t, recorder.events, 4,
		"a downstream terminal verdict must follow the identity observation without a coarse fallback duplicate")
	require.Equal(t, securityevent.EventAuthentication, recorder.events[2].EventType)
	require.Equal(t, securityevent.ReasonIdentityClaimNonCanonical, recorder.events[2].ReasonCode)
	require.Equal(t, securityevent.EventPrivilegedAction, recorder.events[3].EventType)
	require.Equal(t, securityevent.ReasonAuthorizationDenied, recorder.events[3].ReasonCode)
}

func TestNightwatchServerMutationFallbacksArePrivilegedActionDenials(t *testing.T) {
	for _, route := range []securityevent.RouteTemplate{
		securityevent.RouteServerMemberPatch, securityevent.RouteServerMemberDelete,
		securityevent.RouteServerBanCreate, securityevent.RouteServerBanDelete,
		securityevent.RouteServerRoleCreate, securityevent.RouteServerRolePatch,
		securityevent.RouteServerRoleDelete, securityevent.RouteServerMemberRoleCreate,
		securityevent.RouteServerMemberRoleDelete, securityevent.RouteServerTransferOwnership,
		securityevent.RouteServerTransferOwnershipOK, securityevent.RouteServerMFAEnforcement,
	} {
		event := nightwatchFallbackEvent(route)
		require.Equal(t, securityevent.EventPrivilegedAction, event.EventType, route)
		require.Equal(t, securityevent.ReasonPrivilegedRouteDenied, event.ReasonCode, route)
		require.Equal(t, route, event.RouteTemplate)
	}
}

func TestNightwatchRecoveryFallbacksAreRecoveryAuthenticationDenials(t *testing.T) {
	for _, route := range []securityevent.RouteTemplate{
		securityevent.RouteRecoveryVerifyCode,
		securityevent.RouteRecoveryResetPassword,
		securityevent.RouteRecoveryResetAccount,
	} {
		event := nightwatchFallbackEvent(route)
		require.Equal(t, securityevent.EventAuthentication, event.EventType, route)
		require.Equal(t, securityevent.OutcomeDenied, event.Outcome, route)
		require.Equal(t, securityevent.ReasonInvalidCredentials, event.ReasonCode, route)
		require.Equal(t, securityevent.AuthRecovery, event.AuthMethod, route)
	}
}

func TestTask4AssemblySecurityEventInjection(t *testing.T) {
	log := logger.New("test")

	t.Run("rbac audit writer", func(t *testing.T) {
		recorder := &eventRecorder{}
		audit := newAuditWriterWithSecurityEvents(&sql.DB{}, log, recorder)
		require.Error(t, audit.Log(context.Background(), "server", nil, "role_created", "role", nil, map[string]any{"invalid": math.Inf(1)}))
		require.Equal(t, []securityevent.Event{{
			EventType: securityevent.EventAudit, Outcome: securityevent.OutcomeFailure,
			Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuditWriteFailed,
		}}, recorder.events)
	})

	t.Run("ops runtime receiver", func(t *testing.T) {
		recorder := &eventRecorder{}
		receiver := newOpsMetricsReceiverWithSecurityEvents(nil, "cvn_aaaaaaaaaaaaaaaa", nil, opsmetrics.NewCounters(), log, recorder)
		require.Error(t, receiver.Subscribe())
		require.Equal(t, []securityevent.Event{{
			EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded,
			Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable,
		}}, recorder.events)
	})

	t.Run("media and hub owners", func(t *testing.T) {
		recorder := &eventRecorder{}
		cfg := &config.Config{InstanceType: "saas", AdminConsoleEnabled: true, AdminWebAuthnRPID: "admin.example.org", AdminWebAuthnRPOrigins: []string{"https://admin.example.org"}, AdminWebAuthnAllowedAAGUIDs: []string{"ee882879-721c-4913-9775-3dfcce97072a"}}
		mediaHandler := media.NewHandler(nil, nil, log, cfg, nil, nil)
		wireMediaHandler(mediaHandler, nil, nil, cfg, log, nil, RouterDependencies{SecurityEvents: recorder})
		require.Equal(t, reflect.ValueOf(recorder).Pointer(), nestedEmitterPointer(t, mediaHandler, "diskWatermark", "events"))
		hub := newHubWithSecurityEvents(nil, nil, nil, recorder)
		require.Equal(t, reflect.ValueOf(recorder).Pointer(), nestedEmitterPointer(t, hub, "securityEvents"))
	})

	t.Run("attestation handler", func(t *testing.T) {
		recorder := &eventRecorder{}
		handler := buildAttestationHandlerWithSecurityEvents(t.Context(), openWiringTestDB(t), nil, nil, newDisabledAttestationConfig(), log, recorder)
		response := httptest.NewRecorder()
		requestContext, _ := gin.CreateTestContext(response)
		requestContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/attestation/verify", bytes.NewBufferString(`{"version":"1.0.0","platform":"web","spa_version":"20260901","spa_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
		requestContext.Request.Header.Set("Content-Type", "application/json")
		requestContext.Request.Header.Set("X-Session-ID", "nightwatch-session")
		requestContext.Set("user_id", "nightwatch-user")
		handler.Verify(requestContext, time.Minute)
		require.Equal(t, http.StatusForbidden, response.Code)
		require.Equal(t, []securityevent.Event{{
			EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied,
			Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonAttestationRejected,
		}}, recorder.events)
	})

	t.Run("admin handler", func(t *testing.T) {
		recorder := &eventRecorder{}
		redisServer := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
		t.Cleanup(func() { require.NoError(t, rdb.Close()) })
		cfg := &config.Config{InstanceType: "saas", AdminConsoleEnabled: true, AdminWebAuthnRPID: "admin.example.org", AdminWebAuthnRPOrigins: []string{"https://admin.example.org"}, AdminWebAuthnAllowedAAGUIDs: []string{"ee882879-721c-4913-9775-3dfcce97072a"}}
		db := openWiringTestDB(t)
		require.NoError(t, db.Close())
		handler := wireAdminRoutesWithSecurityEvents(gin.New(), db, rdb, nil, cfg, log, recorder)
		require.NotNil(t, handler)
		response := httptest.NewRecorder()
		requestContext, _ := gin.CreateTestContext(response)
		requestContext.Request = httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/login", bytes.NewBufferString(`{"username":"nightwatch","password":"invalid"}`))
		requestContext.Request.Header.Set("Content-Type", "application/json")
		handler.PasswordLogin(requestContext)
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.Equal(t, []securityevent.Event{{
			EventType: securityevent.EventAudit, Outcome: securityevent.OutcomeFailure,
			Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuditWriteFailed,
		}}, recorder.events)
	})
}

func nestedEmitterPointer(t *testing.T, owner any, fields ...string) uintptr {
	t.Helper()
	value := reflect.ValueOf(owner).Elem()
	for _, field := range fields {
		value = value.FieldByName(field)
		require.True(t, value.IsValid())
		if isPointerOrInterface(value.Kind()) {
			value = value.Elem()
		}
	}
	return value.Pointer()
}

func isPointerOrInterface(kind reflect.Kind) bool {
	return kind == reflect.Pointer || kind == reflect.Interface
}
