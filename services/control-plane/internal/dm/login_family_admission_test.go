package dm

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type familyDMFixture struct {
	db                         *sql.DB
	redis                      *redis.Client
	handler                    *Handler
	actor, other, conversation uuid.UUID
	token                      string
	secret                     string
	sid                        uuid.UUID
}

func newFamilyDMFixture(t *testing.T) familyDMFixture {
	t.Helper()
	db, _ := dbtest.SetupTestDB(t)
	t.Cleanup(func() {
		_, err := dbtest.ExecReplica(db, `TRUNCATE login_family_revocation_targets,login_family_sids,login_families,login_family_revocations,login_family_rollout CASCADE`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO login_family_rollout(id) VALUES(true)`)
		require.NoError(t, err)
	})
	_, err := db.Exec(`UPDATE login_family_rollout SET activated_at=clock_timestamp(),receipt='{}'::jsonb WHERE id`)
	require.NoError(t, err)
	rc := redistest.Client(t)
	actor, other, conv := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), uuid.New()
	_, err = db.Exec(`INSERT INTO dm_conversations(id,created_by) VALUES($1,$2)`, conv, actor)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants(conversation_id,user_id) VALUES($1,$2),($1,$3)`, conv, actor, other)
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/mint", nil)
	secret := uuid.NewString()
	token, _, sid, err := auth.NewHandler(db, rc, logger.New("test"), secret, nil).IssueTokenPair(c, actor.String(), "")
	require.NoError(t, err)
	h := NewHandler(HandlerDeps{DB: db, Redis: rc, Log: logger.New("test"), Cfg: &config.Config{JWTSecret: secret}})
	return familyDMFixture{db: db, redis: rc, handler: h, actor: actor, other: other, conversation: conv, token: token, sid: uuid.MustParse(sid), secret: secret}
}
func (f familyDMFixture) router(callback gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(middleware.AuthRequired(f.secret, f.redis, f.db, credepoch.New(f.db, f.redis, logger.New("test"))))
	r.POST("/join", callback)
	return r
}
func (f familyDMFixture) request() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/join", nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	return r
}
func (f familyDMFixture) revoke(ctx context.Context, t *testing.T, rollback bool) {
	t.Helper()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer rollbackFamilyDMTx(t, tx)
	b, err := lf.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	set, err := lf.LockUsersTx(ctx, tx, b, []uuid.UUID{f.actor}, lf.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := set.ForUser(f.actor)
	require.NoError(t, err)
	targets, err := lf.ResolveTargetSIDTx(ctx, tx, user, f.sid)
	require.NoError(t, err)
	_, err = lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonExplicitRevoke})
	require.NoError(t, err)
	if rollback {
		require.NoError(t, tx.Rollback())
	} else {
		require.NoError(t, tx.Commit())
	}
}

// Removing the productive family check permits a Redis admission after the
// middleware's formerly valid snapshot has been revoked in real SQL.
func TestLoginFamilyDMRevokeWinsBeforeAdmission(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "rolled_back"}[rollback], func(t *testing.T) {
			f := newFamilyDMFixture(t)
			r := f.router(func(c *gin.Context) {
				f.revoke(c.Request.Context(), t, rollback)
				_, call, ok := f.handler.authorizeVoiceJoinTx(c, f.conversation.String(), f.actor.String(), f.conversation, f.actor, uuid.Nil)
				if ok {
					c.JSON(http.StatusOK, gin.H{"call": call})
				}
			})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, f.request())
			_, found, err := LookupDMVoiceJoinAdmission(context.Background(), f.redis, f.conversation, f.actor)
			require.NoError(t, err)
			if rollback {
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.True(t, found)
			} else {
				require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
				require.False(t, found, "revoked family must create no Redis admission")
			}
		})
	}
}

