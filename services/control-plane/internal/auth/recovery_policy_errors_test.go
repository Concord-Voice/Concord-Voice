package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type policyErrorMFAChecker struct {
	ambiguousRecoveryMFAChecker
	err error
}

func (s *policyErrorMFAChecker) ValidateRecoveryToken(context.Context, string) (*RecoveryClaims, error) {
	return nil, s.err
}

func TestRecoveryPolicyUnavailableDoesNotSpendTokenOrCountAuthFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, consumer := range []string{"password reset", "account reset", "device", "social poll", "social create"} {
		for _, unavailable := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/unavailable=%t", consumer, unavailable), func(t *testing.T) {
				mini := miniredis.RunT(t)
				rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
				t.Cleanup(func() { require.NoError(t, rdb.Close()) })
				validationErr := errors.New("invalid proof")
				if unavailable {
					validationErr = fmt.Errorf("policy read: %w", ErrRecoveryPolicyUnavailable)
				}
				h := &Handler{redis: rdb, mfaChecker: &policyErrorMFAChecker{err: validationErr}}
				response := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(response)
				c.Request = httptest.NewRequest(http.MethodPost, "/recovery?recovery_token=token", strings.NewReader(`{"recovery_token":"token","ephemeral_public_key":"key"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.RemoteAddr = "192.0.2.1:1234"
				switch consumer {
				case "password reset", "account reset":
					claims, usedKey := h.validateAndConsumeRecoveryToken(c, "token", consumer == "password reset")
					require.Nil(t, claims)
					require.Empty(t, usedKey)
				case "device":
					require.Nil(t, h.validateDeviceRecoveryToken(c, "token"))
				case "social poll":
					require.Nil(t, h.validateRecoveryTokenParam(c))
				case "social create":
					h.CreateSocialRecoveryRequest(c)
				}
				status := http.StatusUnauthorized
				if unavailable {
					status = http.StatusServiceUnavailable
				}
				require.Equal(t, status, response.Code, response.Body.String())
				keys := mini.Keys()
				if !unavailable && consumer == "password reset" {
					require.Contains(t, keys, "auth_failures:ip:192.0.2.1")
				} else {
					require.Empty(t, keys, "failed validation must neither spend the token nor penalize a dependency outage")
				}
			})
		}
	}
}
