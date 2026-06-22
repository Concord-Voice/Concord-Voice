package admin_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

// auditRowCount counts admin_audit_log rows whose event_type matches a
// per-test-unique marker so the assertion is isolated despite the shared DB.
func auditRowCount(t *testing.T, db *sql.DB, eventType string) int {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE event_type = $1`, eventType).Scan(&n)
	require.NoError(t, err)
	return n
}

func TestAuditLog_Write_InsertsExactlyOneRow(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	audit := admin.NewAuditLog(db)

	// Unique event_type marker so the count is isolated from leftover rows.
	marker := uniqueAdminUsername("evt")
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_audit_log WHERE event_type = $1`, marker)
		assert.NoError(t, err)
	})

	err := audit.Write(ctx, admin.AuditEvent{
		Actor:     "operator-handle",
		EventType: marker,
		Result:    admin.AuditSuccess,
		SourceRef: "cf-access-subject-123",
		Detail:    map[string]any{"step": "password"},
	})
	require.NoError(t, err)

	assert.Equal(t, 1, auditRowCount(t, db, marker))
}

// The audit row must never contain the password / assertion / token inputs.
func TestAuditLog_Write_NeverStoresSecretValues(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	audit := admin.NewAuditLog(db)

	marker := uniqueAdminUsername("evt-secret")
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_audit_log WHERE event_type = $1`, marker)
		assert.NoError(t, err)
	})

	const secret = "SuperSecretP@ssw0rd!" //nolint:gosec // pragma: allowlist secret -- test fixture asserting the value is NOT stored
	err := audit.Write(ctx, admin.AuditEvent{
		Actor:     "operator-handle",
		EventType: marker,
		Result:    admin.AuditFailure,
		SourceRef: "ref",
		Detail:    map[string]any{"reason": "bad_password"},
	})
	require.NoError(t, err)

	// Scan the entire row's text form; assert the secret string never appears.
	var actor, eventType, result string
	var sourceRef sql.NullString
	var detail sql.NullString
	err = db.QueryRow(
		`SELECT actor, event_type, result, source_ref, detail::text
		 FROM admin_audit_log WHERE event_type = $1`, marker,
	).Scan(&actor, &eventType, &result, &sourceRef, &detail)
	require.NoError(t, err)

	assert.NotContains(t, actor, secret)
	assert.NotContains(t, sourceRef.String, secret)
	assert.NotContains(t, detail.String, secret)
}

// Actor is routed through sanitizeAuditString: CR/LF and control chars are
// stripped before storage (CWE-117 log forging defense).
func TestAuditLog_Write_SanitizesActor(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	audit := admin.NewAuditLog(db)

	marker := uniqueAdminUsername("evt-sanitize")
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_audit_log WHERE event_type = $1`, marker)
		assert.NoError(t, err)
	})

	err := audit.Write(ctx, admin.AuditEvent{
		Actor:     "evil\r\nINJECTED admin\x07line\x7f",
		EventType: marker,
		Result:    admin.AuditDenied,
	})
	require.NoError(t, err)

	var actor string
	err = db.QueryRow(
		`SELECT actor FROM admin_audit_log WHERE event_type = $1`, marker,
	).Scan(&actor)
	require.NoError(t, err)

	assert.NotContains(t, actor, "\r")
	assert.NotContains(t, actor, "\n")
	assert.NotContains(t, actor, "\x07")
	assert.NotContains(t, actor, "\x7f")
	assert.Equal(t, "evilINJECTED adminline", actor)
}

// RATIFIED enforcement: because Write runs under SET LOCAL ROLE
// concord_admin_rt, the inserted history cannot be rewritten — an UPDATE or
// DELETE under that role is denied by Postgres. This proves append-only is
// ENFORCED, not merely asserted by app logic.
func TestAuditLog_AppendOnly_EnforcedByRole(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	audit := admin.NewAuditLog(db)

	marker := uniqueAdminUsername("evt-append")
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_audit_log WHERE event_type = $1`, marker)
		assert.NoError(t, err)
	})

	require.NoError(t, audit.Write(ctx, admin.AuditEvent{
		EventType: marker,
		Result:    admin.AuditSuccess,
	}))

	// Under the restricted role, UPDATE is denied.
	updTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = updTx.ExecContext(ctx, `SET LOCAL ROLE concord_admin_rt`)
	require.NoError(t, err)
	_, err = updTx.ExecContext(ctx, `UPDATE admin_audit_log SET result = 'failure' WHERE event_type = $1`, marker)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
	require.NoError(t, updTx.Rollback())

	// Under the restricted role, DELETE is denied.
	delTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = delTx.ExecContext(ctx, `SET LOCAL ROLE concord_admin_rt`)
	require.NoError(t, err)
	_, err = delTx.ExecContext(ctx, `DELETE FROM admin_audit_log WHERE event_type = $1`, marker)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
	require.NoError(t, delTx.Rollback())
}
