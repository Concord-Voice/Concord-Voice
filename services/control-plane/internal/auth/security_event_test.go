package auth

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
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type securityEventRecorder struct{ events []securityevent.Event }

func (r *securityEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
}

func TestLoginUnknownUserAndWrongPasswordEmitIdenticalDomainEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })

	requestContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(response)
		ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
		return ginContext, response
	}

	noRowsDB := sql.OpenDB(noRowsConnector{})
	t.Cleanup(func() { require.NoError(t, noRowsDB.Close()) })
	unknownHandler := NewHandler(noRowsDB, redisClient, logger.New("test"), "test", nil)
	unknownRecorder := &securityEventRecorder{}
	unknownHandler.SetSecurityEvents(unknownRecorder)
	unknownContext, unknownResponse := requestContext()
	unknownContext.Request.Body = io.NopCloser(strings.NewReader(`{"email":"unknown@example.test","password":"wrong"}`))
	unknownContext.Request.Header.Set("Content-Type", "application/json")
	unknownHandler.Login(unknownContext)
	require.Equal(t, http.StatusUnauthorized, unknownResponse.Code)
	require.Len(t, unknownRecorder.events, 1)

	passwordHash, err := HashPasswordWithParams("correct", &Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	wrongHandler := NewHandler(nil, redisClient, logger.New("test"), "test", nil)
	wrongRecorder := &securityEventRecorder{}
	wrongHandler.SetSecurityEvents(wrongRecorder)
	wrongContext, wrongResponse := requestContext()
	require.False(t, wrongHandler.verifyCredentials(wrongContext.Request.Context(), wrongContext, "known@example.test", "wrong", passwordHash))
	require.Equal(t, http.StatusUnauthorized, wrongResponse.Code)
	require.Len(t, wrongRecorder.events, 1)

	unknownSerialized, err := json.Marshal(unknownRecorder.events)
	require.NoError(t, err)
	wrongSerialized, err := json.Marshal(wrongRecorder.events)
	require.NoError(t, err)
	require.Equal(t, unknownSerialized, wrongSerialized)
	require.Equal(t, securityevent.Event{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials, AuthMethod: securityevent.AuthPassword, RouteTemplate: securityevent.RouteAuthLogin}, unknownRecorder.events[0])
}

func TestLoginLockoutEmitsClosedSecurityControlEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	const email = "locked@example.test"
	require.NoError(t, rdb.Set(context.Background(), fmt.Sprintf(redisKeyLoginLockout, email), "1", time.Minute).Err())
	handler := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	recorder := &securityEventRecorder{}
	handler.SetSecurityEvents(recorder)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"locked@example.test","password":"correct"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Login(c)

	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonAccountLocked, AuthMethod: securityevent.AuthPassword, RouteTemplate: securityevent.RouteAuthLogin}}, recorder.events)
}

