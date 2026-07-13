package presencehistory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerStrictSettingsDecoder(t *testing.T) {
	validHash := strings.Repeat("a", 64)
	valid := `{"enabled":true,"retention_days":30,"acknowledged":true,"consent_version":1,"consent_copy_hash":"` + validHash + `"}`
	request, err := DecodeUpdateSettingsRequest(strings.NewReader(valid))
	require.NoError(t, err)
	require.NotNil(t, request.Enabled)
	assert.True(t, *request.Enabled)
	require.NotNil(t, request.RetentionDays)
	assert.Equal(t, int16(30), *request.RetentionDays)

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "empty object", body: `{}`},
		{name: "unknown", body: `{"unknown":true}`},
		{name: "case alias", body: `{"Enabled":true}`},
		{name: "duplicate", body: `{"enabled":true,"enabled":false}`},
		{name: "trailing", body: `{"enabled":false}{}`},
		{name: "bad type", body: `{"enabled":"false"}`},
		{name: "empty input", body: ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeUpdateSettingsRequest(strings.NewReader(tc.body))
			require.Error(t, err)
		})
	}
	for _, field := range []string{
		"enabled",
		"retention_days",
		"acknowledged",
		"consent_version",
		"consent_copy_hash",
	} {
		t.Run("null "+field, func(t *testing.T) {
			_, err := DecodeUpdateSettingsRequest(strings.NewReader(`{"` + field + `":null}`))
			require.Error(t, err)
		})
	}
}

func TestHandlerRegisterRoutesRelativeWithReadAndMutationLimiters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	users := router.Group("/api/v1/users")
	readLimiter := func(c *gin.Context) {
		c.Header("X-Test-Limiter", "read-30")
		c.AbortWithStatus(http.StatusNoContent)
	}
	mutationLimiter := func(c *gin.Context) {
		c.Header("X-Test-Limiter", "mutation-10")
		c.AbortWithStatus(http.StatusAccepted)
	}
	NewHandler(nil).RegisterRoutes(users, readLimiter, mutationLimiter)

	wants := []struct {
		method  string
		path    string
		status  int
		limiter string
	}{
		{method: http.MethodGet, path: "/api/v1/users/me/presence-history/settings", status: http.StatusNoContent, limiter: "read-30"},
		{method: http.MethodPatch, path: "/api/v1/users/me/presence-history/settings", status: http.StatusAccepted, limiter: "mutation-10"},
		{method: http.MethodGet, path: "/api/v1/users/me/presence-history", status: http.StatusNoContent, limiter: "read-30"},
		{method: http.MethodDelete, path: "/api/v1/users/me/presence-history", status: http.StatusAccepted, limiter: "mutation-10"},
	}
	for _, want := range wants {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(want.method, want.path, nil))
		assert.Equal(t, want.status, response.Code, "%s %s", want.method, want.path)
		assert.Equal(t, want.limiter, response.Header().Get("X-Test-Limiter"))
	}

	for _, path := range []string{
		"/api/v1/users/users/me/presence-history/settings",
		"/api/v1/users/" + uuid.NewString() + "/presence-history/settings",
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, response.Code, path)
	}
}

func TestHandlerSelfOnlyRoutesAndNoStoreHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ts := setupPresenceHistoryTestServer(t)
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	service := NewService(ts.DB, disclosure, true)
	user := ts.CreateTestUser(t, "history_handler")
	userID := uuid.MustParse(user.ID)
	router := historyTestRouter(service, userID.String())

	settingsRequest := httptest.NewRequest(http.MethodGet,
		"/api/v1/users/me/presence-history/settings", nil)
	settingsResponse := httptest.NewRecorder()
	router.ServeHTTP(settingsResponse, settingsRequest)
	assert.Equal(t, http.StatusOK, settingsResponse.Code)

	otherPath := httptest.NewRequest(http.MethodGet,
		"/api/v1/users/"+uuid.NewString()+"/presence-history/settings", nil)
	otherResponse := httptest.NewRecorder()
	router.ServeHTTP(otherResponse, otherPath)
	assert.Equal(t, http.StatusNotFound, otherResponse.Code,
		"there must be no route accepting a target user")

	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	historyID := insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"self only"}`, now, now.Add(time.Second), now, now.Add(time.Hour))
	historyRequest := httptest.NewRequest(http.MethodGet,
		"/api/v1/users/me/presence-history?limit=1", nil)
	historyResponse := httptest.NewRecorder()
	router.ServeHTTP(historyResponse, historyRequest)
	assert.Equal(t, http.StatusOK, historyResponse.Code)
	assert.Equal(t, "no-store", historyResponse.Header().Get("Cache-Control"))
	var page HistoryPage
	require.NoError(t, json.Unmarshal(historyResponse.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, historyID, page.Items[0].ID)
}

func TestHandlerInvalidCursorLimitDeleteAndGenericErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ts := setupPresenceHistoryTestServer(t)
	user := ts.CreateTestUser(t, "history_handler_errors")
	userID := uuid.MustParse(user.ID)
	service := NewService(ts.DB, BuildDisclosure(DisclosureOptions{InstanceType: "saas"}), true)
	router := historyTestRouter(service, userID.String())

	for _, target := range []string{
		"/api/v1/users/me/presence-history?limit=0",
		"/api/v1/users/me/presence-history?limit=101",
		"/api/v1/users/me/presence-history?before=not-a-cursor",
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		assert.Equal(t, http.StatusBadRequest, response.Code, target)
	}

	deleteResponse := httptest.NewRecorder()
	router.ServeHTTP(deleteResponse, httptest.NewRequest(http.MethodDelete,
		"/api/v1/users/me/presence-history", nil))
	assert.Equal(t, http.StatusNoContent, deleteResponse.Code)
	assert.Zero(t, settingsRowCount(t, ts.DB, userID))

	db, err := sql.Open("postgres", "postgres://127.0.0.1:1/unreachable?sslmode=disable")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	broken := historyTestRouter(NewService(db,
		BuildDisclosure(DisclosureOptions{InstanceType: "saas"}), true), userID.String())
	response := httptest.NewRecorder()
	broken.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/v1/users/me/presence-history/settings", nil))
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.NotContains(t, response.Body.String(), "sql")
	assert.NotContains(t, response.Body.String(), "closed")
}

func TestHandlerPatchAndMissingSubject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ts := setupPresenceHistoryTestServer(t)
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	service := NewService(ts.DB, disclosure, true)
	user := ts.CreateTestUser(t, "history_handler_patch")
	userID := uuid.MustParse(user.ID)
	router := historyTestRouter(service, userID.String())
	body, err := json.Marshal(currentEnableRequest(disclosure, 30))
	require.NoError(t, err)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch,
		"/api/v1/users/me/presence-history/settings", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusOK, response.Code)

	missing := gin.New()
	NewHandler(service).RegisterRoutes(
		missing.Group("/api/v1/users"),
		passthroughHistoryMiddleware,
		passthroughHistoryMiddleware,
	)
	missingResponse := httptest.NewRecorder()
	missing.ServeHTTP(missingResponse, httptest.NewRequest(http.MethodGet,
		"/api/v1/users/me/presence-history/settings", nil))
	assert.Equal(t, http.StatusUnauthorized, missingResponse.Code)
}

func historyTestRouter(service *Service, userID string) *gin.Engine {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", userID)
		c.Next()
	})
	NewHandler(service).RegisterRoutes(
		router.Group("/api/v1/users"),
		passthroughHistoryMiddleware,
		passthroughHistoryMiddleware,
	)
	return router
}

func passthroughHistoryMiddleware(c *gin.Context) { c.Next() }
