package dm

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	_ "github.com/lib/pq"
)

func TestBeginDMVoiceMembershipTxRejectsMissingConversationAndMember(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h, _ := newDMHandlerWithRail(t, db, "")

	t.Run("missing conversation", func(t *testing.T) {
		userID := dbtest.CreateUser(t, db).String()
		recorder := serveAdmittedVoiceFenceRequest(t, db, userID, uuid.NewString(), func(ctx *gin.Context) {
			tx, locked := h.beginDMVoiceMembershipTx(ctx, ctx.Param("id"), userID)
			if tx != nil {
				defer rollbackDMVoiceMembershipTx(tx, h.log)
			}
			require.False(t, locked)
			require.Nil(t, tx)
		})
		require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
		require.JSONEq(t, `{"error":"dm_unavailable"}`, recorder.Body.String())
	})

	t.Run("missing member", func(t *testing.T) {
		conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
		userID := dbtest.CreateUser(t, db).String()
		var memberships int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, userID).Scan(&memberships))
		require.Zero(t, memberships, "the admitted actor must be a genuine nonmember")
		recorder := serveAdmittedVoiceFenceRequest(t, db, userID, conversationID, func(ctx *gin.Context) {
			tx, locked := h.beginDMVoiceMembershipTx(ctx, conversationID, userID)
			if tx != nil {
				defer rollbackDMVoiceMembershipTx(tx, h.log)
			}
			require.False(t, locked)
			require.Nil(t, tx)
		})
		require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
		require.JSONEq(t, `{"error":"dm_unavailable"}`, recorder.Body.String())
	})
}

func TestBeginDMVoiceMembershipTxAcceptsMemberAndRollsBackClosedDB(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h, _ := newDMHandlerWithRail(t, db, "")
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	userID := conversationCreatorID(t, db, conversationID)

	recorder := serveAdmittedVoiceFenceRequest(t, db, userID, conversationID, func(ctx *gin.Context) {
		tx, locked := h.beginDMVoiceMembershipTx(ctx, conversationID, userID)
		if tx != nil {
			defer rollbackDMVoiceMembershipTx(tx, h.log)
		}
		require.True(t, locked)
		require.NotNil(t, tx)
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	closed, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	closedHandler, _ := newDMHandlerWithRail(t, closed, "")
	// Middleware admits on the live DB; only the productive membership pool is closed.
	recorder = serveAdmittedVoiceFenceRequest(t, db, userID, conversationID, func(ctx *gin.Context) {
		tx, locked := closedHandler.beginDMVoiceMembershipTx(ctx, conversationID, userID)
		if tx != nil {
			defer rollbackDMVoiceMembershipTx(tx, closedHandler.log)
		}
		require.False(t, locked)
		require.Nil(t, tx)
	})
	// BeginTx unavailability is a dependency failure, not an authority contract error.
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	require.JSONEq(t, `{"error":"Failed to authorize"}`, recorder.Body.String())
}

func TestLockedDMVoiceStateLoadersReadMemberState(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	userID := conversationCreatorID(t, db, conversationID)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Rollback()) })
	require.NoError(t, lockDMVoiceMembership(context.Background(), tx, conversationID, userID))

	joinState, err := loadVoiceJoinStateTx(context.Background(), tx, conversationID, userID)
	require.NoError(t, err)
	require.False(t, joinState.isGroup)

	identity, err := loadDMVoiceAuthorizeIdentityTx(context.Background(), tx, conversationID, userID)
	require.NoError(t, err)
	require.False(t, identity.isGroup)
	require.NotEmpty(t, identity.username)
}

