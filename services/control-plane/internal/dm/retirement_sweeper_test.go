package dm

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

func TestRetirementSweeperRetiresOnlyFullyHiddenEmptyNonPersonalConversations(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	reapable := seedHiddenEmptyConversation(t, db, false, false, 2)
	group := seedHiddenEmptyConversation(t, db, false, true, 3)
	personal := seedHiddenEmptyConversation(t, db, true, false, 1)
	visible := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertVisibleParticipant(t, db, visible)
	withMessage := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertRetirementMessage(t, db, withMessage)

	result, err := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(io.Discard)).RunPass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, result.Retired)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, reapable))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, group))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, personal))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, visible))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, withMessage))
}

func TestRetirementSweeperDefersWhenPendingRingHasNoVoiceParticipantRow(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	conversationUUID := uuid.MustParse(conversationID)
	callerID := uuid.MustParse(conversationCreatorID(t, db, conversationID))
	ring := newPendingCall(conversationUUID, callerID, []uuid.UUID{uuid.New()}, time.Minute)
	storePendingDMCallForRetirementTest(t, conversationUUID, ring)

	result, err := NewRetirementSweeper(db, redisClient, nil, logger.NewWithWriter(io.Discard)).RunPass(context.Background())
	require.NoError(t, err)
	require.Zero(t, result.Retired)
	require.Equal(t, 1, result.Skipped)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func TestRetirementSweeperDefersForRemotePendingRingMarker(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	conversationUUID := uuid.MustParse(conversationID)
	require.NoError(t, MarkDMPendingVoiceCall(context.Background(), redisClient, conversationUUID, uuid.New()))
	pendingDMCalls.Delete(conversationUUID)

	result, err := NewRetirementSweeper(db, redisClient, nil, logger.NewWithWriter(io.Discard)).RunPass(context.Background())
	require.NoError(t, err)
	require.Zero(t, result.Retired)
	require.Equal(t, 1, result.Skipped)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func TestRetirementSweeperChecksPendingMarkerAfterParentLock(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	conversationUUID := uuid.MustParse(conversationID)
	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var lockedID string
	require.NoError(t, tx.QueryRow(`SELECT id FROM dm_conversations WHERE id = $1 FOR KEY SHARE`, conversationID).Scan(&lockedID))
	initialFenceRead := make(chan struct{})
	releaseInitialFence := make(chan struct{})
	sweeper := NewRetirementSweeper(db, redisClient, nil, logger.NewWithWriter(io.Discard))
	sweeper.afterCandidateReadHook = func() {
		close(initialFenceRead)
		<-releaseInitialFence
	}

	resultCh := make(chan struct {
		result RetirementSweepResult
		err    error
	}, 1)
	go func() {
		result, err := sweeper.RunPass(context.Background())
		resultCh <- struct {
			result RetirementSweepResult
			err    error
		}{result: result, err: err}
	}()
	select {
	case <-initialFenceRead:
	case <-time.After(5 * time.Second):
		t.Fatal("retirement pass did not complete its initial fence read")
	}
	require.NoError(t, MarkDMPendingVoiceCall(context.Background(), redisClient, conversationUUID, uuid.New()))
	close(releaseInitialFence)
	require.NoError(t, tx.Commit())

	select {
	case outcome := <-resultCh:
		require.NoError(t, outcome.err)
		require.Zero(t, outcome.result.Retired)
		require.Equal(t, 1, outcome.result.Skipped)
	case <-time.After(5 * time.Second):
		t.Fatal("retirement pass did not complete after parent key-share release")
	}
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func TestRetirementSweeperDefersWhenDirectVoiceReservationHasNoVoiceParticipantRow(t *testing.T) {
	testRetirementSweeperDefersForLease(t, false)
}

func TestRetirementSweeperDefersWhenAcceptedRingLeaseHasNoVoiceParticipantRow(t *testing.T) {
	testRetirementSweeperDefersForLease(t, true)
}

func TestRetirementSweeperFailsClosedWhenRedisFenceLookupFails(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	deadRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = deadRedis.Close() })

	result, err := NewRetirementSweeper(db, deadRedis, nil, logger.NewWithWriter(io.Discard)).RunPass(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, result.Selected)
	require.Equal(t, 1, result.Failed)
	require.Zero(t, result.Retired)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func testRetirementSweeperDefersForLease(t *testing.T, acceptedRing bool) {
	t.Helper()
	db, _ := dbtest.SetupTestDB(t)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	conversationUUID := uuid.MustParse(conversationID)
	callerID := uuid.MustParse(conversationCreatorID(t, db, conversationID))
	lease := VoiceCallLease{
		ConversationID: conversationUUID,
		CallID:         uuid.New(),
		CallerUserID:   callerID,
	}
	if acceptedRing {
		lease.RingID = uuid.New()
	}
	require.NoError(t, RefreshDMVoiceCallLease(context.Background(), redisClient, lease, time.Minute, true))

	result, err := NewRetirementSweeper(db, redisClient, nil, logger.NewWithWriter(io.Discard)).RunPass(context.Background())
	require.NoError(t, err)
	require.Zero(t, result.Retired)
	require.Equal(t, 1, result.Skipped)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func TestRetirementSweeperPreflightMakesProgressPastFencedCandidateBatch(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	for i := 0; i < retirementCandidateLimit+1; i++ {
		seedHiddenEmptyConversation(t, db, false, false, 2)
	}

	rows, err := db.Query(`SELECT id FROM dm_conversations ORDER BY id LIMIT $1`, retirementCandidateLimit+1)
	require.NoError(t, err)
	var candidates []string
	for rows.Next() {
		var conversationID string
		require.NoError(t, rows.Scan(&conversationID))
		candidates = append(candidates, conversationID)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, candidates, retirementCandidateLimit+1)
	fenced := candidates[:retirementCandidateLimit]
	unfenced := candidates[retirementCandidateLimit]
	for _, conversationID := range fenced {
		callerID := uuid.MustParse(conversationCreatorID(t, db, conversationID))
		conversationUUID := uuid.MustParse(conversationID)
		storePendingDMCallForRetirementTest(t, conversationUUID,
			newPendingCall(conversationUUID, callerID, []uuid.UUID{uuid.New()}, time.Minute))
	}
	require.Len(t, fenced, retirementCandidateLimit)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, newRetirementSweeperForTest(t, db, nil, logger.NewWithWriter(io.Discard)).RunPreflight(ctx))
	for _, conversationID := range fenced {
		require.Equal(t, 1, countRows(t, db,
			`SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID),
			"a fenced candidate must remain available for a later pass")
	}
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, unfenced),
		"the unfenced candidate after the first bounded batch must be retired")
	require.Equal(t, retirementCandidateLimit, countRows(t, db, `SELECT count(*) FROM dm_conversations
		WHERE is_personal = FALSE
		  AND NOT EXISTS (SELECT 1 FROM dm_participants WHERE conversation_id = dm_conversations.id AND hidden_at IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM dm_messages WHERE conversation_id = dm_conversations.id)`),
		"only the fenced candidates should remain eligible")
}

func TestRetirementSweeperPreflightSkipsTooManyVoiceCandidatesAndRetiresLaterCandidate(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	seedHiddenEmptyConversation(t, db, false, false, 2)
	seedHiddenEmptyConversation(t, db, false, false, 2)

	ids := retirementCandidateIDs(t, db)
	require.Len(t, ids, 2)
	poison, eligible := ids[0], ids[1]
	addVoiceEvidenceForAllParticipants(t, db, poison, maxGroupVoiceCandidates+1)

	err := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(io.Discard)).RunPreflight(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, poison),
		"an oversized voice candidate must remain for a later remediation pass")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, eligible),
		"a later eligible candidate must not be blocked by the oversized candidate")
}

func TestRetirementSweeperWorkerRetriesOversizedCandidateAfterAdvancing(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	for i := 0; i < retirementCandidateLimit+1; i++ {
		seedHiddenEmptyConversation(t, db, false, false, 2)
	}

	ids := retirementCandidateIDs(t, db)
	require.Len(t, ids, retirementCandidateLimit+1)
	poison, eligible := ids[0], ids[retirementCandidateLimit]
	addVoiceEvidenceForAllParticipants(t, db, poison, maxGroupVoiceCandidates+1)

	sweeper := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(io.Discard))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweeper.RunWorker(ctx, 10*time.Millisecond)
	}()

	require.Eventually(t, func() bool {
		return countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, eligible) == 0
	}, 10*time.Second, 10*time.Millisecond, "worker must advance past an oversized candidate")

	_, err := db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1`, poison)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, poison) == 0
	}, 10*time.Second, 10*time.Millisecond, "worker must retry the oversized candidate after the cursor wraps")
	cancel()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
}

func retirementCandidateIDs(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM dm_conversations
		WHERE is_personal = FALSE
		  AND NOT EXISTS (SELECT 1 FROM dm_participants WHERE conversation_id = dm_conversations.id AND hidden_at IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM dm_messages WHERE conversation_id = dm_conversations.id)
		ORDER BY id`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func addVoiceEvidenceForAllParticipants(t *testing.T, db *sql.DB, conversationID string, count int) {
	t.Helper()
	rows, err := db.Query(`SELECT user_id FROM dm_participants WHERE conversation_id = $1`, conversationID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var users []string
	for rows.Next() {
		var userID string
		require.NoError(t, rows.Scan(&userID))
		users = append(users, userID)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	for len(users) < count {
		users = append(users, dbtest.CreateUser(t, db).String())
		_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id, hidden_at) VALUES ($1, $2, clock_timestamp())`, conversationID, users[len(users)-1])
		require.NoError(t, err)
	}
	for _, userID := range users {
		_, err = db.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, userID)
		require.NoError(t, err)
	}
}

