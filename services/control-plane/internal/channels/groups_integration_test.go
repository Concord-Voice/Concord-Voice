package channels_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assignChannelToCategory(t *testing.T, ts *testhelpers.TestServer, channelID, groupID string, synced bool) {
	t.Helper()
	_, err := ts.DB.Exec(`UPDATE channels SET group_id = $1, sync_permissions = $2 WHERE id = $3`, groupID, synced, channelID)
	require.NoError(t, err)
}

func TestDeleteChannelGroup_ClearsInheritedOverridesFromSyncedChildren(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "delete-inherited", owner.AccessToken)
	channelID := ts.CreateVoiceChannel(t, serverID, "delete-inherited-channel")
	assignChannelToCategory(t, ts, channelID, groupID, true)
	unsyncedID := ts.CreateTestChannel(t, serverID, "delete-manual-channel")
	assignChannelToCategory(t, ts, unsyncedID, groupID, false)
	member := ts.CreateTestUser(t, "delete-inherited-member")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	_, err := ts.DB.Exec(`
		INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny)
		VALUES (gen_random_uuid(), $1, 'user', $2, $3, 0)`,
		groupID, member.ID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		SELECT gen_random_uuid(), $1, target_type, target_id, allow, deny
		FROM category_permission_overrides WHERE category_id = $2`, channelID, groupID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		VALUES (gen_random_uuid(), $1, 'user', $2, 0, $3)`,
		unsyncedID, member.ID, int64(rbac.PermJoinVoice))
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var group sql.NullString
	var syncEnabled bool
	require.NoError(t, ts.DB.QueryRow(`SELECT group_id, sync_permissions FROM channels WHERE id = $1`, channelID).Scan(&group, &syncEnabled))
	assert.False(t, group.Valid, "deleting a category uncategorizes its children")
	assert.False(t, syncEnabled, "a child cannot remain marked synced without its category")
	var inherited int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, channelID).Scan(&inherited))
	assert.Zero(t, inherited, "deleting a category must not leave copied VIEW/JOIN authority behind")
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, unsyncedID).Scan(&inherited))
	assert.Equal(t, 1, inherited, "deleting a category must preserve an unsynced child's manual override")
}

func TestDeleteChannelGroup_RefusesMoreThan500ChildrenBeforeWrite(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "too-many-children", owner.AccessToken)
	for i := 0; i < 501; i++ {
		channelID := ts.CreateTestChannel(t, serverID, "large-group-channel")
		assignChannelToCategory(t, ts, channelID, groupID, false)
	}

	w := ts.DoRequest("DELETE", groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var groupCount, childCount int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_groups WHERE id = $1`, groupID).Scan(&groupCount))
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channels WHERE group_id = $1`, groupID).Scan(&childCount))
	assert.Equal(t, 1, groupCount, "bounded preflight must not delete the category")
	assert.Equal(t, 501, childCount, "bounded preflight must not detach any child")
}

