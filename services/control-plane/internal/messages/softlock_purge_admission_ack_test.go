package messages_test

// Handler half of the lost admission acknowledgement (review of #3509, Codex
// P2): an over-threshold self-purge confirms in the admission transaction,
// and that transaction's COMMIT reports an error after the server committed.

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

// TestSelfPurge_AdmissionAckLost:
//
//   - acknowledgement lost: the factor and the audit row committed, so the
//     purge is admitted and runs, and the settlement follows a success —
//     before the fix it answered 500 and purged nothing although the factor
//     was spent;
//   - reconciling read fails too: the admission cannot be proven either way,
//     so the answer is a 500 and nothing is settled — the budget stays
//     charged and the counters stand — and nothing is purged.
func TestSelfPurge_AdmissionAckLost(t *testing.T) {
	errAckLost := errors.New("connection lost after COMMIT was sent")
	for _, tc := range []struct {
		name     string
		failRead bool
	}{
		{name: "acknowledgement lost"},
		{name: "reconciling read fails too", failRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, hook := newHookedSoftLockHarness(t)
			w := s.world(t, false)
			s.enroll(t, w.author.ID)
			s.seed(t, w.channelID, w.author, 16)
			if tc.failRead {
				hook.Arm([]string{"SELECT EXISTS (SELECT 1 FROM message_purges WHERE id"}, nil, stmthook.ErrInjected)
			}
			s.handler.SetBeforeSoftLockConfirmHookForTest(func() { hook.ArmCommit(true, errAckLost) })

			res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all", "mfa_code": softLockValidCode})

			assert.True(t, s.totpStepSpent(t, w.author.ID), "the admission committed, spending the factor")
			assert.Equal(t, 1, s.auditRows(t, w.channelID), "with its audit row")
			if !tc.failRead {
				require.Equal(t, http.StatusOK, res.Code, res.Body.String())
				assert.EqualValues(t, 16, decode(t, res)["deleted_count"])
				assert.Zero(t, s.countBy(t, w.author.ID))
				assert.Empty(t, s.counter(budgetKey(w.author.ID)), "the budget is cleared")
				assert.Empty(t, s.counter(burstKey(w.author.ID, w.serverID)), "and the counters reset")
				return
			}
			require.Equal(t, http.StatusInternalServerError, res.Code, res.Body.String())
			assert.Equal(t, 16, s.countBy(t, w.author.ID), "nothing is purged on an unproven admission")
			assert.Equal(t, "1", s.counter(budgetKey(w.author.ID)), "and nothing is settled: the budget stays charged")
			assert.Equal(t, "16", s.counter(burstKey(w.author.ID, w.serverID)), "and the counters stand")
		})
	}
}
