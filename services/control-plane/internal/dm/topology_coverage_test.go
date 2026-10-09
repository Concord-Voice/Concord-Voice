package dm

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedTopologyUsers(t *testing.T, db *sql.DB, count int) []uuid.UUID {
	t.Helper()
	users := make([]uuid.UUID, count)
	for i := range users {
		users[i] = dbtest.CreateUser(t, db)
	}
	return users
}

func setTopologyPrivacy(t *testing.T, db *sql.DB, user uuid.UUID, level int, fof bool) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO privacy_settings (user_id, dm_privacy_level, dm_friends_of_friends)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET dm_privacy_level = EXCLUDED.dm_privacy_level,
			dm_friends_of_friends = EXCLUDED.dm_friends_of_friends`, user, level, fof)
	require.NoError(t, err)
}

func addTopologyFriendship(t *testing.T, db *sql.DB, left, right uuid.UUID, status string) {
	t.Helper()
	if status == "blocked" {
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, $3)`, left, right, status)
		require.NoError(t, err)
		require.NoError(t, dmblock.RecordBlockTx(context.Background(), tx, left.String(), right.String(), uuid.NewString()))
		require.NoError(t, tx.Commit())
		return
	}
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, $3)`, left, right, status)
	require.NoError(t, err)
}

func TestDMTopologyPredicates_CoverRelationshipAndFailurePaths(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	users := seedTopologyUsers(t, db, 7)
	ctx := context.Background()

	t.Run("blocked and pending pairs fail closed", func(t *testing.T) {
		addTopologyFriendship(t, db, users[0], users[1], "blocked")
		blocked, err := dmTopologyBlockedTx(ctx, db, users[0].String(), users[1].String())
		require.NoError(t, err)
		assert.True(t, blocked)

		left, right := users[2], users[3]
		if left.String() > right.String() {
			left, right = right, left
		}
		_, err = db.Exec(`INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a)
			VALUES ($1, $2, $3, true)`, left, right, uuid.New())
		require.NoError(t, err)
		blocked, err = dmTopologyBlockedTx(ctx, db, users[2].String(), users[3].String())
		require.NoError(t, err)
		assert.True(t, blocked)
	})

	t.Run("privacy relationship branches", func(t *testing.T) {
		// Open-to-all short-circuits after the friendship query.
		setTopologyPrivacy(t, db, users[3], dmPrivacyOpenToAll, false)
		allowed, err := dmTopologyPermittedTx(ctx, db, users[1].String(), users[3].String())
		require.NoError(t, err)
		assert.True(t, allowed)

		// Friends are allowed even when server fallback is disabled.
		setTopologyPrivacy(t, db, users[2], dmPrivacyFriendsOnly, false)
		addTopologyFriendship(t, db, users[1], users[2], "accepted")
		allowed, err = dmTopologyPermittedTx(ctx, db, users[1].String(), users[2].String())
		require.NoError(t, err)
		assert.True(t, allowed)

		// Friends-of-friends are allowed when the shared friend is present.
		setTopologyPrivacy(t, db, users[4], dmPrivacyFriendsOnly, true)
		addTopologyFriendship(t, db, users[5], users[6], "accepted")
		addTopologyFriendship(t, db, users[6], users[4], "accepted")
		allowed, err = dmTopologyPermittedTx(ctx, db, users[5].String(), users[4].String())
		require.NoError(t, err)
		assert.True(t, allowed)

		setTopologyPrivacy(t, db, users[0], dmPrivacyOff, false)
		allowed, err = dmTopologyPermittedTx(ctx, db, users[1].String(), users[0].String())
		require.NoError(t, err)
		assert.False(t, allowed)

		// Friends-only with no relationship reaches the explicit denial arm.
		setTopologyPrivacy(t, db, users[4], dmPrivacyFriendsOnly, false)
		allowed, err = dmTopologyPermittedTx(ctx, db, users[0].String(), users[4].String())
		require.NoError(t, err)
		assert.False(t, allowed)
	})

	t.Run("topology recheck rejects any blocked pair", func(t *testing.T) {
		got := recheckDMTopologyTx(ctx, db, users[0], []uuid.UUID{users[0], users[1]})
		assert.ErrorIs(t, got, errDMTopologyBlocked)
	})

	t.Run("closed database reports query errors", func(t *testing.T) {
		closed, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
		require.NoError(t, err)
		require.NoError(t, closed.Close())
		_, err = dmTopologyBlockedTx(ctx, closed, users[0].String(), users[1].String())
		assert.Error(t, err)
	})
}

func TestDMTopologyFriendOfFriendDefaultWithoutPrivacyRow(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	users := seedTopologyUsers(t, db, 3)
	ctx := context.Background()
	addTopologyFriendship(t, db, users[0], users[1], "accepted")
	addTopologyFriendship(t, db, users[1], users[2], "accepted")
	h := NewHandler(HandlerDeps{DB: db})
	level, fof, err := h.fetchDMPrivacySettings(users[2].String())
	require.NoError(t, err)
	assert.Equal(t, dmPrivacyFriendsAndServer, level)
	assert.True(t, fof, "the ordinary DM path must use the same no-row default")

	allowed, err := dmTopologyPermittedTx(ctx, db, users[0].String(), users[2].String())
	require.NoError(t, err)
	assert.True(t, allowed, "a fresh target accepts a mutual friend's DM")

	setTopologyPrivacy(t, db, users[2], dmPrivacyFriendsAndServer, false)
	allowed, err = dmTopologyPermittedTx(ctx, db, users[0].String(), users[2].String())
	require.NoError(t, err)
	assert.False(t, allowed, "a saved Off preference still denies the mutual friend")
}

func TestLockPreparedDMParticipants_DetectsMembershipMismatch(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	users := seedTopologyUsers(t, db, 2)
	var convID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, created_by) VALUES (false, $1) RETURNING id`, users[0]).Scan(&convID))
	_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, convID, users[0], users[1])
	require.NoError(t, err)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	err = lockPreparedDMParticipantsTx(context.Background(), tx, convID, []uuid.UUID{users[0]})
	assert.ErrorIs(t, err, dmblock.ErrMembershipChanged)
}

func TestBeginDMTopologyEffect_SuccessAndInvalidPreparation(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	users := seedTopologyUsers(t, db, 2)
	var convID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, created_by) VALUES (false, $1) RETURNING id`, users[0]).Scan(&convID))
	_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, convID, users[0], users[1])
	require.NoError(t, err)
	h := NewHandler(HandlerDeps{DB: db})
	tx, err := h.beginDMTopologyEffect(context.Background(), convID, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	_, err = h.beginDMTopologyEffect(context.Background(), "not-a-uuid", nil)
	assert.Error(t, err)
}