func storePendingDMCallForRetirementTest(t *testing.T, conversationID uuid.UUID, ring *PendingCall) {
	t.Helper()
	pendingDMCalls.Store(conversationID, ring)
	t.Cleanup(func() {
		if current, loaded := pendingDMCalls.LoadAndDelete(conversationID); loaded {
			current.(*PendingCall).StopTimer()
		}
	})
}

func TestRetirementSweeperPreflightAbortsWhenCandidateDiscoveryFails(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://127.0.0.1:1/task2821?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	var logs retirementLogBuffer
	err = NewRetirementSweeper(db, nil, nil, logger.NewWithWriter(&logs)).RunPreflight(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "discover retirement candidates")
	require.Contains(t, logs.String(), "phase=preflight selected=0 retired=0 skipped=0 failed=0")
}

func TestRetirementSweeperRequiresDatabaseForCandidateDiscovery(t *testing.T) {
	err := NewRetirementSweeper(nil, nil, nil, nil).RunPreflight(context.Background())
	require.ErrorContains(t, err, "requires database")
}

func TestRetirementSweeperCandidateDiscoveryHonorsCanceledContext(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewRetirementSweeper(db, nil, nil, nil).RunPass(ctx)
	require.Error(t, err)
	require.ErrorContains(t, err, "discover retirement candidates")
}

