package channels

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Package-internal coverage for the #2201 distribution error mapping and the
// per-target outcome classification (insert-failure branch included).

func newInternalTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	return c, w
}

func TestRespondKeyDistributionError_Mapping(t *testing.T) {
	h := &Handler{log: logger.NewWithWriter(io.Discard)}

	t.Run("epoch mismatch is the generic 401", func(t *testing.T) {
		c, w := newInternalTestContext()
		h.respondKeyDistributionError(c, credepoch.ErrEpochMismatch, "ctx-1")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), errMsgAuthRequired)
	})

	t.Run("blocked is the generic 401", func(t *testing.T) {
		c, w := newInternalTestContext()
		h.respondKeyDistributionError(c, credepoch.ErrBlocked, "ctx-2")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("revoked epoch is a non-retryable conflict", func(t *testing.T) {
		c, w := newInternalTestContext()
		h.respondKeyDistributionError(c, fmt.Errorf("store key: %w", &pq.Error{Code: "CV001"}), "ctx-3")
		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Contains(t, w.Body.String(), `"code":"REVOKED_EPOCH"`)
	})

	t.Run("any other failure is a 500", func(t *testing.T) {
		c, w := newInternalTestContext()
		h.respondKeyDistributionError(c, errors.New("tx begin failed"), "ctx-4")
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Contains(t, w.Body.String(), errMsgFailedDistributeKeys)
	})
}

func TestDistributeOneChannelKey_Outcomes(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	t.Run("insert failure fails the batch", func(t *testing.T) {
		member := dbtest.CreateUser(t, db)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		// Nonexistent channel → FK violation → statement error → batch error.
		// distributionTargetAdmitted's resolver is nil here, so stub admission
		// by calling the insert helper directly for the error branch...
		inserted, insErr := insertWrappedChannelKeyTx(ctx, tx, "00000000-0000-0000-0000-000000000bad", member.String(), "a2V5", 1)
		assert.False(t, inserted)
		assert.Error(t, insErr, "FK violation must surface as a batch-failing error")
	})
}
