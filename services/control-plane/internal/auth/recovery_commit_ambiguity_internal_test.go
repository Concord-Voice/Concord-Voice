package auth

import (
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
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errForcedAmbiguousRecoveryCommit = errors.New("forced ambiguous recovery commit")

const ambiguousRecoveryCommitDriverName = "ambiguous-recovery-commit-test"

var ambiguousRecoveryCommitDriverOnce sync.Once

type ambiguousRecoveryCommitDriver struct{}

func (ambiguousRecoveryCommitDriver) Open(string) (driver.Conn, error) {
	return &ambiguousRecoveryCommitConn{}, nil
}

type ambiguousRecoveryCommitConn struct{}

func (*ambiguousRecoveryCommitConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*ambiguousRecoveryCommitConn) Close() error { return nil }

func (*ambiguousRecoveryCommitConn) Begin() (driver.Tx, error) {
	return &ambiguousRecoveryCommitTx{}, nil
}

func (*ambiguousRecoveryCommitConn) ExecContext(
	context.Context,
	string,
	[]driver.NamedValue,
) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

type ambiguousRecoveryCommitTx struct{}

func (*ambiguousRecoveryCommitTx) Commit() error {
	return errForcedAmbiguousRecoveryCommit
}

func (*ambiguousRecoveryCommitTx) Rollback() error { return nil }

type ambiguousRecoveryMFAChecker struct {
	claims *RecoveryClaims
}

func (*ambiguousRecoveryMFAChecker) IsEnabled(context.Context, string) bool { return false }

func (*ambiguousRecoveryMFAChecker) GetEnabledMethods(context.Context, string) ([]string, error) {
	return nil, nil
}

func (*ambiguousRecoveryMFAChecker) GetLoginMethods(context.Context, string) ([]string, error) {
	return nil, nil
}

func (*ambiguousRecoveryMFAChecker) GenerateLoginChallenge(context.Context, string, bool) (string, string, error) {
	return "", "", nil
}

func (*ambiguousRecoveryMFAChecker) GenerateUpgradeChallenge(context.Context, string, bool) (string, string, error) {
	return "", "", nil
}

func (*ambiguousRecoveryMFAChecker) BeginWebAuthnLogin(context.Context, string, string) (interface{}, error) {
	return nil, nil
}

func (*ambiguousRecoveryMFAChecker) GenerateRecoveryToken(string) (string, string, error) {
	return "", "", nil
}

func (c *ambiguousRecoveryMFAChecker) ValidateRecoveryToken(string) (*RecoveryClaims, error) {
	return c.claims, nil
}

func openAmbiguousRecoveryCommitDB(t *testing.T) *sql.DB {
	t.Helper()
	ambiguousRecoveryCommitDriverOnce.Do(func() {
		sql.Register(ambiguousRecoveryCommitDriverName, ambiguousRecoveryCommitDriver{})
	})
	db, err := sql.Open(ambiguousRecoveryCommitDriverName, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func TestRecoveryCommitAmbiguityConsumesTokenAndFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name    string
		path    string
		body    string
		handler func(*Handler, *gin.Context)
	}{
		{
			name: "password recovery",
			path: "/api/v1/auth/recovery/reset-password",
			body: `{
				"recovery_token":"token",
				"new_password":"NewSecurePassword123!",
				"wrapped_private_key":"a2V5",
				"key_derivation_salt":"c2FsdA==",
				"key_derivation_alg":"argon2id"
			}`,
			handler: func(h *Handler, c *gin.Context) { h.RecoveryResetPassword(c) },
		},
		{
			name: "account recovery",
			path: "/api/v1/auth/recovery/reset-account",
			body: `{
				"recovery_token":"token",
				"new_password":"NewSecurePassword123!",
				"wrapped_private_key":"a2V5",
				"key_derivation_salt":"c2FsdA==",
				"key_derivation_alg":"argon2id",
				"public_key":"cHVibGlj",
				"acknowledge_data_loss":true
			}`,
			handler: func(h *Handler, c *gin.Context) { h.RecoveryResetAccount(c) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rdb := setupAuthAttemptRedis(t)
			userID := uuid.New()
			claims := &RecoveryClaims{UserID: userID.String(), JTI: uuid.NewString()}
			hub := &recordingRecoveryResetHub{}
			h := &Handler{
				db:         openAmbiguousRecoveryCommitDB(t),
				redis:      rdb,
				log:        logger.NewWithWriter(io.Discard),
				hub:        hub,
				mfaChecker: &ambiguousRecoveryMFAChecker{claims: claims},
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			c.Request.Header.Set("Content-Type", "application/json")

			tt.handler(h, c)

			assert.Equal(t, http.StatusInternalServerError, w.Code)
			usedKey := "recovery_token_used:" + claims.JTI
			exists, err := rdb.Exists(context.Background(), usedKey).Result()
			require.NoError(t, err)
			assert.Equal(t, int64(1), exists,
				"an ambiguously committed recovery token must remain consumed")
			assert.Equal(t, []string{"clear", "disconnect"}, hub.events,
				"possibly committed destructive recovery must clear presence before disconnecting sessions")
		})
	}
}
