package auth

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHandlerForInstance_WiresSelfHostedEntitlementCache(t *testing.T) {
	h := NewHandlerForInstance(nil, nil, logger.New("test"), "test-secret", nil, " self-hosted ")
	require.NotNil(t, h)

	assert.Equal(t, entitlements.TierPremium, h.entCache.GetTier(context.Background(), "user-1"))
}

func TestNewHandler_DefaultConstructorStillInitializes(t *testing.T) {
	h := NewHandler(nil, nil, logger.New("test"), "test-secret", nil)
	require.NotNil(t, h)
	assert.NotNil(t, h.entCache)
}

func TestSetPresenceHistoryStoresConcreteSharedService(t *testing.T) {
	h := NewHandler(nil, nil, logger.New("test"), "test-secret", nil)
	service := presencehistory.NewService(nil, presencehistory.DisclosureState{}, false)

	h.SetPresenceHistory(service)

	assert.Same(t, service, h.presenceHistory)
}
