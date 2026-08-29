package api

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type recordingObjectDeleter struct{ deleted []string }

func (r *recordingObjectDeleter) DeleteObject(_ context.Context, key string) error {
	r.deleted = append(r.deleted, key)
	return nil
}

// TestNewErasedMediaReclaimer_RoutesEachTierToItsOwnRail is the mutant-killer
// for the wiring. Swap the two calls inside newErasedMediaReclaimer and this
// test fails; before the closure was extracted, that swap was invisible to the
// whole repo.
//
// The swap is not a style defect. EnqueueBlobDeletes deletes unconditionally,
// without the live-key guard reapSweptBlob applies, and its contract says so —
// so feeding it a DETERMINISTIC tier-1 key (`avatars/<uid>`, shared with every
// past and future row for that subject) deletes an object a live row may still
// point at.
func TestNewErasedMediaReclaimer_RoutesEachTierToItsOwnRail(t *testing.T) {
	store := &recordingObjectDeleter{}
	var enqueued []string
	reclaim := newErasedMediaReclaimer(store, func(refs []media.BlobRef) {
		for _, r := range refs {
			enqueued = append(enqueued, r.Key)
		}
	}, logger.New("test"))

	require.NotNil(t, reclaim, "an unwired reclaimer is a silent leak, not a degrade")

	reclaim(context.Background(),
		[]media.BlobRef{media.NewBlobRef("avatars/u1", sql.NullString{})},
		[]media.BlobRef{media.NewBlobRef("attachments/f1", sql.NullString{})})

	assert.Equal(t, []string{"avatars/u1"}, store.deleted,
		"tier-1 must go to ReclaimErasedTier1, which checks placement per ref")
	assert.Equal(t, []string{"attachments/f1"}, enqueued,
		"tier-2 must go to the purge queue, whose per-upload-unique-key contract it satisfies")

	assert.NotContains(t, enqueued, "avatars/u1",
		"a deterministic tier-1 key on the purge queue is deleted without the live-key guard")
	assert.NotContains(t, store.deleted, "attachments/f1")
}

// TestNewErasedMediaReclaimer_EmptyLegsAreHarmless: a user with media on only
// one tier must not trip the other rail.
func TestNewErasedMediaReclaimer_EmptyLegsAreHarmless(t *testing.T) {
	store := &recordingObjectDeleter{}
	enqueueCalls := 0
	reclaim := newErasedMediaReclaimer(store, func([]media.BlobRef) { enqueueCalls++ }, logger.New("test"))

	reclaim(context.Background(), nil, nil)

	assert.Empty(t, store.deleted)
	assert.Equal(t, 1, enqueueCalls, "enqueue is still called; it no-ops on an empty slice")
}
