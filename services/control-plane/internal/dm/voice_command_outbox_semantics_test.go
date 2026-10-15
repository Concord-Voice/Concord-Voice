package dm_test

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type dmOutboxCommand struct {
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

func readDMOutboxCommand(t *testing.T, db *sql.DB, userID string) dmOutboxCommand {
	t.Helper()
	var total int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_command_outbox`).Scan(&total))
	require.Equal(t, 1, total, "one decision captures one exact target or one deliberate offline marker")
	var command dmOutboxCommand
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

func assertDMOutboxTarget(t *testing.T, command dmOutboxCommand) {
	t.Helper()
	assert.Equal(t, sql.NullString{String: dmOutboxSessionGeneration, Valid: true}, command.SessionGeneration)
	assert.Equal(t, sql.NullString{String: dmOutboxNodeBootID, Valid: true}, command.NodeBootID)
	assert.Equal(t, sql.NullString{String: dmOutboxSocketID, Valid: true}, command.SocketID)
	assert.Equal(t, sql.NullString{String: "", Valid: true}, command.CredentialEpoch)
}

func assertDMOutboxNullTarget(t *testing.T, command dmOutboxCommand) {
	t.Helper()
	assert.False(t, command.SessionGeneration.Valid)
	assert.False(t, command.NodeBootID.Valid)
	assert.False(t, command.SocketID.Valid)
	assert.False(t, command.CredentialEpoch.Valid)
}

func requireNoDMOutboxCommands(t *testing.T, db *sql.DB) {
	t.Helper()
	var count, allocated, published int64
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_command_outbox`).Scan(&count))
	require.NoError(t, db.QueryRow(`SELECT last_allocated_sequence, last_published_sequence FROM voice_command_sequence WHERE id = TRUE`).Scan(&allocated, &published))
	assert.Zero(t, count)
	assert.Zero(t, allocated, "a refused or rolled-back decision must not consume a command position")
	assert.Zero(t, published)
}

func rejectDMOutboxInserts(t *testing.T, db *sql.DB) {
	t.Helper()
	t.Cleanup(func() {
		_, err := db.Exec(`DROP TRIGGER IF EXISTS test_dm_outbox_insert_failure ON voice_command_outbox`)
		assert.NoError(t, err)
		_, err = db.Exec(`DROP FUNCTION IF EXISTS test_dm_outbox_insert_failure()`)
		assert.NoError(t, err)
	})
	_, err := db.Exec(`
		CREATE FUNCTION test_dm_outbox_insert_failure() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected outbox insert failure' USING ERRCODE = 'check_violation';
		END;
		$$;
		CREATE TRIGGER test_dm_outbox_insert_failure BEFORE INSERT ON voice_command_outbox
		FOR EACH ROW EXECUTE FUNCTION test_dm_outbox_insert_failure()
	`)
	require.NoError(t, err)
}

func assertDMSnapshotFields(t *testing.T, command dmOutboxCommand, action string, muted, deafened bool) {
	t.Helper()
	assert.Equal(t, "server_moderation", command.Kind)
	assert.Equal(t, action, command.Action)
	assert.Equal(t, sql.NullBool{Bool: muted, Valid: true}, command.Muted)
	assert.Equal(t, sql.NullBool{Bool: deafened, Valid: true}, command.Deafened)
	assert.False(t, command.Reason.Valid)
	assert.True(t, command.Revision.Valid)
	assert.Positive(t, command.Revision.Int64)
}

