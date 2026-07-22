package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/google/uuid"
	gorillaws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- #1233 custom-text fan-out + on-connect snapshot integration tests ---
//
// These tests are the risk: privacy regression locks for the Custom Text Status
// feature. The load-bearing assertions are the NON-AUDIENCE EXCLUSIONS: a viewer
// who is not in the sender's tier-audience must NEVER receive the sender's custom
// text — neither via BroadcastCustomText (B3) nor via the on-connect snapshot
// (B4).

const customTextHash = "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec // dev test hash // pragma: allowlist secret

// insertCTUser inserts a minimal user row and returns its UUID.
func insertCTUser(t *testing.T, db *sql.DB, username string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	email := fmt.Sprintf("%s@test.concord.chat", username)
	// nosemgrep: go.lang.security.audit.sqli.gosql-sqli.gosql-sqli -- fully parameterized ($1..$4); email/username are test-controlled values passed as bound params, not interpolated SQL
	_, err := db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		id.String(), email, username, customTextHash)
	require.NoError(t, err)
	return id
}

// makeFriends inserts an accepted friendship between a and b.
func makeFriends(t *testing.T, db *sql.DB, a, b uuid.UUID) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`,
		a.String(), b.String())
	require.NoError(t, err)
}

// shareServer puts users a and b in the same newly-created server.
func shareServer(t *testing.T, db *sql.DB, owner uuid.UUID, members ...uuid.UUID) {
	t.Helper()
	serverID := uuid.New()
	name := fmt.Sprintf("ct-server-%s", serverID.String()[:8])
	// nosemgrep: go.lang.security.audit.sqli.gosql-sqli.gosql-sqli -- fully parameterized ($1..$3); name is a test-controlled UUID-derived value passed as a bound param, not interpolated SQL
	_, err := db.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, $2, $3)`,
		serverID.String(), name, owner.String())
	require.NoError(t, err)
	for _, m := range members {
		_, err = db.Exec(
			`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`,
			serverID.String(), m.String())
		require.NoError(t, err)
	}
}

// setCustomText writes a user's presence settings (tier + text + optional emoji).
func setCustomText(t *testing.T, db *sql.DB, userID uuid.UUID, tier int, text, emoji string) {
	t.Helper()
	var emojiArg interface{}
	if emoji == "" {
		emojiArg = nil
	} else {
		emojiArg = emoji
	}
	_, err := db.Exec(
		`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text, custom_text_emoji)
		 VALUES ($1, $2, $3, $4)`,
		userID.String(), tier, text, emojiArg)
	require.NoError(t, err)
}

func insertCustomTextPendingOperation(t *testing.T, db *sql.DB, senderID uuid.UUID) uuid.UUID {
	t.Helper()
	operationID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO presence_settings_pending_operations (
			user_id,
			operation_id,
			prior_settings_version,
			created_at,
			reconcile_after
		)
		VALUES ($1, $2, 0, clock_timestamp(), clock_timestamp() + INTERVAL '30 seconds')
	`, senderID, operationID)
	require.NoError(t, err)
	return operationID
}

func excludeCustomTextViewer(t *testing.T, db *sql.DB, senderID, viewerID uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO presence_override_preferences (user_id, category, encrypted_data)
		VALUES ($1, 'custom_text', 'dGVzdA==')
	`, senderID.String())
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
		VALUES ($1, 'custom_text', $2)
	`, senderID.String(), viewerID.String())
	require.NoError(t, err)
}

// connectClient registers a synthetic client for userID in the hub and returns it.
func connectClient(hub *Hub, userID uuid.UUID) *Client {
	clientID := uuid.New()
	client := &Client{
		ID:       clientID,
		UserID:   userID,
		Send:     make(chan []byte, 16),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[clientID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{clientID: true}
	return client
}

// assertNoMessage asserts no message arrives on the client within a short window.
// This is the privacy-exclusion lock: a non-audience viewer receives nothing.
func assertNoMessage(t *testing.T, client *Client) {
	t.Helper()
	select {
	case data := <-client.Send:
		t.Fatalf("expected NO custom-text message for non-audience viewer, got: %s", string(data))
	case <-time.After(150 * time.Millisecond):
		// good — nothing delivered
	}
}

func awaitCustomTextSignal[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for deterministic custom-text signal")
		var zero T
		return zero
	}
}

func requireCustomTextDatabaseWriterBlocked(
	t *testing.T,
	db *sql.DB,
	writerBackendPID int,
	writerFinished <-chan struct{},
	stage string,
) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-writerFinished:
			t.Fatalf("database writer completed before %s", stage)
		default:
		}

		var blockerCount int
		err := db.QueryRow(`
			SELECT cardinality(pg_blocking_pids($1))
		`, writerBackendPID).Scan(&blockerCount)
		require.NoError(t, err)
		if blockerCount > 0 {
			return
		}

		select {
		case <-writerFinished:
			t.Fatalf("database writer completed before %s", stage)
		case <-timer.C:
			t.Fatalf("timed out proving database writer was blocked during %s", stage)
		default:
		}
	}
}

func runCustomTextMasterOffWriter(
	ctx context.Context,
	service *presencehistory.Service,
	senderID uuid.UUID,
	viewerID uuid.UUID,
) error {
	return service.WithSender(
		ctx,
		senderID,
		func() (returnErr error) {
			tx, err := service.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() {
				rollbackErr := service.RollbackTx(tx)
				if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
					returnErr = errors.Join(returnErr, rollbackErr)
				}
			}()

			operation, err := service.BeginAudienceOperation(
				ctx,
				tx,
				senderID,
				presencehistory.ForcedSecurityClear,
			)
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `
				UPDATE user_presence_settings
				SET master_enabled = FALSE,
				    updated_at = clock_timestamp()
				WHERE user_id = $1
			`, senderID)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected != 1 {
				return fmt.Errorf("master-off writer affected %d settings rows", affected)
			}
			if err := service.RecordCustomTextTransition(
				ctx,
				tx,
				senderID,
				operation.Before,
				presencehistory.CustomTextState{},
			); err != nil {
				return err
			}
			if err := service.CommitTx(tx); err != nil {
				return err
			}

			completion := service.CompleteClaim(ctx, presencehistory.DeliveryPlan{
				Mode:        presencehistory.DeliveryExactDelta,
				OperationID: operation.ID,
				SenderID:    senderID,
				ClearRecipients: map[uuid.UUID]bool{
					senderID: true,
					viewerID: true,
				},
			})
			return completion.Err
		},
	)
}

func awaitClientMessageType(t *testing.T, client *Client, messageType string) map[string]interface{} {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case raw, ok := <-client.Send:
			require.True(t, ok, "client queue closed before %s", messageType)
			var message map[string]interface{}
			require.NoError(t, json.Unmarshal(raw, &message))
			if message["type"] == messageType {
				return message
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", messageType)
			return nil
		}
	}
}

func setupCustomTextHub(t *testing.T) (*Hub, *sql.DB) {
	t.Helper()
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	hub.SetPresenceHistoryService(presencehistory.NewService(db, presencehistory.DisclosureState{}, false))
	return hub, db
}

func TestSendCustomTextSnapshot_ReplayIsDeterministicBySenderID(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewer := insertCTUser(t, db, "ctsnapshotorderedviewer")
	senders := []uuid.UUID{
		insertCTUser(t, db, "ctsnapshotorderedsenderb"),
		insertCTUser(t, db, "ctsnapshotorderedsendera"),
	}
	for index, senderID := range senders {
		makeFriends(t, db, senderID, viewer)
		setCustomText(t, db, senderID, 1, fmt.Sprintf("ordered-%d", index), "")
	}
	expected := append([]uuid.UUID(nil), senders...)
	sort.Slice(expected, func(left, right int) bool {
		return expected[left].String() < expected[right].String()
	})
	client := connectClient(hub, viewer)
	client.beginBootstrap()
	var replayOrder []uuid.UUID
	hub.customTextSnapshotBeforeEnqueue = func(senderID, _ uuid.UUID) {
		replayOrder = append(replayOrder, senderID)
	}

	require.NoError(t, hub.sendCustomTextSnapshot(context.Background(), client))
	assert.Equal(t, expected, replayOrder)
	require.True(t, hub.completeClientBootstrap(client, []byte(`{"type":"presence_snapshot"}`)))
	require.Equal(t, "presence_snapshot", readClientMsg(t, client)["type"])
	for _, senderID := range expected {
		message := readClientMsg(t, client)
		assert.Equal(t, senderID.String(), message["data"].(map[string]interface{})["user_id"])
	}
}

// TestBroadcastCustomText_FriendsTier_AudienceVsNonAudience is the core B3
// privacy lock. Sender at Friends tier (1): a friend RECEIVES the update; a
// shared-server-only peer (NOT a friend) does NOT (Friends tier excludes
// server-only peers); a stranger does NOT.
func TestEnqueuePrivacyCritical_FullQueueNeverDequeuesExistingFrame(t *testing.T) {
	queuedUpdate, err := marshalCustomTextFrame(uuid.New(), &CustomTextPayload{Text: "stale"})
	require.NoError(t, err)
	queuedClear, err := marshalCustomTextFrame(uuid.New(), nil)
	require.NoError(t, err)
	nextClear, err := marshalCustomTextFrame(uuid.New(), nil)
	require.NoError(t, err)

	tests := []struct {
		name   string
		queued []byte
	}{
		{name: "best-effort update", queued: queuedUpdate},
		{name: "privacy clear", queued: queuedClear},
		{name: "unknown frame", queued: []byte(`{"type":"future_security_event"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{Send: make(chan []byte, 1)}
			client.Send <- tt.queued

			outcome := enqueuePrivacyCritical(client, nextClear)

			assert.Equal(t, privacyCriticalEnqueueDisconnectRequired, outcome)
			assert.Equal(t, tt.queued, <-client.Send)
		})
	}
}

