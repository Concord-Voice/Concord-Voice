package voice

import (
	"context"
	"errors"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestHandleDMVoiceDependencyFailure_OnlyDisconnectsForRecoverableFailures(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		disconnect bool
	}{
		{name: "lease conflict", err: dm.ErrDMVoiceCallLeaseConflict},
		{name: "lease closed", err: dm.ErrDMVoiceCallLeaseClosed},
		{name: "canceled", err: context.Canceled, disconnect: true},
		{name: "deadline", err: context.DeadlineExceeded, disconnect: true},
		{name: "dependency", err: errors.New("redis unavailable"), disconnect: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			subscriber := &NATSSubscriber{
				log:                                  logger.New("test"),
				disconnectAllRichPresenceClientsHook: func() { called = true },
			}

			subscriber.handleDMVoiceDependencyFailure("ignored", tc.err)

			assert.Equal(t, tc.disconnect, called)
		})
	}
}

func TestVoiceLifecycleTokenState_RejectsInvalidInputsBeforeRedis(t *testing.T) {
	validID := uuid.New()
	for _, tc := range []struct {
		name     string
		subject  *NATSSubscriber
		category presence.Category
		sender   uuid.UUID
		token    uuid.UUID
	}{
		{name: "nil subscriber"},
		{name: "nil redis", subject: &NATSSubscriber{}},
		{name: "nil sender", subject: &NATSSubscriber{redis: nil}, sender: uuid.Nil, token: validID},
		{name: "nil token", subject: &NATSSubscriber{redis: nil}, sender: validID, token: uuid.Nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := tc.subject.voiceLifecycleTokenState(context.Background(), tc.category, tc.sender, tc.token)
			assert.Equal(t, voiceLifecycleTokenMissing, state)
			assert.Error(t, err)
		})
	}
}

func TestVoiceLifecycleAdvisoryKey_RejectsNilAndUnsupportedCategory(t *testing.T) {
	validID := uuid.New()
	_, err := voiceLifecycleAdvisoryKey(presence.CategoryServerVoice, uuid.Nil)
	assert.Error(t, err)
	_, err = voiceLifecycleAdvisoryKey(presence.Category("unknown"), validID)
	assert.Error(t, err)
	key, err := voiceLifecycleAdvisoryKey(presence.CategoryPrivateCall, validID)
	assert.NoError(t, err)
	assert.NotZero(t, key)
}
