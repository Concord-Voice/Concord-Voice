package users

// The 15-minute window in GetMyKeys cannot be reached over HTTP: a token past
// it is also past exp and fails as 401 first. So it is pinned here directly.

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

func ctxWithClaims(claims jwt.MapClaims) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if claims != nil {
		c.Set(middleware.JWTClaimsContextKey, claims)
	}
	return c
}

func TestRecentPrimaryAuth(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	at := func(d time.Duration) jwt.MapClaims {
		return jwt.MapClaims{"auth_time": float64(now.Add(-d).Unix())}
	}
	cases := []struct {
		name   string
		claims jwt.MapClaims
		want   bool
	}{
		{"fresh sign-in", at(time.Minute), true},
		{"exactly one token lifetime", at(auth.AccessTokenTTL), true},
		{"one second past the window", at(auth.AccessTokenTTL + time.Second), false},
		{"renewed token has no auth_time", jwt.MapClaims{"sub": "u"}, false},
		{"non-numeric auth_time", jwt.MapClaims{"auth_time": "1800000000"}, false},
		{"no claims at all", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, recentPrimaryAuth(ctxWithClaims(tc.claims), now))
		})
	}
}