func TestEnqueuePrivacyCritical_FullQueueClosesSocketImmediately(t *testing.T) {
	serverRead := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := gorillaws.Upgrader{
			CheckOrigin: func(request *http.Request) bool {
				return request.Header.Get("Origin") == ""
			},
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverRead <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_, _, err = conn.ReadMessage()
		serverRead <- err
	}))
	defer server.Close()

	socketURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := gorillaws.DefaultDialer.Dial(socketURL, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	queuedUpdate, err := marshalCustomTextFrame(uuid.New(), &CustomTextPayload{Text: "stale"})
	require.NoError(t, err)
	queuedClear, err := marshalCustomTextFrame(uuid.New(), nil)
	require.NoError(t, err)
	client := &Client{Conn: conn, Send: make(chan []byte, 1)}
	client.Send <- queuedUpdate

	outcome := enqueuePrivacyCritical(client, queuedClear)

	assert.Equal(t, privacyCriticalEnqueueDisconnectRequired, outcome)
	assert.Equal(t, queuedUpdate, <-client.Send)
	select {
	case readErr := <-serverRead:
		require.Error(t, readErr, "server read must unblock when the privacy close closes the socket")
	case <-time.After(10 * time.Second):
		t.Fatal("privacy-critical full-queue path did not close the WebSocket")
	}
}

// TestSendCustomTextSnapshot_AudienceVsNonAudience is the core B4 privacy lock.
// Two senders have custom text at Friends tier. The connecting viewer is a friend
// of senderA only. The snapshot MUST contain senderA's text and MUST NOT contain
// senderB's (the viewer is not in senderB's audience).
func TestSendCustomTextSnapshot_AudienceVsNonAudience(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctsnapviewer")
	senderA := insertCTUser(t, db, "ctsnapsenderA")
	senderB := insertCTUser(t, db, "ctsnapsenderB")

	// viewer is a friend of senderA (in A's Friends-tier audience).
	makeFriends(t, db, senderA, viewer)
	setCustomText(t, db, senderA, 1, "A is coding", "💻")

	// viewer is NOT a friend of senderB and shares no server with B — and even if
	// they shared a server, B's Friends tier (1) would still exclude a server-only
	// peer. Here they have NO relation at all.
	setCustomText(t, db, senderB, 1, "B is secret", "🤫")

	viewerClient := connectClient(hub, viewer)

	require.NoError(t, hub.sendCustomTextSnapshot(context.Background(), viewerClient))

	// Drain all snapshot frames and collect which senders appear.
	seen := map[string]map[string]interface{}{}
	for {
		select {
		case raw := <-viewerClient.Send:
			var msg map[string]interface{}
			require.NoError(t, json.Unmarshal(raw, &msg))
			require.Equal(t, "rich_presence_update", msg["type"])
			data := msg["data"].(map[string]interface{})
			seen[data["user_id"].(string)] = data
		case <-time.After(150 * time.Millisecond):
			goto done
		}
	}
done:
	// Audience member: senderA's text IS in the snapshot.
	require.Contains(t, seen, senderA.String(), "viewer is in senderA's audience and must receive A's custom text")
	payloadA := seen[senderA.String()]["payload"].(map[string]interface{})
	assert.Equal(t, "A is coding", payloadA["text"])
	assert.Equal(t, "💻", payloadA["emoji"])

	// PRIVACY LOCK: senderB's text MUST NOT appear — viewer is not in B's audience.
	assert.NotContains(t, seen, senderB.String(), "viewer is NOT in senderB's audience and must NOT receive B's custom text")
}