func TestInitializePendingDMCallLeavesExpiredRingForFailedStartCleanup(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h, _ := newDMHandlerWithRail(t, db, "")
	h.hub = websocket.NewHub(db, nil)
	conversationID := uuid.New()
	ring := newPendingCall(conversationID, uuid.New(), []uuid.UUID{uuid.New()}, time.Minute)
	ring.RingStartedAt = time.Now().Add(-time.Duration(DefaultRingTimeoutSeconds+1) * time.Second)
	pendingDMCalls.Store(conversationID, ring)
	t.Cleanup(func() {
		ring.StopTimer()
		pendingDMCalls.Delete(conversationID)
	})

	initialized, announced := h.initializePendingDMCall(ring, conversationID, false, nil, nil)
	require.False(t, initialized)
	require.False(t, announced)
	_, present := pendingDMCalls.Load(conversationID)
	require.True(t, present, "failed initialization leaves cleanup ownership with the caller")
	require.Nil(t, ring.TimeoutTimer)
	h.cancelFailedPendingDMCall(conversationID, ring)
	_, present = pendingDMCalls.Load(conversationID)
	require.False(t, present)
	require.Nil(t, ring.TimeoutTimer)
}

func TestInitializePendingDMCallRejectsUnclaimedRing(t *testing.T) {
	conversationID := uuid.New()
	ring := newPendingCall(conversationID, uuid.New(), []uuid.UUID{uuid.New()}, time.Minute)

	ring.mu.Lock()
	initialized, announced := (&Handler{}).initializePendingDMCall(
		ring, conversationID, false, nil, nil,
	)
	ring.mu.Unlock()

	require.False(t, initialized)
	require.False(t, announced)
	require.Nil(t, ring.TimeoutTimer)
}

func TestDiscardPendingDMCallLockedRemovesUnannouncedRing(t *testing.T) {
	conversationID := uuid.New()
	ring := newPendingCall(conversationID, uuid.New(), nil, time.Minute)
	ring.TimeoutTimer = time.AfterFunc(time.Hour, func() {})
	pendingDMCalls.Store(conversationID, ring)
	t.Cleanup(func() {
		ring.StopTimer()
		pendingDMCalls.Delete(conversationID)
	})

	ring.mu.Lock()
	discardPendingDMCallLocked(ring)
	ring.mu.Unlock()
	_, present := pendingDMCalls.Load(conversationID)
	require.False(t, present)
	require.False(t, ring.TimeoutTimer.Stop(), "discard must stop the pending timeout timer")
}

func TestNoDMVoiceCalleesErrorMessages(t *testing.T) {
	t.Run("group", func(t *testing.T) {
		require.Equal(t, "no online members to call", noDMVoiceCalleesError(true))
	})
	t.Run("direct", func(t *testing.T) {
		require.Equal(t, "no callees in this conversation", noDMVoiceCalleesError(false))
	})
}

