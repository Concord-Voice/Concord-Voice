package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func TestRTFix_SessionHeaderMustMatchSid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	run := func(sid, header string) int {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			if sid != "" {
				c.Set(JWTClaimsContextKey, jwt.MapClaims{"sid": sid})
			}
			c.Next()
		})
		r.Use(RequireSessionHeaderMatchesToken())
		r.POST("/v", func(c *gin.Context) { c.Status(http.StatusOK) })
		req := httptest.NewRequest(http.MethodPost, "/v", nil)
		req.Header.Set("X-Session-ID", header)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	require.Equal(t, http.StatusForbidden, run("s-attacker", "s-victim"), "foreign slot refused")
	require.Equal(t, http.StatusOK, run("s-1", "s-1"))
	require.Equal(t, http.StatusOK, run("", "s-legacy"), "legacy token without sid passes")
}
