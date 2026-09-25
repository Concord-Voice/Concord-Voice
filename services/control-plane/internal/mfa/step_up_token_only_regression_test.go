package mfa_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readBackupEmail returns the user's backup_email, treating a NULL column
// (the "cleared" state) as an empty string — matching GetBackupEmail's own
// HTTP-level semantics.
func readBackupEmail(t *testing.T, ts *testhelpers.TestServer, userID string) string {
	t.Helper()

	var backupEmail sql.NullString
	require.NoError(t, ts.DB.QueryRow(`SELECT backup_email FROM users WHERE id = $1`, userID).Scan(&backupEmail))
	if backupEmail.Valid {
		return backupEmail.String
	}
	return ""
}

// assertRefusedByHandler fails unless the handler itself refused the request:
// the code must be outside 2xx (NotEqual(200) alone would wave a 204 through),
// and must not be 404 — a renamed or removed route leaves state unchanged too,
// and would otherwise pass this test while the endpoint it guards is gone.
func assertRefusedByHandler(t *testing.T, code int, msg string) {
	t.Helper()
	assert.Falsef(t, code >= 200 && code < 300, "%s (got HTTP %d)", msg, code)
	assert.NotEqualf(t, 404, code, "%s: request never reached the handler (got HTTP 404)", msg)
}

// TestEmailSmsDisable_WithoutStepUp_LeavesStateUnchanged pins the property
// that a request carrying only a bearer token, with no re-authentication,
// must not be able to delete email/SMS MFA state.
//
// regression: EmailSmsDisable destroys MFA state with only a bearer token (Semgrep AI finding, router:1962)
func TestEmailSmsDisable_WithoutStepUp_LeavesStateUnchanged(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepupemaildis")
	ctx := context.Background()

	enabledKey := fmt.Sprintf(redisEmailSMSEnabled, user.ID)
	require.NoError(t, ts.Redis.Set(ctx, enabledKey, "1", 0).Err())

	// Fixture guard: the key must exist before the token-only request.
	existsBefore := ts.Redis.Exists(ctx, enabledKey).Val()
	require.Equal(t, int64(1), existsBefore, "fixture guard: email MFA must be enabled before the request")

	w := ts.DoRequest("POST", urlEmailSmsDisable, nil, testhelpers.AuthHeaders(user.AccessToken))

	existsAfter := ts.Redis.Exists(ctx, enabledKey).Val()
	assert.Equal(t, int64(1), existsAfter,
		"a token-only request must not change email MFA state")
	assertRefusedByHandler(t, w.Code,
		"a token-only disable request must not succeed without step-up")
}

// TestStoreRecoveryKey_OverwriteWithoutStepUp_DoesNotOverwrite pins the
// property that a bearer-token-only request cannot overwrite an EXISTING
// recovery key. First-time creation is exempt (the TOTP enrollment flow
// auto-uploads one); this targets overwrite of an already-stored key.
//
// regression: StoreRecoveryKey upserts over an existing key with only a bearer token (Semgrep AI finding, router:2138)
func TestStoreRecoveryKey_OverwriteWithoutStepUp_DoesNotOverwrite(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepupreckey")

	// Seed K1 directly rather than through the route under test: whether
	// first-time creation needs step-up is the fix design's call, and this
	// fixture must not depend on it.
	_, err := ts.DB.Exec(`INSERT INTO user_recovery_keys (user_id, recovery_wrapped_private_key, recovery_key_salt) VALUES ($1, $2, $3)`,
		user.ID, []byte("initial-recovery-key-32-bytes!!"), []byte("initial-salt-16b"))
	require.NoError(t, err, "fixture setup: seed the existing recovery key K1")

	// Fixture guard: the stored value is K1.
	var stored []byte
	err = ts.DB.QueryRow(`SELECT recovery_wrapped_private_key FROM user_recovery_keys WHERE user_id = $1`, user.ID).Scan(&stored)
	require.NoError(t, err, "fixture guard: recovery key row must exist")
	require.Equal(t, []byte("initial-recovery-key-32-bytes!!"), stored, "fixture guard: stored key must be K1")

	// Token-only overwrite attempt with a different key, no password/mfa fields.
	k2 := base64.StdEncoding.EncodeToString([]byte("attacker-recovery-key-32-bytes!"))
	salt2 := base64.StdEncoding.EncodeToString([]byte("attacker-salt-16"))
	w := ts.DoRequest("PUT", urlRecoveryKey, map[string]interface{}{
		"recovery_wrapped_private_key": k2,
		"recovery_key_salt":            salt2,
	}, testhelpers.AuthHeaders(user.AccessToken))

	var storedAfter []byte
	err = ts.DB.QueryRow(`SELECT recovery_wrapped_private_key FROM user_recovery_keys WHERE user_id = $1`, user.ID).Scan(&storedAfter)
	require.NoError(t, err, "recovery key row must still exist")
	assert.Equal(t, []byte("initial-recovery-key-32-bytes!!"), storedAfter,
		"a token-only request must not overwrite an existing recovery key")
	assertRefusedByHandler(t, w.Code,
		"a token-only overwrite request must not succeed without step-up")
}

// TestSetBackupEmail_WithoutStepUp_LeavesEmailUnchanged pins the property
// that a bearer-token-only request must not change users.backup_email,
// whether setting a new value or clearing it.
//
// regression: SetBackupEmail ignores the caller-supplied password field (Semgrep AI finding, router:2041)
func TestSetBackupEmail_WithoutStepUp_LeavesEmailUnchanged(t *testing.T) {
	const originalBackupEmail = "old-backup@example.com"

	t.Run("overwrite", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "stepupbackupow")

		_, err := ts.DB.Exec(`UPDATE users SET backup_email = $1 WHERE id = $2`, originalBackupEmail, user.ID)
		require.NoError(t, err)
		require.Equal(t, originalBackupEmail, readBackupEmail(t, ts, user.ID), "fixture guard: backup email must be set before the request")

		w := ts.DoRequest("PUT", urlBackupEmail, map[string]interface{}{
			"email": "attacker@example.com",
		}, testhelpers.AuthHeaders(user.AccessToken))

		assert.Equal(t, originalBackupEmail, readBackupEmail(t, ts, user.ID),
			"a token-only request must not change the backup email")
		assertRefusedByHandler(t, w.Code,
			"a token-only backup-email change must not succeed without step-up")
	})

	t.Run("clear", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "stepupbackupclr")

		_, err := ts.DB.Exec(`UPDATE users SET backup_email = $1 WHERE id = $2`, originalBackupEmail, user.ID)
		require.NoError(t, err)
		require.Equal(t, originalBackupEmail, readBackupEmail(t, ts, user.ID), "fixture guard: backup email must be set before the request")

		w := ts.DoRequest("PUT", urlBackupEmail, map[string]interface{}{
			"email": "",
		}, testhelpers.AuthHeaders(user.AccessToken))

		assert.Equal(t, originalBackupEmail, readBackupEmail(t, ts, user.ID),
			"a token-only request must not clear the backup email")
		assertRefusedByHandler(t, w.Code,
			"a token-only backup-email clear must not succeed without step-up")
	})
}
