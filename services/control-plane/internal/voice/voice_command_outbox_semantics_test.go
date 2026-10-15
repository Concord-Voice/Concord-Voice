package voice_test

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type serverOutboxCommand struct {
	Sequence                                                 int64
	CommandID, Generation                                    string
	Version                                                  int
	Scope, Kind, Action, UserID                              string
	ChannelID, ServerID                                      sql.NullString
	SessionGeneration, NodeBootID, SocketID, CredentialEpoch sql.NullString
	Reason                                                   sql.NullString
	Revision                                                 sql.NullInt64
	Muted, Deafened                                          sql.NullBool
	CreatedAt                                                time.Time
	PublishedAt                                              sql.NullTime
	BrokerSequence                                           sql.NullInt64
}

func readServerOutboxCommand(t *testing.T, db *sql.DB, userID string) serverOutboxCommand {
	t.Helper()
	var total int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_command_outbox`).Scan(&total))
	require.Equal(t, 1, total, "one decision captures one exact target or one deliberate offline marker")
	var command serverOutboxCommand
	require.NoError(t, db.QueryRow(`
		SELECT sequence, command_id::text, stream_generation::text, envelope_version,
			scope, kind, action, user_id::text, channel_id::text, server_id::text,
			session_generation::text, node_boot_id::text, socket_id, credential_epoch,
			reason, authorization_revision, server_muted, server_deafened,
			created_at, published_at, broker_sequence
		FROM voice_command_outbox WHERE user_id = $1
	`, userID).Scan(
		&command.Sequence, &command.CommandID, &command.Generation, &command.Version,
		&command.Scope, &command.Kind, &command.Action, &command.UserID, &command.ChannelID, &command.ServerID,
		&command.SessionGeneration, &command.NodeBootID, &command.SocketID, &command.CredentialEpoch,
		&command.Reason, &command.Revision, &command.Muted, &command.Deafened,
		&command.CreatedAt, &command.PublishedAt, &command.BrokerSequence,
	))
	var generation string
	require.NoError(t, db.QueryRow(`SELECT generation::text FROM voice_command_transport WHERE id = TRUE`).Scan(&generation))
	assert.Equal(t, generation, command.Generation)
	assert.Equal(t, 1, command.Version)
	assert.Positive(t, command.Sequence)
	assert.NotEmpty(t, command.CommandID)
	assert.False(t, command.CreatedAt.IsZero())
	assert.False(t, command.PublishedAt.Valid, "an unavailable broker cannot confirm publication")
	assert.False(t, command.BrokerSequence.Valid, "pending obligations have no fabricated receipt")
	return command
}

func assertServerOutboxTarget(t *testing.T, command serverOutboxCommand) {
	t.Helper()
	assert.Equal(t, sql.NullString{String: outboxSessionGeneration, Valid: true}, command.SessionGeneration)
	assert.Equal(t, sql.NullString{String: outboxNodeBootID, Valid: true}, command.NodeBootID)
	assert.Equal(t, sql.NullString{String: outboxSocketID, Valid: true}, command.SocketID)
	assert.Equal(t, sql.NullString{String: "", Valid: true}, command.CredentialEpoch)
}

func assertServerOutboxNullTarget(t *testing.T, command serverOutboxCommand) {
	t.Helper()
	assert.False(t, command.SessionGeneration.Valid)
	assert.False(t, command.NodeBootID.Valid)
	assert.False(t, command.SocketID.Valid)
	assert.False(t, command.CredentialEpoch.Valid)
}

func requireNoServerOutboxCommands(t *testing.T, db *sql.DB) {
	t.Helper()
	var count, allocated, published int64
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_command_outbox`).Scan(&count))
	require.NoError(t, db.QueryRow(`SELECT last_allocated_sequence, last_published_sequence FROM voice_command_sequence WHERE id = TRUE`).Scan(&allocated, &published))
	assert.Zero(t, count)
	assert.Zero(t, allocated, "a refused or rolled-back decision must not consume a command position")
	assert.Zero(t, published)
}

func rejectServerOutboxInserts(t *testing.T, db *sql.DB) {
	t.Helper()
	t.Cleanup(func() {
		_, err := db.Exec(`DROP TRIGGER IF EXISTS test_server_outbox_insert_failure ON voice_command_outbox`)
		assert.NoError(t, err)
		_, err = db.Exec(`DROP FUNCTION IF EXISTS test_server_outbox_insert_failure()`)
		assert.NoError(t, err)
	})
	_, err := db.Exec(`
		CREATE FUNCTION test_server_outbox_insert_failure() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected outbox insert failure' USING ERRCODE = 'check_violation';
		END;
		$$;
		CREATE TRIGGER test_server_outbox_insert_failure BEFORE INSERT ON voice_command_outbox
		FOR EACH ROW EXECUTE FUNCTION test_server_outbox_insert_failure()
	`)
	require.NoError(t, err)
}