func TestCustomTextCandidates_PendingSenderIsAbsent(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewer := insertCTUser(t, db, "ctpendingviewer")
	sender := insertCTUser(t, db, "ctpendingcandidate")
	makeFriends(t, db, sender, viewer)
	setCustomText(t, db, sender, 1, "quarantined", "")
	insertCustomTextPendingOperation(t, db, sender)

	candidates, err := hub.customTextCandidates(context.Background(), viewer)

	require.NoError(t, err)
	assert.NotContains(t, candidates, sender)
}

func TestCustomTextCandidates_MasterOffSenderIsAbsent(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewer := insertCTUser(t, db, "ctmasteroffviewer")
	sender := insertCTUser(t, db, "ctmasteroffcandidate")
	makeFriends(t, db, sender, viewer)
	setCustomText(t, db, sender, 2, "saved but disabled", "")
	_, err := db.Exec(
		`UPDATE user_presence_settings SET master_enabled = FALSE WHERE user_id = $1`,
		sender,
	)
	require.NoError(t, err)

	candidates, err := hub.customTextCandidates(context.Background(), viewer)

	require.NoError(t, err)
	assert.NotContains(t, candidates, sender)
}

func TestCustomTextCandidates_ViewerSpecificBound(t *testing.T) {
	t.Run("unrelated rows do not consume the replay budget", func(t *testing.T) {
		hub, db := setupCustomTextHub(t)
		viewer := insertCTUser(t, db, "ctboundedunrelatedviewer")
		for index := 0; index < clientBootstrapFrameLimit; index++ {
			sender := insertCTUser(t, db, fmt.Sprintf("ctboundedunrelated%03d", index))
			setCustomText(t, db, sender, 2, "not visible", "")
		}

		candidates, err := hub.customTextCandidates(context.Background(), viewer)

		require.NoError(t, err)
		assert.Empty(t, candidates)
	})

	t.Run("visible rows above the replay budget fail closed", func(t *testing.T) {
		hub, db := setupCustomTextHub(t)
		viewer := insertCTUser(t, db, "ctboundedvisibleviewer")
		for index := 0; index < clientBootstrapFrameLimit; index++ {
			sender := insertCTUser(t, db, fmt.Sprintf("ctboundedvisible%03d", index))
			makeFriends(t, db, sender, viewer)
			setCustomText(t, db, sender, 1, "visible", "")
		}

		candidates, err := hub.customTextCandidates(context.Background(), viewer)

		assert.ErrorIs(t, err, errClientBootstrapOverflow)
		assert.Nil(t, candidates)
	})

	t.Run("empty related rows do not consume the replay budget", func(t *testing.T) {
		hub, db := setupCustomTextHub(t)
		viewer := insertCTUser(t, db, "ctboundedemptyviewer")
		for index := 0; index < clientBootstrapFrameLimit; index++ {
			sender := insertCTUser(t, db, fmt.Sprintf("ctboundedempty%03d", index))
			makeFriends(t, db, sender, viewer)
			setCustomText(t, db, sender, 1, "", "")
		}
		visible := insertCTUser(t, db, "ctboundedemptyvisible")
		makeFriends(t, db, visible, viewer)
		setCustomText(t, db, visible, 1, "visible", "")

		candidates, err := hub.customTextCandidates(context.Background(), viewer)

		require.NoError(t, err)
		assert.Equal(t, []uuid.UUID{visible}, candidates)
	})
}

func TestCustomTextCandidateQueryStaysViewerCorrelated(t *testing.T) {
	source, err := os.ReadFile("customtext.go")
	require.NoError(t, err)
	assert.NotContains(t, string(source), "WITH accepted_edges AS",
		"bounded reconnect must not materialize the global friendship graph")
	assert.NotContains(t, string(source), "ComputeCustomTextAudience(ctx, tx, senderID)",
		"final authorization for one viewer must not materialize the sender's full audience")
}

func TestCustomTextCandidates_UsesSenderOwnedFoFAndFinalExclusions(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewer := insertCTUser(t, db, "ctinverseviewer")
	mutual := insertCTUser(t, db, "ctinversemutual")
	sender := insertCTUser(t, db, "ctinversesender")
	makeFriends(t, db, sender, mutual)
	makeFriends(t, db, mutual, viewer)
	setCustomText(t, db, sender, 1, "sender owned", "")
	_, err := db.Exec(`
		INSERT INTO privacy_settings (user_id, dm_friends_of_friends)
		VALUES ($1, TRUE), ($2, FALSE)
	`, sender, viewer)
	require.NoError(t, err)

	candidates, err := hub.customTextCandidates(context.Background(), viewer)
	require.NoError(t, err)
	assert.Contains(t, candidates, sender)

	_, err = db.Exec(`
		UPDATE privacy_settings
		SET dm_friends_of_friends = (user_id = $1)
		WHERE user_id IN ($1, $2)
	`, viewer, sender)
	require.NoError(t, err)
	candidates, err = hub.customTextCandidates(context.Background(), viewer)
	require.NoError(t, err)
	assert.NotContains(t, candidates, sender,
		"the viewer's FoF preference must not authorize a sender")

	makeFriends(t, db, sender, viewer)
	excludeCustomTextViewer(t, db, sender, viewer)
	candidates, err = hub.customTextCandidates(context.Background(), viewer)
	require.NoError(t, err)
	assert.NotContains(t, candidates, sender)

	setCustomText(t, db, viewer, 2, "self", "")
	candidates, err = hub.customTextCandidates(context.Background(), viewer)
	require.NoError(t, err)
	assert.NotContains(t, candidates, viewer)
}

