package middleware

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
)

func TestTokenAuthTime(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	_, ok := TokenAuthTime(c)
	assert.False(t, ok, "no claims on the context")

	c.Set(JWTClaimsContextKey, jwt.MapClaims{"sid": "s"})
	_, ok = TokenAuthTime(c)
	assert.False(t, ok, "refresh-minted token carries no auth_time")

	c.Set(JWTClaimsContextKey, jwt.MapClaims{"auth_time": "not-a-number"})
	_, ok = TokenAuthTime(c)
	assert.False(t, ok, "a non-numeric auth_time is refused")

	c.Set(JWTClaimsContextKey, jwt.MapClaims{"auth_time": float64(1700000000)})
	got, ok := TokenAuthTime(c)
	assert.True(t, ok)
	assert.Equal(t, time.Unix(1700000000, 0), got)
}