// A successful admission guard retains every participant row and the actor
// family until its owner's transaction ends. Competing writers must wait.
func TestLoginFamilyDMAdmissionRetainsCompleteUsersAndFamily(t *testing.T) {
	f := newFamilyDMFixture(t)
	r := f.router(func(c *gin.Context) {
		tx, ok := f.handler.beginDMVoiceMembershipTx(c, f.conversation.String(), f.actor.String())
		require.True(t, ok)
		defer rollbackFamilyDMTx(t, tx)
		for _, id := range []uuid.UUID{f.actor, f.other} {
			probe, err := f.db.Begin()
			require.NoError(t, err)
			err = probe.QueryRow(`SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE NOWAIT`, id).Scan(new(uuid.UUID))
			var locked *pq.Error
			require.ErrorAs(t, err, &locked)
			require.Equal(t, "55P03", string(locked.Code), "complete participant and actor lock prefix retained")
			require.NoError(t, probe.Rollback())
		}
		probe, err := f.db.Begin()
		require.NoError(t, err)
		err = probe.QueryRow(`SELECT family_id FROM login_families WHERE user_id=$1 FOR NO KEY UPDATE NOWAIT`, f.actor).Scan(new(uuid.UUID))
		var locked *pq.Error
		require.ErrorAs(t, err, &locked)
		require.Equal(t, "55P03", string(locked.Code), "family SHARE retained through admission")
		require.NoError(t, probe.Rollback())
		require.NoError(t, tx.Commit())
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, f.request())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	f.revoke(ctx, t, false)
}

func TestLoginFamilyDMUsersSnapshotRejectsCandidateDrift(t *testing.T) {
	f := newFamilyDMFixture(t)
	extra := dbtest.CreateUser(t, f.db)
	tx, err := f.db.Begin()
	require.NoError(t, err)
	defer rollbackFamilyDMTx(t, tx)
	b, err := lf.AcquireWriterBarrierTx(context.Background(), tx)
	require.NoError(t, err)
	subjects, _, err := dmblock.LockConversationAuthorityUsersTx(context.Background(), tx, b, f.conversation.String(), []uuid.UUID{f.actor}, lf.UserShare)
	require.NoError(t, err)
	// A compatible users SHARE trigger can insert a new member between discovery
	// and parent acquisition. The split leaf must reject this stale candidate set.
	_, err = f.db.Exec(`INSERT INTO dm_participants(conversation_id,user_id) VALUES($1,$2)`, f.conversation, extra)
	require.NoError(t, err)
	_, err = dmblock.PrepareConversationAfterUserLocksTx(context.Background(), tx, f.conversation.String(), subjects, dmblock.LockShare)
	require.ErrorIs(t, err, dmblock.ErrMembershipChanged)
	require.NoError(t, tx.Rollback())
}

// Removing the family recheck after lifecycle acquisition admits an expired
// identity into the real Redis lease despite the earlier SQL family guard.
func TestLoginFamilyDMExpiresDuringLifecycleWait(t *testing.T) {
	exerciseFamilyDMExpiryWait(t, "lifecycle")
}

func TestLoginFamilyDMExpiresDuringRingAcceptanceWait(t *testing.T) {
	exerciseFamilyDMExpiryWait(t, "active")
}

func TestLoginFamilyDMExpiresDuringInactiveRingFallback(t *testing.T) {
	for _, state := range []string{"removed", "terminal"} {
		t.Run(state, func(t *testing.T) { exerciseFamilyDMExpiryWait(t, state) })
	}
}

func rollbackFamilyDMTx(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
		require.NoError(t, err)
	}
}

