package activepresence

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// One user may owe a Custom Status settings operation and an active-category
// plan at the same time. This is the acceptance criterion that decided the
// SHAPE of migration 000111: presence_settings_pending_operations is
// PRIMARY KEY (user_id), so it can hold exactly one outstanding operation per
// user, and widening it to (user_id, kind) would have put an active-category
// row in the table that readCustomTextSnapshotCandidate consults with
// NOT EXISTS (... pending ...) -- suppressing that sender's entire Custom
// Status reconnect snapshot for the row's lifetime.
//
// A sibling table is what makes coexistence possible, so the coexistence is
// worth asserting rather than assuming. If a later change tries to merge the
// two rails back into one table, this test is what fails.
func TestCustomStatusPendingOperationCoexistsWithAnActivePlan(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	ctx := context.Background()

	// The Custom Status rail's own outstanding operation, on 000087's table.
	_, err := db.ExecContext(ctx, `
		INSERT INTO presence_settings_pending_operations (user_id, operation_id, prior_settings_version)
		VALUES ($1, $2, 0)`, subject, uuid.New())
	require.NoError(t, err, "the Custom Status rail must accept its own pending row")

	// The active-category rail's obligation for the SAME user.
	insert(t, db, conservativePlan(subject, presence.CategoryPrivateCall))

	var settingsRows, planRows int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM presence_settings_pending_operations WHERE user_id = $1`,
		subject).Scan(&settingsRows))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`,
		subject).Scan(&planRows))

	require.Equal(t, 1, settingsRows, "the Custom Status operation must survive")
	require.Equal(t, 1, planRows, "the active-category plan must survive alongside it")

	// Draining one rail must not disturb the other. DrainSubjectTx is the
	// erasure path's entry point, and an erasure that silently discharged a
	// Custom Status obligation would lose a retraction nobody is tracking.
	tx := beginTx(t, db)
	drained, err := DrainSubjectTx(ctx, tx, subject)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, []presence.Category{presence.CategoryPrivateCall}, drained)

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM presence_settings_pending_operations WHERE user_id = $1`,
		subject).Scan(&settingsRows))
	require.Equal(t, 1, settingsRows,
		"draining the active-category rail must leave the Custom Status operation untouched")
}

// The two categories are independent obligations for one subject, which is what
// PRIMARY KEY (user_id, category) buys over PRIMARY KEY (user_id). A user may be
// in a Server Voice channel and a Private Call at once.
func TestBothActiveCategoriesCoexistForOneSubject(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	ctx := context.Background()

	insert(t, db, conservativePlan(subject, presence.CategoryServerVoice))
	insert(t, db, conservativePlan(subject, presence.CategoryPrivateCall))

	var rows int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`,
		subject).Scan(&rows))
	require.Equal(t, 2, rows, "the composite key must admit one plan per category")
}