func assertServerSnapshotFields(t *testing.T, command serverOutboxCommand, action string, muted, deafened bool) {
	t.Helper()
	assert.Equal(t, "server_moderation", command.Kind)
	assert.Equal(t, action, command.Action)
	assert.Equal(t, sql.NullBool{Bool: muted, Valid: true}, command.Muted)
	assert.Equal(t, sql.NullBool{Bool: deafened, Valid: true}, command.Deafened)
	assert.False(t, command.Reason.Valid)
	assert.True(t, command.Revision.Valid)
	assert.Positive(t, command.Revision.Int64)
}

func newServerOutboxFixture(t *testing.T) (*testhelpers.TestServer, testhelpers.TestUser, testhelpers.TestUser, string, string) {
	t.Helper()
	t.Setenv("NATS_URL", outboxUnavailableNATSURL)
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "outbox-semantic-owner")
	target := ts.CreateTestUser(t, "outbox-semantic-target")
	serverID := ts.CreateTestServer(t, owner.ID, "Outbox Semantics")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "outbox-semantic-channel")
	return ts, owner, target, serverID, channelID
}

func TestVoiceCommandOutbox_ServerOfflineSnapshotMarker(t *testing.T) {
	ts, owner, target, serverID, _ := newServerOutboxFixture(t)
	response := ts.DoRequest(http.MethodPost, voiceEnforcePath(serverID, target.ID, pathDeafen), nil, testhelpers.AuthHeaders(owner.AccessToken))
	requireOutboxQueued(t, response, http.StatusOK)
	command := readServerOutboxCommand(t, ts.DB, target.ID)
	assert.Equal(t, "channel", command.Scope)
	assert.Equal(t, "server_moderation", command.Kind)
	assert.Equal(t, "deafen", command.Action)
	assert.False(t, command.ChannelID.Valid, "offline server policy has no fabricated room")
	assert.Equal(t, sql.NullString{String: serverID, Valid: true}, command.ServerID)
	assertServerOutboxNullTarget(t, command)
	assert.Equal(t, sql.NullBool{Bool: true, Valid: true}, command.Muted)
	assert.Equal(t, sql.NullBool{Bool: true, Valid: true}, command.Deafened)
	assert.True(t, command.Revision.Valid)
	assert.Positive(t, command.Revision.Int64)
}

func TestVoiceCommandOutbox_ServerProvisionalTransient(t *testing.T) {
	for _, test := range []struct{ name, suffix, kind, action, reason string }{
		{name: "user mute", suffix: pathUserMute, kind: "user_moderation", action: "mute"},
		{name: "user deafen", suffix: pathUserDeafen, kind: "user_moderation", action: "deafen"},
		{name: "disconnect", suffix: pathDisconnect, kind: "disconnect", action: "disconnect", reason: "moderator_request"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts, owner, target, serverID, channelID := newServerOutboxFixture(t)
			seedOutboxVoiceSession(t, ts, channelID, target.ID)
			response := ts.DoRequest(http.MethodPost, voiceEnforcePath(serverID, target.ID, test.suffix), nil, testhelpers.AuthHeaders(owner.AccessToken))
			requireOutboxQueued(t, response, http.StatusAccepted)
			command := readServerOutboxCommand(t, ts.DB, target.ID)
			assert.Equal(t, "channel", command.Scope)
			assert.Equal(t, test.kind, command.Kind)
			assert.Equal(t, test.action, command.Action)
			assert.Equal(t, sql.NullString{String: channelID, Valid: true}, command.ChannelID)
			assert.False(t, command.ServerID.Valid)
			assertServerOutboxTarget(t, command)
			assert.Equal(t, sql.NullString{String: test.reason, Valid: test.reason != ""}, command.Reason)
			assert.False(t, command.Revision.Valid)
			assert.False(t, command.Muted.Valid)
			assert.False(t, command.Deafened.Valid)
			var presence int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM voice_participants WHERE user_id = $1`, target.ID).Scan(&presence))
			assert.Zero(t, presence, "immutable registration, not a later presence event, identifies the target")
		})
	}
}

func TestVoiceCommandOutbox_ServerRejectsUnregisteredPresence(t *testing.T) {
	for _, test := range []struct {
		name, suffix string
		status       int
	}{
		{name: "user mute", suffix: pathUserMute, status: http.StatusBadRequest},
		{name: "user deafen", suffix: pathUserDeafen, status: http.StatusBadRequest},
		{name: "disconnect", suffix: pathDisconnect, status: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts, owner, target, serverID, channelID := newServerOutboxFixture(t)
			_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
			require.NoError(t, err)
			response := ts.DoRequest(http.MethodPost, voiceEnforcePath(serverID, target.ID, test.suffix), nil, testhelpers.AuthHeaders(owner.AccessToken))
			require.Equal(t, test.status, response.Code, response.Body.String())
			requireNoServerOutboxCommands(t, ts.DB)
		})
	}
}

func TestVoiceCommandOutbox_ServerDeniedActorCreatesNoCommand(t *testing.T) {
	for _, mode := range []string{"unauthorized", "stale credential"} {
		t.Run(mode, func(t *testing.T) {
			ts, owner, target, serverID, channelID := newServerOutboxFixture(t)
			seedOutboxVoiceSession(t, ts, channelID, target.ID)
			var token string
			status := http.StatusUnauthorized
			if mode == "stale credential" {
				token = ts.SimulateStaleEpochWindow(t, owner.ID)
			} else {
				actor := ts.CreateTestUser(t, "outbox-denied-actor")
				ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
				token, status = actor.AccessToken, http.StatusForbidden
			}
			response := ts.DoRequest(http.MethodPost, voiceEnforcePath(serverID, target.ID, pathDeafen), nil, testhelpers.AuthHeaders(token))
			require.Equal(t, status, response.Code, response.Body.String())
			var muted, deafened bool
			require.NoError(t, ts.DB.QueryRow(`SELECT server_muted, server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&muted, &deafened))
			assert.False(t, muted)
			assert.False(t, deafened)
			requireNoServerOutboxCommands(t, ts.DB)
		})
	}
}

