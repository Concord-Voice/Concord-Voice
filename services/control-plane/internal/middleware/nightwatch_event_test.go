package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func requireSerializedVerdictPrivacy(t *testing.T, verdicts []NightwatchVerdict) {
	t.Helper()
	require.NotEmpty(t, verdicts)
	serialized, err := json.Marshal(verdicts)
	require.NoError(t, err)
	for _, fixture := range []string{
		"code-fixture", "credential-fixture", "user-fixture", "session-fixture",
		"device-fixture", "token-fixture", "198.51.100.44", "email-fixture@example.test", "raw-error-fixture",
	} {
		require.NotContains(t, string(serialized), fixture)
	}
}

func TestMarkAuthFailureOutcomeUsesClosedVerdicts(t *testing.T) {
	for _, test := range []struct {
		name    string
		outcome AuthFailureOutcome
		want    securityevent.ReasonCode
	}{
		{"ban", AuthFailureBanCreated, securityevent.ReasonSourceBanCreated},
		{"backend", AuthFailureBackendUnavailable, securityevent.ReasonRateLimitBackendUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
			MarkAuthFailureOutcome(ctx, test.outcome)
			verdict, ok := NightwatchVerdictFromContext(ctx)
			require.True(t, ok)
			require.Equal(t, test.want, verdict.Reason)
		})
	}
}

func TestNightwatchVerdictsComeFromRealControlBranches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/admin", nil)
		return c
	}
	var verdicts []NightwatchVerdict
	requireVerdict := func(t *testing.T, c *gin.Context, want securityevent.ReasonCode) {
		t.Helper()
		verdict, ok := NightwatchVerdictFromContext(c)
		require.True(t, ok)
		require.Equal(t, want, verdict.Reason)
		verdicts = append(verdicts, verdict)
	}

	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })

	t.Run("rate limit exceeded", func(t *testing.T) {
		c := newContext()
		abortRateLimitExceeded(c, RateLimitConfig{}, time.Minute)
		requireVerdict(t, c, securityevent.ReasonRateLimitExceeded)
	})
	t.Run("rate limit backend unavailable", func(t *testing.T) {
		c := newContext()
		require.True(t, abortOnRateLimitBackendError(c, RateLimitConfig{FailClosed: true}))
		requireVerdict(t, c, securityevent.ReasonRateLimitBackendUnavailable)
	})
	t.Run("auth ban check and create", func(t *testing.T) {
		c := newContext()
		require.NoError(t, rdb.Set(context.Background(), authBanKeyPrefix+c.ClientIP(), "1", time.Minute).Err())
		AuthBanCheck(rdb)(c)
		requireVerdict(t, c, securityevent.ReasonSourceBanned)

		created := RecordAuthFailure(context.Background(), rdb, "198.51.100.44", AuthBanConfig{Threshold: 1, Window: time.Minute, Duration: time.Minute})
		require.Equal(t, AuthFailureBanCreated, created)
		createdContext := newContext()
		MarkAuthFailureOutcome(createdContext, created)
		requireVerdict(t, createdContext, securityevent.ReasonSourceBanCreated)
	})
	t.Run("cloudflare and attestation rejection", func(t *testing.T) {
		cloudflare := newContext()
		RequireCloudflareAccess(newAccessVerifier("example.cloudflareaccess.com", "audience"), nil)(cloudflare)
		requireVerdict(t, cloudflare, securityevent.ReasonCloudflareAccessDenied)

		attestation := newContext()
		rejectAtt(attestation, logger.New("test"), rdb, "invalid")
		requireVerdict(t, attestation, securityevent.ReasonAttestationRejected)
	})
	requireSerializedVerdictPrivacy(t, verdicts)
}

func TestAuthRequiredFailuresAlwaysSetClosedVerdicts(t *testing.T) {
	for _, test := range []struct {
		name      string
		call      func(*gin.Context)
		eventType securityevent.EventType
		outcome   securityevent.Outcome
		severity  securityevent.Severity
		want      securityevent.ReasonCode
		status    int
	}{
		{name: "missing bearer", call: abortUnauthorized, eventType: securityevent.EventAuthentication, outcome: securityevent.OutcomeDenied, severity: securityevent.SeverityMedium, want: securityevent.ReasonInvalidCredentials, status: http.StatusUnauthorized},
		{name: "disabled account", call: abortAccountDisabled, eventType: securityevent.EventAuthentication, outcome: securityevent.OutcomeDenied, severity: securityevent.SeverityMedium, want: securityevent.ReasonAccountDisabled, status: http.StatusForbidden},
		{name: "auth dependency", call: abortAuthDependency, eventType: securityevent.EventDependency, outcome: securityevent.OutcomeDegraded, severity: securityevent.SeverityHigh, want: securityevent.ReasonDependencyUnavailable, status: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodGet, "/not-in-nightwatch-route-allowlist", nil)
			test.call(context)
			verdict, ok := NightwatchVerdictFromContext(context)
			require.True(t, ok)
			require.Equal(t, test.eventType, verdict.EventType)
			require.Equal(t, test.outcome, verdict.Outcome)
			require.Equal(t, test.severity, verdict.Severity)
			require.Equal(t, test.want, verdict.Reason)
			require.Equal(t, test.status, recorder.Code)
		})
	}
}
