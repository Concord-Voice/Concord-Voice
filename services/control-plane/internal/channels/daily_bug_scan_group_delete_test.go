package channels_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/channels"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type channelScopePlan struct{}

func (channelScopePlan) HasWork() bool { return false }

type channelScopeRecorder struct {
	mu     sync.Mutex
	scopes [][]string
}

func (r *channelScopeRecorder) PrepareCapture(_ context.Context, _ string, channelIDs []string, _ *string) (rbac.PresenceRecheckPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scopes = append(r.scopes, append([]string(nil), channelIDs...))
	return channelScopePlan{}, nil
}

func (*channelScopeRecorder) CaptureVisibility(context.Context, *sql.Tx, rbac.PresenceRecheckPlan) error {
	return nil
}

func (*channelScopeRecorder) Execute(rbac.PresenceRecheckPlan)         {}
func (*channelScopeRecorder) Abandon(rbac.PresenceRecheckPlan, string) {}

func (r *channelScopeRecorder) capturedScopes() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([][]string, len(r.scopes))
	for i := range r.scopes {
		result[i] = append([]string(nil), r.scopes[i]...)
	}
	return result
}

// A synced child's voice classification can change after preflight while the
// delete waits for the visibility lock. The group delete must compare the
// locked voice-ID set and retry capture with the newly voice-enabled child.
func TestDeleteChannelGroup_RetriesWhenVoiceChildSetChanges(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "voice-set-retry", owner.AccessToken)
	channelID := ts.CreateTestChannel(t, serverID, "voice-set-child")
	assignChannelToCategory(t, ts, channelID, groupID, true)

	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
	authority := rbac.NewHandler(ts.DB, logger.New("test"), ts.Redis, ts.Hub, resolver, cache, rbac.NewAuditWriter(ts.DB, logger.New("test")))
	recorder := &channelScopeRecorder{}
	authority.SetPresenceRecheck(recorder)
	h := channels.NewHandler(ts.DB, logger.New("test"), ts.Hub, resolver, ts.Redis)
	h.SetAuthorityHandler(authority)

	barrier, probe := holdGroupMutationVisibility(t, ts, serverID)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "group_id", Value: groupID}}
	c.Set("user_id", owner.ID)
	c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
	completed := make(chan struct{})
	go func() {
		h.DeleteChannelGroup(c)
		close(completed)
	}()
	// The initial preflight/capture used the text-only voice scope. Waiting for
	// the lock waiter proves the handler reached its first transaction.
	dbtest.WaitForAdvisoryLockWaiter(t, probe, mustGroupVisibilityKey(t, serverID))
	_, err := barrier.Exec(`UPDATE channels SET type = 'voice' WHERE id = $1`, channelID)
	require.NoError(t, err)
	require.NoError(t, barrier.Commit())
	<-completed

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, [][]string{nil, {channelID}}, recorder.capturedScopes(),
		"a changed locked voice-child set must reject the stale capture and retry with the voice child")
}
