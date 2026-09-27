package voice_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitForVoiceParticipantUserLock(t *testing.T, db *sql.DB) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND wait_event_type = 'Lock'
				  AND query LIKE '%SELECT id FROM users WHERE id = ANY%'
				  AND (query LIKE '%FOR SHARE%' OR query LIKE '%FOR NO KEY UPDATE%')
		)`).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 10*time.Millisecond,
		"voice participant mutation must wait on users before its domain lock")
}

func waitForServerVoiceCapacityLockWaiters(t *testing.T, db *sql.DB) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int
		err := db.QueryRow(`
			SELECT count(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%SELECT id FROM channels WHERE id = $1 FOR UPDATE%'`).Scan(&waiting)
		return err == nil && waiting >= 2
	}, 3*time.Second, 10*time.Millisecond,
		"both capacity checks must be parked on the channel row lock before release")
}

func rollbackVoiceLockOrderBlocker(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("rollback voice lock-order blocker: %v", err)
	}
}

func TestUpsertServerVoiceParticipantLocksUserBeforeChannel(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	owner := ts.CreateTestUser(t, "voice_user_lock_server_owner")
	sender := ts.CreateTestUser(t, "voice_user_lock_server_sender")
	serverID := ts.CreateTestServer(t, owner.ID, "Voice User Lock Server")
	ts.AddMemberToServer(t, serverID, sender.ID, "member")
	channelID := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "voice-user-lock-server"))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	blocker, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { rollbackVoiceLockOrderBlocker(t, blocker) })
	var lockedUser uuid.UUID
	require.NoError(t, blocker.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, sender.ID,
	).Scan(&lockedUser))

	done := make(chan error, 1)
	go func() {
		_, upsertErr := sub.UpsertServerVoiceParticipantForTest(
			ctx, channelID, uuid.MustParse(sender.ID), time.Now().UTC(),
		)
		done <- upsertErr
	}()
	waitForVoiceParticipantUserLock(t, ts.DB)

	require.NoError(t, func() error {
		if _, err := blocker.ExecContext(ctx, `SET LOCAL lock_timeout = '250ms'`); err != nil {
			return err
		}
		var lockedChannel uuid.UUID
		return blocker.QueryRowContext(ctx,
			`SELECT id FROM channels WHERE id = $1 FOR UPDATE`, channelID,
		).Scan(&lockedChannel)
	}(), "server join must not hold the channel while waiting on users")
	require.NoError(t, blocker.Commit())

	select {
	case upsertErr := <-done:
		require.NoError(t, upsertErr)
	case <-ctx.Done():
		t.Fatal("server voice join did not complete after the users/channel holder released")
	}
}

