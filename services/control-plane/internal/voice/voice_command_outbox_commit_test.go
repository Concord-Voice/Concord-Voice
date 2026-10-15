package voice

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedCommitOutboxSession(t *testing.T, db *sql.DB, channelID, userID uuid.UUID) uuid.UUID {
	t.Helper()
	session, boot := uuid.New(), uuid.New()
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM voice_enforcement_sessions WHERE session_generation = $1 AND node_boot_id = $2`, session, boot)
		assert.NoError(t, err)
	})
	_, err := db.Exec(`
		INSERT INTO voice_enforcement_sessions (session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
		VALUES ($1, $2, $3, 'channel', $4, '', 'commit-outbox-test-socket')
	`, session, boot, channelID, userID)
	require.NoError(t, err)
	return session
}

func TestVoiceCommandOutbox_ServerDisconnectCommitAckLossRetainsCommand(t *testing.T) {
	db, redisClient, owner, target, serverID, channelID, _ := commitFailureVoiceEffectFixture(t)
	session := seedCommitOutboxSession(t, db, channelID, target)
	var effects atomic.Int32
	handler := newCommitFailureVoiceEffectHandler(t, db, redisClient, &effects)
	response := invokeCommitFailureVoiceEffect(t, handler, owner.String(), http.MethodPost,
		"/servers/"+serverID.String()+"/voice/"+target.String()+"/disconnect", nil)
	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	assert.Zero(t, effects.Load(), "unknown commit acknowledgement must not emit a direct effect")
	var retained int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM voice_command_outbox
		WHERE user_id = $1 AND channel_id = $2 AND session_generation = $3
			AND scope = 'channel' AND kind = 'disconnect' AND action = 'disconnect'
			AND reason = 'moderator_request' AND published_at IS NULL AND broker_sequence IS NULL
	`, target, channelID, session).Scan(&retained))
	assert.Equal(t, 1, retained, "a successful commit with a lost acknowledgement must retain its obligation")
}

func TestVoiceCommandOutbox_ServerDisconnectNilNATSRetainsCommand(t *testing.T) {
	db, redisClient, owner, target, serverID, channelID, _ := commitFailureVoiceEffectFixture(t)
	session := seedCommitOutboxSession(t, db, channelID, target)
	var effects atomic.Int32
	handler := newCommitFailureVoiceEffectHandler(t, db, redisClient, &effects)
	handler.commitVoiceEffectTxForTest = nil
	require.Nil(t, handler.nats)
	response := invokeCommitFailureVoiceEffect(t, handler, owner.String(), http.MethodPost,
		"/servers/"+serverID.String()+"/voice/"+target.String()+"/disconnect", nil)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	var body struct {
		EnforcementStatus string `json:"enforcement_status"`
		Disconnected      bool   `json:"disconnected"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, "queued", body.EnforcementStatus)
	assert.False(t, body.Disconnected)
	var retained int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM voice_command_outbox WHERE user_id = $1 AND session_generation = $2
			AND scope = 'channel' AND kind = 'disconnect' AND action = 'disconnect'
			AND published_at IS NULL AND broker_sequence IS NULL
	`, target, session).Scan(&retained))
	assert.Equal(t, 1, retained)
}
