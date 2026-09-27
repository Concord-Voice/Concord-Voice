package dm_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedLegacyBlockedPair creates the pre-000130 shape: a blocked friendship
// inserted directly (rather than transitioned), with no durable marker. The
// group remains intentionally inconsistent until an authorized resolver acts.
func seedLegacyBlockedPair(t *testing.T, ts *testhelpers.TestServer, owner, member, blocked testhelpers.TestUser) string {
	t.Helper()
	convID := ts.CreateGroupDMConversation(t, owner.ID, member.ID, blocked.ID)
	_, err := ts.DB.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, owner.ID)
	require.NoError(t, err)
	// This fixture intentionally represents data written before migration 000137.
	// Disable only the reconciliation guard while seeding, then restore it before
	// exercising the resolver.
	tx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`ALTER TABLE friendships DISABLE TRIGGER require_blocked_friendship_reconciliation`)
	require.NoError(t, err)
	_, err = tx.Exec(`
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'blocked')`, member.ID, blocked.ID)
	require.NoError(t, err)
	_, err = tx.Exec(`ALTER TABLE friendships ENABLE TRIGGER require_blocked_friendship_reconciliation`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var markers int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_block_reconciliations WHERE user_a_id = LEAST($1::uuid, $2::uuid)
		 AND user_b_id = GREATEST($1::uuid, $2::uuid)`, member.ID, blocked.ID).Scan(&markers))
	assert.Zero(t, markers, "legacy fixture must have no reconciliation marker")
	return convID
}

func TestLegacyBlockedGroup_OnlyAuthorizedResolutionClearsTopology(t *testing.T) {
	t.Run("self leave", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "legacyselfowner")
		member := ts.CreateTestUser(t, "legacyselfmember")
		blocked := ts.CreateTestUser(t, "legacyselfblocked")
		convID := seedLegacyBlockedPair(t, ts, owner, member, blocked)

		res := ts.DoRequest(http.MethodDelete, pathDMConversationsPrefix+convID+pathMembersSlash+member.ID, nil,
			testhelpers.AuthHeaders(member.AccessToken))
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		assertGroupLacksMember(t, ts, convID, member.ID)
		assertNoBlockedCoMembership(t, ts, convID, member.ID, blocked.ID)
	})

	t.Run("fresh admin removal", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "legacyadminowner")
		admin := ts.CreateTestUser(t, "legacyadminadmin")
		blocked := ts.CreateTestUser(t, "legacyadminblocked")
		convID := seedLegacyBlockedPair(t, ts, owner, admin, blocked)
		_, err := ts.DB.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, admin.ID)
		require.NoError(t, err)

		res := ts.DoRequest(http.MethodDelete, pathDMConversationsPrefix+convID+pathMembersSlash+blocked.ID, nil,
			testhelpers.AuthHeaders(admin.AccessToken))
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		assertGroupLacksMember(t, ts, convID, blocked.ID)
		assertNoBlockedCoMembership(t, ts, convID, admin.ID, blocked.ID)
	})

	t.Run("authorized group delete", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "legacydeleteowner")
		member := ts.CreateTestUser(t, "legacydeletemember")
		blocked := ts.CreateTestUser(t, "legacydeleteblocked")
		convID := seedLegacyBlockedPair(t, ts, owner, member, blocked)

		res := ts.DoRequest(http.MethodDelete, pathDMConversationsPrefix+convID, nil,
			testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		var exists bool
		require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_conversations WHERE id = $1)`, convID).Scan(&exists))
		assert.False(t, exists)
	})
}

func TestLegacyBlockedGroup_UnauthorizedAndNonResolutionWritesRemainDenied(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "legacydenyowner")
	member := ts.CreateTestUser(t, "legacydenymember")
	blocked := ts.CreateTestUser(t, "legacydenyblocked")
	outsider := ts.CreateTestUser(t, "legacydenyoutsider")
	convID := seedLegacyBlockedPair(t, ts, owner, member, blocked)

	tests := []struct {
		name   string
		method string
		path   string
		body   interface{}
		auth   string
	}{
		{"member cannot remove", http.MethodDelete, pathDMConversationsPrefix + convID + pathMembersSlash + blocked.ID, nil, member.AccessToken},
		{"member cannot delete", http.MethodDelete, pathDMConversationsPrefix + convID, nil, member.AccessToken},
		{"admin cannot rename", http.MethodPatch, pathDMConversationsPrefix + convID, map[string]interface{}{"name": "new"}, owner.AccessToken},
		{"admin cannot change role", http.MethodPatch, pathDMConversationsPrefix + convID + pathMembersSlash + blocked.ID, map[string]interface{}{"role": "admin"}, owner.AccessToken},
		{"outsider cannot delete", http.MethodDelete, pathDMConversationsPrefix + convID, nil, outsider.AccessToken},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := ts.DoRequest(tc.method, tc.path, tc.body, testhelpers.AuthHeaders(tc.auth))
			assert.Equal(t, http.StatusForbidden, res.Code, res.Body.String())
		})
	}
	assertGroupHasMember(t, ts, convID, blocked.ID)
}

func assertGroupLacksMember(t *testing.T, ts *testhelpers.TestServer, convID, userID string) {
	t.Helper()
	var exists bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`, convID, userID).Scan(&exists))
	assert.False(t, exists)
}

func assertGroupHasMember(t *testing.T, ts *testhelpers.TestServer, convID, userID string) {
	t.Helper()
	var exists bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`, convID, userID).Scan(&exists))
	assert.True(t, exists)
}

func assertNoBlockedCoMembership(t *testing.T, ts *testhelpers.TestServer, convID, leftID, rightID string) {
	t.Helper()
	var exists bool
	require.NoError(t, ts.DB.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM friendships f
			JOIN dm_participants p1 ON p1.conversation_id = $1 AND p1.user_id = f.requester_id
			JOIN dm_participants p2 ON p2.conversation_id = $1 AND p2.user_id = f.addressee_id
			WHERE f.status = 'blocked'
			  AND ((f.requester_id = $2 AND f.addressee_id = $3)
			    OR (f.requester_id = $3 AND f.addressee_id = $2)))`, convID, leftID, rightID).Scan(&exists))
	assert.False(t, exists, "resolution must leave no blocked co-members")
}