func TestRecoveryVerificationFailuresEmitClosedInvalidCredentialsEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func(code string) (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/recovery/verify-code", strings.NewReader(`{"email":"recovery@example.test","code":"`+code+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		return c, response
	}
	want := securityevent.Event{
		EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials,
		AuthMethod: securityevent.AuthRecovery, RouteTemplate: securityevent.RouteRecoveryVerifyCode,
	}
	for _, test := range []struct {
		name   string
		record *recoveryRecord
		code   string
	}{
		{name: "missing", code: "123456"},
		{name: "exhausted", record: &recoveryRecord{CodeHash: hashRecoveryCode("123456"), Attempts: recoveryMaxAttempts}, code: "123456"},
		{name: "wrong", record: &recoveryRecord{CodeHash: hashRecoveryCode("123456")}, code: "654321"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mini := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
			t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
			handler := &Handler{redis: redisClient}
			recorder := &securityEventRecorder{}
			handler.SetSecurityEvents(recorder)
			if test.record != nil {
				encoded, err := json.Marshal(test.record)
				require.NoError(t, err)
				require.NoError(t, handler.redis.Set(context.Background(), recoveryRedisKey("recovery@example.test"), encoded, time.Minute).Err())
			}
			c, response := newContext(test.code)
			handler.RecoveryVerifyCode(c)
			require.Equal(t, http.StatusUnauthorized, response.Code)
			require.Equal(t, []securityevent.Event{want}, recorder.events)
		})
	}
}

func TestDisabledLoginPathsEmitAccountDisabledEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	want := securityevent.Event{
		EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonAccountDisabled,
		AuthMethod: securityevent.AuthPassword, RouteTemplate: securityevent.RouteAuthLogin,
	}

	t.Run("direct password login", func(t *testing.T) {
		handler, db, userID := newAdmitPathHandler(t)
		passwordHash, err := HashPasswordWithParams("correct-password", &Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
		require.NoError(t, err)
		email := userID.String() + "@test.local"
		_, err = db.Exec(`UPDATE users SET disabled = TRUE, password_hash = $1 WHERE id = $2`, passwordHash, userID)
		require.NoError(t, err)
		recorder := &securityEventRecorder{}
		handler.SetSecurityEvents(recorder)
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"`+email+`","password":"correct-password"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.Login(c)
		require.Equal(t, http.StatusForbidden, response.Code)
		require.Equal(t, []securityevent.Event{want}, recorder.events)
	})

	t.Run("locked post MFA completion", func(t *testing.T) {
		handler, db, userID := newAdmitPathHandler(t)
		_, err := db.Exec(`UPDATE users SET disabled = TRUE WHERE id = $1`, userID)
		require.NoError(t, err)
		recorder := &securityEventRecorder{}
		handler.SetSecurityEvents(recorder)
		c, response := newLoginContext()
		require.False(t, handler.CompleteLogin(c, userID.String(), true, "", securityevent.AuthPassword))
		require.Equal(t, http.StatusForbidden, response.Code)
		require.Equal(t, []securityevent.Event{want}, recorder.events)
	})
}

func TestCompleteLoginEmitsOnlyAfterSessionMintCommit(t *testing.T) {
	failingHandler, _, failingUserID := newAdmitPathHandler(t)
	failingRecorder := &securityEventRecorder{}
	failingHandler.SetSecurityEvents(failingRecorder)
	failingDB := sql.OpenDB(noRowsConnector{})
	failingHandler.db = failingDB
	require.NoError(t, failingDB.Close())
	failingContext, failingResponse := newLoginContext()
	require.False(t, failingHandler.CompleteLogin(failingContext, failingUserID.String(), true, "", securityevent.AuthPassword))
	require.Equal(t, http.StatusInternalServerError, failingResponse.Code)
	require.Empty(t, failingRecorder.events, "a failed session-mint transaction must not emit authentication_succeeded")

	successHandler, _, successUserID := newAdmitPathHandler(t)
	successRecorder := &securityEventRecorder{}
	successHandler.SetSecurityEvents(successRecorder)
	successContext, successResponse := newLoginContext()
	require.True(t, successHandler.CompleteLogin(successContext, successUserID.String(), true, "", securityevent.AuthPassword))
	require.Equal(t, http.StatusOK, successResponse.Code)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonAuthenticationSucceeded, AuthMethod: securityevent.AuthPassword, RouteTemplate: securityevent.RouteAuthLogin}}, successRecorder.events)
}

func TestCompleteLoginPreservesSSOPrimaryMethod(t *testing.T) {
	handler, _, userID := newAdmitPathHandler(t)
	recorder := &securityEventRecorder{}
	handler.SetSecurityEvents(recorder)
	c, response := newLoginContext()

	require.True(t, handler.CompleteLogin(c, userID.String(), true, "", securityevent.AuthSSO))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonAuthenticationSucceeded,
		AuthMethod: securityevent.AuthSSO, RouteTemplate: securityevent.RouteAuthLogin,
	}}, recorder.events)
}

func TestCompleteLoginRejectsUnknownPrimaryMethodBeforeMint(t *testing.T) {
	handler := NewHandler(nil, nil, logger.New("test"), "test", nil)
	recorder := &securityEventRecorder{}
	handler.SetSecurityEvents(recorder)
	c, response := newLoginContext()

	require.False(t, handler.CompleteLogin(c, uuid.NewString(), true, "", securityevent.AuthMethod("forged-sso")))
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Empty(t, recorder.events)
}

func TestSSOMintEmitsOnlyAfterCommittedRefreshSession(t *testing.T) {
	failingHandler, _, failingUserID := newAdmitPathHandler(t)
	failingRecorder := &securityEventRecorder{}
	failingHandler.SetSecurityEvents(failingRecorder)
	failingDB := sql.OpenDB(noRowsConnector{})
	failingHandler.db = failingDB
	require.NoError(t, failingDB.Close())
	_, _, _, err := failingHandler.IssueAccessAndRefresh(context.Background(), failingUserID.String())
	require.Error(t, err)
	require.Empty(t, failingRecorder.events, "a failed SSO session transaction must not emit authentication_succeeded")

	successHandler, _, successUserID := newAdmitPathHandler(t)
	successRecorder := &securityEventRecorder{}
	successHandler.SetSecurityEvents(successRecorder)
	_, _, _, err = successHandler.IssueAccessAndRefresh(context.Background(), successUserID.String())
	require.NoError(t, err)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonAuthenticationSucceeded,
		AuthMethod: securityevent.AuthSSO,
	}}, successRecorder.events)
}

func TestRefreshBranchesEmitOnlyTheirAuthoritativeOutcomes(t *testing.T) {
	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
		return c, response
	}

	t.Run("rotation failure then committed rotation", func(t *testing.T) {
		failingHandler, _, failingUserID := newAdmitPathHandler(t)
		failingRecorder := &securityEventRecorder{}
		failingHandler.SetSecurityEvents(failingRecorder)
		failingDB := sql.OpenDB(noRowsConnector{})
		failingHandler.db = failingDB
		require.NoError(t, failingDB.Close())
		failingContext, failingResponse := newContext()
		failingHandler.rotateAndRespond(failingContext, models.RefreshToken{ID: uuid.NewString(), UserID: failingUserID.String(), ExpiresAt: time.Now().Add(time.Hour)}, "")
		require.Equal(t, http.StatusInternalServerError, failingResponse.Code)
		require.Empty(t, failingRecorder.events, "a failed refresh transaction must not emit refresh_rotated")

		rig := newFamilyTestRig(t)
		access, _, sourceID, err := rig.h.IssueAccessAndRefresh(t.Context(), rig.user)
		require.NoError(t, err)
		family, _ := assertFamilyAccess(t, rig, access)
		var expiry time.Time
		require.NoError(t, rig.db.QueryRow(`SELECT expires_at FROM refresh_tokens WHERE id=$1`, sourceID).Scan(&expiry))
		successRecorder := &securityEventRecorder{}
		rig.h.SetSecurityEvents(successRecorder)
		successContext, successResponse := newContext()
		rig.h.rotateAndRespond(successContext, models.RefreshToken{ID: sourceID, UserID: rig.user, ExpiresAt: expiry}, "")
		require.Equal(t, http.StatusOK, successResponse.Code, successResponse.Body.String())
		pair := familyResponse(t, successResponse)
		successorFamily, successorID := assertFamilyAccess(t, rig, pair.Access)
		require.Equal(t, family, successorFamily)
		require.NotEqual(t, sourceID, successorID)
		var revoked sql.NullTime
		var predecessor, successorHash string
		require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, sourceID).Scan(&revoked))
		require.True(t, revoked.Valid, "the rotation event must follow a committed source revocation")
		require.NoError(t, rig.db.QueryRow(`SELECT predecessor_id,token_hash FROM refresh_tokens WHERE id=$1 AND revoked_at IS NULL`, successorID).Scan(&predecessor, &successorHash))
		require.Equal(t, sourceID, predecessor)
		require.Equal(t, HashRefreshToken(pair.Refresh), successorHash)
		require.Equal(t, []securityevent.Event{{
			EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess,
			Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonRefreshRotated,
			AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteAuthRefresh,
		}}, successRecorder.events)
	})

	t.Run("stale replay", func(t *testing.T) {
		handler, db, userID := newAdmitPathHandler(t)
		replayedID := uuid.NewString()
		_, err := db.Exec(`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, revoked_at, ip_address, user_agent) VALUES ($1, $2, $3, $4, $5, $6, $7)`, replayedID, userID, HashRefreshToken("replayed-refresh"), time.Now().Add(time.Hour), time.Now().Add(-time.Minute), "198.51.100.20", "stored-agent")
		require.NoError(t, err)
		recorder := &securityEventRecorder{}
		handler.SetSecurityEvents(recorder)
		c, response := newContext()
		c.Request.Header.Set(headerUserAgent, "different-agent")
		require.True(t, handler.attemptGracePeriodRecovery(c, HashRefreshToken("replayed-refresh")))
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.Equal(t, []securityevent.Event{{
			EventType: securityevent.EventSession, Outcome: securityevent.OutcomeDenied,
			Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonRefreshReplay,
			AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteAuthRefresh,
		}}, recorder.events)
	})

	t.Run("machine and network mismatch", func(t *testing.T) {
		handler, _, userID := newAdmitPathHandler(t)
		handler.hub = tokenTheftTestDisconnector{}
		recorder := &securityEventRecorder{}
		handler.SetSecurityEvents(recorder)
		c, response := newContext()
		require.True(t, handler.handleTokenTheft(c, models.RefreshToken{UserID: userID.String(), MachineID: "known-machine", IPAddress: "198.51.100.20"}))
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.Equal(t, []securityevent.Event{{
			EventType: securityevent.EventSession, Outcome: securityevent.OutcomeDenied,
			Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonTokenTheftSuspected,
			AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteAuthRefresh,
		}}, recorder.events)
	})
}

func TestLogoutEmitsOnlyAfterCommittedRevoke(t *testing.T) {
	for _, unavailable := range []bool{true, false} {
		name := "committed selected family"
		if unavailable {
			name = "family SQL unavailable"
		}
		t.Run(name, func(t *testing.T) {
			rig := newFamilyTestRig(t)
			pair, family, sid := terminalMint(t, rig)
			claims, err := ValidateAccessToken(pair.Access, familyTestSecret)
			require.NoError(t, err)
			recorder := &securityEventRecorder{}
			rig.h.SetSecurityEvents(recorder)
			if unavailable {
				closedDB := sql.OpenDB(noRowsConnector{})
				require.NoError(t, closedDB.Close())
				rig.h.db = closedDB
			}
			c, response := familyTestContext()
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
			c.Request.Header.Set("X-Refresh-Token", pair.Refresh)
			c.Request.Header.Set("Authorization", "Bearer "+pair.Access)
			c.Request.AddCookie(&http.Cookie{Name: "refresh_token", Value: pair.Refresh, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			rig.h.Logout(c)
			var revoked sql.NullTime
			require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, sid).Scan(&revoked))
			at, owner := terminalState(t, rig, family)
			blacklisted, err := rig.h.redis.Exists(t.Context(), "blacklist:"+claims.ID).Result()
			require.NoError(t, err)
			if unavailable {
				require.Equal(t, http.StatusServiceUnavailable, response.Code)
				require.JSONEq(t, `{"error":"Authentication temporarily unavailable"}`, response.Body.String())
				require.Empty(t, recorder.events)
				require.Empty(t, response.Header().Values("Set-Cookie"))
				require.Zero(t, blacklisted)
				require.False(t, revoked.Valid)
				require.False(t, at.Valid)
				require.False(t, owner.Valid)
				return
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.True(t, revoked.Valid)
			require.True(t, at.Valid)
			require.True(t, owner.Valid)
			var reason string
			require.NoError(t, rig.db.QueryRow(`SELECT reason FROM login_family_revocations WHERE operation_id=$1 AND user_id=$2`, owner.String, rig.user).Scan(&reason))
			require.Equal(t, "logout", reason)
			require.Equal(t, []securityevent.Event{{
				EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess,
				Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonSessionRevoked,
				AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteAuthLogout,
			}}, recorder.events)
		})
	}
}

type recoveryTokenFailureChecker struct{ ambiguousRecoveryMFAChecker }

func (recoveryTokenFailureChecker) GenerateRecoveryToken(string, string) (string, string, error) {
	return "", "", errors.New("recovery token issuer failed")
}

func TestRecoveryVerificationEmitsOnlyAfterTokenAndResponseSucceed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/recovery/verify-code", strings.NewReader(`{"email":"recovery@example.test","code":"123456"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		return c, response
	}
	seedCode := func(t *testing.T, h *Handler, userID string) {
		t.Helper()
		encoded, err := json.Marshal(recoveryRecord{CodeHash: hashRecoveryCode("123456"), UserID: userID})
		require.NoError(t, err)
		require.NoError(t, h.redis.Set(context.Background(), recoveryRedisKey("recovery@example.test"), encoded, time.Minute).Err())
	}

	failingHandler, _, failingUserID := newAdmitPathHandler(t)
	failingHandler.mfaChecker = &recoveryTokenFailureChecker{}
	failingRecorder := &securityEventRecorder{}
	failingHandler.SetSecurityEvents(failingRecorder)
	seedCode(t, failingHandler, failingUserID.String())
	failingContext, failingResponse := newContext()
	failingHandler.RecoveryVerifyCode(failingContext)
	require.Equal(t, http.StatusInternalServerError, failingResponse.Code)
	require.Empty(t, failingRecorder.events, "a failed recovery-token operation must not emit recovery_verified")

	successHandler, _, successUserID := newAdmitPathHandler(t)
	successHandler.mfaChecker = &ambiguousRecoveryMFAChecker{}
	successRecorder := &securityEventRecorder{}
	successHandler.SetSecurityEvents(successRecorder)
	seedCode(t, successHandler, successUserID.String())
	successContext, successResponse := newContext()
	successHandler.RecoveryVerifyCode(successContext)
	require.Equal(t, http.StatusOK, successResponse.Code)
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonRecoveryVerified,
		AuthMethod: securityevent.AuthRecovery, RouteTemplate: securityevent.RouteRecoveryVerifyCode,
	}}, successRecorder.events)
}