func TestRetirementSweeperCountsCandidateFailureAndKeepsConversation(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertDMVoiceParticipant(t, db, conversationID)

	result, err := newRetirementSweeperForTest(t, db, failingRemovalRail{db: db, t: t}, logger.NewWithWriter(io.Discard)).RunPass(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, result.Selected)
	require.Zero(t, result.Retired)
	require.Zero(t, result.Skipped)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_voice_participants WHERE conversation_id = $1`, conversationID))
}

func TestRetirementSweeperFailsClosedWhenActivePlanRailIsUnavailable(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertDMVoiceParticipant(t, db, conversationID)

	result, err := NewRetirementSweeper(db, nil, nil, logger.NewWithWriter(io.Discard)).RunPass(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
}

func TestRetirementSweeperCountsRailFailureAndPreservesRetryEvidence(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertDMVoiceParticipant(t, db, conversationID)

	result, err := newRetirementSweeperForTest(t, db, retirementGateFailureRail{}, logger.NewWithWriter(io.Discard)).RunPass(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_voice_participants WHERE conversation_id = $1`, conversationID))
}

func TestRetirementSweeperCascadeDeletesKeysAndLeavesMediaForOrphanRecovery(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	seedConversationKeyAndRevocation(t, db, conversationID)
	fileID, _ := seedConversationTier2Media(t, db, conversationID)

	_, err := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(io.Discard)).RunPass(context.Background())
	require.NoError(t, err)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_channel_keys WHERE conversation_id = $1`, conversationID))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_key_revocations WHERE conversation_id = $1`, conversationID))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM media_files WHERE id = $1`, fileID))
}

func TestRetirementSweeperDefersWhenEligibilityDriftsAfterCandidateRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *sql.DB, string)
	}{
		{name: "message", mutate: insertRetirementMessage},
		{name: "unhide", mutate: insertVisibleParticipant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
			seedConversationKeyAndRevocation(t, db, conversationID)
			sweeper := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(io.Discard))
			sweeper.afterCandidateReadHook = func() { tc.mutate(t, db, conversationID) }

			result, err := sweeper.RunPass(context.Background())
			require.NoError(t, err)
			require.Zero(t, result.Retired)
			require.Equal(t, 1, result.Skipped)
			require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
			require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_channel_keys WHERE conversation_id = $1`, conversationID))
			require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_key_revocations WHERE conversation_id = $1`, conversationID))
		})
	}
}

func TestRetirementSweeperDefersWhenVoiceCandidateAppearsAfterCandidateRead(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	seedConversationKeyAndRevocation(t, db, conversationID)
	sweeper := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(io.Discard))
	sweeper.afterCandidateReadHook = func() { insertDMVoiceParticipant(t, db, conversationID) }

	result, err := sweeper.RunPass(context.Background())
	require.NoError(t, err)
	require.Zero(t, result.Retired)
	require.Equal(t, 1, result.Skipped)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_channel_keys WHERE conversation_id = $1`, conversationID))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_key_revocations WHERE conversation_id = $1`, conversationID))
}

func TestRetirementSweeperDefersWhenUngatedVoiceCandidateAppearsAfterUsersLock(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	seedConversationKeyAndRevocation(t, db, conversationID)
	insertDMVoiceParticipant(t, db, conversationID)
	sweeper := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(io.Discard))
	sweeper.afterUsersLockHook = func(*sql.Tx) {
		insertHiddenDMVoiceParticipant(t, db, conversationID)
	}

	result, err := sweeper.RunPass(context.Background())
	require.NoError(t, err)
	require.Zero(t, result.Retired)
	require.Equal(t, 1, result.Skipped)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID))
	require.Equal(t, 2, countRows(t, db, `SELECT count(*) FROM dm_voice_participants WHERE conversation_id = $1`, conversationID))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_channel_keys WHERE conversation_id = $1`, conversationID))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_key_revocations WHERE conversation_id = $1`, conversationID))
}

func TestRetirementSweeperCapturesActiveVoicePlanBeforeCascade(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	participant := insertDMVoiceParticipant(t, db, conversationID)
	rail, deliverer := newRetirementRailAndDeliverer(t, db, conversationID)

	result, err := newRetirementSweeperForTest(t, db, rail, logger.NewWithWriter(io.Discard)).RunPass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, result.Retired)
	require.Equal(t, []uuid.UUID{participant}, deliverer.subjectsCleared())
	require.Len(t, deliverer.clears, 1)
	require.Equal(t, 1, deliverer.clears[0].plansVisible)
	require.Zero(t, deliverer.clears[0].conversationsLeft)
}

func TestRetirementSweeperPreflightDrainsUntilNoCandidateAndWorkerStopsOnCancel(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	seedHiddenEmptyConversation(t, db, false, false, 2)
	sweeper := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(io.Discard))
	require.NoError(t, sweeper.RunPreflight(context.Background()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweeper.RunWorker(ctx, time.Millisecond)
	}()
	cancel()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
}

func TestRetirementSweeperWorkerUsesDefaultIntervalWhenUnset(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	NewRetirementSweeper(nil, nil, nil, nil).RunWorker(ctx, 0)
}

func TestRetirementSweeperLogsSuccessfulAndSkippedPreflightPasses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configure  func(*RetirementSweeper, *sql.DB, string)
		wantResult string
	}{
		{
			name:       "successful",
			wantResult: "phase=preflight selected=1 retired=1 skipped=0 failed=0",
		},
		{
			name: "skipped",
			configure: func(sweeper *RetirementSweeper, db *sql.DB, conversationID string) {
				sweeper.afterCandidateReadHook = func() { insertVisibleParticipant(t, db, conversationID) }
			},
			wantResult: "phase=preflight selected=1 retired=0 skipped=1 failed=0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
			var logs retirementLogBuffer
			sweeper := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(&logs))
			if tc.configure != nil {
				tc.configure(sweeper, db, conversationID)
			}

			require.NoError(t, sweeper.RunPreflight(context.Background()))
			require.Contains(t, logs.String(), tc.wantResult)
			require.NotContains(t, logs.String(), conversationID)
		})
	}
}

func TestRetirementSweeperLogsPeriodicPass(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	var logs retirementLogBuffer
	sweeper := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(&logs))
	sweeper.afterCandidateReadHook = func() { insertVisibleParticipant(t, db, conversationID) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweeper.RunWorker(ctx, time.Millisecond)
	}()
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "phase=periodic selected=1 retired=0 skipped=1 failed=0")
	}, time.Second, time.Millisecond)
	cancel()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	require.NotContains(t, logs.String(), conversationID)
}

func TestRetirementSweeperCascadeLeavesTier2ObjectForOrphanReaper(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	_, tier2Key := seedConversationTier2Media(t, db, conversationID)
	creatorID := conversationCreatorID(t, db, conversationID)
	tier1Key := "avatars/" + uuid.NewString()
	_, err := db.Exec(`UPDATE users SET avatar_url = $1 WHERE id = $2`, tier1Key, creatorID)
	require.NoError(t, err)

	_, err = newRetirementSweeperForTest(t, db, newRetirementRail(t, db), logger.NewWithWriter(io.Discard)).RunPass(context.Background())
	require.NoError(t, err)

	store := &retirementOrphanStore{objects: []storage.StoredObject{{Key: tier2Key}, {Key: tier1Key}}}
	result, err := media.NewOrphanReaper(db, store, string(storage.LegacyBackendID), logger.NewWithWriter(io.Discard)).SweepOrphans(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, result.Reaped)
	require.Equal(t, []string{tier2Key}, store.deleted)
}

func seedHiddenEmptyConversation(t *testing.T, db *sql.DB, personal, group bool, participants int) string {
	t.Helper()
	require.Positive(t, participants)
	creator := dbtest.CreateUser(t, db)
	var conversationID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, is_personal, created_by)
		VALUES ($1, $2, $3) RETURNING id`, group, personal, creator).Scan(&conversationID))
	for i := 0; i < participants; i++ {
		userID := creator
		if i > 0 {
			userID = dbtest.CreateUser(t, db)
		}
		_, err := db.Exec(`
			INSERT INTO dm_participants (conversation_id, user_id, hidden_at)
			VALUES ($1, $2, clock_timestamp())`, conversationID, userID)
		require.NoError(t, err)
	}
	return conversationID
}

