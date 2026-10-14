//go:build integration

package channels_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

// chanExpirationScopeLockSQL matches only lockChannelExpirationScopeTx's
// channel lock.
const chanExpirationScopeLockSQL = `FROM channels WHERE id = $1 AND server_id = $2 FOR NO KEY UPDATE`

// AC7: the prior include_pinned value is read UNDER the channel lock. The
// channel starts with pinned expiry on (E true), so a request that sends true
// is a no-op the gate admits; a toggle-off committed by another session
// immediately before the lock statement turns that same request into a
// false->true change, and the gate must judge it against the value it locked.
// The control runs the same request with nothing injected.
//
// Mutant killed: judging the include arm on a value read before the channel
// lock (the injected toggle-off is invisible and the request is admitted).
func TestChannelExpirationGate_PinnedPriorIsReadUnderTheLock(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	for _, tc := range []struct {
		name       string
		inject     bool
		wantStatus int
	}{
		{"control: nothing injected", false, http.StatusOK},
		{"toggle-off injected before the lock", true, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChanGateFixture(t, ts)
			path := pathChannelsPrefix + f.channelID + "/expiration"
			auth := testhelpers.AuthHeaders(f.mod.AccessToken)
			hook.Arm([]string{"never-matches-anything"}, nil, nil)
			w := ts.DoRequest(http.MethodPatch, path, pinnedSetBody(86400, boolRef(true), ""), auth)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
			enrollChanGateWebAuthn(t, ts.DB, f.mod.ID)

			if tc.inject {
				hook.Arm([]string{chanGateLockSQL, chanExpirationScopeLockSQL}, func() error {
					_, err := ts.DB.Exec(`UPDATE channels SET expiration_include_pinned = false WHERE id = $1`, f.channelID)
					return err
				}, nil)
			}
			w = ts.DoRequest(http.MethodPatch, path, pinnedSetBody(86400, boolRef(true), ""), auth)
			if tc.inject {
				seen, betweenErr := hook.Report()
				require.Equal(t, 2, seen, "the toggle-off must land between the gate's servers read and the channel lock")
				require.NoError(t, betweenErr)
			}
			require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.inject {
				assert.JSONEq(t, chanGateBodyMFARequired, w.Body.String())
				assert.False(t, readChanPinnedPolicy(t, ts.DB, f.channelID).effective, "the refusal wrote nothing over the toggle-off")
			}
		})
	}
}
