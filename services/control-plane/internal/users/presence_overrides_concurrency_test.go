package users_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type queuedCustomStatusCoordinator struct {
	mu      sync.Mutex
	held    map[uuid.UUID]bool
	waiters map[uuid.UUID][]chan struct{}
	queued  chan uuid.UUID
}

func newQueuedCustomStatusCoordinator() *queuedCustomStatusCoordinator {
	return &queuedCustomStatusCoordinator{
		held:    make(map[uuid.UUID]bool),
		waiters: make(map[uuid.UUID][]chan struct{}),
		queued:  make(chan uuid.UUID, 4),
	}
}

// WithSender has the same externally-observable contract as the production
// striped mutex coordinator: same-sender callbacks are mutually exclusive,
// while different senders remain independent. The queued signal is emitted
// only after a later same-sender callback is registered behind the holder.
func (c *queuedCustomStatusCoordinator) WithSender(senderID uuid.UUID, fn func()) {
	permission := make(chan struct{})
	queued := false
	c.mu.Lock()
	if c.held[senderID] {
		c.waiters[senderID] = append(c.waiters[senderID], permission)
		queued = true
	} else {
		c.held[senderID] = true
		close(permission)
	}
	c.mu.Unlock()
	if queued {
		c.queued <- senderID
	}

	<-permission
	defer c.release(senderID)
	fn()
}

func (c *queuedCustomStatusCoordinator) release(senderID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	waiters := c.waiters[senderID]
	if len(waiters) == 0 {
		delete(c.held, senderID)
		delete(c.waiters, senderID)
		return
	}
	next := waiters[0]
	if len(waiters) == 1 {
		delete(c.waiters, senderID)
	} else {
		c.waiters[senderID] = waiters[1:]
	}
	close(next)
}

type customStatusConcurrencyEvent struct {
	kind        string
	version     int
	oldTier     int
	oldAudience map[uuid.UUID]bool
	newAudience map[uuid.UUID]bool
	payload     *websocket.CustomTextPayload
}

type gatedCustomStatusBroadcaster struct {
	gateKind    string
	gateEntered chan struct{}
	releaseGate chan struct{}
	gated       atomic.Bool

	mu     sync.Mutex
	events []customStatusConcurrencyEvent
}

func newGatedCustomStatusBroadcaster(gateKind string) *gatedCustomStatusBroadcaster {
	return &gatedCustomStatusBroadcaster{
		gateKind:    gateKind,
		gateEntered: make(chan struct{}),
		releaseGate: make(chan struct{}),
	}
}

func (b *gatedCustomStatusBroadcaster) gate(kind string) {
	if b.gateKind == kind && b.gated.CompareAndSwap(false, true) {
		close(b.gateEntered)
		<-b.releaseGate
	}
}

func (b *gatedCustomStatusBroadcaster) record(event customStatusConcurrencyEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	event.oldAudience = cloneAudience(event.oldAudience)
	event.newAudience = cloneAudience(event.newAudience)
	if event.payload != nil {
		payload := *event.payload
		event.payload = &payload
	}
	b.events = append(b.events, event)
}

func (b *gatedCustomStatusBroadcaster) snapshot() []customStatusConcurrencyEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]customStatusConcurrencyEvent, len(b.events))
	copy(out, b.events)
	return out
}

func (b *gatedCustomStatusBroadcaster) BroadcastCustomText(
	_ uuid.UUID,
	oldTier int,
	payload *websocket.CustomTextPayload,
) {
	b.gate("custom_text")
	b.record(customStatusConcurrencyEvent{kind: "custom_text", oldTier: oldTier, payload: payload})
}

func (b *gatedCustomStatusBroadcaster) BroadcastToUser(
	_ uuid.UUID,
	message websocket.OutgoingMessage,
) {
	version, _ := message.Data["version"].(int)
	b.record(customStatusConcurrencyEvent{kind: "metadata", version: version})
}

func (b *gatedCustomStatusBroadcaster) BroadcastCustomTextAudienceDelta(
	_ uuid.UUID,
	oldAudience map[uuid.UUID]bool,
	newAudience map[uuid.UUID]bool,
	payload *websocket.CustomTextPayload,
) {
	b.gate("delta")
	b.record(customStatusConcurrencyEvent{
		kind:        "delta",
		oldAudience: oldAudience,
		newAudience: newAudience,
		payload:     payload,
	})
}

func cloneAudience(in map[uuid.UUID]bool) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool, len(in))
	for id, allowed := range in {
		out[id] = allowed
	}
	return out
}

func awaitValue[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for deterministic concurrency signal")
		var zero T
		return zero
	}
}

func invokePresenceSettingsPATCH(
	h *users.Handler,
	senderID uuid.UUID,
	body map[string]interface{},
) *httptest.ResponseRecorder {
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", senderID.String())
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/api/v1/users/me/presence-settings",
		bytes.NewReader(encoded),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdatePresenceSettings(c)
	return w
}

