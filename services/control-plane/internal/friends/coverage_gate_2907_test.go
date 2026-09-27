package friends_test

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// Blocking must leave durable reconciliation evidence even when no DM exists;
// the marker is the safety boundary for a later conversation creation.
func TestCoverageGate2907_BlockWithoutDMLeavesReconciliationMarker(t *testing.T) {
	ts := setupTS(t)
	blocker := ts.CreateTestUser(t, "coverage-blocker")
	blocked := ts.CreateTestUser(t, "coverage-blocked")

	w := ts.DoRequest("POST", "/api/v1/friends/"+blocked.ID+"/block", nil,
		testhelpers.AuthHeaders(blocker.AccessToken))
	require.Equal(t, 200, w.Code)

	var count int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM dm_block_reconciliations
		WHERE user_a_id = LEAST($1::uuid, $2::uuid)
		  AND user_b_id = GREATEST($1::uuid, $2::uuid)`, blocker.ID, blocked.ID).Scan(&count))
	require.Equal(t, 1, count)
}
