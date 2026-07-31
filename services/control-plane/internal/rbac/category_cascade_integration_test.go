//go:build integration

package rbac

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DeleteCategoryOverride's child cascade was the one narrowing write in the
// RBAC family left unhooked by #2445 — absent from the design's §6.7 matrix,
// and issuing its DELETE through a bare `_, _ = h.db.Exec` whose error was
// discarded. The #2445 review's adversarial pass proved the disclosure with a
// proof-of-concept: the exploit arm narrowed visibility with zero captures and
// zero clears, while its control arm — the identical narrowing routed through
// the already-hooked DeleteChannelOverride — captured the victim.
//
// This is that control/exploit pair collapsed into one regression: the viewer's
// ONLY sight of the occupied channel is the allow-override the category mirrors
// onto its synced child, so deleting the category override strictly narrows.
func TestDeleteCategoryOverride_CascadeNarrowing_ClearsExactlyTheLosingViewer(t *testing.T) {
	env := newRBACPresenceEnv(t)
	defer env.Close()

	category := env.createCategory(t)
	channel := env.createVoiceChannel(t, category, true)

	viewer := env.addMemberWithoutSight(t)
	grantRole := env.createRole("cat-allow", 0)
	env.assignRole(t, viewer, grantRole)

	overrideID := uuid.New().String()
	env.exec(`INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny)
	          VALUES ($1, $2, 'role', $3, $4, 0)`,
		overrideID, category, grantRole, int64(PermViewVoiceChannels))
	env.handler.syncCategoryOverridesToChannels(context.Background(), env.serverID, category)

	// Retains sight through viewRole's base permissions, so the cascade must
	// leave it alone — the no-false-clear half.
	retained := env.addViewerViaRole(t, env.viewRole)

	require.True(t, env.viewerCanSeeChannel(t, channel, viewer),
		"precondition: the mirrored allow is this viewer's only sight of the channel")
	require.True(t, env.viewerCanSeeChannel(t, channel, retained),
		"precondition: the control viewer sees the channel through its role")

	sender := env.joinVoice(t, channel)
	env.waitForDispatch(t)

	require.NoError(t, env.handler.deleteCategoryOverrideWithCapture(
		context.Background(), env.serverID, category, overrideID, "role", grantRole,
	))
	env.waitForDispatch(t)

	assert.False(t, env.viewerCanSeeChannel(t, channel, viewer),
		"deleting the category override removes the mirrored child allow")
	assert.True(t, env.wasCleared(sender, viewer),
		"a viewer that lost sight through the cascade is cleared")
	assert.False(t, env.wasCleared(sender, retained),
		"a viewer retaining sight through a role is never false-cleared")
}

// The parent row and its mirrored children now share one transaction, so a
// failed cascade can no longer leave the parent deleted and the children stale
// behind an HTTP 200 — the shape the bare `_, _ = h.db.Exec` allowed.
func TestDeleteCategoryOverride_CaptureFailure_BlocksTheWrite(t *testing.T) {
	env := newRBACPresenceEnv(t)
	defer env.Close()

	category := env.createCategory(t)
	channel := env.createVoiceChannel(t, category, true)
	env.joinVoice(t, channel)

	overrideID := uuid.New().String()
	env.exec(`INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny)
	          VALUES ($1, $2, 'role', $3, $4, 0)`,
		overrideID, category, env.viewRole, int64(PermViewVoiceChannels))
	env.handler.syncCategoryOverridesToChannels(context.Background(), env.serverID, category)

	env.injectCaptureFailure()
	defer env.clearInjectedFailure()

	err := env.handler.deleteCategoryOverrideWithCapture(
		context.Background(), env.serverID, category, overrideID, "role", env.viewRole,
	)
	require.Error(t, err, "a capture failure blocks the write here: nothing has committed yet")

	var remaining int
	require.NoError(t, env.db.QueryRow(
		`SELECT COUNT(*) FROM category_permission_overrides WHERE id = $1`, overrideID,
	).Scan(&remaining))
	assert.Equal(t, 1, remaining,
		"the parent override is rolled back, not left half-applied")
}

// Spec §12 test 8: the cascade hook lives inside syncCategoryOverridesToChannels'
// existing transaction and captures exactly the SYNCED channel list it rewrites.
// Unsynced channels in the same category are untouched (F4/F5), and each sender
// receives exactly one RefreshServerVoiceRecheck, UUID-ordered.
func TestSyncCategoryOverridesToChannels_CapturesOnlySyncedChannels(t *testing.T) {
	env := newRBACPresenceEnv(t)
	defer env.Close()

	category := env.createCategory(t)
	synced := []string{
		env.createVoiceChannel(t, category, true),
		env.createVoiceChannel(t, category, true),
		env.createVoiceChannel(t, category, true),
	}
	unsynced := []string{
		env.createVoiceChannel(t, category, false),
		env.createVoiceChannel(t, category, false),
	}

	syncedSenders := make([]string, 0, len(synced))
	for _, channelID := range synced {
		syncedSenders = append(syncedSenders, env.joinVoice(t, channelID))
	}
	unsyncedSenders := make([]string, 0, len(unsynced))
	for _, channelID := range unsynced {
		unsyncedSenders = append(unsyncedSenders, env.joinVoice(t, channelID))
	}
	viewer := env.addViewerWithSight(t, append(synced, unsynced...))

	env.denyViewOnCategory(t, category)
	env.handler.syncCategoryOverridesToChannels(context.Background(), env.serverID, category)
	env.waitForDispatch(t)

	for _, senderID := range syncedSenders {
		assert.Equal(t, 1, env.refreshCount(senderID),
			"exactly one RefreshServerVoiceRecheck per synced-channel sender")
		assert.True(t, env.wasCleared(senderID, viewer))
	}
	for _, senderID := range unsyncedSenders {
		assert.Zero(t, env.refreshCount(senderID),
			"an unsynced channel is not rewritten, so its senders are untouched")
	}
	assert.True(t, env.refreshOrderIsUUIDSorted(), "dispatch order is deterministic")
}

// Spec §12 test 8 (second half): the two explicit non-hooks.
func TestSetChannelPermissionSyncFlag_IsVisibilityInert_NoCapture(t *testing.T) {
	env := newRBACPresenceEnv(t)
	defer env.Close()

	category := env.createCategory(t)
	channelID := env.createVoiceChannel(t, category, false)
	senderID := env.joinVoice(t, channelID)
	env.addViewerWithSight(t, []string{channelID})

	// `UPDATE channels SET sync_permissions` alone changes no visibility input:
	// filterVisibleUserIDsForChannel reads neither sync_permissions nor
	// category_permission_overrides.
	env.setSyncFlagOnly(t, channelID, true)
	env.waitForDispatch(t)

	assert.Zero(t, env.refreshCount(senderID))
}

func TestInvalidateSyncedChannelCaches_PerformsNoWrite_NoCapture(t *testing.T) {
	env := newRBACPresenceEnv(t)
	defer env.Close()

	category := env.createCategory(t)
	channelID := env.createVoiceChannel(t, category, false)
	senderID := env.joinVoice(t, channelID)

	env.handler.invalidateSyncedChannelCaches(context.Background(), env.serverID, category)
	env.waitForDispatch(t)

	require.Zero(t, env.refreshCount(senderID),
		"invalidateSyncedChannelCaches performs no write; it keeps only its recheckVoiceChannel loop")
}
