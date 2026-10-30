// services/control-plane/internal/privacy/handler_integration_test.go

package privacy_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
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

func TestEraseAccount_Integration_BlacklistsCurrentAccessToken(t *testing.T) {
	// Regression for #718: erasure must revoke the caller's still-live access token.
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "eraseblacklist")
	headers := testhelpers.AuthHeaders(user.AccessToken)

	w := ts.DoRequest(
		http.MethodPost,
		"/api/v1/privacy/erase-account",
		map[string]interface{}{},
		headers,
	)
	require.Equal(t, http.StatusNoContent, w.Code)

	w = ts.DoRequest(
		http.MethodPost,
		"/api/v1/auth/ws-ticket",
		nil,
		headers,
	)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestEraseAccount_Integration_DeniesOtherLiveAccessTokens(t *testing.T) {
	// Regression for Gitar review on #718: erasure must reject every already-issued
	// access token for the erased user, not only the token that called erasure.
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "eraseallsessions")
	otherAccessToken, err := auth.GenerateAccessToken(user.ID, testhelpers.TestJWTSecret, true, "", "")
	require.NoError(t, err)

	w := ts.DoRequest(
		http.MethodPost,
		"/api/v1/privacy/erase-account",
		map[string]interface{}{},
		testhelpers.AuthHeaders(user.AccessToken),
	)
	require.Equal(t, http.StatusNoContent, w.Code)

	w = ts.DoRequest(
		http.MethodPost,
		"/api/v1/auth/ws-ticket",
		nil,
		testhelpers.AuthHeaders(otherAccessToken),
	)
	assert.Equal(t, http.StatusForbidden, w.Code)
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

// A correctly signed token loses principal authority when the user disappears
// before real AuthRequired admission; this case covers the generic 401.
func TestEraseAccount_Integration_401OnMissingPrincipalBeforeAdmission(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "ghostuser")
	foreign := ts.CreateTestUser(t, "eraseforeign")
	requireLiveErasureFamily(t, ts, user.AccessToken)
	_, err := ts.DB.ExecContext(t.Context(), `DELETE FROM users WHERE id = $1`, user.ID)
	require.NoError(t, err)
	before := erasureFailureState(t, ts, foreign.ID)
	requireErasureTokensUnblacklisted(t, ts, user.AccessToken, foreign.AccessToken)

	w := ts.DoRequest(http.MethodPost, "/api/v1/privacy/erase-account", map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.JSONEq(t, `{"error":"Authentication required"}`, w.Body.String())
	assert.Empty(t, w.Result().Cookies(), "failed admission must not emit success cookies")
	assert.True(t, reflect.DeepEqual(before, erasureFailureState(t, ts, foreign.ID)), "failed erasure changed durable family/audit/foreign state")
	requireErasureTokensUnblacklisted(t, ts, user.AccessToken, foreign.AccessToken)
	var exists bool
	require.NoError(t, ts.DB.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, user.ID).Scan(&exists))
	assert.False(t, exists)
}

// The handler's separate 404 is reachable after actual successful family
// admission when the principal disappears before the first erasure candidate
// read. Only the router's pool is hooked; the deletion uses the ordinary pool.
func TestEraseAccount_Integration_404OnMissingUserAfterAdmission(t *testing.T) {
	hook, routerDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, routerDB)
	user := ts.CreateTestUser(t, "erasedafterauth")
	foreign := ts.CreateTestUser(t, "eraseforeignrace")
	claims := requireLiveErasureFamily(t, ts, user.AccessToken)
	before := erasureFailureState(t, ts, foreign.ID)
	requireErasureTokensUnblacklisted(t, ts, user.AccessToken, foreign.AccessToken)
	admitted := false
	require.NoError(t, hook.ArmAdmission(user.ID, claims.SessionID, func() error { admitted = true; return nil }))
	const candidateQuery = `SELECT DISTINCT dvp.user_id
		FROM dm_conversations c
		JOIN dm_voice_participants dvp ON dvp.conversation_id = c.id
		WHERE c.created_by = $1 AND dvp.user_id <> $1
		ORDER BY dvp.user_id
		LIMIT $2`
	hook.ArmArg([]string{candidateQuery}, user.ID, func() error {
		// This exact successful auth transaction must precede the candidate query.
		if !admitted {
			return fmt.Errorf("candidate read preceded successful authentication")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		result, err := ts.DB.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, user.ID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("candidate hook deleted %d principals, want one", rows)
		}
		return nil
	}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/privacy/erase-account", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header = testhelpers.AuthHeaders(user.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	seen, betweenErr := hook.Report()
	require.Equal(t, 1, seen, "this request must reach its first erasure candidate query")
	require.NoError(t, betweenErr)
	admission := hook.AdmissionReport()
	require.Equal(t, 1, admission.Fired)
	require.True(t, admission.Completed)
	require.False(t, admission.Pending || admission.Running)
	require.NoError(t, admission.CallbackErr)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.JSONEq(t, `{"error":"account not found"}`, w.Body.String())
	assert.Empty(t, w.Result().Cookies(), "failed erasure must not emit success cookies")
	assert.True(t, reflect.DeepEqual(before, erasureFailureState(t, ts, foreign.ID)), "failed erasure changed durable family/audit/foreign state")
	requireErasureTokensUnblacklisted(t, ts, user.AccessToken, foreign.AccessToken)
	var exists bool
	require.NoError(t, ts.DB.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, user.ID).Scan(&exists))
	assert.False(t, exists, "the ordinary-pool race deletion must be durable")
}

