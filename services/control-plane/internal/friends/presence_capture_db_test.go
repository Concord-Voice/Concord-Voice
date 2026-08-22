// Package friends_test drives the hooked handlers against a real database.
//
// These tests are EXTERNAL on purpose: internal/testhelpers builds the router
// and therefore imports internal/friends, so an in-package test importing it
// would cycle. The capture is wired through the exported setter instead of the
// unexported field.
package friends_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/friends"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// probingCapture is a GraphPresenceCapture double that runs a caller-supplied
// probe against the handler's own transaction at capture time. That is how these
// tests prove the capture strictly precedes the mutation write: the probe reads
// the pre-mutation row through the very transaction that is about to destroy it.
type probingCapture struct {
	mu sync.Mutex

	// db opens the transaction WithGatedTx hands to work. The handlers no
	// longer own a BeginTx of their own, so a double without one cannot run a
	// hooked route at all.
	db *sql.DB
	// probe runs inside CaptureInTx, on the handler's transaction.
	probe func(*sql.Tx) (int, error)
	// captureErr, when set, makes stage-1 capture fail.
	captureErr error
	// completeErr, when set, is returned by Complete AFTER it commits — the
	// post-commit terminal's shape, where the mutation is durable and only the
	// delivery leg failed.
	completeErr error
	// beforeWork, when set, runs after the gate is recorded and BEFORE the
	// transaction opens. That is the only window in which a hoisted
	// pre-transaction read can be invalidated, so it is how the re-assertion
	// guards are exercised.
	beforeWork func()
	// plan, when set, is handed back as the capture result.
	plan presencecapture.Plan

	// gatedSubjects records what WithGatedTx was asked to gate; subjects records
	// what CaptureInTx was asked to capture. They are kept apart on purpose:
	// nothing in the contract forces the two to agree, so a handler that builds
	// one subject for the gate and another for the capture would gate one
	// stripe while reconciling a different pair, silently.
	gatedSubjects []presencecapture.Subject
	subjects      []presencecapture.Subject
	probed        []int
	completes     int
	abandons      []string
}

// WithGatedTx mirrors the real bridge's runInTx: it opens the transaction the
// probe reads through and guarantees the discard, but NEVER commits — work's
// Complete owns that on both paths. It takes no process-local gate, which is
// the one thing this double cannot reproduce; the gate ORDER is proven in
// graphpresence's own tests, and what these tests prove is that the handler
// surrendered BeginTx and that the subject it gated is the subject it captured.
func (c *probingCapture) WithGatedTx(
	ctx context.Context, subject presencecapture.Subject, work func(*sql.Tx) error,
) error {
	c.mu.Lock()
	c.gatedSubjects = append(c.gatedSubjects, subject)
	db := c.db
	c.mu.Unlock()

	if db == nil {
		return errors.New("probingCapture: WithGatedTx requires a database")
	}
	c.mu.Lock()
	beforeWork := c.beforeWork
	c.mu.Unlock()
	if beforeWork != nil {
		beforeWork()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil &&
			!errors.Is(rollbackErr, sql.ErrTxDone) {
			panic("probingCapture: rollback failed: " + rollbackErr.Error())
		}
	}()
	return work(tx)
}

func (c *probingCapture) CaptureInTx(
	_ context.Context, tx *sql.Tx, subject presencecapture.Subject,
) (presencecapture.Plan, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.subjects = append(c.subjects, subject)
	if c.probe != nil {
		got, err := c.probe(tx)
		if err != nil {
			return nil, err
		}
		c.probed = append(c.probed, got)
	}
	if c.captureErr != nil {
		return nil, c.captureErr
	}
	return c.plan, nil
}

// Complete owns the COMMIT, exactly as the real reconciler does.
func (c *probingCapture) Complete(_ context.Context, tx *sql.Tx, _ presencecapture.Plan) error {
	c.mu.Lock()
	c.completes++
	completeErr := c.completeErr
	c.mu.Unlock()

	if err := tx.Commit(); err != nil {
		return err
	}
	// Ordered deliberately: the commit runs FIRST, so completeErr describes a
	// mutation that is already durable. Returning it before the commit would
	// make this double reproduce a pre-commit failure instead, which is the
	// opposite terminal.
	return completeErr
}

