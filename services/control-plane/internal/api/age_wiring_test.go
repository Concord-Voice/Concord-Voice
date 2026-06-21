package api

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// fakeAgeHub satisfies age.SessionDisconnector for the DI-builder test without a
// real WebSocket hub.
type fakeAgeHub struct{}

func (fakeAgeHub) DisconnectUser(uuid.UUID) {}

// TestBuildAgeHandler verifies the #1623 DI builder wires a non-nil age.Handler.
// buildAgeHandler delegates to age.NewHandler, which stores its deps without
// dialing, so nil db/rdb are acceptable for this construction-only check.
func TestBuildAgeHandler(t *testing.T) {
	h := buildAgeHandler(nil, nil, fakeAgeHub{}, logger.New("test"))
	require.NotNil(t, h)
}
