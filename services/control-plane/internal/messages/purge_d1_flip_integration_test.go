//go:build integration

package messages_test

// #3454 A-3.6: the per-batch enforcement recheck. The server turns MFA
// enforcement ON after a purge's admission committed and before its first
// batch. The flip is ordered with no sleep: a statement hook runs it at the
// batch's credential-epoch fence — the first statement after the admission's
// audit INSERT — when the batch transaction holds only the actor's users row,
// so the toggle's UPDATE commits at once and the recheck that follows reads ON.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// batchFenceAfterAdmission is the hook sequence: the admission's audit row,
// then the next batch's GuardTx.
var batchFenceAfterAdmission = []string{
	"INSERT INTO message_purges",
	"SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE",
}

// TestPurgeD1Gate_FlipOnBetweenAdmissionAndBatch:
//
//   - D1 spec, admitted unconfirmed (the server did not enforce): the batch
//     is refused, the engine's existing partial outcome with nothing deleted;
//   - self spec (exempt): the batch runs, because #3455's admission-time
//     recheck already covers the self-purge;
//   - control: with no flip the D1 purge runs.
//
// Mutant killed: deleting recheckPurgeEnforcementTx from the batch guard (the
// unconfirmed D1 batch runs after the flip).
func TestPurgeD1Gate_FlipOnBetweenAdmissionAndBatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		self  bool
		flip  bool
		want  int
		after int
	}{
		{name: "D1 spec, unconfirmed", flip: true, want: http.StatusInternalServerError, after: 3},
		{name: "self spec, exempt", self: true, flip: true, want: http.StatusOK},
		{name: "control: no flip", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, hook := newHookedSoftLockHarness(t)
			w := s.world(t, false)
			s.setOwnRule(t, w.author.ID, false)
			s.seed(t, w.channelID, w.author, 3)
			flipped := false
			between := func() error {
				flipped = true
				_, err := s.ts.DB.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = TRUE WHERE id = $1`, w.serverID)
				return err
			}
			if !tc.flip {
				between = nil
			}
			hook.Arm(batchFenceAfterAdmission, between, nil)

			actor, body := w.moderator.ID, map[string]any{"range": "all", "target_user_id": w.author.ID}
			if tc.self {
				actor, body = w.author.ID, map[string]any{"range": "all"}
			}
			res := s.d1Purge(t, actor, "/channels/"+w.channelID+"/messages", body)

			seen, betweenErr := hook.Report()
			require.Equal(t, 2, seen, "the hook fired at the first batch, after the admission")
			require.NoError(t, betweenErr)
			assert.Equal(t, tc.flip, flipped)
			require.Equal(t, tc.want, res.Code, res.Body.String())
			assert.Equal(t, tc.after, s.countBy(t, w.author.ID))
			assert.Empty(t, s.verifier.calls(), "the recheck never re-verifies")
		})
	}
}

// TestPurgeD1Gate_BatchGuardRS5: an enrolled moderator confirms a D1 purge on
// an enforcing server, then loses their only factor before the first batch.
// The batch guard's re-derived author filter is refused because the mask now
// withholds ManageAllMessages, and with nothing deleted the answer is RS5's
// mfa_enrollment_required, read on the batch's transaction. The refusal is
// answered before the failure line, which carries channel_id, so it is never
// logged as a failed purge (observability principle 7, C7; Codex review of
// #3454).
//
// Mutants killed: dropping RS5 from recheckPurgeAuthorTx (a 500); logging the
// failure line before respondChannelPurgeFailure answers the refusal.
func TestPurgeD1Gate_BatchGuardRS5(t *testing.T) {
	s, hook := newHookedSoftLockHarness(t)
	w := s.world(t, true)
	s.enroll(t, w.moderator.ID)
	s.seed(t, w.channelID, w.author, 3)
	hook.Arm(batchFenceAfterAdmission, func() error {
		_, err := s.ts.DB.Exec(`DELETE FROM user_mfa_totp WHERE user_id = $1`, w.moderator.ID)
		return err
	}, nil)

	res := s.d1Purge(t, w.moderator.ID, "/channels/"+w.channelID+"/messages",
		map[string]any{"range": "all", "target_user_id": w.author.ID, "mfa_code": softLockValidCode})

	seen, betweenErr := hook.Report()
	require.Equal(t, 2, seen)
	require.NoError(t, betweenErr)
	require.Equal(t, http.StatusForbidden, res.Code, res.Body.String())
	assert.JSONEq(t, `{"error":"Set up an authenticator app or security key to do this.","mfa_enrollment_required":true}`, res.Body.String())
	assert.Equal(t, 3, s.countBy(t, w.author.ID))
	assert.NotContains(t, s.logs.String(), "Channel purge failed", "a refusal is not a failed purge")
}
