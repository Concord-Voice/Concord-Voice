package servers

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// A handler built without a hub (only tests do) has nobody to tell: the
// announcement is skipped, silently. The skip is not a failed delivery, so it
// must not log the perm_change_broadcast failure line either.
//
// Kills: the nil-hub guard in announceServerPermissionsChanged deleted (a nil
// dereference), and the guard moved below the log (a failure line for a
// handler that never had a hub).
func TestRefreshMFAEnforcementViews_NilHubSkipsAnnouncementSilently(t *testing.T) {
	var logs bytes.Buffer
	// A zero Resolver has no cache, so the bump that runs first is a no-op.
	h := &Handler{log: logger.NewWithWriter(&logs), resolver: &rbac.Resolver{}}
	require.Nil(t, h.hub, "setup: the handler has no hub")

	require.NotPanics(t, func() {
		h.refreshMFAEnforcementViews(context.Background(), uuid.NewString())
	})
	require.NotPanics(t, func() {
		h.announceServerPermissionsChanged(context.Background(), uuid.NewString())
	})
	assert.NotContains(t, logs.String(), "perm_change_broadcast")
	assert.Empty(t, logs.String(), "a skipped announcement logs nothing")
}
