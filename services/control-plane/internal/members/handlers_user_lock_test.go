package members

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockModerationUsersTxLocksDistinctSubjects(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	actor := dbtest.CreateUser(t, db)
	target := dbtest.CreateUser(t, db)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, lockModerationUsersTx(context.Background(), tx, target.String(), actor.String()))

	for _, userID := range []string{target.String(), actor.String()} {
		probe, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		var locked string
		err = probe.QueryRowContext(context.Background(),
			`SELECT id FROM users WHERE id = $1 FOR UPDATE NOWAIT`, userID).Scan(&locked)
		require.Error(t, err)
		var pqErr *pq.Error
		require.ErrorAs(t, err, &pqErr)
		require.Equal(t, "55P03", string(pqErr.Code))
		require.NoError(t, probe.Rollback())
	}
}

func TestLockModerationUsersTxRejectsMissingOrMalformedSubject(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	existing := dbtest.CreateUser(t, db)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	err = lockModerationUsersTx(context.Background(), tx, "00000000-0000-0000-0000-000000000099", existing.String())
	require.ErrorIs(t, err, errModerationTargetGone)
	err = lockModerationUsersTx(context.Background(), tx, "not-a-uuid", existing.String())
	require.Error(t, err)
}

func TestLockModerationUsersTxAllowsSelfModerationSubjectOnce(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	user := dbtest.CreateUser(t, db)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, lockModerationUsersTx(context.Background(), tx, user.String(), user.String()))
}

func TestValidateBanProbeDistinguishesProbeRaces(t *testing.T) {
	assert.NoError(t, validateBanProbe(false, true))
	assert.ErrorIs(t, validateBanProbe(true, true), presencehook.ErrProbeStale)
	assert.NoError(t, validateBanProbe(false, false))
	assert.NoError(t, validateBanProbe(true, false))
}

func TestModerationTargetCanonicalizesUUIDsAndRejectsBadBindings(t *testing.T) {
	serverID := "11111111-1111-1111-1111-111111111111"
	userID := "22222222-2222-2222-2222-222222222222"

	newContext := func(server, user string) (*gin.Context, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
		c.Params = gin.Params{{Key: "id", Value: server}, {Key: "user_id", Value: user}}
		return c, w
	}

	c, w := newContext("{"+serverID+"}", "22222222222222222222222222222222")
	gotServer, gotUser, ok := moderationTarget(c)
	require.True(t, ok)
	assert.Equal(t, serverID, gotServer)
	assert.Equal(t, userID, gotUser)
	assert.Equal(t, http.StatusOK, w.Code)

	c, w = newContext("bad", userID)
	_, _, ok = moderationTarget(c)
	assert.False(t, ok)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	c, w = newContext(serverID, "bad")
	_, _, ok = moderationTarget(c)
	assert.False(t, ok)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
