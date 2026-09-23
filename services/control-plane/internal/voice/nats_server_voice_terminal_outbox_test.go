package voice_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	concordws "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func insertServerVoiceTerminalOutbox(t *testing.T, db *sql.DB, channelID, userID, serverID, operationID uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO server_voice_terminal_outbox
			(channel_id, user_id, server_id, operation_id, created_at, reconcile_after)
		VALUES ($1, $2, $3, $4, clock_timestamp() - interval '1 second', clock_timestamp() - interval '1 second')
	`, channelID, userID, serverID, operationID)
	require.NoError(t, err)
}

func requireServerVoiceTerminalOutboxCount(t *testing.T, db *sql.DB, operationID uuid.UUID, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var got int
		err := db.QueryRow(`SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE operation_id = $1`, operationID).Scan(&got)
		return err == nil && got == want
	}, 2*time.Second, 10*time.Millisecond)
}

func installServerVoiceTerminalAckFailureForOperation(t *testing.T, db *sql.DB, operationID uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`
		CREATE TABLE public.test_fail_server_voice_terminal_ack_operations (
			operation_id UUID PRIMARY KEY
		);
		CREATE FUNCTION test_fail_server_voice_terminal_ack_for_operation() RETURNS trigger AS $$
		BEGIN
			IF EXISTS (
				SELECT 1
				FROM public.test_fail_server_voice_terminal_ack_operations
				WHERE operation_id = OLD.operation_id
			) THEN
				RAISE EXCEPTION 'forced server voice terminal acknowledgement failure';
			END IF;
			RETURN OLD;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_server_voice_terminal_ack_for_operation
			BEFORE DELETE ON server_voice_terminal_outbox
			FOR EACH ROW EXECUTE FUNCTION test_fail_server_voice_terminal_ack_for_operation();
	`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO public.test_fail_server_voice_terminal_ack_operations (operation_id) VALUES ($1)`, operationID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := db.Exec(`
			DROP TRIGGER IF EXISTS test_fail_server_voice_terminal_ack_for_operation ON server_voice_terminal_outbox;
			DROP FUNCTION IF EXISTS test_fail_server_voice_terminal_ack_for_operation();
			DROP TABLE IF EXISTS public.test_fail_server_voice_terminal_ack_operations;
		`)
		require.NoError(t, cleanupErr)
	})
}

func assertNoServerVoiceLeftBeforeSentinel(t *testing.T, hub *concordws.Hub, conn *gorillaWS.Conn, serverID uuid.UUID) {
	t.Helper()
	hub.BroadcastToServer(serverID, concordws.OutgoingMessage{Type: "terminal_test_sentinel", Data: map[string]interface{}{}})
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	for {
		var envelope voiceWireEnvelope
		require.NoError(t, conn.ReadJSON(&envelope))
		if envelope.Type == "voice_state_update" {
			assert.NotEqual(t, "left", envelope.Data["action"])
		}
		if envelope.Type == "terminal_test_sentinel" {
			return
		}
	}
}

