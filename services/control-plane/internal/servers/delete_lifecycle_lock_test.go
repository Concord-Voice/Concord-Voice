package servers

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
)

type recordingServerDeleteRail struct {
	plans      []activepresence.Plan
	keys       []activepresence.PlanKey
	captureErr error
}

type serverDeleteTestResult struct {
	committed bool
	err       error
}

func startServerDelete(
	ctx context.Context,
	db *sql.DB,
	h *Handler,
	serverID, ownerID uuid.UUID,
	preflight []serverVoiceCandidate,
) <-chan serverDeleteTestResult {
	done := make(chan serverDeleteTestResult, 1)
	go func() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			done <- serverDeleteTestResult{err: err}
			return
		}
		outcome := serverDeleteOutcome{}
		err = h.deleteServerWithActivePlans(
			ctx, tx, serverID.String(), ownerID.String(), preflight, &outcome,
		)
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
		done <- serverDeleteTestResult{committed: outcome.committed, err: err}
	}()
	return done
}

func createServerVoiceDeleteFixture(
	t *testing.T,
	db *sql.DB,
	eventAt time.Time,
) (owner, sender, serverID, channelID uuid.UUID) {
	t.Helper()
	owner = dbtest.CreateUser(t, db)
	sender = dbtest.CreateUser(t, db)
	serverID, channelID = uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'delete-lock', $2)`, serverID, owner)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO server_members (server_id, user_id, role)
		VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, serverID, owner, sender)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'voice', 'voice')`, channelID, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)`, channelID, sender, eventAt)
	require.NoError(t, err)
	return owner, sender, serverID, channelID
}

func (*recordingServerDeleteRail) WithGatedRevocationTx(
	context.Context, []uuid.UUID, func() func(), func(*sql.Tx) error,
) error {
	return errors.New("recording server-delete rail cannot gate")
}

func (r *recordingServerDeleteRail) CapturePlansTx(
	_ context.Context, _ *sql.Tx, plans []activepresence.Plan,
) error {
	if r.captureErr != nil {
		return r.captureErr
	}
	r.plans = append(r.plans, plans...)
	return nil
}

func (r *recordingServerDeleteRail) CompleteAlreadyGated(
	_ context.Context, _ *sql.Tx, keys []activepresence.PlanKey,
) error {
	r.keys = append(r.keys, keys...)
	return nil
}

func TestServerDeleteTransactionFailureClassDistinguishesAnUnresolvedCommit(t *testing.T) {
	require.Equal(t, "commit_unresolved", serverDeleteTransactionFailureClass(
		errors.Join(errServerDeleteCommitUnresolved, errors.New("driver commit failure"))))
	require.Equal(t, "transaction", serverDeleteTransactionFailureClass(errors.New("pre-commit failure")))
}

func TestDeleteServerSerializesWithServerVoiceLifecycleWriter(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	oldEventAt := time.Date(2026, time.January, 17, 10, 0, 0, 0, time.UTC)
	newEventAt := oldEventAt.Add(time.Minute)
	owner, sender, serverID, channelID := createServerVoiceDeleteFixture(t, db, oldEventAt)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	writer, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := writer.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback lifecycle writer: %v", rollbackErr)
		}
	})
	require.NoError(t, voice.LockServerVoiceLifecycleTx(ctx, writer, sender))
	_, err = writer.ExecContext(ctx, `
		UPDATE voice_participants SET lifecycle_event_at = $3
		WHERE channel_id = $1 AND user_id = $2`, channelID, sender, newEventAt)
	require.NoError(t, err)
	var writerPID int
	require.NoError(t, writer.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID))

	rail := &recordingServerDeleteRail{}
	h := &Handler{db: db, activePlans: rail}
	done := startServerDelete(ctx, db, h, serverID, owner, []serverVoiceCandidate{{
		subjectID: sender, channelID: channelID, lifecycleEventAt: oldEventAt,
	}})

	waitForBackendBlockedBy(ctx, t, db, writerPID)
	select {
	case result := <-done:
		t.Fatalf("server deletion passed the lifecycle writer before it committed: %v", result.err)
	default:
	}
	require.NoError(t, writer.Commit())

	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.True(t, result.committed)
	case <-ctx.Done():
		t.Fatal("server deletion did not finish after the lifecycle writer committed")
	}
	require.Len(t, rail.plans, 1)
	require.Equal(t, newEventAt, rail.plans[0].EventAt,
		"the final reread must capture the lifecycle writer's committed generation")
	require.Equal(t, []activepresence.PlanKey{{
		SubjectID: sender, Category: activepresence.CategoryServerVoice,
	}}, rail.keys)
}

func TestDeleteServerAllowsDepartedPreflightCandidate(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	eventAt := time.Date(2026, time.January, 17, 10, 30, 0, 0, time.UTC)
	owner, sender, serverID, channelID := createServerVoiceDeleteFixture(t, db, eventAt)

	rail := &recordingServerDeleteRail{}
	h := &Handler{db: db, activePlans: rail}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	preflight, err := h.preflightServerVoiceCandidates(ctx, serverID.String(), owner.String())
	require.NoError(t, err)
	require.Len(t, preflight, 1)

	blocker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := blocker.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback lifecycle blocker: %v", rollbackErr)
		}
	})
	require.NoError(t, voice.LockServerVoiceLifecycleTx(ctx, blocker, sender))
	var blockerPID int
	require.NoError(t, blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))

	type departureResult struct {
		backendPID                      int
		survivingUser, survivingChannel bool
		err                             error
	}
	departure := make(chan departureResult, 2)
	releaseDeparture := make(chan struct{}, 1)
	go func() {
		result := departureResult{}
		tx, beginErr := db.BeginTx(ctx, nil)
		if beginErr != nil {
			result.err = beginErr
			departure <- result
			return
		}
		defer func() { _ = tx.Rollback() }()
		if result.err = voice.LockServerVoiceLifecycleTx(ctx, tx, sender); result.err != nil {
			departure <- result
			return
		}
		if result.err = tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&result.backendPID); result.err != nil {
			departure <- result
			return
		}
		_, result.err = tx.ExecContext(ctx,
			`DELETE FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, channelID, sender)
		if result.err != nil {
			departure <- result
			return
		}
		result.err = tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, sender).Scan(&result.survivingUser)
		if result.err == nil {
			result.err = tx.QueryRowContext(ctx,
				`SELECT EXISTS(SELECT 1 FROM channels WHERE id = $1)`, channelID).Scan(&result.survivingChannel)
		}
		departure <- result
		if result.err != nil {
			return
		}
		select {
		case <-releaseDeparture:
			departure <- departureResult{err: tx.Commit()}
		case <-ctx.Done():
			departure <- departureResult{err: ctx.Err()}
		}
	}()
	waitForBackendBlockedBy(ctx, t, db, blockerPID)

	done := startServerDelete(ctx, db, h, serverID, owner, preflight)
	require.NoError(t, blocker.Commit())

	var ready departureResult
	select {
	case ready = <-departure:
		require.NoError(t, ready.err)
	case <-ctx.Done():
		t.Fatal("participant departure did not acquire the lifecycle lock")
	}
	require.True(t, ready.survivingUser)
	require.True(t, ready.survivingChannel)
	waitForBackendBlockedBy(ctx, t, db, ready.backendPID)
	releaseDeparture <- struct{}{}
	select {
	case result := <-departure:
		require.NoError(t, result.err)
	case <-ctx.Done():
		t.Fatal("participant departure did not commit after release")
	}

	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.True(t, result.committed)
	case <-ctx.Done():
		t.Fatal("server deletion did not finish after participant departure")
	}
	require.Empty(t, rail.plans)
	require.Empty(t, rail.keys)
}