func TestSendCustomTextSnapshot_FinalStateQuerySuppressesNewMasterOffSender(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewer := insertCTUser(t, db, "ctmasterofffinalviewer")
	sender := insertCTUser(t, db, "ctmasterofffinalsender")
	makeFriends(t, db, sender, viewer)
	setCustomText(t, db, sender, 1, "candidate before master off", "")
	viewerClient := connectClient(hub, viewer)
	candidatesRead := make(chan struct{})
	releaseCandidates := make(chan struct{})
	audienceBoundaryReached := make(chan struct{}, 1)
	hub.customTextSnapshotAfterCandidates = func() {
		close(candidatesRead)
		<-releaseCandidates
	}
	hub.customTextSnapshotAfterStateRead = func(_, _ uuid.UUID) {
		audienceBoundaryReached <- struct{}{}
	}
	snapshotDone := make(chan error, 1)
	go func() {
		snapshotDone <- hub.sendCustomTextSnapshot(context.Background(), viewerClient)
	}()
	awaitCustomTextSignal(t, candidatesRead)

	_, err := db.Exec(
		`UPDATE user_presence_settings SET master_enabled = FALSE WHERE user_id = $1`,
		sender,
	)
	require.NoError(t, err)
	close(releaseCandidates)
	require.NoError(t, awaitCustomTextSignal(t, snapshotDone))

	select {
	case <-audienceBoundaryReached:
		t.Fatal("master-off final read reached the pre-audience hook")
	default:
	}
	assertNoMessage(t, viewerClient)
}

func TestSendCustomTextSnapshot_FinalStateQuerySuppressesNewPendingSender(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewer := insertCTUser(t, db, "ctpendingfinalviewer")
	sender := insertCTUser(t, db, "ctpendingfinalsender")
	makeFriends(t, db, sender, viewer)
	setCustomText(t, db, sender, 1, "candidate before quarantine", "")
	viewerClient := connectClient(hub, viewer)
	candidatesRead := make(chan struct{})
	releaseCandidates := make(chan struct{})
	hub.customTextSnapshotAfterCandidates = func() {
		close(candidatesRead)
		<-releaseCandidates
	}
	snapshotDone := make(chan error, 1)
	go func() {
		snapshotDone <- hub.sendCustomTextSnapshot(context.Background(), viewerClient)
	}()
	awaitCustomTextSignal(t, candidatesRead)

	insertCustomTextPendingOperation(t, db, sender)
	close(releaseCandidates)
	require.NoError(t, awaitCustomTextSignal(t, snapshotDone))

	assertNoMessage(t, viewerClient)
}

func TestSendCustomTextSnapshot_CancellationImmediatelyBeforeEnqueueSuppressesFrame(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewer := insertCTUser(t, db, "ctsnapshotcancelviewer")
	sender := insertCTUser(t, db, "ctsnapshotcancelsender")
	makeFriends(t, db, sender, viewer)
	setCustomText(t, db, sender, 1, "must be canceled", "")
	viewerClient := connectClient(hub, viewer)
	ctx, cancel := context.WithCancel(context.Background())
	hub.customTextSnapshotBeforeEnqueue = func(candidateID, viewerID uuid.UUID) {
		if candidateID == sender && viewerID == viewer {
			cancel()
		}
	}

	snapshotErr := hub.sendCustomTextSnapshot(ctx, viewerClient)

	require.ErrorIs(t, snapshotErr, context.Canceled)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	assertNoMessage(t, viewerClient)
}

func TestSendCustomTextSnapshot_TimeoutBeforeSharedGateCannotEnqueueLater(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	hub.SetPresenceHistoryService(service)
	viewer := insertCTUser(t, db, "ctsnapshotgateviewer")
	sender := insertCTUser(t, db, "ctsnapshotgatesender")
	makeFriends(t, db, sender, viewer)
	setCustomText(t, db, sender, 1, "must stay gated", "")
	viewerClient := connectClient(hub, viewer)
	gateHeld := make(chan struct{})
	releaseGate := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- service.WithSender(context.Background(), sender, func() error {
			close(gateHeld)
			<-releaseGate
			return nil
		})
	}()
	awaitCustomTextSignal(t, gateHeld)
	candidatesRead := make(chan struct{})
	hub.customTextSnapshotAfterCandidates = func() {
		close(candidatesRead)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	snapshotDone := make(chan error, 1)
	go func() {
		snapshotDone <- hub.sendCustomTextSnapshot(ctx, viewerClient)
	}()
	awaitCustomTextSignal(t, candidatesRead)
	require.ErrorIs(t, awaitCustomTextSignal(t, snapshotDone), context.DeadlineExceeded)
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)

	close(releaseGate)
	require.NoError(t, awaitCustomTextSignal(t, holderDone))
	assertNoMessage(t, viewerClient)
}

