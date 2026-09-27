package voice

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/stretchr/testify/assert"
)

func TestPrivateVoiceGuardRejectionIncludesMembershipDrift(t *testing.T) {
	assert.True(t, isPrivateVoiceGuardRejection(dmblock.ErrUnavailable))
	assert.True(t, isPrivateVoiceGuardRejection(dmblock.ErrMembershipChanged))
	assert.True(t, isPrivateVoiceGuardRejection(
		fmt.Errorf("prepare private voice conversation: %w", dmblock.ErrMembershipChanged),
	))
	assert.False(t, isPrivateVoiceGuardRejection(errors.New("unrelated private voice failure")))
}
