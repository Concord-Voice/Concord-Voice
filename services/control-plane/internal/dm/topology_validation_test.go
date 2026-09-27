package dm

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTopologyHelpersRejectInvalidInputs(t *testing.T) {
	t.Run("advisory keys reject nil subjects", func(t *testing.T) {
		_, err := VoiceParticipantSetAdvisoryKey(uuid.Nil)
		assert.Error(t, err)
		_, err = PrivateVoiceScopeAdvisoryKey(uuid.Nil)
		assert.Error(t, err)
	})

	t.Run("transaction locks reject missing transaction or subjects", func(t *testing.T) {
		ctx := context.Background()
		assert.Error(t, LockDMVoiceParticipantSetTx(ctx, nil, uuid.New()))
		assert.Error(t, LockPrivateVoiceScopesTx(ctx, nil, []uuid.UUID{uuid.New()}))
		assert.Error(t, LockPrivateVoiceScopesTx(ctx, nil, nil))
	})

	t.Run("topology actor requires a UUID", func(t *testing.T) {
		_, err := topologyActor("not-a-uuid")
		assert.Error(t, err)
		actor := uuid.New()
		ids, err := topologyActor(actor.String())
		require.NoError(t, err)
		require.Len(t, ids, 1)
		assert.Equal(t, actor, ids[0])
	})
}

func TestHandlerSetActivePlanRailNilReceiverIsSafe(t *testing.T) {
	var handler *Handler
	assert.NotPanics(t, func() { handler.SetActivePlanRail(nil) })
	assert.False(t, handler.HasActivePlanRail())

	handler = &Handler{}
	handler.SetActivePlanRail(nil)
	assert.False(t, handler.HasActivePlanRail())
}
