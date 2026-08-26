package presence_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
)

// Never set REDIS_URL by hand: redistest allocates its own per-process database
// index and default URL, and overriding it breaks the whole suite.
func writeLeasedActivityState(t *testing.T, key string) {
	t.Helper()
	client := redistest.Client(t)
	envelope, err := json.Marshal(map[string]any{
		"source_token":   uuid.New().String(),
		"source_version": 1,
		"minimized":      false,
		"payload":        json.RawMessage(`{"kind":"private_call"}`),
		"updated_at":     time.Now().Unix(),
	})
	require.NoError(t, err)
	require.NoError(t, client.Set(
		context.Background(), key, envelope, presence.ActivityStateTTL,
	).Err())
}

func leasedActivityStateKey(userID uuid.UUID) string {
	return "presence:rich:" + userID.String() + ":" + string(presence.CategoryPrivateCall)
}

func TestGetWithLeaseAcceptsAnExpiringKey(t *testing.T) {
	client := redistest.Client(t)
	store := presence.NewActivityStore(client)
	userID := uuid.New()
	key := leasedActivityStateKey(userID)

	writeLeasedActivityState(t, key)

	state, found, err := store.GetWithLease(
		context.Background(), userID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 1, state.SourceVersion)
}

// The whole point of #2448's resolver gap: a key with no expiry has no 90-second
// level arm, so reading it as "it will expire" is the failure. PERSIST is the
// only way to produce that state, because every sanctioned write sets its expiry
// atomically in the same Lua script.
func TestGetWithLeaseRejectsAPersistedKey(t *testing.T) {
	ctx := context.Background()
	client := redistest.Client(t)
	store := presence.NewActivityStore(client)
	userID := uuid.New()
	key := leasedActivityStateKey(userID)

	writeLeasedActivityState(t, key)
	require.NoError(t, client.Persist(ctx, key).Err())

	// Pin the server contract the Lua guard reads: PTTL on an existing key with
	// no expiry is the raw integer -1. Go-side sentinel handling is exactly what
	// the guard cannot rely on, so the assertion lives at the protocol level.
	rawPTTL, err := client.Do(ctx, "PTTL", key).Int64()
	require.NoError(t, err)
	t.Logf("raw PTTL after PERSIST: %d", rawPTTL)
	require.EqualValues(t, -1, rawPTTL)

	_, found, err := store.GetWithLease(ctx, userID, presence.CategoryPrivateCall)
	require.ErrorIs(t, err, presence.ErrUnexpiringActivityState)
	require.False(t, found)
}

func TestGetWithLeaseReportsAnAbsentKey(t *testing.T) {
	store := presence.NewActivityStore(redistest.Client(t))

	_, found, err := store.GetWithLease(
		context.Background(), uuid.New(), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.False(t, found)
}
