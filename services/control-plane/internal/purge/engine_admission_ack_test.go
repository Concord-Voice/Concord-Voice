package purge

// Regression for review of #3509 (Codex P2): when the admission transaction —
// Plan.Admit plus the in_progress audit row — commits but its COMMIT
// acknowledgement is lost, Run reported ErrNotAdmitted, so the caller skipped
// the batches and the settlement although the factor and the audit row had
// committed. Run must reconcile a failed COMMIT by reading the audit row it
// generated before it classifies the admission.

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// admissionReconcileRead is a fragment of the read that reconciles a failed
// admission COMMIT against the audit row.
const admissionReconcileRead = "SELECT EXISTS (SELECT 1 FROM message_purges WHERE id"

var errCommitAckLost = errors.New("connection lost after COMMIT was sent")

// TestEngineRun_AdmissionCommitFails_IsReconciled pins the three outcomes of
// a failed admission COMMIT:
//
//   - the COMMIT landed (acknowledgement lost): the audit row is there, so the
//     purge was admitted and runs to completion;
//   - the COMMIT did not land: no audit row, so ErrNotAdmitted, nothing
//     deleted (a control: this held before the fix too);
//   - the COMMIT landed and the reconciling read fails: the outcome is
//     unknown, so the error must not claim "not admitted", and nothing is
//     deleted on an admission Run cannot prove.
func TestEngineRun_AdmissionCommitFails_IsReconciled(t *testing.T) {
	for _, tc := range []struct {
		name       string
		committed  bool
		failRead   bool
		wantErr    bool
		admitted   bool
		wantStatus string
	}{
		{name: "commit landed, acknowledgement lost", committed: true, admitted: true, wantStatus: "completed"},
		{name: "control: commit refused", committed: false, wantErr: true},
		{name: "commit landed, reconciling read fails", committed: true, failRead: true, wantErr: true,
			wantStatus: "in_progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedEngineFixture(t)
			f.seedMessages(t, f.authorID, 3, 0)
			hook, db := stmthook.Open(t)
			log := logger.NewWithWriter(io.Discard)
			e := NewEngine(db, log, NewReaper(db, log, nil), 5000)
			if tc.failRead {
				hook.Arm([]string{admissionReconcileRead}, nil, stmthook.ErrInjected)
			}

			plan := f.channelPlan()
			plan.Admit = func(context.Context, *sql.Tx) error {
				hook.ArmCommit(tc.committed, errCommitAckLost)
				return nil
			}
			res, err := e.Run(context.Background(), plan)

			var audits int
			require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM message_purges WHERE context_id = $1`,
				f.channelID).Scan(&audits))
			switch {
			case tc.admitted:
				require.NoError(t, err, "an admission whose audit row committed is admitted")
				assert.Equal(t, 3, res.DeletedCount)
				assert.Zero(t, f.countMessages(t))
			case tc.committed:
				require.Error(t, err)
				require.ErrorIs(t, err, ErrAdmissionUnknown)
				assert.False(t, errors.Is(err, ErrNotAdmitted),
					"an admission whose audit row committed must not be reported as not admitted: %v", err)
				assert.Equal(t, 3, f.countMessages(t), "nothing is deleted on an unproven admission")
			default:
				require.ErrorIs(t, err, ErrNotAdmitted)
				assert.Zero(t, audits, "a refused COMMIT leaves no audit row")
				assert.Equal(t, 3, f.countMessages(t))
			}
			if tc.failRead {
				seen, _ := hook.Report()
				assert.Equal(t, 1, seen, "the reconciling read was attempted")
			}
			if tc.wantStatus != "" {
				require.Equal(t, 1, audits)
				status, _, _ := f.auditRow(t)
				assert.Equal(t, tc.wantStatus, status)
			}
		})
	}
}
