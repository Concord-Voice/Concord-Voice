package presence_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func addCustomTextOverrides(t *testing.T, db *sql.DB, sender uuid.UUID, targets ...uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO presence_override_preferences (user_id, category, encrypted_data)
		VALUES ($1, 'custom_text', 'dGVzdA==')
	`, sender)
	require.NoError(t, err)

	for _, target := range targets {
		_, err = db.Exec(`
			INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
			VALUES ($1, 'custom_text', $2)
		`, sender, target)
		require.NoError(t, err)
	}
}

type overrideQueryErrorDB struct {
	presence.DBTX
}

func (db overrideQueryErrorDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "FROM user_presence_overrides") {
		return nil, errors.New("forced override query failure")
	}
	return db.DBTX.QueryContext(ctx, query, args...)
}

func TestComputeCustomTextAudience_AcceptsTransaction(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	require.NoError(t, testhelpers.TruncateAllTables(db))
	ctx := context.Background()

	sender := testhelpers.CreateUser(t, db)
	friend := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, sender, friend)
	testhelpers.SetCustomTextTier(t, db, sender, 1)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			t.Errorf("rollback audience transaction: %v", rollbackErr)
		}
	}()

	audience, err := presence.ComputeCustomTextAudience(ctx, tx, sender)
	require.NoError(t, err)
	require.True(t, audience[friend])
}

func TestComputeCustomTextAudience_OverrideFilter(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	t.Run("no exclusions preserves the tier audience", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		friend := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, friend)
		testhelpers.SetCustomTextTier(t, db, sender, 1)

		audience, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.True(t, audience[friend])
	})

	t.Run("one exclusion removes only that viewer", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		excluded := testhelpers.CreateUser(t, db)
		permitted := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, excluded)
		testhelpers.AddFriendship(t, db, sender, permitted)
		testhelpers.SetCustomTextTier(t, db, sender, 1)
		addCustomTextOverrides(t, db, sender, excluded)

		audience, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.False(t, audience[excluded])
		require.True(t, audience[permitted])
	})

	t.Run("many exclusions remove every selected viewer", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		excludedA := testhelpers.CreateUser(t, db)
		excludedB := testhelpers.CreateUser(t, db)
		permitted := testhelpers.CreateUser(t, db)
		for _, viewer := range []uuid.UUID{excludedA, excludedB, permitted} {
			testhelpers.AddFriendship(t, db, sender, viewer)
		}
		testhelpers.SetCustomTextTier(t, db, sender, 1)
		addCustomTextOverrides(t, db, sender, excludedA, excludedB)

		audience, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.False(t, audience[excludedA])
		require.False(t, audience[excludedB])
		require.True(t, audience[permitted])
	})

	t.Run("Off remains empty with an exclusion", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		friend := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, friend)
		testhelpers.SetCustomTextTier(t, db, sender, 0)
		addCustomTextOverrides(t, db, sender, friend)

		audience, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.Empty(t, audience)
	})

	t.Run("friend-of-friend can be excluded", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		friend := testhelpers.CreateUser(t, db)
		friendOfFriend := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, sender, friend)
		testhelpers.AddFriendship(t, db, friend, friendOfFriend)
		testhelpers.SetFriendsOfFriends(t, db, sender, true)
		testhelpers.SetCustomTextTier(t, db, sender, 1)
		addCustomTextOverrides(t, db, sender, friendOfFriend)

		audience, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.True(t, audience[friend])
		require.False(t, audience[friendOfFriend])
	})

	t.Run("shared-server peer can be excluded", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		sender := testhelpers.CreateUser(t, db)
		peer := testhelpers.CreateUser(t, db)
		server := testhelpers.CreateServer(t, db, sender)
		testhelpers.AddServerMember(t, db, server, sender)
		testhelpers.AddServerMember(t, db, server, peer)
		testhelpers.SetCustomTextTier(t, db, sender, 2)
		addCustomTextOverrides(t, db, sender, peer)

		audience, err := presence.ComputeCustomTextAudience(ctx, db, sender)
		require.NoError(t, err)
		require.False(t, audience[peer])
	})
}

func TestComputeCustomTextAudience_OverrideQueryFailureFailsClosed(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	require.NoError(t, testhelpers.TruncateAllTables(db))
	ctx := context.Background()

	sender := testhelpers.CreateUser(t, db)
	friend := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, sender, friend)
	testhelpers.SetCustomTextTier(t, db, sender, 1)

	audience, err := presence.ComputeCustomTextAudience(ctx, overrideQueryErrorDB{DBTX: db}, sender)
	require.ErrorContains(t, err, "forced override query failure")
	require.Nil(t, audience)
}