func TestHubRun_BlockedCustomTextSnapshotGateDoesNotBlockOtherEvents(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	hub.SetPresenceHistoryService(service)
	sender := insertCTUser(t, db, "cthubloopsender")
	viewer := insertCTUser(t, db, "cthubloopviewer")
	otherUser := insertCTUser(t, db, "cthubloopother")
	unregisterUser := insertCTUser(t, db, "cthubloopunregister")
	makeFriends(t, db, sender, viewer)
	makeFriends(t, db, sender, otherUser)
	setCustomText(t, db, sender, 1, "snapshot waits off loop", "")

	unregisterClient := connectClient(hub, unregisterUser)
	unregisterClient.Send = make(chan []byte, 32)
	gateHeld := make(chan struct{})
	releaseGate := make(chan struct{})
	var releaseOnce sync.Once
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- service.WithSender(context.Background(), sender, func() error {
			close(gateHeld)
			<-releaseGate
			return nil
		})
	}()
	awaitCustomTextSignal(t, gateHeld)

	snapshotWaiting := make(chan struct{})
	var snapshotOnce sync.Once
	hub.customTextSnapshotAfterCandidates = func() {
		snapshotOnce.Do(func() { close(snapshotWaiting) })
	}
	go hub.Run()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseGate) })
		hub.Shutdown()
	})

	viewerClient := &Client{
		ID:       uuid.New(),
		UserID:   viewer,
		Send:     make(chan []byte, 32),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.register <- viewerClient
	awaitCustomTextSignal(t, snapshotWaiting)

	otherClient := &Client{
		ID:       uuid.New(),
		UserID:   otherUser,
		Send:     make(chan []byte, 32),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.register <- otherClient
	awaitClientMessageType(t, otherClient, "connected")

	hub.BroadcastToAll(OutgoingMessage{
		Type: "custom_text_hub_loop_probe",
		Data: map[string]interface{}{"count": 1},
	})
	// A newly registered client intentionally buffers ordinary live frames
	// behind its replacement. Observe the broadcast on an already-ready client
	// so this assertion measures Hub.Run responsiveness, not bootstrap ordering.
	awaitClientMessageType(t, unregisterClient, "custom_text_hub_loop_probe")

	hub.unregister <- unregisterClient
	closeTimer := time.NewTimer(10 * time.Second)
	defer closeTimer.Stop()
	for {
		select {
		case _, ok := <-unregisterClient.Send:
			if !ok {
				goto unregistered
			}
		case <-closeTimer.C:
			t.Fatal("unrelated unregister did not complete while snapshot waited on sender gate")
		}
	}

unregistered:
	releaseOnce.Do(func() { close(releaseGate) })
	require.NoError(t, awaitCustomTextSignal(t, holderDone))
}

func TestSendCustomTextSnapshot_StateAndAudienceUseOneDatabaseSnapshot(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewer := insertCTUser(t, db, "ctsnapshotconsistentviewer")
	sender := insertCTUser(t, db, "ctsnapshotconsistentsender")
	shareServer(t, db, sender, sender, viewer)
	setCustomText(t, db, sender, 2, "old private status", "")
	viewerClient := connectClient(hub, viewer)

	candidatesRead := make(chan struct{})
	releaseCandidates := make(chan struct{})
	hub.customTextSnapshotAfterCandidates = func() {
		close(candidatesRead)
		<-releaseCandidates
	}
	stateRead := make(chan struct{})
	releaseSnapshot := make(chan struct{})
	var releaseSnapshotOnce sync.Once
	releaseSnapshotRead := func() {
		releaseSnapshotOnce.Do(func() { close(releaseSnapshot) })
	}
	t.Cleanup(releaseSnapshotRead)
	hub.customTextSnapshotAfterStateRead = func(candidateID, viewerID uuid.UUID) {
		if candidateID != sender || viewerID != viewer {
			return
		}
		close(stateRead)
		<-releaseSnapshot
	}

	snapshotDone := make(chan error, 1)
	go func() {
		snapshotDone <- hub.sendCustomTextSnapshot(context.Background(), viewerClient)
	}()
	awaitCustomTextSignal(t, candidatesRead)
	_, err := db.Exec(`
		UPDATE user_presence_settings
		SET custom_text_tier = 1
		WHERE user_id = $1
	`, sender)
	require.NoError(t, err)
	close(releaseCandidates)
	awaitCustomTextSignal(t, stateRead)

	// The viewer is not authorized for the old Friends-tier payload. A writer
	// that tries to expand the tier and replace the text after the final state
	// read must wait for the snapshot authorization and enqueue boundary.
	newText := "new server-visible status"
	updateAttempted := make(chan struct{})
	updateDone := make(chan error, 1)
	go func() {
		close(updateAttempted)
		_, err := db.Exec(`
			UPDATE user_presence_settings
			SET custom_text_tier = 2,
			    custom_text = $2,
			    custom_text_emoji = NULL,
			    updated_at = NOW()
			WHERE user_id = $1
		`, sender, newText)
		updateDone <- err
	}()
	awaitCustomTextSignal(t, updateAttempted)
	releaseSnapshotRead()
	require.NoError(t, awaitCustomTextSignal(t, snapshotDone))
	require.NoError(t, awaitCustomTextSignal(t, updateDone))

	assertNoMessage(t, viewerClient)
}

func TestSendCustomTextSnapshot_SharedGateOrdersOldSnapshotBeforeNewerClearUpdate(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	hub.SetPresenceHistoryService(service)
	viewer := insertCTUser(t, db, "ctsnapshotorderedviewer")
	sender := insertCTUser(t, db, "ctsnapshotorderedsender")
	makeFriends(t, db, sender, viewer)
	setCustomText(t, db, sender, 1, "old snapshot state", "")
	viewerClient := connectClient(hub, viewer)
	stateRead := make(chan struct{})
	releaseSnapshot := make(chan struct{})
	hub.customTextSnapshotAfterStateRead = func(candidateID, viewerID uuid.UUID) {
		if candidateID == sender && viewerID == viewer {
			close(stateRead)
			<-releaseSnapshot
		}
	}
	snapshotDone := make(chan error, 1)
	go func() {
		snapshotDone <- hub.sendCustomTextSnapshot(context.Background(), viewerClient)
	}()
	awaitCustomTextSignal(t, stateRead)

	operationID := uuid.New()
	writerAttempted := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		close(writerAttempted)
		writerDone <- service.WithSender(context.Background(), sender, func() (returnErr error) {
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				return err
			}
			defer func() {
				if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone && returnErr == nil {
					returnErr = rollbackErr
				}
			}()
			if _, err := tx.Exec(`
				UPDATE user_presence_settings
				SET custom_text_tier = 1,
				    custom_text = 'new committed state',
				    custom_text_emoji = NULL,
				    presence_settings_version = 1,
				    presence_settings_operation_id = $2,
				    updated_at = clock_timestamp()
				WHERE user_id = $1
			`, sender, operationID); err != nil {
				return err
			}
			if _, err := tx.Exec(`
				INSERT INTO presence_settings_pending_operations (
					user_id,
					operation_id,
					prior_settings_version,
					created_at,
					reconcile_after
				)
				VALUES ($1, $2, 0, clock_timestamp(), clock_timestamp() + INTERVAL '30 seconds')
			`, sender, operationID); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			ack, err := hub.DeliverCustomText(context.Background(), presencehistory.DeliveryPlan{
				Mode:             presencehistory.DeliveryExactDelta,
				OperationID:      operationID,
				SenderID:         sender,
				ClearRecipients:  map[uuid.UUID]bool{viewer: true},
				UpdateRecipients: map[uuid.UUID]bool{viewer: true},
				Payload:          &presencehistory.CustomTextState{Text: "new committed state"},
			})
			if err != nil {
				return err
			}
			if ack.OperationID != operationID {
				return fmt.Errorf("delivery acknowledgement mismatch")
			}
			return nil
		})
	}()
	awaitCustomTextSignal(t, writerAttempted)
	close(releaseSnapshot)
	require.NoError(t, awaitCustomTextSignal(t, snapshotDone))
	require.NoError(t, awaitCustomTextSignal(t, writerDone))

	oldUpdate := readClientMsg(t, viewerClient)
	newClear := readClientMsg(t, viewerClient)
	newUpdate := readClientMsg(t, viewerClient)
	require.Equal(t, "rich_presence_update", oldUpdate["type"])
	oldPayload := oldUpdate["data"].(map[string]interface{})["payload"].(map[string]interface{})
	assert.Equal(t, "old snapshot state", oldPayload["text"])
	assert.Equal(t, "rich_presence_clear", newClear["type"])
	require.Equal(t, "rich_presence_update", newUpdate["type"])
	newPayload := newUpdate["data"].(map[string]interface{})["payload"].(map[string]interface{})
	assert.Equal(t, "new committed state", newPayload["text"])
}