func TestDeleteServerKeepsVoiceEvidenceAfterChannelTypeChanges(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	eventAt := time.Date(2026, time.January, 17, 10, 45, 0, 0, time.UTC)
	owner, sender, serverID, channelID := createServerVoiceDeleteFixture(t, db, eventAt)

	rail := &recordingServerDeleteRail{}
	h := &Handler{db: db, activePlans: rail}
	preflight, err := h.preflightServerVoiceCandidates(context.Background(), serverID.String(), owner.String())
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE channels SET type = 'text' WHERE id = $1`, channelID)
	require.NoError(t, err)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	outcome := serverDeleteOutcome{}
	require.NoError(t, h.deleteServerWithActivePlans(
		context.Background(), tx, serverID.String(), owner.String(), preflight, &outcome,
	))
	require.True(t, outcome.committed)
	require.Len(t, rail.plans, 1)
	require.Equal(t, sender, rail.plans[0].SubjectID)
	require.Equal(t, channelID, rail.plans[0].LifecycleID)
	require.Equal(t, eventAt, rail.plans[0].EventAt)
}

func TestDeleteServerPreservesVoiceEvidenceAfterChannelDeletion(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	eventAt := time.Date(2026, time.January, 17, 10, 47, 0, 0, time.UTC)
	owner, sender, serverID, channelID := createServerVoiceDeleteFixture(t, db, eventAt)

	rail := &recordingServerDeleteRail{}
	h := &Handler{db: db, activePlans: rail}
	preflight, err := h.preflightServerVoiceCandidates(context.Background(), serverID.String(), owner.String())
	require.NoError(t, err)
	require.Len(t, preflight, 1)
	_, err = db.Exec(`DELETE FROM channels WHERE id = $1`, channelID)
	require.NoError(t, err)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	outcome := serverDeleteOutcome{}
	require.NoError(t, h.deleteServerWithActivePlans(
		context.Background(), tx, serverID.String(), owner.String(), preflight, &outcome,
	))
	require.True(t, outcome.committed)
	require.Len(t, rail.plans, 1)
	require.Equal(t, sender, rail.plans[0].SubjectID)
	require.Equal(t, channelID, rail.plans[0].LifecycleID)
	require.Equal(t, eventAt, rail.plans[0].EventAt)
}

func TestDeleteServerLocksCandidateUsersBeforeServer(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	eventAt := time.Date(2026, time.January, 17, 10, 50, 0, 0, time.UTC)
	owner, sender, serverID, _ := createServerVoiceDeleteFixture(t, db, eventAt)
	rail := &recordingServerDeleteRail{}
	h := &Handler{db: db, activePlans: rail}
	preflight, err := h.preflightServerVoiceCandidates(context.Background(), serverID.String(), owner.String())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	blocker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := blocker.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback server-lock blocker: %v", rollbackErr)
		}
	})
	var lockedServer uuid.UUID
	require.NoError(t, blocker.QueryRowContext(ctx,
		`SELECT id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&lockedServer))
	var blockerPID int
	require.NoError(t, blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))

	done := startServerDelete(ctx, db, h, serverID, owner, preflight)
	waitForBackendBlockedBy(ctx, t, db, blockerPID)
	var lockedUser uuid.UUID
	err = db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, sender).Scan(&lockedUser)
	var lockErr *pq.Error
	require.ErrorAs(t, err, &lockErr)
	require.Equal(t, "55P03", string(lockErr.Code),
		"the candidate user must already be locked while deletion waits on the server")
	require.NoError(t, blocker.Commit())

	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.True(t, result.committed)
	case <-ctx.Done():
		t.Fatal("server deletion did not finish after the server lock was released")
	}
}

