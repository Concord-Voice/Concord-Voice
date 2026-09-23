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
		ctx, recorder := voiceFenceTestContext()
		tx, locked := h.beginDMVoiceMembershipTx(ctx, uuid.NewString(), uuid.NewString())
		require.False(t, locked)
		require.Nil(t, tx)
		require.Equal(t, http.StatusForbidden, recorder.Code)
	})

	t.Run("missing member", func(t *testing.T) {
		conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
		ctx, recorder := voiceFenceTestContext()
		tx, locked := h.beginDMVoiceMembershipTx(ctx, conversationID, uuid.NewString())
		require.False(t, locked)
		require.Nil(t, tx)
		require.Equal(t, http.StatusForbidden, recorder.Code)
	})
}

func TestBeginDMVoiceMembershipTxAcceptsMemberAndRollsBackClosedDB(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h, _ := newDMHandlerWithRail(t, db, "")
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	userID := conversationCreatorID(t, db, conversationID)

	ctx, recorder := voiceFenceTestContext()
	tx, locked := h.beginDMVoiceMembershipTx(ctx, conversationID, userID)
	require.True(t, locked)
	require.NotNil(t, tx)
	require.Equal(t, http.StatusOK, recorder.Code)
	rollbackDMVoiceMembershipTx(tx, h.log)

	closed, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	closedHandler, _ := newDMHandlerWithRail(t, closed, "")
	ctx, recorder = voiceFenceTestContext()
	tx, locked = closedHandler.beginDMVoiceMembershipTx(ctx, conversationID, userID)
	require.False(t, locked)
	require.Nil(t, tx)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
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

	require.False(t, h.initializePendingDMCall(ring, conversationID, false, nil, nil))
	_, present := pendingDMCalls.Load(conversationID)
	require.True(t, present, "failed initialization leaves cleanup ownership with the caller")
	require.Nil(t, ring.TimeoutTimer)
	h.cancelFailedPendingDMCall(conversationID, ring)
	_, present = pendingDMCalls.Load(conversationID)
	require.False(t, present)
	require.Nil(t, ring.TimeoutTimer)
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

	ctx, recorder := voiceFenceTestContext()
	ctx.Params = gin.Params{{Key: "id", Value: conversationID}}
	ctx.Set("user_id", conversationCreatorID(t, db, conversationID))
	h.RingDMCall(ctx)
	require.Equal(t, http.StatusOK, recorder.Code)
	_, present := pendingDMCalls.Load(uuid.MustParse(conversationID))
	require.True(t, present)
	shared, err := HasDMPendingVoiceCall(ctx.Request.Context(), redisClient, uuid.MustParse(conversationID))
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

	ctx, recorder := voiceFenceTestContext()
	ctx.Params = gin.Params{{Key: "id", Value: conversationID}}
	ctx.Set("user_id", userID)
	h.AuthorizeVoiceJoin(ctx)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
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
