//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A reachable sentinel proves deployment denial stops downstream work even for
// a benign provider spelling not recognized by the registry. No SSO state is needed.
func TestRequireSSODeployment_RouteBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	modes := []struct {
		name, raw string
		denied    bool
	}{
		{"self-hosted", "self-hosted", true},
		{"normalized self-hosted", " \tSELF-HOSTED\n", true},
		{"saas", "saas", false},
		{"empty fallback", "", false},
		{"unknown fallback", "enterprise", false},
	}
	routes := []struct{ name, method, suffix string }{
		{"initiate", http.MethodGet, ""},
		{"session", http.MethodPost, "/session"},
		{"complete registration", http.MethodPost, "/complete-registration"},
		{"complete link", http.MethodPost, "/complete-link"},
		{"sign client secret", http.MethodPost, "/sign-client-secret"},
	}
	for _, mode := range modes {
		for _, route := range routes {
			for _, provider := range []string{"google", "apple", "unlisted"} {
				t.Run(mode.name+"/"+route.name+"/"+provider, func(t *testing.T) {
					router := gin.New()
					called := false
					group := router.Group("/api/v1/auth/sso", requireSSODeployment(&config.Config{InstanceType: mode.raw}))
					group.Handle(route.method, "/:provider"+route.suffix, func(c *gin.Context) {
						called = true
						c.Status(http.StatusNoContent)
					})
					w := httptest.NewRecorder()
					router.ServeHTTP(w, httptest.NewRequest(route.method, "/api/v1/auth/sso/"+provider+route.suffix, nil))
					if mode.denied {
						assert.False(t, called, "deployment denial must stop a reachable downstream handler")
						require.Equal(t, http.StatusForbidden, w.Code)
						assert.JSONEq(t, `{"error_code":"sso_disabled_self_hosted"}`, w.Body.String())
					} else {
						assert.True(t, called, "SaaS/fallback deployment must preserve downstream traversal")
						assert.Equal(t, http.StatusNoContent, w.Code)
						assert.Empty(t, w.Body.String())
					}
				})
			}
		}
	}
}