func TestDeleteServerLocksChannelParentsBeforeFinalReread(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	oldEventAt := time.Date(2026, time.January, 17, 10, 55, 0, 0, time.UTC)
	newEventAt := oldEventAt.Add(time.Minute)
	owner, sender, serverID, channelID := createServerVoiceDeleteFixture(t, db, oldEventAt)
	rail := &recordingServerDeleteRail{}
	h := &Handler{db: db, activePlans: rail}
	preflight, err := h.preflightServerVoiceCandidates(context.Background(), serverID.String(), owner.String())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	blocker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := blocker.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback channel-lock blocker: %v", rollbackErr)
		}
	})
	var lockedChannel uuid.UUID
	require.NoError(t, blocker.QueryRowContext(ctx,
		`SELECT id FROM channels WHERE id = $1 FOR UPDATE`, channelID).Scan(&lockedChannel))
	var blockerPID int
	require.NoError(t, blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))

	done := startServerDelete(ctx, db, h, serverID, owner, preflight)
	waitForBackendBlockedBy(ctx, t, db, blockerPID)
	_, err = db.ExecContext(ctx, `
		UPDATE voice_participants SET lifecycle_event_at = $3
		WHERE channel_id = $1 AND user_id = $2`, channelID, sender, newEventAt)
	require.NoError(t, err)
	require.NoError(t, blocker.Commit())

	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.True(t, result.committed)
	case <-ctx.Done():
		t.Fatal("server deletion did not finish after the channel lock was released")
	}
	require.Len(t, rail.plans, 1)
	require.Equal(t, newEventAt, rail.plans[0].EventAt,
		"the final reread must occur after the channel-parent lock is acquired")
}

