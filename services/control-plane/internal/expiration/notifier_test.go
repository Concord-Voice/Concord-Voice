package expiration

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// recordingBroadcaster captures every frame the notifier hands the hub.
type recordingBroadcaster struct {
	mu        sync.Mutex
	frames    [][]byte
	targets   []uuid.UUID
	delivered bool
}

func (r *recordingBroadcaster) record(target uuid.UUID, msg websocket.OutgoingMessage) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := json.Marshal(msg)
	r.frames = append(r.frames, raw)
	r.targets = append(r.targets, target)
	return r.delivered
}

func (r *recordingBroadcaster) BroadcastToDMParticipantsContext(_ context.Context, id uuid.UUID, msg websocket.OutgoingMessage) bool {
	return r.record(id, msg)
}

func (r *recordingBroadcaster) BroadcastToServerContext(_ context.Context, id uuid.UUID, msg websocket.OutgoingMessage) bool {
	return r.record(id, msg)
}

// The frames the sweeper's closures in cmd/server/main.go produced before
// #3459 moved them here, byte for byte: a client must not be able to tell the
// sweeper's deletion from the expirer's.
const (
	goldenServerPurged = `{"type":"server_purged","data":{"expires_before":"2026-10-09T01:02:03.456789Z","purged_by":null,"reason":"expiry","server_id":"%s"}}`
	goldenDMPurged     = `{"type":"dm_purged","data":{"conversation_id":"%s","expires_before":"2026-10-09T01:02:03.456789Z","purged_by":null,"reason":"expiry"}}`
)

// #3459 §8 parity: the sweeper (handed the notifier's methods as its
// SweeperDeps sinks, as main.go does) and the ContextExpirer emit
// byte-identical frames for the same input, and both equal the pre-extraction
// golden frame. The cutoff carries nanoseconds and a non-UTC zone to prove the
// truncation and the zone conversion are the shared ones.
func TestHubPurgeNotifier_SweeperAndExpirerFramesAreByteIdentical(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 3, 2, 3, 456789999, time.FixedZone("x", 2*3600))
	server, conversation := uuid.New(), uuid.New()

	sweeperSide := &recordingBroadcaster{delivered: true}
	sweeperNotifier, err := NewHubPurgeNotifier(sweeperSide, logger.NewWithWriter(&bytes.Buffer{}))
	require.NoError(t, err)
	deps := SweeperDeps{EmitServerPurged: sweeperNotifier.EmitServerPurged, EmitDMPurged: sweeperNotifier.EmitDMPurged}
	deps.EmitServerPurged(context.Background(), server.String(), cutoff)
	deps.EmitDMPurged(context.Background(), conversation.String(), cutoff)

	expirerSide := &recordingBroadcaster{delivered: true}
	expirerNotifier, err := NewHubPurgeNotifier(expirerSide, logger.NewWithWriter(&bytes.Buffer{}))
	require.NoError(t, err)
	expirer, _ := newTestExpirer(context.Background(), t, &fakeExpiryRunner{}, expirerNotifier)
	transition := Transition{Current: Policy{UpdatedAt: &cutoff}, DuePinnedIDs: []string{uuid.NewString()}}
	require.Equal(t, 1, expirer.ExpireChannelPins(context.Background(), uuid.NewString(), server.String(), transition))
	require.Equal(t, 1, expirer.ExpireConversationPins(context.Background(), conversation.String(), false, transition))

	require.Len(t, sweeperSide.frames, 2)
	require.Equal(t, sweeperSide.frames, expirerSide.frames, "sweeper and expirer frames must be byte-identical")
	assert.Equal(t, []uuid.UUID{server, conversation}, expirerSide.targets)
	assert.JSONEq(t, sprintf(goldenServerPurged, server), string(expirerSide.frames[0]))
	assert.JSONEq(t, sprintf(goldenDMPurged, conversation), string(expirerSide.frames[1]))
}

func sprintf(format string, id uuid.UUID) string {
	return string(bytes.Replace([]byte(format), []byte("%s"), []byte(id.String()), 1))
}

func TestNewHubPurgeNotifier_RefusesMissingDependencies(t *testing.T) {
	log := logger.NewWithWriter(&bytes.Buffer{})
	_, err := NewHubPurgeNotifier(nil, log)
	assert.Error(t, err)
	var typedNil *websocket.Hub
	_, err = NewHubPurgeNotifier(typedNil, log)
	assert.Error(t, err, "a typed-nil hub inside the interface is still no hub")
	_, err = NewHubPurgeNotifier(&recordingBroadcaster{}, nil)
	assert.Error(t, err)
}

// A malformed id and an undelivered frame are logged by scope only; a
// cancelled context that suppressed delivery is not logged at all.
func TestHubPurgeNotifier_SuppressionLogsScopeOnly(t *testing.T) {
	var logs bytes.Buffer
	hub := &recordingBroadcaster{delivered: false}
	notifier, err := NewHubPurgeNotifier(hub, logger.NewWithWriter(&logs))
	require.NoError(t, err)
	id := uuid.NewString()

	notifier.EmitServerPurged(context.Background(), "not-a-uuid", time.Now())
	notifier.EmitDMPurged(context.Background(), "not-a-uuid", time.Now())
	assert.Empty(t, hub.frames, "a malformed id reaches no hub")
	notifier.EmitServerPurged(context.Background(), id, time.Now())
	notifier.EmitDMPurged(context.Background(), id, time.Now())
	assert.Equal(t, 4, bytes.Count(logs.Bytes(), []byte("purge notification suppressed")))
	assert.NotContains(t, logs.String(), id)
	assert.NotContains(t, logs.String(), "not-a-uuid")

	logs.Reset()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	notifier.EmitServerPurged(cancelled, id, time.Now())
	notifier.EmitDMPurged(cancelled, id, time.Now())
	assert.Empty(t, logs.String(), "a shutdown is not a suppression")
}

// AC6 wire fields (§6.4): include_pinned on every event, the previous values
// on "changed" only.
func TestAddPinnedEventFields(t *testing.T) {
	day, week := 86400, 604800
	tru, fls := true, false
	policy := Policy{WindowSeconds: &week, IncludePinned: true}

	changed := map[string]interface{}{}
	AddPinnedEventFields(changed, EventPayload{Kind: EventKindChanged, PreviousWindowSeconds: &day, PreviousIncludePinned: &fls}, policy)
	assert.Equal(t, map[string]interface{}{"include_pinned": true, "previous_include_pinned": false, "previous_window_seconds": &day}, changed)

	set := map[string]interface{}{}
	AddPinnedEventFields(set, EventPayload{Kind: EventKindSet, IncludePinned: &tru}, policy)
	assert.Equal(t, map[string]interface{}{"include_pinned": true}, set)

	cleared := map[string]interface{}{}
	AddPinnedEventFields(cleared, EventPayload{Kind: EventKindCleared, PreviousWindowSeconds: &week}, Policy{})
	assert.Equal(t, map[string]interface{}{"include_pinned": false}, cleared, "cleared carries E (false) and no previous values")
}
