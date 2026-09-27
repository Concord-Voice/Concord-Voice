package voice

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestVoiceParticipantLockHelpers_RejectInvalidInputs(t *testing.T) {
	assert.Error(t, lockVoiceParticipantUsersTx(context.Background(), nil, nil, dmblock.LockShare))
	assert.Error(t, lockVoiceParticipantUsersTx(context.Background(), nil, []uuid.UUID{uuid.Nil}, dmblock.LockShare))
	assert.Error(t, lockPrivateVoiceConversationUsersTx(context.Background(), nil, nil, nil))
	_, err := privateVoiceConversationSubjectsTx(context.Background(), nil, nil)
	assert.Error(t, err)
}

func TestPrivateVoiceParticipantUnionWithinLimit_DeduplicatesSharedMembers(t *testing.T) {
	shared := uuid.New()
	assert.True(t, privateVoiceParticipantUnionWithinLimit(
		[]uuid.UUID{shared}, []uuid.UUID{shared},
	))
}

func TestPrivateVoiceParticipantUnionWithinLimit_RejectsDistinctOverflow(t *testing.T) {
	existing := make([]uuid.UUID, maxPrivateVoiceParticipantIDs)
	incoming := []uuid.UUID{uuid.New()}
	for i := range existing {
		existing[i] = uuid.New()
	}
	assert.False(t, privateVoiceParticipantUnionWithinLimit(existing, incoming))
}

func TestServerVoiceParticipantUnionWithinLimit_RejectsOversizedInputAndDeduplicates(t *testing.T) {
	tooMany := make([]uuid.UUID, maxServerVoiceParticipantIDs+1)
	for i := range tooMany {
		tooMany[i] = uuid.New()
	}
	assert.False(t, serverVoiceParticipantUnionWithinLimit(tooMany, nil))
	assert.False(t, serverVoiceParticipantUnionWithinLimit(nil, tooMany))
	shared := uuid.New()
	assert.True(t, serverVoiceParticipantUnionWithinLimit([]uuid.UUID{shared}, []uuid.UUID{shared}))
}

func TestVoiceComparisonHelpers_DistinguishLengthAndValueChanges(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	assert.True(t, sameUUIDs([]uuid.UUID{first}, []uuid.UUID{first}))
	assert.False(t, sameUUIDs([]uuid.UUID{first}, []uuid.UUID{first, second}))
	assert.False(t, sameUUIDs([]uuid.UUID{first}, []uuid.UUID{second}))

	at := time.Unix(10, 0)
	left := []voiceParticipantRecord{{userID: first, lifecycleEventAt: at}}
	assert.True(t, sameVoiceParticipantRecords(left, left))
	assert.False(t, sameVoiceParticipantRecords(left, nil))
	assert.False(t, sameVoiceParticipantRecords(left, []voiceParticipantRecord{{userID: second, lifecycleEventAt: at}}))
	assert.False(t, sameVoiceParticipantRecords(left, []voiceParticipantRecord{{userID: first, lifecycleEventAt: at.Add(time.Second)}}))

	membership := []privateVoiceScopeMembership{{conversationID: first, userID: second}}
	assert.True(t, samePrivateVoiceScopeMemberships(membership, membership))
	assert.False(t, samePrivateVoiceScopeMemberships(membership, nil))
	assert.False(t, samePrivateVoiceScopeMemberships(membership, []privateVoiceScopeMembership{{conversationID: second, userID: first}}))
}