func assertDMRemovalOutboxCommand(t *testing.T, ts *testhelpers.TestServer, convID, userID string) {
	t.Helper()
	command := readDMOutboxCommand(t, ts.DB, userID)
	assert.Equal(t, "dm", command.Scope)
	assert.Equal(t, "disconnect", command.Kind)
	assert.Equal(t, "disconnect", command.Action)
	assert.Equal(t, sql.NullString{String: convID, Valid: true}, command.ChannelID)
	assert.False(t, command.ServerID.Valid)
	assertDMOutboxTarget(t, command)
	assert.Equal(t, sql.NullString{String: "membership_removed", Valid: true}, command.Reason)
	assert.False(t, command.Revision.Valid)
	assert.False(t, command.Muted.Valid)
	assert.False(t, command.Deafened.Valid)
	var remains bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`, convID, userID).Scan(&remains))
	assert.False(t, remains)
}

func newDMOutboxFixture(t *testing.T) (*testhelpers.TestServer, testhelpers.TestUser, testhelpers.TestUser, string) {
	t.Helper()
	t.Setenv("NATS_URL", dmOutboxUnavailableNATSURL)
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "dm-outbox-semantic-admin")
	target := ts.CreateTestUser(t, "dm-outbox-semantic-target")
	convID := createTestGroup(t, ts, admin, target)
	return ts, admin, target, convID
}

func TestVoiceCommandOutbox_DMOfflineSnapshotMarker(t *testing.T) {
	ts, admin, target, convID := newDMOutboxFixture(t)
	response := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+pathDeafen, nil, testhelpers.AuthHeaders(admin.AccessToken))
	requireDMOutboxQueued(t, response, http.StatusOK)
	command := readDMOutboxCommand(t, ts.DB, target.ID)
	assert.Equal(t, "dm", command.Scope)
	assert.Equal(t, "server_moderation", command.Kind)
	assert.Equal(t, "deafen", command.Action)
	assert.Equal(t, sql.NullString{String: convID, Valid: true}, command.ChannelID)
	assert.False(t, command.ServerID.Valid)
	assertDMOutboxNullTarget(t, command)
	assert.Equal(t, sql.NullBool{Bool: true, Valid: true}, command.Muted)
	assert.Equal(t, sql.NullBool{Bool: true, Valid: true}, command.Deafened)
	assert.True(t, command.Revision.Valid)
	assert.Positive(t, command.Revision.Int64)
}

func TestVoiceCommandOutbox_DMRemovalOfflineMarker(t *testing.T) {
	ts, admin, target, convID := newDMOutboxFixture(t)
	response := ts.DoRequest(http.MethodDelete, pathDMConversationsPrefix+convID+pathMembersSlash+target.ID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	requireDMOutboxQueued(t, response, http.StatusOK)
	command := readDMOutboxCommand(t, ts.DB, target.ID)
	assert.Equal(t, "dm", command.Scope)
	assert.Equal(t, "disconnect", command.Kind)
	assert.Equal(t, "disconnect", command.Action)
	assert.Equal(t, sql.NullString{String: convID, Valid: true}, command.ChannelID)
	assert.False(t, command.ServerID.Valid)
	assertDMOutboxNullTarget(t, command)
	assert.Equal(t, sql.NullString{String: "membership_removed", Valid: true}, command.Reason)
	assert.False(t, command.Revision.Valid)
	assert.False(t, command.Muted.Valid)
	assert.False(t, command.Deafened.Valid)
	var remains bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`, convID, target.ID).Scan(&remains))
	assert.False(t, remains)
}

func TestVoiceCommandOutbox_DMProvisionalSnapshot(t *testing.T) {
	ts, admin, target, convID := newDMOutboxFixture(t)
	seedDMOutboxSession(t, ts, convID, target.ID)
	response := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+pathDeafen, nil, testhelpers.AuthHeaders(admin.AccessToken))
	requireDMOutboxQueued(t, response, http.StatusOK)
	command := readDMOutboxCommand(t, ts.DB, target.ID)
	assert.Equal(t, "dm", command.Scope)
	assert.Equal(t, sql.NullString{String: convID, Valid: true}, command.ChannelID)
	assert.False(t, command.ServerID.Valid)
	assertDMOutboxTarget(t, command)
	assertDMSnapshotFields(t, command, "deafen", true, true)
	var presence int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_voice_participants WHERE conversation_id = $1`, convID).Scan(&presence))
	assert.Zero(t, presence)
}

func TestVoiceCommandOutbox_DMProvisionalUserMuteRequiresPresence(t *testing.T) {
	ts, actor, target, convID := newDMOutboxFixture(t)
	seedDMOutboxSession(t, ts, convID, target.ID)
	response := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+pathUserMute, nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	requireNoDMOutboxCommands(t, ts.DB)
}

func TestVoiceCommandOutbox_DMRejectsUnregisteredPresence(t *testing.T) {
	ts, actor, target, convID := newDMOutboxFixture(t)
	seedDMVoiceParticipant(t, ts, convID, target.ID)
	response := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+pathUserMute, nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	requireNoDMOutboxCommands(t, ts.DB)
}

func TestVoiceCommandOutbox_DMDeniedActorCreatesNoCommand(t *testing.T) {
	for _, mode := range []string{"unauthorized", "stale credential"} {
		t.Run(mode, func(t *testing.T) {
			ts, admin, target, convID := newDMOutboxFixture(t)
			seedDMOutboxSession(t, ts, convID, target.ID)
			token, status := target.AccessToken, http.StatusForbidden
			if mode == "stale credential" {
				token, status = ts.SimulateStaleEpochWindow(t, admin.ID), http.StatusUnauthorized
			}
			response := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+pathDeafen, nil, testhelpers.AuthHeaders(token))
			require.Equal(t, status, response.Code, response.Body.String())
			var muted, deafened bool
			require.NoError(t, ts.DB.QueryRow(`SELECT server_muted, server_deafened FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target.ID).Scan(&muted, &deafened))
			assert.False(t, muted)
			assert.False(t, deafened)
			requireNoDMOutboxCommands(t, ts.DB)
		})
	}
}

