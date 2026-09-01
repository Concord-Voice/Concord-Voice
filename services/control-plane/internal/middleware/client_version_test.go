package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clientVersionRouter(minimum string) (*gin.Engine, *bool) {
	r := gin.New()
	called := new(bool)
	r.GET("/test", middleware.RequireClientVersion(minimum), func(c *gin.Context) {
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
