package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathRequestIDTest = "/reqid-test"
)

func setupRequestIDRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.RequestID())
	return router
}

func TestRequestIDGeneratesUUID(t *testing.T) {
	router := setupRequestIDRouter()

	var capturedID string
	router.GET(pathRequestIDTest, func(c *gin.Context) {
		val, exists := c.Get(middleware.RequestIDContextKey)
		require.True(t, exists)
		capturedID = val.(string)
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathRequestIDTest, nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	responseID := w.Header().Get(middleware.RequestIDHeader)
	assert.NotEmpty(t, responseID)

	// Verify it's a valid UUID
	_, err := uuid.Parse(responseID)
	assert.NoError(t, err, "response X-Request-ID should be a valid UUID")

	// Context value should match response header
	assert.Equal(t, responseID, capturedID)
}

func TestRequestIDReusesClientID(t *testing.T) {
	router := setupRequestIDRouter()

	clientID := "client-provided-request-id-12345"
	var capturedID string
	router.GET(pathRequestIDTest, func(c *gin.Context) {
		val, _ := c.Get(middleware.RequestIDContextKey)
		capturedID = val.(string)
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathRequestIDTest, nil)
	req.Header.Set(middleware.RequestIDHeader, clientID)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, clientID, w.Header().Get(middleware.RequestIDHeader))
	assert.Equal(t, clientID, capturedID)
}

func TestRequestIDEmptyHeaderGeneratesNew(t *testing.T) {
	router := setupRequestIDRouter()
	router.GET(pathRequestIDTest, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathRequestIDTest, nil)
	req.Header.Set(middleware.RequestIDHeader, "")
	router.ServeHTTP(w, req)

	responseID := w.Header().Get(middleware.RequestIDHeader)
	assert.NotEmpty(t, responseID)

	_, err := uuid.Parse(responseID)
	assert.NoError(t, err, "empty header should result in a new UUID")
}

func TestRequestIDUniquePerRequest(t *testing.T) {
	router := setupRequestIDRouter()
	router.GET(pathRequestIDTest, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w1 := httptest.NewRecorder()
	req1 := httptest.NewRequest("GET", pathRequestIDTest, nil)
	router.ServeHTTP(w1, req1)

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", pathRequestIDTest, nil)
	router.ServeHTTP(w2, req2)

	id1 := w1.Header().Get(middleware.RequestIDHeader)
	id2 := w2.Header().Get(middleware.RequestIDHeader)

	assert.NotEmpty(t, id1)
	assert.NotEmpty(t, id2)
	assert.NotEqual(t, id1, id2, "each request should get a unique ID")
}

func TestRequestIDAvailableInContext(t *testing.T) {
	router := setupRequestIDRouter()

	var contextValue interface{}
	var exists bool
	router.GET(pathRequestIDTest, func(c *gin.Context) {
		contextValue, exists = c.Get(middleware.RequestIDContextKey)
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathRequestIDTest, nil)
	router.ServeHTTP(w, req)

	assert.True(t, exists, "request_id should be set in context")
	assert.IsType(t, "", contextValue, "request_id should be a string")
	assert.NotEmpty(t, contextValue.(string))
}

func TestRequestIDInHealthCheckResponse(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/health", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	responseID := w.Header().Get(middleware.RequestIDHeader)
	assert.NotEmpty(t, responseID, "health endpoint should return X-Request-ID")

	_, err := uuid.Parse(responseID)
	assert.NoError(t, err)
}

func TestRequestIDRejectsControlChars(t *testing.T) {
	router := setupRequestIDRouter()
	router.GET(pathRequestIDTest, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathRequestIDTest, nil)
	req.Header.Set(middleware.RequestIDHeader, "id-with-\nnewline")
	router.ServeHTTP(w, req)

	responseID := w.Header().Get(middleware.RequestIDHeader)
	assert.NotEqual(t, "id-with-\nnewline", responseID, "control chars should be rejected")
	_, err := uuid.Parse(responseID)
	assert.NoError(t, err, "should generate a fresh UUID when control chars present")
}

func TestRequestIDRejectsTooLong(t *testing.T) {
	router := setupRequestIDRouter()
	router.GET(pathRequestIDTest, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	longIDBytes := make([]byte, 200)
	for i := range longIDBytes {
		longIDBytes[i] = 'a'
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathRequestIDTest, nil)
	req.Header.Set(middleware.RequestIDHeader, string(longIDBytes))
	router.ServeHTTP(w, req)

	responseID := w.Header().Get(middleware.RequestIDHeader)
	assert.NotEqual(t, string(longIDBytes), responseID, "too-long ID should be rejected")
	_, err := uuid.Parse(responseID)
	assert.NoError(t, err, "should generate a fresh UUID when ID too long")
}

func TestRequestIDAcceptsNginxHexID(t *testing.T) {
	router := setupRequestIDRouter()
	router.GET(pathRequestIDTest, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// nginx $request_id is a 32-char hex string
	nginxID := "abcdef0123456789abcdef0123456789"
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathRequestIDTest, nil)
	req.Header.Set(middleware.RequestIDHeader, nginxID)
	router.ServeHTTP(w, req)

	assert.Equal(t, nginxID, w.Header().Get(middleware.RequestIDHeader), "valid nginx hex ID should be preserved")
}

func TestRequestIDPreservedAcrossMiddlewareChain(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "reqidchain")

	w := ts.DoRequest("GET", "/api/v1/users/me", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	responseID := w.Header().Get(middleware.RequestIDHeader)
	assert.NotEmpty(t, responseID, "authenticated routes should also return X-Request-ID")
}
