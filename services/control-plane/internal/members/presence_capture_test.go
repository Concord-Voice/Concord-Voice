package members

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

var errCaptureDoubleReached = errors.New("members: capture double reached")

// recordingCapture is a GraphPresenceCapture double that records the Subject the
// handler hands it. By default it exercises handler-side wiring without a
// database; its optional db is used by the capture-failure test so the handler
// closure reaches the real transaction guard before the forced capture error.
//
// WithGatedTx returns a sentinel rather than running work: the point of these
// tests is the SUBJECT, and running the closure would need a real *sql.Tx. A
// double that swallowed the work and reported success would let a handler whose
// mutation never ran look healthy, so the sentinel makes that loud.
type recordingCapture struct {
	subjects []presencecapture.Subject

	// runWork makes WithGatedTx invoke the handler's closure instead of
	// short-circuiting, so the capture-failure branch inside it can be reached.
	// With no db it hands the closure a nil *sql.Tx for subject-only tests. That
	// mode is safe only when the closure returns before any statement runs.
	runWork    bool
	captureErr error
	db         *sql.DB
}

func (r *recordingCapture) WithGatedTx(
	ctx context.Context, subject presencecapture.Subject, work func(*sql.Tx) error,
) (err error) {
	r.subjects = append(r.subjects, subject)
	if r.runWork {
		if r.db == nil {
			return work(nil)
		}
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				err = errors.Join(err, fmt.Errorf("rollback capture-double transaction: %w", rollbackErr))
			}
		}()
		if err = work(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	return errCaptureDoubleReached
}

func (r *recordingCapture) CaptureInTx(
	context.Context, *sql.Tx, presencecapture.Subject,
) (presencecapture.Plan, error) {
	if r.captureErr != nil {
		return nil, r.captureErr
	}
	return nil, nil
}
func (*recordingCapture) Complete(context.Context, *sql.Tx, presencecapture.Plan) error { return nil }
func (*recordingCapture) Abandon(presencecapture.Plan, presencecapture.Cause)           {}

func newCaptureHandler(t *testing.T) (*Handler, *recordingCapture) {
	t.Helper()
	capture := &recordingCapture{}
	h := &Handler{log: logger.New("test")}
	h.SetGraphPresenceCapture(capture)
	return h, capture
}

const (
	captureServerID = "11111111-1111-1111-1111-111111111111"
	captureTargetID = "22222222-2222-2222-2222-222222222222"
	captureActorID  = "33333333-3333-3333-3333-333333333333"
)

// The removal family must be the REVOKING one, and the principal must be the
// user losing membership rather than the moderator performing the removal. Get
// the principal wrong and the capture reconciles the wrong person's audience.
func TestRemovalCaptureSubject(t *testing.T) {
	h, capture := newCaptureHandler(t)

	err := h.execRemovalTx(context.Background(), captureServerID, captureTargetID, captureTargetID)
	require.ErrorIs(t, err, errCaptureDoubleReached,
		"the removal must enter through presencehook.WithGatedTx")

	require.Len(t, capture.subjects, 1)
	subject := capture.subjects[0]
	require.Equal(t, presencecapture.FamilyMemberRemove, subject.Family,
		"kick and self-leave revoke shared-server visibility")
	require.Equal(t, presencecapture.FailClosedBlockWrite, subject.FailPosture,
		"a capture failure must refuse the removal, never complete one it cannot reconcile")
	require.Equal(t, captureTargetID, subject.Principal.String(),
		"the principal is the removed user, not the actor")
}

// Ban is the sharpest case: a banned user still observing the server's voice
// activity is a moderation-bypass signal, not merely stale state.
func TestBanCaptureSubject(t *testing.T) {
	h, capture := newCaptureHandler(t)

	// probedMember=true is the MEMBER case: the ban revokes a real audience, so
	// the gate is warranted and the capture must run (#2854 stage C).
	err := h.execBanTx(context.Background(), captureServerID, captureTargetID, captureActorID, nil, true)
	require.ErrorIs(t, err, errCaptureDoubleReached,
		"the ban must enter through presencehook.WithGatedTx")

	require.Len(t, capture.subjects, 1)
	subject := capture.subjects[0]
	require.Equal(t, presencecapture.FamilyMemberBan, subject.Family)
	require.Equal(t, presencecapture.FailClosedBlockWrite, subject.FailPosture)
	require.Equal(t, captureTargetID, subject.Principal.String(),
		"the principal is the banned user, not the moderator")
}

// The revoking sites must declare families the registry marks as revoking, and
// the additive sites must not. This is the invariant that keeps a join from
// disconnecting the joiner's own devices.
func TestMembershipFamilyPolicies(t *testing.T) {
	revoking, err := presencecapture.PolicyFor(presencecapture.FamilyMemberRemove)
	require.NoError(t, err)
	require.True(t, revoking.CanRevokeVisibility)

	banned, err := presencecapture.PolicyFor(presencecapture.FamilyMemberBan)
	require.NoError(t, err)
	require.True(t, banned.CanRevokeVisibility)

	added, err := presencecapture.PolicyFor(presencecapture.FamilyMemberAdd)
	require.NoError(t, err)
	require.False(t, added.CanRevokeVisibility,
		"direct add is additive; revoking would tear down the added user's devices")
}