func TestReplacePresenceOverrides_SameSenderVersionsCommitAndFanOutInOrder(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	firstViewerID := testhelpers.CreateUser(t, db)
	secondViewerID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, firstViewerID)
	testhelpers.AddFriendship(t, db, senderID, secondViewerID)
	_, err := db.Exec(
		`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
		 VALUES ($1, 1, 'focused')`, senderID,
	)
	require.NoError(t, err)

	h := directPresenceOverrideHandler(db)
	coordinator := newQueuedCustomStatusCoordinator()
	broadcaster := newGatedCustomStatusBroadcaster("delta")
	users.SetCustomStatusCoordinatorForTest(h, coordinator)
	users.SetPresenceOverrideBroadcasterForTest(h, broadcaster)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- invokePresenceOverridePUT(t, h, senderID, presenceOverridePUTBody{
			EncryptedData:   "Zmlyc3Q=",
			ExpectedVersion: 0,
			ExcludedUserIDs: []string{firstViewerID.String()},
		})
	}()
	awaitValue(t, broadcaster.gateEntered)

	// The first handler has committed version 1 but is still inside its
	// post-commit fan-out and therefore still owns the sender coordinator.
	ciphertext, version, targets := readOverridePreference(t, db, senderID)
	assert.Equal(t, "Zmlyc3Q=", ciphertext)
	assert.Equal(t, 1, version)
	assert.Equal(t, []uuid.UUID{firstViewerID}, targets)

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		secondDone <- invokePresenceOverridePUT(t, h, senderID, presenceOverridePUTBody{
			EncryptedData:   "c2Vjb25k",
			ExpectedVersion: 1,
			ExcludedUserIDs: []string{firstViewerID.String(), secondViewerID.String()},
		})
	}()
	require.Equal(t, senderID, awaitValue(t, coordinator.queued))
	ciphertext, version, targets = readOverridePreference(t, db, senderID)
	assert.Equal(t, "Zmlyc3Q=", ciphertext)
	assert.Equal(t, 1, version, "queued version 2 must not commit before version 1 fan-out")
	assert.Equal(t, []uuid.UUID{firstViewerID}, targets)

	// The queued signal is emitted only after the second handler is registered
	// behind the first. Releasing fan-out is therefore an explicit handoff, not
	// a scheduler/timing assumption.
	close(broadcaster.releaseGate)
	firstResponse := awaitValue(t, firstDone)
	secondResponse := awaitValue(t, secondDone)
	require.Equal(t, http.StatusOK, firstResponse.Code)
	assert.JSONEq(t, `{"version":1}`, firstResponse.Body.String())
	require.Equal(t, http.StatusOK, secondResponse.Code)
	assert.JSONEq(t, `{"version":2}`, secondResponse.Body.String())

	ciphertext, version, targets = readOverridePreference(t, db, senderID)
	assert.Equal(t, "c2Vjb25k", ciphertext)
	assert.Equal(t, 2, version)
	assert.ElementsMatch(t, []uuid.UUID{firstViewerID, secondViewerID}, targets)

	events := broadcaster.snapshot()
	require.Len(t, events, 4)
	assert.Equal(t, "delta", events[0].kind)
	assert.Equal(t, map[uuid.UUID]bool{firstViewerID: true, secondViewerID: true}, events[0].oldAudience)
	assert.Equal(t, map[uuid.UUID]bool{secondViewerID: true}, events[0].newAudience)
	require.NotNil(t, events[0].payload)
	assert.Equal(t, "focused", events[0].payload.Text)
	assert.Equal(t, "metadata", events[1].kind)
	assert.Equal(t, 1, events[1].version)
	assert.Equal(t, "delta", events[2].kind)
	assert.Equal(t, map[uuid.UUID]bool{secondViewerID: true}, events[2].oldAudience)
	assert.Empty(t, events[2].newAudience)
	require.NotNil(t, events[2].payload)
	assert.Equal(t, "focused", events[2].payload.Text)
	assert.Equal(t, "metadata", events[3].kind)
	assert.Equal(t, 2, events[3].version)
}

