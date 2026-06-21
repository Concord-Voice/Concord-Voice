package presence_test

import (
	"context"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/presence"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestComputePresenceAudience(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	t.Run("no relations returns empty audience", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		_ = testhelpers.CreateUser(t, db) // an unrelated user, must not appear
		aud, err := presence.ComputePresenceAudience(ctx, db, sender)
		require.NoError(t, err)
		require.Empty(t, aud)
	})

	t.Run("accepted friend is in audience", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		friend := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, friend)
		aud, err := presence.ComputePresenceAudience(ctx, db, sender)
		require.NoError(t, err)
		require.True(t, aud[friend], "accepted friend must be in audience")
	})

	t.Run("pending friendship is excluded", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		other := testhelpers.CreateUser(t, db)
		// Insert a non-accepted friendship directly (helper only inserts accepted).
		_, err := db.Exec(
			`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'pending')`,
			sender, other,
		)
		require.NoError(t, err)
		aud, err := presence.ComputePresenceAudience(ctx, db, sender)
		require.NoError(t, err)
		require.False(t, aud[other], "pending friendship must not grant audience")
	})

	t.Run("shared-server peer is in audience", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		peer := testhelpers.CreateUser(t, db)
		srv := testhelpers.CreateServer(t, db, sender)
		testhelpers.AddServerMember(t, db, srv, sender)
		testhelpers.AddServerMember(t, db, srv, peer)
		aud, err := presence.ComputePresenceAudience(ctx, db, sender)
		require.NoError(t, err)
		require.True(t, aud[peer], "shared-server peer must be in audience")
	})

	t.Run("non-shared-server user is excluded", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		stranger := testhelpers.CreateUser(t, db)
		srvA := testhelpers.CreateServer(t, db, sender)
		srvB := testhelpers.CreateServer(t, db, stranger)
		testhelpers.AddServerMember(t, db, srvA, sender)
		testhelpers.AddServerMember(t, db, srvB, stranger)
		aud, err := presence.ComputePresenceAudience(ctx, db, sender)
		require.NoError(t, err)
		require.False(t, aud[stranger], "user in a different server must be excluded")
	})

	t.Run("friend-of-friend excluded unless dm_friends_of_friends enabled", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		mutual := testhelpers.CreateUser(t, db)
		fof := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, mutual)
		testhelpers.AddFriendship(t, db, mutual, fof)

		aud, err := presence.ComputePresenceAudience(ctx, db, sender)
		require.NoError(t, err)
		require.False(t, aud[fof], "FoF must be excluded when the flag is off")

		testhelpers.SetFriendsOfFriends(t, db, sender, true)
		aud, err = presence.ComputePresenceAudience(ctx, db, sender)
		require.NoError(t, err)
		require.True(t, aud[fof], "FoF must be included when the flag is on")
		require.True(t, aud[mutual], "the direct friend remains in audience")
	})

	t.Run("sender is never in own audience", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		friend := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, friend)
		srv := testhelpers.CreateServer(t, db, sender)
		testhelpers.AddServerMember(t, db, srv, sender)
		aud, err := presence.ComputePresenceAudience(ctx, db, sender)
		require.NoError(t, err)
		require.False(t, aud[sender], "sender must never appear in their own audience")
	})
}
