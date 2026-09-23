package keyrotation_test

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextRotator_BroadcastContextRequiresConfiguredBroadcaster(t *testing.T) {
	r := keyrotation.NewContextRotator(nil, logger.New("test"), nil, nil)
	err := r.BroadcastContext(context.Background(), keyrotation.Rotation{ChannelID: "channel"})
	assert.EqualError(t, err, "context key-revocation broadcaster unavailable")
}

func TestContextRotator_BroadcastContextUsesConfiguredBroadcaster(t *testing.T) {
	var got keyrotation.Rotation
	r := keyrotation.NewContextRotator(nil, logger.New("test"), nil, func(_ context.Context, rotation keyrotation.Rotation) error {
		got = rotation
		return nil
	})
	want := keyrotation.Rotation{ChannelID: "channel", SuccessorEpoch: 4}
	require.NoError(t, r.BroadcastContext(context.Background(), want))
	assert.Equal(t, want, got)
}