func TestServerVoiceTerminalOutbox_CapturesStaleDeleteDurably(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-outbox-owner")
	user := ts.CreateTestUser(t, "terminal-outbox-user")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-outbox-server")
	ts.AddMemberToServer(t, serverID, user.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-outbox-channel")
	insertVoiceParticipant(t, ts.DB, channelID, user.ID)
	_, err := ts.DB.Exec(`UPDATE voice_participants SET lifecycle_observed_at = $1 WHERE channel_id = $2 AND user_id = $3`, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), channelID, user.ID)
	require.NoError(t, err)

	// A stopped hub makes queue refusal deterministic, leaving the atomically
	// captured obligation available for a later pass.
	ts.Hub.Shutdown()
	counters := opsmetrics.NewCounters()
	sub := voice.NewNATSSubscriber(ts.DB, logger.New("test"), ts.Hub, nil, ts.Redis, nil, nil)
	sub.SetOpsCounters(counters)
	sub.CompleteServerVoiceCleanupGraceForTest()
	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.False(t, voiceParticipantExists(t, ts.DB, channelID, user.ID))
	var outboxCount int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE channel_id = $1 AND user_id = $2`, channelID, user.ID).Scan(&outboxCount))
	assert.Equal(t, 1, outboxCount, "a stale delete must leave a durable terminal obligation when the hub refuses enqueue")
	assert.Equal(t, float64(1), counters.Snapshot()[opsmetrics.MetricServerVoiceTerminalOutboxCapturedTotal])
}

func TestServerVoiceTerminalOutbox_StaleCleanupDoesNotDeadlockWithAccountErasure(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-erasure-race-owner")
	erasedUser := ts.CreateTestUser(t, "terminal-erasure-race-user")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-erasure-race-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-erasure-race-channel")
	userID := uuid.MustParse(erasedUser.ID)
	insertVoiceParticipant(t, ts.DB, channelID, erasedUser.ID)
	_, err := ts.DB.Exec(`
		UPDATE voice_participants
		SET lifecycle_observed_at = $1
		WHERE channel_id = $2 AND user_id = $3
	`, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), channelID, erasedUser.ID)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	erasureTx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := erasureTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback erasure transaction: %v", rollbackErr)
		}
	})
	var lockedUserID uuid.UUID
	require.NoError(t, erasureTx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&lockedUserID))
	var erasureTxID int64
	require.NoError(t, erasureTx.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&erasureTxID))

	sub := newTestSubscriber(ts)
	sub.CompleteServerVoiceCleanupGraceForTest()
	reconcileDone := make(chan error, 1)
	go func() {
		_, reconcileErr := sub.ReconcileStaleServerVoiceParticipants(ctx, 1)
		reconcileDone <- reconcileErr
	}()
	testdb.WaitForRowLockWaiter(t, ts.DB, erasureTxID)

	_, err = erasureTx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	require.NoError(t, err)
	require.NoError(t, erasureTx.Commit())

	select {
	case reconcileErr := <-reconcileDone:
		require.NoError(t, reconcileErr)
	case <-ctx.Done():
		t.Fatal("stale cleanup did not complete after account erasure")
	}
}

func TestServerVoiceTerminalOutbox_DrainWaitsForAccountErasure(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-drain-erasure-owner")
	erasedUser := ts.CreateTestUser(t, "terminal-drain-erasure-user")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-drain-erasure-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-drain-erasure-channel")
	userID := uuid.MustParse(erasedUser.ID)
	operationID := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), userID, uuid.MustParse(serverID), operationID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	erasureTx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := erasureTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback erasure transaction: %v", rollbackErr)
		}
	})
	var lockedUserID uuid.UUID
	require.NoError(t, erasureTx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&lockedUserID))
	var erasureTxID int64
	require.NoError(t, erasureTx.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&erasureTxID))

	sub := newTestSubscriber(ts)
	drainDone := make(chan error, 1)
	go func() {
		drainDone <- sub.DrainServerVoiceTerminalOutboxCandidateForTest(
			ctx, uuid.MustParse(channelID), userID, operationID,
		)
	}()
	testdb.WaitForRowLockWaiter(t, ts.DB, erasureTxID)

	_, err = erasureTx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	require.NoError(t, err)
	require.NoError(t, erasureTx.Commit())

	select {
	case drainErr := <-drainDone:
		require.NoError(t, drainErr)
	case <-ctx.Done():
		t.Fatal("terminal drain did not complete after account erasure")
	}
	var pending int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE operation_id = $1`, operationID).Scan(&pending))
	assert.Zero(t, pending, "account erasure must cascade the unadmitted terminal obligation")
}

func TestServerVoiceTerminalOutbox_AckFailureRetainsButDoesNotStopBatch(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-drain-continue-owner")
	firstUser := ts.CreateTestUser(t, "terminal-drain-continue-first")
	secondUser := ts.CreateTestUser(t, "terminal-drain-continue-second")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-drain-continue-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-drain-continue-channel")
	firstOp, secondOp := uuid.New(), uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(firstUser.ID), uuid.MustParse(serverID), firstOp)
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(secondUser.ID), uuid.MustParse(serverID), secondOp)
	installServerVoiceTerminalAckFailureForOperation(t, ts.DB, firstOp)

	sub := newTestSubscriber(ts)
	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 2)
	require.NoError(t, err)
	assert.Zero(t, removed)

	requireServerVoiceTerminalOutboxCount(t, ts.DB, firstOp, 1)
	requireServerVoiceTerminalOutboxCount(t, ts.DB, secondOp, 0)
}