func TestUpsertPrivateVoiceParticipantLocksUserBeforeConversation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "voice_user_lock_private_sender")
	peer := ts.CreateTestUser(t, "voice_user_lock_private_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, sender.ID, peer.ID))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	blocker, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { rollbackVoiceLockOrderBlocker(t, blocker) })
	var lockedUser uuid.UUID
	require.NoError(t, blocker.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, sender.ID,
	).Scan(&lockedUser))

	done := make(chan error, 1)
	go func() {
		_, upsertErr := sub.UpsertPrivateVoiceParticipantForTest(
			ctx, conversationID, uuid.MustParse(sender.ID), uuid.New(), time.Now().UTC(),
		)
		done <- upsertErr
	}()
	waitForVoiceParticipantUserLock(t, ts.DB)

	require.NoError(t, func() error {
		if _, err := blocker.ExecContext(ctx, `SET LOCAL lock_timeout = '250ms'`); err != nil {
			return err
		}
		var lockedConversation uuid.UUID
		return blocker.QueryRowContext(ctx,
			`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, conversationID,
		).Scan(&lockedConversation)
	}(), "private join must not hold the conversation while waiting on users")
	require.NoError(t, blocker.Commit())

	select {
	case upsertErr := <-done:
		require.NoError(t, upsertErr)
	case <-ctx.Done():
		t.Fatal("private voice join did not complete after the users/conversation holder released")
	}
}

func TestDMHeartbeatLocksUsersBeforeConversation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "voice_user_lock_heartbeat_caller")
	peer := ts.CreateTestUser(t, "voice_user_lock_heartbeat_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	blocker, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { rollbackVoiceLockOrderBlocker(t, blocker) })
	var lockedUser uuid.UUID
	require.NoError(t, blocker.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, caller.ID,
	).Scan(&lockedUser))

	done := make(chan struct{})
	callID := uuid.New()
	go func() {
		defer close(done)
		sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
			"channelId": conversationID.String(), "callId": callID.String(),
			"callerUserId": caller.ID, "userIds": []string{caller.ID, peer.ID},
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		}))
	}()
	waitForVoiceParticipantUserLock(t, ts.DB)

	require.NoError(t, func() error {
		if _, err := blocker.ExecContext(ctx, `SET LOCAL lock_timeout = '250ms'`); err != nil {
			return err
		}
		var lockedConversation uuid.UUID
		return blocker.QueryRowContext(ctx,
			`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, conversationID,
		).Scan(&lockedConversation)
	}(), "heartbeat must not hold the conversation while waiting on users")
	require.NoError(t, blocker.Commit())

	select {
	case <-done:
		require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID))
	case <-ctx.Done():
		t.Fatal("private heartbeat did not complete after the users/conversation holder released")
	}
}

func TestUpsertPrivateVoiceParticipantRejectsBlockedOrPendingGroupMember(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *testhelpers.TestServer, string, string)
	}{
		{
			name: "blocked",
			prepare: func(t *testing.T, ts *testhelpers.TestServer, senderID, memberID string) {
				ts.CreateFriendship(t, senderID, memberID, "blocked")
			},
		},
		{
			name: "pending reconciliation",
			prepare: func(t *testing.T, ts *testhelpers.TestServer, senderID, memberID string) {
				var hasOperationID bool
				require.NoError(t, ts.DB.QueryRow(`
					SELECT EXISTS (
						SELECT 1 FROM information_schema.columns
						WHERE table_schema = 'public' AND table_name = 'dm_block_reconciliations'
						  AND column_name = 'operation_id'
					)
				`).Scan(&hasOperationID))
				if !hasOperationID {
					t.Skip("pending-reconciliation assertion requires the current 000130 migration")
				}
				_, err := ts.DB.Exec(`
					INSERT INTO dm_block_reconciliations
						(user_a_id, user_b_id, operation_id, remove_a)
					VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE)
				`, senderID, memberID, uuid.New())
				require.NoError(t, err)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriber(ts)
			sender := ts.CreateTestUser(t, "voice_group_guard_sender_"+strings.ReplaceAll(test.name, " ", "_"))
			peer := ts.CreateTestUser(t, "voice_group_guard_peer_"+strings.ReplaceAll(test.name, " ", "_"))
			member := ts.CreateTestUser(t, "voice_group_guard_member_"+strings.ReplaceAll(test.name, " ", "_"))
			conversationID := uuid.MustParse(ts.CreateDMConversation(t, sender.ID, peer.ID))
			_, err := ts.DB.Exec(`UPDATE dm_conversations SET is_group = TRUE WHERE id = $1`, conversationID)
			require.NoError(t, err)
			_, err = ts.DB.Exec(`
				INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)
			`, conversationID, member.ID)
			require.NoError(t, err)
			test.prepare(t, ts, sender.ID, member.ID)

			applied, err := sub.UpsertPrivateVoiceParticipantForTest(
				context.Background(), conversationID, uuid.MustParse(sender.ID), uuid.New(), time.Now().UTC(),
			)
			require.ErrorIs(t, err, dmblock.ErrUnavailable)
			assert.False(t, applied)
			assert.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), sender.ID))
		})
	}
}