func insertVisibleParticipant(t *testing.T, db *sql.DB, conversationID string) {
	t.Helper()
	_, err := db.Exec(`UPDATE dm_participants SET hidden_at = NULL
		WHERE conversation_id = $1
		  AND user_id = (SELECT created_by FROM dm_conversations WHERE id = $1)`, conversationID)
	require.NoError(t, err)
}

func insertRetirementMessage(t *testing.T, db *sql.DB, conversationID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO dm_messages (conversation_id, user_id, content, type)
		SELECT id, created_by, 'ciphertext', 'user' FROM dm_conversations WHERE id = $1`, conversationID)
	require.NoError(t, err)
}

func seedConversationKeyAndRevocation(t *testing.T, db *sql.DB, conversationID string) {
	t.Helper()
	creatorID := conversationCreatorID(t, db, conversationID)
	_, err := db.Exec(`INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version)
		VALUES ($1, $2, 'wrapped', 1)`, conversationID, creatorID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_key_revocations
		(conversation_id, revoked_epoch, successor_epoch, reason, revoked_by)
		VALUES ($1, 1, 2, 'test', $2)`, conversationID, creatorID)
	require.NoError(t, err)
}

func seedConversationTier2Media(t *testing.T, db *sql.DB, conversationID string) (string, string) {
	t.Helper()
	creatorID := conversationCreatorID(t, db, conversationID)
	storageKey := "attachments/" + uuid.NewString()
	var fileID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO media_files
			(uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id)
		VALUES ($1, 'file', 2, 'application/octet-stream', 1, $2, 1, $3)
		RETURNING id`, creatorID, storageKey, conversationID).Scan(&fileID))
	return fileID, storageKey
}

func conversationCreatorID(t *testing.T, db *sql.DB, conversationID string) string {
	t.Helper()
	var creatorID string
	require.NoError(t, db.QueryRow(`SELECT created_by FROM dm_conversations WHERE id = $1`, conversationID).Scan(&creatorID))
	return creatorID
}

func newRetirementRail(t *testing.T, db *sql.DB) ActivePlanRail {
	t.Helper()
	h, _ := newDMHandlerWithRail(t, db, "")
	return h.activePlans
}

func newRetirementRailAndDeliverer(t *testing.T, db *sql.DB, conversationID string) (ActivePlanRail, *recordingDeliverer) {
	t.Helper()
	h, deliverer := newDMHandlerWithRail(t, db, conversationID)
	return h.activePlans, deliverer
}

func newRetirementSweeperForTest(t *testing.T, db *sql.DB, rail ActivePlanRail, log *logger.Logger) *RetirementSweeper {
	t.Helper()
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	return NewRetirementSweeper(db, redisClient, rail, log)
}

func insertDMVoiceParticipant(t *testing.T, db *sql.DB, conversationID string) uuid.UUID {
	t.Helper()
	participantID, err := uuid.Parse(conversationCreatorID(t, db, conversationID))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, participantID)
	require.NoError(t, err)
	return participantID
}

func insertHiddenDMVoiceParticipant(t *testing.T, db *sql.DB, conversationID string) uuid.UUID {
	t.Helper()
	participantID := dbtest.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id, hidden_at)
		VALUES ($1, $2, clock_timestamp())`, conversationID, participantID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, participantID)
	require.NoError(t, err)
	return participantID
}

type retirementOrphanStore struct {
	objects []storage.StoredObject
	deleted []string
}

type retirementGateFailureRail struct{}

func (retirementGateFailureRail) WithGatedTx(context.Context, []uuid.UUID, func(*sql.Tx) error) error {
	return errors.New("sender gate unavailable")
}

func (retirementGateFailureRail) CapturePlansTx(context.Context, *sql.Tx, []activepresence.Plan) error {
	return errors.New("unreachable")
}

func (retirementGateFailureRail) CompleteAlreadyGated(context.Context, *sql.Tx, []activepresence.PlanKey) error {
	return errors.New("unreachable")
}

type retirementLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *retirementLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *retirementLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func (s *retirementOrphanStore) ListObjects(context.Context, string, time.Time) ([]storage.StoredObject, error) {
	return s.objects, nil
}

func (s *retirementOrphanStore) DeleteObject(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	return nil
}