func TestRecoveryResetEmitsOnlyAfterCommittedTransaction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, flow := range recoveryForcedFlows() {
		t.Run(flow.name, func(t *testing.T) {
			failedDB, _ := dbtest.SetupTestDB(t)
			failedUserID := dbtest.CreateUser(t, failedDB)
			seedRecoveryCommitKeyRows(t, failedDB, failedUserID)
			seedRecoveryForcedPresence(t, failedDB, failedUserID)
			failedRedis := setupAuthAttemptRedis(t)
			failedClaims := &RecoveryClaims{UserID: failedUserID.String(), JTI: uuid.NewString()}
			failedService := newRecoveryForcedService(t, failedDB, &recoveryCommitDelivery{})
			restore := failedService.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
				RecordTransition: func(context.Context, *sql.Tx, uuid.UUID, presencehistory.CustomTextState, presencehistory.CustomTextState) error {
					return errors.New("forced transaction failure")
				},
			})
			t.Cleanup(restore)
			failedHandler := newRecoveryForcedHandler(failedDB, failedRedis, &recoveryForcedEventRecorder{}, failedService, failedClaims)
			failedRecorder := &securityEventRecorder{}
			failedHandler.SetSecurityEvents(failedRecorder)
			failedResponse := invokeRecoveryForcedFlow(flow, failedHandler)
			require.Equal(t, http.StatusInternalServerError, failedResponse.Code)
			require.Empty(t, failedRecorder.events, "a failed recovery transaction must not emit recovery_reset")

			successDB, _ := dbtest.SetupTestDB(t)
			successUserID := dbtest.CreateUser(t, successDB)
			seedRecoveryCommitKeyRows(t, successDB, successUserID)
			seedRecoveryForcedPresence(t, successDB, successUserID)
			successRedis := setupAuthAttemptRedis(t)
			successClaims := &RecoveryClaims{UserID: successUserID.String(), JTI: uuid.NewString()}
			successHandler := newRecoveryForcedHandler(successDB, successRedis, &recoveryForcedEventRecorder{}, newRecoveryForcedService(t, successDB, &recoveryCommitDelivery{}), successClaims)
			successRecorder := &securityEventRecorder{}
			successHandler.SetSecurityEvents(successRecorder)
			successResponse := invokeRecoveryForcedFlow(flow, successHandler)
			require.Equal(t, http.StatusOK, successResponse.Code)
			require.Len(t, successRecorder.events, 1)
			require.Equal(t, securityevent.EventAuthentication, successRecorder.events[0].EventType)
			require.Equal(t, securityevent.OutcomeSuccess, successRecorder.events[0].Outcome)
			require.Equal(t, securityevent.SeverityHigh, successRecorder.events[0].Severity)
			require.Equal(t, securityevent.ReasonRecoveryReset, successRecorder.events[0].ReasonCode)
			require.Equal(t, securityevent.AuthRecovery, successRecorder.events[0].AuthMethod)
			if flow.name == "password recovery" {
				require.Equal(t, securityevent.RouteRecoveryResetPassword, successRecorder.events[0].RouteTemplate)
			} else {
				require.Equal(t, securityevent.RouteRecoveryResetAccount, successRecorder.events[0].RouteTemplate)
			}
		})
	}
}