func (c *probingCapture) Abandon(_ presencecapture.Plan, cause presencecapture.Cause) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.abandons = append(c.abandons, string(cause))
}

// snapshot copies before unlocking. Returning the internal slice headers
// handed the caller aliases that a later append from a dispatch goroutine
// could write through, so the read was racy the moment it left the lock
// (PR #2738 review, CodeRabbit).
func (c *probingCapture) snapshot() ([]presencecapture.Subject, []int, int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]presencecapture.Subject(nil), c.subjects...),
		append([]int(nil), c.probed...),
		c.completes,
		append([]string(nil), c.abandons...)
}

// gated copies under the same lock, for the same aliasing reason as snapshot.
func (c *probingCapture) gated() []presencecapture.Subject {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]presencecapture.Subject(nil), c.gatedSubjects...)
}

// hookedHandler builds a friends handler wired to capture and mounts it on a
// bare engine that authenticates as actingUserID. It deliberately does NOT reuse
// ts.Router: that router owns its own handler, which the capture cannot reach.
func hookedHandler(
	t *testing.T, ts *testhelpers.TestServer, capture presencecapture.GraphPresenceCapture,
	actingUserID string, register func(*gin.Engine, *friends.Handler),
) *gin.Engine {
	t.Helper()

	h := friends.NewHandler(ts.DB, logger.New("test"), nil)
	h.SetGraphPresenceCapture(capture)
	require.True(t, h.HasGraphPresenceCapture())

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("user_id", actingUserID)
		c.Next()
	})
	register(engine, h)
	return engine
}

func acceptedFriendshipCount(tx *sql.Tx, userID, targetID string) (int, error) {
	var n int
	err := tx.QueryRow(`
		SELECT COUNT(*) FROM friendships
		WHERE ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
		  AND status = 'accepted'
	`, userID, targetID).Scan(&n)
	return n, err
}