func TestServerVoiceTerminalOutbox_SuccessorAckFailureDoesNotStopBatch(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-successor-continue-owner")
	firstUser := ts.CreateTestUser(t, "terminal-successor-continue-first")
	secondUser := ts.CreateTestUser(t, "terminal-successor-continue-second")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-successor-continue-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-successor-continue-channel")
	firstOp, secondOp := uuid.New(), uuid.New()
	insertVoiceParticipant(t, ts.DB, channelID, firstUser.ID)
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(firstUser.ID), uuid.MustParse(serverID), firstOp)
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(secondUser.ID), uuid.MustParse(serverID), secondOp)
	installServerVoiceTerminalAckFailureForOperation(t, ts.DB, firstOp)

	sub := newTestSubscriber(ts)
	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 2)
	require.ErrorContains(t, err, "ack successor-suppressed server voice terminal obligation")
	assert.Zero(t, removed)

	requireServerVoiceTerminalOutboxCount(t, ts.DB, firstOp, 1)
	requireServerVoiceTerminalOutboxCount(t, ts.DB, secondOp, 0)
}

func TestServerVoiceTerminalOutbox_CaptureFailureRollsBackStaleDelete(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-capture-failure-owner")
	user := ts.CreateTestUser(t, "terminal-capture-failure-user")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-capture-failure-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-capture-failure-channel")
	insertVoiceParticipant(t, ts.DB, channelID, user.ID)
	_, err := ts.DB.Exec(`
		UPDATE voice_participants
		SET lifecycle_observed_at = $1
		WHERE channel_id = $2 AND user_id = $3
	`, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), channelID, user.ID)
	require.NoError(t, err)

	_, err = ts.DB.Exec(`
		CREATE FUNCTION test_fail_server_voice_terminal_capture() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'forced server voice terminal capture failure';
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_server_voice_terminal_capture
			BEFORE INSERT ON server_voice_terminal_outbox
			FOR EACH ROW EXECUTE FUNCTION test_fail_server_voice_terminal_capture();
	`)
	require.NoError(t, err)
	triggerInstalled := true
	t.Cleanup(func() {
		if !triggerInstalled {
			return
		}
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_fail_server_voice_terminal_capture ON server_voice_terminal_outbox;
			DROP FUNCTION IF EXISTS test_fail_server_voice_terminal_capture();
		`)
		require.NoError(t, cleanupErr)
	})

	sub := newTestSubscriber(ts)
	sub.CompleteServerVoiceCleanupGraceForTest()
	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.ErrorContains(t, err, "capture stale server voice terminal obligation")
	require.ErrorContains(t, err, "forced server voice terminal capture failure")
	assert.Zero(t, removed)
	assert.True(t, voiceParticipantExists(t, ts.DB, channelID, user.ID))
	var outboxCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE channel_id = $1 AND user_id = $2
	`, channelID, user.ID).Scan(&outboxCount))
	assert.Zero(t, outboxCount)

	_, err = ts.DB.Exec(`
		DROP TRIGGER test_fail_server_voice_terminal_capture ON server_voice_terminal_outbox;
		DROP FUNCTION test_fail_server_voice_terminal_capture();
	`)
	require.NoError(t, err)
	triggerInstalled = false

	ts.Hub.Shutdown()
	removed, err = sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.False(t, voiceParticipantExists(t, ts.DB, channelID, user.ID))
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE channel_id = $1 AND user_id = $2
	`, channelID, user.ID).Scan(&outboxCount))
	assert.Equal(t, 1, outboxCount, "recovery must retain the captured marker when the hub refuses enqueue")
}

func TestServerVoiceTerminalOutbox_AckFailureRetainsAndLaterSettles(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-ack-failure-owner")
	user := ts.CreateTestUser(t, "terminal-ack-failure-user")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-ack-failure-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-ack-failure-channel")
	op := uuid.New()
	insertServerVoiceTerminalOutbox(
		t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op,
	)

	_, err := ts.DB.Exec(`
		CREATE FUNCTION test_fail_server_voice_terminal_ack() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'forced server voice terminal acknowledgement failure';
			RETURN OLD;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_server_voice_terminal_ack
			BEFORE DELETE ON server_voice_terminal_outbox
			FOR EACH ROW EXECUTE FUNCTION test_fail_server_voice_terminal_ack();
	`)
	require.NoError(t, err)
	triggerInstalled := true
	t.Cleanup(func() {
		if !triggerInstalled {
			return
		}
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_fail_server_voice_terminal_ack ON server_voice_terminal_outbox;
			DROP FUNCTION IF EXISTS test_fail_server_voice_terminal_ack();
		`)
		require.NoError(t, cleanupErr)
	})

	sub := newTestSubscriber(ts)
	err = sub.DrainServerVoiceTerminalOutboxCandidateForTest(
		context.Background(), uuid.MustParse(channelID), uuid.MustParse(user.ID), op,
	)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var claimed int
		err := ts.DB.QueryRow(`
			SELECT COUNT(*) FROM server_voice_terminal_outbox
			WHERE operation_id = $1 AND delivery_claim_id IS NOT NULL
		`, op).Scan(&claimed)
		return err == nil && claimed == 1
	}, 2*time.Second, 10*time.Millisecond)

	_, err = ts.DB.Exec(`
		DROP TRIGGER test_fail_server_voice_terminal_ack ON server_voice_terminal_outbox;
		DROP FUNCTION test_fail_server_voice_terminal_ack();
	`)
	require.NoError(t, err)
	triggerInstalled = false
	_, err = ts.DB.Exec(`
		UPDATE server_voice_terminal_outbox
		SET delivery_claim_until = clock_timestamp() - interval '1 second'
		WHERE operation_id = $1
	`, op)
	require.NoError(t, err)

	require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(
		context.Background(), uuid.MustParse(channelID), uuid.MustParse(user.ID), op,
	))
	requireServerVoiceTerminalOutboxCount(t, ts.DB, op, 0)
}

