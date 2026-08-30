package ownership

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type captureFailureRecheck struct {
	prepareErr error
	strictErr  error
	prepared   int
	strictUsed int
	visibility int
	commits    int
	abandons   []string
	trace      *[]string
	executeCh  chan struct{}
}

type captureFailurePlan struct{}

func (captureFailurePlan) HasWork() bool { return true }

func (r *captureFailureRecheck) PrepareCapture(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	r.prepared++
	return captureFailurePlan{}, r.prepareErr
}

func (r *captureFailureRecheck) PrepareCaptureStrict(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	r.strictUsed++
	return captureFailurePlan{}, r.strictErr
}

func (r *captureFailureRecheck) CaptureVisibility(context.Context, *sql.Tx, rbac.PresenceRecheckPlan) error {
	r.visibility++
	if r.trace != nil {
		*r.trace = append(*r.trace, "capture")
	}
	return nil
}

func (r *captureFailureRecheck) Execute(rbac.PresenceRecheckPlan) {
	r.commits++
	if r.executeCh != nil {
		r.executeCh <- struct{}{}
	}
}

func (r *captureFailureRecheck) Abandon(_ rbac.PresenceRecheckPlan, cause string) {
	r.abandons = append(r.abandons, cause)
}

type cacheFailureEnforcer struct{ users []string }

func (e *cacheFailureEnforcer) RecheckUser(_, userID string) { e.users = append(e.users, userID) }
func (*cacheFailureEnforcer) RecheckChannel(string, string)  {}
func (*cacheFailureEnforcer) RecheckServer(string)           {}
func (*cacheFailureEnforcer) DisconnectUser(string, string)  {}

type cacheFailurePlan struct{}

func (cacheFailurePlan) HasWork() bool { return true }

type noWorkPresencePlan struct{}

func (noWorkPresencePlan) HasWork() bool { return false }

func TestPresenceReconciliationHelpersSkipNoWork(t *testing.T) {
	recheck := &captureFailureRecheck{}
	h := &Handler{presenceRecheck: recheck}
	h.presenceExecute(noWorkPresencePlan{})
	h.presenceAbandon(noWorkPresencePlan{}, "test")
	require.Zero(t, recheck.commits)
	require.Empty(t, recheck.abandons)
}

func TestInsertTransferRecordReturnsServerLockError(t *testing.T) {
	wantErr := errors.New("server lock failed")
	trace := []string{}
	db := sql.OpenDB(ambiguousConnector{trace: &trace, queryErr: wantErr})
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	h := &Handler{db: db, log: logger.New("ownership-test")}
	testContext, ctx, recorder := ownershipRaceContext()
	err := h.insertTransferRecord(ctx, testContext, &transferRecord{
		id: uuid.NewString(), serverID: uuid.NewString(), fromUserID: uuid.NewString(),
		toUserID: uuid.NewString(), reversalToken: uuid.NewString(),
		requestedAt: time.Now(), expiresAt: time.Now().Add(time.Hour),
	})

	require.ErrorIs(t, err, wantErr)
	require.Equal(t, 500, recorder.Code)
}

type blockingCacheHook struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (h *blockingCacheHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *blockingCacheHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *blockingCacheHook) ProcessHook(_ redis.ProcessHook) redis.ProcessHook {
	return func(context.Context, redis.Cmder) error {
		h.once.Do(func() {
			close(h.started)
			<-h.release
		})
		return errors.New("cache invalidation blocked")
	}
}

func TestReconcileOwnershipPostCommitExecutesPresenceBeforeCacheUnblocks(t *testing.T) {
	hook := &blockingCacheHook{started: make(chan struct{}), release: make(chan struct{})}
	rdb := redistest.Client(t)
	rdb.AddHook(hook)
	presence := &captureFailureRecheck{executeCh: make(chan struct{}, 1)}
	h := &Handler{
		cache: rbac.NewPermissionCache(rdb), presenceRecheck: presence, log: logger.New("ownership-test"),
	}
	done := make(chan struct{})
	go func() {
		h.reconcileOwnershipPostCommit("server", "from", "to", captureFailurePlan{})
		close(done)
	}()

	select {
	case <-hook.started:
	case <-time.After(time.Second):
		t.Fatal("cache invalidation did not start")
	}
	select {
	case <-presence.executeCh:
		close(hook.release)
	default:
		close(hook.release)
		<-done
		t.Fatal("presence reconciliation waited for cache invalidation")
	}
	<-done
}

