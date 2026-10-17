package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/roomplacement"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestGuardLifecycleEventRequiresD8FieldsAndHonorsNonAuthoritativeHeartbeat(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	subscriber := NewNATSSubscriber(nil, logger.New("test"), nil, nil, client, nil, nil)
	subscriber.SetLifecyclePlacementEnabled(true)

	require.Equal(t, roomplacement.LifecycleRefuse, subscriber.guardLifecycleEvent(natsSubjectVoiceJoined, []byte(`{"channelId":"550e8400-e29b-41d4-a716-446655440000"}`)))
	roomID := "550e8400-e29b-41d4-a716-446655440000"
	userID := "550e8400-e29b-41d4-a716-446655440001"
	require.NoError(t, server.Set("voice:room_owner:"+roomID, "blue"))
	server.SetTTL("voice:room_owner:"+roomID, 5*time.Second)
	require.NoError(t, server.Set("voice:room_live:"+roomID, "blue-42"))
	frame, err := json.Marshal(map[string]any{
		"channelId": roomID, "colour": "blue", "instance_id": "blue-42",
		"participants_authoritative": false,
	})
	require.NoError(t, err)
	require.Equal(t, roomplacement.LifecycleRefuse, subscriber.guardLifecycleEvent(natsSubjectVoiceHeartbeat, frame))
	require.Equal(t, 90*time.Second, server.TTL("voice:room_owner:"+roomID),
		"a non-authoritative heartbeat renews placement but does not reconcile participant state")
	server.SetTTL("voice:room_owner:"+roomID, 5*time.Second)
	frame, err = json.Marshal(map[string]any{
		"channelId": roomID, "colour": "blue", "instance_id": "blue-42",
	})
	require.NoError(t, err)
	require.Equal(t, roomplacement.LifecycleRefuse, subscriber.guardLifecycleEvent(natsSubjectVoiceHeartbeat, frame))
	require.Equal(t, 90*time.Second, server.TTL("voice:room_owner:"+roomID),
		"a legacy heartbeat without the authority flag still renews its owner lease")

	// An owner reservation with no live instance has refresh-only authority.
	// One minted intent permits only the matching first joined insert.
	server.Del("voice:room_live:" + roomID)
	require.NoError(t, subscriber.roomPlacement.MintJoinIntent(
		t.Context(), roomID, userID,
	))
	firstJoin, err := json.Marshal(map[string]any{
		"channelId": roomID, "userId": userID, "colour": "blue", "instance_id": "blue-42",
	})
	require.NoError(t, err)
	require.Equal(t, roomplacement.LifecycleRefreshOnly, subscriber.guardLifecycleEvent(natsSubjectVoiceJoined, firstJoin))

	// Once the matching live instance is published, subsequent lifecycle frames
	// receive full authority. A stale generation remains fenced.
	require.NoError(t, server.Set("voice:room_live:"+roomID, "blue-42"))
	fullJoin, err := json.Marshal(map[string]any{
		"channelId": roomID, "userId": userID, "colour": "blue", "instance_id": "blue-42",
	})
	require.NoError(t, err)
	require.Equal(t, roomplacement.LifecycleFull, subscriber.guardLifecycleEvent(natsSubjectVoiceJoined, fullJoin))
	stale, err := json.Marshal(map[string]any{
		"channelId": roomID, "userId": userID, "colour": "blue", "instance_id": "blue-41",
	})
	require.NoError(t, err)
	require.Equal(t, roomplacement.LifecycleRefreshOnly, subscriber.guardLifecycleEvent(natsSubjectVoiceLeft, stale))

	frame, err = json.Marshal(map[string]any{
		"channelId": roomID, "colour": "blue", "instance_id": "blue-42",
	})
	require.NoError(t, err)
	require.Equal(t, roomplacement.LifecycleFull, subscriber.guardLifecycleEvent(natsSubjectVoiceJoined, frame))
}

