package rbac_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
)

// TestMFAMaskVisibility proves I-2 (spec #3453 §4 "visibility resolvers stay
// raw", §13 I-2): the MFA-enforcement mask must never change what a member
// can see. One server carries an Administrator, a moderator, a text and a
// voice channel hidden from @all, and one ordinary visible channel. The same
// results are computed three times — enforcement off, enforcement on with
// neither member enrolled, and enforcement on with both enrolled — and every
// visibility surface must come back byte-identical across all three:
// GetVisibleChannelIDs, GetAllVisibleChannelIDs, the channel-viewer filter
// behind FilterVisibleUserIDsForChannelFresh, and the voice-permission bits
// (PermViewVoiceChannels, PermJoinVoice, PermSpeak, PermScreenShare,
// PermVideo) from ResolveEffectivePermissionsUncached.
func TestMFAMaskVisibility(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "i2visowner")
	admin := ts.CreateTestUser(t, "i2visadmin")
	mod := ts.CreateTestUser(t, "i2vismod")
	serverID := ts.CreateTestServer(t, owner.ID, "I2 Visibility Server")

	ts.AddMemberToServer(t, serverID, admin.ID, "member")
	ts.AddMemberToServer(t, serverID, mod.ID, "member")

	adminRole := ts.CreateTestRole(t, serverID, "Administrator", 10, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRole)

	modRole := ts.CreateTestRole(t, serverID, "Moderator", 6, int64(rbac.ModeratorPermissions))
	ts.AssignRoleToUser(t, serverID, mod.ID, modRole)

	visibleChannel := ts.CreateTestChannel(t, serverID, "visible")
	hiddenText := ts.CreateTestChannel(t, serverID, "hidden-text")
	hiddenVoice := ts.CreateVoiceChannel(t, serverID, "hidden-voice")

	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID,
	).Scan(&allRoleID))

	// Hide both non-visible channels from @all — DENY the type-appropriate
	// view bit on the default role.
	ts.CreateChannelOverride(t, hiddenText, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	ts.CreateChannelOverride(t, hiddenVoice, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))

	compute := func(t *testing.T) visSnapshot {
		t.Helper()
		return computeVisSnapshot(ctx, t, resolver, serverID, owner.ID, admin.ID, mod.ID,
			visibleChannel, hiddenText, hiddenVoice)
	}

	// off
	off := compute(t)

	// on+unenrolled: enforce_mfa_dangerous_actions = TRUE, no factor rows.
	enforceMFA(t, ts, serverID)
	onUnenrolled := compute(t)

	// on+enrolled: same flag, plus a WebAuthn row for both members.
	enrollWebAuthn(t, ts, admin.ID)
	enrollWebAuthn(t, ts, mod.ID)
	onEnrolled := compute(t)

	// Ground truth, independent of MFA state: an Administrator sees every
	// channel (raw bit 62 bypasses SBAC entirely); a moderator sees only the
	// one channel @all was never denied on; and the hidden channels' only
	// viewers are the owner (identity bypass) and the Administrator (raw bit
	// 62). None of this depends on rbac.DangerousPermissions, which is why a
	// stripped-Administrator (the mask's effect when it wrongly binds here)
	// changes it: the fast-path bypass in visibleChannelIDs is driven by the
	// RAW basePerms, and losing bit 62 falls through into SBAC evaluation,
	// where @all's DENY overrides start to apply.
	wantAdminVisible := []string{visibleChannel, hiddenText, hiddenVoice}
	wantModVisible := []string{visibleChannel}
	wantHiddenViewers := []string{owner.ID, admin.ID}

	for name, got := range map[string]visSnapshot{
		"off":           off,
		"on+unenrolled": onUnenrolled,
		"on+enrolled":   onEnrolled,
	} {
		t.Run(name, func(t *testing.T) {
			assert.ElementsMatch(t, wantAdminVisible, got.adminVisible, "admin GetVisibleChannelIDs")
			assert.ElementsMatch(t, wantAdminVisible, got.adminAllVisible, "admin GetAllVisibleChannelIDs")
			assert.ElementsMatch(t, wantModVisible, got.modVisible, "moderator GetVisibleChannelIDs")
			assert.ElementsMatch(t, wantModVisible, got.modAllVisible, "moderator GetAllVisibleChannelIDs")
			assert.ElementsMatch(t, wantHiddenViewers, got.hiddenTextViewers, "hidden text channel viewers")
			assert.ElementsMatch(t, wantHiddenViewers, got.hiddenVoiceViewers, "hidden voice channel viewers")
		})
	}

	// The three configurations must also be identical to EACH OTHER, not
	// merely to the hardcoded ground truth above — this is the literal I-2
	// assertion (spec §13).
	for _, pair := range []struct {
		name string
		a, b visSnapshot
	}{
		{"off vs on+unenrolled", off, onUnenrolled},
		{"off vs on+enrolled", off, onEnrolled},
		{"on+unenrolled vs on+enrolled", onUnenrolled, onEnrolled},
	} {
		t.Run(pair.name, func(t *testing.T) {
			assert.ElementsMatch(t, pair.a.adminVisible, pair.b.adminVisible)
			assert.ElementsMatch(t, pair.a.adminAllVisible, pair.b.adminAllVisible)
			assert.ElementsMatch(t, pair.a.modVisible, pair.b.modVisible)
			assert.ElementsMatch(t, pair.a.modAllVisible, pair.b.modAllVisible)
			assert.ElementsMatch(t, pair.a.hiddenTextViewers, pair.b.hiddenTextViewers)
			assert.ElementsMatch(t, pair.a.hiddenVoiceViewers, pair.b.hiddenVoiceViewers)
			assert.Equal(t, pair.a.voiceBits, pair.b.voiceBits, "ResolveEffectivePermissionsUncached voice bits")
		})
	}
}