func TestServerVoiceTerminalOutbox_AckFailureStillBroadcastsCountsAfterLocalLeave(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-ack-count-owner")
	user := ts.CreateTestUser(t, "terminal-ack-count-user")
	viewer := ts.CreateTestUser(t, "terminal-ack-count-viewer")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-ack-count-server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-ack-count-channel")
	ts.CreateChannelOverride(t, channelID, "user", viewer.ID, int64(rbac.PermViewVoiceChannels), 0)
	op := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op)
	installServerVoiceTerminalAckFailureForOperation(t, ts.DB, op)

	hub, baseURL := newVoiceReplicaHub(t, ts)
	conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_server", "data": map[string]interface{}{"server_id": serverID}}))
	synchronizeVoiceWireClient(t, conn)

	sub := newTestSubscriberWithHub(ts, hub)
	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	assert.Zero(t, removed)

	sawLeft, sawCounts := false, false
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	for !sawLeft || !sawCounts {
		var envelope voiceWireEnvelope
		require.NoError(t, conn.ReadJSON(&envelope))
		switch envelope.Type {
		case "voice_state_update":
			sawLeft = envelope.Data["action"] == "left" || sawLeft
		case "server_voice_counts":
			counts, _ := envelope.Data["counts"].(map[string]interface{})
			sawCounts = counts[serverID] == float64(0) || sawCounts
		}
	}
}