func TestReorderChannels_CanonicalizesAlternateUUIDSpellingsBeforeSyncedMove(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	sourceID := createGroup(t, ts, serverID, "move-source", owner.AccessToken)
	destinationID := createGroup(t, ts, serverID, "move-destination", owner.AccessToken)
	channelID := ts.CreateVoiceChannel(t, serverID, "move-synced-channel")
	assignChannelToCategory(t, ts, channelID, sourceID, true)
	member := ts.CreateTestUser(t, "move-synced-member")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	_, err := ts.DB.Exec(`
		INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny)
		VALUES (gen_random_uuid(), $1, 'user', $2, $3, 0),
		       (gen_random_uuid(), $4, 'user', $2, 0, $5)`,
		sourceID, member.ID, int64(rbac.PermViewVoiceChannels), destinationID, int64(rbac.PermViewVoiceChannels))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		SELECT gen_random_uuid(), $1, target_type, target_id, allow, deny
		FROM category_permission_overrides WHERE category_id = $2`, channelID, sourceID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, 'wrapped', 1)`,
		channelID, member.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{"channels": []map[string]interface{}{{
		"channel_id": "{" + channelID + "}", "group_id": "{" + destinationID + "}", "position": 0,
	}}}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var allow, deny int64
	require.NoError(t, ts.DB.QueryRow(`
		SELECT allow, deny FROM channel_permission_overrides
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`, channelID, member.ID).Scan(&allow, &deny))
	assert.Zero(t, allow, "source-category authority must not survive a synced move")
	assert.Equal(t, int64(rbac.PermViewVoiceChannels), deny, "destination-category authority must be inherited")
	var keyCount, revocationCount int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID,
	).Scan(&keyCount))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1`, channelID,
	).Scan(&revocationCount))
	assert.Zero(t, keyCount, "a member newly denied VIEW must lose its channel key")
	assert.Equal(t, 1, revocationCount, "a member newly denied VIEW must advance the channel key epoch")
	w = ts.DoRequest("POST", "/api/v1/e2ee/validate-epochs", map[string]interface{}{
		"epochs": map[string]int{channelID: 1},
	}, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var validation map[string]interface{}
	testhelpers.ParseJSON(t, w, &validation)
	assert.Equal(t, []interface{}{channelID}, validation["access_lost"])

	w = ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{"channels": []map[string]interface{}{{
		"channel_id": channelID, "group_id": nil, "position": 0,
	}}}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var syncEnabled bool
	require.NoError(t, ts.DB.QueryRow(`SELECT sync_permissions FROM channels WHERE id = $1`, channelID).Scan(&syncEnabled))
	assert.False(t, syncEnabled, "ungrouping must unsync the former category child")
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, channelID).Scan(&allow))
	assert.Zero(t, allow, "ungrouping must clear inherited destination authority")
}

func TestReorderChannels_RejectsProtectedMoveGrant(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	sourceID := createGroup(t, ts, serverID, "protected-source", owner.AccessToken)
	destinationID := createGroup(t, ts, serverID, "protected-destination", owner.AccessToken)
	channelID := ts.CreateVoiceChannel(t, serverID, "protected-channel")
	assignChannelToCategory(t, ts, channelID, sourceID, true)
	member := ts.CreateTestUser(t, "protected-member")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	_, err := ts.DB.Exec(`
		INSERT INTO channel_permission_overrides
		(id, channel_id, target_type, target_id, allow, deny, is_temporary, temporary_reason, granted_at)
		VALUES (gen_random_uuid(), $1, 'user', $2, $3, 0, TRUE, 'move_granted', NOW())`,
		channelID, member.ID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice))
	require.NoError(t, err)

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{"channels": []map[string]interface{}{{
		"channel_id": channelID, "group_id": destinationID, "position": 0,
	}}}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var groupID string
	var syncEnabled bool
	require.NoError(t, ts.DB.QueryRow(`SELECT group_id, sync_permissions FROM channels WHERE id = $1`, channelID).Scan(&groupID, &syncEnabled))
	assert.Equal(t, sourceID, groupID)
	assert.True(t, syncEnabled)
}

func TestUpdateChannel_ReconcilesSyncedAuthorityAcrossCategoryMoves(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	sourceID := createGroup(t, ts, serverID, "update-source", owner.AccessToken)
	destinationID := createGroup(t, ts, serverID, "update-destination", owner.AccessToken)
	channelID := ts.CreateVoiceChannel(t, serverID, "update-synced-channel")
	assignChannelToCategory(t, ts, channelID, sourceID, true)
	member := ts.CreateTestUser(t, "update-synced-member")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	_, err := ts.DB.Exec(`
		INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny)
		VALUES (gen_random_uuid(), $1, 'user', $2, $3, 0),
		       (gen_random_uuid(), $4, 'user', $2, 0, $5)`,
		sourceID, member.ID, int64(rbac.PermViewVoiceChannels), destinationID, int64(rbac.PermJoinVoice))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		SELECT gen_random_uuid(), $1, target_type, target_id, allow, deny
		FROM category_permission_overrides WHERE category_id = $2`, channelID, sourceID)
	require.NoError(t, err)

	w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
		"name": "update-synced-channel", "type": "voice", "group_id": destinationID,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var allow, deny int64
	require.NoError(t, ts.DB.QueryRow(`
		SELECT allow, deny FROM channel_permission_overrides
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`, channelID, member.ID).Scan(&allow, &deny))
	assert.Zero(t, allow)
	assert.Equal(t, int64(rbac.PermJoinVoice), deny)

	w = ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
		"name": "update-synced-channel", "type": "voice", "group_id": "",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var syncEnabled bool
	require.NoError(t, ts.DB.QueryRow(`SELECT sync_permissions FROM channels WHERE id = $1`, channelID).Scan(&syncEnabled))
	assert.False(t, syncEnabled)
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, channelID).Scan(&allow))
	assert.Zero(t, allow)
}

