package messages_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// These requests rotate credentials after real HTTP admission and must reach
// the transaction-local guard before being refused.
func staleMessageFixture(t *testing.T) (*testhelpers.TestServer, *testhelpers.StaleBoundary, testhelpers.TestUser, string) {
	t.Helper()
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	user := ts.CreateTestUser(t, "stale-guard")
	serverID := ts.CreateTestServer(t, user.ID, "stale guard server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	messageID := ts.CreateTestMessage(t, channelID, user, "guarded message")
	return ts, boundary, user, messageID
}

func TestChannelMessageMutationsRejectStaleCredentialEpoch(t *testing.T) {
	tests := []struct {
		name   string
		method string
		suffix string
		body   map[string]interface{}
	}{
		{name: "update", method: http.MethodPatch, body: map[string]interface{}{"content": testhelpers.ValidCiphertext(), "key_version": 1}},
		{name: "delete", method: http.MethodDelete},
		{name: "suppress", method: http.MethodPost, suffix: "/suppress-embeds"},
		{name: "pin", method: http.MethodPost, suffix: "/pin"},
		{name: "unpin", method: http.MethodDelete, suffix: "/pin"},
		{name: "reaction", method: http.MethodPut, suffix: "/reactions", body: map[string]interface{}{"emoji": "👍"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, boundary, user, messageID := staleMessageFixture(t)
			if tt.name == "unpin" {
				_, err := ts.DB.Exec(`UPDATE messages SET pinned_at = NOW(), pinned_by = $2 WHERE id = $1`, messageID, user.ID)
				require.NoError(t, err)
			}
			if tt.name == "suppress" {
				_, err := ts.DB.Exec(`UPDATE messages SET embeds_suppressed = FALSE WHERE id = $1`, messageID)
				require.NoError(t, err)
			}
			var originalContent string
			var originalVersion int
			var originalPinned, originalSuppressed bool
			require.NoError(t, ts.DB.QueryRow(`SELECT content, key_version, pinned_at IS NOT NULL, embeds_suppressed FROM messages WHERE id = $1`, messageID).Scan(&originalContent, &originalVersion, &originalPinned, &originalSuppressed))
			staleToken := user.AccessToken
			require.NoError(t, boundary.Arm(staleToken))
			boundary.Hook.ArmArg([]string{messageEpochGuardSQL}, user.ID, nil, nil)
			// A nil map must go out as NO body: DoRequest marshals a typed nil
			// to `null`, which the delete route's optional step-up body refuses
			// with 400 before any fence runs (#3455).
			var body interface{}
			if tt.body != nil {
				body = tt.body
			}
			w := ts.DoRequest(tt.method, "/api/v1/messages/"+messageID+tt.suffix, body,
				testhelpers.AuthHeaders(staleToken))
			require.Equal(t, http.StatusUnauthorized, w.Code)
			requireMessageStaleBoundary(t, boundary)
			var content string
			var version, reactions int
			var pinned, suppressed bool
			require.NoError(t, ts.DB.QueryRow(`SELECT content, key_version, pinned_at IS NOT NULL, embeds_suppressed FROM messages WHERE id = $1`, messageID).Scan(&content, &version, &pinned, &suppressed))
			require.Equal(t, originalContent, content, "refused mutation must preserve ciphertext")
			require.Equal(t, originalVersion, version)
			require.Equal(t, originalPinned, pinned)
			require.Equal(t, originalSuppressed, suppressed)
			require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM message_reactions WHERE message_id = $1`, messageID).Scan(&reactions))
			require.Zero(t, reactions)
		})
	}
}

func TestChannelMessageMutationRejectsRemovedMember(t *testing.T) {
	ts, user, serverID, channelID, messageID := setupWithMessage(t)
	_, err := ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, user.ID)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodPost, "/api/v1/messages/"+messageID+"/pin", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusForbidden, w.Code)

	// Keep the channel referenced so the fixture exercises the channel-scoped
	// membership path rather than a missing-resource response.
	var exists bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM channels WHERE id = $1)`, channelID).Scan(&exists))
	require.True(t, exists)
}

func TestPurgeRejectsStaleCredentialEpoch(t *testing.T) {
	for _, scope := range []string{"channel", "server"} {
		t.Run(scope, func(t *testing.T) {
			ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
			user := ts.CreateTestUser(t, "purge-stale-"+scope)
			serverID := ts.CreateTestServer(t, user.ID, "stale purge server")
			channelID := ts.CreateTestChannel(t, serverID, "general")
			messageID := ts.CreateTestMessage(t, channelID, user, "guarded message")
			path := "/api/v1/channels/" + channelID + "/messages"
			if scope == "server" {
				path = "/api/v1/servers/" + serverID + "/messages"
			}
			require.NoError(t, boundary.Arm(user.AccessToken))
			// Purge admission first fences the subject through LockGateTx;
			// its epoch refusal precedes the later per-batch GuardTx.
			boundary.Hook.ArmArg([]string{`SELECT credential_epoch, COALESCE(password_hash, '') FROM users WHERE id = $1 FOR SHARE`}, user.ID, nil, nil)
			w := ts.DoRequest(http.MethodDelete, path, map[string]interface{}{"range": "all"}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			requireMessageStaleBoundary(t, boundary)
			require.Contains(t, w.Body.String(), "Authentication required")
			assertMessageExists(t, ts, messageID)
		})
	}
}

// A credential epoch may change between purge batches. The first irreversible
// batch must still be broadcast and reported as a partial failure; only a
// rejection before every batch is eligible for the retryable 401 response.
func TestPurgeChannel_EpochChangeAfterCommittedBatchRemainsPartialFailure(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "purge_epoch_partial_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "purge epoch partial server")
	channelID := ts.CreateTestChannel(t, serverID, "purge epoch partial channel")
	_, err := ts.DB.Exec(`
		INSERT INTO messages (id, channel_id, user_id, content, key_version, created_at)
		SELECT gen_random_uuid(), $1, $2, 'purge-epoch-' || g, 1, NOW()
		FROM generate_series(1, 5001) g`, channelID, owner.ID)
	require.NoError(t, err)

	// The default 5,000-row stride commits once before this trigger advances the
	// actor's epoch. The second batch must then be rejected by GuardTx.
	_, err = ts.DB.Exec(`
		CREATE FUNCTION test_purge_epoch_after_first_batch() RETURNS trigger AS $$
		BEGIN
			IF OLD.deleted_count = 0 AND NEW.deleted_count > 0 THEN
				UPDATE users SET credential_epoch = repeat('a', 32) WHERE id = NEW.actor_id;
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_purge_epoch_after_first_batch
		AFTER UPDATE OF deleted_count ON message_purges
		FOR EACH ROW EXECUTE FUNCTION test_purge_epoch_after_first_batch()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_purge_epoch_after_first_batch ON message_purges;
			DROP FUNCTION IF EXISTS test_purge_epoch_after_first_batch()`)
		require.NoError(t, cleanupErr)
	})

	conn := dialPurgeObserver(t, ts, owner, serverID, channelID)
	w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID), map[string]interface{}{"range": "all"},
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), "Purge failed")
	require.Equal(t, 1, countChannelMessages(t, ts, channelID), "the rejected second batch must remain")

	channelEvents, serverEvents := collectPurgeEvents(t, conn, time.Second)
	require.Empty(t, serverEvents)
	require.Len(t, channelEvents, 1, "the committed first batch must still invalidate subscribers")
	require.Equal(t, channelID, channelEvents[0]["channel_id"])
	require.Equal(t, owner.ID, channelEvents[0]["purged_by"])
	require.Equal(t, float64(5000), channelEvents[0]["deleted_count"])
}

func assertMessageExists(t *testing.T, ts *testhelpers.TestServer, messageID string) {
	t.Helper()
	var exists bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1)`, messageID).Scan(&exists))
	require.True(t, exists, "a rejected stale-epoch purge must leave the message intact")
}