func TestDeleteServerWithActivePlansFailsClosedBeforeDelete(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	priorOwner := dbtest.CreateUser(t, db)
	currentOwner := dbtest.CreateUser(t, db)
	serverID, channelID := uuid.New(), uuid.New()
	eventAt := time.Date(2026, time.January, 17, 11, 0, 0, 0, time.UTC)
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'stale-owner', $2)`, serverID, currentOwner)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO server_members (server_id, user_id, role)
		VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, serverID, currentOwner, priorOwner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'voice', 'voice')`, channelID, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)`, channelID, priorOwner, eventAt)
	require.NoError(t, err)

	captureErr := errors.New("capture failed")
	candidate := serverVoiceCandidate{subjectID: priorOwner, channelID: channelID, lifecycleEventAt: eventAt}
	for _, tc := range []struct {
		name      string
		serverID  uuid.UUID
		userID    uuid.UUID
		preflight []serverVoiceCandidate
		rail      *recordingServerDeleteRail
		want      error
	}{
		{name: "owner changed", serverID: serverID, userID: priorOwner, rail: &recordingServerDeleteRail{}, want: errServerDeleteNotOwner},
		{name: "server disappeared", serverID: uuid.New(), userID: priorOwner, rail: &recordingServerDeleteRail{}, want: errServerDeleteNotFound},
		{name: "ungated sender arrived", serverID: serverID, userID: currentOwner, rail: &recordingServerDeleteRail{}, want: errServerVoiceCandidateDrift},
		{name: "plan capture failed", serverID: serverID, userID: currentOwner, preflight: []serverVoiceCandidate{candidate}, rail: &recordingServerDeleteRail{captureErr: captureErr}, want: captureErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, beginErr := db.BeginTx(context.Background(), nil)
			require.NoError(t, beginErr)
			outcome := serverDeleteOutcome{}
			deleteErr := (&Handler{db: db, activePlans: tc.rail}).deleteServerWithActivePlans(
				context.Background(), tx, tc.serverID.String(), tc.userID.String(), tc.preflight, &outcome)
			require.ErrorIs(t, deleteErr, tc.want)
			require.False(t, outcome.committed)
			require.NoError(t, tx.Rollback())
		})
	}
}

func TestCandidateSubjectIDsSortsForDeterministicLockOrder(t *testing.T) {
	low := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	high := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	require.Equal(t, []uuid.UUID{low, high}, candidateSubjectIDs([]serverVoiceCandidate{
		{subjectID: high}, {subjectID: low},
	}))
}

func TestValidateServerVoiceCandidatesTreatsPreflightAsAnUpperBound(t *testing.T) {
	preflightSender := uuid.New()
	allowed := map[uuid.UUID]struct{}{preflightSender: {}}

	require.NoError(t, validateServerVoiceCandidates(nil, allowed),
		"a departed preflight sender is safely omitted")
	err := validateServerVoiceCandidates([]serverVoiceCandidate{{
		subjectID: uuid.New(),
	}}, allowed)
	require.ErrorIs(t, err, errServerVoiceCandidateDrift,
		"a new sender was never gated and must force a retry")
	require.Equal(t, "candidate_drift", serverVoiceCandidateFailureClass(err))
}

func TestValidateServerVoiceCandidatesClassifiesDuplicateBeforeBound(t *testing.T) {
	candidates := make([]serverVoiceCandidate, 0, maxServerVoiceCandidates+1)
	for range maxServerVoiceCandidates {
		candidates = append(candidates, serverVoiceCandidate{subjectID: uuid.New()})
	}
	candidates = append(candidates, candidates[0])

	err := validateServerVoiceCandidates(candidates, nil)
	require.ErrorIs(t, err, errServerVoiceCandidateAmbiguous)
	require.Equal(t, "candidate_ambiguous", serverVoiceCandidateFailureClass(err))
}

func waitForBackendBlockedBy(ctx context.Context, t *testing.T, db *sql.DB, blockerPID int) {
	t.Helper()
	for {
		var waiting bool
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE $1 = ANY(pg_blocking_pids(pid))
			)`, blockerPID).Scan(&waiting)
		if err != nil {
			t.Fatalf("wait for lifecycle-lock waiter: %v", err)
		}
		if waiting {
			return
		}
	}
}