func countFriendships(t *testing.T, ts *testhelpers.TestServer, userID, targetID, status string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM friendships
		WHERE ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
		  AND status = $3
	`, userID, targetID, status).Scan(&n))
	return n
}

// The load-bearing ordering claim: the capture reads the friendship the DELETE
// is about to destroy, through the same transaction, BEFORE the delete runs.
func TestRemoveFriendCapturesBeforeTheDelete(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remover")
	user2 := ts.CreateTestUser(t, "removed")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	capture := &probingCapture{db: ts.DB, probe: func(tx *sql.Tx) (int, error) {
		return acceptedFriendshipCount(tx, user1.ID, user2.ID)
	}}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.DELETE("/friends/:user_id", h.RemoveFriend)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/friends/"+user2.ID, nil))
	require.Equal(t, http.StatusOK, w.Code)

	subjects, probed, completes, abandons := capture.snapshot()
	require.Len(t, subjects, 1)
	assert.Equal(t, presencecapture.FamilyFriendshipRemove, subjects[0].Family)
	assert.Equal(t, presencecapture.FailClosedBlockWrite, subjects[0].FailPosture,
		"removal must block the write on capture failure — only block degrades")
	assert.Equal(t, user1.ID, subjects[0].Principal.String())
	assert.Equal(t, user2.ID, subjects[0].Counterpart.String())

	require.Equal(t, []int{1}, probed,
		"the capture must still see the friendship: the write destroys what it reads")
	assert.Equal(t, 1, completes, "Complete owns the commit")
	assert.Empty(t, abandons)
	assert.Equal(t, 0, countFriendships(t, ts, user1.ID, user2.ID, statusAccepted))
}

// Fail-closed: a stage-1 capture failure rolls the transaction back, so the
// friendship survives and nothing was disclosed.
func TestRemoveFriendCaptureFailureBlocksTheWrite(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remover_fc")
	user2 := ts.CreateTestUser(t, "removed_fc")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	capture := &probingCapture{db: ts.DB, captureErr: errors.New("capture boom")}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.DELETE("/friends/:user_id", h.RemoveFriend)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/friends/"+user2.ID, nil))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	_, _, completes, _ := capture.snapshot()
	assert.Equal(t, 0, completes)
	assert.Equal(t, 1, countFriendships(t, ts, user1.ID, user2.ID, statusAccepted),
		"a fail-closed capture must leave the friend graph untouched")
}

// degradedPlan stands in for the conservative superset the bridge substitutes
// when a stage-1 read fails under FailConservativeDegrade.
type degradedPlan struct{}

func (degradedPlan) HasWork() bool  { return true }
func (degradedPlan) Degraded() bool { return true }

// The block site captures the prior friendship state through its own
// transaction, before the status write replaces it. A 2xx here also proves
// executeBlockTx no longer commits: a handler-owned commit would leave Complete
// with sql.ErrTxDone and a 500.
func TestBlockUserCapturesBeforeTheFriendshipWrite(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker_cap")
	user2 := ts.CreateTestUser(t, "blocked_cap")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	capture := &probingCapture{db: ts.DB, probe: func(tx *sql.Tx) (int, error) {
		return acceptedFriendshipCount(tx, user1.ID, user2.ID)
	}}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/:user_id/block", h.BlockUser)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/"+user2.ID+"/block", nil))
	require.Equal(t, http.StatusOK, w.Code)

	subjects, probed, completes, abandons := capture.snapshot()
	require.Len(t, subjects, 1)
	assert.Equal(t, presencecapture.FamilyBlock, subjects[0].Family)
	assert.Equal(t, presencecapture.FailConservativeDegrade, subjects[0].FailPosture)
	require.Equal(t, []int{1}, probed, "the capture must still see the pre-block friendship")
	assert.Equal(t, 1, completes)
	assert.Empty(t, abandons)
	assert.Equal(t, 1, countFriendships(t, ts, user1.ID, user2.ID, statusBlocked))
}

// No prior friendship row exists, so the write is an INSERT.
//
// UPDATED BY #2854 STAGE C. This test used to assert "the capture is still
// unconditional — the delta simply comes out empty. No status branch." Stage C
// deliberately introduced that branch: with no accepted edge the capture is
// nil'd, because CaptureInTx's own accepted-edge gate would have produced an
// empty plan anyway and entering the gated path to compute it holds one of 64
// shared stripes on a user who is not party to the request.
//
// What this test uniquely still proves, and no other test does: the UNGATED
// path still COMMITS. presencehook.Complete with a nil capture and a nil plan
// takes the bare-commit arm, so the block lands even though nothing was
// captured. That is the property a naive "skip the transaction" fix would break.
func TestBlockUserCapturesWithNoPriorFriendship(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker_new")
	user2 := ts.CreateTestUser(t, "blocked_new")

	capture := &probingCapture{db: ts.DB}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/:user_id/block", h.BlockUser)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/"+user2.ID+"/block", nil))
	require.Equal(t, http.StatusOK, w.Code)

	subjects, _, completes, _ := capture.snapshot()
	require.Empty(t, subjects,
		"#2854 stage C: with no accepted edge the capture is nil'd, so none runs")
	require.Empty(t, capture.gated(),
		"and no stripe is held on a user with no relationship to the actor")
	assert.Equal(t, 0, completes,
		"Complete never reaches the double either — the unwired arm commits directly")
	assert.Equal(t, 1, countFriendships(t, ts, user1.ID, user2.ID, statusBlocked),
		"the ungated path MUST still commit the block")
}

// Degraded() is a counter, never a business-logic branch: the block still
// returns its normal 2xx and the write still lands.
func TestBlockUserSucceedsWithADegradedPlan(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker_deg")
	user2 := ts.CreateTestUser(t, "blocked_deg")

	capture := &probingCapture{db: ts.DB, plan: degradedPlan{}}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/:user_id/block", h.BlockUser)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/"+user2.ID+"/block", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, countFriendships(t, ts, user1.ID, user2.ID, statusBlocked))
}

// A stage-2 failure — the error the bridge returns rather than a degraded plan —
// still blocks the write regardless of the declared posture.
//
// UPDATED BY #2854 STAGE C: the two users are now ACCEPTED FRIENDS. Without an
// accepted edge the capture is nil'd and captureErr can never fire, so this
// test would pass while exercising nothing — the same vacuity that hid in
// TestAbortedRemoveFriendDisconnectsNobody. A real edge keeps the capture on
// the path this test exists to fail.
func TestBlockUserCaptureErrorBlocksTheWrite(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker_err")
	user2 := ts.CreateTestUser(t, "blocked_err")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	capture := &probingCapture{db: ts.DB, captureErr: errors.New("capture boom")}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/:user_id/block", h.BlockUser)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/"+user2.ID+"/block", nil))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	_, _, completes, _ := capture.snapshot()
	assert.Equal(t, 0, completes)
	assert.Equal(t, 0, countFriendships(t, ts, user1.ID, user2.ID, statusBlocked))
}

// SECURITY REGRESSION (#2738 review). A no-op delete must NOT abandon.
//
// This test previously asserted the opposite — that a no-op removal abandons —
// and in doing so locked in a live vulnerability. `Abandon` disconnects the
// captured audience, whose viewer set always contains BOTH principals, and
// `Hub.DisconnectRichPresenceClients` closes every local DEVICE for those
// users. So any authenticated caller could `DELETE /friends/<stranger>` and
// force a full websocket teardown of that stranger's sessions, repeatably,
// having mutated nothing — a cheap DoS amplification vector against arbitrary
// users.
//
// `rowsAffected == 0` is positive proof that nothing was written, so there is
// no stale audience and nobody to disconnect. Contrast the driver-error branch,
// which DOES abandon because the write's outcome is genuinely unknown, and
// unknown state must fail closed. Proven no-change must not.
func TestRemoveFriendDoesNotDisconnectWhenNoFriendshipExists(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remover_none")
	user2 := ts.CreateTestUser(t, "removed_none")

	capture := &probingCapture{db: ts.DB}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.DELETE("/friends/:user_id", h.RemoveFriend)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/friends/"+user2.ID, nil))

	assert.Equal(t, http.StatusNotFound, w.Code)
	_, _, completes, abandons := capture.snapshot()
	assert.Equal(t, 0, completes, "nothing was written, so nothing may commit")
	assert.Empty(t, abandons,
		"a non-mutating RemoveFriend must disconnect nobody: Abandon would tear down "+
			"every device of both principals, letting any caller DoS a stranger")
}

// The gate and the capture are two separately-supplied subjects, and nothing in
// the GraphPresenceCapture contract forces them to agree — a handler that gated
// one pair and captured another would take the wrong stripe and write a durable
// marker for a sender it never gated, with no compile error and no failing
// assertion anywhere else. These tests pin the agreement per site.
func TestHookedSitesGateTheSubjectTheyCapture(t *testing.T) {
	t.Run("remove", func(t *testing.T) {
		ts := setupTS(t)
		user1 := ts.CreateTestUser(t, "gate_rm_a")
		user2 := ts.CreateTestUser(t, "gate_rm_b")
		ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

		capture := &probingCapture{db: ts.DB}
		engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
			e.DELETE("/friends/:user_id", h.RemoveFriend)
		})

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/friends/"+user2.ID, nil))
		require.Equal(t, http.StatusOK, w.Code)

		gated := capture.gated()
		subjects, _, _, _ := capture.snapshot()
		require.Len(t, gated, 1, "the handler must surrender BeginTx to WithGatedTx")
		require.Len(t, subjects, 1)
		assert.Equal(t, subjects[0], gated[0],
			"the gated subject and the captured subject must be the same value")
		// One spec VARIABLE feeds both calls here, so the assertion above cannot
		// fail — but it is satisfied just as well by a variable built with the
		// pair swapped. Orientation is the separate property.
		assert.Equal(t, presencecapture.FamilyFriendshipRemove, gated[0].Family)
		assert.Equal(t, user1.ID, gated[0].Principal.String())
		assert.Equal(t, user2.ID, gated[0].Counterpart.String())
	})

	t.Run("accept", func(t *testing.T) {
		ts := setupTS(t)
		requester := ts.CreateTestUser(t, "gate_ac_a")
		addressee := ts.CreateTestUser(t, "gate_ac_b")
		requestID := createPendingRequest(t, ts, requester.ID, addressee.ID)

		capture := &probingCapture{db: ts.DB}
		engine := hookedHandler(t, ts, capture, addressee.ID, func(e *gin.Engine, h *friends.Handler) {
			e.PATCH("/friends/request/:id", h.RespondRequest)
		})

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/friends/request/"+requestID,
			strings.NewReader(`{"action":"accept"}`))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		gated := capture.gated()
		subjects, _, completes, abandons := capture.snapshot()
		require.Len(t, gated, 1)
		require.Len(t, subjects, 1)
		assert.Equal(t, subjects[0], gated[0])
		assert.Equal(t, presencecapture.FamilyFriendshipAccept, gated[0].Family)
		assert.Equal(t, addressee.ID, gated[0].Principal.String())
		assert.Equal(t, requester.ID, gated[0].Counterpart.String())
		assert.Equal(t, 1, completes)
		assert.Empty(t, abandons)
		assert.Equal(t, 1, countFriendships(t, ts, requester.ID, addressee.ID, statusAccepted))
	})

	// The four sites do NOT offer the same guarantee, and they rank in this
	// order. acceptFriendRequest and RemoveFriend each hand a single spec
	// VARIABLE to both WithGatedTx and Capture, so their two subjects are the
	// same value by construction and nothing can drift. Block builds its two
	// from two calls to the SAME constructor — BlockUser calls
	// blockCaptureSpec(userID, targetUserID) for the gate and executeBlockTx
	// calls it again for its own Capture — so Family and FailPosture are fixed
	// in the constructor body and only the argument lists can fall out of step.
	// The WEAKEST is the claim site, not block: ClaimFriendCode and
	// executeFriendCodeClaim each hand-write an INDEPENDENT presencehook.Spec
	// literal, so Family, Posture and the ID pairing can each drift on their
	// own.
	//
	// Hence orientation, not just equality, at every site: swapping the blocker
	// and the blocked in either of block's argument lists compiles, gates the
	// wrong stripe and still returns 200.
	t.Run("block", func(t *testing.T) {
		ts := setupTS(t)
		blocker := ts.CreateTestUser(t, "gate_bl_a")
		blocked := ts.CreateTestUser(t, "gate_bl_b")
		ts.CreateFriendship(t, blocker.ID, blocked.ID, statusAccepted)

		capture := &probingCapture{db: ts.DB}
		engine := hookedHandler(t, ts, capture, blocker.ID, func(e *gin.Engine, h *friends.Handler) {
			e.POST("/friends/:user_id/block", h.BlockUser)
		})

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/"+blocked.ID+"/block", nil))
		require.Equal(t, http.StatusOK, w.Code)

		gated := capture.gated()
		subjects, _, completes, abandons := capture.snapshot()
		require.Len(t, gated, 1, "the handler must surrender BeginTx to WithGatedTx")
		require.Len(t, subjects, 1)
		assert.Equal(t, subjects[0], gated[0],
			"the gated subject and the captured subject must be the same value")
		// Orientation, not just equality: two calls that agreed on a SWAPPED
		// pair would satisfy the assertion above and still reconcile the wrong
		// direction of a block.
		assert.Equal(t, presencecapture.FamilyBlock, gated[0].Family)
		assert.Equal(t, presencecapture.FailConservativeDegrade, gated[0].FailPosture)
		assert.Equal(t, blocker.ID, gated[0].Principal.String())
		assert.Equal(t, blocked.ID, gated[0].Counterpart.String())
		assert.Equal(t, 1, completes)
		assert.Empty(t, abandons)
		assert.Equal(t, 1, countFriendships(t, ts, blocker.ID, blocked.ID, statusBlocked))
	})

	// The claim site is the reason the code owner is read BEFORE the
	// transaction: the gate needs the counterpart, and the counterpart is the
	// owner. Without the hoist the gate would be taken with no counterpart at
	// all while the capture inside still named one — the exact silent
	// divergence this test exists to catch.
	//
	// This is the two-independent-literals site named above. The full-struct
	// equality below catches any drift BETWEEN the gate literal and the capture
	// literal, which is the hazard that matters here; what it cannot catch is
	// both moving together, so TestExecuteFriendCodeClaimCarriesTheConsumedCodeID
	// (friend_code_claim_internal_test.go) pins the capture literal's family,
	// posture and orientation absolutely.
	t.Run("auto-accepting friend-code claim", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "gate_fc_owner")
		claimer := ts.CreateTestUser(t, "gate_fc_claim")
		createFriendCode(t, ts, owner.ID, "GATEAUTO", nil, nil, true)

		capture := &probingCapture{db: ts.DB}
		engine := hookedHandler(t, ts, capture, claimer.ID, func(e *gin.Engine, h *friends.Handler) {
			e.POST("/friends/codes/:code/claim", h.ClaimFriendCode)
		})

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/codes/GATEAUTO/claim", nil))
		require.Equal(t, http.StatusOK, w.Code)

		gated := capture.gated()
		subjects, _, completes, abandons := capture.snapshot()
		require.Len(t, gated, 1)
		require.Len(t, subjects, 1)
		assert.Equal(t, subjects[0], gated[0])
		assert.Equal(t, owner.ID, gated[0].Counterpart.String(),
			"the gate must name the code OWNER, which is only knowable from the hoisted read")
		assert.Equal(t, claimer.ID, gated[0].Principal.String())
		assert.Equal(t, 1, completes)
		assert.Empty(t, abandons)
		assert.Equal(t, 1, countFriendships(t, ts, claimer.ID, owner.ID, statusAccepted))
	})
}

// A non-auto-accepting claim writes a 'pending' row, which confers no
// friend-of-friends visibility. It must therefore take NO gate and run NO
// capture even on a fully wired handler — and still commit, because the unwired
// terminal owns the commit on that path too. A closure that returned nil early
// here would 200 a transaction nobody committed.
func TestPendingFriendCodeClaimTakesNoGateAndStillCommits(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "gate_fc_p_owner")
	claimer := ts.CreateTestUser(t, "gate_fc_p_claim")
	createFriendCode(t, ts, owner.ID, "GATEPEND", nil, nil, false)

	capture := &probingCapture{db: ts.DB}
	engine := hookedHandler(t, ts, capture, claimer.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/codes/:code/claim", h.ClaimFriendCode)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/codes/GATEPEND/claim", nil))
	require.Equal(t, http.StatusOK, w.Code)

	subjects, _, completes, abandons := capture.snapshot()
	assert.Empty(t, capture.gated(), "a pending claim must take no sender gate")
	assert.Empty(t, subjects, "a pending claim must run no capture")
	assert.Equal(t, 0, completes, "the unwired terminal commits, so the double sees nothing")
	assert.Empty(t, abandons)
	assert.Equal(t, 1, countFriendships(t, ts, claimer.ID, owner.ID, statusPending),
		"the claim must still be durable")
}

// #2446 §3.6's whole point: a post-commit delivery failure is 503, not 500.
// Reporting "Failed to remove friend" with a 500 for a friendship that WAS
// removed is what drives duplicate-action retries, and every one of these four
// sites did exactly that before this task. Both directions are asserted — the
// status that must appear and the status that must not — plus the durability
// that makes the 500 wrong in the first place.
func TestRemoveFriendPostCommitDeliveryFailureIs503(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "pcd_remover")
	user2 := ts.CreateTestUser(t, "pcd_removed")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	capture := &probingCapture{
		db:          ts.DB,
		completeErr: fmt.Errorf("dispatch: %w", presencecapture.ErrPostCommitDelivery),
	}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.DELETE("/friends/:user_id", h.RemoveFriend)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/friends/"+user2.ID, nil))

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotEqual(t, http.StatusInternalServerError, w.Code,
		"the removal committed: a 500 here tells the client to retry a completed action")
	assert.Empty(t, w.Header().Get("Retry-After"),
		"delivery failure is post-commit and does not self-resolve")
	assert.Equal(t, 0, countFriendships(t, ts, user1.ID, user2.ID, statusAccepted),
		"the 503 must accompany a mutation that really is durable")

	// The status was only half the fix. This test asserted status and
	// Retry-After and nothing else, so it passed while the BODY still read
	// "Failed to remove friend" for a friendship that was removed -- the exact
	// duplicate-retry lie the sentinel exists to prevent, moved from the status
	// line into the body (Gitar, PR #2823). The friendship count above is what
	// makes this assertion meaningful: the removal provably landed.
	// Literal, not the unexported constant: this is an external test package,
	// and asserting against the same constant the handler reads would compare
	// the wire string to itself anyway.
	assert.NotContains(t, w.Body.String(), "Failed to remove friend",
		"a removal that COMMITTED must never be reported to the client as failed")
	assert.Contains(t, w.Body.String(), "saved",
		"the body must say the change landed, not that it failed")
}

// The mirror of the case above, and the reason the split is not just cosmetic:
// an ordinary terminal error proves nothing landed, so it stays a 500 and the
// friendship survives. A responder that mapped every terminal to 503 would pass
// the test above and fail this one.
func TestRemoveFriendOpaqueTerminalStays500(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "opq_remover")
	user2 := ts.CreateTestUser(t, "opq_removed")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	capture := &probingCapture{db: ts.DB, captureErr: errors.New("capture boom")}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.DELETE("/friends/:user_id", h.RemoveFriend)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/friends/"+user2.ID, nil))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotEqual(t, http.StatusServiceUnavailable, w.Code)
	assert.Empty(t, w.Header().Get("Retry-After"))
	assert.Equal(t, 1, countFriendships(t, ts, user1.ID, user2.ID, statusAccepted))
}

// ClaimFriendCode reads the code owner OUTSIDE the transaction so the sender
// gates can be acquired before BeginTx, then re-asserts it against the
// FOR UPDATE row. If the row changed in that window the gates and the capture
// cover the wrong sender, so the claim must fail CLOSED rather than write under
// them — a 500 with no friendship row, not a silently mis-gated success.
func TestFriendCodeClaimFailsClosedWhenTheHoistedReadGoesStale(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "stale_fc_owner")
	claimer := ts.CreateTestUser(t, "stale_fc_claim")
	createFriendCode(t, ts, owner.ID, "STALEFCD", nil, nil, true)

	capture := &probingCapture{db: ts.DB}
	capture.beforeWork = func() {
		// Between the preview and the locked read the code stops
		// auto-accepting, so the gate this request already took no longer
		// matches what the locked row describes.
		_, err := ts.DB.Exec(`UPDATE friend_codes SET auto_accept = FALSE WHERE code = $1`, "STALEFCD")
		require.NoError(t, err)
	}
	engine := hookedHandler(t, ts, capture, claimer.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/codes/:code/claim", h.ClaimFriendCode)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/codes/STALEFCD/claim", nil))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	subjects, _, completes, abandons := capture.snapshot()
	require.Len(t, capture.gated(), 1, "the gate was taken on the stale preview")
	assert.Empty(t, subjects, "the mismatch must be caught before any capture runs")
	assert.Equal(t, 0, completes, "nothing may commit under gates taken for the wrong shape")
	assert.Empty(t, abandons, "nothing was written, so nobody may be disconnected")
	assert.Equal(t, 0, countFriendships(t, ts, claimer.ID, owner.ID, statusAccepted))
	assert.Equal(t, 0, countFriendships(t, ts, claimer.ID, owner.ID, statusPending))
}

// recordingBroadcaster captures what a handler put on the wire.
//
// It is mutex-guarded: BroadcastToUser runs on the handler's goroutine while the
// assertions read from the test goroutine.
type recordingBroadcaster struct {
	mu       sync.Mutex
	messages []broadcastRecord
}

type broadcastRecord struct {
	userID uuid.UUID
	msg    websocket.OutgoingMessage
}

func (b *recordingBroadcaster) BroadcastToUser(userID uuid.UUID, msg websocket.OutgoingMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages = append(b.messages, broadcastRecord{userID: userID, msg: msg})
}

func (b *recordingBroadcaster) firstOfType(t *testing.T, msgType string) broadcastRecord {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.messages {
		if m.msg.Type == msgType {
			return m
		}
	}
	require.FailNowf(t, "no broadcast of that type", "wanted %q, got %d message(s)",
		msgType, len(b.messages))
	return broadcastRecord{}
}

// The user-visible tail of the friend-code claim, and the reason the hub is an
// interface at all.
//
// claimResult.codeID is pinned by an internal test, but nothing asserted it
// survives the hop into the broadcast: mutating the claimNotification literal to
// codeID: "" left the ENTIRE internal/friends package green, because every other
// test here builds the handler with a nil hub and no test named
// friend_code_claimed at all.
//
// The consequence is three layers from the mutated line and invisible on the
// server. An empty string is a perfectly good Go string, so nothing fails here;
// it fails at the desktop's dispatch boundary, where code_id is UUID.optional()
// (client/desktop/src/renderer/types/ws-events.ts) and "" is not a UUID. Zod
// rejects the whole event, so the code OWNER -- who is not the caller, and gets
// no error of their own -- silently never learns their code was redeemed.
func TestFriendCodeClaimBroadcastCarriesTheCodeID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "bcast_owner")
	claimer := ts.CreateTestUser(t, "bcast_claimer")
	createFriendCode(t, ts, owner.ID, "BCASTID1", nil, nil, true)

	broadcaster := &recordingBroadcaster{}
	h := friends.NewHandler(ts.DB, logger.New("test"), broadcaster)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set("user_id", claimer.ID); c.Next() })
	engine.POST("/friends/codes/:code/claim", h.ClaimFriendCode)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(
		http.MethodPost, "/friends/codes/BCASTID1/claim", nil))
	require.Equal(t, http.StatusOK, w.Code)

	claimed := broadcaster.firstOfType(t, "friend_code_claimed")
	assert.Equal(t, owner.ID, claimed.userID.String(),
		"the claim is announced to the code OWNER, who has no other way to learn of it")

	codeID, present := claimed.msg.Data["code_id"]
	require.True(t, present, "code_id must be on the wire")
	require.IsType(t, "", codeID, "code_id must be a string")
	assert.NotEmpty(t, codeID, `an empty code_id fails the desktop's UUID.optional() `+
		`schema and the whole event is dropped at the dispatch boundary`)

	// Pin it to the row actually consumed, not merely to non-emptiness: a
	// hard-coded or stale id would satisfy NotEmpty and still be wrong.
	var consumed string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM friend_codes WHERE code = $1`, "BCASTID1").Scan(&consumed))
	assert.Equal(t, consumed, codeID, "and it must be the id of the code that was claimed")
}

// The typed-nil guard in NewHandler. A nil *websocket.Hub boxed into
// UserBroadcaster is NOT nil, so without normalization every `h.hub == nil`
// guard passes and BroadcastToUser panics dereferencing its receiver.
func TestNewHandlerNormalizesATypedNilHub(t *testing.T) {
	ts := setupTS(t)
	var nilHub *websocket.Hub

	h := friends.NewHandler(ts.DB, logger.New("test"), nilHub)

	user := ts.CreateTestUser(t, "typednil_a")
	other := ts.CreateTestUser(t, "typednil_b")
	createFriendCode(t, ts, other.ID, "TYPEDNIL", nil, nil, true)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set("user_id", user.ID); c.Next() })
	engine.POST("/friends/codes/:code/claim", h.ClaimFriendCode)

	w := httptest.NewRecorder()
	require.NotPanics(t, func() {
		engine.ServeHTTP(w, httptest.NewRequest(
			http.MethodPost, "/friends/codes/TYPEDNIL/claim", nil))
	}, "a typed-nil hub must be treated as absent, not broadcast through")
	assert.Equal(t, http.StatusOK, w.Code, "and the claim itself still succeeds")
}
