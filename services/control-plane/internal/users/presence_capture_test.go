package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// stubCapture records the subjects it was handed. Complete owns the commit on
// the wired path, exactly as the real reconciler does.
type stubCapture struct {
	// db opens the transaction WithGatedTx hands to work. UpdatePrivacySettings
	// no longer owns a BeginTx of its own, so a double without one cannot run
	// the hooked route at all — which is exactly what the sentinel below used
	// to assert, and what this field now replaces.
	db *sql.DB

	// gatedSubjects records what WithGatedTx was asked to gate; subjects
	// records what CaptureInTx was asked to capture. They are kept apart on
	// purpose: nothing in the contract forces the two to agree, so a handler
	// that built one subject for the gate and another for the capture would
	// gate one stripe while reconciling a different pair, silently.
	gatedSubjects []presencecapture.Subject
	subjects      []presencecapture.Subject
	abandoned     []string

	// poisonTx aborts the transaction from inside CaptureInTx so the write
	// after it fails deterministically. See the comment on CaptureInTx.
	poisonTx bool
}

// WithGatedTx mirrors the real bridge's runInTx: it opens the transaction work
// runs on and guarantees the discard, but NEVER commits — work's Complete owns
// that on both paths. It takes no process-local gate, which is the one thing
// this double cannot reproduce; the gate ORDER is proven in graphpresence's own
// tests, and what these tests prove is that the handler surrendered BeginTx and
// that the subject it gated is the subject it captured.
func (c *stubCapture) WithGatedTx(
	ctx context.Context, subject presencecapture.Subject, work func(*sql.Tx) error,
) (err error) {
	c.gatedSubjects = append(c.gatedSubjects, subject)
	if c.db == nil {
		return errors.New("stubCapture: WithGatedTx requires a database")
	}
	tx, beginErr := c.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return fmt.Errorf("begin gated graph mutation: %w", beginErr)
	}
	// Fail closed by JOINING a failed discard, exactly as runInTx does, rather
	// than panicking: TestFirstWriteFoFRaceStillCapturesTheNarrowing drives the
	// handler on its own goroutine, where a panic kills the package binary with
	// no attribution to the case that caused it.
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
			return
		}
		err = errors.Join(err, fmt.Errorf("discard gated graph mutation: %w", rollbackErr))
	}()
	return work(tx)
}

func (c *stubCapture) CaptureInTx(
	ctx context.Context, tx *sql.Tx, s presencecapture.Subject,
) (presencecapture.Plan, error) {
	c.subjects = append(c.subjects, s)
	// poisonTx aborts the transaction without returning an error, so the
	// capture SUCCEEDS and the write that follows it fails — the only ordering
	// that reaches the Abandon terminal. Returning an error here would take the
	// capture-failed path instead and never touch it.
	if c.poisonTx {
		_, _ = tx.ExecContext(ctx, `SELECT 1/0`)
	}
	return nil, nil
}

func (c *stubCapture) Complete(_ context.Context, tx *sql.Tx, _ presencecapture.Plan) error {
	return tx.Commit()
}

func (c *stubCapture) Abandon(_ presencecapture.Plan, cause presencecapture.Cause) {
	c.abandoned = append(c.abandoned, string(cause))
}

func TestUsersHandlerReportsCaptureWiring(t *testing.T) {
	h := &Handler{}
	require.False(t, h.HasGraphPresenceCapture(), "unwired handler must report false")

	h.SetGraphPresenceCapture(&stubCapture{})
	require.True(t, h.HasGraphPresenceCapture(), "wired handler must report true")
}

// This exercises the SHARED plumbing, not the handler: it calls
// presencehook.Capture directly, because there is no capturePresence wrapper on
// Handler to drive. The handler-level guarantee is covered by
// TestUpdatePrivacySettingsCapturesOnlyOnActualFoFChange below (PR #2738
// review, CodeRabbit).
func TestUsersWiredCaptureCarriesNoCounterpart(t *testing.T) {
	capture := &stubCapture{}
	h := &Handler{}
	h.SetGraphPresenceCapture(capture)
	principal := uuid.New()

	plan, err := presencehook.Capture(context.Background(), h.graphPresence, nil, presencehook.Spec{
		Family:      presencecapture.FamilyFriendsOfFriendsToggle,
		Posture:     presencecapture.FailClosedBlockWrite,
		PrincipalID: principal.String(),
	})

	require.NoError(t, err)
	assert.Nil(t, plan)
	require.Len(t, capture.subjects, 1)
	assert.Equal(t, principal, capture.subjects[0].Principal)
	assert.Equal(t, uuid.Nil, capture.subjects[0].Counterpart)
}

