package messages_test

// Regression for review of #3509 (Codex P1): a self-purge the unlocked
// population read put OUTSIDE the population carried neither a victim fence
// nor a confirmation, and the per-batch guard never re-reads the server's
// enforcement flag. An owner turning enforcement ON between that read and the
// admission therefore let the actor delete their whole matching history with
// no counter charged and no MFA asked for. The admission must re-read the flag
// under the servers-row lock and, if it now enforces, confirm the server rule.

import (
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

// selfPurgePopulationRead is a fragment of the self-purge's unlocked
// population read (selfPurgeSoftLockQuery). The empty fragment after it in
// each Arm matches whatever statement comes next, so the flip lands after
// that read and before anything the purge's admission sends.
const selfPurgePopulationRead = "SELECT s.enforce_mfa_dangerous_actions,"

// newHookedSoftLockHarness is newSoftLockHarness on a stmthook pool.
func newHookedSoftLockHarness(t *testing.T) (*softLockHarness, *stmthook.Hook) {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	hook, db := stmthook.Open(t)
	mr := miniredis.RunT(t)
	return buildSoftLockHarness(t, ts, db, fastRedisClient(t, mr.Addr()), mr), hook
}

// TestSelfPurge_EnforcementTurnedOnBeforeAdmission: an enrolled author whose
// own rule is off purges their own three messages on a server that does not
// enforce, so the unlocked read puts them outside the population. The flag is
// turned ON right after that read.
//
//   - control: nothing flips, so the purge still runs uncounted and asks for
//     no factor — a non-enforcing server keeps its behaviour.
//   - turned on, no factor: the server rule now governs and the request
//     carries no code, so it is refused exactly as a soft-locked request
//     without one is, and nothing is purged or audited.
//   - turned on, valid code: the server rule is confirmed in the admission,
//     so the purge runs, and the code was verified for this route.
func TestSelfPurge_EnforcementTurnedOnBeforeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server bool
		flip   bool
		body   map[string]any
	}{
		{name: "control: still not enforcing", body: map[string]any{"range": "all"}},
		{name: "turned on, no factor", flip: true, body: map[string]any{"range": "all"}},
		{name: "turned on, no factor, server route", server: true, flip: true,
			body: map[string]any{"range": "all"}},
		{name: "turned on, valid code", flip: true,
			body: map[string]any{"range": "all", "mfa_code": softLockValidCode}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, hook := newHookedSoftLockHarness(t)
			w := s.world(t, false)
			s.setOwnRule(t, w.author.ID, false)
			s.enroll(t, w.author.ID)
			s.seed(t, w.channelID, w.author, 3)

			var between func() error
			if tc.flip {
				between = func() error {
					_, err := s.ts.DB.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = TRUE WHERE id = $1`, w.serverID)
					return err
				}
			}
			hook.Arm([]string{selfPurgePopulationRead, ""}, between, nil)

			body := tc.body
			purge := s.purgeChannel
			path := w.channelID
			if tc.server {
				body["target_user_id"] = w.author.ID
				purge, path = s.purgeServer, w.serverID
			}
			r := purge(t, w.author.ID, path, body)
			code, raw := r.Code, r.Body.String()
			if tc.flip && tc.body["mfa_code"] == nil {
				requireSoftLockRefusal(t, r, "mfa_required")
			}

			seen, betweenErr := hook.Report()
			require.Equal(t, 2, seen, "the hook fired after the population read")
			require.NoError(t, betweenErr)

			switch {
			case !tc.flip:
				require.Equal(t, http.StatusOK, code, raw)
				assert.Zero(t, s.countBy(t, w.author.ID), "a non-enforcing server's purge runs")
				assert.Empty(t, s.verifier.calls(), "and asks for no factor")
				assert.Empty(t, s.counter(burstKey(w.author.ID, w.serverID)), "and is never counted")
			case tc.body["mfa_code"] == nil:
				assert.Equal(t, 3, s.countBy(t, w.author.ID), "nothing is purged")
				assert.Zero(t, s.auditRows(t, w.channelID)+s.auditRows(t, w.serverID), "a refusal leaves no audit row")
			default:
				require.Equal(t, http.StatusOK, code, raw)
				assert.Zero(t, s.countBy(t, w.author.ID), "a confirmed purge runs")
				assert.Equal(t, []stepup.Purpose{stepup.PurposeChannelPurge}, s.verifier.calls(),
					"the server rule's code was verified in the admission")
				assert.True(t, s.totpStepSpent(t, w.author.ID), "and spent with it")
			}
		})
	}
}
