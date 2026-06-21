// services/control-plane/internal/privacy/handler_integration_test.go

package privacy_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

// TestEraseAccount_Integration_HappyPath performs a full end-to-end erasure:
// creates a test user via the testhelpers, sends POST /api/v1/privacy/erase-account
// through the full router stack, then asserts that the user row is gone and
// a privacy-safe audit row exists in account_deletions with user_id = NULL.
func TestEraseAccount_Integration_HappyPath(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erasehappy")

	w := ts.DoRequest(
		http.MethodPost,
		"/api/v1/privacy/erase-account",
		map[string]interface{}{},
		testhelpers.AuthHeaders(user.AccessToken),
	)
	assert.Equal(t, http.StatusNoContent, w.Code)

	// User row must be deleted.
	var userRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1`, user.ID,
	).Scan(&userRows))
	assert.Equal(t, 0, userRows, "user row must be deleted after successful erasure")

	// Audit row must be inserted with user_id = NULL (FK nulled post-delete).
	var auditRows int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM account_deletions
		WHERE user_id IS NULL AND deleted_at >= NOW() - INTERVAL '1 minute'
	`).Scan(&auditRows))
	assert.GreaterOrEqual(t, auditRows, 1, "audit row must be inserted with NULL user_id")
}

// TestEraseAccount_Integration_IgnoresUnknownClientId is the end-to-end
// regression guard for the #758 transition window. During the rollout, the
// desktop client (#757 not yet shipped) may continue to POST {"clientId":"..."}
// bodies after the server-side handler is Sentry-free. Gin's ShouldBindJSON
// does not reject unknown fields, so the handler must accept the extra field
// and complete the erasure normally.
func TestEraseAccount_Integration_IgnoresUnknownClientId(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "eraseclientid")

	const clientID = "deadbeefcafef00d1122334455667788" //nolint:gosec // pragma: allowlist secret — test sentinel
	w := ts.DoRequest(
		http.MethodPost,
		"/api/v1/privacy/erase-account",
		map[string]interface{}{"clientId": clientID},
		testhelpers.AuthHeaders(user.AccessToken),
	)
	assert.Equal(t, http.StatusNoContent, w.Code,
		"unknown clientId field must be silently ignored — POST returns 204")

	// User must still be deleted even though the body had an extra field.
	var userRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1`, user.ID,
	).Scan(&userRows))
	assert.Equal(t, 0, userRows, "user must be deleted even when body has unknown clientId field")
}

// TestEraseAccount_Integration_404OnMissingUser calls the erasure endpoint with
// a JWT that references a user UUID that does not exist in the database.
// The handler must return 404; no audit row must be written (nothing was erased).
func TestEraseAccount_Integration_404OnMissingUser(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	// Build a JWT for a ghost user that was never inserted into `users`.
	ghostUser := ts.CreateTestUser(t, "ghostuser")
	// Delete the row directly so the JWT is valid but the account is gone.
	_, err := ts.DB.Exec(`DELETE FROM users WHERE id = $1`, ghostUser.ID)
	require.NoError(t, err, "failed to pre-delete ghost user row")

	auditBefore := 0
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM account_deletions`,
	).Scan(&auditBefore))

	w := ts.DoRequest(
		http.MethodPost,
		"/api/v1/privacy/erase-account",
		map[string]interface{}{},
		testhelpers.AuthHeaders(ghostUser.AccessToken),
	)
	assert.Equal(t, http.StatusNotFound, w.Code)

	// No audit row must be written — nothing happened.
	var auditAfter int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM account_deletions`,
	).Scan(&auditAfter))
	assert.Equal(t, auditBefore, auditAfter, "no audit row must be written when user was not found")
}

// TestEraseAccount_Integration_AuditTableHasNoSentryColumn pins the
// post-migration-000060 schema: the sentry_delete_attempted column must not
// exist in account_deletions. If this test fails it means the database is
// running against migrations that pre-date 000060 — the column was dropped as
// part of the #758 Sentry strip.
func TestEraseAccount_Integration_AuditTableHasNoSentryColumn(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	var hasColumn bool
	err := ts.DB.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public'
			  AND table_name   = 'account_deletions'
			  AND column_name  = 'sentry_delete_attempted'
		)
	`).Scan(&hasColumn)
	require.NoError(t, err)
	assert.False(t, hasColumn,
		"sentry_delete_attempted column must be dropped by migration 000060; "+
			"failing here means the DB has not applied migration 000060")
}
