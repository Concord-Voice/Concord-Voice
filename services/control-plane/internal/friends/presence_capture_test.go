package friends

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// recordingCapture is a GraphPresenceCapture double that records what the
// handlers hand it. It never touches a real transaction, so it exercises the
// handler-side wiring without a database.
type recordingCapture struct {
	subjects []presencecapture.Subject
	abandons []string
}

// WithGatedTx satisfies the interface without taking a gate or opening a
// transaction, and returns a sentinel rather than nil.
//
// Two claims an earlier version of this comment made were falsified by later
// work on this same branch: topology_test is no longer the tree's only caller
// (graphpresence's integration_test, presencehook's own suite and the friends
// disconnect regressions all reach it now), and the hooked handlers no longer
// open their own transactions -- all five enter through
// presencehook.WithGatedTx and none calls db.BeginTx.
//
// What survives is the reason for the sentinel. NO test installs this double on
// a path that routes through WithGatedTx, and a double that swallowed work and
// reported success would let such a test pass with its mutation never run. The
// sentinel makes any future test that does route here fail loudly instead.
func (*recordingCapture) WithGatedTx(
	context.Context, presencecapture.Subject, func(*sql.Tx) error,
) error {
	return errors.New("recordingCapture: WithGatedTx is not implemented by this double")
}

func (c *recordingCapture) CaptureInTx(
	_ context.Context, _ *sql.Tx, subject presencecapture.Subject,
) (presencecapture.Plan, error) {
	c.subjects = append(c.subjects, subject)
	return nil, nil
}

func (c *recordingCapture) Complete(_ context.Context, tx *sql.Tx, _ presencecapture.Plan) error {
	return tx.Commit()
}

func (c *recordingCapture) Abandon(_ presencecapture.Plan, cause presencecapture.Cause) {
	c.abandons = append(c.abandons, string(cause))
}

func TestHasGraphPresenceCaptureReportsWiring(t *testing.T) {
	h := &Handler{}
	assert.False(t, h.HasGraphPresenceCapture(), "an unwired handler must report false")

	h.SetGraphPresenceCapture(&recordingCapture{})
	assert.True(t, h.HasGraphPresenceCapture(), "a wired handler must report true")
}

// The accessor must reflect handler state, not the caller's local value — that
// distinction is what makes the router boot guard non-tautological.
func TestHasGraphPresenceCaptureIsHandlerState(t *testing.T) {
	h := &Handler{}
	capture := &recordingCapture{}
	_ = capture // constructed but never wired

	assert.False(t, h.HasGraphPresenceCapture(),
		"constructing a capture must not make the handler report wired")
}

// The handler's capture goes through the shared plumbing, so wiring the setter
// is enough to make a spec reach the bridge as a parsed subject.
func TestWiredHandlerCaptureReachesBridge(t *testing.T) {
	capture := &recordingCapture{}
	h := &Handler{}
	h.SetGraphPresenceCapture(capture)
	principal, counterpart := uuid.New(), uuid.New()

	_, err := presencehook.Capture(context.Background(), h.graphPresence, nil, presencehook.Spec{
		Family:        presencecapture.FamilyFriendshipRemove,
		Posture:       presencecapture.FailClosedBlockWrite,
		PrincipalID:   principal.String(),
		CounterpartID: counterpart.String(),
	})

	require.NoError(t, err)
	require.Len(t, capture.subjects, 1)
	assert.Equal(t, presencecapture.FamilyFriendshipRemove, capture.subjects[0].Family)
	assert.Equal(t, principal, capture.subjects[0].Principal)
	assert.Equal(t, counterpart, capture.subjects[0].Counterpart)
}

// Block is the one #2446 site that degrades rather than blocking the write:
// refusing a block because a capture read failed would let a large friend graph
// deny a safety affordance.
func TestBlockCaptureSpecDeclaresConservativeDegrade(t *testing.T) {
	blocker, blocked := uuid.New(), uuid.New()

	spec := blockCaptureSpec(blocker.String(), blocked.String())

	assert.Equal(t, presencecapture.FamilyBlock, spec.Family)
	assert.Equal(t, presencecapture.FailConservativeDegrade, spec.Posture,
		"block must declare FailConservativeDegrade: a large friend graph must not "+
			"be able to deny a safety affordance")

	subject, err := spec.Subject()
	require.NoError(t, err)
	assert.Equal(t, blocker, subject.Principal)
	assert.Equal(t, blocked, subject.Counterpart)
}