// A login revoked after its request passed admission must not erase the
// account: session revocation rotates no epoch, so erasure revalidates the
// family under the users lock and answers 401.
func TestEraseAccount_Integration_401OnFamilyRevokedAfterAdmission(t *testing.T) {
	hook, routerDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, routerDB)
	user := ts.CreateTestUser(t, "erasedrevokedauth")
	claims := requireLiveErasureFamily(t, ts, user.AccessToken)
	admitted := false
	require.NoError(t, hook.ArmAdmission(user.ID, claims.SessionID, func() error { admitted = true; return nil }))
	const candidateQuery = `SELECT DISTINCT dvp.user_id
		FROM dm_conversations c
		JOIN dm_voice_participants dvp ON dvp.conversation_id = c.id
		WHERE c.created_by = $1 AND dvp.user_id <> $1
		ORDER BY dvp.user_id
		LIMIT $2`
	hook.ArmArg([]string{candidateQuery}, user.ID, func() error {
		if !admitted {
			return fmt.Errorf("candidate read preceded successful authentication")
		}
		return revokeErasureSession(t.Context(), ts, user.AccessToken)
	}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/privacy/erase-account", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header = testhelpers.AuthHeaders(user.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	seen, betweenErr := hook.Report()
	require.Equal(t, 1, seen, "this request must reach its first erasure candidate query")
	require.NoError(t, betweenErr)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.JSONEq(t, `{"error":"authentication required"}`, w.Body.String())
	var exists bool
	require.NoError(t, ts.DB.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, user.ID).Scan(&exists))
	assert.True(t, exists, "a revoked requester must not erase the account")
}

// revokeErasureSession logs out the token's session family the way the
// session-revocation writers do.
func revokeErasureSession(ctx context.Context, ts *testhelpers.TestServer, accessToken string) (err error) {
	claims, err := auth.ValidateAccessToken(accessToken, ts.Config.JWTSecret)
	if err != nil {
		return err
	}
	userID, err := uuid.Parse(claims.UserID)
	if err != nil {
		return err
	}
	sid, err := uuid.Parse(claims.SessionID)
	if err != nil {
		return err
	}
	tx, err := ts.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	if err != nil {
		return err
	}
	locked, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{userID}, loginfamily.UserNoKeyUpdate)
	if err != nil {
		return err
	}
	authority, err := locked.ForUser(userID)
	if err != nil {
		return err
	}
	targets, err := loginfamily.ResolveTargetSIDTx(ctx, tx, authority, sid)
	if err != nil {
		return err
	}
	if _, err = loginfamily.RevokeFamiliesTx(ctx, tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeSession, Reason: loginfamily.ReasonLogout}); err != nil {
		return err
	}
	return tx.Commit()
}

func requireLiveErasureFamily(t *testing.T, ts *testhelpers.TestServer, accessToken string) *auth.Claims {
	t.Helper()
	claims, err := auth.ValidateAccessToken(accessToken, ts.Config.JWTSecret)
	require.NoError(t, err)
	userID, err := uuid.Parse(claims.UserID)
	require.NoError(t, err)
	sid, err := uuid.Parse(claims.SessionID)
	require.NoError(t, err)
	require.NotNil(t, claims.ExpiresAt)
	_, err = loginfamily.ResolveVerifiedAccess(t.Context(), ts.DB, loginfamily.AccessIdentity{
		UserID: userID, SID: sid, CredentialEpoch: claims.CredentialEpoch, AccessExpiresAt: claims.ExpiresAt.Time,
	})
	require.NoError(t, err, "real family authority must be live before dispatch")
	return claims
}

// Snapshot complete durable family records, the audit count and the unrelated
// principal/refresh records: a uniform refusal alone cannot prove no mutation.
func erasureFailureState(t *testing.T, ts *testhelpers.TestServer, foreignID string) map[string]string {
	t.Helper()
	state := make(map[string]string)
	for _, table := range []string{"login_family_rollout", "login_families", "login_family_sids", "login_family_revocations", "login_family_revocation_targets"} {
		var snapshot string
		require.NoError(t, ts.DB.QueryRowContext(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(s) ORDER BY to_jsonb(s)::text)::text, '[]') FROM `+table+` s`).Scan(&snapshot))
		state[table] = snapshot
	}
	var audit string
	require.NoError(t, ts.DB.QueryRowContext(t.Context(), `SELECT COUNT(*)::text FROM account_deletions`).Scan(&audit))
	state["audit"] = audit
	var foreignUser, foreignRefresh string
	require.NoError(t, ts.DB.QueryRowContext(t.Context(), `SELECT to_jsonb(u)::text FROM users u WHERE id=$1`, foreignID).Scan(&foreignUser))
	require.NoError(t, ts.DB.QueryRowContext(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.id)::text, '[]') FROM refresh_tokens r WHERE user_id=$1`, foreignID).Scan(&foreignRefresh))
	state["foreign_user"], state["foreign_refresh"] = foreignUser, foreignRefresh
	return state
}

func requireErasureTokensUnblacklisted(t *testing.T, ts *testhelpers.TestServer, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		claims, err := auth.ValidateAccessToken(token, ts.Config.JWTSecret)
		require.NoError(t, err)
		require.NotEmpty(t, claims.ID)
		count, err := ts.Redis.Exists(t.Context(), "blacklist:"+claims.ID).Result()
		require.NoError(t, err)
		assert.Zero(t, count, "failed erasure must not blacklist an access token")
	}
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
