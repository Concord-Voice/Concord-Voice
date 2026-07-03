package subscriptions_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/subscriptions"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestHandler builds the status handler over a migrated test DB and a gin
// engine, plus a fixtures DB handle on the same database.
func newTestHandler(t *testing.T) (*subscriptions.Handler, *gin.Engine, *sql.DB) {
	t.Helper()
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	h := subscriptions.NewHandler(db, logger.New("test"))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	return h, r, db
}

// authAs injects a user_id into the gin context, mimicking AuthRequired.
func authAs(userID uuid.UUID) gin.HandlerFunc {
	return func(c *gin.Context) { c.Set("user_id", userID.String()) }
}

func doGet(r *gin.Engine, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, bytes.NewReader(nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// insertSubscription writes a subscriptions row directly for fixture setup.
func insertSubscription(t *testing.T, db *sql.DB, userID uuid.UUID, tier, status, source string, periodEnd *time.Time) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO subscriptions (user_id, tier, status, source, current_period_end)
		 VALUES ($1, $2, $3, $4, $5)`,
		userID, tier, status, source, periodEnd,
	)
	require.NoError(t, err)
}

// TestGetMe_ActivePremiumFromCode: a live premium row created by a Kickstarter
// code redemption renders its tier/status/source/expiry.
func TestGetMe_ActivePremiumFromCode(t *testing.T) {
	h, r, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	expiry := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	insertSubscription(t, db, user, "premium", "active", "code", &expiry)

	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "premium", resp.Tier)
	assert.Equal(t, "active", resp.Status)
	assert.Equal(t, "code", resp.Source)
	require.NotNil(t, resp.CurrentPeriodEnd)
	assert.Equal(t, expiry.Format("2006-01-02T15:04:05Z07:00"), *resp.CurrentPeriodEnd)
}

// TestGetMe_NoRowFreeDefault: a user with no subscription row gets the
// free-default shape (tier=free, status=none, no expiry).
func TestGetMe_NoRowFreeDefault(t *testing.T) {
	h, r, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)

	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "free", resp.Tier)
	assert.Equal(t, "none", resp.Status)
	assert.Empty(t, resp.Source)
	assert.Nil(t, resp.CurrentPeriodEnd)
}

// TestGetMe_ExpiredRowFreeDefault: a row whose current_period_end has passed is
// NOT live (the query's period predicate excludes it) → free default.
func TestGetMe_ExpiredRowFreeDefault(t *testing.T) {
	h, r, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	past := time.Now().Add(-time.Hour).UTC()
	insertSubscription(t, db, user, "premium", "active", "code", &past)

	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "free", resp.Tier)
	assert.Equal(t, "none", resp.Status)
}

// TestGetMe_CanceledRowFreeDefault: a canceled status is not in the live set →
// free default (the status predicate excludes it).
func TestGetMe_CanceledRowFreeDefault(t *testing.T) {
	h, r, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	insertSubscription(t, db, user, "premium", "canceled", "stripe", nil)

	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "free", resp.Tier)
	assert.Equal(t, "none", resp.Status)
}

// TestGetMe_NullPeriodEndActive: a live premium row with a NULL expiry (the
// kickstarter/code Beta shape) renders active with no expiry field.
func TestGetMe_NullPeriodEndActive(t *testing.T) {
	h, r, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	insertSubscription(t, db, user, "premium", "active", "kickstarter", nil)

	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "premium", resp.Tier)
	assert.Equal(t, "active", resp.Status)
	assert.Equal(t, "kickstarter", resp.Source)
	assert.Nil(t, resp.CurrentPeriodEnd)
}

// TestGetMe_DBErrorDegradesToFree: a closed DB handle forces a query error; the
// handler degrades to the free default (200) rather than fabricating premium or
// returning 500.
func TestGetMe_DBErrorDegradesToFree(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	cleanup() // close the pool so the query errors
	h := subscriptions.NewHandler(db, logger.New("test"))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	user := uuid.New()

	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "free", resp.Tier)
	assert.Equal(t, "none", resp.Status)
}