func TestServerVoiceTerminalOutbox_AccountErasureCancelsPendingLeave(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-erasure-owner")
	erasedUser := ts.CreateTestUser(t, "terminal-erasure-user")
	viewer := ts.CreateTestUser(t, "terminal-erasure-viewer")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-erasure-server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-erasure-channel")
	op := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(erasedUser.ID), uuid.MustParse(serverID), op)

	hub, baseURL := newVoiceReplicaHub(t, ts)
	conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_server", "data": map[string]interface{}{"server_id": serverID}}))
	synchronizeVoiceWireClient(t, conn)

	svc := users.NewAccountService(ts.DB, logger.New("test"))
	require.NoError(t, svc.DeleteAccount(context.Background(), erasedUser.ID))

	sub := newTestSubscriberWithHub(ts, hub)
	require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(context.Background(), uuid.MustParse(channelID), uuid.MustParse(erasedUser.ID), op))
	assertNoServerVoiceLeftBeforeSentinel(t, hub, conn, uuid.MustParse(serverID))

	var pending int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE operation_id = $1`, op).Scan(&pending))
	assert.Zero(t, pending)
}

func TestServerVoiceTerminalOutbox_StaleOperationCannotSettleReplacement(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-token-owner")
	user := ts.CreateTestUser(t, "terminal-token-user")
	viewer := ts.CreateTestUser(t, "terminal-token-viewer")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-token-server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-token-channel")
	oldOperation, newOperation := uuid.New(), uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), oldOperation)
	_, err := ts.DB.Exec(`UPDATE server_voice_terminal_outbox SET operation_id = $1 WHERE channel_id = $2 AND user_id = $3`, newOperation, channelID, user.ID)
	require.NoError(t, err)

	hub, baseURL := newVoiceReplicaHub(t, ts)
	conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_server", "data": map[string]interface{}{"server_id": serverID}}))
	synchronizeVoiceWireClient(t, conn)
	sub := newTestSubscriberWithHub(ts, hub)
	require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(context.Background(), uuid.MustParse(channelID), uuid.MustParse(user.ID), oldOperation))
	assertNoServerVoiceLeftBeforeSentinel(t, hub, conn, uuid.MustParse(serverID))
	var stored uuid.UUID
	require.NoError(t, ts.DB.QueryRow(`SELECT operation_id FROM server_voice_terminal_outbox WHERE channel_id = $1 AND user_id = $2`, channelID, user.ID).Scan(&stored))
	assert.Equal(t, newOperation, stored)
}

func TestServerVoiceTerminalOutbox_CaptureReplacementClearsActiveClaim(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-claim-replacement-owner")
	user := ts.CreateTestUser(t, "terminal-claim-replacement-user")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-claim-replacement-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-claim-replacement-channel")
	insertVoiceParticipant(t, ts.DB, channelID, user.ID)
	oldOperation := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), oldOperation)
	_, err := ts.DB.Exec(`
		UPDATE server_voice_terminal_outbox
		SET delivery_claim_id = $1,
		    delivery_claim_until = clock_timestamp() + interval '1 hour'
		WHERE operation_id = $2
	`, uuid.New(), oldOperation)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		UPDATE voice_participants
		SET lifecycle_observed_at = clock_timestamp() - interval '2 minutes'
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, user.ID)
	require.NoError(t, err)

	ts.Hub.Shutdown()
	sub := newTestSubscriber(ts)
	sub.CompleteServerVoiceCleanupGraceForTest()
	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, removed)

	var operationID uuid.UUID
	var claimID uuid.NullUUID
	require.NoError(t, ts.DB.QueryRow(`
		SELECT operation_id, delivery_claim_id
		FROM server_voice_terminal_outbox
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, user.ID).Scan(&operationID, &claimID))
	assert.NotEqual(t, oldOperation, operationID)
	assert.False(t, claimID.Valid, "a replacement obligation must not inherit an old delivery claim")
}