// The capture is a no-op on an unwired handler: a replica without the hook must
// behave exactly as it did before #2446. (There is no capturePresence method —
// an earlier comment named one that does not exist.)
func TestUsersCapturePresenceNoOpWhenUnwired(t *testing.T) {
	h := &Handler{}
	plan, err := presencehook.Capture(context.Background(), h.graphPresence, nil, presencehook.Spec{
		Family:      presencecapture.FamilyFriendsOfFriendsToggle,
		Posture:     presencecapture.FailClosedBlockWrite,
		PrincipalID: uuid.NewString(),
	})
	require.NoError(t, err)
	assert.Nil(t, plan)
}

// Drives the REAL handler, which is what the two tests above cannot do. The
// capture is deliberately gated on an ACTUAL change of the flag, so a PATCH
// that resubmits the current value must reconcile nothing: the audience is
// identical, and capturing anyway would take users-row work — and, once the
// durable leg lands, write markers — for a mutation that changed nothing.
func TestUpdatePrivacySettingsCapturesOnlyOnActualFoFChange(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)

	capture := &stubCapture{db: db}
	h := &Handler{db: db, log: logger.New("test")}
	h.SetGraphPresenceCapture(capture)
	require.True(t, h.HasGraphPresenceCapture())

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("user_id", userID.String())
		c.Next()
	})
	engine.PATCH("/users/me/privacy", h.UpdatePrivacySettings)

	patch := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(
			http.MethodPatch, "/users/me/privacy", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w, req)
		return w
	}

	// The column defaults to FALSE (migration 000032), so this is a real change.
	require.Equal(t, http.StatusOK, patch(`{"dm_friends_of_friends":true}`).Code)
	require.Len(t, capture.subjects, 1, "a real FoF change must reach the bridge")
	assert.Equal(t, presencecapture.FamilyFriendsOfFriendsToggle, capture.subjects[0].Family)
	assert.Equal(t, userID, capture.subjects[0].Principal)
	assert.Equal(t, uuid.Nil, capture.subjects[0].Counterpart,
		"the FoF toggle has no counterpart")
	assert.Equal(t, presencecapture.FailClosedBlockWrite, capture.subjects[0].FailPosture)

	// The gate and the capture are handed ONE value and ONE spec, so they cannot
	// disagree. Gating one stripe while reconciling a different pair is silent
	// by construction — nothing in the contract forces the two Subjects to
	// match, and only this assertion catches a handler that rebuilds one.
	require.Len(t, capture.gatedSubjects, 1,
		"the handler must surrender BeginTx to WithGatedTx")
	assert.Equal(t, capture.subjects[0], capture.gatedSubjects[0],
		"the subject gated must be the subject captured")

	// Same value again: nothing changed, so nothing may be captured. The gate is
	// still taken, because it keys on the field being SUPPLIED — whether the
	// value actually transitions is knowable only after the in-transaction read,
	// which is far too late to acquire a gate that must precede BeginTx.
	require.Equal(t, http.StatusOK, patch(`{"dm_friends_of_friends":true}`).Code)
	assert.Len(t, capture.subjects, 1, "a no-op FoF PATCH must not capture")
	assert.Len(t, capture.gatedSubjects, 2,
		"a supplied-but-unchanged value still takes the gate")

	// A different privacy field must not drag the FoF hook in with it — no
	// capture AND no gate, so such a PATCH behaves exactly as it did before.
	require.Equal(t, http.StatusOK, patch(`{"searchable_by_username":true}`).Code)
	assert.Len(t, capture.subjects, 1, "an unrelated privacy field must not capture")
	assert.Len(t, capture.gatedSubjects, 2,
		"a PATCH that never mentions dm_friends_of_friends must take no gate")

	assert.Empty(t, capture.abandoned, "no failure path ran")
}