// visSnapshot is every visibility-surface result computed for one MFA
// configuration.
type visSnapshot struct {
	adminVisible, adminAllVisible []string
	modVisible, modAllVisible     []string
	hiddenTextViewers             []string
	hiddenVoiceViewers            []string
	// voiceBits is keyed "<persona>/<channel>" and holds only the five voice
	// bits (spec I-2): PermViewVoiceChannels, PermJoinVoice, PermSpeak,
	// PermScreenShare, PermVideo.
	voiceBits map[string]rbac.Permission
}

const mfaVisibilityVoiceBits = rbac.PermViewVoiceChannels | rbac.PermJoinVoice | rbac.PermSpeak |
	rbac.PermScreenShare | rbac.PermVideo

func computeVisSnapshot(
	ctx context.Context,
	t *testing.T,
	r *rbac.Resolver,
	serverID, ownerID, adminID, modID string,
	visibleChannel, hiddenText, hiddenVoice string,
) visSnapshot {
	t.Helper()
	var snap visSnapshot
	var err error

	snap.adminVisible, err = r.GetVisibleChannelIDs(ctx, serverID, adminID)
	require.NoError(t, err)
	snap.adminAllVisible, err = r.GetAllVisibleChannelIDs(ctx, adminID)
	require.NoError(t, err)
	snap.modVisible, err = r.GetVisibleChannelIDs(ctx, serverID, modID)
	require.NoError(t, err)
	snap.modAllVisible, err = r.GetAllVisibleChannelIDs(ctx, modID)
	require.NoError(t, err)

	candidates := []string{ownerID, adminID, modID}
	snap.hiddenTextViewers, err = r.FilterVisibleUserIDsForChannelFresh(ctx, serverID, hiddenText, candidates)
	require.NoError(t, err)
	snap.hiddenVoiceViewers, err = r.FilterVisibleUserIDsForChannelFresh(ctx, serverID, hiddenVoice, candidates)
	require.NoError(t, err)

	snap.voiceBits = make(map[string]rbac.Permission, 6)
	personas := map[string]string{"admin": adminID, "moderator": modID}
	channels := map[string]string{"visible": visibleChannel, "hiddenText": hiddenText, "hiddenVoice": hiddenVoice}
	for personaName, personaID := range personas {
		for channelName, channelID := range channels {
			perms, err := r.ResolveEffectivePermissionsUncached(ctx, serverID, personaID, channelID)
			require.NoError(t, err)
			snap.voiceBits[personaName+"/"+channelName] = perms & mfaVisibilityVoiceBits
		}
	}
	return snap
}