// The removal path fails CLOSED on a capture read failure: the handler returns
// an error and the mutation never runs, so nothing is written and nothing is
// disclosed. An uncleared viewer is a disclosure; refusing the write is not.
//
// Removal only. The ban path now probes membership inside the transaction
// before capturing (see TestBanProbesMembershipBeforeCapturing), so it cannot
// reach a capture without a database and its fail-closed behaviour is asserted
// end-to-end by TestBanMemberFailsClosedWhenTheDeleteFails instead.
func TestRemovalFailsClosedOnCaptureFailure(t *testing.T) {
	forced := errors.New("forced capture read failure")
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	ownerID := dbtest.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'capture failure server', $2)`, captureServerID, ownerID)
	require.NoError(t, err)
	capture := &recordingCapture{runWork: true, captureErr: forced, db: db}
	h := &Handler{db: db, log: logger.New("test")}
	h.SetGraphPresenceCapture(capture)

	err = h.execRemovalTx(context.Background(), captureServerID, captureTargetID, captureTargetID)

	require.ErrorIs(t, err, forced,
		"a capture read failure must surface, not be swallowed into a successful removal")
}

// A post-commit delivery failure must classify as 503 DURABLE, never 500. The
// distinction is what tells the handler to de-authorize before responding: a
// 500 means nothing happened, and acting on that reading is what left removed
// members holding a live RBAC cache entry.
func TestPostCommitDeliveryClassifiesAsDurable(t *testing.T) {
	err := fmt.Errorf("deliver reconcile: %w", presencecapture.ErrPostCommitDelivery)

	failure := presencehook.Classify(err)

	require.Equal(t, http.StatusServiceUnavailable, failure.Status,
		"a committed mutation whose delivery failed is 503, not 500")
	require.True(t, errors.Is(err, presencecapture.ErrPostCommitDelivery),
		"the sentinel must survive wrapping; the handlers branch on errors.Is")
}

// Coverage limit, stated rather than implied: driving execRemovalTx all the way
// to Complete requires a real transaction (a nil *sql.Tx panics on the first
// statement), and package members cannot import testhelpers without an import
// cycle. So the classification contract above is unit-tested, and the
// end-to-end de-authorization-before-503 behaviour is verified by reading the
// handlers. That gap is worth closing with a self-contained DB harness of the
// kind internal/age and internal/voice use.

// respondDurableDeliveryFailure must carry the purge result through. The purge
// already ran by the time it is called; dropping it loses a moderation outcome
// on exactly the path where the caller most needs to know it happened.
func TestDurableDeliveryFailureCarriesThePurgeResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{log: logger.New("test")}
	failure := presencehook.Classify(
		fmt.Errorf("deliver: %w", presencecapture.ErrPostCommitDelivery))

	t.Run("with a purge", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		h.respondDurableDeliveryFailure(c, &failure, "failed", gin.H{
			"purge": gin.H{"deleted": 7},
		})

		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		require.Contains(t, rec.Body.String(), "deleted",
			"the purge result must survive into the durable-failure body")
	})

	t.Run("without a purge", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		h.respondDurableDeliveryFailure(c, &failure, "failed", gin.H{})

		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		require.NotContains(t, rec.Body.String(), "purge",
			"no purge key when none ran")
	})
}

// classifyMutationOutcome is the split the whole post-commit fix rests on. Both
// arms matter, and they must behave OPPOSITELY: a pre-commit failure responds
// and stops the handler, a durable one stays silent so the handler can finish
// de-authorizing first.
func TestClassifyMutationOutcomeSplitsDurableFromTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{log: logger.New("test")}

	t.Run("pre-commit failure responds and halts", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)

		failure, handled := h.classifyMutationOutcome(
			c, errors.New("begin tx: connection refused"), "log", "user-facing")

		require.True(t, handled, "nothing committed, so the handler must stop here")
		require.Nil(t, failure)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("durable delivery failure defers and continues", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)

		failure, handled := h.classifyMutationOutcome(
			c, fmt.Errorf("deliver: %w", presencecapture.ErrPostCommitDelivery),
			"log", "user-facing")

		require.False(t, handled,
			"the mutation committed; the handler must continue and de-authorize")
		require.NotNil(t, failure)
		require.Equal(t, http.StatusServiceUnavailable, failure.Status)
		// httptest defaults Code to 200, so an empty body is what proves nothing
		// was written — the response comes after de-authorization.
		require.Empty(t, rec.Body.String(),
			"nothing may be written yet; the handler still has de-authorization to do")
	})
}

func TestClassifyModerationTxErrorMapsTransactionDenialsAndDelegates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{log: logger.New("test")}
	cases := []struct {
		name, errText, hierarchyText, want string
		err                                error
	}{
		{name: "kick current owner", err: errRemoveCurrentOwner, errText: "Cannot remove the server owner", want: "Cannot remove the server owner"},
		{name: "self leave current owner", err: errRemoveCurrentOwner, errText: "Server owner cannot leave. Delete the server or transfer ownership first.", want: "Server owner cannot leave. Delete the server or transfer ownership first."},
		{name: "ban current owner", err: errBanCurrentOwner, errText: "Cannot ban the server owner", want: "Cannot ban the server owner"},
		{name: "kick permission", err: errModerationPermissionDenied, errText: "unused", want: errMsgInsufficientPerms},
		{name: "ban permission", err: errModerationPermissionDenied, errText: "unused", want: errMsgInsufficientPerms},
		{name: "kick hierarchy", err: errModerationHierarchyDenied, errText: "unused", hierarchyText: "Cannot remove a member with equal or higher role position", want: "Cannot remove a member with equal or higher role position"},
		{name: "ban hierarchy", err: errModerationHierarchyDenied, errText: "unused", hierarchyText: "Cannot ban a member with equal or higher role position", want: "Cannot ban a member with equal or higher role position"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			failure, handled := h.classifyModerationTxError(
				c, tc.err, tc.errText,
				tc.hierarchyText, "log", "user-facing")

			require.True(t, handled)
			require.Nil(t, failure)
			require.Equal(t, http.StatusForbidden, rec.Code)
			require.JSONEq(t, fmt.Sprintf(`{"error":%q}`, tc.want), rec.Body.String())
		})
	}

	t.Run("pre-commit failure delegates and halts", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		failure, handled := h.classifyModerationTxError(
			c, errors.New("begin tx: connection refused"),
			"owner", "hierarchy", "log", "user-facing")

		require.True(t, handled)
		require.Nil(t, failure)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("durable delivery failure delegates and continues", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		failure, handled := h.classifyModerationTxError(
			c, fmt.Errorf("deliver: %w", presencecapture.ErrPostCommitDelivery),
			"owner", "hierarchy", "log", "user-facing")

		require.False(t, handled)
		require.NotNil(t, failure)
		require.Equal(t, http.StatusServiceUnavailable, failure.Status)
		require.Empty(t, rec.Body.String())
	})
}

// TestPreEmptiveBanOfANonMemberTakesNoGate is the #2854 stage C acceptance
// criterion for the ban path: "BanMember against a non-member target acquires
// NO gate for the target".
//
// It needs no database. With probedMember=false the capture is nil'd, so
// presencehook.WithGatedTx takes its UNWIRED arm and never reaches the double —
// which is exactly the observable: recordingCapture records a subject only when
// the gated path is entered, so an empty subjects slice IS "no gate was taken".
//
// The paired error assertion matters. An empty subjects slice alone would also
// be satisfied by a handler that failed before reaching WithGatedTx at all, so
// the test additionally pins WHICH terminal it reached: the unwired arm's
// missing-database error, proving it went through presencehook rather than
// bailing earlier.
func TestPreEmptiveBanOfANonMemberTakesNoGate(t *testing.T) {
	h, capture := newCaptureHandler(t)

	err := h.execBanTx(context.Background(), captureServerID, captureTargetID, captureActorID, nil, false)

	require.Error(t, err)
	require.NotErrorIs(t, err, errCaptureDoubleReached,
		"AC (#2854): a pre-emptive ban must NOT enter the gated path")
	require.Contains(t, err.Error(), "requires a database",
		"it reached presencehook's UNWIRED arm, which is the path a nil capture selects")

	require.Empty(t, capture.subjects,
		"AC (#2854): no gate may be acquired for a target with no audience to reconcile")
}

// TestBanFailsClosedWhenTheProbeWentStale pins the C4 fail-closed arm's
// PRESENCE. The behavioural half — that nothing is written — needs a real
// transaction and lives in the DB-backed test; what this asserts is that the
// arm is reachable at all from a nil capture, which is the precondition the
// stale case depends on.
//
// It is deliberately NOT a "gate not taken" assertion. Removing the gate leaves
// this green; only deleting the fail-closed branch or the in-transaction read
// changes it, which is the ORDERING invariant rather than the presence one.
func TestExecBanTxSelectsTheUngatedPathFromTheProbeVerdict(t *testing.T) {
	h, capture := newCaptureHandler(t)

	gatedErr := h.execBanTx(context.Background(), captureServerID, captureTargetID, captureActorID, nil, true)
	require.ErrorIs(t, gatedErr, errCaptureDoubleReached)
	require.Len(t, capture.subjects, 1, "member -> gated")

	h2, capture2 := newCaptureHandler(t)
	ungatedErr := h2.execBanTx(context.Background(), captureServerID, captureTargetID, captureActorID, nil, false)
	require.NotErrorIs(t, ungatedErr, errCaptureDoubleReached)
	require.Empty(t, capture2.subjects, "non-member -> ungated")
}
