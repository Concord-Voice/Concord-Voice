package keyrotation_test

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/require"
)

func TestCoverageGate2907_BroadcastContextPropagatesBroadcasterError(t *testing.T) {
	want := context.Canceled
	r := keyrotation.NewContextRotator(nil, logger.New("test"), nil,
		func(context.Context, keyrotation.Rotation) error { return want })
	require.ErrorIs(t, r.BroadcastContext(context.Background(), keyrotation.Rotation{}), want)
}
