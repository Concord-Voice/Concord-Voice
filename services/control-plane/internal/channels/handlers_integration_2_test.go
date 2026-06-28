package channels_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/e2eekeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathChannelsSuffix   = "/channels"
	pathChannelGroupsSfx = "/channel-groups"
	notAUUID             = "not-a-uuid"
	zeroUUID             = "00000000-0000-0000-0000-000000000000"
	pathRotateKey        = "/rotate-key"
	pathValidateEpochs   = "/api/v1/e2ee/validate-epochs"
	pathE2EEKeys         = "/api/v1/e2ee/keys/"
	pathE2EEKeysZero     = pathE2EEKeys + zeroUUID
)

// ===========================================================================
// List Channels — edge cases
// ===========================================================================

func TestListChannelsEdgeCases(t *testing.T) {
	ts, user, serverID, _ := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "lcedgeouter")

	t.Run("InvalidServerID", func(t *testing.T) {
		w := ts.DoRequest("GET", pathServersPrefix+notAUUID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", pathServersPrefix+serverID+pathChannelsSuffix, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("NotMemberOtherUser", func(t *testing.T) {
		w := ts.DoRequest("GET", pathServersPrefix+serverID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("IncludesGroupsAndChannels", func(t *testing.T) {
		// Create a channel group
		w := ts.DoRequest("POST", pathServersPrefix+serverID+pathChannelGroupsSfx, map[string]interface{}{
			"name": "LC Text Channels",
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)

		// List channels — should include both channels and channel_groups
		w = ts.DoRequest("GET", pathServersPrefix+serverID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.NotNil(t, body["channels"])
		assert.NotNil(t, body["channel_groups"])
		groups := body["channel_groups"].([]interface{})
		assert.GreaterOrEqual(t, len(groups), 1)
	})
}

// ===========================================================================
// Create Channel — edge cases
// ===========================================================================

func TestCreateChannelEdgeCases(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "ccedge")
	serverID := ts.CreateTestServer(t, user.ID, "CC Edge Server")

	t.Run("BadRequestBody", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("InvalidType", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "bad-type-channel",
			"type":      "invalid",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("EncryptedWithoutKeys", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "enc-no-keys",
			"type":      "text",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NameTooShort", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "ab", // min=3
			"type":      "text",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "noauth-channel",
			"type":      "text",
		}, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("VoiceCreatesLinkedText", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "voice-room",
			"type":      "voice",
			keyWrappedKeys: map[string]string{
				user.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "voice", channel["type"])

		assert.NotNil(t, body["linked_text_channel"])
		ltc := body["linked_text_channel"].(map[string]interface{})
		assert.Equal(t, "text", ltc["type"])
		assert.Equal(t, "voice-room", ltc["name"])
	})

	t.Run("WithGroupID", func(t *testing.T) {
		// Create a channel group first
		w := ts.DoRequest("POST", pathServersPrefix+serverID+pathChannelGroupsSfx, map[string]interface{}{
			"name": "CC Group",
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)
		var groupBody map[string]interface{}
		testhelpers.ParseJSON(t, w, &groupBody)
		groupID := groupBody["channel_group"].(map[string]interface{})["id"].(string)

		w = ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "grouped-channel",
			"type":      "text",
			"group_id":  groupID,
			keyWrappedKeys: map[string]string{
				user.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, groupID, channel["group_id"])
	})

	t.Run("BulletinType", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "announcements",
			"type":      "bulletin",
			keyWrappedKeys: map[string]string{
				user.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "bulletin", channel["type"])
	})

	t.Run("WithEmoji", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "emoji-channel",
			"type":      "text",
			"emoji":     "rocket",
			keyWrappedKeys: map[string]string{
				user.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "rocket", channel["emoji"])
	})
}

func TestCreateChannelPermissions(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ccpermown")
	admin := ts.CreateTestUser(t, "ccpermadm")
	member := ts.CreateTestUser(t, "ccpermmem")
	serverID := ts.CreateTestServer(t, owner.ID, "CC Perm Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	t.Run("AdminCanCreate", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "admin-channel",
			"type":      "text",
			keyWrappedKeys: map[string]string{
				admin.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(admin.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("MemberCannotCreate", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "blocked-channel",
			"type":      "text",
		}, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// ===========================================================================
// Get Channel — edge cases
// ===========================================================================

func TestGetChannelEdgeCases(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "gcedgeouter")

	t.Run("InvalidID", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+notAUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+zeroUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("NotMember", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID, nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code) // combined "not found or access denied"
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// ===========================================================================
// Update Channel — edge cases
// ===========================================================================

func TestUpdateChannelEdgeCases(t *testing.T) {
	ts, user, serverID, channelID := setupWithChannel(t)
	member := ts.CreateTestUser(t, "ucedgemem")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	t.Run("InvalidID", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+notAUUID, map[string]interface{}{
			"name": "updated",
			"type": "text",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("BadRequestBody", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+zeroUUID, map[string]interface{}{
			"name": "ghost",
			"type": "text",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("NotAdmin", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
			"name": "hacked",
			"type": "text",
		}, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("InvalidAudioQualityTier", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
			"name":               "updated",
			"type":               "text",
			"audio_quality_tier": "ultra-mega",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("ValidAudioQualityTier", func(t *testing.T) {
		// Create voice channel
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "voice-aqt",
			"type":      "voice",
			keyWrappedKeys: map[string]string{
				user.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)
		var createBody map[string]interface{}
		testhelpers.ParseJSON(t, w, &createBody)
		voiceChID := createBody[keyChannel].(map[string]interface{})["id"].(string)

		// "standard" is the ceiling on a Groundspeed server (#179); "hifi" is
		// above the ceiling and now correctly rejected. Use "standard" here.
		w = ts.DoRequest("PATCH", pathChannelsPrefix+voiceChID, map[string]interface{}{
			"name":               "voice-aqt",
			"type":               "voice",
			"audio_quality_tier": "standard",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "standard", channel["audio_quality_tier"])
	})

	t.Run("WithGroupID", func(t *testing.T) {
		// Create a group
		w := ts.DoRequest("POST", pathServersPrefix+serverID+pathChannelGroupsSfx, map[string]interface{}{
			"name": "UC Target Group",
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)
		var groupBody map[string]interface{}
		testhelpers.ParseJSON(t, w, &groupBody)
		groupID := groupBody["channel_group"].(map[string]interface{})["id"].(string)

		w = ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
			"name":     "movable-chan",
			"type":     "text",
			"group_id": groupID,
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, groupID, channel["group_id"])
	})

	t.Run("ClearGroupID", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
			"name":     "grouped-chan",
			"type":     "text",
			"group_id": "",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Nil(t, channel["group_id"])
	})

	t.Run("WithEmoji", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
			"name":  "emoji-updated",
			"type":  "text",
			"emoji": "fire",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "fire", channel["emoji"])
	})

	t.Run("ChangeType", func(t *testing.T) {
		newCh := ts.CreateTestChannel(t, serverID, "type-change-ch")
		w := ts.DoRequest("PATCH", pathChannelsPrefix+newCh, map[string]interface{}{
			"name": "now-voice",
			"type": "voice",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "voice", channel["type"])
	})
}

func TestUpdateChannelAsAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ucadmown")
	admin := ts.CreateTestUser(t, "ucadm")
	serverID := ts.CreateTestServer(t, owner.ID, "Admin Update Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	channelID := ts.CreateTestChannel(t, serverID, "admin-update-target")

	w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
		"name": "admin-updated",
		"type": "text",
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ===========================================================================
// Delete Channel — edge cases
// ===========================================================================

func TestDeleteChannelEdgeCases(t *testing.T) {
	ts, user, serverID, channelID := setupWithChannel(t)

	t.Run("InvalidID", func(t *testing.T) {
		w := ts.DoRequest("DELETE", pathChannelsPrefix+notAUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("DELETE", pathChannelsPrefix+zeroUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("DELETE", pathChannelsPrefix+channelID, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("ThenGetReturnsNotFound", func(t *testing.T) {
		ch := ts.CreateTestChannel(t, serverID, "deleteme")
		w := ts.DoRequest("DELETE", pathChannelsPrefix+ch, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		w = ts.DoRequest("GET", pathChannelsPrefix+ch, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestDeleteChannelAsAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dcadmown")
	admin := ts.CreateTestUser(t, "dcadm")
	serverID := ts.CreateTestServer(t, owner.ID, "Admin Delete Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	channelID := ts.CreateTestChannel(t, serverID, "admin-delete-target")

	w := ts.DoRequest("DELETE", pathChannelsPrefix+channelID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ===========================================================================
// Mark Channel Read — edge cases
// ===========================================================================

func TestMarkChannelReadEdgeCases(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "mcredgeouter")

	t.Run("InvalidID", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+"not-a-uuid/read", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotMember", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+"/read", nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("Idempotent", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+"/read", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		w = ts.DoRequest("POST", pathChannelsPrefix+channelID+"/read", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

// ===========================================================================
// Mark Server Read — edge cases
// ===========================================================================

func TestMarkServerReadEdgeCases(t *testing.T) {
	ts, _, serverID, _ := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "msredgeouter")

	t.Run("InvalidID", func(t *testing.T) {
		w := ts.DoRequest("POST", pathServersPrefix+"not-a-uuid/read", nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotMember", func(t *testing.T) {
		w := ts.DoRequest("POST", pathServersPrefix+serverID+"/read", nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// ===========================================================================
// Get Unread Counts — edge cases
// ===========================================================================

func TestGetUnreadCountsEdgeCases(t *testing.T) {
	ts, user, serverID, channelID := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "ucedgeouter")

	t.Run("InvalidServerID", func(t *testing.T) {
		w := ts.DoRequest("GET", pathServersPrefix+"not-a-uuid/unread", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotMember", func(t *testing.T) {
		w := ts.DoRequest("GET", pathServersPrefix+serverID+"/unread", nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("ReturnsZeroAfterMarkRead", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+"/read", nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusOK, w.Code)

		w = ts.DoRequest("GET", pathServersPrefix+serverID+"/unread", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		unreads := body["unreads"].([]interface{})
		for _, u := range unreads {
			entry := u.(map[string]interface{})
			if entry["channel_id"] == channelID {
				assert.Equal(t, float64(0), entry["unread_count"])
			}
		}
	})
}

// ===========================================================================
// Get Server Unread Status — edge cases
// ===========================================================================

func TestGetServerUnreadStatusEdgeCases(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "susedge")

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", "/api/v1/servers/unread-status", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("EmptyServers", func(t *testing.T) {
		w := ts.DoRequest("GET", "/api/v1/servers/unread-status", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		serverIDs := body["server_ids"].([]interface{})
		assert.Empty(t, serverIDs)
	})
}

// ===========================================================================
// Distribute Channel Keys — edge cases
// ===========================================================================

func TestDistributeChannelKeysEdgeCases(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)

	t.Run("InvalidChannelID", func(t *testing.T) {
		w := ts.DoRequest("POST", "/api/v1/channels/not-a-uuid/keys", map[string]interface{}{
			keyWrappedKeys: map[string]string{
				owner.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("BadBody", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NoOwnKey", func(t *testing.T) {
		// New member has no key yet — cannot distribute
		newMember := ts.CreateTestUser(t, "dknoown")
		ts.AddMemberToServer(t, serverID, newMember.ID, roleMember)

		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
			keyWrappedKeys: map[string]string{
				owner.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(newMember.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("WithExplicitVersion", func(t *testing.T) {
		newMember := ts.CreateTestUser(t, "dkexpver")
		ts.AddMemberToServer(t, serverID, newMember.ID, roleMember)

		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
			keyWrappedKeys: map[string]string{
				newMember.ID: testhelpers.ValidCiphertext(),
			},
			"key_version": 1,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, float64(1), body["distributed"])
	})
}

// ===========================================================================
// Get Channel Keys — edge cases
// ===========================================================================

func TestGetChannelKeysEdgeCases(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("WithVersionParam", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID+"/keys?version=1", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		key := body["key"].(map[string]interface{})
		assert.Equal(t, float64(1), key["key_version"])
	})

	t.Run("InvalidVersionParam", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID+"/keys?version=abc", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NonExistentVersion", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID+"/keys?version=999", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

// ===========================================================================
// Rotate Key — all paths
// ===========================================================================

func TestRotateKeyAllPaths(t *testing.T) {
	ts, user, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "rkmember")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	t.Run("Success", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Contains(t, body, "new_key_version")
		assert.Equal(t, float64(2), body["new_key_version"])
	})

	t.Run("InvalidChannelID", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+notAUUID+pathRotateKey, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+zeroUUID+pathRotateKey, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("NotAdmin", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// ===========================================================================
// Validate Epochs — all paths
// ===========================================================================

func TestValidateEpochsAllPaths(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)
	outsider := ts.CreateTestUser(t, "epochsouter")

	t.Run("Success", func(t *testing.T) {
		w := ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{
			"epochs": map[string]int{
				channelID: 1,
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		revocations := body["revocations"].([]interface{})
		assert.Empty(t, revocations, "no revocations should exist for a fresh channel")
	})

	t.Run("BadBody", func(t *testing.T) {
		w := ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{
			"epochs": map[string]int{},
		}, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("InvalidChannelIDSkipped", func(t *testing.T) {
		w := ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{
			"epochs": map[string]int{
				notAUUID: 1,
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		revocations := body["revocations"].([]interface{})
		assert.Empty(t, revocations)
	})

	t.Run("NoAccessChannelSkipped", func(t *testing.T) {
		w := ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{
			"epochs": map[string]int{
				channelID: 1,
			},
		}, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		revocations := body["revocations"].([]interface{})
		assert.Empty(t, revocations)
	})
}

// ===========================================================================
// Get Unified Keys — all paths
// ===========================================================================

func TestGetUnifiedKeysAllPaths(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	t.Run("ChannelSuccess", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "channel", body["kind"])
		assert.NotNil(t, body["key"])
	})

	t.Run("InvalidContextID", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+notAUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeysZero, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("ChannelWithVersion", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID+"?version=1", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "channel", body["kind"])
		key := body["key"].(map[string]interface{})
		assert.Equal(t, float64(1), key["key_version"])
	})

	t.Run("ChannelInvalidVersion", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID+"?version=abc", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("ChannelNonExistentVersion", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID+"?version=999", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeysZero, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("EnvelopeShape_InvalidContextID", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+notAUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)

		rawBytes := w.Body.Bytes()
		var body e2eekeys.ErrorResponse
		require.NoError(t, json.Unmarshal(rawBytes, &body))
		assert.Equal(t, e2eekeys.CodeInvalidRequest, body.Code)
		assert.Equal(t, e2eekeys.KindUnknown, body.Kind)
		// Wire-format: omitempty drops the pending key from JSON on INVALID_REQUEST.
		var raw map[string]interface{}
		require.NoError(t, json.Unmarshal(rawBytes, &raw))
		_, hasPending := raw["pending"]
		assert.False(t, hasPending, "pending key must be absent from JSON on INVALID_REQUEST (bad context id)")
	})

	t.Run("EnvelopeShape_InvalidVersion", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID+"?version=abc", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)

		rawBytes := w.Body.Bytes()
		var body e2eekeys.ErrorResponse
		require.NoError(t, json.Unmarshal(rawBytes, &body))
		assert.Equal(t, e2eekeys.CodeInvalidRequest, body.Code)
		assert.Equal(t, e2eekeys.KindChannel, body.Kind)
		// Wire-format: omitempty drops the pending key from JSON on INVALID_REQUEST.
		var raw map[string]interface{}
		require.NoError(t, json.Unmarshal(rawBytes, &raw))
		_, hasPending := raw["pending"]
		assert.False(t, hasPending, "pending key must be absent from JSON on INVALID_REQUEST (bad version)")
	})

	t.Run("EnvelopeShape_Channel_NoKeyYet", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID+"?version=999", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)

		var body e2eekeys.ErrorResponse
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, e2eekeys.CodeNoKeyYet, body.Code)
		assert.Equal(t, e2eekeys.KindChannel, body.Kind)
		assert.True(t, body.Pending)
	})

	t.Run("EnvelopeShape_NotMember", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeysZero, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)

		rawBytes := w.Body.Bytes()
		var body e2eekeys.ErrorResponse
		require.NoError(t, json.Unmarshal(rawBytes, &body))
		assert.Equal(t, e2eekeys.CodeNotMember, body.Code)
		assert.Equal(t, e2eekeys.KindUnknown, body.Kind)
		assert.False(t, body.Pending)
		// Wire-format: omitempty drops the pending key from JSON on NOT_MEMBER.
		var raw map[string]interface{}
		require.NoError(t, json.Unmarshal(rawBytes, &raw))
		_, hasPending := raw["pending"]
		assert.False(t, hasPending, "pending key must be absent from JSON on NOT_MEMBER (channel-side)")
	})

	t.Run("EnvelopeShape_Channel_HappyPath", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body e2eekeys.KeyResponse
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, e2eekeys.KindChannel, body.Kind)
		assert.NotEmpty(t, body.Key.WrappedKey)
		assert.GreaterOrEqual(t, body.Key.KeyVersion, 1)
	})
}

func TestGetUnifiedKeysDM(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "ukdm1")
	user2 := ts.CreateTestUser(t, "ukdm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")

	t.Run("DMSuccess", func(t *testing.T) {
		convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
		ts.SeedDMKey(t, convID, user1.ID, 1)

		w := ts.DoRequest("GET", pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "dm", body["kind"])
		assert.NotNil(t, body["key"])
	})

	t.Run("DMNoKey", func(t *testing.T) {
		convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

		w := ts.DoRequest("GET", pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, true, body["pending"])
	})

	t.Run("EnvelopeShape_DM_NoKeyYet", func(t *testing.T) {
		convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
		// No SeedDMKey — no key row exists

		w := ts.DoRequest("GET", pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)

		var body e2eekeys.ErrorResponse
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, e2eekeys.CodeNoKeyYet, body.Code)
		assert.Equal(t, e2eekeys.KindDM, body.Kind)
		assert.True(t, body.Pending)
	})

	t.Run("EnvelopeShape_DM_NotMember", func(t *testing.T) {
		user3 := ts.CreateTestUser(t, "ukdm_outsider")
		convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
		ts.SeedDMKey(t, convID, user1.ID, 1)

		w := ts.DoRequest("GET", pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user3.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)

		rawBytes := w.Body.Bytes()
		var body e2eekeys.ErrorResponse
		require.NoError(t, json.Unmarshal(rawBytes, &body))
		assert.Equal(t, e2eekeys.CodeNotMember, body.Code)
		assert.Equal(t, e2eekeys.KindUnknown, body.Kind)
		assert.False(t, body.Pending)
		// Wire-format: omitempty drops the pending key from JSON on NOT_MEMBER.
		var raw map[string]interface{}
		require.NoError(t, json.Unmarshal(rawBytes, &raw))
		_, hasPending := raw["pending"]
		assert.False(t, hasPending, "pending key must be absent from JSON on NOT_MEMBER (DM-side)")
	})

	t.Run("EnvelopeShape_DM_RevokedEpoch", func(t *testing.T) {
		convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
		ts.SeedDMKey(t, convID, user1.ID, 3)    // caller's current epoch = 3
		ts.SeedDMKeyRevocation(t, convID, 3, 4) // epoch 3 is revoked, successor is 4

		w := ts.DoRequest("GET", pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)

		// Capture raw JSON first (testhelpers.ParseJSON consumes w.Body via
		// json.NewDecoder, so subsequent reads would be empty).
		// Wire-format assertion: omitempty must drop the "pending" key from JSON,
		// not emit "pending":false. Client parsers rely on key absence as a signal.
		rawBytes := w.Body.Bytes()
		var raw map[string]interface{}
		require.NoError(t, json.Unmarshal(rawBytes, &raw))
		_, hasPending := raw["pending"]
		assert.False(t, hasPending, "pending key must be absent from JSON on REVOKED_EPOCH, not false")

		var body e2eekeys.ErrorResponse
		require.NoError(t, json.Unmarshal(rawBytes, &body))
		assert.Equal(t, e2eekeys.CodeRevokedEpoch, body.Code)
		assert.Equal(t, e2eekeys.KindDM, body.Kind)
		assert.False(t, body.Pending, "revoked epoch must not carry pending:true")
	})

	t.Run("EnvelopeShape_DM_HappyPath", func(t *testing.T) {
		convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
		ts.SeedDMKey(t, convID, user1.ID, 1)

		w := ts.DoRequest("GET", pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body e2eekeys.KeyResponse
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, e2eekeys.KindDM, body.Kind)
		assert.NotEmpty(t, body.Key.WrappedKey)
	})
}

// renameTableStmts maps each table name this test is allowed to rename to its
// pre-built ALTER TABLE statements. Using a fixed allowlist avoids dynamic
// string concatenation in SQL — `withRenamedTable` picks statements by map
// lookup, so no caller-supplied string reaches the query builder.
var renameTableStmts = map[string]struct {
	rename string
	revert string
}{
	"channels": {
		rename: `ALTER TABLE channels RENAME TO channels_hidden_for_test`,
		revert: `ALTER TABLE channels_hidden_for_test RENAME TO channels`,
	},
	"channel_keys": {
		rename: `ALTER TABLE channel_keys RENAME TO channel_keys_hidden_for_test`,
		revert: `ALTER TABLE channel_keys_hidden_for_test RENAME TO channel_keys`,
	},
	"dm_conversations": {
		rename: `ALTER TABLE dm_conversations RENAME TO dm_conversations_hidden_for_test`,
		revert: `ALTER TABLE dm_conversations_hidden_for_test RENAME TO dm_conversations`,
	},
	"dm_channel_keys": {
		rename: `ALTER TABLE dm_channel_keys RENAME TO dm_channel_keys_hidden_for_test`,
		revert: `ALTER TABLE dm_channel_keys_hidden_for_test RENAME TO dm_channel_keys`,
	},
	"dm_key_revocations": {
		rename: `ALTER TABLE dm_key_revocations RENAME TO dm_key_revocations_hidden_for_test`,
		revert: `ALTER TABLE dm_key_revocations_hidden_for_test RENAME TO dm_key_revocations`,
	},
}

// withRenamedTable temporarily renames a PostgreSQL table so the handler's
// query against it fails with a relation-not-found error, exercising a
// non-ErrNoRows failure path. The rename is reverted via defer so the test
// teardown (which may call TruncateAllTables) still finds the table.
//
// The `table` argument selects a pair of pre-built statements from
// renameTableStmts — no string concatenation hits the query builder, so a
// future caller cannot introduce SQL injection by passing user-controlled data.
//
// Pattern reference: internal/privacy/handler_integration_test.go
// #TestEraseAccount_DbFails_500 and the existing
// TestGetUnifiedKeys_DM_DBError_* tests in this file.
func withRenamedTable(t *testing.T, ts *testhelpers.TestServer, table string, fn func()) {
	t.Helper()
	stmts, ok := renameTableStmts[table]
	require.True(t, ok, "withRenamedTable: %q not in renameTableStmts allowlist", table)
	_, err := ts.DB.Exec(stmts.rename)
	require.NoError(t, err, "rename %s", table)
	defer func() {
		if _, revertErr := ts.DB.Exec(stmts.revert); revertErr != nil {
			t.Logf("testhelpers: failed to revert %s rename: %v", table, revertErr)
		}
	}()
	fn()
}

// TestGetUnifiedKeys_NoWrapKeyInLogs drives every logged branch of
// GetUnifiedKeys and asserts no wrapped-key bytes, long base64 blobs, or the
// literal field name "wrapped_key" appear in captured structured logs.
// Regression guard per [internal]rules/e2ee.md: key material is never logged at
// any level. Spec reference: [internal]specs for #751, section 10.
//
// Covered branches (8 total):
//  1. Happy path (200)
//  2. no_channel_key_row (version=999)
//  3. context_not_found_or_forbidden (zero UUID)
//  4. channel_check_db_error (rename channels)
//  5. channel_key_fetch_db_error (rename channel_keys)
//  6. dm_check_db_error (rename dm_conversations)
//  7. dm_key_fetch_db_error (rename dm_channel_keys, DM pre-seeded)
//  8. dm_revocation_check_db_error (rename dm_key_revocations, DM+key seeded)
func TestGetUnifiedKeys_NoWrapKeyInLogs(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	// Seed a DISTINCT, known wrapped-key sentinel into the DB so Check 3 can
	// assert the exact bytes never surface in log output. We use a long
	// recognizable string that would stand out in any log line — if a future
	// refactor accidentally logs `key.WrappedKey`, the substring check below
	// will fail immediately and identify this test as the reason.
	sentinelWrappedKey := []byte("SENTINEL-WRAPPED-KEY-THIS-MUST-NEVER-APPEAR-IN-LOGS-XYZZY-001")
	sentinelB64 := base64.StdEncoding.EncodeToString(sentinelWrappedKey)
	// channel_keys.wrapped_key is a TEXT column (base64-encoded wrap bytes).
	// Seed with the base64 string, not the raw []byte — passing []byte to a
	// TEXT column causes the driver to hex-encode it, which would leave
	// neither the raw string nor the base64 form in the row and silently
	// defeat the log-leak regex assertions below.
	_, err := ts.DB.Exec(
		`UPDATE channel_keys SET wrapped_key = $1 WHERE channel_id = $2 AND user_id = $3`,
		sentinelB64, channelID, user.ID,
	)
	require.NoError(t, err, "seed sentinel wrapped key")

	// Seed DM fixtures used by the dm_key_fetch_db_error and
	// dm_revocation_check_db_error branches. The DM key is seeded with a
	// distinct sentinel so branch 7 / 8 can't accidentally leak it either.
	partner := ts.CreateTestUser(t, "logwrapdm")
	ts.CreateFriendship(t, user.ID, partner.ID, "accepted")
	dmConvID := ts.CreateDMConversation(t, user.ID, partner.ID)
	dmSentinelWrappedKey := []byte("SENTINEL-DM-WRAPPED-KEY-MUST-NEVER-APPEAR-IN-LOGS-XYZZY-002")
	dmSentinelB64 := base64.StdEncoding.EncodeToString(dmSentinelWrappedKey)
	ts.SeedDMKey(t, dmConvID, user.ID, 1)
	_, err = ts.DB.Exec(
		`UPDATE dm_channel_keys SET wrapped_key = $1 WHERE conversation_id = $2 AND user_id = $3`,
		dmSentinelB64, dmConvID, user.ID,
	)
	require.NoError(t, err, "seed DM sentinel wrapped key")

	logs := ts.CaptureLogs(t)

	// 1. Happy path — 200; handler does NOT currently emit a 2xx log line, but
	//    we include it so any future success-log addition is covered.
	w := ts.DoRequest("GET", pathE2EEKeys+channelID, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "happy path returned %d: %s", w.Code, w.Body.String())

	// 2. no_channel_key_row — 404 with version=999 (channel exists, version doesn't)
	w = ts.DoRequest("GET", pathE2EEKeys+channelID+"?version=999", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusNotFound, w.Code, "version=999 returned %d", w.Code)

	// 3. context_not_found_or_forbidden — 404 on zero UUID (not in channels nor DMs)
	w = ts.DoRequest("GET", pathE2EEKeysZero, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusNotFound, w.Code, "zero UUID returned %d", w.Code)

	// 4. channel_check_db_error — rename channels so the initial channel INNER
	//    JOIN fails with a relation-not-found error (non-ErrNoRows).
	withRenamedTable(t, ts, "channels", func() {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID, nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusInternalServerError, w.Code,
			"channel_check_db_error returned %d: %s", w.Code, w.Body.String())
	})

	// 5. channel_key_fetch_db_error — channels table intact (so channel check
	//    succeeds) but channel_keys renamed so the follow-up fetch errors.
	withRenamedTable(t, ts, "channel_keys", func() {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID, nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusInternalServerError, w.Code,
			"channel_key_fetch_db_error returned %d: %s", w.Code, w.Body.String())
	})

	// 6. dm_check_db_error — use a fresh UUID (not in channels) so the handler
	//    falls through to getDMKeyResponse, then rename dm_conversations so the
	//    DM membership query errors with a non-ErrNoRows failure.
	freshCtxID := uuid.NewString()
	withRenamedTable(t, ts, "dm_conversations", func() {
		w := ts.DoRequest("GET", pathE2EEKeys+freshCtxID, nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusInternalServerError, w.Code,
			"dm_check_db_error returned %d: %s", w.Code, w.Body.String())
	})

	// 7. dm_key_fetch_db_error — DM exists + encrypted; rename dm_channel_keys
	//    so the key-fetch SELECT errors after the membership join succeeds.
	withRenamedTable(t, ts, "dm_channel_keys", func() {
		w := ts.DoRequest("GET", pathE2EEKeys+dmConvID, nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusInternalServerError, w.Code,
			"dm_key_fetch_db_error returned %d: %s", w.Code, w.Body.String())
	})

	// 8. dm_revocation_check_db_error — DM + key both present; only the
	//    dm_key_revocations EXISTS subquery fails.
	withRenamedTable(t, ts, "dm_key_revocations", func() {
		w := ts.DoRequest("GET", pathE2EEKeys+dmConvID, nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusInternalServerError, w.Code,
			"dm_revocation_check_db_error returned %d: %s", w.Code, w.Body.String())
	})

	captured := logs.String()

	// Check 1a: the literal field name "wrapped_key" must not appear in logs.
	// Guards against someone accidentally adding `"wrapped_key", key.WrappedKey`
	// to a log call — the string match catches it regardless of whether the
	// value marshals readably (e.g., `[]byte` or `string`).
	assert.NotContains(t, captured, "wrapped_key",
		"captured logs must not contain the wrapped_key field name per [internal]rules/e2ee.md")

	// Check 1b: also guard against the Go field-name form, which could appear
	// if someone logs the struct directly via `%+v` or similar.
	assert.NotContains(t, captured, "WrappedKey",
		"captured logs must not contain the WrappedKey Go field name either")

	// Check 3: the exact sentinel wrapped-key values — both raw string form and
	// base64 form — must not appear anywhere in logs. (Both channel and DM
	// sentinels are checked so a leak from either side is caught.)
	assert.NotContains(t, captured, string(sentinelWrappedKey),
		"channel sentinel wrapped-key (raw bytes) must not leak to logs")
	assert.NotContains(t, captured, sentinelB64,
		"channel sentinel wrapped-key (base64 form) must not leak to logs")
	assert.NotContains(t, captured, string(dmSentinelWrappedKey),
		"DM sentinel wrapped-key (raw bytes) must not leak to logs")
	assert.NotContains(t, captured, dmSentinelB64,
		"DM sentinel wrapped-key (base64 form) must not leak to logs")

	// Check 2: no long base64-like token appears in log output. A 512-byte
	// RSA-OAEP output base64s to ~684 chars; even a 32-byte AES raw key base64s
	// to ~44 chars. A contiguous run of >= 40 base64 chars in logs is highly
	// suspicious of a wrap-byte leak.
	re := regexp.MustCompile(`[A-Za-z0-9+/]{40,}={0,2}`)
	matches := re.FindAllString(captured, -1)

	// Build an explicit per-token allowlist of known-benign long base64-like
	// substrings. Only matches that are a substring of one of these allowlist
	// entries pass; anything else is flagged as a potential wrap-byte leak.
	// This is stricter than the prior inclusion check (which only guarded the
	// current user's JWT and would silently allow unrelated long base64 runs
	// that happened to share a prefix with the token).
	//
	// Entries:
	//   - both test users' full JWTs (header.payload.signature, emitted by
	//     middleware in request-logging breadcrumbs)
	//   - the JWT components split on '.' so runs of >=40 chars inside a
	//     single component still pass
	tokens := []string{user.AccessToken, partner.AccessToken}
	allowlist := make([]string, 0, len(tokens)*4)
	allowlist = append(allowlist, tokens...)
	for _, tok := range tokens {
		allowlist = append(allowlist, strings.Split(tok, ".")...)
	}

	var suspicious []string
	for _, m := range matches {
		allowed := false
		for _, entry := range allowlist {
			if strings.Contains(entry, m) {
				allowed = true
				break
			}
		}
		if !allowed {
			suspicious = append(suspicious, m)
		}
	}
	assert.Empty(t, suspicious,
		"no long base64-like tokens should appear in logs (potential wrap-byte leak): %v",
		suspicious)
}

// ===========================================================================
// getDMKeyResponse — DB-error branches
//
// The three 500-path branches below cannot be exercised by ordinary integration
// fixtures (no valid request can cause a non-ErrNoRows failure on a healthy
// Postgres) but they are load-bearing for log correlation: each branch emits a
// distinct `kind` value that ops uses to distinguish fault categories at the
// log sink. We simulate transient DB failure by temporarily renaming the
// relation each branch queries, following the precedent established in
// internal/privacy/handler_integration_test.go#TestEraseAccount_DbFails_500.
//
// Rename preserves attached indexes and constraints, so the deferred reverse
// rename fully restores schema state without re-running migrations. defer (not
// t.Cleanup) is used so the reverse rename runs BEFORE SetupTestServer's
// cleanup, which calls TruncateAllTables and would otherwise fail on a renamed
// table. Tests run sequentially (-p 1), so the only panic window is a
// connection-level failure, in which case the whole suite is already
// compromised.
// ===========================================================================

// TestGetUnifiedKeys_DM_DBError_Check covers the dm_check_db_error branch
// when the initial dm_conversations membership query fails with a non-
// ErrNoRows error (simulated by renaming dm_conversations out from under
// the handler). The channel check correctly returns ErrNoRows for a fresh
// UUID, so the handler falls through to getDMKeyResponse and then hits
// the forced failure on the DM membership SELECT.
func TestGetUnifiedKeys_DM_DBError_Check(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "dmdberr1")

	// Fresh UUID: not in channels table (so channel check returns ErrNoRows,
	// and the handler falls through to getDMKeyResponse per handlers.go line
	// 1261–1275). Also not in dm_conversations — but we're about to rename
	// that table, so the DM membership query will fail with a relation-not-
	// found error, not ErrNoRows.
	ctxID := uuid.NewString()

	_, err := ts.DB.Exec(`ALTER TABLE dm_conversations RENAME TO dm_conversations_hidden_for_test`)
	require.NoError(t, err)
	defer func() {
		if _, revertErr := ts.DB.Exec(`ALTER TABLE dm_conversations_hidden_for_test RENAME TO dm_conversations`); revertErr != nil {
			t.Logf("testhelpers: failed to revert dm_conversations rename: %v", revertErr)
		}
	}()

	w := ts.DoRequest("GET", pathE2EEKeys+ctxID, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())

	var body e2eekeys.ErrorResponse
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, e2eekeys.KindUnknown, body.Kind,
		"dm_check_db_error must envelope Kind=unknown, not leak DB state")
	// CodeInternalError on generic DB failure; client treats as retryable.
	assert.Equal(t, e2eekeys.CodeInternalError, body.Code)
	assert.False(t, body.Pending, "DB errors never set pending:true")
}

// TestGetUnifiedKeys_DM_DBError_KeyFetch covers the dm_key_fetch_db_error
// branch: the DM membership query succeeds (encrypted DM exists) but the
// subsequent dm_channel_keys SELECT fails. Simulated by creating the
// encrypted DM first, then renaming dm_channel_keys so the key-fetch
// query errors while the membership join continues to work.
func TestGetUnifiedKeys_DM_DBError_KeyFetch(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmdberr2a")
	user2 := ts.CreateTestUser(t, "dmdberr2b")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	_, err := ts.DB.Exec(`ALTER TABLE dm_channel_keys RENAME TO dm_channel_keys_hidden_for_test`)
	require.NoError(t, err)
	defer func() {
		if _, revertErr := ts.DB.Exec(`ALTER TABLE dm_channel_keys_hidden_for_test RENAME TO dm_channel_keys`); revertErr != nil {
			t.Logf("testhelpers: failed to revert dm_channel_keys rename: %v", revertErr)
		}
	}()

	w := ts.DoRequest("GET", pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())

	var body e2eekeys.ErrorResponse
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, e2eekeys.KindUnknown, body.Kind,
		"dm_key_fetch_db_error must envelope Kind=unknown")
	assert.Equal(t, e2eekeys.CodeInternalError, body.Code)
	assert.False(t, body.Pending)
}

// TestGetUnifiedKeys_DM_DBError_RevocationCheck covers the
// dm_revocation_check_db_error branch added in Task 5. The DM membership
// and DM key-fetch queries both succeed; only the dm_key_revocations
// EXISTS subquery fails. Simulated by seeding the DM + key first, then
// renaming dm_key_revocations so only the revocation check errors.
func TestGetUnifiedKeys_DM_DBError_RevocationCheck(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmdberr3a")
	user2 := ts.CreateTestUser(t, "dmdberr3b")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)

	_, err := ts.DB.Exec(`ALTER TABLE dm_key_revocations RENAME TO dm_key_revocations_hidden_for_test`)
	require.NoError(t, err)
	defer func() {
		if _, revertErr := ts.DB.Exec(`ALTER TABLE dm_key_revocations_hidden_for_test RENAME TO dm_key_revocations`); revertErr != nil {
			t.Logf("testhelpers: failed to revert dm_key_revocations rename: %v", revertErr)
		}
	}()

	w := ts.DoRequest("GET", pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())

	var body e2eekeys.ErrorResponse
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, e2eekeys.KindUnknown, body.Kind,
		"dm_revocation_check_db_error must envelope Kind=unknown")
	assert.Equal(t, e2eekeys.CodeInternalError, body.Code)
	assert.False(t, body.Pending)
}

// ===========================================================================
// Distribute Unified Keys — all paths
// ===========================================================================

func TestDistributeUnifiedKeysAllPaths(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	newMember := ts.CreateTestUser(t, "dukch")
	ts.AddMemberToServer(t, serverID, newMember.ID, roleMember)

	t.Run("ChannelSuccess", func(t *testing.T) {
		w := ts.DoRequest("POST", pathE2EEKeys+channelID, map[string]interface{}{
			keyWrappedKeys: map[string]string{
				newMember.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, float64(1), body["distributed"])
	})

	t.Run("InvalidContextID", func(t *testing.T) {
		w := ts.DoRequest("POST", pathE2EEKeys+notAUUID, map[string]interface{}{
			keyWrappedKeys: map[string]string{
				owner.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("POST", pathE2EEKeysZero, map[string]interface{}{
			keyWrappedKeys: map[string]string{
				owner.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestDistributeUnifiedKeysDM(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dukdm1")
	user2 := ts.CreateTestUser(t, "dukdm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathE2EEKeys+convID, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			user2.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "dm", body["context_type"])
	assert.Equal(t, float64(1), body["distributed"])
}

// queryDMKeyVersion returns the key_version of the row in dm_channel_keys for
// a given (conversation, user). Fails the test if no row exists.
func queryDMKeyVersion(t *testing.T, ts *testhelpers.TestServer, conversationID, userID string) int {
	t.Helper()
	var version int
	err := ts.DB.QueryRow(
		`SELECT key_version FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, userID,
	).Scan(&version)
	require.NoError(t, err, "expected dm_channel_keys row for (%s, %s)", conversationID, userID)
	return version
}

// TestDistributeUnifiedKeysDM_NewConversationStartsAtVersion1 verifies that the
// first peer-fulfilled wrap for a brand-new DM conversation lands at
// key_version=1. This is the baseline behavior pre- and post-fix.
func TestDistributeUnifiedKeysDM_NewConversationStartsAtVersion1(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmnewv1a")
	user2 := ts.CreateTestUser(t, "dmnewv1b")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathE2EEKeys+convID, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			user2.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, 1, queryDMKeyVersion(t, ts, convID, user2.ID),
		"first key in a new DM conversation should be version 1")
}

// TestDistributeUnifiedKeysDM_PeerFulfillmentPreservesVersion is the regression
// test for the bug fixed in PR #1080: when a peer wraps the current CSK for
// a recovering or newly-joining user, the inserted row MUST be tagged at the
// EXISTING key_version, not MAX+1. Otherwise the recovering user receives a
// wrap at a version no historical message references, breaking history
// decryption.
func TestDistributeUnifiedKeysDM_PeerFulfillmentPreservesVersion(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmpfa")
	user2 := ts.CreateTestUser(t, "dmpfb")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Seed user1 at version 1 (simulates established participant).
	ts.SeedDMKey(t, convID, user1.ID, 1)

	// user1 peer-fulfills the CSK wrap for user2 (the recovering/onboarding
	// participant). The client did NOT pass an explicit key_version in the
	// body; the server must default to the EXISTING version (1), NOT MAX+1
	// (which would be 2).
	w := ts.DoRequest("POST", pathE2EEKeys+convID, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			user2.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, 1, queryDMKeyVersion(t, ts, convID, user2.ID),
		"peer-fulfilled wrap must preserve the existing key_version (1), not stamp MAX+1 (2)")
}

// TestDistributeUnifiedKeysDM_ExplicitVersionRotation verifies the rotation
// path: when the caller passes an explicit key_version (typically MAX+1 from
// the rotation broadcast), the server inserts at that version.
func TestDistributeUnifiedKeysDM_ExplicitVersionRotation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrotA")
	user2 := ts.CreateTestUser(t, "dmrotB")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Seed both participants at version 1.
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	// Rotation: client passes key_version: 2 explicitly.
	explicitVersion := 2
	w := ts.DoRequest("POST", pathE2EEKeys+convID, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			user1.ID: testhelpers.ValidCiphertext(),
			user2.ID: testhelpers.ValidCiphertext(),
		},
		"key_version": explicitVersion,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(2), body["distributed"])

	// Each user should have BOTH a version 1 and version 2 row.
	var v1Count, v2Count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_channel_keys WHERE conversation_id = $1 AND key_version = 1`,
		convID,
	).Scan(&v1Count))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_channel_keys WHERE conversation_id = $1 AND key_version = 2`,
		convID,
	).Scan(&v2Count))
	assert.Equal(t, 2, v1Count, "seeded version 1 rows should be intact")
	assert.Equal(t, 2, v2Count, "rotation inserts at explicit key_version=2")
}

// TestDistributeUnifiedKeysDM_RecoveryAfterRotation verifies the cross-product
// bug: a recovery happens AFTER a rotation. The conversation has rows at
// version 2 (current), and a user recovers their wrap. The peer wrap must
// land at version 2 (current MAX), not version 3 (MAX+1).
func TestDistributeUnifiedKeysDM_RecoveryAfterRotation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrecA")
	user2 := ts.CreateTestUser(t, "dmrecB")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Simulate post-rotation state: both at v1, user1 at v2 (user2 missing v2 wrap).
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)
	ts.SeedDMKey(t, convID, user1.ID, 2)

	// user1 peer-fulfills the recovery for user2 with no explicit version.
	// MAX(key_version) is 2; the server must insert user2's wrap at version 2
	// (the existing/current epoch), NOT at version 3.
	w := ts.DoRequest("POST", pathE2EEKeys+convID, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			user2.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// user2 now has both v1 and v2 wraps.
	var versions []int
	rows, err := ts.DB.Query(
		`SELECT key_version FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2 ORDER BY key_version`,
		convID, user2.ID,
	)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		versions = append(versions, v)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []int{1, 2}, versions,
		"recovery wrap must land at current MAX version (2), preserving access to history")

	// And there must NOT be a phantom version-3 row.
	var v3Count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_channel_keys WHERE conversation_id = $1 AND key_version = 3`,
		convID,
	).Scan(&v3Count))
	assert.Equal(t, 0, v3Count, "no phantom MAX+1 row should be created on peer fulfillment")
}

// ===========================================================================
// Get Pending Key Requests — edge cases
// ===========================================================================

func TestGetPendingKeyRequestsEdgeCases(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pkredge")

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("NoChannels", func(t *testing.T) {
		w := ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		requests := body["pending_requests"].([]interface{})
		assert.Empty(t, requests)
	})
}

// ===========================================================================
// Channel Encryption Flow — end-to-end
// ===========================================================================

func TestEncryptedChannelFullFlow(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "e2eeflowown")
	member := ts.CreateTestUser(t, "e2eeflowmem")
	serverID := ts.CreateTestServer(t, owner.ID, "E2EE Flow Server")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// 1. Owner creates encrypted channel with their own key
	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverID,
		"name":      "encrypted-flow",
		"type":      "text",
		keyWrappedKeys: map[string]string{
			owner.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	channelID := createBody[keyChannel].(map[string]interface{})["id"].(string)

	// 2. Owner fetches their key
	w = ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// 3. Member tries to fetch key — should get not found (pending)
	w = ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
	var pendingBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &pendingBody)
	assert.Equal(t, true, pendingBody["pending"])

	// 4. Owner distributes key to member
	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			member.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var distBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &distBody)
	assert.Equal(t, float64(1), distBody["distributed"])

	// 5. Member can now fetch key
	w = ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// 6. Owner rotates key
	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var rotateBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &rotateBody)
	assert.Equal(t, float64(2), rotateBody["new_key_version"])

	// 7. Validate epochs — no revocations (rotation is initiated but not sealed)
	w = ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{
		"epochs": map[string]int{
			channelID: 1,
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCreateEncryptedVoiceChannelWithLinkedText(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "encvoice")
	serverID := ts.CreateTestServer(t, user.ID, "Enc Voice Server")

	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverID,
		"name":      "encrypted-voice",
		"type":      "voice",
		keyWrappedKeys: map[string]string{
			user.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)

	channel := body[keyChannel].(map[string]interface{})
	assert.Equal(t, "voice", channel["type"])

	// Linked text channel exists
	assert.NotNil(t, body["linked_text_channel"])
	ltc := body["linked_text_channel"].(map[string]interface{})
	assert.Equal(t, "text", ltc["type"])
}

// ===========================================================================
// Reorder Channels — additional edge cases (supplements groups_test.go)
// ===========================================================================

func TestReorderChannelsAdditional(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	ch1 := ts.CreateTestChannel(t, serverID, "reorder-a")
	ch2 := ts.CreateTestChannel(t, serverID, "reorder-b")
	ch3 := ts.CreateTestChannel(t, serverID, "reorder-c")
	groupID := createGroup(t, ts, serverID, "Reorder Target", user.AccessToken)

	t.Run("MultipleChannels", func(t *testing.T) {
		w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
			"channels": []map[string]interface{}{
				{"channel_id": ch3, "position": 0},
				{"channel_id": ch1, "position": 1},
				{"channel_id": ch2, "position": 2},
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Verify order
		w = ts.DoRequest("GET", pathServersPrefix+serverID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channels := body["channels"].([]interface{})
		assert.GreaterOrEqual(t, len(channels), 3)

		first := channels[0].(map[string]interface{})
		assert.Equal(t, ch3, first["id"])
	})

	t.Run("MoveToGroup", func(t *testing.T) {
		w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
			"channels": []map[string]interface{}{
				{"channel_id": ch1, "group_id": groupID, "position": 0},
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Verify channel is now in the group
		w = ts.DoRequest("GET", pathChannelsPrefix+ch1, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, groupID, channel["group_id"])
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
			"channels": []map[string]interface{}{},
		}, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// ===========================================================================
// Channel Groups — additional edge cases (supplements groups_test.go)
// ===========================================================================

func TestChannelGroupsAdditional(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	admin := ts.CreateTestUser(t, "grpadm")
	member := ts.CreateTestUser(t, "grpmem")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	t.Run("NameTooLong", func(t *testing.T) {
		longName := ""
		for i := 0; i < 101; i++ {
			longName += "x"
		}
		w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
			"name": longName,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("AdminCanCreate", func(t *testing.T) {
		w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
			"name": "Admin Category",
		}, testhelpers.AuthHeaders(admin.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("MemberCanList", func(t *testing.T) {
		w := ts.DoRequest("GET", groupsPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		groups := body["channel_groups"].([]interface{})
		assert.GreaterOrEqual(t, len(groups), 1)
	})

	t.Run("DeleteGroupChannelsSurvive", func(t *testing.T) {
		groupID := createGroup(t, ts, serverID, "Doomed Group", owner.AccessToken)

		// Create a channel in this group
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "survivor-channel",
			"type":      "text",
			"group_id":  groupID,
			keyWrappedKeys: map[string]string{
				owner.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)
		var createBody map[string]interface{}
		testhelpers.ParseJSON(t, w, &createBody)
		channelID := createBody[keyChannel].(map[string]interface{})["id"].(string)

		// Delete the group
		w = ts.DoRequest("DELETE", groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Channel should still exist with group_id = NULL (ON DELETE SET NULL)
		w = ts.DoRequest("GET", pathChannelsPrefix+channelID, nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Nil(t, channel["group_id"], "channel's group_id should be null after group deletion")
	})
}

// ===========================================================================
// Multiple operations — scenario tests
// ===========================================================================

func TestMultipleChannelOperations(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "multiop")
	serverID := ts.CreateTestServer(t, user.ID, "Multi Op Server")

	t.Run("CreateMultiple", func(t *testing.T) {
		channelNames := []string{"general", "random", "announcements", "dev-talk", "off-topic"}
		for _, name := range channelNames {
			w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
				keyServerID: serverID,
				"name":      name,
				"type":      "text",
				keyWrappedKeys: map[string]string{
					user.ID: testhelpers.ValidCiphertext(),
				},
			}, testhelpers.AuthHeaders(user.AccessToken))
			assert.Equal(t, http.StatusCreated, w.Code)
		}

		// List all channels
		w := ts.DoRequest("GET", pathServersPrefix+serverID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channels := body["channels"].([]interface{})
		assert.Equal(t, len(channelNames), len(channels))
	})

	t.Run("DeleteAllThenListEmpty", func(t *testing.T) {
		// Create a new server to avoid conflicts with subtests above
		srvID := ts.CreateTestServer(t, user.ID, "Del All Server")
		ids := make([]string, 0, 2)
		for _, name := range []string{"chan-a", "chan-b"} {
			id := ts.CreateTestChannel(t, srvID, name)
			ids = append(ids, id)
		}

		for _, id := range ids {
			w := ts.DoRequest("DELETE", pathChannelsPrefix+id, nil, testhelpers.AuthHeaders(user.AccessToken))
			assert.Equal(t, http.StatusOK, w.Code)
		}

		w := ts.DoRequest("GET", pathServersPrefix+srvID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channels := body["channels"].([]interface{})
		assert.Empty(t, channels)
	})
}

// ===========================================================================
// Concurrent contention — rewrap idempotency (#1023)
// ===========================================================================

func TestRequestRewrapConcurrentSameUserSameContext(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	const N = 8
	var wg sync.WaitGroup
	statuses := make([]int, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+channelID+"/rewrap", nil,
				testhelpers.AuthHeaders(user.AccessToken))
			statuses[idx] = w.Code
		}(i)
	}
	wg.Wait()

	// All requests succeed (rate-limit allows up to 10/min; we send 8)
	for i, code := range statuses {
		assert.Equal(t, http.StatusAccepted, code, "concurrent request %d", i)
	}

	// Exactly one row
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`,
		channelID, user.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestGetUnifiedKeysAutoEnrollConcurrent(t *testing.T) {
	ts := setupTS(t)
	userA := ts.CreateTestUser(t, "autoenrconcA")
	userB := ts.CreateTestUser(t, "autoenrconcB")
	conversationID := ts.CreateDMConversation(t, userA.ID, userB.ID)
	_ = userB

	const N = 6
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := ts.DoRequest("GET", "/api/v1/e2ee/keys/"+conversationID, nil,
				testhelpers.AuthHeaders(userA.AccessToken))
			// All N concurrent GETs return 404+pending; we only check status here.
			assert.Equal(t, http.StatusNotFound, w.Code)
		}()
	}
	wg.Wait()

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, userA.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestAutoEnrollAndRequestRewrapRaceConverge(t *testing.T) {
	ts := setupTS(t)
	userA := ts.CreateTestUser(t, "raceA")
	userB := ts.CreateTestUser(t, "raceB")
	conversationID := ts.CreateDMConversation(t, userA.ID, userB.ID)
	_ = userB

	// One GET (triggers auto-enroll) and one POST /rewrap concurrently.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ts.DoRequest("GET", "/api/v1/e2ee/keys/"+conversationID, nil,
			testhelpers.AuthHeaders(userA.AccessToken))
	}()
	go func() {
		defer wg.Done()
		ts.DoRequest("POST", "/api/v1/e2ee/keys/"+conversationID+"/rewrap", nil,
			testhelpers.AuthHeaders(userA.AccessToken))
	}()
	wg.Wait()

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, userA.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}