func TestUpdatePresenceSettings_NewestClearFollowsInFlightPresenceOverrideFanout(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	viewerID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, viewerID)
	_, err := db.Exec(
		`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
		 VALUES ($1, 1, 'focused')`, senderID,
	)
	require.NoError(t, err)

	h := directPresenceOverrideHandler(db)
	coordinator := newQueuedCustomStatusCoordinator()
	broadcaster := newGatedCustomStatusBroadcaster("delta")
	users.SetCustomStatusCoordinatorForTest(h, coordinator)
	users.SetPresenceOverrideBroadcasterForTest(h, broadcaster)

	overrideDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		overrideDone <- invokePresenceOverridePUT(t, h, senderID, presenceOverridePUTBody{
			EncryptedData:   "YXV0aG9yaXplZA==",
			ExpectedVersion: 0,
			ExcludedUserIDs: []string{},
		})
	}()
	awaitValue(t, broadcaster.gateEntered)

	settingsDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		settingsDone <- invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
			"custom_text_tier": 0,
		})
	}()
	require.Equal(t, senderID, awaitValue(t, coordinator.queued))

	// The settings handler has registered behind the in-flight override fan-out,
	// so its newer clear cannot commit or overtake that update.
	var tier int
	require.NoError(t, db.QueryRow(
		`SELECT custom_text_tier FROM user_presence_settings WHERE user_id = $1`, senderID,
	).Scan(&tier))
	assert.Equal(t, 1, tier)

	close(broadcaster.releaseGate)
	overrideResponse := awaitValue(t, overrideDone)
	settingsResponse := awaitValue(t, settingsDone)
	require.Equal(t, http.StatusOK, overrideResponse.Code)
	assert.JSONEq(t, `{"version":1}`, overrideResponse.Body.String())
	require.Equal(t, http.StatusOK, settingsResponse.Code)

	require.NoError(t, db.QueryRow(
		`SELECT custom_text_tier FROM user_presence_settings WHERE user_id = $1`, senderID,
	).Scan(&tier))
	assert.Zero(t, tier)

	events := broadcaster.snapshot()
	require.Len(t, events, 3)
	assert.Equal(t, "delta", events[0].kind)
	assert.Equal(t, map[uuid.UUID]bool{viewerID: true}, events[0].oldAudience)
	assert.Equal(t, map[uuid.UUID]bool{viewerID: true}, events[0].newAudience)
	require.NotNil(t, events[0].payload)
	assert.Equal(t, "focused", events[0].payload.Text)
	assert.Equal(t, "metadata", events[1].kind)
	assert.Equal(t, 1, events[1].version)
	assert.Equal(t, "custom_text", events[2].kind)
	assert.Equal(t, 1, events[2].oldTier)
	assert.Nil(t, events[2].payload, "the newest event must be the tier-zero clear")
}

func TestReplacePresenceOverrides_WaitsForPresenceSettingsClearAndReauthorizesCurrentState(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	viewerID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, viewerID)
	_, err := db.Exec(
		`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
		 VALUES ($1, 1, 'focused')`, senderID,
	)
	require.NoError(t, err)

	h := directPresenceOverrideHandler(db)
	coordinator := newQueuedCustomStatusCoordinator()
	broadcaster := newGatedCustomStatusBroadcaster("custom_text")
	users.SetCustomStatusCoordinatorForTest(h, coordinator)
	users.SetPresenceOverrideBroadcasterForTest(h, broadcaster)

	settingsDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		settingsDone <- invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
			"custom_text_tier": 0,
		})
	}()
	awaitValue(t, broadcaster.gateEntered)

	// The tier-zero write is committed while its clear fan-out is gated.
	var tier int
	var text *string
	require.NoError(t, db.QueryRow(
		`SELECT custom_text_tier, custom_text FROM user_presence_settings WHERE user_id = $1`,
		senderID,
	).Scan(&tier, &text))
	assert.Zero(t, tier)
	require.NotNil(t, text)
	assert.Equal(t, "focused", *text)

	overrideDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		overrideDone <- invokePresenceOverridePUT(t, h, senderID, presenceOverridePUTBody{
			EncryptedData:   "Y2xlYXJlZA==",
			ExpectedVersion: 0,
			ExcludedUserIDs: []string{},
		})
	}()
	require.Equal(t, senderID, awaitValue(t, coordinator.queued))

	close(broadcaster.releaseGate)
	settingsResponse := awaitValue(t, settingsDone)
	overrideResponse := awaitValue(t, overrideDone)
	require.Equal(t, http.StatusOK, settingsResponse.Code)
	require.Equal(t, http.StatusOK, overrideResponse.Code)
	assert.JSONEq(t, `{"version":1}`, overrideResponse.Body.String())

	_, version, targets := readOverridePreference(t, db, senderID)
	assert.Equal(t, 1, version)
	assert.Empty(t, targets)

	events := broadcaster.snapshot()
	require.Len(t, events, 3)
	assert.Equal(t, "custom_text", events[0].kind)
	assert.Equal(t, 1, events[0].oldTier)
	assert.Nil(t, events[0].payload)
	assert.Equal(t, "delta", events[1].kind)
	assert.Empty(t, events[1].oldAudience)
	assert.Empty(t, events[1].newAudience)
	assert.Nil(t, events[1].payload, "post-commit delivery must use the current tier-zero payload")
	assert.Equal(t, "metadata", events[2].kind)
	assert.Equal(t, 1, events[2].version)
}
