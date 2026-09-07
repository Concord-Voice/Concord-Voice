//go:build integration

package messages_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelPinsExposeNullableAndExactExpiry(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinexpirychannel")
	serverID := ts.CreateTestServer(t, user.ID, "Pin Expiry Channel")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	nullID := ts.CreateTestMessage(t, channelID, user, "null expiry pin")
	expiringID := ts.CreateTestMessage(t, channelID, user, "expiring pin")
	expected := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	_, err := ts.DB.Exec(`UPDATE messages SET pinned_at = NOW(), pinned_by = $1, expires_at = $2 WHERE id = $3`, user.ID, expected, expiringID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE messages SET pinned_at = NOW(), pinned_by = $1, expires_at = NULL WHERE id = $2`, user.ID, nullID)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodGet, "/api/v1/channels/"+channelID+"/pins", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	pins := testhelpers.JSONField[[]interface{}](t, body, "pinned_messages")
	require.Len(t, pins, 2)
	ids := make([]string, 0, len(pins))
	for _, raw := range pins {
		pin := testhelpers.JSONAs[map[string]interface{}](t, raw, "channel pin")
		id := testhelpers.JSONField[string](t, pin, "id")
		ids = append(ids, id)
		assert.Contains(t, pin, "expires_at")
		switch id {
		case expiringID:
			assert.Equal(t, expected.Format(time.RFC3339Nano), testhelpers.JSONField[string](t, pin, "expires_at"))
		case nullID:
			assert.Nil(t, pin["expires_at"])
		}
	}
	assert.ElementsMatch(t, []string{nullID, expiringID}, ids)
}

func TestDMPinsExposeNullableAndExactExpiry(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinexpirydm")
	peer := ts.CreateTestUser(t, "pinexpirydmpeer")
	conversationID := ts.CreateDMConversation(t, user.ID, peer.ID)
	nullID := insertDMMessageDirect(t, ts, conversationID, user.ID, "null expiry dm pin")
	expiringID := insertDMMessageDirect(t, ts, conversationID, user.ID, "expiring dm pin")
	expected := time.Date(2031, time.February, 3, 4, 5, 6, 0, time.UTC)
	_, err := ts.DB.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = $1, expires_at = $2 WHERE id = $3`, user.ID, expected, expiringID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = $1, expires_at = NULL WHERE id = $2`, user.ID, nullID)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodGet, "/api/v1/channels/"+conversationID+"/pins", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	pins := testhelpers.JSONField[[]interface{}](t, body, "pinned_messages")
	require.Len(t, pins, 2)
	ids := make([]string, 0, len(pins))
	for _, raw := range pins {
		pin := testhelpers.JSONAs[map[string]interface{}](t, raw, "DM pin")
		id := testhelpers.JSONField[string](t, pin, "id")
		ids = append(ids, id)
		assert.Contains(t, pin, "expires_at")
		switch id {
		case expiringID:
			assert.Equal(t, expected.Format(time.RFC3339Nano), testhelpers.JSONField[string](t, pin, "expires_at"))
		case nullID:
			assert.Nil(t, pin["expires_at"])
		}
	}
	assert.ElementsMatch(t, []string{nullID, expiringID}, ids)
}
