package members_test

// Codex's sixth review of #3508.

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

// A genuine fault at the hierarchy check must stay a fault even when the
// server is also deleted. The re-read before a refusal exists for a deletion
// the check misread as a missing row or a violation; a fault that merely
// coincides with a deletion is not that, and confirming the deletion must not
// turn the outage into the route's vanished answer.
func TestModerationHierarchy_FaultWithServerGone_StaysAFault(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	sc := stmthook.Scenario{
		Name:      "a real fault at the hierarchy check while the server is deleted",
		DeleteSQL: `DELETE FROM servers WHERE id = $1`,
		Fault:     stmthook.ErrInjected,
		Want:      stmthook.Fault,
	}

	ran := 0
	for _, site := range moderationSites() {
		if !strings.Contains(site.name, "hierarchy") {
			continue
		}
		ran++
		t.Run(site.name, func(t *testing.T) {
			f := newModerationFixture(t, ts)
			hook.Arm(site.sequence, sc.Between(ts.DB, f.owner.ID, f.serverID), sc.Fault)

			w := ts.DoRequest(site.method, site.path(f), site.body, testhelpers.AuthHeaders(f.actor.AccessToken))

			stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(site.sequence), f.serverID)
			assert.Equal(t, sc.Want, site.classify(w.Code, w.Body.String()))
		})
	}
	require.Equal(t, 6, ran, "two hierarchy sites for each of removal, timeout and ban")
}

// A pre-emptive ban (the target is not a member) runs through presencehook's
// unwired WithGatedTx branch, which logged a failed rollback and kept the
// work's denial. The hook ends the hooked transaction's own backend and then
// answers the ban's server lock with no row, which the ban reads as its
// vanished-server 403. The control leaves the connection alive.
func TestBanMember_PreemptiveFailedDiscard_IsAFault(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	for _, terminate := range []bool{false, true} {
		name, wantCode, wantBody := "control: the discard succeeds", http.StatusForbidden, bodyInsufficient
		if terminate {
			name, wantCode, wantBody = "the discard fails", http.StatusInternalServerError, `{"error":"Failed to ban member"}`
		}
		t.Run(name, func(t *testing.T) {
			f := newModerationFixture(t, ts)
			outsider := ts.CreateTestUser(t, "modpo"+uuid.NewString()[:8])
			terminated := 0
			var between func() error
			if terminate {
				// The ban transaction's last statement before the server lock is the
				// gate's enrollment read (#3454: LockGateTx follows the
				// credential-epoch guard and reads the actor's factors before it
				// locks the servers row).
				between = stmthook.TerminateIdleBackend(ts.DB, `FROM user_mfa_webauthn WHERE user_id = $1`, &terminated)
			}
			hook.Arm([]string{txGateLock}, between, sql.ErrNoRows)

			w := ts.DoRequest(http.MethodPost, "/api/v1/servers/"+f.serverID+"/bans/"+outsider.ID, nil,
				testhelpers.AuthHeaders(f.actor.AccessToken))

			seen, betweenErr := hook.Report()
			require.Equal(t, 1, seen, "the hook must fire at the ban's server lock")
			require.NoError(t, betweenErr)
			if terminate {
				require.Equal(t, 1, terminated, "exactly the hooked backend must be ended")
			}
			assert.Equal(t, wantCode, w.Code)
			assert.Equal(t, wantBody, w.Body.String())
		})
	}
}
