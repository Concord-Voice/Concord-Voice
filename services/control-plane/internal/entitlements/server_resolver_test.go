package entitlements_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
)

// insertServer creates a minimal servers row owned by a fresh user so the
// resolver has a real server_id to resolve against (proving it still returns
// Groundspeed today — the Mach hook is inert).
func insertServer(t *testing.T, ts *testhelpers.TestServer) string {
	t.Helper()
	owner := insertUser(t, ts)
	return ts.CreateTestServer(t, owner, "Resolver Test Server")
}

func TestResolveServerTier_AllServersGroundspeed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	// A real, existing server still resolves Groundspeed — server subscriptions
	// do not exist yet; the Mach hook is present but inert.
	serverID := insertServer(t, ts)
	assert.Equal(t, entitlements.TierGroundspeed, entitlements.ResolveServerTier(ctx, ts.DB, serverID))

	// An arbitrary (non-existent) server id also resolves Groundspeed.
	assert.Equal(t, entitlements.TierGroundspeed, entitlements.ResolveServerTier(ctx, ts.DB, uuid.New().String()))
}
