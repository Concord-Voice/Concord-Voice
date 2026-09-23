//go:build integration

package rbac

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteCategoryOverride_DoesNotCascadeStaleParentAfterABA(t *testing.T) {
	env := newRBACPresenceEnv(t)
	defer env.Close()

	categoryA := env.createCategory(t)
	oldChild := env.createVoiceChannel(t, categoryA, true)
	overrideID := uuid.NewString()
	env.exec(`INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, 'role', $3, $4, 0)`, overrideID, categoryA, env.viewRole, int64(PermViewVoiceChannels))
	env.exec(`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, 'role', $3, $4, 0)`, uuid.NewString(), oldChild, env.viewRole, int64(PermViewVoiceChannels))

	preflightReached := make(chan struct{})
	var once sync.Once
	SetSyncedCategoryPreflightForTest(env.handler, func() { once.Do(func() { close(preflightReached) }) })

	tx, err := env.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, LockServerVisibilityCapture(context.Background(), tx, env.serverID))
	defer tx.Rollback() //nolint:errcheck // cleanup after commit is a no-op

	result := make(chan error, 1)
	go func() {
		_, _, deleteErr := env.handler.deleteCategoryOverrideWithCapture(
			context.Background(), categoryAuthorityRequest{
				serverID: env.serverID, categoryID: categoryA, userID: env.serverOwnerID,
			}, overrideID, "role", env.viewRole,
			&[]keyrotation.Rotation{}, &map[string][]string{},
		)
		result <- deleteErr
	}()

	select {
	case <-preflightReached:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for synchronized category preflight")
	}
	// Replace override A with B under the same category/target while the stale
	// attempt waits. A must not be mistaken for B when the delete commits.
	_, err = tx.Exec(`DELETE FROM category_permission_overrides WHERE id = $1`, overrideID)
	require.NoError(t, err)
	newOverrideID := uuid.NewString()
	_, err = tx.Exec(`INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, 'role', $3, 0, $4)`, newOverrideID, categoryA, env.viewRole, int64(PermJoinVoice))
	require.NoError(t, err)
	_, err = tx.Exec(`DELETE FROM channel_permission_overrides WHERE channel_id = $1 AND target_type = 'role' AND target_id = $2`, oldChild, env.viewRole)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, 'role', $3, 0, $4)`, uuid.NewString(), oldChild, env.viewRole, int64(PermJoinVoice))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	resultErr := <-result
	assert.True(t, errors.Is(resultErr, sql.ErrNoRows), "stale A delete must report that override A no longer exists: %v", resultErr)
	var retainedOverrideID string
	var retainedDeny int64
	require.NoError(t, env.db.QueryRow(`SELECT id, deny FROM category_permission_overrides WHERE category_id = $1 AND target_type = 'role' AND target_id = $2`, categoryA, env.viewRole).Scan(&retainedOverrideID, &retainedDeny))
	assert.Equal(t, newOverrideID, retainedOverrideID, "replacement override B remains the category parent row")
	assert.Equal(t, int64(PermJoinVoice), retainedDeny, "replacement override B retains its value")
	var childOverrides int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1 AND target_type = 'role' AND target_id = $2 AND allow = 0 AND deny = $3`, oldChild, env.viewRole, int64(PermJoinVoice)).Scan(&childOverrides))
	assert.Equal(t, 1, childOverrides, "replacement override B and its child enforcement remain unchanged")
}