func TestSendCustomTextSnapshot_CommitsBeforeReplayAndOrdersCrossServiceClearAfterIt(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	snapshotService := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	writerService := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	hub.SetPresenceHistoryService(snapshotService)
	require.NoError(t, writerService.BindDelivery(hub))

	viewer := insertCTUser(t, db, "ctsnapshotcrossserviceviewer")
	sender := insertCTUser(t, db, "ctsnapshotcrossservicesender")
	makeFriends(t, db, sender, viewer)
	setCustomText(t, db, sender, 1, "old cross-service snapshot", "")
	viewerClient := connectClient(hub, viewer)
	viewerClient.beginBootstrap()

	stateRead := make(chan struct{})
	releaseStateRead := make(chan struct{})
	enqueueStarted := make(chan struct{})
	releaseEnqueue := make(chan struct{})
	snapshotDone := make(chan error, 1)
	writerFinished := make(chan struct{})
	writerResult := make(chan error, 1)
	var releaseStateReadOnce sync.Once
	var releaseEnqueueOnce sync.Once
	var stateReadOnce sync.Once
	var enqueueStartedOnce sync.Once
	writerStarted := false
	releaseSnapshotState := func() {
		releaseStateReadOnce.Do(func() { close(releaseStateRead) })
	}
	releaseSnapshotEnqueue := func() {
		releaseEnqueueOnce.Do(func() { close(releaseEnqueue) })
	}
	t.Cleanup(func() {
		releaseSnapshotState()
		releaseSnapshotEnqueue()
		select {
		case <-snapshotDone:
		case <-time.After(10 * time.Second):
		}
		if writerStarted {
			select {
			case <-writerFinished:
			case <-time.After(10 * time.Second):
			}
		}
	})

	hub.customTextSnapshotAfterStateRead = func(candidateID, viewerID uuid.UUID) {
		if candidateID != sender || viewerID != viewer {
			return
		}
		stateReadOnce.Do(func() { close(stateRead) })
		<-releaseStateRead
	}
	hub.customTextFrameMarshaler = func(
		senderID uuid.UUID,
		payload *CustomTextPayload,
	) ([]byte, error) {
		if senderID == sender && payload != nil && payload.Text == "old cross-service snapshot" {
			enqueueStartedOnce.Do(func() { close(enqueueStarted) })
			<-releaseEnqueue
		}
		return marshalCustomTextFrame(senderID, payload)
	}

	go func() {
		snapshotDone <- hub.sendCustomTextSnapshot(context.Background(), viewerClient)
	}()
	awaitCustomTextSignal(t, stateRead)

	writerBackendPIDs := make(chan int, 8)
	restoreWriterHooks := writerService.SetTransactionTestHooks(
		presencehistory.TransactionTestHooks{
			Begin: func(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
				tx, err := db.BeginTx(ctx, options)
				if err != nil {
					return nil, err
				}
				var backendPID int
				if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
					_ = tx.Rollback()
					return nil, err
				}
				writerBackendPIDs <- backendPID
				return tx, nil
			},
		},
	)
	t.Cleanup(restoreWriterHooks)
	writerStarted = true
	go func() {
		defer close(writerFinished)
		writerResult <- runCustomTextMasterOffWriter(
			context.Background(),
			writerService,
			sender,
			viewer,
		)
	}()

	writerBackendPID := awaitCustomTextSignal(t, writerBackendPIDs)
	requireCustomTextDatabaseWriterBlocked(
		t,
		db,
		writerBackendPID,
		writerFinished,
		"snapshot final-state authorization",
	)
	releaseSnapshotState()
	awaitCustomTextSignal(t, enqueueStarted)
	// The read-only authorization transaction must commit before collection.
	// That releases the cross-service database writer, whose clear is buffered
	// as live state while this committed snapshot replay is still paused.
	awaitCustomTextSignal(t, writerFinished)
	require.NoError(t, awaitCustomTextSignal(t, writerResult))
	releaseSnapshotEnqueue()

	require.NoError(t, awaitCustomTextSignal(t, snapshotDone))
	require.True(t, hub.completeClientBootstrap(viewerClient, []byte(`{"type":"presence_snapshot"}`)))

	require.Equal(t, "presence_snapshot", readClientMsg(t, viewerClient)["type"])
	oldUpdate := readClientMsg(t, viewerClient)
	newClear := readClientMsg(t, viewerClient)
	require.Equal(t, "rich_presence_update", oldUpdate["type"])
	oldPayload := oldUpdate["data"].(map[string]interface{})["payload"].(map[string]interface{})
	assert.Equal(t, "old cross-service snapshot", oldPayload["text"])
	require.Equal(t, "rich_presence_clear", newClear["type"])
	select {
	case trailing := <-viewerClient.Send:
		t.Fatalf("unexpected trailing custom-text frame after master-off clear: %s", trailing)
	default:
	}
}

func TestClearCustomTextForPresenceAudience_ScopesClearToBaseAudience(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	sender := insertCTUser(t, db, "ctscopedclearsender")
	viewer := insertCTUser(t, db, "ctscopedclearviewer")
	unrelated := insertCTUser(t, db, "ctscopedclearunrelated")
	makeFriends(t, db, sender, viewer)
	viewerClient := connectClient(hub, viewer)
	unrelatedClient := connectClient(hub, unrelated)

	hub.ClearCustomTextForPresenceAudience(sender)

	message := readClientMsg(t, viewerClient)
	assert.Equal(t, "rich_presence_clear", message["type"])
	data := message["data"].(map[string]interface{})
	assert.Equal(t, sender.String(), data["user_id"])
	assert.Equal(t, "custom_text", data["category"])
	assertNoMessage(t, unrelatedClient)
}