func TestUpdateChannel_CanonicalizesSameGroupAlias(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "update-canonical-group", owner.AccessToken)
	channelID := ts.CreateVoiceChannel(t, serverID, "update-canonical-channel")
	assignChannelToCategory(t, ts, channelID, groupID, true)

	w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
		"name": "update-canonical-renamed", "type": "voice",
		"group_id": "{" + strings.ToUpper(groupID) + "}",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var storedGroupID, name string
	require.NoError(t, ts.DB.QueryRow(`SELECT group_id, name FROM channels WHERE id = $1`, channelID).Scan(&storedGroupID, &name))
	assert.Equal(t, groupID, storedGroupID)
	assert.Equal(t, "update-canonical-renamed", name)
}

func TestUpdateChannel_CanonicalizesRouteIDBeforePermissionRecheck(t *testing.T) {
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}
	t.Setenv("NATS_URL", natsURL)
	observer, err := natsclient.Connect(natsURL)
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live enforcement test (runs in CI)", err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	msgs := make(chan map[string]interface{}, 8)
	sub, err := observer.Subscribe("voice.enforce.permissions", func(data []byte) {
		var payload map[string]interface{}
		if json.Unmarshal(data, &payload) == nil {
			msgs <- payload
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, observer.Flush())

	ts, owner, serverID := setupWithServer(t)
	sourceID := createGroup(t, ts, serverID, "route-canonical-source", owner.AccessToken)
	destinationID := createGroup(t, ts, serverID, "route-canonical-destination", owner.AccessToken)
	channelID := ts.CreateVoiceChannel(t, serverID, "route-canonical-channel")
	assignChannelToCategory(t, ts, channelID, sourceID, true)
	member := ts.CreateTestUser(t, "route-canonical-member")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	_, err = ts.DB.Exec(`
		INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny)
		VALUES (gen_random_uuid(), $1, 'user', $2, $3, 0),
		       (gen_random_uuid(), $4, 'user', $2, 0, $5)`,
		sourceID, member.ID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice), destinationID, int64(rbac.PermJoinVoice))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		SELECT gen_random_uuid(), $1, target_type, target_id, allow, deny
		FROM category_permission_overrides WHERE category_id = $2`, channelID, sourceID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, member.ID)
	require.NoError(t, err)

	w := ts.DoRequest("PATCH", pathChannelsPrefix+"{"+strings.ToUpper(channelID)+"}", map[string]interface{}{
		"name": "route-canonical-channel", "type": "voice", "group_id": destinationID,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	deadline := time.After(3 * time.Second)
	for {
		select {
		case payload := <-msgs:
			if payload["userId"] == member.ID {
				assert.Equal(t, channelID, payload["channelId"])
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for voice.enforce.permissions push")
		}
	}
}

// --- Additional Channel Group Tests for Coverage ---

func TestCreateChannelGroupAdminMember(t *testing.T) {
	ts, _, serverID := setupWithServer(t)
	admin := ts.CreateTestUser(t, "groupadmin1")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
		"name": "Admin Created Group",
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestCreateChannelGroupMemberWithPermission(t *testing.T) {
	ts, _, serverID := setupWithServer(t)
	member := ts.CreateTestUser(t, "grouppermmember")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// Grant PermManageChannels via custom role
	roleID := ts.CreateTestRole(t, serverID, "ChannelManager", 5, int64(rbac.PermManageChannels))
	ts.AssignRoleToUser(t, serverID, member.ID, roleID)

	w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
		"name": "Permission Created Group",
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestUpdateChannelGroupNameAndPosition(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "Original", user.AccessToken)

	newName := "Renamed"
	newPos := 10
	w := ts.DoRequest("PATCH", groupPath(serverID, groupID), map[string]interface{}{
		"name":     newName,
		"position": newPos,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	group := body["channel_group"].(map[string]interface{})
	assert.Equal(t, newName, group["name"])
	assert.Equal(t, float64(newPos), group["position"])
}

func TestUpdateChannelGroupInvalidBody(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "NeedUpdate", user.AccessToken)

	// Send invalid JSON (wrong type for name)
	w := ts.DoRequest("PATCH", groupPath(serverID, groupID), "not-json", testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteChannelGroupAdminMember(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "AdminDelete", user.AccessToken)

	admin := ts.CreateTestUser(t, "groupdeladmin")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("DELETE", groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Reorder Channels Additional Tests ---

func TestReorderChannelsMultiple(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "Reorder Group", user.AccessToken)
	ch1 := ts.CreateTestChannel(t, serverID, "chan-a")
	ch2 := ts.CreateTestChannel(t, serverID, "chan-b")
	ch3 := ts.CreateTestChannel(t, serverID, "chan-c")

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
		"channels": []map[string]interface{}{
			{"channel_id": ch1, "group_id": groupID, "position": 2},
			{"channel_id": ch2, "group_id": groupID, "position": 0},
			{"channel_id": ch3, "group_id": groupID, "position": 1},
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify positions were updated
	var pos1, pos2, pos3 int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM channels WHERE id = $1`, ch1).Scan(&pos1))
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM channels WHERE id = $1`, ch2).Scan(&pos2))
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM channels WHERE id = $1`, ch3).Scan(&pos3))
	assert.Equal(t, 2, pos1)
	assert.Equal(t, 0, pos2)
	assert.Equal(t, 1, pos3)
}

