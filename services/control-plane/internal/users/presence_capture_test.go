package users

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
