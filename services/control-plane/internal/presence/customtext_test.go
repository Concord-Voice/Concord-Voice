package presence_test

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
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

		// No privacy row: FoF is enabled by default.
		aud, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.True(t, aud[fof], "FoF included when no privacy row exists")

		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO friendships (requester_id, addressee_id, status)
			VALUES ($1, $2, 'blocked')
		`, sender, fof)
		require.NoError(t, err)
		require.NoError(t, dmblock.RecordBlockTx(ctx, tx, sender.String(), fof.String(), uuid.NewString()))
		require.NoError(t, tx.Commit())
		aud, err = presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.NotContains(t, aud, fof, "a direct block vetoes the default FoF path")
		_, err = db.Exec(`
			DELETE FROM friendships
			WHERE requester_id = $1 AND addressee_id = $2 AND status = 'blocked'
		`, sender, fof)
		require.NoError(t, err)

		// Explicit Off must still exclude FoF.
		testhelpers.SetFriendsOfFriends(t, db, sender, false)
		aud, err = presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.False(t, aud[fof], "FoF excluded when dm_friends_of_friends is off")
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

func TestComputeCustomTextAudience_MasterOffKeepsCapturedPriorTierIndependent(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	sender := testhelpers.CreateUser(t, db)
	friend := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, sender, friend)
	testhelpers.SetCustomTextTier(t, db, sender, 2)
	_, err := db.Exec(
		`UPDATE user_presence_settings SET master_enabled = FALSE WHERE user_id = $1`,
		sender,
	)
	require.NoError(t, err)

	current, err := presence.ComputeCustomTextAudience(ctx, db, sender)
	require.NoError(t, err)
	require.Empty(t, current, "master off must suppress the current Custom Status audience")

	prior, err := presence.ComputeCustomTextAudienceForTier(ctx, db, sender, 2)
	require.NoError(t, err)
	require.True(t, prior[friend], "captured prior-tier reconstruction remains master agnostic")
}