func TestClearCustomTextForPresenceAudience_DatabaseWaitIsBounded(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://concord:" + hubTestDBPassword + "@localhost:5432/concord?sslmode=disable" //nolint:gosec
	}
	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	defer func() { _ = db.Close() }()
	hub := NewHub(db, setupHubTestRedis(t))
	sender := uuid.New()
	viewer := uuid.New()
	viewerClient := connectClient(hub, viewer)

	db.SetMaxOpenConns(1)
	heldConnection, err := db.Conn(context.Background())
	require.NoError(t, err)

	done := make(chan struct{})
	startedAt := time.Now()
	go func() {
		defer close(done)
		hub.ClearCustomTextForPresenceAudience(sender)
	}()

	const maximumWait = 5 * time.Second
	select {
	case <-done:
		assert.Less(t, time.Since(startedAt), maximumWait)
	case <-time.After(maximumWait):
		db.SetMaxOpenConns(2)
		_ = heldConnection.Close()
		t.Fatal("reset Custom Status clear blocked indefinitely waiting for a database connection")
	}
	require.NoError(t, heldConnection.Close())
	assertNoMessage(t, viewerClient)
}

func TestSendCustomTextSnapshot_RecipientOverrideExcludedThenRestored(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctsnapoverrideviewer")
	sender := insertCTUser(t, db, "ctsnapoverridesender")
	makeFriends(t, db, sender, viewer)
	setCustomText(t, db, sender, 1, "private focus", "")
	excludeCustomTextViewer(t, db, sender, viewer)

	excludedClient := connectClient(hub, viewer)
	require.NoError(t, hub.sendCustomTextSnapshot(context.Background(), excludedClient))
	assertNoMessage(t, excludedClient)

	_, err := db.Exec(`
		DELETE FROM user_presence_overrides
		WHERE sender_id = $1 AND category = 'custom_text' AND target_user_id = $2
	`, sender.String(), viewer.String())
	require.NoError(t, err)
	var remainingOverrides int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM user_presence_overrides
		WHERE sender_id = $1 AND category = 'custom_text' AND target_user_id = $2
	`, sender, viewer).Scan(&remainingOverrides))
	require.Zero(t, remainingOverrides)
	var friendshipCount int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM friendships
		WHERE status = 'accepted'
		  AND ((requester_id = $1 AND addressee_id = $2)
		    OR (requester_id = $2 AND addressee_id = $1))
	`, sender, viewer).Scan(&friendshipCount))
	require.Equal(t, 1, friendshipCount)
	audience, err := presence.ComputeCustomTextAudience(context.Background(), db, sender)
	require.NoError(t, err)
	require.True(t, audience[viewer], "viewer must be restored before the replacement read")

	restoredClient := connectClient(hub, viewer)
	require.NoError(t, hub.sendCustomTextSnapshot(context.Background(), restoredClient))
	msg := readClientMsg(t, restoredClient)
	require.Equal(t, "rich_presence_update", msg["type"])
	data := msg["data"].(map[string]interface{})
	require.Equal(t, sender.String(), data["user_id"])
	payload := data["payload"].(map[string]interface{})
	require.Equal(t, "private focus", payload["text"])
}

// TestSendCustomTextSnapshot_ServerPeerExcludedAtFriendsTier is a focused privacy
// lock: a sender at Friends tier (1) whose ONLY relation to the viewer is a shared
// server must NOT appear in the viewer's snapshot. (At tier 2 they would.)
func TestSendCustomTextSnapshot_ServerPeerExcludedAtFriendsTier(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctsnapviewer2")
	sender := insertCTUser(t, db, "ctsnapserveronly")

	// Shared server only, no friendship. Sender is Friends-tier.
	shareServer(t, db, sender, sender, viewer)
	setCustomText(t, db, sender, 1, "friends only status", "")

	viewerClient := connectClient(hub, viewer)
	require.NoError(t, hub.sendCustomTextSnapshot(context.Background(), viewerClient))

	// PRIVACY LOCK: nothing delivered — a Friends-tier sender excludes a
	// server-only peer from their custom-text audience.
	assertNoMessage(t, viewerClient)
}

// TestSendCustomTextSnapshot_ServerPeerIncludedAtServersTier confirms the tier is
// honored in the snapshot direction too: the same server-only peer DOES appear
// when the sender is at Servers tier (2).
func TestSendCustomTextSnapshot_ServerPeerIncludedAtServersTier(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctsnapviewer3")
	sender := insertCTUser(t, db, "ctsnapserverstier")

	shareServer(t, db, sender, sender, viewer)
	setCustomText(t, db, sender, 2, "servers can see me", "")

	viewerClient := connectClient(hub, viewer)
	require.NoError(t, hub.sendCustomTextSnapshot(context.Background(), viewerClient))

	msg := readClientMsg(t, viewerClient)
	assert.Equal(t, "rich_presence_update", msg["type"])
	data := msg["data"].(map[string]interface{})
	assert.Equal(t, sender.String(), data["user_id"])
	payload := data["payload"].(map[string]interface{})
	assert.Equal(t, "servers can see me", payload["text"])
}

// TestSendCustomTextSnapshot_TierOffExcluded confirms tier 0 (Off) senders are
// never snapshotted even to a friend.
func TestSendCustomTextSnapshot_TierOffExcluded(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctsnapviewer4")
	sender := insertCTUser(t, db, "ctsnapoff")

	makeFriends(t, db, sender, viewer)
	// Tier 0 (Off) with text present — the candidate query filters tier > 0.
	setCustomText(t, db, sender, 0, "should be hidden", "")

	viewerClient := connectClient(hub, viewer)
	require.NoError(t, hub.sendCustomTextSnapshot(context.Background(), viewerClient))

	// PRIVACY LOCK: tier Off ⇒ no audience ⇒ nothing delivered even to a friend.
	assertNoMessage(t, viewerClient)
}