func TestVoiceCommandOutbox_DMEnqueueFailureRollsBackPolicy(t *testing.T) {
	ts, admin, target, convID := newDMOutboxFixture(t)
	seedDMOutboxSession(t, ts, convID, target.ID)
	rejectDMOutboxInserts(t, ts.DB)
	response := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+pathDeafen, nil, testhelpers.AuthHeaders(admin.AccessToken))
	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	var muted, deafened bool
	require.NoError(t, ts.DB.QueryRow(`SELECT server_muted, server_deafened FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target.ID).Scan(&muted, &deafened))
	assert.False(t, muted)
	assert.False(t, deafened)
	requireNoDMOutboxCommands(t, ts.DB)
}

func TestVoiceCommandOutbox_DMEnqueueFailureRollsBackRemoval(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "provisional"
		if active {
			name = "active"
		}
		t.Run(name, func(t *testing.T) {
			ts, admin, target, convID := newDMOutboxFixture(t)
			seedDMOutboxSession(t, ts, convID, target.ID)
			if active {
				seedDMVoiceParticipant(t, ts, convID, target.ID)
			}
			rejectDMOutboxInserts(t, ts.DB)
			response := ts.DoRequest(http.MethodDelete, pathDMConversationsPrefix+convID+pathMembersSlash+target.ID, nil, testhelpers.AuthHeaders(admin.AccessToken))
			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			var remains bool
			require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`, convID, target.ID).Scan(&remains))
			assert.True(t, remains, "enqueue failure must roll back membership removal")
			var presence int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target.ID).Scan(&presence))
			if active {
				assert.Equal(t, 1, presence)
			} else {
				assert.Zero(t, presence)
			}
			var ejections, revocations int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_block_voice_ejections WHERE conversation_id = $1 AND user_id = $2`, convID, target.ID).Scan(&ejections))
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_key_revocations WHERE conversation_id = $1`, convID).Scan(&revocations))
			assert.Zero(t, ejections, "the existing membership-ejection rail must roll back with the mutation")
			assert.Zero(t, revocations, "key-authority changes must roll back with the mutation")
			requireNoDMOutboxCommands(t, ts.DB)
		})
	}
}

func TestVoiceCommandOutbox_DMSnapshotCapturesEveryRegisteredSession(t *testing.T) {
	ts, admin, target, convID := newDMOutboxFixture(t)
	seedDMOutboxSession(t, ts, convID, target.ID)
	const secondSession = "26230000-0000-4000-8000-000000000013"
	const secondBoot = "26230000-0000-4000-8000-000000000014"
	const secondSocket = "second-dm-outbox-test-socket"
	const secondEpoch = "00000000000000000000000000000001"
	t.Cleanup(func() {
		_, err := ts.DB.Exec(`DELETE FROM voice_enforcement_sessions WHERE session_generation = $1 AND node_boot_id = $2`, secondSession, secondBoot)
		assert.NoError(t, err)
	})
	_, err := ts.DB.Exec(`
		INSERT INTO voice_enforcement_sessions (session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
		VALUES ($1, $2, $3, 'dm', $4, $5, $6)
	`, secondSession, secondBoot, convID, target.ID, secondEpoch, secondSocket)
	require.NoError(t, err)
	response := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+pathDeafen, nil, testhelpers.AuthHeaders(admin.AccessToken))
	requireDMOutboxQueued(t, response, http.StatusOK)
	var commands int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM voice_command_outbox`).Scan(&commands))
	require.Equal(t, 2, commands)
	for _, exact := range []struct{ session, boot, socket, epoch string }{
		{dmOutboxSessionGeneration, dmOutboxNodeBootID, dmOutboxSocketID, ""},
		{secondSession, secondBoot, secondSocket, secondEpoch},
	} {
		var matching int
		require.NoError(t, ts.DB.QueryRow(`
			SELECT COUNT(*) FROM voice_command_outbox
			WHERE scope = 'dm' AND kind = 'server_moderation' AND action = 'deafen'
				AND channel_id = $1 AND user_id = $2 AND server_id IS NULL
				AND session_generation = $3 AND node_boot_id = $4 AND socket_id = $5 AND credential_epoch = $6
				AND server_muted AND server_deafened AND authorization_revision > 0
				AND stream_generation = (SELECT generation FROM voice_command_transport WHERE id = TRUE)
				AND published_at IS NULL AND broker_sequence IS NULL
		`, convID, target.ID, exact.session, exact.boot, exact.socket, exact.epoch).Scan(&matching))
		assert.Equal(t, 1, matching, "every immutable session gets its own complete retained snapshot")
	}
}
