package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarshalPresenceSnapshot_EnrichesAuthorizedRichOnlySendersDeterministically(t *testing.T) {
	onlineID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	offlineID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	richOnlyID := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	base := map[uuid.UUID]string{
		offlineID: statusOffline,
		onlineID:  statusOnline,
	}
	activity := presence.ActivitySnapshot{
		richOnlyID: {
			presence.CategoryPrivateCall: {
				Payload:   json.RawMessage(`{"call_type":"dm","started_at":1784088000}`),
				UpdatedAt: 1784088002,
			},
			presence.CategoryServerVoice: {
				Minimized: true,
				Payload:   json.RawMessage(`{"server_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}`),
				UpdatedAt: 1784088001,
			},
		},
	}

	first, err := marshalPresenceSnapshot(base, activity)
	require.NoError(t, err)
	second, err := marshalPresenceSnapshot(
		map[uuid.UUID]string{onlineID: statusOnline, offlineID: statusOffline},
		presence.ActivitySnapshot{
			richOnlyID: {
				presence.CategoryServerVoice: activity[richOnlyID][presence.CategoryServerVoice],
				presence.CategoryPrivateCall: activity[richOnlyID][presence.CategoryPrivateCall],
			},
		},
	)
	require.NoError(t, err)
	assert.Equal(t, first, second, "snapshot bytes must not depend on map insertion order")

	var message struct {
		Type string `json:"type"`
		Data struct {
			OnlineUserIDs []string `json:"online_user_ids"`
			Users         []struct {
				UserID       string                               `json:"user_id"`
				Status       string                               `json:"status"`
				RichPresence map[string]richPresenceSnapshotEntry `json:"rich_presence"`
			} `json:"users"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(first, &message))
	assert.Equal(t, "presence_snapshot", message.Type)
	assert.Equal(t, []string{onlineID.String()}, message.Data.OnlineUserIDs)
	require.Len(t, message.Data.Users, 3)
	assert.Equal(t, onlineID.String(), message.Data.Users[0].UserID)
	assert.Nil(t, message.Data.Users[0].RichPresence, "empty rich_presence must be omitted")
	assert.Equal(t, offlineID.String(), message.Data.Users[1].UserID)
	assert.Nil(t, message.Data.Users[1].RichPresence, "empty rich_presence must be omitted")
	assert.Equal(t, richOnlyID.String(), message.Data.Users[2].UserID)
	assert.Equal(t, statusOffline, message.Data.Users[2].Status)
	require.Len(t, message.Data.Users[2].RichPresence, 2)
	assert.True(t, message.Data.Users[2].RichPresence[string(presence.CategoryServerVoice)].Minimized)
	assert.Equal(
		t,
		int64(1784088002),
		message.Data.Users[2].RichPresence[string(presence.CategoryPrivateCall)].UpdatedAt,
	)
}

func TestMarshalPresenceSnapshot_RejectsMalformedTrustedActivity(t *testing.T) {
	_, err := marshalPresenceSnapshot(nil, presence.ActivitySnapshot{
		uuid.New(): {
			presence.CategoryServerVoice: {
				Payload:   json.RawMessage(`[]`),
				UpdatedAt: 1784088000,
			},
		},
	})
	assert.ErrorIs(t, err, presence.ErrInvalidActivitySnapshot)
}

func TestMarshalPresenceSnapshot_RejectsUpdatedAtAboveWireCap(t *testing.T) {
	_, err := marshalPresenceSnapshot(nil, presence.ActivitySnapshot{
		uuid.New(): {
			presence.CategoryServerVoice: {
				Payload:   json.RawMessage(`{"channel_id":"current"}`),
				UpdatedAt: presence.MaxActivityUnixSeconds + 1,
			},
		},
	})
	assert.ErrorIs(t, err, presence.ErrInvalidActivitySnapshot)
}

func TestCapturePresenceSnapshotSeedBoundsConnectedIDsAndRetainsViewer(t *testing.T) {
	const wantLimit = 512

	hub := NewHub(nil, nil)
	for range wantLimit + 128 {
		userID := uuid.New()
		hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}
		hub.hiddenPresence[userID] = statusInvisible
	}
	viewerID := uuid.New()
	hub.userClients[viewerID] = map[uuid.UUID]bool{uuid.New(): true}
	hub.hiddenPresence[viewerID] = statusDND

	seed := hub.capturePresenceSnapshotSeed(viewerID)

	require.Len(t, seed.connectedIDs, wantLimit)
	require.Len(t, seed.hidden, wantLimit)
	assert.True(t, seed.truncated,
		"the cap is the ONLY signal that connectedIDs is a proper subset of who is "+
			"connected; without it the coverage computation cannot tell a cap-excluded "+
			"sender from an offline one")
	assert.Contains(t, seed.connectedIDs, viewerID,
		"a bounded reconnect seed must always retain its connected viewer")
	assert.Equal(t, statusDND, seed.hidden[viewerID])
	for _, userID := range seed.connectedIDs {
		assert.Equal(t, hub.hiddenPresence[userID], seed.hidden[userID])
	}
}

// TestBasePresenceSnapshotExcludesNonAudience is the #47 snapshot leak lock,
// migrated from the deleted TestSendPresenceSnapshotExcludesNonAudience (#1654).
// It drives the live bootstrap path — capturePresenceSnapshotSeed +
// loadBasePresenceSnapshot — rather than the legacy sendPresenceSnapshot wrapper
// it used to call.
func TestBasePresenceSnapshotExcludesNonAudience(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	viewer := presenceTestUser(t, db)
	friend := presenceTestUser(t, db)
	stranger := presenceTestUser(t, db)
	presenceTestFriendship(t, db, viewer, friend)

	for _, userID := range []uuid.UUID{viewer, friend, stranger} {
		client := &Client{ID: uuid.New(), UserID: userID, Send: make(chan []byte, 10)}
		hub.clients[client.ID] = client
		hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}
		require.NoError(t, redisClient.Set(
			context.Background(), presence.StatusRedisKey(userID), statusOnline, time.Minute,
		).Err())
	}

	seed := hub.capturePresenceSnapshotSeed(viewer)
	base, err := hub.loadBasePresenceSnapshot(context.Background(), seed)

	require.NoError(t, err)
	assert.Contains(t, base, friend, "a friend must appear in the viewer's snapshot")
	assert.NotContains(t, base, stranger,
		"#47 leak: an unrelated user appeared in the viewer's snapshot")
}

func TestLoadBasePresenceSnapshot_UsesSenderOwnedInverseAudience(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	viewerID := insertCTUser(t, db, "baseinverseviewer")
	directID := insertCTUser(t, db, "baseinversedirect")
	fofSenderID := insertCTUser(t, db, "baseinversefofsender")
	mutualID := insertCTUser(t, db, "baseinversemutual")
	serverPeerID := insertCTUser(t, db, "baseinverseserverpeer")
	unrelatedID := insertCTUser(t, db, "baseinverseunrelated")
	makeFriends(t, db, viewerID, directID)
	makeFriends(t, db, fofSenderID, mutualID)
	makeFriends(t, db, mutualID, viewerID)
	shareServer(t, db, viewerID, viewerID, serverPeerID)
	_, err := db.Exec(`
		INSERT INTO privacy_settings (user_id, dm_friends_of_friends)
		VALUES ($1, FALSE), ($2, TRUE)
	`, viewerID, fofSenderID)
	require.NoError(t, err)
	seed := presenceSnapshotSeed{
		viewerID: viewerID,
		connectedIDs: []uuid.UUID{
			viewerID, directID, fofSenderID, serverPeerID, unrelatedID,
		},
		hidden: map[uuid.UUID]string{},
	}
	for _, userID := range seed.connectedIDs {
		require.NoError(t, redisClient.Set(
			context.Background(), "presence:"+userID.String(), statusOnline, time.Minute,
		).Err())
	}

	base, err := hub.loadBasePresenceSnapshot(context.Background(), seed)

	require.NoError(t, err)
	assert.Equal(t, map[uuid.UUID]string{
		viewerID:     statusOnline,
		directID:     statusOnline,
		fofSenderID:  statusOnline,
		serverPeerID: statusOnline,
	}, base)
	assert.NotContains(t, base, unrelatedID)

	_, err = db.Exec(`
		UPDATE privacy_settings
		SET dm_friends_of_friends = (user_id = $1)
		WHERE user_id IN ($1, $2)
	`, viewerID, fofSenderID)
	require.NoError(t, err)
	base, err = hub.loadBasePresenceSnapshot(context.Background(), seed)
	require.NoError(t, err)
	assert.NotContains(t, base, fofSenderID,
		"the viewer's FoF preference must not authorize a sender")
}

func TestLoadBasePresenceSnapshot_PropagatesGlobalDependencyFailure(t *testing.T) {
	tests := map[string]func(*testing.T) *Hub{
		"database": func(t *testing.T) *Hub {
			hub, _, _ := closedDBHub(t)
			return hub
		},
		"redis": func(t *testing.T) *Hub {
			redisClient := setupHubTestRedis(t)
			require.NoError(t, redisClient.Close())
			return NewHub(nil, redisClient)
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			viewerID := uuid.New()
			hub := setup(t)

			base, err := hub.loadBasePresenceSnapshot(context.Background(), presenceSnapshotSeed{
				viewerID: viewerID, connectedIDs: []uuid.UUID{viewerID},
			})

			require.Error(t, err)
			assert.Nil(t, base, "a global dependency failure must not fabricate a partial base snapshot")
		})
	}
}

func TestRunClientBootstrap_GlobalDependencyFailureDisconnectsWithoutSnapshotOrReplay(t *testing.T) {
	tests := map[string]func(*testing.T) *Hub{
		"database": func(t *testing.T) *Hub {
			hub, _, _ := closedDBHub(t)
			healthyDB := setupHubTestDB(t)
			hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
				// Recover immediately after the initial audience query. If that failure
				// were swallowed, every later step could succeed and publish a partial
				// snapshot plus the already buffered replay.
				hub.db = healthyDB
				return make(presence.ActivitySnapshot), nil
			}
			return hub
		},
		"redis": func(t *testing.T) *Hub {
			redisClient := setupHubTestRedis(t)
			require.NoError(t, redisClient.Close())
			return NewHub(nil, redisClient)
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			viewerID := uuid.New()
			hub := setup(t)
			disconnected := false
			hub.customTextClientDisconnect = func(got *Client) error {
				assert.Equal(t, viewerID, got.UserID)
				disconnected = true
				return nil
			}
			client := &Client{UserID: viewerID, Send: make(chan []byte, 4), Hub: hub}
			client.beginBootstrap()
			require.NoError(t, client.appendBootstrapReplay([]byte(`{"type":"must_not_escape"}`)))

			hub.runClientBootstrap(context.Background(), client, presenceSnapshotSeed{
				viewerID: viewerID, connectedIDs: []uuid.UUID{viewerID},
			})

			assert.True(t, disconnected)
			assert.Empty(t, client.Send, "no partial snapshot or buffered replay may escape")
		})
	}
}

func TestBasePresenceCandidateQueryStaysCandidateCorrelated(t *testing.T) {
	source, err := os.ReadFile("activity_bootstrap.go")
	require.NoError(t, err)
	assert.NotContains(t, string(source), "accepted_edges AS",
		"bounded reconnect must not materialize the global friendship graph")
}

func TestRunClientBootstrap_ReauthorizesBasePresenceAtPublication(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewerID := insertCTUser(t, db, "basefinalviewer")
	senderID := insertCTUser(t, db, "basefinalsender")
	makeFriends(t, db, viewerID, senderID)
	for _, userID := range []uuid.UUID{viewerID, senderID} {
		require.NoError(t, hub.redis.Set(
			context.Background(), "presence:"+userID.String(), statusOnline, time.Minute,
		).Err())
	}
	initialLoadComplete := make(chan struct{})
	releasePublication := make(chan struct{})
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		close(initialLoadComplete)
		<-releasePublication
		return make(presence.ActivitySnapshot), nil
	}
	client := &Client{UserID: viewerID, Send: make(chan []byte, 8), Hub: hub}
	client.beginBootstrap()
	done := make(chan struct{})
	go func() {
		hub.runClientBootstrap(context.Background(), client, presenceSnapshotSeed{
			viewerID: viewerID, connectedIDs: []uuid.UUID{viewerID, senderID},
			hidden: map[uuid.UUID]string{},
		})
		close(done)
	}()
	select {
	case <-initialLoadComplete:
	case <-time.After(time.Second):
		t.Fatal("initial base presence load did not finish")
	}
	_, err := db.Exec(`
		DELETE FROM friendships
		WHERE (requester_id = $1 AND addressee_id = $2)
		   OR (requester_id = $2 AND addressee_id = $1)
	`, viewerID, senderID)
	require.NoError(t, err)
	close(releasePublication)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bootstrap did not reach publication")
	}

	snapshot := readClientMsg(t, client)
	require.Equal(t, "presence_snapshot", snapshot["type"])
	users := snapshot["data"].(map[string]interface{})["users"].([]interface{})
	for _, rawUser := range users {
		assert.NotEqual(t, senderID.String(), rawUser.(map[string]interface{})["user_id"],
			"a relation revoked after preload must not survive publication reauthorization")
	}
}

func TestClientBootstrap_FlushesSnapshotThenCustomReplayThenLive(t *testing.T) {
	hub := NewHub(nil, nil)
	client := &Client{Send: make(chan []byte, 8), Hub: hub}
	client.beginBootstrap()

	snapshot := []byte(`{"type":"presence_snapshot"}`)
	customReplay := []byte(`{"type":"custom_replay"}`)
	liveRich := []byte(`{"type":"rich_presence_update"}`)
	liveBase := []byte(`{"type":"presence"}`)
	require.NoError(t, client.appendBootstrapReplay(customReplay))
	assert.Equal(t, bootstrapBufferEnqueued, client.bufferBootstrapLive(liveRich))
	assert.Equal(t, bootstrapBufferEnqueued, client.bufferBootstrapLive(liveBase))

	assert.True(t, hub.completeClientBootstrap(client, snapshot))
	assert.Equal(t, snapshot, <-client.Send)
	assert.Equal(t, customReplay, <-client.Send)
	assert.Equal(t, liveRich, <-client.Send)
	assert.Equal(t, liveBase, <-client.Send)
}

func TestDeliverPrivacyCriticalToClient_BootstrapOverflowDisconnectsWithoutPartialSend(t *testing.T) {
	hub := NewHub(nil, nil)
	client := &Client{Send: make(chan []byte, clientBootstrapFrameLimit+2), Hub: hub}
	client.beginBootstrap()
	for range clientBootstrapBufferedFrameLimit {
		require.NoError(t, client.appendBootstrapReplay([]byte(`{"type":"custom_replay"}`)))
	}
	disconnected := false
	hub.customTextClientDisconnect = func(got *Client) error {
		assert.Same(t, client, got)
		disconnected = true
		return nil
	}

	wasDisconnected, err := hub.deliverPrivacyCriticalToClient(
		context.Background(),
		client,
		[]byte(`{"type":"rich_presence_clear"}`),
	)

	require.NoError(t, err)
	assert.True(t, wasDisconnected)
	assert.True(t, disconnected)
	assert.Empty(t, client.Send, "bootstrap frames must stay private until the replacement is complete")
}

func TestClientBootstrap_CancelPreventsLateCompletionEnqueue(t *testing.T) {
	hub := NewHub(nil, nil)
	client := &Client{Send: make(chan []byte, 4), Hub: hub}
	client.beginBootstrap()
	require.NoError(t, client.appendBootstrapReplay([]byte(`{"type":"custom_replay"}`)))
	client.cancelBootstrap()

	assert.False(t, hub.completeClientBootstrap(client, []byte(`{"type":"presence_snapshot"}`)))
	assert.Empty(t, client.Send)
	assert.Equal(t, bootstrapBufferCanceled, client.bufferBootstrapLive([]byte(`{"type":"late"}`)))
	assert.False(t, client.enqueuePostBootstrap(context.Background(), []byte(`{"type":"late_voice"}`)))
}

func TestClientBootstrap_ConcurrentQueueFillAfterPreflightCannotBlockCompletion(t *testing.T) {
	hub := NewHub(nil, nil)
	client := &Client{Send: make(chan []byte, 2), Hub: hub}
	client.beginBootstrap()
	disconnected := false
	hub.customTextClientDisconnect = func(*Client) error {
		disconnected = true
		return nil
	}
	hub.clientBootstrapBeforeFlush = func() {
		client.Send <- []byte(`{"type":"concurrent_one"}`)
		client.Send <- []byte(`{"type":"concurrent_two"}`)
	}
	done := make(chan bool, 1)

	go func() {
		done <- hub.completeClientBootstrap(client, []byte(`{"type":"presence_snapshot"}`))
	}()

	select {
	case completed := <-done:
		assert.False(t, completed)
	case <-time.After(time.Second):
		t.Fatal("bootstrap completion blocked after a concurrent queue fill")
	}
	assert.True(t, disconnected)
	assert.Len(t, client.Send, 2)
}

func TestClientBootstrap_GenericWriterCannotInterleaveAfterSnapshotFrame(t *testing.T) {
	hub := NewHub(nil, nil)
	client := &Client{Send: make(chan []byte, 4), Hub: hub}
	client.beginBootstrap()
	snapshot := []byte(`{"type":"presence_snapshot"}`)
	replay := []byte(`{"type":"custom_replay"}`)
	live := []byte(`{"type":"rich_presence_update"}`)
	filler := []byte(`{"type":"generic_event"}`)
	require.NoError(t, client.appendBootstrapReplay(replay))
	assert.Equal(t, bootstrapBufferEnqueued, client.bufferBootstrapLive(live))
	writerAttempted := make(chan struct{})
	writerCompleted := make(chan struct{})
	hub.clientBootstrapAfterFirstFrame = func() {
		go func() {
			close(writerAttempted)
			client.enqueueOutbound(filler)
			close(writerCompleted)
		}()
		<-writerAttempted
		select {
		case <-writerCompleted:
			t.Error("generic writer interleaved while the replacement held sendMu")
		default:
		}
	}

	require.True(t, hub.completeClientBootstrap(client, snapshot))
	select {
	case <-writerCompleted:
	case <-time.After(time.Second):
		t.Fatal("generic writer did not resume after atomic replacement flush")
	}
	assert.Equal(t, snapshot, <-client.Send)
	assert.Equal(t, replay, <-client.Send)
	assert.Equal(t, live, <-client.Send)
	assert.Equal(t, filler, <-client.Send)
}

func TestClientOutbound_SendAndCloseAreSerialized(_ *testing.T) {
	for range 100 {
		client := &Client{Send: make(chan []byte, 1)}
		client.sendMu.Lock()
		started := make(chan struct{}, 2)
		done := make(chan struct{}, 2)
		go func() {
			started <- struct{}{}
			client.enqueueOutbound([]byte(`{"type":"race"}`))
			done <- struct{}{}
		}()
		go func() {
			started <- struct{}{}
			client.closeOutbound()
			done <- struct{}{}
		}()
		<-started
		<-started
		client.sendMu.Unlock()
		<-done
		<-done
		client.closeOutbound()
	}
}

func TestHubRun_ClientBootstrapKeepsRunResponsiveAndOrdersReplacementBeforeLive(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	viewerID := uuid.New()
	senderID := uuid.New()
	activityStarted := make(chan struct{})
	releaseActivity := make(chan struct{})
	var releaseOnce sync.Once
	hub.activitySnapshot = func(ctx context.Context, gotViewerID uuid.UUID) (presence.ActivitySnapshot, error) {
		assert.Equal(t, viewerID, gotViewerID)
		close(activityStarted)
		select {
		case <-releaseActivity:
			return presence.ActivitySnapshot{
				senderID: {
					presence.CategoryPrivateCall: {
						Payload:   json.RawMessage(`{"call_type":"dm","started_at":1784088000}`),
						UpdatedAt: 1784088001,
					},
				},
			}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	go hub.Run()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseActivity) })
		hub.Shutdown()
	})

	client := &Client{
		ID:                          uuid.New(),
		UserID:                      viewerID,
		Hub:                         hub,
		Send:                        make(chan []byte, 32),
		Channels:                    make(map[uuid.UUID]bool),
		activityRichPresenceCapable: true,
	}
	hub.register <- client
	assert.Equal(t, "connected", readClientMsg(t, client)["type"])
	select {
	case <-activityStarted:
	case <-time.After(time.Second):
		t.Fatal("activity snapshot did not start")
	}

	hub.BroadcastToAll(OutgoingMessage{Type: "bootstrap_run_probe"})
	require.NoError(t, hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         senderID,
		Category:         presence.CategoryPrivateCall,
		UpdateRecipients: map[uuid.UUID]bool{viewerID: true},
		Payload:          json.RawMessage(`{"call_type":"dm","started_at":1784088002}`),
		UpdatedAt:        1784088003,
	}))
	assert.Empty(t, client.Send, "privacy-critical live state must remain buffered during replacement")

	releaseOnce.Do(func() { close(releaseActivity) })
	snapshot := readClientMsg(t, client)
	require.Equal(t, "presence_snapshot", snapshot["type"])
	users := snapshot["data"].(map[string]interface{})["users"].([]interface{})
	var richOnly map[string]interface{}
	for _, rawUser := range users {
		user := rawUser.(map[string]interface{})
		if user["user_id"] == senderID.String() {
			richOnly = user
			break
		}
	}
	require.NotNil(t, richOnly)
	assert.Equal(t, statusOffline, richOnly["status"])
	assert.Contains(t, richOnly, "rich_presence")

	// Every remaining frame follows the replacement, which is the invariant this
	// test is named for and which the read above already proved. Their order
	// RELATIVE TO EACH OTHER is not a contract, and since #1654 it is not even
	// deterministic: the base-presence frame is now produced asynchronously and
	// applied by Run from presenceAudienceResults, so it races bootstrap_run_probe
	// (globalBroadcast), rich_presence_update, and the post-bootstrap
	// server_voice_counts. Measured over `-count=30`: presence inverts with
	// rich_presence_update, and can also land after server_voice_counts when the
	// audience query contends for the pool with the bootstrap's own reads.
	//
	// The replacement-before-live guarantee is untouched by that, and holds by
	// construction rather than by timing: a live frame raised during bootstrap is
	// buffered and replayed after the replacement, and one raised afterwards is
	// enqueued directly, which is later still. Pinning a fixed interleaving here
	// would be a latent flake wearing the costume of a stronger assertion.
	assert.ElementsMatch(t, []interface{}{
		"presence", "bootstrap_run_probe", "rich_presence_update", "server_voice_counts",
	}, []interface{}{
		readClientMsg(t, client)["type"],
		readClientMsg(t, client)["type"],
		readClientMsg(t, client)["type"],
		readClientMsg(t, client)["type"],
	}, "every live frame follows the replacement, in any order")
}

func TestHandleRegister_ActivitySnapshotFailureDisconnectsWithoutPartialReplacement(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	snapshotErr := errors.New("snapshot sentinel")
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		return nil, snapshotErr
	}
	disconnected := make(chan struct{}, 1)
	hub.customTextClientDisconnect = func(*Client) error {
		disconnected <- struct{}{}
		return nil
	}
	client := newTestClient(hub, uuid.New())

	hub.handleRegister(client)
	assert.Equal(t, "connected", readClientMsg(t, client)["type"])
	client.asyncWg.Wait()
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("failed replacement did not disconnect the client")
	}
	assert.Empty(t, client.Send, "no base, custom, rich, or voice frame may escape a failed replacement")
	hub.handleUnregister(client)
}

func TestLoadClientBootstrapActivity_CandidateLimitFallsBackToEmptyReset(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		return nil, presence.ErrActivitySnapshotCandidateLimit
	}

	activity, err := hub.loadClientBootstrapActivity(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, activity,
		"an authoritative empty replacement clears stale activity without disconnecting")
}

func TestRefreshBasePresenceForPublication_DropsDisconnectedCandidate(t *testing.T) {
	hub := NewHub(nil, nil)
	viewerID := uuid.New()
	senderID := uuid.New()
	hub.userClients[viewerID] = map[uuid.UUID]bool{uuid.New(): true}
	hub.userClients[senderID] = map[uuid.UUID]bool{uuid.New(): true}
	seed := presenceSnapshotSeed{
		viewerID:     viewerID,
		connectedIDs: []uuid.UUID{viewerID, senderID},
		hidden:       map[uuid.UUID]string{},
	}
	delete(hub.userClients, senderID)

	refreshed, err := hub.refreshBasePresenceForPublication(
		context.Background(), seed,
		map[uuid.UUID]string{viewerID: statusOnline, senderID: statusDND},
		map[uuid.UUID]bool{viewerID: true, senderID: true},
	)
	require.NoError(t, err)
	assert.Equal(t, map[uuid.UUID]string{viewerID: statusOnline}, refreshed)
}

// Regression for #2404.
func TestHiddenPresenceWritesSynchronizeWithBootstrapPublication(t *testing.T) {
	tests := []struct {
		name    string
		initial string
		mutate  func(*Hub, uuid.UUID)
		want    string
	}{
		{
			name: "set invisible",
			mutate: func(hub *Hub, userID uuid.UUID) {
				hub.setHiddenPresence(userID, statusInvisible)
			},
			want: statusInvisible,
		},
		{
			name:    "clear",
			initial: statusInvisible,
			mutate: func(hub *Hub, userID uuid.UUID) {
				hub.clearHiddenPresence(userID)
			},
			want: statusOnline,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hub := NewHub(nil, nil)
			viewerID := uuid.New()
			hub.userClients[viewerID] = map[uuid.UUID]bool{uuid.New(): true}
			if test.initial != "" {
				hub.hiddenPresence[viewerID] = test.initial
			}
			seed := presenceSnapshotSeed{
				viewerID:     viewerID,
				connectedIDs: []uuid.UUID{viewerID},
				hidden:       map[uuid.UUID]string{},
			}
			base := map[uuid.UUID]string{viewerID: statusOnline}
			visible := map[uuid.UUID]bool{viewerID: true}

			hub.mu.RLock()
			connected, _, _ := hub.currentBasePresenceCandidateLocked(
				seed, visible, true, viewerID,
			)
			if !connected {
				hub.mu.RUnlock()
				t.Fatal("bootstrap publication fixture did not include the viewer")
			}
			mutationDone := make(chan struct{})
			go func() {
				test.mutate(hub, viewerID)
				close(mutationDone)
			}()

			for {
				hub.currentBasePresenceCandidateLocked(seed, visible, true, viewerID)
				select {
				case <-mutationDone:
					hub.mu.RUnlock()
					t.Fatalf("%s completed while bootstrap publication held h.mu.RLock", test.name)
				default:
				}
				if !hub.mu.TryRLock() {
					break
				}
				hub.mu.RUnlock()
				runtime.Gosched()
			}
			hub.mu.RUnlock()
			<-mutationDone

			refreshed, err := hub.refreshBasePresenceForPublication(
				context.Background(), seed, base, visible,
			)
			require.NoError(t, err)
			assert.Equal(t, test.want, refreshed[viewerID])
		})
	}
}

func TestRunClientBootstrap_GlobalPreparationFailureDisconnectsAfterOrdinaryOverflow(t *testing.T) {
	hub := NewHub(nil, nil)
	preparationErr := errors.New("snapshot preparation sentinel")
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		return nil, preparationErr
	}
	disconnected := false
	hub.customTextClientDisconnect = func(*Client) error {
		disconnected = true
		return nil
	}
	viewerID := uuid.New()
	client := &Client{UserID: viewerID, Send: make(chan []byte, 4), Hub: hub}
	client.beginBootstrap()
	for range clientBootstrapBufferedFrameLimit {
		require.True(t, client.enqueueOutbound([]byte(`{"type":"ordinary"}`)))
	}
	require.False(t, client.enqueueOutbound([]byte(`{"type":"overflow"}`)))

	hub.runClientBootstrap(context.Background(), client, presenceSnapshotSeed{
		viewerID: viewerID, connectedIDs: []uuid.UUID{viewerID},
	})

	assert.True(t, disconnected, "bootstrapFailed records overflow, not a completed disconnect")
	assert.Empty(t, client.Send)
}

func TestRunClientBootstrap_FinalizesActivityAtPublicationBarrier(t *testing.T) {
	hub := NewHub(nil, nil)
	viewerID := uuid.New()
	senderID := uuid.New()
	client := &Client{UserID: viewerID, Send: make(chan []byte, 4), Hub: hub}
	client.beginBootstrap()
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		return presence.ActivitySnapshot{
			senderID: {
				presence.CategoryServerVoice: {
					Payload:   json.RawMessage(`{"channel_id":"projected"}`),
					UpdatedAt: 1784088000,
				},
			},
		}, nil
	}
	genericFrame := []byte(`{"type":"generic_after_barrier"}`)
	writerDone := make(chan struct{})
	finalizerCalls := 0
	hub.activitySnapshotFinalize = func(
		_ context.Context,
		gotViewerID uuid.UUID,
		projected presence.ActivitySnapshot,
		publish func(presence.ActivitySnapshot) error,
	) error {
		finalizerCalls++
		assert.Equal(t, viewerID, gotViewerID)
		assert.Contains(t, projected, senderID)
		writerStarted := make(chan struct{})
		go func() {
			close(writerStarted)
			client.enqueueOutbound(genericFrame)
			close(writerDone)
		}()
		<-writerStarted
		select {
		case <-writerDone:
		case <-time.After(time.Second):
			t.Error("generic writer blocked Hub work instead of joining the buffered live tail")
		}
		return publish(make(presence.ActivitySnapshot))
	}

	hub.runClientBootstrap(context.Background(), client, presenceSnapshotSeed{
		viewerID: viewerID, connectedIDs: []uuid.UUID{viewerID},
	})

	assert.Equal(t, 1, finalizerCalls)
	snapshot := readClientMsg(t, client)
	assert.Equal(t, "presence_snapshot", snapshot["type"])
	users := snapshot["data"].(map[string]interface{})["users"].([]interface{})
	for _, rawUser := range users {
		assert.NotEqual(t, senderID.String(), rawUser.(map[string]interface{})["user_id"])
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("generic writer did not resume after activity publication")
	}
	assert.Equal(t, genericFrame, <-client.Send)
}

func TestRunClientBootstrap_HoldsSenderGateThroughSnapshotEnqueue(t *testing.T) {
	hub := NewHub(nil, nil)
	coordinator := presencehistory.NewService(nil, presencehistory.DisclosureState{}, false)
	viewerID := uuid.New()
	senderID := uuid.New()
	client := &Client{UserID: viewerID, Send: make(chan []byte, 4), Hub: hub}
	client.beginBootstrap()
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		return make(presence.ActivitySnapshot), nil
	}

	writerStarted := make(chan struct{})
	writerEntered := make(chan struct{})
	writerDone := make(chan error, 1)
	hub.activitySnapshotFinalize = func(
		ctx context.Context,
		_ uuid.UUID,
		_ presence.ActivitySnapshot,
		publish func(presence.ActivitySnapshot) error,
	) error {
		return coordinator.WithSenders(ctx, []uuid.UUID{senderID}, func() error {
			go func() {
				close(writerStarted)
				writerDone <- coordinator.WithSender(context.Background(), senderID, func() error {
					close(writerEntered)
					return nil
				})
			}()
			<-writerStarted
			return publish(make(presence.ActivitySnapshot))
		})
	}
	hub.clientBootstrapAfterFirstFrame = func() {
		select {
		case <-writerEntered:
			t.Error("sender writer entered before the atomic snapshot enqueue completed")
		default:
		}
	}

	hub.runClientBootstrap(context.Background(), client, presenceSnapshotSeed{
		viewerID: viewerID, connectedIDs: []uuid.UUID{viewerID},
	})

	assert.Equal(t, "presence_snapshot", readClientMsg(t, client)["type"])
	select {
	case <-writerEntered:
	case <-time.After(time.Second):
		t.Fatal("sender writer did not resume after snapshot enqueue")
	}
	require.NoError(t, <-writerDone)
}

func TestRunClientBootstrap_FinalizerFailureDisconnectsWithoutSnapshot(t *testing.T) {
	hub := NewHub(nil, nil)
	finalizeErr := errors.New("activity finalization sentinel")
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		return make(presence.ActivitySnapshot), nil
	}
	hub.activitySnapshotFinalize = func(
		context.Context,
		uuid.UUID,
		presence.ActivitySnapshot,
		func(presence.ActivitySnapshot) error,
	) error {
		return finalizeErr
	}
	disconnected := false
	hub.customTextClientDisconnect = func(*Client) error {
		disconnected = true
		return nil
	}
	viewerID := uuid.New()
	client := &Client{UserID: viewerID, Send: make(chan []byte, 4), Hub: hub}
	client.beginBootstrap()

	hub.runClientBootstrap(context.Background(), client, presenceSnapshotSeed{
		viewerID: viewerID, connectedIDs: []uuid.UUID{viewerID},
	})

	assert.True(t, disconnected)
	assert.Empty(t, client.Send)
}

func TestRunClientBootstrap_CustomReplayFailureDisconnectsWithoutPartialReplacement(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewerID := insertCTUser(t, db, "bootstrap_custom_failure_viewer")
	senderID := insertCTUser(t, db, "bootstrap_custom_failure_sender")
	makeFriends(t, db, senderID, viewerID)
	setCustomText(t, db, senderID, 1, "must not escape", "")
	marshalErr := errors.New("custom replay marshal sentinel")
	hub.customTextFrameMarshaler = func(uuid.UUID, *CustomTextPayload) ([]byte, error) {
		return nil, marshalErr
	}
	disconnected := false
	hub.customTextClientDisconnect = func(*Client) error {
		disconnected = true
		return nil
	}
	client := &Client{UserID: viewerID, Send: make(chan []byte, 8), Hub: hub}
	client.beginBootstrap()

	hub.runClientBootstrap(context.Background(), client, presenceSnapshotSeed{
		viewerID: viewerID, connectedIDs: []uuid.UUID{viewerID},
	})

	assert.True(t, disconnected)
	assert.Empty(t, client.Send, "fallible Custom replay work must finish before replacement publication")
}

func TestHubRun_RemainsResponsiveWhileFinalizerHoldsPublicationBarrier(t *testing.T) {
	hub := NewHub(nil, nil)
	viewerID := uuid.New()
	bootClient := &Client{
		ID: uuid.New(), UserID: viewerID, Send: make(chan []byte, 8), Hub: hub,
	}
	bootClient.beginBootstrap()
	customReplay := []byte(`{"type":"rich_presence_update","data":{"category":"custom_text"}}`)
	require.NoError(t, bootClient.appendBootstrapReplay(customReplay))
	steadyClient := &Client{
		ID: uuid.New(), UserID: uuid.New(), Send: make(chan []byte, 8), Hub: hub,
	}
	hub.clients[bootClient.ID] = bootClient
	hub.clients[steadyClient.ID] = steadyClient
	hub.userClients[bootClient.UserID] = map[uuid.UUID]bool{bootClient.ID: true}
	hub.userClients[steadyClient.UserID] = map[uuid.UUID]bool{steadyClient.ID: true}

	finalizerEntered := make(chan struct{})
	releaseFinalizer := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFinalizer) }) }
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		return make(presence.ActivitySnapshot), nil
	}
	hub.activitySnapshotFinalize = func(
		ctx context.Context,
		_ uuid.UUID,
		projected presence.ActivitySnapshot,
		publish func(presence.ActivitySnapshot) error,
	) error {
		close(finalizerEntered)
		select {
		case <-releaseFinalizer:
			return publish(projected)
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	go hub.Run()
	bootstrapDone := make(chan struct{})
	go func() {
		hub.runClientBootstrap(context.Background(), bootClient, presenceSnapshotSeed{
			viewerID: viewerID, connectedIDs: []uuid.UUID{viewerID},
		})
		close(bootstrapDone)
	}()
	t.Cleanup(func() {
		release()
		<-bootstrapDone
		hub.Shutdown()
	})
	select {
	case <-finalizerEntered:
	case <-time.After(time.Second):
		t.Fatal("activity finalizer did not reach publication barrier")
	}

	firstLive := OutgoingMessage{Type: "generic_during_finalizer"}
	secondLive := OutgoingMessage{Type: "run_responsiveness_probe"}
	hub.BroadcastToAll(firstLive)
	hub.BroadcastToAll(secondLive)
	readType := func(client *Client) string {
		select {
		case frame := <-client.Send:
			var message OutgoingMessage
			require.NoError(t, json.Unmarshal(frame, &message))
			return message.Type
		case <-time.After(250 * time.Millisecond):
			return ""
		}
	}
	assert.Equal(t, firstLive.Type, readType(steadyClient))
	assert.Equal(t, secondLive.Type, readType(steadyClient), "Hub.Run blocked behind one bootstrapping client")
	blockingConfirmation := []byte(`{"type":"subscribed"}`)
	blockingDone := make(chan bool, 1)
	go func() { blockingDone <- bootClient.enqueueOutboundBlocking(blockingConfirmation) }()
	select {
	case enqueued := <-blockingDone:
		assert.True(t, enqueued)
	case <-time.After(250 * time.Millisecond):
		t.Error("blocking subscription confirmation stalled behind publication sendMu")
	}
	assert.Empty(t, bootClient.Send, "generic live frames must remain buffered until replacement publication")

	release()
	select {
	case <-bootstrapDone:
	case <-time.After(time.Second):
		t.Fatal("bootstrap did not resume after finalizer release")
	}
	assert.Equal(t, "presence_snapshot", readType(bootClient))
	assert.Equal(t, "rich_presence_update", readType(bootClient))
	assert.Equal(t, firstLive.Type, readType(bootClient))
	assert.Equal(t, secondLive.Type, readType(bootClient))
	assert.Equal(t, "subscribed", readType(bootClient))
}

func TestHandleUnregister_CancelsAndWaitsBeforeClosingBootstrapQueue(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	userID := uuid.New()
	buddy := newTestClient(hub, userID)
	hub.clients[buddy.ID] = buddy
	hub.userClients[userID] = map[uuid.UUID]bool{buddy.ID: true}
	activityStarted := make(chan struct{})
	activityReturned := make(chan struct{})
	hub.activitySnapshot = func(ctx context.Context, _ uuid.UUID) (presence.ActivitySnapshot, error) {
		close(activityStarted)
		<-ctx.Done()
		close(activityReturned)
		return nil, ctx.Err()
	}
	disconnected := false
	hub.customTextClientDisconnect = func(*Client) error {
		disconnected = true
		return nil
	}
	client := newTestClient(hub, userID)

	hub.handleRegister(client)
	assert.Equal(t, "connected", readClientMsg(t, client)["type"])
	select {
	case <-activityStarted:
	case <-time.After(time.Second):
		t.Fatal("activity snapshot did not start")
	}
	unregisterDone := make(chan struct{})
	go func() {
		hub.handleUnregister(client)
		close(unregisterDone)
	}()
	select {
	case <-unregisterDone:
	case <-time.After(time.Second):
		t.Fatal("unregister did not cancel and wait for bootstrap")
	}
	select {
	case <-activityReturned:
	default:
		t.Fatal("unregister closed the queue before bootstrap returned")
	}
	_, open := <-client.Send
	assert.False(t, open)
	assert.False(t, disconnected, "ordinary unregister cancellation must not trigger replacement failure close")
}

func TestAcquireClientBootstrapSlot_IsGloballyBoundedByContext(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.clientBootstrapSlots = make(chan struct{}, 1)
	release, err := hub.acquireClientBootstrapSlot(context.Background())
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	_, err = hub.acquireClientBootstrapSlot(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRunClientBootstrap_DeadlineDisconnectsWithoutEnqueue(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.clientBootstrapTimeout = 25 * time.Millisecond
	hub.activitySnapshot = func(ctx context.Context, _ uuid.UUID) (presence.ActivitySnapshot, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	disconnected := false
	hub.customTextClientDisconnect = func(*Client) error {
		disconnected = true
		return nil
	}
	viewerID := uuid.New()
	client := &Client{UserID: viewerID, Send: make(chan []byte, 4), Hub: hub}
	client.beginBootstrap()

	hub.runClientBootstrap(context.Background(), client, presenceSnapshotSeed{
		viewerID:     viewerID,
		connectedIDs: []uuid.UUID{viewerID},
	})

	assert.True(t, disconnected)
	assert.Empty(t, client.Send)
}

// TestClientSnapshotCoverageComesFromTheAuthorizedSnapshot locks the property
// Codex named on PR #2975: the registration frontier's coverage set must be the
// senders the snapshot ACTUALLY carried, not the seed's candidates.
//
// seed.connectedIDs is a pre-authorization candidate list —
// capturePresenceSnapshotSeed enumerates connected users and stops at
// presenceSnapshotConnectedLimit, then authorizeBasePresenceCandidates removes
// the ones this viewer may not see. A coverage set derived from the seed would
// mark a sender covered that the snapshot dropped, and the frontier would then
// discard that sender's in-flight delta too, leaving the viewer rendering them
// offline until an unrelated transition.
//
// This drives the revocation-at-publication path: the friendship is deleted
// after the initial load and before publication, so the sender is in the seed
// and NOT in the published snapshot — precisely the divergence under test.
func TestClientSnapshotCoverageComesFromTheAuthorizedSnapshot(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewerID := insertCTUser(t, db, "covviewer")
	senderID := insertCTUser(t, db, "covsender")
	makeFriends(t, db, viewerID, senderID)
	for _, userID := range []uuid.UUID{viewerID, senderID} {
		require.NoError(t, hub.redis.Set(
			context.Background(), "presence:"+userID.String(), statusOnline, time.Minute,
		).Err())
	}

	initialLoadComplete := make(chan struct{})
	releasePublication := make(chan struct{})
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		close(initialLoadComplete)
		<-releasePublication
		return make(presence.ActivitySnapshot), nil
	}

	client := &Client{UserID: viewerID, Send: make(chan []byte, 8), Hub: hub}
	client.beginBootstrap()

	// Seeded as a candidate — this is what a seed-derived coverage set would use.
	require.True(t, client.snapshotCovered(senderID),
		"before publication nothing is proven, so the frontier still filters")

	done := make(chan struct{})
	go func() {
		hub.runClientBootstrap(context.Background(), client, presenceSnapshotSeed{
			viewerID: viewerID, connectedIDs: []uuid.UUID{viewerID, senderID},
			hidden: map[uuid.UUID]string{},
		})
		close(done)
	}()

	select {
	case <-initialLoadComplete:
	case <-time.After(2 * time.Second):
		t.Fatal("initial base presence load did not finish")
	}
	// Revoked between the initial load and publication, so re-authorization drops
	// this sender from the snapshot even though the seed still names them.
	_, err := db.Exec(`
		DELETE FROM friendships
		WHERE (requester_id = $1 AND addressee_id = $2)
		   OR (requester_id = $2 AND addressee_id = $1)
	`, viewerID, senderID)
	require.NoError(t, err)
	close(releasePublication)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap did not complete")
	}

	assert.False(t, client.snapshotCovered(senderID),
		"the published snapshot did NOT carry this sender, so the frontier must not "+
			"treat their in-flight delta as redundant — deriving coverage from the seed "+
			"is exactly the bug this locks")
	assert.True(t, client.snapshotCovered(viewerID),
		"the viewer's own entry IS carried, so coverage must not be empty-by-accident")
}

// TestClientSnapshotOmittedIsNilWhenNothingWasDropped locks the memory property,
// which is not decoration: the round-3 fix stored the full COVERAGE set and so
// allocated up to presenceSnapshotConnectedLimit UUIDs per socket for its whole
// lifetime — O(N^2) aggregate across a hub — while the design text still claimed
// the common case was free. Codex caught the contradiction.
//
// Storing the COMPLEMENT restores the claim: on an ordinary hub every candidate is
// published, nothing is omitted, and the client retains nil.
func TestClientSnapshotOmittedIsNilWhenNothingWasDropped(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewerID := insertCTUser(t, db, "omitnilviewer")
	senderID := insertCTUser(t, db, "omitnilsender")
	makeFriends(t, db, viewerID, senderID)
	for _, userID := range []uuid.UUID{viewerID, senderID} {
		require.NoError(t, hub.redis.Set(
			context.Background(), "presence:"+userID.String(), statusOnline, time.Minute,
		).Err())
	}
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		return make(presence.ActivitySnapshot), nil
	}

	client := &Client{UserID: viewerID, Send: make(chan []byte, 8), Hub: hub}
	client.beginBootstrap()
	hub.runClientBootstrap(context.Background(), client, presenceSnapshotSeed{
		viewerID: viewerID, connectedIDs: []uuid.UUID{viewerID, senderID},
		hidden: map[uuid.UUID]string{},
	})

	client.bootstrapMu.Lock()
	omitted := client.presenceSnapshotOmitted
	covered := client.presenceSnapshotCovered
	client.bootstrapMu.Unlock()
	assert.Nil(t, covered,
		"an untruncated seed must not take the positive-coverage branch at all — that "+
			"branch stores up to presenceSnapshotConnectedLimit UUIDs per socket")
	assert.Nil(t, omitted,
		"every candidate reached the published snapshot, so the client must retain "+
			"nothing — this is the property that keeps the frontier's memory cost at zero "+
			"on an ordinary hub")

	assert.True(t, client.snapshotCovered(senderID),
		"a nil omitted set must still mean COVERED, or the frontier stops filtering")
}

// TestTruncatedSeedCoverageIncludesCapExcludedSenders locks the fourth case of
// snapshot coverage, which the complement alone cannot express — and which the
// complement REOPENED after fixing the third.
//
// Coverage was computed as candidates MINUS published. That is only sound while
// the candidate list IS the connected set. Above presenceSnapshotConnectedLimit
// it is not: capturePresenceSnapshotSeed breaks at the cap, so a sender excluded
// BY truncation is never in connectedIDs and therefore can never be subtracted
// from it. The complement came back empty, snapshotCovered answered true, and the
// frontier discarded that sender's in-flight delta — the viewer renders them
// offline until an unrelated transition. That is the >512 defect the coverage set
// was introduced to fix, reintroduced by inverting it.
//
// The case table needs four rows, not three. Rows 3 and 4 are indistinguishable
// from connectedIDs alone, which is the whole reason truncated exists:
//
//	connected? in candidates? published? -> covered?
//	yes        yes             yes          yes
//	yes        yes             no           NO  (unauthorized)
//	yes        NO              no           NO  (cut by the cap)   <- this test
//	no         no              no           yes (absent means offline)
//
// It drives the real derivation in publishClientPresenceSnapshot. It must NOT set
// presenceSnapshotOmitted/Covered directly: the frontier arm in
// TestPresenceFrontierDropsDeltaDispatchedBeforeRegistration does inject one, and
// that injection is exactly why it stayed green through this defect.
func TestTruncatedSeedCoverageIncludesCapExcludedSenders(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	viewerID := insertCTUser(t, db, "truncviewer")
	// Authorized and connected, but BELOW the cap line — so absent from the seed.
	excludedID := insertCTUser(t, db, "truncexcluded")
	makeFriends(t, db, viewerID, excludedID)
	for _, userID := range []uuid.UUID{viewerID, excludedID} {
		require.NoError(t, hub.redis.Set(
			context.Background(), "presence:"+userID.String(), statusOnline, time.Minute,
		).Err())
	}
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		return make(presence.ActivitySnapshot), nil
	}

	client := &Client{UserID: viewerID, Send: make(chan []byte, 8), Hub: hub}
	client.beginBootstrap()
	// Truncated: the viewer made the cut, excludedID did not. Constructed rather
	// than captured from 513 live clients because the flag's own wiring to the cap
	// is locked separately, in
	// TestCapturePresenceSnapshotSeedBoundsConnectedIDsAndRetainsViewer.
	hub.runClientBootstrap(context.Background(), client, presenceSnapshotSeed{
		viewerID:     viewerID,
		connectedIDs: []uuid.UUID{viewerID},
		hidden:       map[uuid.UUID]string{},
		truncated:    true,
	})

	assert.False(t, client.snapshotCovered(excludedID),
		"a sender the cap excluded was never a candidate, so no complement can name "+
			"them; the snapshot did not carry them and the frontier must not treat their "+
			"delta as redundant")
	assert.True(t, client.snapshotCovered(viewerID),
		"the viewer WAS published, so positive coverage must not be empty-by-accident")

	client.bootstrapMu.Lock()
	omitted, covered := client.presenceSnapshotOmitted, client.presenceSnapshotCovered
	client.bootstrapMu.Unlock()
	assert.Nil(t, omitted, "a truncated seed must not take the complement branch")
	assert.NotNil(t, covered, "coverage above the cap is carried positively")
	assert.LessOrEqual(t, len(covered), presenceSnapshotConnectedLimit,
		"positive coverage is bounded by the cap that forced it")
}
