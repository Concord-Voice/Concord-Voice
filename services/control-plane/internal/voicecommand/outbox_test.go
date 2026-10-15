package voicecommand

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

const outboxTestStream = "VOICE_COMMANDS_V1"

// Each fixture holds SetupTestDB's cross-process advisory lock. Its cleanup is
// registered last so durable rows and registry evidence are removed before the
// shared helper closes the pool or a transport fixture replaces the singleton.
func outboxDatabase(t *testing.T) (*sql.DB, uuid.UUID) {
	t.Helper()
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "outbox tests require an explicitly isolated DATABASE_URL")
	db, _ := testdb.SetupTestDB(t)
	reset := func() {
		_, err := db.Exec(`TRUNCATE voice_command_admission, voice_command_retirement,
			voice_command_outbox, voice_command_sequence`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO voice_command_sequence (id) VALUES (TRUE)`)
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM voice_enforcement_sessions`)
		require.NoError(t, err)
	}
	reset()
	_, err := db.Exec(`DELETE FROM voice_command_transport`)
	require.NoError(t, err)
	generation := uuid.New()
	_, err = db.Exec(`INSERT INTO voice_command_transport (id, generation) VALUES (TRUE, $1)`, generation)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`TRUNCATE voice_command_admission, voice_command_retirement,
			voice_command_outbox, voice_command_sequence`)
		assert.NoError(t, err)
		_, err = db.Exec(`INSERT INTO voice_command_sequence (id) VALUES (TRUE)`)
		assert.NoError(t, err)
		_, err = db.Exec(`DELETE FROM voice_enforcement_sessions`)
		assert.NoError(t, err)
		_, err = db.Exec(`DELETE FROM voice_command_transport`)
		assert.NoError(t, err)
	})
	return db, generation
}

func outboxString(value string) *string { return &value }
func outboxBool(value bool) *bool       { return &value }

func outboxExactTarget() *ExactTarget {
	return &ExactTarget{
		SessionGeneration: "26230000-0000-4000-8000-000000000011",
		NodeBootID:        "26230000-0000-4000-8000-000000000012",
		SocketID:          "outbox-exact-socket",
		CredentialEpoch:   "11111111111111111111111111111111",
	}
}

func outboxSnapshot() CommandSpec {
	return CommandSpec{
		Scope: "dm", Kind: "server_moderation", Action: "mute",
		ChannelID: outboxString("26230000-0000-4000-8000-000000000013"),
		UserID:    "26230000-0000-4000-8000-000000000014", Target: outboxExactTarget(),
		AuthorizationRevision: "9007199254740993",
		ServerMuted:           outboxBool(true), ServerDeafened: outboxBool(false),
	}
}

func outboxCursor(t *testing.T, db *sql.DB) (int64, int64) {
	t.Helper()
	var allocated, published int64
	require.NoError(t, db.QueryRow(`SELECT last_allocated_sequence, last_published_sequence
		FROM voice_command_sequence WHERE id = TRUE`).Scan(&allocated, &published))
	return allocated, published
}

func requireOutboxCursor(t *testing.T, db *sql.DB, wantAllocated, wantPublished int64) {
	t.Helper()
	allocated, published := outboxCursor(t, db)
	assert.Equal(t, wantAllocated, allocated)
	assert.Equal(t, wantPublished, published)
}

func outboxCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_command_outbox`).Scan(&count))
	return count
}

func outboxBegin(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			assert.NoError(t, err)
		}
	})
	return tx
}

func outboxEnqueue(t *testing.T, db *sql.DB, specs ...CommandSpec) []VoiceCommand {
	t.Helper()
	tx := outboxBegin(t, db)
	commands, err := EnqueueTx(context.Background(), tx, specs)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Len(t, commands, len(specs))
	return commands
}

// Read every immutable field from typed SQL columns rather than asking the
// repository to reconstruct its own expected result.
func outboxStoredCommand(t *testing.T, db *sql.DB, sequence int64) VoiceCommand {
	t.Helper()
	var command VoiceCommand
	var channel, server, session, boot, socket, epoch, reason sql.NullString
	var revision sql.NullInt64
	var muted, deafened sql.NullBool
	var storedSequence int64
	require.NoError(t, db.QueryRow(`SELECT sequence, command_id, stream_generation, envelope_version,
		scope, kind, action, channel_id, server_id, user_id, session_generation, node_boot_id,
		socket_id, credential_epoch, reason, authorization_revision, server_muted, server_deafened
		FROM voice_command_outbox WHERE sequence = $1`, sequence).Scan(
		&storedSequence, &command.CommandID, &command.StreamGeneration, &command.Version,
		&command.Scope, &command.Kind, &command.Action, &channel, &server, &command.UserID,
		&session, &boot, &socket, &epoch, &reason, &revision, &muted, &deafened))
	command.Schema, command.Sequence = Schema, strconv.FormatInt(storedSequence, 10)
	if channel.Valid {
		command.ChannelID = outboxString(channel.String)
	}
	if server.Valid {
		command.ServerID = outboxString(server.String)
	}
	assert.Equal(t, session.Valid, boot.Valid)
	assert.Equal(t, session.Valid, socket.Valid)
	assert.Equal(t, session.Valid, epoch.Valid)
	if session.Valid {
		command.Target = &ExactTarget{SessionGeneration: session.String, NodeBootID: boot.String,
			SocketID: socket.String, CredentialEpoch: epoch.String}
	}
	if reason.Valid {
		command.Reason = reason.String
	}
	if revision.Valid {
		command.AuthorizationRevision = strconv.FormatInt(revision.Int64, 10)
	}
	if muted.Valid {
		command.ServerMuted = outboxBool(muted.Bool)
	}
	if deafened.Valid {
		command.ServerDeafened = outboxBool(deafened.Bool)
	}
	return command
}

func TestVoiceOutbox_EnqueueRollbackRestoresMutationAndSequence(t *testing.T) {
	db, generation := outboxDatabase(t)
	userID := testdb.CreateUser(t, db).String()
	var beforeName string
	require.NoError(t, db.QueryRow(`SELECT username FROM users WHERE id = $1`, userID).Scan(&beforeName))
	tx := outboxBegin(t, db)
	_, err := tx.Exec(`UPDATE users SET username = 'outbox-rolled-back' WHERE id = $1`, userID)
	require.NoError(t, err)
	spec := outboxSnapshot()
	spec.UserID = userID
	commands, err := EnqueueTx(context.Background(), tx, []CommandSpec{spec, spec})
	require.NoError(t, err, "unprovisioned transport must still admit durable work")
	require.Len(t, commands, 2)
	assert.Equal(t, "1", commands[0].Sequence)
	assert.Equal(t, "2", commands[1].Sequence)
	assert.Equal(t, generation.String(), commands[0].StreamGeneration)
	assert.NotEqual(t, commands[0].CommandID, commands[1].CommandID)
	assert.NotEqual(t, uuid.Nil.String(), commands[0].CommandID)
	assert.Equal(t, Schema, commands[0].Schema)
	assert.Equal(t, Version, commands[0].Version)
	assert.Zero(t, outboxCount(t, db), "enqueue must not commit its caller's transaction")
	requireOutboxCursor(t, db, 0, 0)
	require.NoError(t, tx.Rollback())
	var afterName string
	require.NoError(t, db.QueryRow(`SELECT username FROM users WHERE id = $1`, userID).Scan(&afterName))
	assert.Equal(t, beforeName, afterName)
	assert.Zero(t, outboxCount(t, db))
	requireOutboxCursor(t, db, 0, 0)
	committed := outboxEnqueue(t, db, spec)
	assert.Equal(t, "1", committed[0].Sequence, "rollback must leave no sequence gap")
	assert.Equal(t, committed[0], outboxStoredCommand(t, db, 1))
	requireOutboxCursor(t, db, 1, 0)
}

func TestVoiceOutbox_InvalidBatchRollsBackAllObligations(t *testing.T) {
	db, _ := outboxDatabase(t)
	userID := testdb.CreateUser(t, db).String()
	var beforeName string
	require.NoError(t, db.QueryRow(`SELECT username FROM users WHERE id = $1`, userID).Scan(&beforeName))
	tx := outboxBegin(t, db)
	_, err := tx.Exec(`UPDATE users SET username = 'outbox-invalid-batch' WHERE id = $1`, userID)
	require.NoError(t, err)
	invalid := outboxSnapshot()
	invalid.Target.SocketID = strings.Repeat("x", 129)
	_, err = EnqueueTx(context.Background(), tx, []CommandSpec{outboxSnapshot(), invalid})
	require.Error(t, err)
	require.NoError(t, tx.Rollback())
	var afterName string
	require.NoError(t, db.QueryRow(`SELECT username FROM users WHERE id = $1`, userID).Scan(&afterName))
	assert.Equal(t, beforeName, afterName)
	assert.Zero(t, outboxCount(t, db))
	requireOutboxCursor(t, db, 0, 0)
	assert.Equal(t, "1", outboxEnqueue(t, db, outboxSnapshot())[0].Sequence)
}

func TestVoiceOutbox_ValidExactCommandsAndOfflineMarkers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*CommandSpec)
	}{
		{"complete exact snapshot", func(*CommandSpec) {}},
		{"complete inverse flags", func(s *CommandSpec) { s.Action = "unmute"; s.ServerMuted = outboxBool(false) }},
		{"complete deafen", func(s *CommandSpec) { s.Action = "deafen"; s.ServerDeafened = outboxBool(true) }},
		{"legacy empty epoch", func(s *CommandSpec) { s.Target.CredentialEpoch = "" }},
		{"128 UTF-8 bytes", func(s *CommandSpec) { s.Target.SocketID = strings.Repeat("é", 64) }},
		{"offline server policy", func(s *CommandSpec) {
			s.Scope, s.ChannelID, s.Target = "channel", nil, nil
			s.ServerID = outboxString("26230000-0000-4000-8000-000000000015")
		}},
		{"offline DM policy", func(s *CommandSpec) { s.Target = nil }},
		{"offline DM membership removal", func(s *CommandSpec) {
			s.Kind, s.Action, s.Reason, s.Target = "disconnect", "disconnect", "membership_removed", nil
			s.AuthorizationRevision, s.ServerMuted, s.ServerDeafened = "", nil, nil
		}},
		{"transient exact mute", func(s *CommandSpec) {
			s.Kind, s.AuthorizationRevision, s.ServerMuted, s.ServerDeafened = "user_moderation", "", nil, nil
		}},
		{"transient exact disconnect", func(s *CommandSpec) {
			s.Kind, s.Action, s.Reason = "disconnect", "disconnect", "moderator_request"
			s.AuthorizationRevision, s.ServerMuted, s.ServerDeafened = "", nil, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, generation := outboxDatabase(t)
			spec := outboxSnapshot()
			tc.change(&spec)
			commands := outboxEnqueue(t, db, spec)
			stored := outboxStoredCommand(t, db, 1)
			assert.Equal(t, commands[0], stored)
			assert.Equal(t, generation.String(), stored.StreamGeneration)
			assert.Equal(t, spec.Scope, stored.Scope)
			assert.Equal(t, spec.Kind, stored.Kind)
			assert.Equal(t, spec.Action, stored.Action)
			assert.Equal(t, spec.ChannelID, stored.ChannelID)
			assert.Equal(t, spec.ServerID, stored.ServerID)
			assert.Equal(t, spec.Target, stored.Target)
			assert.Equal(t, spec.AuthorizationRevision, stored.AuthorizationRevision)
			assert.Equal(t, spec.ServerMuted, stored.ServerMuted)
			assert.Equal(t, spec.ServerDeafened, stored.ServerDeafened)
			assert.Equal(t, spec.Reason, stored.Reason)
			requireOutboxCursor(t, db, 1, 0)
		})
	}
}

func TestVoiceOutbox_RejectsInvalidSpecs(t *testing.T) {
	_, err := EnqueueTx(context.Background(), nil, []CommandSpec{outboxSnapshot()})
	require.Error(t, err)
	for _, tc := range []struct {
		name   string
		change func(*CommandSpec)
	}{
		{"missing user", func(s *CommandSpec) { s.UserID = "" }},
		{"zero user UUID", func(s *CommandSpec) { s.UserID = uuid.Nil.String() }},
		{"noncanonical user UUID", func(s *CommandSpec) { s.UserID = "not-a-uuid" }},
		{"unknown scope", func(s *CommandSpec) { s.Scope = "group" }},
		{"unknown kind", func(s *CommandSpec) { s.Kind = "policy" }},
		{"unknown action", func(s *CommandSpec) { s.Action = "pause" }},
		{"DM snapshot without room", func(s *CommandSpec) { s.ChannelID, s.Target = nil, nil }},
		{"DM snapshot with server", func(s *CommandSpec) { s.ServerID = outboxString(uuid.NewString()) }},
		{"channel snapshot without server", func(s *CommandSpec) { s.Scope = "channel" }},
		{"snapshot without revision", func(s *CommandSpec) { s.AuthorizationRevision = "" }},
		{"snapshot zero revision", func(s *CommandSpec) { s.AuthorizationRevision = "0" }},
		{"snapshot overflowing revision", func(s *CommandSpec) { s.AuthorizationRevision = "9223372036854775808" }},
		{"snapshot missing flag", func(s *CommandSpec) { s.ServerDeafened = nil }},
		{"deafened without muted", func(s *CommandSpec) { s.ServerMuted = outboxBool(false); s.ServerDeafened = outboxBool(true) }},
		{"snapshot with reason", func(s *CommandSpec) { s.Reason = "membership_removed" }},
		{"target without room", func(s *CommandSpec) { s.ChannelID = nil }},
		{"target without session generation", func(s *CommandSpec) { s.Target.SessionGeneration = "" }},
		{"target without node birth", func(s *CommandSpec) { s.Target.NodeBootID = "" }},
		{"empty socket", func(s *CommandSpec) { s.Target.SocketID = "" }},
		{"oversized legacy socket", func(s *CommandSpec) { s.Target.SocketID = strings.Repeat("x", 255) }},
		{"129 UTF-8 bytes", func(s *CommandSpec) { s.Target.SocketID = strings.Repeat("é", 64) + "x" }},
		{"control in socket", func(s *CommandSpec) { s.Target.SocketID = "socket\nother" }},
		{"invalid credential epoch", func(s *CommandSpec) { s.Target.CredentialEpoch = "legacy" }},
		{"targetless transient", func(s *CommandSpec) {
			s.Kind, s.Target, s.AuthorizationRevision, s.ServerMuted, s.ServerDeafened = "user_moderation", nil, "", nil, nil
		}},
		{"transient with snapshot fields", func(s *CommandSpec) { s.Kind = "user_moderation" }},
		{"offline moderator disconnect", func(s *CommandSpec) {
			s.Kind, s.Action, s.Reason, s.Target = "disconnect", "disconnect", "moderator_request", nil
			s.AuthorizationRevision, s.ServerMuted, s.ServerDeafened = "", nil, nil
		}},
		{"offline channel membership removal", func(s *CommandSpec) {
			s.Scope, s.Kind, s.Action, s.Reason, s.Target = "channel", "disconnect", "disconnect", "membership_removed", nil
			s.AuthorizationRevision, s.ServerMuted, s.ServerDeafened = "", nil, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := outboxDatabase(t)
			spec := outboxSnapshot()
			tc.change(&spec)
			tx := outboxBegin(t, db)
			_, err := EnqueueTx(context.Background(), tx, []CommandSpec{spec})
			require.Error(t, err)
			require.NoError(t, tx.Rollback())
			assert.Zero(t, outboxCount(t, db))
			requireOutboxCursor(t, db, 0, 0)
		})
	}
	t.Run("empty batch", func(t *testing.T) {
		db, _ := outboxDatabase(t)
		tx := outboxBegin(t, db)
		_, err := EnqueueTx(context.Background(), tx, nil)
		require.Error(t, err)
		require.NoError(t, tx.Rollback())
		requireOutboxCursor(t, db, 0, 0)
	})
}

func TestVoiceOutbox_CommandOwnsItsFrozenInput(t *testing.T) {
	db, _ := outboxDatabase(t)
	spec := outboxSnapshot()
	tx := outboxBegin(t, db)
	commands, err := EnqueueTx(context.Background(), tx, []CommandSpec{spec})
	require.NoError(t, err)
	require.Len(t, commands, 1)
	*spec.ChannelID = uuid.NewString()
	spec.Target.SocketID = "changed-after-enqueue"
	*spec.ServerMuted, *spec.ServerDeafened = false, true
	require.NoError(t, tx.Commit())
	require.NotNil(t, commands[0].ChannelID)
	assert.Equal(t, "26230000-0000-4000-8000-000000000013", *commands[0].ChannelID)
	require.NotNil(t, commands[0].Target)
	assert.Equal(t, "outbox-exact-socket", commands[0].Target.SocketID)
	require.NotNil(t, commands[0].ServerMuted)
	require.NotNil(t, commands[0].ServerDeafened)
	assert.True(t, *commands[0].ServerMuted)
	assert.False(t, *commands[0].ServerDeafened)
	assert.Equal(t, commands[0], outboxStoredCommand(t, db, 1))
}

func TestVoiceOutbox_AllocationFollowsCommitOrder(t *testing.T) {
	for _, commitFirst := range []bool{true, false} {
		t.Run(strconv.FormatBool(commitFirst), func(t *testing.T) {
			db, _ := outboxDatabase(t)
			probe, err := sql.Open("postgres", testdb.DatabaseURL())
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, probe.Close()) })
			first := outboxBegin(t, db)
			var firstXID int64
			require.NoError(t, first.QueryRow(`SELECT txid_current()`).Scan(&firstXID))
			firstCommands, err := EnqueueTx(context.Background(), first, []CommandSpec{outboxSnapshot()})
			require.NoError(t, err)
			require.Len(t, firstCommands, 1)
			type result struct {
				commands []VoiceCommand
				err      error
			}
			finished := make(chan result, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			go func() {
				tx, beginErr := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
				if beginErr != nil {
					finished <- result{err: beginErr}
					return
				}
				commands, enqueueErr := EnqueueTx(ctx, tx, []CommandSpec{outboxSnapshot()})
				if enqueueErr != nil {
					finished <- result{err: errors.Join(enqueueErr, tx.Rollback())}
					return
				}
				finished <- result{commands: commands, err: tx.Commit()}
			}()
			// PostgreSQL's waiter proves the allocator reached the counter lock;
			// there is no sleep or timing window asserting that it stayed blocked.
			testdb.WaitForRowLockWaiter(t, probe, firstXID)
			assert.Zero(t, outboxCount(t, db))
			requireOutboxCursor(t, db, 0, 0)
			if commitFirst {
				require.NoError(t, first.Commit())
			} else {
				require.NoError(t, first.Rollback())
			}
			got := <-finished
			require.NoError(t, got.err)
			require.Len(t, got.commands, 1)
			wantSequence, wantCount := "1", 1
			if commitFirst {
				wantSequence, wantCount = "2", 2
			}
			assert.Equal(t, wantSequence, got.commands[0].Sequence)
			assert.Equal(t, wantCount, outboxCount(t, db))
			requireOutboxCursor(t, db, int64(wantCount), 0)
		})
	}
}

func TestVoiceOutbox_BIGINTCapacityRejectsWholeBatch(t *testing.T) {
	db, _ := outboxDatabase(t)
	// This oracle exercises BIGINT allocation, with no outstanding queue to
	// trigger the independent admission bound before the final numeric slot.
	_, err := db.Exec(`UPDATE voice_command_sequence SET last_allocated_sequence = $1,
		last_published_sequence = $1 WHERE id = TRUE`, int64(math.MaxInt64-1))
	require.NoError(t, err)
	tx := outboxBegin(t, db)
	_, err = EnqueueTx(context.Background(), tx, []CommandSpec{outboxSnapshot(), outboxSnapshot()})
	require.Error(t, err)
	require.NoError(t, tx.Rollback())
	assert.Zero(t, outboxCount(t, db))
	requireOutboxCursor(t, db, math.MaxInt64-1, math.MaxInt64-1)
	command := outboxEnqueue(t, db, outboxSnapshot())[0]
	assert.Equal(t, strconv.FormatInt(math.MaxInt64, 10), command.Sequence)
	tx = outboxBegin(t, db)
	_, err = EnqueueTx(context.Background(), tx, []CommandSpec{outboxSnapshot()})
	require.Error(t, err)
	require.NoError(t, tx.Rollback())
	assert.Equal(t, 1, outboxCount(t, db))
	requireOutboxCursor(t, db, math.MaxInt64, math.MaxInt64-1)
}

func outboxChannel(t *testing.T, db *sql.DB, ownerID string) (string, string) {
	t.Helper()
	serverID, channelID := uuid.NewString(), uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'Outbox target scope', $2)`, serverID, ownerID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'outbox-target-room', 'voice')`, channelID, serverID)
	require.NoError(t, err)
	return serverID, channelID
}

func outboxRegister(t *testing.T, db *sql.DB, scope, room, user string) RegisteredTarget {
	t.Helper()
	target := ExactTarget{SessionGeneration: uuid.NewString(), NodeBootID: uuid.NewString(),
		SocketID: "outbox-registered-" + uuid.NewString(), CredentialEpoch: ""}
	_, err := db.Exec(`INSERT INTO voice_enforcement_sessions
		(session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, target.SessionGeneration, target.NodeBootID,
		room, scope, user, target.CredentialEpoch, target.SocketID)
	require.NoError(t, err)
	return RegisteredTarget{ChannelID: room, Target: target}
}