func TestServerVoiceTerminalOutbox_SuccessorSuppressesAndSettles(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-successor-owner")
	user := ts.CreateTestUser(t, "terminal-successor-user")
	viewer := ts.CreateTestUser(t, "terminal-successor-viewer")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-successor-server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-successor-channel")
	insertVoiceParticipant(t, ts.DB, channelID, user.ID)
	operationID := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), operationID)

	hub, baseURL := newVoiceReplicaHub(t, ts)
	conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_server", "data": map[string]interface{}{"server_id": serverID}}))
	synchronizeVoiceWireClient(t, conn)
	sub := newTestSubscriberWithHub(ts, hub)
	require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(context.Background(), uuid.MustParse(channelID), uuid.MustParse(user.ID), operationID))
	assertNoServerVoiceLeftBeforeSentinel(t, hub, conn, uuid.MustParse(serverID))
	var outboxCount int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE channel_id = $1 AND user_id = $2`, channelID, user.ID).Scan(&outboxCount))
	assert.Zero(t, outboxCount)
	assert.True(t, voiceParticipantExists(t, ts.DB, channelID, user.ID), "successor membership remains authoritative")
}

func TestServerVoiceTerminalOutbox_PendingDrainsDuringStartupGraceBeforeFreshDiscovery(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-grace-owner")
	pendingUser := ts.CreateTestUser(t, "terminal-grace-pending")
	freshUser := ts.CreateTestUser(t, "terminal-grace-fresh")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-grace-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-grace-channel")
	insertVoiceParticipant(t, ts.DB, channelID, freshUser.ID)
	_, err := ts.DB.Exec(`UPDATE voice_participants SET lifecycle_observed_at = $1 WHERE channel_id = $2 AND user_id = $3`, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), channelID, freshUser.ID)
	require.NoError(t, err)
	operationID := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(pendingUser.ID), uuid.MustParse(serverID), operationID)

	sub := newTestSubscriber(ts)
	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	assert.Zero(t, removed, "startup grace must defer fresh discovery")
	assert.True(t, voiceParticipantExists(t, ts.DB, channelID, freshUser.ID))
	requireServerVoiceTerminalOutboxCount(t, ts.DB, operationID, 0)
}

func TestServerVoiceTerminalOutbox_FullQueueReschedulesThenNewHubDelivers(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-queue-owner")
	user := ts.CreateTestUser(t, "terminal-queue-user")
	viewer := ts.CreateTestUser(t, "terminal-queue-viewer")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-queue-server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-queue-channel")
	ts.CreateChannelOverride(t, channelID, "user", viewer.ID, int64(rbac.PermViewVoiceChannels), 0)
	op := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op)
	oldHub := concordws.NewHub(ts.DB, ts.Redis)
	for i := 0; i < 256; i++ {
		require.True(t, oldHub.BroadcastToServerChannelAuthorizedContext(
			context.Background(), uuid.MustParse(serverID), uuid.MustParse(channelID),
			concordws.OutgoingMessage{Type: "queue_fill", Data: map[string]interface{}{}},
		))
	}
	sub := newTestSubscriberWithHub(ts, oldHub)
	require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(context.Background(), uuid.MustParse(channelID), uuid.MustParse(user.ID), op))
	var due time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT reconcile_after FROM server_voice_terminal_outbox WHERE operation_id = $1`, op).Scan(&due))
	assert.True(t, due.After(time.Now()), "full local queue must reschedule the obligation")

	hub, baseURL := newVoiceReplicaHub(t, ts)
	conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_server", "data": map[string]interface{}{"server_id": serverID}}))
	synchronizeVoiceWireClient(t, conn)
	sub = newTestSubscriberWithHub(ts, hub)
	_, err := ts.DB.Exec(`UPDATE server_voice_terminal_outbox SET reconcile_after = clock_timestamp() WHERE operation_id = $1`, op)
	require.NoError(t, err)
	require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(context.Background(), uuid.MustParse(channelID), uuid.MustParse(user.ID), op))
	envelope := waitForVoiceWireType(t, conn, "voice_state_update")
	assert.Equal(t, "left", envelope.Data["action"])
	require.Eventually(t, func() bool {
		var count int
		return ts.DB.QueryRow(`SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE operation_id = $1`, op).Scan(&count) == nil && count == 0
	}, 3*time.Second, 20*time.Millisecond)
}

func TestServerVoiceTerminalOutbox_HeldLifecycleLockRetainsThenDelivers(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-lock-owner")
	user := ts.CreateTestUser(t, "terminal-lock-user")
	viewer := ts.CreateTestUser(t, "terminal-lock-viewer")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-lock-server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-lock-channel")
	ts.CreateChannelOverride(t, channelID, "user", viewer.ID, int64(rbac.PermViewVoiceChannels), 0)
	op := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op)

	tx, err := ts.DB.Begin()
	require.NoError(t, err)
	require.NoError(t, voice.LockServerVoiceLifecycleTx(context.Background(), tx, uuid.MustParse(user.ID)))
	sub := newTestSubscriber(ts)
	require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(context.Background(), uuid.MustParse(channelID), uuid.MustParse(user.ID), op))
	require.NoError(t, tx.Commit())

	hub, baseURL := newVoiceReplicaHub(t, ts)
	conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_server", "data": map[string]interface{}{"server_id": serverID}}))
	synchronizeVoiceWireClient(t, conn)
	sub = newTestSubscriberWithHub(ts, hub)
	require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(context.Background(), uuid.MustParse(channelID), uuid.MustParse(user.ID), op))
	envelope := waitForVoiceWireType(t, conn, "voice_state_update")
	assert.Equal(t, "left", envelope.Data["action"])
}

