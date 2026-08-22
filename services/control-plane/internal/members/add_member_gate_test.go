package members_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/members"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// errGateDoubleReached is returned INSTEAD of running the closure, for the same
// reason internal/members' recordingCapture does it: a double that swallowed the
// work and reported success would let a handler whose mutation never ran look
// healthy. Here the question is only whether the gated path was ENTERED.
var errGateDoubleReached = errors.New("members_test: gate double reached")

// gateRecorder records the Subject WithGatedTx was asked to gate. It keeps that
// list SEPARATE from what CaptureInTx was asked to capture — internal/members'
// recordingCapture conflates the two into one slice, which makes a nil-capture
// regression invisible, and that conflation is why this double exists.
type gateRecorder struct {
	mu       sync.Mutex
	gated    []presencecapture.Subject
	captured []presencecapture.Subject
}

func (g *gateRecorder) WithGatedTx(
	_ context.Context, subject presencecapture.Subject, _ func(*sql.Tx) error,
) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gated = append(g.gated, subject)
	return errGateDoubleReached
}

func (g *gateRecorder) CaptureInTx(
	_ context.Context, _ *sql.Tx, subject presencecapture.Subject,
) (presencecapture.Plan, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.captured = append(g.captured, subject)
	return nil, nil
}

func (*gateRecorder) Complete(context.Context, *sql.Tx, presencecapture.Plan) error { return nil }
func (*gateRecorder) Abandon(presencecapture.Plan, presencecapture.Cause)           {}

func (g *gateRecorder) gatedSubjects() []presencecapture.Subject {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]presencecapture.Subject(nil), g.gated...)
}

// hookedMembersHandler builds a members handler wired to capture and mounts it
// on a bare engine authenticating as actingUserID. It deliberately does not
// reuse ts.Router, which owns its own handler the capture cannot reach — the
// same reasoning as internal/friends' hookedHandler.
func hookedMembersHandler(
	t *testing.T, ts *testhelpers.TestServer, capture presencecapture.GraphPresenceCapture,
	actingUserID string, register func(*gin.Engine, *members.Handler),
) *gin.Engine {
	t.Helper()

	log := logger.New("test")
	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, log)
	audit := rbac.NewAuditWriter(ts.DB, log)
	h := members.NewHandler(ts.DB, log, ts.Redis, ts.Hub, resolver, audit)
	h.SetGraphPresenceCapture(capture)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("user_id", actingUserID)
		c.Next()
	})
	register(engine, h)
	return engine
}

// TestAddMemberOnAnExistingMemberTakesNoGate is the #2854 stage C acceptance
// criterion for the add path: "AddMember against an already-member target
// acquires NO gate for the target".
//
// PAIRED with the terminal assertion. "No gate was taken" alone passes
// vacuously for any early return — a 400 on request shape, a 403 on
// permissions — so on its own it would certify a broken handler.
func TestAddMemberOnAnExistingMemberTakesNoGate(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "gateaddowner")
	joiner := ts.CreateTestUser(t, "gateaddjoiner")
	serverID := ts.CreateTestServer(t, owner.ID, "GateAddServer")

	// The joiner is ALREADY a member, so the add is a proven no-op.
	_, err := ts.DB.Exec(
		`INSERT INTO server_members (server_id, user_id, role, joined_at)
		 VALUES ($1, $2, 'member', NOW()) ON CONFLICT DO NOTHING`,
		serverID, joiner.ID)
	require.NoError(t, err)

	capture := &gateRecorder{}
	engine := hookedMembersHandler(t, ts, capture, owner.ID, func(e *gin.Engine, h *members.Handler) {
		e.POST("/servers/:id/members", h.AddMember)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(
		http.MethodPost, "/servers/"+serverID+"/members",
		strings.NewReader(`{"user_id":"`+joiner.ID+`"}`)))

	require.Equal(t, http.StatusConflict, w.Code,
		"the request must traverse the whole handler and reach its own 409, not bail earlier")
	require.Empty(t, capture.gatedSubjects(),
		"AC (#2854): no gate may be acquired for a target whose add is a proven no-op")

	var rows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, joiner.ID).Scan(&rows))
	require.Equal(t, 1, rows, "the no-op must remain a no-op")
}

// TestAddMemberOnANewMemberStillGates is the non-regression half: it fails if
// the probe's branch is inverted and real adds start short-circuiting.
func TestAddMemberOnANewMemberStillGates(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "gateaddowner2")
	joiner := ts.CreateTestUser(t, "gateaddjoiner2")
	serverID := ts.CreateTestServer(t, owner.ID, "GateAddServer2")

	capture := &gateRecorder{}
	engine := hookedMembersHandler(t, ts, capture, owner.ID, func(e *gin.Engine, h *members.Handler) {
		e.POST("/servers/:id/members", h.AddMember)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(
		http.MethodPost, "/servers/"+serverID+"/members",
		strings.NewReader(`{"user_id":"`+joiner.ID+`"}`)))

	gated := capture.gatedSubjects()
	require.Len(t, gated, 1, "a genuine add must still enter the gated path")
	require.Equal(t, joiner.ID, gated[0].Principal.String(),
		"the principal is the user being added, not the actor")
	require.Equal(t, presencecapture.FamilyMemberAdd, gated[0].Family)
}
