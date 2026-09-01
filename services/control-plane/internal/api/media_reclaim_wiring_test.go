package api

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
)

func TestNewDurableErasedMediaReclaimer_WakesWorkerAndEnqueuesTier2(t *testing.T) {
	wakes := 0
	var enqueued []string
	reclaim := newDurableErasedMediaReclaimer(func() { wakes++ }, func(refs []media.BlobRef) {
		for _, ref := range refs {
			enqueued = append(enqueued, ref.Key)
		}
	})
	require.NotNil(t, reclaim)
	reclaim(context.Background(), []media.BlobRef{media.NewBlobRef("avatars/u1", sql.NullString{})}, []media.BlobRef{media.NewBlobRef("attachments/f1", sql.NullString{})})
	assert.Equal(t, 1, wakes, "each erasure callback wakes the durable Tier-1 worker once")
	assert.Equal(t, []string{"attachments/f1"}, enqueued, "Tier-2 remains an exact purge enqueue")
}

func TestNewDurableErasedMediaReclaimer_EmptyTier2StillWakesOnlyWorker(t *testing.T) {
	wakes := 0
	enqueueCalls := 0
	reclaim := newDurableErasedMediaReclaimer(func() { wakes++ }, func(refs []media.BlobRef) {
		enqueueCalls++
		assert.Empty(t, refs)
	})
	reclaim(context.Background(), nil, nil)
	assert.Equal(t, 1, wakes)
	assert.Equal(t, 1, enqueueCalls, "empty Tier-2 is still passed to its no-op queue")
}