func TestServerVoiceTerminalOutbox_MissingChannelSettlesAndBroadcastsCounts(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-erased-owner")
	user := ts.CreateTestUser(t, "terminal-erased-user")
	viewer := ts.CreateTestUser(t, "terminal-erased-viewer")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-erased-server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-erased-channel")
	op := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op)
	_, err := ts.DB.Exec(`DELETE FROM channels WHERE id = $1`, channelID)
	require.NoError(t, err, "the terminal obligation must outlive its channel parent")

	hub, baseURL := newVoiceReplicaHub(t, ts)
	conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_server", "data": map[string]interface{}{"server_id": serverID}}))
	synchronizeVoiceWireClient(t, conn)
	waitForVoiceWireType(t, conn, "server_voice_counts")
	sub := newTestSubscriberWithHub(ts, hub)
	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	assert.Zero(t, removed)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	for sawCounts := false; !sawCounts; {
		var envelope voiceWireEnvelope
		require.NoError(t, conn.ReadJSON(&envelope))
		switch envelope.Type {
		case "voice_state_update":
			require.Fail(t, "missing channel must not emit a participant frame")
		case "server_voice_counts":
			counts, _ := envelope.Data["counts"].(map[string]interface{})
			sawCounts = counts[serverID] == float64(0)
		}
	}

	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE operation_id = $1`, op).Scan(&count))
	assert.Zero(t, count)
}

func TestServerVoiceTerminalOutbox_FiltersServerSubscribersByChannelView(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-view-owner")
	user := ts.CreateTestUser(t, "terminal-view-user")
	allowed := ts.CreateTestUser(t, "terminal-view-allowed")
	denied := ts.CreateTestUser(t, "terminal-view-denied")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-view-server")
	for _, viewer := range []testhelpers.TestUser{allowed, denied} {
		ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	}
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-view-channel")
	ts.CreateChannelOverride(t, channelID, "user", allowed.ID, int64(rbac.PermViewVoiceChannels), 0)
	ts.CreateChannelOverride(t, channelID, "user", denied.ID, 0, int64(rbac.PermViewVoiceChannels))
	op := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op)

	hub, baseURL := newVoiceReplicaHub(t, ts)
	allowedConn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, allowed)
	deniedConn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, denied)
	for _, conn := range []*gorillaWS.Conn{allowedConn, deniedConn} {
		require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_server", "data": map[string]interface{}{"server_id": serverID}}))
		synchronizeVoiceWireClient(t, conn)
	}

	sub := newTestSubscriberWithHub(ts, hub)
	require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(context.Background(), uuid.MustParse(channelID), uuid.MustParse(user.ID), op))
	require.NoError(t, allowedConn.SetReadDeadline(time.Now().Add(2*time.Second)))
	for {
		var envelope voiceWireEnvelope
		require.NoError(t, allowedConn.ReadJSON(&envelope))
		if envelope.Type == "voice_state_update" && envelope.Data["action"] == "left" {
			break
		}
	}

	hub.BroadcastToServer(uuid.MustParse(serverID), concordws.OutgoingMessage{Type: "terminal_test_sentinel", Data: map[string]interface{}{}})
	require.NoError(t, deniedConn.SetReadDeadline(time.Now().Add(2*time.Second)))
	for {
		var envelope voiceWireEnvelope
		require.NoError(t, deniedConn.ReadJSON(&envelope))
		assert.NotEqual(t, "voice_state_update", envelope.Type, "viewer denied voice-channel access must not receive terminal state")
		if envelope.Type == "terminal_test_sentinel" {
			break
		}
	}
}

func TestServerVoiceTerminalOutbox_LimitOneAlternatesPendingAndFreshWork(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-fair-owner")
	pendingUsers := []string{
		ts.CreateTestUser(t, "terminal-fair-pending-one").ID,
		ts.CreateTestUser(t, "terminal-fair-pending-two").ID,
	}
	freshUsers := []string{
		ts.CreateTestUser(t, "terminal-fair-fresh-one").ID,
		ts.CreateTestUser(t, "terminal-fair-fresh-two").ID,
	}
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-fair-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-fair-channel")
	boundary := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	pendingOperations := make([]uuid.UUID, 0, len(pendingUsers))
	for _, userID := range freshUsers {
		insertVoiceParticipant(t, ts.DB, channelID, userID)
		_, err := ts.DB.Exec(`
			UPDATE voice_participants SET lifecycle_observed_at = $1
			WHERE channel_id = $2 AND user_id = $3
		`, boundary, channelID, userID)
		require.NoError(t, err)
	}
	for _, userID := range pendingUsers {
		op := uuid.New()
		pendingOperations = append(pendingOperations, op)
		insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(userID), uuid.MustParse(serverID), op)
		_, err := ts.DB.Exec(`
			UPDATE server_voice_terminal_outbox SET created_at = $1, reconcile_after = $1
			WHERE operation_id = $2
		`, boundary, op)
		require.NoError(t, err)
	}

	ts.Hub.Shutdown()
	sub := newTestSubscriber(ts)
	sub.CompleteServerVoiceCleanupGraceForTest()
	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	var freshRemainingAfterFirst, pendingRescheduledAfterFirst int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1
	`, channelID).Scan(&freshRemainingAfterFirst))
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM server_voice_terminal_outbox
		WHERE operation_id IN ($1, $2) AND reconcile_after > $3
	`, pendingOperations[0], pendingOperations[1], boundary).Scan(&pendingRescheduledAfterFirst))
	assert.Equal(t, 1, (2-freshRemainingAfterFirst)+pendingRescheduledAfterFirst,
		"the first pass must spend exactly one shared work item")
	firstWasPending := pendingRescheduledAfterFirst == 1
	if firstWasPending {
		assert.Zero(t, removed)
		assert.Equal(t, 2, freshRemainingAfterFirst)
	} else {
		assert.Equal(t, 1, removed)
		assert.Equal(t, 1, freshRemainingAfterFirst)
	}

	removed, err = sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	var freshRemainingAfterSecond, pendingRescheduledAfterSecond int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1
	`, channelID).Scan(&freshRemainingAfterSecond))
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM server_voice_terminal_outbox
		WHERE operation_id IN ($1, $2) AND reconcile_after > $3
	`, pendingOperations[0], pendingOperations[1], boundary).Scan(&pendingRescheduledAfterSecond))
	assert.Equal(t, 1,
		(freshRemainingAfterFirst-freshRemainingAfterSecond)+
			(pendingRescheduledAfterSecond-pendingRescheduledAfterFirst),
		"the second pass must spend exactly one shared work item")
	if firstWasPending {
		assert.Equal(t, 1, removed, "the second pass must select fresh work while pending remains eligible")
		assert.Equal(t, 1, freshRemainingAfterSecond)
		assert.Equal(t, 1, pendingRescheduledAfterSecond)
	} else {
		assert.Zero(t, removed, "the second pass must select pending work while fresh work remains eligible")
		assert.Equal(t, 1, freshRemainingAfterSecond)
		assert.Equal(t, 1, pendingRescheduledAfterSecond)
	}
}

func TestServerVoiceTerminalOutbox_ClampsCombinedWorkToOneThousand(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "terminal-cap-owner")
	freshUser := ts.CreateTestUser(t, "terminal-cap-fresh")
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-cap-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-cap-channel")
	insertVoiceParticipant(t, ts.DB, channelID, freshUser.ID)
	_, err := ts.DB.Exec(`UPDATE voice_participants SET lifecycle_observed_at = $1 WHERE channel_id = $2 AND user_id = $3`, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), channelID, freshUser.ID)
	require.NoError(t, err)
	boundary := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = ts.DB.Exec(`
		WITH generated_users AS (
			SELECT gen_random_uuid() AS id, ordinal
			FROM generate_series(1, 1001) AS series(ordinal)
		), inserted_users AS (
			INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
			SELECT id, 'bulk_' || ordinal::text || '@test.local', 'bulk_' || ordinal::text, 'x', true, true
			FROM generated_users
			RETURNING id
		)
		INSERT INTO server_voice_terminal_outbox
			(channel_id, user_id, server_id, operation_id, created_at, reconcile_after)
		SELECT gen_random_uuid(), id, $1, gen_random_uuid(), $2, $2
		FROM inserted_users
	`, serverID, boundary)
	require.NoError(t, err)

	ts.Hub.Shutdown()
	sub := newTestSubscriber(ts)
	sub.CompleteServerVoiceCleanupGraceForTest()
	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1<<20)
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "the shared 1000-item budget includes the one fresh candidate")
	var pending int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM server_voice_terminal_outbox`).Scan(&pending))
	assert.Equal(t, 3, pending,
		"the 1000-item budget settles 999 missing-channel obligations and captures the one fresh candidate")
	var rescheduled int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE reconcile_after > $1`, boundary).Scan(&rescheduled))
	assert.Equal(t, 1, rescheduled, "only the fresh candidate's captured obligation is rescheduled")
	assert.False(t, voiceParticipantExists(t, ts.DB, channelID, freshUser.ID), "the fresh candidate is one of the 1000 combined attempts")
}
