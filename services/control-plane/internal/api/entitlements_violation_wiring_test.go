package api

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type violationEventRecorder struct {
	mu     sync.Mutex
	events []securityevent.Event
}

func (r *violationEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

type violationTierStub string

func (s violationTierStub) GetTierChecked(context.Context, string) (string, error) {
	return string(s), nil
}

// The threshold's two outermost control-plane effects, through the PRODUCTION
// composition (newViolationRecorder -> EntitlementNotifier -> hub): the
// entitlements_changed frame handed to the hub and the security event handed
// to the sink. Once at the fifth count; not at the fourth, not again at the
// sixth; and never a disconnect.
func TestViolationThresholdPushesEntitlementsChangedOnceAndNeverDisconnects(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	hub := &recordingBroadcaster{}
	events := &violationEventRecorder{}
	rec := newViolationRecorder(rdb, logger.New("test"), violationTierStub(entitlements.TierPremium), hub, events)
	user := uuid.New()

	record := func() {
		rec.Record(context.Background(), user, entitlements.ViolationAttachmentOverTier)
		rec.Wait()
	}
	for i := 1; i < entitlements.ViolationThreshold; i++ {
		record()
	}
	require.Empty(t, hub.broadcasts, "nothing reaches the hub at count 4")
	require.Empty(t, events.events, "no event at count 4")

	record()
	require.Len(t, hub.broadcasts, 1, "one frame at count 5")
	require.Len(t, events.events, 1, "one event at count 5")

	record()
	assert.Len(t, hub.broadcasts, 1, "no second frame at count 6")
	assert.Len(t, events.events, 1, "no second event at count 6")

	frame := hub.broadcasts[0]
	assert.Equal(t, user, frame.userID)
	assert.Equal(t, "entitlements_changed", frame.msg.Type, "the existing wire event, not a new one")
	assert.Equal(t, "premium", frame.msg.Data["tier"], "the set the server enforces")
	assert.Empty(t, hub.disconnected, "flag and resync only: the threshold never disconnects")
	assert.Equal(t, securityevent.ReasonAuthorizationDenied, events.events[0].ReasonCode)
	assert.Equal(t, securityevent.EventSecurityControl, events.events[0].EventType)
}

// Wiring pin, in the idiom of router_rich_presence_test.go: the setters are
// the only thing connecting the two counted doors to the recorder, and an
// unwired door is silent -- it refuses exactly as before and simply counts
// nothing, which no behavioural test of the router would notice.
func TestRouterWiresTheViolationRecorderIntoBothDoors(t *testing.T) {
	source, err := os.ReadFile("router.go")
	require.NoError(t, err)
	text := string(source)
	// The strings.Count matches below are a DELIBERATE source pin on the exact
	// call sites. Do not loosen them to a substring or regexp: a looser match
	// would also pass a second recorder or a differently-built one, which
	// splits the shared per-user window this test exists to protect.
	assert.Equal(t, 1, strings.Count(text, "violationRecorder := newViolationRecorder(redis, log, entCache, hub, securityEvents)"),
		"one recorder, built from the enforcing tier cache")
	assert.Equal(t, 1, strings.Count(text, "mediaHandler.SetViolationRecorder(violationRecorder)"))
	assert.Equal(t, 1, strings.Count(text, "voiceSub.SetTierViolationRecorder(violationRecorder)"))
}
