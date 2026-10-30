package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// fakeAgeHub satisfies age.SessionDisconnector for the DI-builder test without a
// real WebSocket hub.
type fakeAgeHub struct{}

func (fakeAgeHub) DisconnectUser(uuid.UUID) {}
func (fakeAgeHub) DisconnectFamilyContext(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

// TestBuildAgeHandler verifies the #1623 DI builder wires a non-nil age.Handler.
// buildAgeHandler delegates to age.NewHandler, which stores its deps without
// dialing, so nil db/rdb are acceptable for this construction-only check.
func TestBuildAgeHandler(t *testing.T) {
	h := buildAgeHandler(nil, nil, fakeAgeHub{}, logger.New("test"))
	require.NotNil(t, h)
}
