package presence_test

import (
	"context"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/presence"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestComputeCustomTextAudience(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	t.Run("no settings row returns empty (treated as Off)", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		friend := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, friend)
		aud, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.Empty(t, aud, "no row must be Off => empty audience")
	})

	t.Run("tier 0 (Off) returns empty even with friends", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		friend := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, friend)
		testhelpers.SetCustomTextTier(t, db, sender, 0)
		aud, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.Empty(t, aud)
	})

	t.Run("tier 1 (Friends) includes friend, excludes server-only peer", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		friend := testhelpers.CreateUser(t, db)
		peer := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, friend)
		srv := testhelpers.CreateServer(t, db, sender)
		testhelpers.AddServerMember(t, db, srv, sender)
		testhelpers.AddServerMember(t, db, srv, peer)
		testhelpers.SetCustomTextTier(t, db, sender, 1)
		aud, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.True(t, aud[friend], "friend must see Friends-tier custom text")
		require.False(t, aud[peer], "server-only peer must NOT see Friends-tier custom text")
	})

	t.Run("tier 1 (Friends) includes FoF only when dm_friends_of_friends is on", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		friend := testhelpers.CreateUser(t, db)
		fof := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, friend)
		testhelpers.AddFriendship(t, db, friend, fof)
		testhelpers.SetCustomTextTier(t, db, sender, 1)

		// FoF off (default): fof not included.
		aud, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.False(t, aud[fof], "FoF excluded when dm_friends_of_friends is off")

		// FoF on: fof included.
		testhelpers.SetFriendsOfFriends(t, db, sender, true)
		aud, err = presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.True(t, aud[fof], "FoF included when dm_friends_of_friends is on")
	})

	t.Run("tier 2 (Servers) includes shared-server peer", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		friend := testhelpers.CreateUser(t, db)
		peer := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, friend)
		srv := testhelpers.CreateServer(t, db, sender)
		testhelpers.AddServerMember(t, db, srv, sender)
		testhelpers.AddServerMember(t, db, srv, peer)
		testhelpers.SetCustomTextTier(t, db, sender, 2)
		aud, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.True(t, aud[friend], "friend included at Servers tier")
		require.True(t, aud[peer], "shared-server peer included at Servers tier")
	})

	t.Run("sender is never in own audience", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		other := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, other)
		srv := testhelpers.CreateServer(t, db, sender)
		testhelpers.AddServerMember(t, db, srv, sender)
		testhelpers.SetCustomTextTier(t, db, sender, 2)
		aud, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.False(t, aud[sender], "sender must never be in their own audience")
	})
}