type cacheFailurePresence struct{ executes int }

func (*cacheFailurePresence) PrepareCapture(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	return cacheFailurePlan{}, nil
}
func (*cacheFailurePresence) PrepareCaptureStrict(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	return cacheFailurePlan{}, nil
}
func (*cacheFailurePresence) CaptureVisibility(context.Context, *sql.Tx, rbac.PresenceRecheckPlan) error {
	return nil
}
func (p *cacheFailurePresence) Execute(rbac.PresenceRecheckPlan)       { p.executes++ }
func (*cacheFailurePresence) Abandon(rbac.PresenceRecheckPlan, string) {}

var (
	errAmbiguousCommit = errors.New("ambiguous commit")
	errRollbackFailure = errors.New("rollback failed")
)

type ambiguousConnector struct {
	trace       *[]string
	queryErr    error
	ownerID     string
	queryID     string
	rollbackErr error
	onCommit    func()
	commitErr   error
	commitOK    bool
	execRows    int64
	auditCalled *bool
	auditErr    *error
	auditUntil  *time.Time
}

func (c ambiguousConnector) Connect(context.Context) (driver.Conn, error) {
	return ambiguousConn(c), nil
}
func (c ambiguousConnector) Driver() driver.Driver            { return c }
func (c ambiguousConnector) Open(string) (driver.Conn, error) { return c.Connect(context.Background()) }

type ambiguousConn struct {
	trace       *[]string
	queryErr    error
	ownerID     string
	queryID     string
	rollbackErr error
	onCommit    func()
	commitErr   error
	commitOK    bool
	execRows    int64
	auditCalled *bool
	auditErr    *error
	auditUntil  *time.Time
}

func (c ambiguousConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (c ambiguousConn) Close() error { return nil }
func (c ambiguousConn) Begin() (driver.Tx, error) {
	return ambiguousTx{trace: c.trace, rollbackErr: c.rollbackErr, onCommit: c.onCommit, commitErr: c.commitErr, commitOK: c.commitOK}, nil
}
func (c ambiguousConn) ExecContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	query = strings.ToLower(query)
	if strings.Contains(query, "insert into audit_log") {
		*c.auditCalled = true
		*c.auditErr = ctx.Err()
		if deadline, ok := ctx.Deadline(); ok {
			*c.auditUntil = deadline
		}
	}
	step := "write"
	if strings.Contains(query, "pg_advisory_xact_lock") {
		step = "lock"
	}
	*c.trace = append(*c.trace, step)
	return driver.RowsAffected(c.execRows), nil
}
func (c ambiguousConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	*c.trace = append(*c.trace, "server-row")
	value := uuid.NewString()
	lowerQuery := strings.ToLower(query)
	if strings.Contains(lowerQuery, "select owner_id") && c.ownerID != "" {
		value = c.ownerID
	}
	if strings.Contains(lowerQuery, "select id") && c.queryID != "" {
		value = c.queryID
	}
	return &ambiguousRows{value: value}, nil
}

type ambiguousRows struct {
	read  bool
	value string
}

func (*ambiguousRows) Columns() []string { return []string{"id"} }
func (*ambiguousRows) Close() error      { return nil }
func (r *ambiguousRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	dest[0] = r.value
	return nil
}

type ambiguousTx struct {
	trace       *[]string
	rollbackErr error
	onCommit    func()
	commitErr   error
	commitOK    bool
}