func TestHandleJoined_BlockedGroupMemberEjectsOnlyJoiningPeer(t *testing.T) {
	publisher, err := natsclient.Connect(natsTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping Private Call block ejection test", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	observer, err := natsclient.Connect(natsTestURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = observer.Close() })
	disconnects := make(chan map[string]interface{}, 2)
	subscription, err := observer.Subscribe(natsSubjectEnforceDisconnectForTest, func(data []byte) {
		var payload map[string]interface{}
		if json.Unmarshal(data, &payload) == nil {
			disconnects <- payload
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	require.NoError(t, observer.Flush())

	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriberWithHubAndNATS(ts, ts.Hub, publisher)
	caller := ts.CreateTestUser(t, "voice_block_eject_caller")
	peer := ts.CreateTestUser(t, "voice_block_eject_peer")
	blockedMember := ts.CreateTestUser(t, "voice_block_eject_member")
	conversationID := uuid.MustParse(ts.CreateGroupDMConversation(t, caller.ID, peer.ID, blockedMember.ID))
	callID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis,
		dm.VoiceCallLease{ConversationID: conversationID, CallID: callID, CallerUserID: uuid.MustParse(caller.ID)},
		dm.DMVoiceCallLeaseTTL, true,
	))
	ts.CreateFriendship(t, caller.ID, blockedMember.ID, "blocked")

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(),
		"callId":    callID.String(),
		"userId":    caller.ID,
		"username":  caller.Username,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}))

	select {
	case payload := <-disconnects:
		assert.Equal(t, conversationID.String(), payload["channelId"])
		assert.Equal(t, caller.ID, payload["userId"])
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Private Call join did not eject the joining media peer")
	}
	assert.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID))
}

func TestHandleJoined_PreCommitLeaseFailureEjectsJoiningPeer(t *testing.T) {
	publisher, err := natsclient.Connect(natsTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping Private Call pre-commit ejection test", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	observer, err := natsclient.Connect(natsTestURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = observer.Close() })
	disconnects := make(chan map[string]interface{}, 1)
	subscription, err := observer.Subscribe(natsSubjectEnforceDisconnectForTest, func(data []byte) {
		var payload map[string]interface{}
		if json.Unmarshal(data, &payload) == nil {
			disconnects <- payload
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	require.NoError(t, observer.Flush())

	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriberWithHubAndNATS(ts, ts.Hub, publisher)
	caller := ts.CreateTestUser(t, "voice_precommit_eject_caller")
	peer := ts.CreateTestUser(t, "voice_precommit_eject_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis,
		dm.VoiceCallLease{ConversationID: conversationID, CallID: callID, CallerUserID: uuid.MustParse(caller.ID)},
		dm.DMVoiceCallLeaseTTL, true,
	))
	sub.SetPrivateJoinHooksForTest(func(conversation, sender uuid.UUID) {
		require.Equal(t, conversationID, conversation)
		require.Equal(t, uuid.MustParse(caller.ID), sender)
		require.NoError(t, dm.DeleteDMVoiceCallLease(context.Background(), ts.Redis, conversationID, callID))
	}, nil)

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(),
		"callId":    callID.String(),
		"userId":    caller.ID,
		"username":  caller.Username,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}))

	select {
	case payload := <-disconnects:
		assert.Equal(t, conversationID.String(), payload["channelId"])
		assert.Equal(t, caller.ID, payload["userId"])
	case <-time.After(2 * time.Second):
		t.Fatal("pre-commit Private Call failure did not eject the joining media peer")
	}
	assert.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID))
}