// --- #1233 fail-closed / guard-branch coverage ---
//
// The tests below lock the defensive branches that the audience-path tests above
// never exercise: the DB-free hub guards, the fail-closed DB-error paths, the
// snapshot self-skip, and the non-blocking send. These are the risk: privacy
// "never leak on error" guarantees — a DB error must mean "send nothing", never
// "send to everyone".

// dbFreeHub builds a zero-value hub with only the client maps initialized and a
// nil *sql.DB, mirroring the DB-free unit-hub pattern in hub_coverage_test.go.
// It is sufficient for the `h.db == nil` guard branches: those return before any
// DB or lock access, but BroadcastCustomText's deferred RUnlock still needs a
// usable mutex (zero-value sync.RWMutex is ready to use) and the loop needs a
// non-nil userClients map only on the non-guard path.
func dbFreeHub() *Hub {
	return &Hub{
		clients:     make(map[uuid.UUID]*Client),
		userClients: make(map[uuid.UUID]map[uuid.UUID]bool),
	}
}

// closedDBHub returns a real hub whose underlying *sql.DB has been closed, so
// every subsequent query fails — the most practical trigger for the fail-closed
// error branches (mirrors the closed-pool pattern in
// internal/users/handlers_dberror_test.go). The pool is opened INLINE here (not
// via setupHubTestDB) on purpose: setupHubTestDB registers a t.Cleanup that runs
// a TRUNCATE, which would error against an already-closed pool. No data is
// seeded because every query fails before reading any rows. Returns a fresh
// sender/viewer pair only so callers have stable IDs to drive the fail path.
func closedDBHub(t *testing.T) (*Hub, uuid.UUID, uuid.UUID) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://concord:" + hubTestDBPassword + "@localhost:5432/concord?sslmode=disable" //nolint:gosec
	}
	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err)
	// Close immediately so every subsequent query/exec returns
	// "sql: database is closed".
	require.NoError(t, db.Close())

	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	hub.SetPresenceHistoryService(presencehistory.NewService(db, presencehistory.DisclosureState{}, false))
	return hub, uuid.New(), uuid.New()
}

// TestBroadcastCustomText_NilDB_NoPanicNoSend covers the `h.db == nil` guard in
// BroadcastCustomText (lines 53-58): a DB-free hub must return without panicking
// and deliver nothing.
func TestSendCustomTextSnapshot_NilDB_NoPanicNoSend(t *testing.T) {
	hub := dbFreeHub()
	viewer := uuid.New()
	viewerClient := connectClient(hub, viewer)

	var snapshotErr error
	assert.NotPanics(t, func() {
		snapshotErr = hub.sendCustomTextSnapshot(context.Background(), viewerClient)
	})
	require.NoError(t, snapshotErr)
	assertNoMessage(t, viewerClient)
}

// TestSendCustomTextSnapshot_ClosedDB_FailsClosed covers the candidate-query
// error branch in sendCustomTextSnapshot (lines 139-144) AND the query-error
// branch in customTextCandidates (lines 198-200): a closed DB means the
// candidate query errors, so the snapshot emits nothing.
func TestSendCustomTextSnapshot_ClosedDB_FailsClosed(t *testing.T) {
	hub, _, viewer := closedDBHub(t)

	viewerClient := connectClient(hub, viewer)

	var snapshotErr error
	assert.NotPanics(t, func() {
		snapshotErr = hub.sendCustomTextSnapshot(context.Background(), viewerClient)
	})
	require.Error(t, snapshotErr)
	assertNoMessage(t, viewerClient)
}

// TestCustomTextCandidates_ClosedDB_ReturnsError directly asserts the
// customTextCandidates query-error path (lines 198-200): a closed DB yields a
// non-nil error and a nil slice (caller fails closed on it).
func TestCustomTextCandidates_ClosedDB_ReturnsError(t *testing.T) {
	hub, _, viewer := closedDBHub(t)

	out, err := hub.customTextCandidates(context.Background(), viewer)
	require.Error(t, err)
	assert.Nil(t, out)
}

// TestSendCustomTextSnapshot_SelfCandidateSkipped covers the self-skip branch
// (lines 147-148): when the connecting viewer is themselves a custom-text
// candidate, their own row is skipped (self is delivered via live self-sync, not
// the snapshot of others).
func TestSendCustomTextSnapshot_SelfCandidateSkipped(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctselfcand")
	// The viewer has custom text set, so they ARE a candidate row — but the
	// snapshot of OTHERS must skip self.
	setCustomText(t, db, viewer, 2, "my own status", "🙂")

	viewerClient := connectClient(hub, viewer)
	require.NoError(t, hub.sendCustomTextSnapshot(context.Background(), viewerClient))

	// Self row skipped ⇒ no frame delivered to the viewer about themselves.
	assertNoMessage(t, viewerClient)
}

// TestSendCustomTextSnapshot_SendBufferFull covers the privacy-critical
// backpressure branch: the worker returns and disconnects instead of silently
// dropping an authorized replacement frame.
func TestSendCustomTextSnapshot_SendBufferFullDisconnects(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctbufviewer")
	sender := insertCTUser(t, db, "ctbufsender")

	// viewer is in sender's Friends-tier audience, so the snapshot WOULD send.
	makeFriends(t, db, sender, viewer)
	setCustomText(t, db, sender, 1, "would be sent", "")

	// Connect the viewer with a zero-capacity Send channel and pre-fill so the
	// send blocks and hits the `default:` drop path.
	clientID := uuid.New()
	viewerClient := &Client{
		ID:       clientID,
		UserID:   viewer,
		Send:     make(chan []byte), // unbuffered + no reader ⇒ send never ready
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[clientID] = viewerClient
	hub.userClients[viewer] = map[uuid.UUID]bool{clientID: true}
	disconnected := false
	hub.customTextClientDisconnect = func(got *Client) error {
		assert.Same(t, viewerClient, got)
		disconnected = true
		return nil
	}

	// Must not block / panic — the default branch disconnects the stale client.
	done := make(chan error, 1)
	go func() {
		done <- hub.sendCustomTextSnapshot(context.Background(), viewerClient)
	}()
	select {
	case snapshotErr := <-done:
		// good — returned without blocking on the unbuffered Send channel.
		require.ErrorIs(t, snapshotErr, errClientBootstrapOverflow)
	case <-time.After(2 * time.Second):
		t.Fatal("sendCustomTextSnapshot blocked instead of disconnecting the full queue")
	}
	assert.True(t, disconnected)
}
