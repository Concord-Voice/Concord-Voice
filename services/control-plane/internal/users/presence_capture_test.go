package users

import (
	"context"
	"database/sql"
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
	subjects  []presencecapture.Subject
	abandoned []string
}

func (c *stubCapture) CaptureInTx(
	_ context.Context, _ *sql.Tx, s presencecapture.Subject,
) (presencecapture.Plan, error) {
	c.subjects = append(c.subjects, s)
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

	capture := &stubCapture{}
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

	// Same value again: nothing changed, so nothing may be captured.
	require.Equal(t, http.StatusOK, patch(`{"dm_friends_of_friends":true}`).Code)
	assert.Len(t, capture.subjects, 1, "a no-op FoF PATCH must not capture")

	// A different privacy field must not drag the FoF hook in with it.
	require.Equal(t, http.StatusOK, patch(`{"searchable_by_username":true}`).Code)
	assert.Len(t, capture.subjects, 1, "an unrelated privacy field must not capture")

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

	capture := &stubCapture{}
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