func exerciseFamilyDMExpiryWait(t *testing.T, state string) {
	ringWait := state != "lifecycle"
	t.Helper()
	f := newFamilyDMFixture(t)
	sid := uuid.New()
	prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: f.actor, SID: sid, EmailVerified: true}, time.Now().Add(-auth.AccessTokenTTL+8*time.Second))
	require.NoError(t, err)
	tx, err := f.db.Begin()
	require.NoError(t, err)
	defer rollbackFamilyDMTx(t, tx)
	b, err := lf.AcquireWriterBarrierTx(context.Background(), tx)
	require.NoError(t, err)
	set, err := lf.LockUsersTx(context.Background(), tx, b, []uuid.UUID{f.actor}, lf.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := set.ForUser(f.actor)
	require.NoError(t, err)
	family, err := lf.CreateFamilyTx(context.Background(), tx, user, "")
	require.NoError(t, err)
	require.NoError(t, lf.BindSIDTx(context.Background(), tx, family, sid, prepared.ExpiresAt()))
	require.NoError(t, tx.Commit())
	f.token, err = prepared.Sign(f.secret)
	require.NoError(t, err)
	var ring *PendingCall
	var requestedRing uuid.UUID
	waitFunction := ".LockDMCallLifecycle("
	var unlock func()
	if ringWait {
		ring = &PendingCall{RingID: uuid.New(), CallerUserID: f.other, ConversationID: f.conversation, RingingUserIDs: map[uuid.UUID]struct{}{f.actor: {}}, DeclinedUserIDs: make(map[uuid.UUID]struct{}), AcceptedUserIDs: make(map[uuid.UUID]struct{}), RingStartedAt: time.Now()}
		if state == "active" {
			requestedRing = ring.RingID
		}
		pendingDMCalls.Store(f.conversation, ring)
		t.Cleanup(func() { pendingDMCalls.Delete(f.conversation) })
		ring.mu.Lock()
		unlock = ring.mu.Unlock
		waitFunction = ".tryAccept("
	} else {
		unlock = LockDMCallLifecycle(f.conversation)
	}
	unlocked := false
	defer func() {
		if !unlocked {
			unlock()
		}
	}()
	r := f.router(func(c *gin.Context) {
		_, call, ok := f.handler.authorizeVoiceJoinTx(c, f.conversation.String(), f.actor.String(), f.conversation, f.actor, requestedRing)
		if ok {
			c.JSON(http.StatusOK, gin.H{"call": call})
		}
	})
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); r.ServeHTTP(w, f.request()) }()
	// Observe the request blocked in the actual lifecycle mutex, not a sleep or
	// a synthetic test hook before the SQL authority boundary.
	require.Eventually(t, func() bool {
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		for _, goroutine := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.Contains(goroutine, waitFunction) && strings.Contains(goroutine, ".authorizeVoiceJoinTx(") {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond, "authenticated production admission must wait in lifecycle mutex")
	require.Eventually(t, func() bool {
		var expired bool
		e := f.db.QueryRow(`SELECT clock_timestamp()>$1::timestamptz`, prepared.ExpiresAt()).Scan(&expired)
		return e == nil && expired
	}, 10*time.Second, 20*time.Millisecond)
	if state == "removed" {
		pendingDMCalls.Delete(f.conversation)
	}
	if state == "terminal" {
		ring.terminalOwned = true
	}
	unlock()
	unlocked = true
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DM admission did not finish after lifecycle unlock")
	}
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	_, found, err := LookupDMVoiceJoinAdmission(context.Background(), f.redis, f.conversation, f.actor)
	require.NoError(t, err)
	assert.False(t, found, "expired identity creates no Redis admission")
	_, found, err = LookupDMVoiceCallLease(context.Background(), f.redis, f.conversation)
	require.NoError(t, err)
	assert.False(t, found, "expired identity creates no Redis lease")
	if ringWait {
		ring.mu.Lock()
		defer ring.mu.Unlock()
		if state == "terminal" {
			assert.True(t, ring.terminalOwned)
		} else {
			assert.False(t, ring.terminalOwned, "failed admission restores pending ring")
		}
		_, accepted := ring.AcceptedUserIDs[f.actor]
		assert.False(t, accepted)
		_, ringing := ring.RingingUserIDs[f.actor]
		assert.True(t, ringing)
	}
}