// Every other site keeps the fail-closed zero value.
func TestNonBlockSitesFailClosed(t *testing.T) {
	for _, family := range []presencecapture.Family{
		presencecapture.FamilyFriendshipAccept,
		presencecapture.FamilyFriendshipRemove,
		presencecapture.FamilyFriendsOfFriendsToggle,
	} {
		spec := presencehook.Spec{Family: family, PrincipalID: uuid.NewString()}
		subject, err := spec.Subject()
		require.NoError(t, err)
		assert.Equal(t, presencecapture.FailClosedBlockWrite, subject.FailPosture)
	}
}

// respondPresenceTerminal is the ONE place a hooked handler turns a terminal
// error into an HTTP response, so this is where the #2446 §3.6 shape is pinned.
//
// Each subtest asserts BOTH directions of the guard it covers: the status the
// class must produce AND the status it must never produce. A responder that
// collapsed every terminal to 500 — which is what every one of these four sites
// did before this task — passes a one-directional "is it 503?" assertion for
// exactly zero of the cases and a "is it not 200?" assertion for all four.
func TestRespondPresenceTerminalMapsEachClass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{log: logger.New("test")}

	t.Run("post-commit delivery failure is 503 and never 500", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)

		handler.respondPresenceTerminal(c, errMsgFailedRemoveFriend,
			fmt.Errorf("terminal: %w", presencecapture.ErrPostCommitDelivery))

		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		assert.NotEqual(t, http.StatusInternalServerError, recorder.Code,
			"the friendship WAS removed: a 500 here drives duplicate-action retries")
		// Retry-After promises self-resolution. Delivery failure is post-commit
		// and does not resolve itself, so the header must be absent.
		assert.Empty(t, recorder.Header().Get("Retry-After"))
		// The body must NOT be the site's failure message. This arm committed, so
		// "Failed to X" would invite the duplicate retry the 503 exists to prevent
		// -- the lie moved from the status line into the body. An earlier version of
		// this test asserted that lie and so could not catch it (Gitar, PR #2823).
		// Asserted against a LITERAL: comparing to the same constant the handler
		// reads would compare the wire string to itself.
		assert.JSONEq(t,
			`{"error":"Your change was saved. Updating everyone who can see it is taking longer than usual."}`,
			recorder.Body.String())
		assert.NotContains(t, recorder.Body.String(), errMsgFailedRemoveFriend,
			"a committed mutation must never be reported to the client as failed")
	})

	t.Run("pending marker is 503 with a positive Retry-After", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)

		handler.respondPresenceTerminal(c, errMsgFailedAcceptRequest,
			&presencecapture.PendingError{After: 1500 * time.Millisecond})

		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		assert.NotEqual(t, http.StatusInternalServerError, recorder.Code)
		// Rounded UP: 1 would send the client back into a marker still held.
		assert.Equal(t, "2", recorder.Header().Get("Retry-After"))
	})

	t.Run("an eligible marker still yields a positive Retry-After", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)

		handler.respondPresenceTerminal(c, errMsgFailedBlockUser,
			&presencecapture.PendingError{After: 5 * time.Second})

		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		assert.Equal(t, "5", recorder.Header().Get("Retry-After"))
	})

	t.Run("anything else is 500", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)

		handler.respondPresenceTerminal(c, errMsgFailedClaimFriendCode,
			errors.New("connection reset"))

		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.NotEqual(t, http.StatusServiceUnavailable, recorder.Code)
		// The mirror of the delivery case: a 500 proves nothing landed and does
		// not self-resolve, so promising a retry cadence here is the inverse of
		// the behaviour the 503/500 split exists to produce.
		assert.Empty(t, recorder.Header().Get("Retry-After"))
		assert.JSONEq(t, `{"error":"`+errMsgFailedClaimFriendCode+`"}`, recorder.Body.String())
	})

	// presencecapture.ErrCaptureBound has no Classify arm and lands here on
	// purpose. Its only producer is graphpresence.checkFocalBound
	// (len(focal) > maxFocalSenders), which returns before any gate,
	// transaction or savepoint is taken and therefore blocks the write
	// regardless of the declared posture. An oversized focal set is a defect in
	// the focal-set derivation, not a retryable condition, so 500 with no
	// Retry-After is the intended shape rather than an unmapped gap.
	t.Run("a capture-bound refusal is 500 with no retry promise", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)

		handler.respondPresenceTerminal(c, errMsgFailedBlockUser,
			fmt.Errorf("capture: %w", presencecapture.ErrCaptureBound))

		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Empty(t, recorder.Header().Get("Retry-After"))
	})
}