func TestResolvePendingDMRingRejectsInvalidActorID(t *testing.T) {
	ctx, recorder := voiceFenceTestContext()
	ctx.Params = gin.Params{{Key: "id", Value: uuid.NewString()}}
	ctx.Set("user_id", "not-a-uuid")

	ring, convUUID, actorUUID, tx, ok := (&Handler{}).resolvePendingDMRing(ctx)
	require.False(t, ok)
	require.Nil(t, ring)
	require.Equal(t, uuid.Nil, convUUID)
	require.Equal(t, uuid.Nil, actorUUID)
	require.Nil(t, tx)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestRingDMCallFailsClosedWhenRedisFenceIsUnavailable(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h, _ := newDMHandlerWithRail(t, db, "")
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	makeConversationVisible(t, db, conversationID)
	h.redis = closedRedisForTest(t)

	ctx, recorder := voiceFenceTestContext()
	ctx.Params = gin.Params{{Key: "id", Value: conversationID}}
	ctx.Set("user_id", conversationCreatorID(t, db, conversationID))
	h.RingDMCall(ctx)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.False(t, func() bool {
		_, ok := pendingDMCalls.Load(uuid.MustParse(conversationID))
		return ok
	}())
}

func TestRingDMCallPublishesPendingRingAfterSharedFence(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h, _ := newDMHandlerWithRail(t, db, "")
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	makeConversationVisible(t, db, conversationID)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	h.redis = redisClient
	h.hub = websocket.NewHub(db, redisClient)
	go h.hub.Run()
	t.Cleanup(func() { h.hub.Shutdown() })
	t.Cleanup(func() { ResetPendingDMCallsForTest() })

	userID := conversationCreatorID(t, db, conversationID)
	recorder := serveAdmittedVoiceFenceRequest(t, db, userID, conversationID, h.RingDMCall)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	_, present := pendingDMCalls.Load(uuid.MustParse(conversationID))
	require.True(t, present)
	shared, err := HasDMPendingVoiceCall(context.Background(), redisClient, uuid.MustParse(conversationID))
	require.NoError(t, err)
	require.True(t, shared)
}

func TestAuthorizeVoiceJoinFailsClosedWhenRedisLeaseIsUnavailable(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h, _ := newDMHandlerWithRail(t, db, "")
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	makeConversationVisible(t, db, conversationID)
	userID := conversationCreatorID(t, db, conversationID)
	h.redis = closedRedisForTest(t)

	recorder := serveAdmittedVoiceFenceRequest(t, db, userID, conversationID, h.AuthorizeVoiceJoin)
	require.Equal(t, http.StatusInternalServerError, recorder.Code, recorder.Body.String())
	require.JSONEq(t, `{"error":"Failed to authorize"}`, recorder.Body.String())
}

func TestRetirementWithoutPlansSkipsConversationRemovedBeforeLock(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	sweeper := NewRetirementSweeper(db, redisClient, nil, logger.NewWithWriter(httptest.NewRecorder()))

	retired, err := sweeper.retireWithoutPlans(context.Background(), uuid.NewString())
	require.NoError(t, err)
	require.False(t, retired)
}

func TestRetirementWithoutPlansFailsClosedWhenRedisFenceIsUnavailable(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	sweeper := NewRetirementSweeper(db, closedRedisForTest(t), nil, logger.NewWithWriter(httptest.NewRecorder()))

	retired, err := sweeper.retireWithoutPlans(context.Background(), conversationID)
	require.Error(t, err)
	require.False(t, retired)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func TestRetirementWithoutPlansSkipsWhenVoiceEvidenceAppears(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertDMVoiceParticipant(t, db, conversationID)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	sweeper := NewRetirementSweeper(db, redisClient, nil, logger.NewWithWriter(httptest.NewRecorder()))

	retired, err := sweeper.retireWithoutPlans(context.Background(), conversationID)
	require.NoError(t, err)
	require.False(t, retired)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func TestRetirementEligibilityRejectsVisibleAndMissingConversations(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	makeConversationVisible(t, db, conversationID)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Rollback()) })
	eligible, err := retirementEligibleTx(context.Background(), tx, conversationID)
	require.NoError(t, err)
	require.False(t, eligible)

	eligible, err = retirementEligibleTx(context.Background(), tx, uuid.NewString())
	require.NoError(t, err)
	require.False(t, eligible)
}

func TestRetirementHelpersReturnTransactionErrors(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	_, err = lockRetirementConversation(context.Background(), tx, uuid.NewString())
	require.Error(t, err)
	require.Error(t, deleteRetirementConversation(context.Background(), tx, uuid.NewString()))
}

func TestPendingVoiceMarkerRejectsUnavailableInvalidAndCorruptState(t *testing.T) {
	ctx := context.Background()
	conversationID := uuid.New()
	ringID := uuid.New()

	require.Error(t, MarkDMPendingVoiceCall(ctx, nil, conversationID, ringID))
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(ctx, redisClient))
	require.Error(t, MarkDMPendingVoiceCall(ctx, redisClient, uuid.Nil, ringID))
	require.Error(t, MarkDMPendingVoiceCall(ctx, redisClient, conversationID, uuid.Nil))
	_, err := HasDMPendingVoiceCall(ctx, nil, conversationID)
	require.Error(t, err)
	_, err = HasDMPendingVoiceCall(ctx, redisClient, uuid.Nil)
	require.Error(t, err)

	key := dmVoicePendingCallKey(conversationID)
	require.NoError(t, redisClient.Set(ctx, key, "corrupt-ring-id", time.Minute).Err())
	_, err = HasDMPendingVoiceCall(ctx, redisClient, conversationID)
	require.Error(t, err)
}

func TestPendingVoiceMarkerFailsClosedWhenRedisCloses(t *testing.T) {
	ctx := context.Background()
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(ctx, redisClient))
	conversationID, ringID := uuid.New(), uuid.New()
	require.NoError(t, redisClient.Close())

	require.Error(t, MarkDMPendingVoiceCall(ctx, redisClient, conversationID, ringID))
	_, err := HasDMPendingVoiceCall(ctx, redisClient, conversationID)
	require.Error(t, err)
}