// The FIRST-write half of the uncaptured-narrowing race (PR #2770 review,
// CodeRabbit). readPriorFoF locks with FOR UPDATE, which locks NOTHING when no
// row exists — so with the read ordered first, two concurrent first writes both
// saw the absent-row default and neither blocked. One set true; the other, having
// read false and requesting false, found *req == oldFoF, captured nothing, and
// narrowed visibility back with no viewer cleared.
//
// UpdatePrivacySettings now ensures the row BEFORE that read, and ON CONFLICT DO
// NOTHING is the serializer: the loser blocks on the primary-key index until the
// winner's transaction ends, so its read observes a committed value instead of
// the default.
//
// This drives the REAL handler. A version that ran ensure-then-read itself and
// asserted on the result would prove a property of PostgreSQL and pass with the
// handler's ordering reverted — the same reimplementation flaw the #2771 test
// pass was cleaning up. Instead the test holds the competing transaction open
// and asserts on whether a CAPTURE fires, which is the behaviour that differs:
// under the old ordering the handler reads sql.ErrNoRows immediately, sees
// false == false, and captures NOTHING.
func TestFirstWriteFoFRaceStillCapturesTheNarrowing(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	ctx := context.Background()

	var rows int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM privacy_settings WHERE user_id = $1`, userID).Scan(&rows))
	require.Zero(t, rows, "precondition: no row yet, so FOR UPDATE has nothing to lock")

	capture := &stubCapture{db: db}
	h := &Handler{db: db, log: logger.New("test")}
	h.SetGraphPresenceCapture(capture)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("user_id", userID.String())
		c.Next()
	})
	engine.PATCH("/users/me/privacy", h.UpdatePrivacySettings)

	// The competing first write: claims the row and turns the flag ON, holding
	// its transaction open so the handler below races it.
	rival, err := db.BeginTx(ctx, nil)
	require.NoError(t, err, "begin rival")
	defer func() { _ = rival.Rollback() }()
	_, err = rival.ExecContext(ctx,
		`INSERT INTO privacy_settings (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`,
		userID)
	require.NoError(t, err, "rival ensure")
	_, err = rival.ExecContext(ctx,
		`UPDATE privacy_settings SET dm_friends_of_friends = TRUE WHERE user_id = $1`, userID)
	require.NoError(t, err, "rival turns the flag on")

	// The handler asks for FALSE. Against the rival's committed TRUE that is a
	// narrowing and must capture; against the absent-row default it looks like
	// a no-op and captures nothing.
	code := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/users/me/privacy",
			strings.NewReader(`{"dm_friends_of_friends":false}`))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w, req)
		code <- w.Code
	}()

	// A slow machine can only make this MORE likely to still be blocked, so the
	// wait cannot flake toward a false pass: if the ensure serializes, the
	// handler cannot finish before the rival resolves at any speed.
	select {
	case got := <-code:
		t.Fatalf("the handler finished (%d) without blocking on the rival's row — "+
			"it did not ensure the row before its locked read", got)
	case <-time.After(300 * time.Millisecond):
	}

	require.NoError(t, rival.Commit(), "committing the rival releases the handler")

	select {
	case got := <-code:
		require.Equal(t, http.StatusOK, got, "the PATCH must still succeed")
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never resumed after the rival committed")
	}

	require.Len(t, capture.subjects, 1,
		"true→false is a NARROWING and must be captured; capturing nothing here is "+
			"the defect — the handler would have read the absent-row default, seen "+
			"false == false, and disabled the setting with no viewer ever cleared")
	assert.Equal(t, presencecapture.FamilyFriendsOfFriendsToggle, capture.subjects[0].Family)
	assert.Equal(t, userID, capture.subjects[0].Principal)

	var final bool
	require.NoError(t, db.QueryRow(
		`SELECT dm_friends_of_friends FROM privacy_settings WHERE user_id = $1`,
		userID).Scan(&final), "read back")
	assert.False(t, final, "and the requested value still lands")
}

// respondPresenceTerminal is the ONE place this handler turns a hooked
// transaction's terminal error into an HTTP response, so this is where #2446
// §3.6's shape is pinned for internal/users.
//
// Each subtest asserts BOTH directions of the guard it covers: the status the
// class must produce AND the status it must never produce. The blanket 500 this
// replaced passes a one-directional "is it not 200?" assertion for every case
// and a "is it 503?" assertion for none.
func TestUsersRespondPresenceTerminalMapsEachClass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{log: logger.New("test")}

	t.Run("pending marker is 503 with a positive Retry-After", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)

		handler.respondPresenceTerminal(c, msgFailedUpdatePrivacySettings,
			&presencecapture.PendingError{After: 30 * time.Second})

		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		assert.NotEqual(t, http.StatusInternalServerError, recorder.Code,
			"a pending marker proves nothing was written and resolves itself")
		assert.Equal(t, "30", recorder.Header().Get("Retry-After"))
	})

	t.Run("a sub-second pending delay still rounds up to a whole second", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)

		handler.respondPresenceTerminal(c, msgFailedUpdatePrivacySettings,
			&presencecapture.PendingError{After: 1500 * time.Millisecond})

		// 1 would send the client straight back into a marker still held.
		assert.Equal(t, "2", recorder.Header().Get("Retry-After"))
	})

	t.Run("post-commit delivery failure is 503, never 500", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)

		handler.respondPresenceTerminal(c, msgFailedUpdatePrivacySettings,
			fmt.Errorf("terminal: %w", presencecapture.ErrPostCommitDelivery))

		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		assert.NotEqual(t, http.StatusInternalServerError, recorder.Code,
			"the settings WERE written: a 500 here invites a pointless re-PATCH")
		// Retry-After promises self-resolution. A delivery failure is
		// post-commit and does not resolve itself, so the header must be absent.
		assert.Empty(t, recorder.Header().Get("Retry-After"))
		// The body must NOT be the site's failure message. This arm committed, so
		// "Failed to update privacy settings" would invite the duplicate retry the
		// 503 exists to prevent -- the lie moved from the status line into the body.
		// An earlier version of this test asserted that lie and so could not catch
		// it (Gitar, PR #2823). Asserted against a LITERAL: comparing to the same
		// constant the handler reads would compare the wire string to itself.
		assert.JSONEq(t, `{"error":"Your change was saved. Updating everyone who can see it is taking longer than usual."}`, recorder.Body.String())
		assert.NotContains(t, recorder.Body.String(), msgFailedUpdatePrivacySettings,
			"a committed mutation must never be reported to the client as failed")
	})

	t.Run("anything else is 500", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)

		handler.respondPresenceTerminal(c, msgFailedUpdatePrivacySettings,
			errors.New("connection reset"))

		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.NotEqual(t, http.StatusServiceUnavailable, recorder.Code)
		assert.Empty(t, recorder.Header().Get("Retry-After"),
			"a 500 proves nothing landed and does not clear itself")
		assert.JSONEq(t, `{"error":"`+msgFailedUpdatePrivacySettings+`"}`, recorder.Body.String())
	})

	// presencecapture.ErrCaptureBound has no Classify arm and lands here on
	// purpose (examined at Task 8, unchanged here). Its only producer is
	// graphpresence.checkFocalBound, which returns before any gate, transaction
	// or savepoint is taken, so the write provably did not land — which excludes
	// the delivery 503 whose whole meaning is that the mutation IS durable.
	t.Run("a capture-bound refusal is 500 with no retry promise", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)

		handler.respondPresenceTerminal(c, msgFailedUpdatePrivacySettings,
			fmt.Errorf("capture: %w", presencecapture.ErrCaptureBound))

		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Empty(t, recorder.Header().Get("Retry-After"))
	})
}

// The fail-closed Abandon terminal had NO positive coverage: deleting the
// presencehook.Abandon call left the whole internal/users suite green, because
// the only assertion on it was assert.Empty on the happy path. A capture that
// succeeded and a write that then failed must tear the plan down with
// CauseWriteFailed, or the reconciler keeps a plan for a mutation that never
// landed.
//
// This is also the only test that drives respondPresenceTerminal's 500 arm
// through the real handler rather than by calling it directly.
func TestUpdatePrivacySettingsAbandonsThePlanWhenTheWriteFails(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)

	capture := &stubCapture{db: db, poisonTx: true}
	h := &Handler{db: db, log: logger.New("test")}
	h.SetGraphPresenceCapture(capture)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("user_id", userID.String()) })
	router.PATCH("/privacy", h.UpdatePrivacySettings)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/privacy",
		strings.NewReader(`{"dm_friends_of_friends":true}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	require.Len(t, capture.subjects, 1, "the capture must have run before the write failed")
	assert.Equal(t, []string{string(presencecapture.CauseWriteFailed)}, capture.abandoned,
		"a failed write must tear the plan down, and name the cause that caused it")

	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"a poisoned transaction is not a pending marker and not a delivery failure")
	assert.Empty(t, w.Header().Get("Retry-After"),
		"a 500 proves nothing landed and does not clear itself")

	// The body message is part of the route shape. Asserted against a LITERAL,
	// not against msgFailedUpdatePrivacySettings: every other assertion
	// concatenates the constant, so they compare the wire string to itself and
	// changing the constant survives them all.
	assert.JSONEq(t, `{"error":"Failed to update privacy settings"}`, w.Body.String())

	// And nothing was written: the mutation must not survive its own failure.
	var persisted bool
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE((SELECT dm_friends_of_friends FROM privacy_settings WHERE user_id = $1), false)`,
		userID).Scan(&persisted))
	assert.False(t, persisted, "the poisoned transaction must have been discarded")
}
