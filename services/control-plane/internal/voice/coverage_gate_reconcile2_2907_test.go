package voice

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestReconcileOrphanedTemporaryGrantsFailsClosedWithoutManagerOrLimit(t *testing.T) {
	var subscriber NATSSubscriber
	ctx := context.Background()

	revoked, err := subscriber.reconcileOrphanedTemporaryGrants(ctx, 1)
	assert.NoError(t, err)
	assert.Zero(t, revoked)

	subscriber.tempGrant = &tempGrantManager{}
	revoked, err = subscriber.reconcileOrphanedTemporaryGrants(ctx, 0)
	assert.NoError(t, err)
	assert.Zero(t, revoked)
}

func TestParseSortedVoiceParticipantIDsDeduplicatesAndRejectsBounds(t *testing.T) {
	one, two := uuid.New(), uuid.New()
	ids, err := parseSortedVoiceParticipantIDs([]string{two.String(), one.String(), two.String()}, 3)
	assert.NoError(t, err)
	assert.Len(t, ids, 2)
	assert.True(t, ids[0].String() < ids[1].String())

	for _, raw := range [][]string{{uuid.Nil.String()}, {"not-a-uuid"}} {
		_, err := parseSortedVoiceParticipantIDs(raw, 2)
		assert.Error(t, err)
	}
	_, err = parseSortedVoiceParticipantIDs([]string{one.String(), two.String()}, 1)
	assert.Error(t, err)
}

func TestVoiceParticipantUnionRejectsOversizedExistingSet(t *testing.T) {
	existing := make([]uuid.UUID, maxServerVoiceParticipantIDs+1)
	assert.False(t, serverVoiceParticipantUnionWithinLimit(existing, nil))
	assert.True(t, privateVoiceParticipantUnionWithinLimit(nil, []uuid.UUID{uuid.New()}))
}
