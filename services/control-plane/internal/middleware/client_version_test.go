package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clientVersionRouter(minimum string) (*gin.Engine, *bool) {
	r := gin.New()
	called := new(bool)
	r.GET("/test", middleware.RequireClientVersion(middleware.ClientMinimums{Desktop: minimum}), func(c *gin.Context) {
		*called = true
		c.Status(http.StatusNoContent)
	})
	return r, called
}

func requestClientVersion(r *gin.Engine, version string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	if version != "" {
		req.Header.Set(middleware.ClientVersionHeader, version)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRequireClientVersion_DisabledPassesThrough(t *testing.T) {
	r, called := clientVersionRouter("")
	w := requestClientVersion(r, "")

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, *called)
}

func TestRequireClientVersion_RejectsMissingMalformedAndOlderVersions(t *testing.T) {
	for _, supplied := range []string{"", "v0.2.44", "0.2.43"} {
		t.Run(supplied, func(t *testing.T) {
			r, called := clientVersionRouter("0.2.44")
			w := requestClientVersion(r, supplied)

			require.Equal(t, http.StatusForbidden, w.Code)
			assert.False(t, *called)

			var response attestation.ErrorResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			assert.Equal(t, attestation.ErrVersionTooOld, response.Code)
			assert.Equal(t, "0.2.44", response.RequiredMinVersion)
			if supplied != "" {
				assert.NotContains(t, w.Body.String(), supplied)
			}
		})
	}
}

func TestRequireClientVersion_EqualAndNewerPassThrough(t *testing.T) {
	for _, supplied := range []string{"0.2.44", "0.2.45", "0.3.0", "1.0.0"} {
		t.Run(supplied, func(t *testing.T) {
			r, called := clientVersionRouter("0.2.44")
			w := requestClientVersion(r, supplied)

			assert.Equal(t, http.StatusNoContent, w.Code)
			assert.True(t, *called)
		})
	}
}

func TestRequireClientVersion_PilotCompatibility(t *testing.T) {
	for _, tc := range []struct {
		client, floor string
		allowed       bool
	}{
		{"1.0.0-pilot.0", "1.0.0", true},
		{"1.1.0-pilot.2", "1.0.0", true},
		{"0.9.0-pilot.99", "1.0.0", false},
		{"1.1.1-pilot.2", "1.0.0", false},
		{"1.1.0-pilot.01", "1.0.0", false},
		{"1.1.0-pilot.2", "1.0.0-pilot.0", false},
	} {
		t.Run(tc.client+"/"+tc.floor, func(t *testing.T) {
			router, called := clientVersionRouter(tc.floor)
			response := requestClientVersion(router, tc.client)
			if tc.allowed {
				assert.Equal(t, http.StatusNoContent, response.Code)
			} else {
				assert.Equal(t, http.StatusForbidden, response.Code)
			}
			assert.Equal(t, tc.allowed, *called)
		})
	}
}

func requestClientPlatform(minimums middleware.ClientMinimums, platform, version string) (*httptest.ResponseRecorder, bool) {
	r := gin.New()
	called := false
	r.GET("/test", middleware.RequireClientVersion(minimums), func(c *gin.Context) {
		called = true
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(middleware.ClientPlatformHeader, platform)
	if version != "" {
		req.Header.Set(middleware.ClientVersionHeader, version)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, called
}

func TestRequireClientVersion_PlatformMinimums(t *testing.T) {
	minimums := middleware.ClientMinimums{Desktop: "0.2.46", IOS: "1.0.0", Android: "1.2.0"}
	for _, tc := range []struct {
		name, platform, version string
		wantStatus              int
		wantMinimum             string
	}{
		{"desktop below its floor", "", "0.2.45", http.StatusForbidden, "0.2.46"},
		{"desktop at its floor", "", "0.2.46", http.StatusNoContent, ""},
		{"ios ignores desktop floor", "ios", "1.0.0", http.StatusNoContent, ""},
		{"ios below its floor", "ios", "0.9.9", http.StatusForbidden, "1.0.0"},
		{"ios missing version", "ios", "", http.StatusForbidden, "1.0.0"},
		{"android below its floor", "android", "1.1.9", http.StatusForbidden, "1.2.0"},
		{"android at its floor", "android", "1.2.0", http.StatusNoContent, ""},
		{"unknown platform", "web", "9.9.9", http.StatusBadRequest, ""},
		{"platform is case-sensitive", "iOS", "1.0.0", http.StatusBadRequest, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, called := requestClientPlatform(minimums, tc.platform, tc.version)

			require.Equal(t, tc.wantStatus, w.Code)
			assert.Equal(t, tc.wantStatus == http.StatusNoContent, called)
			if tc.wantStatus == http.StatusForbidden {
				var response attestation.ErrorResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				assert.Equal(t, attestation.ErrVersionTooOld, response.Code)
				assert.Equal(t, tc.wantMinimum, response.RequiredMinVersion)
			}
		})
	}
}

func TestRequireClientVersion_EmptyPlatformMinimumSkipsCheck(t *testing.T) {
	w, called := requestClientPlatform(middleware.ClientMinimums{Desktop: "0.2.46"}, "android", "")

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, called)
}

func TestRequireClientVersion_UnknownPlatformRejectedWhenDisabled(t *testing.T) {
	w, called := requestClientPlatform(middleware.ClientMinimums{}, "web", "")

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.False(t, called)
}

func TestClientMinimumsFromConfig(t *testing.T) {
	cfg := &config.Config{ClientMinVersion: "0.2.46", ClientMinVersionIOS: "1.0.0", ClientMinVersionAndroid: "1.1.0"}

	assert.Equal(t, middleware.ClientMinimums{Desktop: "0.2.46", IOS: "1.0.0", Android: "1.1.0"}, middleware.ClientMinimumsFromConfig(cfg))
}