func (t ambiguousTx) Commit() error {
	*t.trace = append(*t.trace, "commit")
	if t.onCommit != nil {
		t.onCommit()
	}
	if t.commitOK {
		return nil
	}
	if t.commitErr != nil {
		return t.commitErr
	}
	return errAmbiguousCommit
}
func (t ambiguousTx) Rollback() error { return t.rollbackErr }

func TestExecuteTransfer_AuditsAfterCommitWhenRequestContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trace := []string{}
	auditCalled := false
	var auditCtxErr error
	var auditDeadline time.Time
	db := sql.OpenDB(ambiguousConnector{
		trace:       &trace,
		execRows:    1,
		commitOK:    true,
		commitErr:   nil,
		onCommit:    cancel,
		auditCalled: &auditCalled,
		auditErr:    &auditCtxErr,
		auditUntil:  &auditDeadline,
	})
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	h := &Handler{
		db:    db,
		hub:   websocket.NewHub(nil, nil),
		audit: rbac.NewAuditWriter(db, logger.New("ownership-test")),
		log:   logger.New("ownership-test"),
	}

	require.NoError(t, h.executeTransfer(ctx, uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.True(t, auditCalled, "post-commit ownership audit must use a live context")
	require.NoError(t, auditCtxErr, "post-commit ownership audit must not inherit the canceled request context")
	require.WithinDuration(t, time.Now().Add(5*time.Second), auditDeadline, 500*time.Millisecond)
}

func TestWithOwnershipCapture_PrepareFailureDoesNotOpenOrWriteTransaction(t *testing.T) {
	wantErr := errors.New("capture unavailable")
	recheck := &captureFailureRecheck{strictErr: wantErr}
	h := &Handler{db: nil, log: logger.New("ownership-test")}
	h.SetPresenceRecheck(recheck)

	_, changed, err := h.withOwnershipCapture(context.Background(), "00000000-0000-0000-0000-000000000001", func(context.Context, *sql.Tx) (bool, error) {
		t.Fatalf("ownership write reached after capture failure")
		return false, nil
	})
	require.ErrorIs(t, err, wantErr)
	require.False(t, changed)
	require.Equal(t, 1, recheck.strictUsed)
	require.Zero(t, recheck.prepared)
	require.Zero(t, recheck.visibility)
	require.Zero(t, recheck.commits)
	require.Empty(t, recheck.abandons)
}

func TestCancelTransferFailsClosedWhenTransactionCannotStart(t *testing.T) {
	db := sql.OpenDB(ambiguousConnector{})
	require.NoError(t, db.Close())

	h := &Handler{db: db, log: logger.New("ownership-test")}
	c, _, recorder := ownershipRaceContext()
	c.Set("user_id", uuid.NewString())
	c.AddParam("id", uuid.NewString())
	h.CancelTransfer(c)

	require.Equal(t, 500, recorder.Code)
	require.JSONEq(t, `{"error":"Failed to cancel transfer"}`, recorder.Body.String())
}

func TestReconcileOwnershipPostCommit_ContinuesAfterCacheFailure(t *testing.T) {
	redisClient := redistest.Client(t)
	require.NoError(t, redisClient.Close())
	voice := &cacheFailureEnforcer{}
	presence := &cacheFailurePresence{}
	h := &Handler{cache: rbac.NewPermissionCache(redisClient), voiceEnforcer: voice, presenceRecheck: presence, log: logger.New("ownership-test")}
	h.reconcileOwnershipPostCommit("server", "from", "to", cacheFailurePlan{})
	require.Equal(t, []string{"from", "to"}, voice.users)
	require.Equal(t, 1, presence.executes)
}

func TestWithOwnershipCapture_AmbiguousCommitAbandonsInOrder(t *testing.T) {
	trace := []string{}
	db := sql.OpenDB(ambiguousConnector{trace: &trace})
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	presence := &captureFailureRecheck{trace: &trace}
	h := &Handler{db: db, log: logger.New("ownership-test"), presenceRecheck: presence}
	_, changed, err := h.withOwnershipCapture(context.Background(), uuid.NewString(), func(ctx context.Context, tx *sql.Tx) (bool, error) {
		_, err := tx.ExecContext(ctx, "UPDATE ownership_transfers SET status = 'completed'")
		require.NoError(t, err)
		return true, nil
	})
	require.ErrorIs(t, err, errAmbiguousCommit)
	require.False(t, changed)
	require.Equal(t, []string{"lock", "capture", "server-row", "write", "commit"}, trace)
	require.Equal(t, []string{"ambiguous_commit"}, presence.abandons)
	require.Zero(t, presence.commits)
}

func TestClassifyReversalCaptureLimitPreservesRollbackFailure(t *testing.T) {
	trace := []string{}
	serverID := uuid.NewString()
	transferID := uuid.NewString()
	toUserID := uuid.NewString()
	db := sql.OpenDB(ambiguousConnector{
		trace:       &trace,
		ownerID:     toUserID,
		queryID:     transferID,
		rollbackErr: errRollbackFailure,
	})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	h := &Handler{db: db, log: logger.New("ownership-test")}

	err := h.classifyReversalCaptureLimit(context.Background(), &reversalRecord{
		transferID: transferID,
		serverID:   serverID,
		fromUserID: uuid.NewString(),
		toUserID:   toUserID,
	})
	require.ErrorIs(t, err, errRollbackFailure)
}

func TestWithOwnershipCapture_RollbackFailureIsReturned(t *testing.T) {
	t.Run("after unchanged write", func(t *testing.T) {
		trace := []string{}
		db := sql.OpenDB(ambiguousConnector{trace: &trace, rollbackErr: errRollbackFailure})
		t.Cleanup(func() {
			require.NoError(t, db.Close())
		})
		h := &Handler{db: db, log: logger.New("ownership-test")}

		_, changed, err := h.withOwnershipCapture(context.Background(), uuid.NewString(), func(context.Context, *sql.Tx) (bool, error) {
			return false, nil
		})

		require.ErrorIs(t, err, errRollbackFailure)
		require.False(t, changed)
		require.Equal(t, []string{"lock", "server-row"}, trace)
	})

	t.Run("joined with write failure", func(t *testing.T) {
		trace := []string{}
		db := sql.OpenDB(ambiguousConnector{trace: &trace, rollbackErr: errRollbackFailure})
		t.Cleanup(func() {
			require.NoError(t, db.Close())
		})
		h := &Handler{db: db, log: logger.New("ownership-test")}
		writeErr := errors.New("ownership write failed")

		_, changed, err := h.withOwnershipCapture(context.Background(), uuid.NewString(), func(context.Context, *sql.Tx) (bool, error) {
			return false, writeErr
		})

		require.ErrorIs(t, err, writeErr)
		require.ErrorIs(t, err, errRollbackFailure)
		require.False(t, changed)
		require.Equal(t, []string{"lock", "server-row"}, trace)
	})
}

func TestExecuteTransfer_StaleClaimReturnsAlreadyCompleted(t *testing.T) {
	trace := []string{}
	db := sql.OpenDB(ambiguousConnector{trace: &trace})
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	h := &Handler{db: db, log: logger.New("ownership-test")}

	err := h.executeTransfer(
		context.Background(),
		uuid.NewString(),
		uuid.NewString(),
		uuid.NewString(),
		uuid.NewString(),
	)

	require.ErrorIs(t, err, errTransferAlreadyCompleted)
	require.Equal(t, []string{"lock", "server-row", "write"}, trace)
}

func TestCompleteExpiredTransfers_QueryFailureIsLogged(t *testing.T) {
	trace := []string{}
	db := sql.OpenDB(ambiguousConnector{trace: &trace})
	require.NoError(t, db.Close())
	var logs bytes.Buffer
	h := &Handler{db: db, log: logger.NewWithWriter(&logs)}

	h.CompleteExpiredTransfers(context.Background())

	require.Contains(t, logs.String(), "failure_class=ownership_expiry_query")
}
