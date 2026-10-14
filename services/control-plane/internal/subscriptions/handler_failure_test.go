package subscriptions_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/subscriptions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A closed pool refuses queries without connecting to any database. An unavailable
// status read must not claim the user has a free or premium subscription.
func TestGetMe_DBErrorReturnsUnavailable(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	w := statusResponse(db)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, map[string]any{"error": "subscription status unavailable"}, resp)
}

// Return a malformed period-end value at the SQL driver boundary so the real
// database/sql Scan fails after reading a row. No network or fixture DB is needed.
func TestGetMe_ScanFailureReturnsUnavailable(t *testing.T) {
	db := sql.OpenDB(statusConnector{values: []driver.Value{"premium", "active", "code", "invalid-time"}})
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	w := statusResponse(db)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, map[string]any{"error": "subscription status unavailable"}, resp)
}

func TestGetMe_NoRowsRemainsFree(t *testing.T) {
	db := sql.OpenDB(statusConnector{})
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	w := statusResponse(db)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, map[string]any{"tier": "free", "status": "none"}, resp)
}

func statusResponse(db *sql.DB) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := subscriptions.NewHandler(db, logger.New("test"))
	r.GET("/subscriptions/me", func(c *gin.Context) {
		c.Set("user_id", "11111111-1111-4111-8111-111111111111")
		h.GetMe(c)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/subscriptions/me", nil))
	return w
}

type statusConnector struct{ values []driver.Value }

func (c statusConnector) Connect(context.Context) (driver.Conn, error) {
	return statusConn(c), nil
}
func (statusConnector) Driver() driver.Driver { return statusDriver{} }

type statusDriver struct{}

func (statusDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use the test connector")
}

type statusConn struct{ values []driver.Value }

func (statusConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("test connection does not prepare statements")
}
func (statusConn) Close() error { return nil }
func (statusConn) Begin() (driver.Tx, error) {
	return nil, errors.New("test connection does not begin transactions")
}
func (c statusConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &statusRows{values: c.values}, nil
}

type statusRows struct{ values []driver.Value }

func (statusRows) Columns() []string {
	return []string{"tier", "status", "source", "current_period_end"}
}
func (statusRows) Close() error { return nil }
func (r *statusRows) Next(dest []driver.Value) error {
	if r.values == nil {
		return io.EOF
	}
	copy(dest, r.values)
	r.values = nil
	return nil
}