func TestVoiceCommandOutbox_ServerEnqueueFailureRollsBackPolicy(t *testing.T) {
	for _, test := range []struct {
		name, method, suffix string
		beforeMuted          bool
	}{
		{name: "deafen", method: http.MethodPost, suffix: pathDeafen},
		{name: "unmute", method: http.MethodDelete, suffix: pathMute, beforeMuted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts, owner, target, serverID, channelID := newServerOutboxFixture(t)
			seedOutboxVoiceSession(t, ts, channelID, target.ID)
			_, err := ts.DB.Exec(`UPDATE server_members SET server_muted = $3 WHERE server_id = $1 AND user_id = $2`, serverID, target.ID, test.beforeMuted)
			require.NoError(t, err)
			rejectServerOutboxInserts(t, ts.DB)
			response := ts.DoRequest(test.method, voiceEnforcePath(serverID, target.ID, test.suffix), nil, testhelpers.AuthHeaders(owner.AccessToken))
			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			var muted, deafened bool
			require.NoError(t, ts.DB.QueryRow(`SELECT server_muted, server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&muted, &deafened))
			assert.Equal(t, test.beforeMuted, muted, "the authoritative policy and enqueue must commit together")
			assert.False(t, deafened)
			requireNoServerOutboxCommands(t, ts.DB)
		})
	}
}

func TestVoiceCommandOutbox_ServerSnapshotCapturesEveryRegisteredSession(t *testing.T) {
	ts, owner, target, serverID, channelID := newServerOutboxFixture(t)
	seedOutboxVoiceSession(t, ts, channelID, target.ID)
	const secondSession = "26230000-0000-4000-8000-000000000003"
	const secondBoot = "26230000-0000-4000-8000-000000000004"
	const secondSocket = "second-outbox-test-socket"
	const secondEpoch = "00000000000000000000000000000001"
	t.Cleanup(func() {
		_, err := ts.DB.Exec(`DELETE FROM voice_enforcement_sessions WHERE session_generation = $1 AND node_boot_id = $2`, secondSession, secondBoot)
		assert.NoError(t, err)
	})
	_, err := ts.DB.Exec(`
		INSERT INTO voice_enforcement_sessions (session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
		VALUES ($1, $2, $3, 'channel', $4, $5, $6)
	`, secondSession, secondBoot, channelID, target.ID, secondEpoch, secondSocket)
	require.NoError(t, err)
	response := ts.DoRequest(http.MethodPost, voiceEnforcePath(serverID, target.ID, pathDeafen), nil, testhelpers.AuthHeaders(owner.AccessToken))
	requireOutboxQueued(t, response, http.StatusOK)
	var commands int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM voice_command_outbox`).Scan(&commands))
	require.Equal(t, 2, commands)
	for _, exact := range []struct{ session, boot, socket, epoch string }{
		{outboxSessionGeneration, outboxNodeBootID, outboxSocketID, ""},
		{secondSession, secondBoot, secondSocket, secondEpoch},
	} {
		var matching int
		require.NoError(t, ts.DB.QueryRow(`
			SELECT COUNT(*) FROM voice_command_outbox
			WHERE scope = 'channel' AND kind = 'server_moderation' AND action = 'deafen'
				AND channel_id = $1 AND server_id = $2 AND user_id = $3
				AND session_generation = $4 AND node_boot_id = $5 AND socket_id = $6 AND credential_epoch = $7
				AND server_muted AND server_deafened AND authorization_revision > 0
				AND stream_generation = (SELECT generation FROM voice_command_transport WHERE id = TRUE)
				AND published_at IS NULL AND broker_sequence IS NULL
		`, channelID, serverID, target.ID, exact.session, exact.boot, exact.socket, exact.epoch).Scan(&matching))
		assert.Equal(t, 1, matching, "every immutable session gets its own complete retained snapshot")
	}
}
