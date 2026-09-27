package voice

import (
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerVoiceMutationReplayKey_RejectsInvalidIdentity(t *testing.T) {
	validTime := time.UnixMicro(1)
	for _, tc := range []struct {
		name     string
		senderID uuid.UUID
		eventAt  time.Time
	}{
		{name: "nil sender", senderID: uuid.Nil, eventAt: validTime},
		{name: "invalid event time", senderID: uuid.New(), eventAt: time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := serverVoiceMutationReplayKey(tc.senderID, tc.eventAt)
			require.Error(t, err)
		})
	}
}

func TestValidateServerVoiceMutationReplay_EnforcesCanonicalRemovalList(t *testing.T) {
	target := uuid.New()
	removed := uuid.New()
	valid, err := validateServerVoiceMutationReplay(serverVoiceMutationReplay{
		TargetRoomID:   target.String(),
		Added:          true,
		RemovedRoomIDs: []string{removed.String()},
	}, target)
	require.NoError(t, err)
	assert.True(t, valid.applied)
	assert.True(t, valid.duplicate)
	assert.Equal(t, []uuid.UUID{removed}, valid.removedRoomIDs)

	for _, tc := range []struct {
		name    string
		removal []string
	}{
		{name: "nil list", removal: nil},
		{name: "target room", removal: []string{target.String()}},
		{name: "duplicate room", removal: []string{removed.String(), removed.String()}},
		{name: "non-canonical room", removal: []string{"{" + removed.String()[1:]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateServerVoiceMutationReplay(serverVoiceMutationReplay{
				TargetRoomID: target.String(), RemovedRoomIDs: tc.removal,
			}, target)
			assert.Error(t, err)
		})
	}
}

func TestVoiceLifecycleAdvisoryKey_RejectsInvalidSender(t *testing.T) {
	_, err := voiceLifecycleAdvisoryKey(presence.CategoryServerVoice, uuid.Nil)
	assert.Error(t, err)
}