func TestVoiceOutbox_CaptureIncludesStagedAndQualifiesServerAndDM(t *testing.T) {
	db, _ := outboxDatabase(t)
	userID := testdb.CreateUser(t, db).String()
	otherUser := testdb.CreateUser(t, db).String()
	serverID, room := outboxChannel(t, db, userID)
	_, foreignRoom := outboxChannel(t, db, userID)
	first := outboxRegister(t, db, "channel", room, userID)
	second := outboxRegister(t, db, "channel", room, userID)
	outboxRegister(t, db, "channel", foreignRoom, userID)
	outboxRegister(t, db, "channel", room, otherUser)
	dm := outboxRegister(t, db, "dm", room, userID)
	outboxRegister(t, db, "dm", foreignRoom, userID)
	var presence int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_participants WHERE user_id = $1`, userID).Scan(&presence))
	require.Zero(t, presence, "the fixture must pin registration before presence")
	tx := outboxBegin(t, db)
	byServer, err := CaptureTargetsTx(context.Background(), tx, "channel", userID, "", serverID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []RegisteredTarget{first, second}, byServer)
	byRoom, err := CaptureTargetsTx(context.Background(), tx, "channel", userID, room, serverID)
	require.NoError(t, err)
	assert.Equal(t, byServer, byRoom, "same captured evidence must keep deterministic order")
	foreign, err := CaptureTargetsTx(context.Background(), tx, "channel", userID, foreignRoom, serverID)
	require.NoError(t, err)
	assert.Empty(t, foreign, "a server filter cannot capture a foreign channel")
	byDM, err := CaptureTargetsTx(context.Background(), tx, "dm", userID, room, "")
	require.NoError(t, err)
	assert.Equal(t, []RegisteredTarget{dm}, byDM)
	_, err = CaptureTargetsTx(context.Background(), tx, "dm", userID, room, serverID)
	require.Error(t, err)
	_, err = CaptureTargetsTx(context.Background(), tx, "dm", userID, "", "")
	require.Error(t, err)
	_, err = CaptureTargetsTx(context.Background(), tx, "channel", userID, "", "")
	require.Error(t, err, "unqualified global channel capture is forbidden")
	_, err = CaptureTargetsTx(context.Background(), nil, "channel", userID, room, serverID)
	require.Error(t, err)
}

func TestVoiceOutbox_OversizedRegisteredSocketCannotBeTruncatedOrOmitted(t *testing.T) {
	db, _ := outboxDatabase(t)
	userID, room := uuid.NewString(), uuid.NewString()
	target := outboxExactTarget()
	_, err := db.Exec(`INSERT INTO voice_enforcement_sessions
		(session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
		VALUES ($1, $2, $3, 'dm', $4, '', $5)`, target.SessionGeneration, target.NodeBootID,
		room, userID, strings.Repeat("x", 255))
	require.NoError(t, err, "the existing registry admits up to 255 socket bytes")
	tx := outboxBegin(t, db)
	registered, captureErr := CaptureTargetsTx(context.Background(), tx, "dm", userID, room, "")
	if captureErr == nil {
		require.Len(t, registered, 1, "invalid evidence cannot silently become an offline marker")
		assert.Equal(t, strings.Repeat("x", 255), registered[0].Target.SocketID)
		spec := outboxSnapshot()
		spec.UserID, spec.ChannelID, spec.Target = userID, outboxString(room), &registered[0].Target
		_, err = EnqueueTx(context.Background(), tx, []CommandSpec{spec})
		require.Error(t, err, "the immutable v1 target cannot admit or truncate legacy evidence")
	}
	require.NoError(t, tx.Rollback())
	assert.Zero(t, outboxCount(t, db))
	requireOutboxCursor(t, db, 0, 0)
}

func TestVoiceOutbox_TypedRowsRemainImmutableAndSurviveDomainDeletion(t *testing.T) {
	db, _ := outboxDatabase(t)
	userID := testdb.CreateUser(t, db).String()
	serverID, room := outboxChannel(t, db, userID)
	spec := outboxSnapshot()
	spec.Scope, spec.UserID, spec.ServerID, spec.ChannelID = "channel", userID, outboxString(serverID), outboxString(room)
	command := outboxEnqueue(t, db, spec)[0]
	for _, statement := range []string{
		`UPDATE voice_command_outbox SET sequence = 2 WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET command_id = gen_random_uuid() WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET stream_generation = gen_random_uuid() WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET envelope_version = 2 WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET scope = 'dm' WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET kind = 'disconnect' WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET action = 'unmute' WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET channel_id = gen_random_uuid() WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET server_id = gen_random_uuid() WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET user_id = gen_random_uuid() WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET session_generation = gen_random_uuid() WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET node_boot_id = gen_random_uuid() WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET socket_id = 'successor-socket' WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET credential_epoch = '' WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET reason = 'membership_removed' WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET authorization_revision = 5 WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET server_muted = FALSE WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET server_deafened = TRUE WHERE sequence = 1`,
		`UPDATE voice_command_outbox SET created_at = created_at + interval '1 second' WHERE sequence = 1`,
	} {
		_, err := db.Exec(statement)
		require.Error(t, err, "semantic SQL evidence cannot be rewritten")
		assert.Equal(t, command, outboxStoredCommand(t, db, 1))
	}
	_, err := db.Exec(`UPDATE voice_command_outbox SET attempts = 1, retry_at = clock_timestamp() WHERE sequence = 1`)
	require.NoError(t, err, "delivery metadata remains mutable")
	_, err = db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	require.NoError(t, err)
	assert.Equal(t, command, outboxStoredCommand(t, db, 1), "account/room cascades cannot erase accepted work")
	assert.Equal(t, 1, outboxCount(t, db))
	requireOutboxCursor(t, db, 1, 0)
}
