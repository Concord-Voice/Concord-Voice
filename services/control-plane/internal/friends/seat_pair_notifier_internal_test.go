package friends

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// The Wing seat hook (follow-up to #4133) runs after an unfriend or a block
// commits, names the blocker for a block and uuid.Nil for an unfriend, and does
// not run when nothing changed.

type seatPairCall struct{ a, b, blocker uuid.UUID }

// seatPairRecorder records each call and, read from the pool at call time,
// how many accepted friendships the pair still had: zero proves the hook ran
// after the change committed, not inside its transaction.
type seatPairRecorder struct {
	db             *sql.DB
	calls          []seatPairCall
	acceptedAtCall []int
}

func (r *seatPairRecorder) notify(ctx context.Context, a, b, blocker uuid.UUID) {
	r.calls = append(r.calls, seatPairCall{a: a, b: b, blocker: blocker})
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM friendships WHERE status = 'accepted'
		AND ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))`, a, b).Scan(&n); err != nil {
		n = -1
	}
	r.acceptedAtCall = append(r.acceptedAtCall, n)
}

func seatPairRequest(h *Handler, fn func(*Handler, *gin.Context), method string, actor, target uuid.UUID) int {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/friends/"+target.String(), nil)
	c.Params = gin.Params{{Key: "user_id", Value: target.String()}}
	c.Set("user_id", actor.String())
	fn(h, c)
	return w.Code
}

func seatPairFixture(t *testing.T) (*Handler, *seatPairRecorder, uuid.UUID, uuid.UUID) {
	t.Helper()
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	a, b := insertTestUser(t, db), insertTestUser(t, db)
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)
	h := NewHandler(db, logger.New("test"), nil)
	rec := &seatPairRecorder{db: db}
	h.SetSeatPairNotifier(rec.notify)
	return h, rec, a, b
}

func TestRemoveFriend_RunsTheSeatHookAfterCommit(t *testing.T) {
	h, rec, a, b := seatPairFixture(t)
	require.Equal(t, http.StatusOK, seatPairRequest(h, (*Handler).RemoveFriend, http.MethodDelete, a, b))
	assert.Equal(t, []seatPairCall{{a: a, b: b, blocker: uuid.Nil}}, rec.calls)
	assert.Equal(t, []int{0}, rec.acceptedAtCall, "the hook ran after the unfriend committed")

	// Nothing left to remove: a 404, and no hook.
	require.Equal(t, http.StatusNotFound, seatPairRequest(h, (*Handler).RemoveFriend, http.MethodDelete, a, b))
	assert.Len(t, rec.calls, 1)
}

func TestBlockUser_RunsTheSeatHookNamingTheBlocker(t *testing.T) {
	h, rec, a, b := seatPairFixture(t)
	require.Equal(t, http.StatusOK, seatPairRequest(h, (*Handler).BlockUser, http.MethodPost, b, a))
	assert.Equal(t, []seatPairCall{{a: b, b: a, blocker: b}}, rec.calls)
	assert.Equal(t, []int{0}, rec.acceptedAtCall, "the hook ran after the block committed")
}

func TestSeatHook_UnwiredIsANoOp(t *testing.T) {
	h, _, a, b := seatPairFixture(t)
	h.SetSeatPairNotifier(nil)
	assert.Equal(t, http.StatusOK, seatPairRequest(h, (*Handler).RemoveFriend, http.MethodDelete, a, b))
}