// noRowsConnector drives Login's real unknown-user branch without a database
// service. QueryRow.Scan sees sql.ErrNoRows, exactly as PostgreSQL returns.
type noRowsConnector struct{}

func (noRowsConnector) Connect(context.Context) (driver.Conn, error) { return noRowsConn{}, nil }
func (noRowsConnector) Driver() driver.Driver                        { return noRowsDriver{} }

type noRowsDriver struct{}

func (noRowsDriver) Open(string) (driver.Conn, error) { return noRowsConn{}, nil }

type noRowsConn struct{}

func (noRowsConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (noRowsConn) Close() error                        { return nil }
func (noRowsConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (noRowsConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return noRows{}, nil
}

type noRows struct{}

func (noRows) Columns() []string         { return []string{"id"} }
func (noRows) Close() error              { return nil }
func (noRows) Next([]driver.Value) error { return io.EOF }

func TestRecoveryVerificationAttributesServerRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := sql.OpenDB(recoveryIssuanceConnector{})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	for _, route := range []securityevent.RouteTemplate{securityevent.RouteRecoveryVerifyCode, securityevent.RouteAuthMFAVerify} {
		t.Run(string(route), func(t *testing.T) {
			recorder := &securityEventRecorder{}
			h := &Handler{db: db, mfaChecker: &ambiguousRecoveryMFAChecker{}, log: logger.NewWithWriter(io.Discard)}
			h.SetSecurityEvents(recorder)
			path := strings.TrimPrefix(string(route), "POST ")
			router := gin.New()
			router.POST(path, func(c *gin.Context) { h.CompleteRecovery(c, "user", "totp") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
			require.Equal(t, http.StatusOK, response.Code)
			require.Len(t, recorder.events, 1)
			require.Equal(t, securityevent.ReasonRecoveryVerified, recorder.events[0].ReasonCode)
			require.Equal(t, route, recorder.events[0].RouteTemplate)
		})
	}
}

type recoveryIssuancePolicyChecker struct {
	ambiguousRecoveryMFAChecker
	err error
}

func (s *recoveryIssuancePolicyChecker) ValidateRecoveryClaimsTx(context.Context, *sql.Tx, *RecoveryClaims) error {
	return s.err
}

func TestRecoveryIssuanceRejectsChangedOrUnavailablePolicy(t *testing.T) {
	db := sql.OpenDB(recoveryIssuanceConnector{})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"changed", ErrRecoveryProofInvalid, http.StatusUnauthorized},
		{"unavailable", ErrRecoveryPolicyUnavailable, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &securityEventRecorder{}
			h := &Handler{db: db, mfaChecker: &recoveryIssuancePolicyChecker{err: tc.err}, log: logger.NewWithWriter(io.Discard)}
			h.SetSecurityEvents(recorder)
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/recovery/verify-code", nil)
			require.False(t, h.CompleteRecovery(c, "user", "email"))
			require.Equal(t, tc.status, response.Code)
			require.NotContains(t, response.Body.String(), "recovery_token")
			require.NotContains(t, response.Body.String(), "recovery_wrapped_private_key")
			require.Empty(t, recorder.events)
		})
	}
}

type recoveryIssuanceConnector struct{ noRowsConnector }

func (recoveryIssuanceConnector) Connect(context.Context) (driver.Conn, error) {
	return recoveryIssuanceConn{}, nil
}

type recoveryIssuanceConn struct{ noRowsConn }

func (recoveryIssuanceConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return recoveryIssuanceTx{}, nil
}

type recoveryIssuanceTx struct{}

func (recoveryIssuanceTx) Commit() error   { return nil }
func (recoveryIssuanceTx) Rollback() error { return nil }
