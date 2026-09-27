package users

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/require"
)

func TestCoverageGate2907_DeleteAccountFailsClosedWhenBlockMarkerDrainFails(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	peerID := testdb.CreateUser(t, db)
	insertErasureBlockMarker(t, db, userID, peerID)
	var err error
	_, err = db.Exec(`
		CREATE FUNCTION coverage_gate_2907_marker_failure() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'coverage gate marker drain failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER coverage_gate_2907_marker_failure
		BEFORE DELETE ON dm_block_reconciliations
		FOR EACH ROW EXECUTE FUNCTION coverage_gate_2907_marker_failure()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := db.Exec(`DROP TRIGGER IF EXISTS coverage_gate_2907_marker_failure ON dm_block_reconciliations`); err != nil {
			t.Errorf("drop marker-failure test trigger: %v", err)
		}
		if _, err := db.Exec(`DROP FUNCTION IF EXISTS coverage_gate_2907_marker_failure()`); err != nil {
			t.Errorf("drop marker-failure test function: %v", err)
		}
	})

	err = NewAccountService(db, logger.New("test")).DeleteAccount(context.Background(), userID.String())
	require.ErrorContains(t, err, "drain DM block reconciliation")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&count))
	require.Equal(t, 1, count)
}