func TestHandleJoined_RejectsMismatchedOrExpiredPendingAdmission(t *testing.T) {
	publisher, err := natsclient.Connect(natsTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping pending-admission rejection test", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	observer, err := natsclient.Connect(natsTestURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = observer.Close() })
	disconnects := make(chan map[string]interface{}, 2)
	subscription, err := observer.Subscribe(natsSubjectEnforceDisconnectForTest, func(data []byte) {
		var payload map[string]interface{}
		if json.Unmarshal(data, &payload) == nil {
			disconnects <- payload
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	require.NoError(t, observer.Flush())

	for _, testCase := range []struct {
		name      string
		expiresIn string
		admission uuid.UUID
		socketID  string
	}{
		{
			name:      "replaced exact tuple",
			expiresIn: "30 seconds",
			admission: uuid.New(),
			socketID:  "replacement-socket",
		},
		{
			name:      "expired exact tuple",
			expiresIn: "-1 second",
			admission: uuid.Nil,
			socketID:  "expired-socket",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriberWithHubAndNATS(ts, ts.Hub, publisher)
			owner := ts.CreateTestUser(t, "pending_admission_owner_"+strings.ReplaceAll(testCase.name, " ", "_"))
			candidate := ts.CreateTestUser(t, "pending_admission_candidate_"+strings.ReplaceAll(testCase.name, " ", "_"))
			serverID := ts.CreateTestServer(t, owner.ID, "Pending Admission Server")
			ts.AddMemberToServer(t, serverID, candidate.ID, "member")
			channelID := ts.CreateVoiceChannel(t, serverID, "pending-admission-voice")
			admissionID := uuid.New()
			socketID := "current-socket"
			if testCase.admission != uuid.Nil {
				admissionID = testCase.admission
				socketID = testCase.socketID
			}
			_, err := ts.DB.Exec(`
				INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
				VALUES ($1, $2, $3, $4, clock_timestamp() + $5::interval)
			`, channelID, candidate.ID, admissionID, socketID, testCase.expiresIn)
			require.NoError(t, err)

			eventAdmissionID, eventSocketID := admissionID, socketID
			if testCase.admission != uuid.Nil {
				eventAdmissionID, eventSocketID = uuid.New(), "stale-socket"
			}
			sub.HandleJoined(mustJSON(t, map[string]interface{}{
				"channelId": channelID, "userId": candidate.ID, "username": candidate.Username,
				"admissionId": eventAdmissionID.String(), "socketId": eventSocketID,
				"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			}))

			select {
			case payload := <-disconnects:
				assert.Equal(t, channelID, payload["channelId"])
				assert.Equal(t, candidate.ID, payload["userId"])
				assert.Equal(t, eventSocketID, payload["socketId"])
				assert.Equal(t, eventAdmissionID.String(), payload["admissionId"])
			case <-time.After(2 * time.Second):
				t.Fatal("invalid pending admission did not eject the joining media peer")
			}
			assert.False(t, voiceParticipantExists(t, ts.DB, channelID, candidate.ID))
		})
	}
}

func TestUpsertServerVoiceParticipant_ConcurrentJoinsAtCapacityCannotOverflow(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	left := newTestSubscriber(ts)
	right := newTestSubscriber(ts)
	owner := ts.CreateTestUser(t, "server_cap_owner")
	first := ts.CreateTestUser(t, "server_cap_first")
	second := ts.CreateTestUser(t, "server_cap_second")
	serverID := ts.CreateTestServer(t, owner.ID, "Server Capacity")
	channelID := ts.CreateVoiceChannel(t, serverID, "server-capacity-channel")
	prefix := "servercap_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	_, err := ts.DB.Exec(`
		INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		SELECT gen_random_uuid(), $1 || '_' || series || '@test.concord.chat', $1 || '_' || series,
		       'server-capacity-hash', TRUE, TRUE
		FROM generate_series(1, 999) AS series`, prefix)
	require.NoError(t, err)
	joinedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	_, err = ts.DB.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		SELECT $1, id, $3, $3 FROM users WHERE LEFT(username, LENGTH($2)) = $2`,
		channelID, prefix, joinedAt)
	require.NoError(t, err)
	require.Equal(t, 999, countVoiceParticipants(t, ts.DB, channelID))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	blocker, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { rollbackVoiceLockOrderBlocker(t, blocker) })
	var lockedChannel uuid.UUID
	require.NoError(t, blocker.QueryRowContext(ctx,
		`SELECT id FROM channels WHERE id = $1 FOR UPDATE`, channelID,
	).Scan(&lockedChannel))

	start := make(chan struct{})
	type result struct {
		userID  string
		applied bool
		err     error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		value, upsertErr := left.UpsertServerVoiceParticipantForTest(
			ctx, uuid.MustParse(channelID), uuid.MustParse(first.ID), joinedAt.Add(time.Second),
		)
		results <- result{userID: first.ID, applied: value.Applied, err: upsertErr}
	}()
	go func() {
		defer group.Done()
		<-start
		value, upsertErr := right.UpsertServerVoiceParticipantForTest(
			ctx, uuid.MustParse(channelID), uuid.MustParse(second.ID), joinedAt.Add(time.Second),
		)
		results <- result{userID: second.ID, applied: value.Applied, err: upsertErr}
	}()
	close(start)
	waitForServerVoiceCapacityLockWaiters(t, ts.DB)
	require.NoError(t, blocker.Commit())
	group.Wait()
	close(results)

	applied := 0
	errorsReturned := 0
	var winner string
	var loser string
	for value := range results {
		if value.err != nil {
			errorsReturned++
			loser = value.userID
			assert.Contains(t, value.err.Error(), "participant limit reached")
			assert.False(t, value.applied)
			continue
		}
		if value.applied {
			applied++
			winner = value.userID
		}
	}
	require.Equal(t, 1, applied, "one of the overlapping joins must fill the final capacity slot")
	require.Equal(t, 1, errorsReturned, "the other overlapping join must hit the capacity limit")
	require.True(t, voiceParticipantExists(t, ts.DB, channelID, winner), "the successful join must persist")
	require.False(t, voiceParticipantExists(t, ts.DB, channelID, loser), "the rejected join must not persist")
	require.Equal(t, 1000, countVoiceParticipants(t, ts.DB, channelID), "capacity remains exactly full")
}

func TestHandleJoined_FullRoomDoesNotDisconnectRichPresenceFleet(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	owner := ts.CreateTestUser(t, "full_room_owner")
	candidate := ts.CreateTestUser(t, "full_room_candidate")
	serverID := ts.CreateTestServer(t, owner.ID, "Full Room Server")
	ts.AddMemberToServer(t, serverID, candidate.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "full-room-channel")
	prefix := "fullroom_" + strings.ReplaceAll(candidate.ID, "-", "")[:8]
	_, err := ts.DB.Exec(`
		INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		SELECT gen_random_uuid(), $1 || '_' || series || '@test.concord.chat', $1 || '_' || series,
		       'full-room-hash', TRUE, TRUE
		FROM generate_series(1, 1000) AS series`, prefix)
	require.NoError(t, err)
	joinedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	_, err = ts.DB.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		SELECT $1, id, $3, $3 FROM users WHERE LEFT(username, LENGTH($2)) = $2`,
		channelID, prefix, joinedAt)
	require.NoError(t, err)
	require.Equal(t, 1000, countVoiceParticipants(t, ts.DB, channelID))

	var disconnects atomic.Int32
	sub.SetDisconnectAllRichPresenceClientsHookForTest(func() { disconnects.Add(1) })
	for index := 0; index < 3; index++ {
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": channelID, "userId": candidate.ID,
			"username":  candidate.Username,
			"timestamp": joinedAt.Add(time.Duration(index+1) * time.Second).Format(time.RFC3339Nano),
		}))
	}

	require.Zero(t, disconnects.Load(), "ordinary full-room joins must not disconnect unrelated Rich Presence clients")
	require.Equal(t, 1000, countVoiceParticipants(t, ts.DB, channelID), "full room roster remains bounded")
}