func TestReorderChannelsNilGroupID(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	ch := ts.CreateTestChannel(t, serverID, "ungrouped-chan")

	// Set group_id to nil (uncategorized)
	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
		"channels": []map[string]interface{}{
			{"channel_id": ch, "group_id": nil, "position": 0},
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReorderChannelsAdminMember(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	ch := ts.CreateTestChannel(t, serverID, "admin-reorder-chan")

	admin := ts.CreateTestUser(t, "reorderadmin")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
		"channels": []map[string]interface{}{
			{"channel_id": ch, "group_id": nil, "position": 5},
		},
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	_ = user // owner created server but admin performs the reorder
}

func TestListChannelGroupsMultipleOrdered(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	// Create groups in order
	g1 := createGroup(t, ts, serverID, "Alpha", user.AccessToken)
	g2 := createGroup(t, ts, serverID, "Bravo", user.AccessToken)
	g3 := createGroup(t, ts, serverID, "Charlie", user.AccessToken)

	// Reorder: Charlie first, Alpha second, Bravo third
	_, err := ts.DB.Exec(`UPDATE channel_groups SET position = 0 WHERE id = $1`, g3)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE channel_groups SET position = 1 WHERE id = $1`, g1)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE channel_groups SET position = 2 WHERE id = $1`, g2)
	require.NoError(t, err)

	w := ts.DoRequest("GET", groupsPath(serverID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	groups := body["channel_groups"].([]interface{})
	require.Len(t, groups, 3)

	assert.Equal(t, "Charlie", groups[0].(map[string]interface{})["name"])
	assert.Equal(t, "Alpha", groups[1].(map[string]interface{})["name"])
	assert.Equal(t, "Bravo", groups[2].(map[string]interface{})["name"])
}

func TestCreateChannelGroupNameTooLong(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	// Max is 100 characters per validation
	longName := ""
	for i := 0; i < 101; i++ {
		longName += "x"
	}

	w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
		"name": longName,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateChannelGroupAdminWithPermission(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "AdminUpdateTarget", user.AccessToken)

	member := ts.CreateTestUser(t, "updperm")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// Grant PermManageChannels
	roleID := ts.CreateTestRole(t, serverID, "ChanManager", 5, int64(rbac.PermManageChannels))
	ts.AssignRoleToUser(t, serverID, member.ID, roleID)

	newName := "UpdatedByPerm"
	w := ts.DoRequest("PATCH", groupPath(serverID, groupID), map[string]interface{}{
		"name": newName,
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteChannelGroupMemberWithPermission(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "PermDelete", user.AccessToken)

	member := ts.CreateTestUser(t, "delperm")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	roleID := ts.CreateTestRole(t, serverID, "ChanDeleter", 5, int64(rbac.PermManageChannels))
	ts.AssignRoleToUser(t, serverID, member.ID, roleID)

	w := ts.DoRequest("DELETE", groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}
