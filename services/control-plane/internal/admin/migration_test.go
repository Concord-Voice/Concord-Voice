package admin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

// TestMigration000077_AdminAuthSchema verifies the three admin-auth tables exist
// after migration 000077 is applied.
func TestMigration000077_AdminAuthSchema(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	for _, table := range []string{"admin_users", "admin_webauthn_credentials", "admin_audit_log"} {
		var exists bool
		err := db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE tablename = $1)`, table).Scan(&exists)
		require.NoError(t, err)
		assert.Truef(t, exists, "table %s should exist after migration 000077", table)
	}
}

// TestMigration000077_AuditLogIsAppendOnly proves the append-only audit guarantee
// is ENFORCED by Postgres via the concord_admin_rt role, not merely documented:
// under SET LOCAL ROLE concord_admin_rt an INSERT succeeds but UPDATE and DELETE
// are denied. This is the load-bearing security property of #1688's audit log.
func TestMigration000077_AuditLogIsAppendOnly(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// INSERT under the restricted role succeeds.
	insTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = insTx.ExecContext(ctx, `SET LOCAL ROLE concord_admin_rt`)
	require.NoError(t, err)
	_, err = insTx.ExecContext(ctx,
		`INSERT INTO admin_audit_log (event_type, result) VALUES ('login_success', 'success')`)
	require.NoError(t, err, "INSERT under concord_admin_rt should succeed")
	require.NoError(t, insTx.Commit())

	// UPDATE under the restricted role is denied by Postgres.
	updTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = updTx.ExecContext(ctx, `SET LOCAL ROLE concord_admin_rt`)
	require.NoError(t, err)
	_, err = updTx.ExecContext(ctx, `UPDATE admin_audit_log SET result = 'failure'`)
	require.Error(t, err, "UPDATE under concord_admin_rt must be denied (append-only)")
	assert.Contains(t, strings.ToLower(err.Error()), "permission denied")
	require.NoError(t, updTx.Rollback())

	// DELETE under the restricted role is denied by Postgres.
	delTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = delTx.ExecContext(ctx, `SET LOCAL ROLE concord_admin_rt`)
	require.NoError(t, err)
	_, err = delTx.ExecContext(ctx, `DELETE FROM admin_audit_log`)
	require.Error(t, err, "DELETE under concord_admin_rt must be denied (append-only)")
	assert.Contains(t, strings.ToLower(err.Error()), "permission denied")
	require.NoError(t, delTx.Rollback())
}
