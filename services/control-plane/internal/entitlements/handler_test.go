package entitlements_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertActiveSubscription INSERTs one active, never-expiring subscription row
// for the user at the given tier (Kickstarter-sourced, matching the Beta entry
// path). current_period_end is NULL so ResolveTier treats it as live.
func insertActiveSubscription(t *testing.T, ts *testhelpers.TestServer, userID, tier string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO subscriptions (user_id, tier, status, source, current_period_end)
		 VALUES ($1, $2, 'active', 'kickstarter', NULL)`,
		userID, tier,
	)
	require.NoError(t, err)
}

func doGet(_ *testing.T, h *entitlements.HTTPHandler, userID string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/entitlements", nil)
	c.Set("user_id", userID)
	h.Get(c)
	return w
}

func TestGet_NoSubscription_ReturnsFreeSet(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	h := entitlements.NewHTTPHandler(ts.DB, ts.Redis, logger.New("test"))

	w := doGet(t, h, uid)

	require.Equal(t, http.StatusOK, w.Code)
	var dto entitlements.EntitlementDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &dto))
	assert.Equal(t, "free", dto.Tier)
	assert.Equal(t, 5120, dto.MaxMessageChars)
}

func TestGet_PremiumSubscription_ReturnsPremiumSet(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	insertActiveSubscription(t, ts, uid, "premium")
	h := entitlements.NewHTTPHandler(ts.DB, ts.Redis, logger.New("test"))

	w := doGet(t, h, uid)

	require.Equal(t, http.StatusOK, w.Code)
	var dto entitlements.EntitlementDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &dto))
	assert.Equal(t, "premium", dto.Tier)
	assert.True(t, dto.AllowMusicMode)
}