func TestRetireOneFailsClosedWhenVoiceEvidenceHasNoPlanRail(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertDMVoiceParticipant(t, db, conversationID)
	sweeper := NewRetirementSweeper(db, redisClient, nil, logger.NewWithWriter(httptest.NewRecorder()))

	result, err := sweeper.RunPass(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func TestRetireOneRejectsTooManyVoiceSubjects(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	for i := 0; i < maxGroupVoiceCandidates+1; i++ {
		insertHiddenDMVoiceParticipant(t, db, conversationID)
	}
	sweeper := NewRetirementSweeper(db, redisClient, nil, logger.NewWithWriter(httptest.NewRecorder()))

	result, err := sweeper.RunPass(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func TestRetireOneFailsClosedWhenContextCancelsBeforeTransaction(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sweeper := NewRetirementSweeper(db, redisClient, nil, logger.NewWithWriter(httptest.NewRecorder()))
	sweeper.afterCandidateReadHook = cancel

	result, err := sweeper.RunPass(ctx)
	require.Error(t, err)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func TestRetireOneDefersWhenMarkerAppearsAfterVoiceLock(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertDMVoiceParticipant(t, db, conversationID)
	sweeper := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(httptest.NewRecorder()))
	sweeper.afterUsersLockHook = func(*sql.Tx) {
		require.NoError(t, MarkDMPendingVoiceCall(context.Background(), redisClient,
			uuid.MustParse(conversationID), uuid.New()))
	}

	result, err := sweeper.RunPass(context.Background())
	require.NoError(t, err)
	require.Zero(t, result.Retired)
	require.Equal(t, 1, result.Skipped)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func TestRetireGatedConversationSkipsMissingParent(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Rollback()) })

	sweeper := NewRetirementSweeper(db, nil, nil, nil)
	retired, err := sweeper.retireGatedConversation(context.Background(), tx, uuid.NewString(), uuid.New(), nil)
	require.NoError(t, err)
	require.False(t, retired)
}

func TestRetireOneRejectsMalformedConversationID(t *testing.T) {
	sweeper := NewRetirementSweeper(nil, nil, nil, nil)
	retired, err := sweeper.retireOne(context.Background(), "malformed")
	require.Error(t, err)
	require.False(t, retired)
}

// Keep admission dependencies live even when the custom handler's DB or Redis is unavailable.
func serveAdmittedVoiceFenceRequest(t *testing.T, db *sql.DB, userID, conversationID string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	redisClient := redistest.Client(t)
	log := logger.NewWithWriter(httptest.NewRecorder())
	secret := uuid.NewString()
	mintContext, _ := voiceFenceTestContext()
	token, _, _, err := auth.NewHandler(db, redisClient, log, secret, nil).IssueTokenPair(mintContext, userID, "")
	require.NoError(t, err)

	router := gin.New()
	router.Use(middleware.AuthRequired(secret, redisClient, db, credepoch.New(db, redisClient, log)))
	entered := false
	router.POST("/voice/:id", func(ctx *gin.Context) {
		entered = true
		require.Equal(t, userID, ctx.GetString("user_id"))
		identity, _, verified := middleware.VerifiedAccessFromContext(ctx)
		require.True(t, verified, "productive handler must receive real middleware authority")
		require.Equal(t, userID, identity.UserID.String())
		handler(ctx)
	})
	request := httptest.NewRequest(http.MethodPost, "/voice/"+conversationID, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.True(t, entered, "request must reach the custom handler after real admission: %s", recorder.Body.String())
	return recorder
}

func voiceFenceTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/voice", nil)
	return ctx, recorder
}

func makeConversationVisible(t *testing.T, db *sql.DB, conversationID string) {
	t.Helper()
	_, err := db.Exec(`UPDATE dm_participants SET hidden_at = NULL WHERE conversation_id = $1`, conversationID)
	require.NoError(t, err)
}

func closedRedisForTest(t *testing.T) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	require.NoError(t, client.Close())
	return client
}
