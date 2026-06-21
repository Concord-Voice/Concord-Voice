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

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// TestUpdatePrivacySettingsDBError covers the consolidated transaction-error path
// of the #1674 fix: when the DB is unavailable, h.db.Begin() fails inside the
// closure and the handler must respond 500 (not panic, not 200). UpdatePrivacySettings
// uses only h.db and h.log, so a nil hub / nil MFA verifier are safe here.
func TestUpdatePrivacySettingsDBError(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	cleanup() // close the pool immediately so tx.Begin() fails

	// nil tier resolver is safe: UpdatePrivacySettings never resolves entitlements (#1298).
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)

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
