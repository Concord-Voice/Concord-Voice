package users_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ambiguousKeyResetCommitDriverName = "ambiguous-key-reset-commit-test"

var ambiguousKeyResetCommitDriverOnce sync.Once

type ambiguousKeyResetCommitDriver struct{}

func (ambiguousKeyResetCommitDriver) Open(passwordHash string) (driver.Conn, error) {
	return &ambiguousKeyResetCommitConn{passwordHash: passwordHash}, nil
}

type ambiguousKeyResetCommitConn struct {
	passwordHash string
}

func (*ambiguousKeyResetCommitConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*ambiguousKeyResetCommitConn) Close() error { return nil }

func (*ambiguousKeyResetCommitConn) Begin() (driver.Tx, error) {
	return &ambiguousKeyResetCommitTx{}, nil
}

func (*ambiguousKeyResetCommitConn) ExecContext(
	context.Context,
	string,
	[]driver.NamedValue,
) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

func (c *ambiguousKeyResetCommitConn) QueryContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	if strings.Contains(query, "mfa_enabled") {
		return &singleMFAStateRow{}, nil
	}
	return &singlePasswordHashRow{passwordHash: c.passwordHash}, nil
}

type ambiguousKeyResetCommitTx struct{}

func (*ambiguousKeyResetCommitTx) Commit() error {
	return errors.New("forced ambiguous key-reset commit")
}

func (*ambiguousKeyResetCommitTx) Rollback() error { return nil }

type singlePasswordHashRow struct {
	passwordHash string
	delivered    bool
}

func (*singlePasswordHashRow) Columns() []string { return []string{"password_hash"} }

func (*singlePasswordHashRow) Close() error { return nil }

func (r *singlePasswordHashRow) Next(values []driver.Value) error {
	if r.delivered {
		return io.EOF
	}
	r.delivered = true
	values[0] = r.passwordHash
	return nil
}

type singleMFAStateRow struct {
	delivered bool
}

func (*singleMFAStateRow) Columns() []string { return []string{"mfa_enabled", "mfa_methods"} }

func (*singleMFAStateRow) Close() error { return nil }

func (r *singleMFAStateRow) Next(values []driver.Value) error {
	if r.delivered {
		return io.EOF
	}
	r.delivered = true
	values[0] = false
	values[1] = "{}"
	return nil
}

type keyResetFailClosedObserver struct {
	events []string
}

func (o *keyResetFailClosedObserver) ClearCustomTextForPresenceAudience(uuid.UUID) {
	o.events = append(o.events, "clear")
}

func (o *keyResetFailClosedObserver) DisconnectUser(uuid.UUID) {
	o.events = append(o.events, "disconnect")
}

func TestReplaceMyKeysCommitAmbiguityClearsAndDisconnectsFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const currentPassword = "CurrentPassword123!" // pragma: allowlist secret
	passwordHash, err := auth.HashPassword(currentPassword)
	require.NoError(t, err)

	ambiguousKeyResetCommitDriverOnce.Do(func() {
		sql.Register(ambiguousKeyResetCommitDriverName, ambiguousKeyResetCommitDriver{})
	})
	db, err := sql.Open(ambiguousKeyResetCommitDriverName, passwordHash)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	observer := &keyResetFailClosedObserver{}
	handler := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
	users.SetCustomTextResetBroadcasterForTest(handler, observer)
	users.SetKeyResetSessionDisconnectorForTest(handler, observer)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", uuid.NewString())
	c.Request = httptest.NewRequest(http.MethodPut, urlUsersMeKeys, bytes.NewBufferString(`{
		"wrapped_private_key":"a2V5",
		"key_derivation_salt":"c2FsdA==",
		"public_key":"cHVibGlj",
		"current_password":"CurrentPassword123!",
		"acknowledge_data_loss":true
	}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ReplaceMyKeys(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, []string{"clear", "disconnect"}, observer.events,
		"possibly committed key replacement must clear presence before disconnecting sessions")
}
