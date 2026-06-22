//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/redemption"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// fakeSessionNotifier records OnTierChange's broadcast/disconnect calls so the
// redemptionTierNotifier adapter can be exercised without a live hub.
type fakeSessionNotifier struct {
	broadcasts  int
	disconnects int
}

func (f *fakeSessionNotifier) DisconnectUser(uuid.UUID) { f.disconnects++ }
func (f *fakeSessionNotifier) BroadcastEntitlements(uuid.UUID, entitlements.EntitlementDTO) {
	f.broadcasts++
}

// TestBuildRedemptionHandler_Constructs verifies the wiring builds a non-nil
// handler in both the admin-token-set and admin-token-empty configs (empty
// disables the HTTP gen endpoint but must still construct the handler so the
// /redeem route works).
func TestBuildRedemptionHandler_Constructs(t *testing.T) {
	log := logger.New("test")
	entCache := entitlements.NewCache(nil, nil) // cache handle unused in construction
	notifier := &fakeSessionNotifier{}

	t.Run("with admin token", func(t *testing.T) {
		cfg := &config.Config{RedemptionAdminToken: "0123456789abcdef0123456789abcdef"} //nolint:gosec // test fixture // pragma: allowlist secret
		h := buildRedemptionHandler(nil, entCache, notifier, cfg, log)
		require.NotNil(t, h)
	})

	t.Run("empty admin token still constructs", func(t *testing.T) {
		cfg := &config.Config{RedemptionAdminToken: ""}
		h := buildRedemptionHandler(nil, entCache, notifier, cfg, log)
		require.NotNil(t, h)
	})
}

// TestRedemptionTierNotifier_SatisfiesInterface pins the compile-time contract
// that the api-side adapter is a valid redemption.Notifier — the engine injects
// it post-commit. The BEHAVIORAL forwarding (OnTierChange broadcasts the new
// set free→premium) is covered end-to-end by the redemption engine's
// integration tests (recordingNotifier asserts the call + tiers) and by the
// entitlements package's own OnTierChange tests; reproducing it here would
// require a live Redis (cache.Invalidate calls redis.Unlink). This test guards
// the wiring seam without that dependency.
func TestRedemptionTierNotifier_SatisfiesInterface(t *testing.T) {
	notifier := &fakeSessionNotifier{}
	var n redemption.Notifier = &redemptionTierNotifier{cache: nil, notifier: notifier}
	require.NotNil(t, n)
}