func TestRefreshOnlyTerminalEventsNeverEnterFullHandlers(t *testing.T) {
	for _, subject := range []string{natsSubjectVoiceLeft, natsSubjectVoiceRoomEmpty} {
		t.Run(subject, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			subscriber := NewNATSSubscriber(nil, logger.New("test"), nil, nil, client, nil, nil)
			subscriber.SetLifecyclePlacementEnabled(true)
			roomID := "550e8400-e29b-41d4-a716-446655440010"
			require.NoError(t, server.Set("voice:room_owner:"+roomID, "blue"))
			server.SetTTL("voice:room_owner:"+roomID, 90*time.Second)
			require.NoError(t, server.Set("voice:room_live:"+roomID, "blue-43"))
			now := time.Now().UTC()
			frame, err := json.Marshal(map[string]any{
				"channelId": roomID,
				"userId":    "550e8400-e29b-41d4-a716-446655440011",
				"colour":    "blue", "instance_id": "blue-42",
				"timestamp": now.Format(time.RFC3339Nano),
			})
			require.NoError(t, err)

			// A full handler would dereference the deliberately absent database.
			subscriber.handleVoiceLifecycleEvent(subject, frame, now)
			wantTTL := 90 * time.Second
			if subject == natsSubjectVoiceRoomEmpty {
				wantTTL = 45 * time.Second
			}
			require.Equal(t, wantTTL, server.TTL("voice:room_owner:"+roomID))
		})
	}
}

func refreshOnlyTestSubscriber(t *testing.T) (*NATSSubscriber, *sql.DB, *miniredis.Miniredis, uuid.UUID, uuid.UUID) {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	userID := testdb.CreateUser(t, db)
	serverID := uuid.New()
	channelID := uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, owner_id, name) VALUES ($1, $2, 'test')`, serverID, userID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id) VALUES ($1, $2)`, serverID, userID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'test', 'voice')`, channelID, serverID)
	require.NoError(t, err)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	subscriber := NewNATSSubscriber(db, logger.New("test"), nil, nil, client, nil, nil)
	subscriber.SetLifecyclePlacementEnabled(true)
	require.NoError(t, server.Set("voice:room_owner:"+channelID.String(), "blue"))
	return subscriber, db, server, channelID, userID
}

func TestRefreshOnlyJoinCannotRemoveAnotherChannel(t *testing.T) {
	subscriber, db, server, target, userID := refreshOnlyTestSubscriber(t)
	var serverID uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, target).Scan(&serverID))
	oldRoom := uuid.New()
	_, err := db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'old', 'voice')`, oldRoom, serverID)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err = db.Exec(`INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at) VALUES ($1, $2, $3)`, oldRoom, userID, now.Add(-time.Second))
	require.NoError(t, err)
	admissionID := uuid.New()
	_, err = db.Exec(`INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at) VALUES ($1, $2, $3, 'socket-test', clock_timestamp() + INTERVAL '1 minute')`, target, userID, admissionID)
	require.NoError(t, err)
	require.NoError(t, subscriber.roomPlacement.MintJoinIntent(t.Context(), target.String(), userID.String()))
	frame, err := json.Marshal(map[string]any{
		"channelId": target.String(), "userId": userID.String(),
		"colour": "blue", "instance_id": "blue-42",
		"admissionId": admissionID.String(), "socketId": "socket-test",
		"timestamp": now.Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	subscriber.handleVoiceLifecycleEvent(natsSubjectVoiceJoined, frame, now)
	var oldCount, targetCount, pendingCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, oldRoom, userID).Scan(&oldCount))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&targetCount))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_pending_admissions WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&pendingCount))
	require.Equal(t, 1, oldCount)
	require.Zero(t, targetCount)
	require.Equal(t, 1, pendingCount)
	require.True(t, server.Exists("voice:room_owner:"+target.String()))
}

