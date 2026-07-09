package database_test

import (
	"context"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000082 (CV-CAN-010/011/012) replaces the existence-only
// channels.group_id FK with a composite (group_id, server_id) FK into
// channel_groups(id, server_id), so a channel can only be bound to a category
// owned by the SAME server. It also performs two one-way security cleanups:
// NULLing any pre-existing cross-server group bindings, and purging the
// channel_permission_overrides the sync cascade had copied from those foreign
// categories (those rows are applied by channel_id and would otherwise stay
// active and orphaned once group_id is nulled).
//
// Scope note (mirrors the migration_000079 precedent): SetupTestServer applies
// ALL migrations at setup against an empty DB, and the composite FK this
// migration adds makes the offending pre-migration state (a channel bound to a
// foreign server's category, with foreign-derived channel_permission_overrides)
// impossible to reconstruct afterwards. The one-time data cleanups (the
// group_id NULLing and the channel_permission_overrides purge) are therefore
// NOT runtime-exercised here; they are read-verifiable in
// 000082_channel_group_same_server.up.sql. What IS tested is the durable
// structural guarantee those cleanups clear the way for: the composite FK
// rejects a cross-server binding continuously, not just at migrate time.
func TestMigration000082_CrossServerCategoryBinding(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	t.Run("the composite unique and FK constraints exist", func(t *testing.T) {
		var uniqueN, fkN, oldN int
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_constraint WHERE conname = 'channel_groups_id_server_key'`).Scan(&uniqueN))
		assert.Equal(t, 1, uniqueN, "migration 000082 must add channel_groups_id_server_key")

		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_constraint WHERE conname = 'channels_group_server_fkey'`).Scan(&fkN))
		assert.Equal(t, 1, fkN, "migration 000082 must add channels_group_server_fkey")

		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_constraint WHERE conname = 'channels_group_id_fkey'`).Scan(&oldN))
		assert.Equal(t, 0, oldN, "migration 000082 must drop the existence-only channels_group_id_fkey")
	})

	owner := ts.CreateTestUser(t, "cvcan082owner")
	serverA := ts.CreateTestServer(t, owner.ID, "CV-CAN-082 Server A")
	serverB := ts.CreateTestServer(t, owner.ID, "CV-CAN-082 Server B")

	// A category owned by server B (a "foreign" category from server A's view).
	var foreignCategoryID string
	require.NoError(t, ts.DB.QueryRowContext(ctx,
		`INSERT INTO channel_groups (id, server_id, name) VALUES (gen_random_uuid(), $1, 'B Category') RETURNING id`,
		serverB).Scan(&foreignCategoryID))

	channelA := ts.CreateTestChannel(t, serverA, "a-chan")

	t.Run("the composite FK rejects binding a channel to a foreign server's category", func(t *testing.T) {
		_, err := ts.DB.ExecContext(ctx,
			`UPDATE channels SET group_id = $1 WHERE id = $2`, foreignCategoryID, channelA)
		require.Error(t, err,
			"binding channelA (server A) to a server B category must violate channels_group_server_fkey")
	})

	t.Run("a same-server binding is allowed and category delete nulls only group_id", func(t *testing.T) {
		var sameCategoryID string
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`INSERT INTO channel_groups (id, server_id, name) VALUES (gen_random_uuid(), $1, 'A Category') RETURNING id`,
			serverA).Scan(&sameCategoryID))

		_, err := ts.DB.ExecContext(ctx,
			`UPDATE channels SET group_id = $1 WHERE id = $2`, sameCategoryID, channelA)
		require.NoError(t, err, "binding to a same-server category must be allowed")

		// ON DELETE SET NULL (group_id) must null ONLY group_id while preserving
		// the NOT NULL server_id (the PG15+ column-list form the migration uses).
		_, err = ts.DB.ExecContext(ctx, `DELETE FROM channel_groups WHERE id = $1`, sameCategoryID)
		require.NoError(t, err)

		var groupID *string
		var serverID string
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT group_id, server_id FROM channels WHERE id = $1`, channelA).Scan(&groupID, &serverID))
		assert.Nil(t, groupID, "deleting the category must null the channel's group_id")
		assert.Equal(t, serverA, serverID, "server_id must be preserved when the category is deleted")
	})
}
