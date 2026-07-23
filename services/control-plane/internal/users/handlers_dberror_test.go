package users_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// TestUpdatePrivacySettingsDBError covers the consolidated transaction-error path
// of the #1674 fix: when the DB is unavailable, h.db.Begin() fails inside the
// closure and the handler must respond 500 (not panic, not 200). UpdatePrivacySettings
// uses only h.db and h.log, so a nil hub / nil MFA verifier are safe here.
func TestUpdatePrivacySettingsDBError(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	cleanup() // close the pool immediately so tx.Begin() fails

	// nil tier resolver is safe: UpdatePrivacySettings never resolves entitlements (#1298).
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", uuid.NewString())
	c.Request = httptest.NewRequest(
		http.MethodPatch, "/api/v1/users/me/privacy",
		strings.NewReader(`{"searchable_by_username": true}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpdatePrivacySettings(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestGetPresenceOverridesDBError(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	cleanup()
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	if err := service.BindDelivery(immediatePresenceDelivery{}); err != nil {
		t.Fatal(err)
	}
	h.SetPresenceHistory(service)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", uuid.NewString())
	c.Params = gin.Params{{Key: "category", Value: "custom_text"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/users/me/presence-overrides/custom_text", nil)

	h.GetPresenceOverrides(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestReplacePresenceOverridesDBError(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	cleanup()
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	if err := service.BindDelivery(immediatePresenceDelivery{}); err != nil {
		t.Fatal(err)
	}
	h.SetPresenceHistory(service)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", uuid.NewString())
	c.Params = gin.Params{{Key: "category", Value: "custom_text"}}
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/v1/users/me/presence-overrides/custom_text",
		strings.NewReader(`{"encrypted_data":"YQ==","expected_version":0,"excluded_user_ids":[]}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.ReplacePresenceOverrides(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestUpdatePresenceSettingsDBError(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	cleanup()
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	if err := service.BindDelivery(immediatePresenceDelivery{}); err != nil {
		t.Fatal(err)
	}
	h.SetPresenceHistory(service)
	bindNoopActivitySettingsSuppressor(h)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", uuid.NewString())
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/api/v1/users/me/presence-settings",
		strings.NewReader(`{"custom_text_tier":1,"custom_text":"must not commit"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpdatePresenceSettings(c)

	// Activity cleanup now runs before Custom Status readiness, so its
	// retryable database failure is the first authoritative error.
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}