func TestRefreshOnlyJoinConsumesExactIntentAndAdmission(t *testing.T) {
	subscriber, db, _, target, userID := refreshOnlyTestSubscriber(t)
	admissionID := uuid.New()
	_, err := db.Exec(`INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at) VALUES ($1, $2, $3, 'socket-test', clock_timestamp() + INTERVAL '1 minute')`, target, userID, admissionID)
	require.NoError(t, err)
	require.NoError(t, subscriber.roomPlacement.MintJoinIntent(t.Context(), target.String(), userID.String()))
	now := time.Now().UTC().Truncate(time.Microsecond)
	frame, err := json.Marshal(map[string]any{
		"channelId": target.String(), "userId": userID.String(),
		"colour": "blue", "instance_id": "blue-42",
		"admissionId": admissionID.String(), "socketId": "socket-test",
		"timestamp": now.Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	subscriber.handleVoiceLifecycleEvent(natsSubjectVoiceJoined, frame, now)
	var joinedAt time.Time
	require.NoError(t, db.QueryRow(`SELECT lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&joinedAt))
	require.WithinDuration(t, now, joinedAt, time.Microsecond)
	var pendingCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_pending_admissions WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&pendingCount))
	require.Zero(t, pendingCount)
	admitted, err := subscriber.roomPlacement.ConsumeJoinIntent(context.Background(), target.String(), userID.String())
	require.NoError(t, err)
	require.False(t, admitted)
}

func TestRefreshOnlyJoinSpendsIntentAfterDatabaseRollback(t *testing.T) {
	subscriber, db, server, target, userID := refreshOnlyTestSubscriber(t)
	admissionID := uuid.New()
	_, err := db.Exec(`INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at) VALUES ($1, $2, $3, 'socket-test', clock_timestamp() + INTERVAL '1 minute')`, target, userID, admissionID)
	require.NoError(t, err)
	require.NoError(t, subscriber.roomPlacement.MintJoinIntent(t.Context(), target.String(), userID.String()))
	_, err = db.Exec(`
		CREATE FUNCTION test_reject_refresh_only_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected_refresh_only_failure'; END; $$
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, cleanupErr := db.Exec(`DROP TRIGGER IF EXISTS test_reject_refresh_only_insert ON voice_participants`); cleanupErr != nil {
			t.Errorf("drop refresh-only test trigger: %v", cleanupErr)
		}
		if _, cleanupErr := db.Exec(`DROP FUNCTION IF EXISTS test_reject_refresh_only_insert()`); cleanupErr != nil {
			t.Errorf("drop refresh-only test function: %v", cleanupErr)
		}
	})
	_, err = db.Exec(`
		CREATE TRIGGER test_reject_refresh_only_insert BEFORE INSERT ON voice_participants
		FOR EACH ROW EXECUTE FUNCTION test_reject_refresh_only_insert()
	`)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	admission := &voiceJoinedAdmission{admissionID: admissionID, socketID: "socket-test"}
	err = subscriber.refreshOnlyJoined(t.Context(), target, userID, now, admission)
	require.ErrorContains(t, err, "injected_refresh_only_failure")
	intentKey := "voice:join_intent:" + target.String() + ":" + userID.String()
	require.False(t, server.Exists(intentKey), "failed insert still spends the one-use intent")
	var participantCount, pendingCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&participantCount))
	require.Zero(t, participantCount)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_pending_admissions WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&pendingCount))
	require.Equal(t, 1, pendingCount)
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = "nats://localhost:4222"
	}
	publisher, err := natsclient.Connect(url)
	if err != nil {
		t.Skipf("NATS unavailable for exact disconnect check: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	observer, err := natsclient.Connect(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = observer.Close() })
	disconnects := make(chan map[string]string, 1)
	subscription, err := observer.Subscribe(natsSubjectEnforceDisconnect, func(data []byte) {
		var payload map[string]string
		if json.Unmarshal(data, &payload) == nil {
			disconnects <- payload
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	require.NoError(t, observer.Flush())
	subscriber.nats = publisher
	require.NoError(t, subscriber.roomPlacement.MintJoinIntent(t.Context(), target.String(), userID.String()))
	frame, err := json.Marshal(map[string]any{
		"channelId": target.String(), "userId": userID.String(),
		"colour": "blue", "instance_id": "blue-42",
		"admissionId": admissionID.String(), "socketId": "socket-test",
		"timestamp": now.Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	subscriber.handleVoiceLifecycleEvent(natsSubjectVoiceJoined, frame, now)
	select {
	case payload := <-disconnects:
		require.Equal(t, target.String(), payload["channelId"])
		require.Equal(t, userID.String(), payload["userId"])
		require.Equal(t, "socket-test", payload["socketId"])
		require.Equal(t, admissionID.String(), payload["admissionId"])
	case <-time.After(2 * time.Second):
		t.Fatal("post-consumption write failure did not eject exact media socket")
	}
	require.False(t, server.Exists(intentKey))
	_, err = db.Exec(`DROP TRIGGER test_reject_refresh_only_insert ON voice_participants`)
	require.NoError(t, err)
	require.NoError(t, subscriber.roomPlacement.MintJoinIntent(t.Context(), target.String(), userID.String()),
		"a fresh client authorization may mint a new intent")
	err = subscriber.refreshOnlyJoined(t.Context(), target, userID, now, admission)
	require.NoError(t, err)
	require.False(t, server.Exists(intentKey))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&participantCount))
	require.Equal(t, 1, participantCount)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_pending_admissions WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&pendingCount))
	require.Zero(t, pendingCount)
}

func TestRefreshOnlyJoinExistingRowConsumesExactAdmission(t *testing.T) {
	for _, match := range []bool{true, false} {
		name := "mismatch_rolls_back"
		if match {
			name = "match_commits"
		}
		t.Run(name, func(t *testing.T) {
			subscriber, db, _, target, userID := refreshOnlyTestSubscriber(t)
			oldTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
			_, err := db.Exec(`INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at) VALUES ($1, $2, $3)`, target, userID, oldTime)
			require.NoError(t, err)
			admissionID := uuid.New()
			_, err = db.Exec(`INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at) VALUES ($1, $2, $3, 'socket-test', clock_timestamp() + INTERVAL '1 minute')`, target, userID, admissionID)
			require.NoError(t, err)
			eventAdmissionID := admissionID
			if !match {
				eventAdmissionID = uuid.New()
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			frame, err := json.Marshal(map[string]any{
				"channelId": target.String(), "userId": userID.String(),
				"colour": "blue", "instance_id": "blue-42",
				"admissionId": eventAdmissionID.String(), "socketId": "socket-test",
				"timestamp": now.Format(time.RFC3339Nano),
			})
			require.NoError(t, err)
			subscriber.handleVoiceLifecycleEvent(natsSubjectVoiceJoined, frame, now)
			var watermark time.Time
			require.NoError(t, db.QueryRow(`SELECT lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&watermark))
			var pendingCount int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_pending_admissions WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&pendingCount))
			if match {
				require.Equal(t, now, watermark)
				require.Zero(t, pendingCount)
			} else {
				require.Equal(t, oldTime, watermark)
				require.Equal(t, 1, pendingCount)
			}
		})
	}
}

func TestRefreshOnlyJoinRejectsMismatchedAdmissionAtExactSocket(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = "nats://localhost:4222"
	}
	publisher, err := natsclient.Connect(url)
	if err != nil {
		t.Skipf("NATS unavailable: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	observer, err := natsclient.Connect(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = observer.Close() })
	disconnects := make(chan map[string]string, 1)
	subscription, err := observer.Subscribe(natsSubjectEnforceDisconnect, func(data []byte) {
		var payload map[string]string
		if json.Unmarshal(data, &payload) == nil {
			disconnects <- payload
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	require.NoError(t, observer.Flush())

	subscriber, db, _, target, userID := refreshOnlyTestSubscriber(t)
	subscriber.nats = publisher
	oldTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	_, err = db.Exec(`INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at) VALUES ($1, $2, $3)`, target, userID, oldTime)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at) VALUES ($1, $2, $3, 'actual-socket', clock_timestamp() + INTERVAL '1 minute')`, target, userID, uuid.New())
	require.NoError(t, err)
	staleAdmission := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	frame, err := json.Marshal(map[string]any{
		"channelId": target.String(), "userId": userID.String(),
		"colour": "blue", "instance_id": "blue-42",
		"admissionId": staleAdmission.String(), "socketId": "stale-socket",
		"timestamp": now.Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	subscriber.handleVoiceLifecycleEvent(natsSubjectVoiceJoined, frame, now)
	select {
	case payload := <-disconnects:
		require.Equal(t, target.String(), payload["channelId"])
		require.Equal(t, userID.String(), payload["userId"])
		require.Equal(t, "stale-socket", payload["socketId"])
		require.Equal(t, staleAdmission.String(), payload["admissionId"])
	case <-time.After(2 * time.Second):
		t.Fatal("refresh-only pending-admission mismatch did not eject stale socket")
	}
	var watermark time.Time
	require.NoError(t, db.QueryRow(`SELECT lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&watermark))
	require.Equal(t, oldTime, watermark)
}

func TestRefreshOnlyHeartbeatUpdatesOnlyExistingRowsInItsChannel(t *testing.T) {
	subscriber, db, _, target, userID := refreshOnlyTestSubscriber(t)
	var serverID uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, target).Scan(&serverID))
	otherRoom := uuid.New()
	_, err := db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'other', 'voice')`, otherRoom, serverID)
	require.NoError(t, err)
	otherUser := testdb.CreateUser(t, db)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id) VALUES ($1, $2)`, serverID, otherUser)
	require.NoError(t, err)
	oldTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	_, err = db.Exec(`INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at) VALUES ($1, $2, $3)`, target, userID, oldTime)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at) VALUES ($1, $2, $3)`, otherRoom, otherUser, oldTime)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	frame, err := json.Marshal(map[string]any{
		"channelId": target.String(), "userIds": []string{userID.String(), otherUser.String()},
		"colour": "blue", "instance_id": "blue-42",
		"participants_authoritative": true, "timestamp": now.Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	subscriber.handleVoiceLifecycleEvent(natsSubjectVoiceHeartbeat, frame, now)
	var refreshed, unchanged time.Time
	require.NoError(t, db.QueryRow(`SELECT lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&refreshed))
	require.NoError(t, db.QueryRow(`SELECT lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, otherRoom, otherUser).Scan(&unchanged))
	require.Equal(t, now, refreshed)
	require.Equal(t, oldTime, unchanged)
	var added int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, otherUser).Scan(&added))
	require.Zero(t, added)
}

func TestRefreshOnlyHeartbeatWaitsForParticipantLifecycleLock(t *testing.T) {
	subscriber, db, _, target, userID := refreshOnlyTestSubscriber(t)
	oldTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	_, err := db.Exec(`INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at) VALUES ($1, $2, $3)`, target, userID, oldTime)
	require.NoError(t, err)
	lockTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lockTx.Rollback() })
	require.NoError(t, LockServerVoiceLifecycleTx(t.Context(), lockTx, userID))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err = subscriber.refreshOnlyHeartbeat(ctx, target, []uuid.UUID{userID}, time.Now().UTC())
	require.Error(t, err)
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	var watermark time.Time
	require.NoError(t, db.QueryRow(`SELECT lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&watermark))
	require.Equal(t, oldTime, watermark)
}

func TestStaleServerVoiceCleanupPreservesLiveRoom(t *testing.T) {
	subscriber, db, server, target, userID := refreshOnlyTestSubscriber(t)
	_, err := db.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, target, userID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE voice_participants SET lifecycle_observed_at = clock_timestamp() - INTERVAL '2 minutes' WHERE channel_id = $1 AND user_id = $2`, target, userID)
	require.NoError(t, err)
	require.NoError(t, server.Set("voice:room_live:"+target.String(), "blue-42"))
	subscriber.serverVoiceCleanupOnce.Do(func() {})
	subscriber.serverVoiceCleanupReadyAt = time.Now().Add(-time.Second)
	removed, err := subscriber.ReconcileStaleServerVoiceParticipants(t.Context(), 1)
	require.NoError(t, err)
	require.Zero(t, removed)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&count))
	require.Equal(t, 1, count)
	server.Del("voice:room_live:" + target.String())
	// An unreadable occupancy state must not be misclassified as absent.
	require.NoError(t, subscriber.redis.Close())
	removed, err = subscriber.ReconcileStaleServerVoiceParticipants(t.Context(), 1)
	require.Error(t, err)
	require.Zero(t, removed)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&count))
	require.Equal(t, 1, count)
}

func TestRefreshOnlyDMDoesNotMutateParticipantRows(t *testing.T) {
	subscriber, db, server, _, userID := refreshOnlyTestSubscriber(t)
	conversationID := uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, conversationID, userID)
	require.NoError(t, err)
	oldTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	_, err = db.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id, lifecycle_event_at) VALUES ($1, $2, $3)`, conversationID, userID, oldTime)
	require.NoError(t, err)
	require.NoError(t, server.Set("voice:room_owner:"+conversationID.String(), "blue"))
	now := time.Now().UTC().Truncate(time.Microsecond)
	frame, err := json.Marshal(map[string]any{
		"channelId": conversationID.String(), "userIds": []string{userID.String()},
		"colour": "blue", "instance_id": "blue-42",
		"participants_authoritative": true, "timestamp": now.Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	subscriber.handleVoiceLifecycleEvent(natsSubjectVoiceHeartbeat, frame, now)
	var unchanged time.Time
	require.NoError(t, db.QueryRow(`SELECT lifecycle_event_at FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, userID).Scan(&unchanged))
	require.Equal(t, oldTime, unchanged)
}
