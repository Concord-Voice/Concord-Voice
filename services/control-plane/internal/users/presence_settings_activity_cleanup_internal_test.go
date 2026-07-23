package users

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type replayActivitySettingsSuppressor struct {
	mu           sync.Mutex
	calls        int
	err          error
	hook         func()
	settingsHook func(context.Context)
	accountCalls int
	accountErr   error
	accountHook  func()
}

func (s *replayActivitySettingsSuppressor) SuppressAllActivityAlreadyGated(
	_ context.Context,
	_ uuid.UUID,
) error {
	if s.accountHook != nil {
		s.accountHook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountCalls++
	return s.accountErr
}

func (s *replayActivitySettingsSuppressor) ApplySettingsSuppressionAlreadyGated(
	ctx context.Context,
	_ uuid.UUID,
	_ presence.ActivityPolicySettings,
	_ presence.ActivityPolicySettings,
) error {
	if s.hook != nil {
		s.hook()
	}
	if s.settingsHook != nil {
		s.settingsHook(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.err
}

func (s *replayActivitySettingsSuppressor) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *replayActivitySettingsSuppressor) accountCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accountCalls
}

func TestActivityTierFromPersisted(t *testing.T) {
	for value, want := range map[int]presence.Tier{
		0: presence.TierOff,
		1: presence.TierFriends,
		2: presence.TierServers,
	} {
		tier, err := activityTierFromPersisted(value)
		require.NoError(t, err)
		assert.Equal(t, want, tier)
	}
	for _, value := range []int{-1, 3} {
		_, err := activityTierFromPersisted(value)
		require.Error(t, err)
	}
}

func TestActivitySettingsCleanupAdvisoryKeyIsNamespacedAndDeterministic(t *testing.T) {
	userID := uuid.New()
	key, err := activitySettingsCleanupAdvisoryKey(userID)
	require.NoError(t, err)
	repeated, err := activitySettingsCleanupAdvisoryKey(userID)
	require.NoError(t, err)
	other, err := activitySettingsCleanupAdvisoryKey(uuid.New())
	require.NoError(t, err)

	assert.Equal(t, key, repeated)
	assert.NotEqual(t, key, other)
	_, err = activitySettingsCleanupAdvisoryKey(uuid.Nil)
	assert.Error(t, err)
}

func TestActivitySettingsCleanupEvidenceRejectsMalformedRecords(t *testing.T) {
	for _, raw := range []string{
		`not-json`,
		`{"version":2,"before":{},"after":{}}`,
		`{"version":1,"before":{"server_voice_tier":3},"after":{}}`,
		`{"version":1,"before":{},"after":{},"unexpected":true}`,
		`{"version":1,"before":{},"after":{}}`,
	} {
		_, _, _, err := decodeActivitySettingsCleanupEvidence([]byte(raw))
		assert.ErrorIs(t, err, errInvalidActivityCleanupEvidence, raw)
	}
}

func TestResumePendingActivitySettingsCleanupClassifiesDatabaseResourceErrorsAsRetryable(t *testing.T) {
	db, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, db.Close())
	handler := &Handler{db: db}

	_, err = handler.resumePendingActivitySettingsCleanup(context.Background(), uuid.New())

	var writerErr *presenceWriterFailure
	require.ErrorAs(t, err, &writerErr)
	assert.Equal(t, 503, writerErr.status)
	assert.Equal(t, "activity_cleanup", writerErr.class)
	assert.False(t, errors.Is(err, errInvalidActivityCleanupEvidence))
}

func TestJoinActivitySettingsCleanupRollbackPreservesWriterFailureAndRollbackCause(t *testing.T) {
	primaryCause := errors.New("cleanup primary failure")
	rollbackCause := errors.New("cleanup rollback failure")
	returnErr := error(&presenceWriterFailure{
		status: 503, class: "activity_cleanup", cause: primaryCause,
	})

	joinActivitySettingsCleanupRollback(func() error { return rollbackCause }, &returnErr)

	assert.ErrorIs(t, returnErr, primaryCause)
	assert.ErrorIs(t, returnErr, rollbackCause)
	var writerErr *presenceWriterFailure
	require.ErrorAs(t, returnErr, &writerErr)
	assert.Equal(t, 503, writerErr.status)
	assert.Equal(t, "activity_cleanup", writerErr.class)

	previous := returnErr
	joinActivitySettingsCleanupRollback(func() error { return sql.ErrTxDone }, &returnErr)
	assert.Equal(t, previous, returnErr, "an already completed transaction is not a rollback failure")
}

func TestDeleteActivitySettingsCleanupCannotDeleteSuccessorMarker(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	successorID := uuid.New()
	before := presence.ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: presence.TierFriends,
	}
	after := presence.ActivityPolicySettings{
		MasterEnabled: false, ServerVoiceTier: presence.TierOff,
	}
	evidence, err := encodeActivitySettingsCleanupEvidence(before, after)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO activity_settings_pending_cleanups (
			user_id, operation_id, evidence
		) VALUES ($1, $2, $3)
	`, userID, successorID, string(evidence))
	require.NoError(t, err)

	require.NoError(t, deleteActivitySettingsCleanup(
		context.Background(), db, userID, uuid.New(),
	))

	var retained uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT operation_id FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, userID).Scan(&retained))
	assert.Equal(t, successorID, retained)
}

func TestResumePendingActivitySettingsCleanupReplaysFinalizationWithoutSuppression(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	before := presence.ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: presence.TierFriends,
	}
	after := presence.ActivityPolicySettings{
		MasterEnabled: false, ServerVoiceTier: presence.TierOff,
	}
	evidence, err := encodeActivitySettingsCleanupEvidence(before, after)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO activity_settings_pending_cleanups (
			user_id, operation_id, evidence
		) VALUES ($1, $2, $3)
	`, userID, uuid.New(), string(evidence))
	require.NoError(t, err)
	require.NoError(t, execInternalActivityCleanupSQL(db, `
		CREATE OR REPLACE FUNCTION reject_internal_activity_cleanup_delete_for_test()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'forced activity cleanup finalization rollback';
		END
		$$
	`))
	require.NoError(t, execInternalActivityCleanupSQL(db, `
		CREATE TRIGGER reject_internal_activity_cleanup_delete_for_test
		BEFORE DELETE ON activity_settings_pending_cleanups
		FOR EACH ROW EXECUTE FUNCTION reject_internal_activity_cleanup_delete_for_test()
	`))
	t.Cleanup(func() {
		assert.NoError(t, execInternalActivityCleanupSQL(db, `
			DROP TRIGGER IF EXISTS reject_internal_activity_cleanup_delete_for_test
			ON activity_settings_pending_cleanups
		`))
		assert.NoError(t, execInternalActivityCleanupSQL(db,
			`DROP FUNCTION IF EXISTS reject_internal_activity_cleanup_delete_for_test()`))
	})

	suppressor := &replayActivitySettingsSuppressor{}
	handler := &Handler{db: db, activitySuppressor: suppressor}
	resumed, err := handler.resumePendingActivitySettingsCleanup(
		context.Background(), userID,
	)
	require.True(t, resumed)
	require.Error(t, err)
	require.Equal(t, 1, suppressor.callCount())

	var suppressionCompleted bool
	require.NoError(t, db.QueryRow(`
		SELECT COALESCE((evidence ->> 'suppressed')::boolean, false)
		FROM activity_settings_pending_cleanups
		WHERE user_id = $1
	`, userID).Scan(&suppressionCompleted))
	assert.True(t, suppressionCompleted)

	require.NoError(t, execInternalActivityCleanupSQL(db, `
		DROP TRIGGER reject_internal_activity_cleanup_delete_for_test
		ON activity_settings_pending_cleanups
	`))
	suppressor.mu.Lock()
	suppressor.err = errors.New("settings evidence unavailable after prior suppression")
	suppressor.mu.Unlock()
	resumed, err = handler.resumePendingActivitySettingsCleanup(
		context.Background(), userID,
	)

	require.NoError(t, err)
	assert.True(t, resumed)
	assert.Equal(t, 1, suppressor.callCount())
	var pending int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, userID).Scan(&pending))
	assert.Zero(t, pending)
}

func execInternalActivityCleanupSQL(db *sql.DB, statement string) error {
	_, err := db.Exec(statement)
	return err
}

var (
	errAmbiguousActivityCleanupReceiptCommit = errors.New("ambiguous activity cleanup receipt commit")
	errReusedActivityCleanupReceiptContext   = errors.New("activity cleanup receipt recovery reused failed context")
)

type activityCleanupDriverState struct {
	mu sync.Mutex

	userID            uuid.UUID
	activityOperation uuid.UUID
	activityEvidence  string
	activityExists    bool

	now                    time.Time
	settingsVersion        int64
	settingsOperation      uuid.UUID
	settingsMasterEnabled  bool
	settingsServerTier     int64
	settingsServerDetails  bool
	settingsPrivateTier    int64
	settingsPrivateDetails bool
	settingsCustomTier     int64
	settingsCustomText     string
	pendingOperation       uuid.UUID
	pendingPriorVersion    int64
	pendingReconcileAfter  time.Time
	customPending          bool

	ambiguousReceiptMode string
	receiptCommitCount   int
	receiptUpdateCount   int
	receiptDeleteCount   int
	successorOperation   uuid.UUID
	successorEvidence    string
	failedReceiptContext context.Context
	beginContexts        []context.Context
}

func newActivityCleanupDriverState(t *testing.T) *activityCleanupDriverState {
	t.Helper()
	userID := uuid.New()
	before := presence.ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: presence.TierFriends,
		ServerVoiceShowDetails: true, PrivateCallTier: presence.TierOff,
	}
	after := before
	after.ServerVoiceShowDetails = false
	evidence, err := encodeActivitySettingsCleanupEvidence(before, after)
	require.NoError(t, err)
	now := time.Date(2026, 7, 22, 18, 0, 0, 0, time.UTC)
	return &activityCleanupDriverState{
		userID: userID, activityOperation: uuid.New(),
		activityEvidence: string(evidence), activityExists: true,
		now: now, settingsVersion: 2, settingsOperation: uuid.New(),
		settingsMasterEnabled: true, settingsServerTier: int64(presence.TierFriends),
		settingsServerDetails: true, settingsPrivateTier: int64(presence.TierOff),
		settingsPrivateDetails: false, settingsCustomTier: 1,
		settingsCustomText: "original custom status",
		pendingOperation:   uuid.New(), pendingPriorVersion: 1,
		pendingReconcileAfter: now.Add(time.Minute),
	}
}

func (s *activityCleanupDriverState) activitySnapshot() (bool, uuid.UUID, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activityExists, s.activityOperation, s.activityEvidence
}

func (s *activityCleanupDriverState) contextSnapshot() []context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]context.Context(nil), s.beginContexts...)
}

type activityCleanupTestConnector struct {
	state *activityCleanupDriverState
}

func (c *activityCleanupTestConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &activityCleanupTestConn{state: c.state}, nil
}

func (c *activityCleanupTestConnector) Driver() driver.Driver {
	return activityCleanupTestDriver{}
}

type activityCleanupTestDriver struct{}

func (activityCleanupTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("activity cleanup test driver requires its connector")
}

type activityCleanupTestConn struct {
	state *activityCleanupDriverState
	mu    sync.Mutex
	tx    *activityCleanupTestTx
}

func (c *activityCleanupTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("activity cleanup test driver does not prepare statements")
}

func (c *activityCleanupTestConn) Close() error { return nil }

func (c *activityCleanupTestConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *activityCleanupTestConn) BeginTx(
	ctx context.Context,
	_ driver.TxOptions,
) (driver.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.state.mu.Lock()
	if c.state.failedReceiptContext != nil && c.state.failedReceiptContext == ctx {
		c.state.mu.Unlock()
		return nil, errReusedActivityCleanupReceiptContext
	}
	c.state.beginContexts = append(c.state.beginContexts, ctx)
	c.state.mu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tx != nil && !c.tx.done {
		return nil, errors.New("activity cleanup test transaction already active")
	}
	tx := &activityCleanupTestTx{conn: c, ctx: ctx}
	c.tx = tx
	return tx, nil
}

func (c *activityCleanupTestConn) currentTx() (*activityCleanupTestTx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tx == nil || c.tx.done {
		return nil, sql.ErrTxDone
	}
	return c.tx, nil
}

func (c *activityCleanupTestConn) ExecContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx, err := c.currentTx()
	if err != nil {
		return nil, err
	}
	switch {
	case strings.Contains(query, "pg_advisory_xact_lock"):
		return driver.RowsAffected(1), nil
	case strings.Contains(query, "UPDATE activity_settings_pending_cleanups"):
		c.state.mu.Lock()
		c.state.receiptUpdateCount++
		c.state.mu.Unlock()
		if len(args) != 3 {
			return nil, errors.New("unexpected activity cleanup receipt arguments")
		}
		operationID, parseErr := uuid.Parse(args[1].Value.(string))
		if parseErr != nil {
			return nil, parseErr
		}
		c.state.mu.Lock()
		matches := c.state.activityExists && c.state.activityOperation == operationID
		c.state.mu.Unlock()
		if !matches {
			return driver.RowsAffected(0), nil
		}
		evidence := args[2].Value.(string)
		tx.receiptEvidence = &evidence
		return driver.RowsAffected(1), nil
	case strings.Contains(query, "DELETE FROM activity_settings_pending_cleanups"):
		c.state.mu.Lock()
		c.state.receiptDeleteCount++
		c.state.mu.Unlock()
		if len(args) != 2 {
			return nil, errors.New("unexpected activity cleanup delete arguments")
		}
		operationID, parseErr := uuid.Parse(args[1].Value.(string))
		if parseErr != nil {
			return nil, parseErr
		}
		c.state.mu.Lock()
		matches := c.state.activityExists && c.state.activityOperation == operationID
		c.state.mu.Unlock()
		if !matches {
			return driver.RowsAffected(0), nil
		}
		tx.deleteActivity = true
		return driver.RowsAffected(1), nil
	default:
		return nil, errors.New("unexpected activity cleanup test ExecContext query")
	}
}

func (c *activityCleanupTestConn) QueryContext(
	ctx context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx, err := c.currentTx()
	if err != nil {
		return nil, err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	switch {
	case strings.Contains(query, "FROM activity_settings_pending_cleanups"):
		if !c.state.activityExists || tx.deleteActivity {
			return activityCleanupRows([]string{"operation_id", "evidence"}), nil
		}
		evidence := c.state.activityEvidence
		if tx.receiptEvidence != nil {
			evidence = *tx.receiptEvidence
		}
		return activityCleanupRows(
			[]string{"operation_id", "evidence"},
			c.state.activityOperation.String(), evidence,
		), nil
	case strings.Contains(query, "SELECT id FROM users"):
		return activityCleanupRows([]string{"id"}, c.state.userID.String()), nil
	case strings.Contains(query, "FROM user_presence_settings"):
		return activityCleanupRows(
			[]string{
				"presence_settings_version", "presence_settings_operation_id",
				"master_enabled", "server_voice_tier", "server_voice_show_details",
				"private_call_tier", "private_call_show_details", "custom_text_tier",
				"custom_text", "custom_text_emoji",
			},
			c.state.settingsVersion, c.state.settingsOperation.String(),
			c.state.settingsMasterEnabled, c.state.settingsServerTier,
			c.state.settingsServerDetails, c.state.settingsPrivateTier,
			c.state.settingsPrivateDetails, c.state.settingsCustomTier,
			c.state.settingsCustomText, nil,
		), nil
	case strings.Contains(query, "FROM presence_settings_pending_operations"):
		if !c.state.customPending {
			return activityCleanupRows(
				[]string{"operation_id", "prior_settings_version", "reconcile_after"},
			), nil
		}
		return activityCleanupRows(
			[]string{"operation_id", "prior_settings_version", "reconcile_after"},
			c.state.pendingOperation.String(), c.state.pendingPriorVersion,
			c.state.pendingReconcileAfter,
		), nil
	case strings.Contains(query, "SELECT clock_timestamp()"):
		return activityCleanupRows([]string{"clock_timestamp"}, c.state.now), nil
	default:
		return nil, errors.New("unexpected activity cleanup test QueryContext query")
	}
}

type activityCleanupTestTx struct {
	conn            *activityCleanupTestConn
	ctx             context.Context
	done            bool
	receiptEvidence *string
	deleteActivity  bool
}

func (tx *activityCleanupTestTx) Commit() error {
	tx.conn.mu.Lock()
	if tx.done {
		tx.conn.mu.Unlock()
		return sql.ErrTxDone
	}
	tx.done = true
	tx.conn.tx = nil
	tx.conn.mu.Unlock()

	tx.conn.state.mu.Lock()
	defer tx.conn.state.mu.Unlock()
	if tx.receiptEvidence != nil {
		tx.conn.state.receiptCommitCount++
		if tx.conn.state.receiptCommitCount == 1 &&
			tx.conn.state.ambiguousReceiptMode != "" {
			switch tx.conn.state.ambiguousReceiptMode {
			case "applied":
				tx.conn.state.activityEvidence = *tx.receiptEvidence
			case "superseded":
				tx.conn.state.activityOperation = tx.conn.state.successorOperation
				tx.conn.state.activityEvidence = tx.conn.state.successorEvidence
			}
			tx.conn.state.failedReceiptContext = tx.ctx
			return errAmbiguousActivityCleanupReceiptCommit
		}
		tx.conn.state.activityEvidence = *tx.receiptEvidence
	}
	if tx.deleteActivity {
		tx.conn.state.activityExists = false
	}
	return nil
}

func (tx *activityCleanupTestTx) Rollback() error {
	tx.conn.mu.Lock()
	defer tx.conn.mu.Unlock()
	if tx.done {
		return sql.ErrTxDone
	}
	tx.done = true
	tx.conn.tx = nil
	return nil
}

type activityCleanupTestRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

func activityCleanupRows(columns []string, values ...driver.Value) driver.Rows {
	return &activityCleanupTestRows{columns: columns, values: values}
}

func (r *activityCleanupTestRows) Columns() []string { return r.columns }
func (r *activityCleanupTestRows) Close() error      { return nil }

func (r *activityCleanupTestRows) Next(dest []driver.Value) error {
	if r.done || len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values)
	r.done = true
	return nil
}

func openActivityCleanupTestDB(t *testing.T, state *activityCleanupDriverState) *sql.DB {
	t.Helper()
	db := sql.OpenDB(&activityCleanupTestConnector{state: state})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return db
}

func TestResumePendingActivitySettingsCleanupRecoversReceiptAfterSuppressionContextCancellation(
	t *testing.T,
) {
	state := newActivityCleanupDriverState(t)
	db := openActivityCleanupTestDB(t, state)
	var cancelSuppression context.CancelFunc
	var phaseContexts []context.Context
	suppressor := &replayActivitySettingsSuppressor{settingsHook: func(ctx context.Context) {
		cancelSuppression()
		<-ctx.Done()
	}}
	handler := &Handler{db: db, activitySuppressor: suppressor}
	handler.activityCleanupPhaseContextFactory = func(
		requestCtx context.Context,
	) (context.Context, context.CancelFunc) {
		phaseCtx, cancelPhase := context.WithTimeout(
			context.WithoutCancel(requestCtx), time.Hour,
		)
		phaseContexts = append(phaseContexts, phaseCtx)
		if len(phaseContexts) == 1 {
			cancelSuppression = cancelPhase
		}
		return phaseCtx, cancelPhase
	}

	firstResumed, firstErr := handler.resumePendingActivitySettingsCleanup(
		context.Background(), state.userID,
	)
	suppressor.mu.Lock()
	suppressor.settingsHook = nil
	suppressor.mu.Unlock()
	retryResumed, retryErr := handler.resumePendingActivitySettingsCleanup(
		context.Background(), state.userID,
	)

	assert.True(t, firstResumed)
	assert.False(t, retryResumed, "the first call should persist and finalize the receipt")
	assert.NoError(t, firstErr)
	assert.NoError(t, retryErr)
	assert.Equal(t, 1, suppressor.callCount(),
		"a fresh post-suppression receipt phase must prevent replay")
	require.GreaterOrEqual(t, len(phaseContexts), 3)
	assert.ErrorIs(t, phaseContexts[0].Err(), context.Canceled)
	assert.False(t, phaseContexts[0] == phaseContexts[1],
		"receipt persistence must not reuse the canceled suppression context")
	assert.False(t, phaseContexts[1] == phaseContexts[2],
		"finalization must receive another fresh phase context")
	exists, _, _ := state.activitySnapshot()
	assert.False(t, exists)
}

func TestResumePendingActivitySettingsCleanupRecoversAmbiguousReceiptCommit(t *testing.T) {
	for _, mode := range []string{"applied", "rolled_back"} {
		t.Run(mode, func(t *testing.T) {
			state := newActivityCleanupDriverState(t)
			state.ambiguousReceiptMode = mode
			db := openActivityCleanupTestDB(t, state)
			suppressor := &replayActivitySettingsSuppressor{}
			handler := &Handler{db: db, activitySuppressor: suppressor}

			resumed, err := handler.resumePendingActivitySettingsCleanup(
				context.Background(), state.userID,
			)

			assert.True(t, resumed)
			assert.NoError(t, err)
			assert.Equal(t, 1, suppressor.callCount())
			exists, _, _ := state.activitySnapshot()
			assert.False(t, exists)
			contexts := state.contextSnapshot()
			if assert.GreaterOrEqual(t, len(contexts), 4) {
				assert.NotEqual(t, contexts[0], contexts[1],
					"receipt persistence needs a fresh suppression-success budget")
				assert.NotEqual(t, contexts[1], contexts[2],
					"ambiguous receipt recovery needs another fresh budget")
				assert.NotEqual(t, contexts[2], contexts[3],
					"finalization needs its own fresh budget")
			}
		})
	}
}

func TestResumePendingActivitySettingsCleanupAmbiguousReceiptPreservesSuccessor(t *testing.T) {
	state := newActivityCleanupDriverState(t)
	successorOperation := uuid.New()
	successorEvidence, err := encodeActivitySettingsCleanupEvidence(
		presence.ActivityPolicySettings{MasterEnabled: false},
		presence.ActivityPolicySettings{MasterEnabled: true},
	)
	require.NoError(t, err)
	state.ambiguousReceiptMode = "superseded"
	state.successorOperation = successorOperation
	state.successorEvidence = string(successorEvidence)
	db := openActivityCleanupTestDB(t, state)
	suppressor := &replayActivitySettingsSuppressor{}
	handler := &Handler{db: db, activitySuppressor: suppressor}

	resumed, resumeErr := handler.resumePendingActivitySettingsCleanup(
		context.Background(), state.userID,
	)

	assert.True(t, resumed)
	require.ErrorIs(t, resumeErr, errActivitySettingsCleanupPending)
	assert.Equal(t, 1, suppressor.callCount())
	exists, operationID, evidence := state.activitySnapshot()
	assert.True(t, exists)
	assert.Equal(t, successorOperation, operationID)
	assert.Equal(t, string(successorEvidence), evidence)
	state.mu.Lock()
	updateCount, deleteCount := state.receiptUpdateCount, state.receiptDeleteCount
	state.mu.Unlock()
	assert.Equal(t, 1, updateCount, "recovery must not update successor evidence")
	assert.Zero(t, deleteCount, "recovery must not delete the successor marker")
}

type activityCleanupReadyDelivery struct{}

func (activityCleanupReadyDelivery) DeliverCustomText(
	_ context.Context,
	plan presencehistory.DeliveryPlan,
) (presencehistory.DeliveryAck, error) {
	return presencehistory.DeliveryAck{OperationID: plan.OperationID}, nil
}

func TestUpdatePresenceSettingsRunsActivityCleanupBeforeCustomStatusReadiness(t *testing.T) {
	for _, test := range []struct {
		name         string
		bindDelivery bool
		wantRetry    bool
	}{
		{name: "unexpired custom status marker", bindDelivery: true, wantRetry: true},
		{name: "custom status delivery unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := newActivityCleanupDriverState(t)
			state.customPending = true
			db := openActivityCleanupTestDB(t, state)
			service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
			if test.bindDelivery {
				require.NoError(t, service.BindDelivery(activityCleanupReadyDelivery{}))
			}
			suppressor := &replayActivitySettingsSuppressor{}
			handler := &Handler{
				db: db, log: logger.NewWithWriter(io.Discard),
				presenceHistory: service, activitySuppressor: suppressor,
			}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Set("user_id", state.userID.String())
			ctx.Request = httptest.NewRequest(
				http.MethodPatch,
				"/api/v1/users/me/presence-settings",
				bytes.NewBufferString(`{"custom_text":"must not be applied"}`),
			)
			ctx.Request.Header.Set("Content-Type", "application/json")

			handler.UpdatePresenceSettings(ctx)

			assert.Equal(t, http.StatusServiceUnavailable, response.Code)
			assert.Equal(t, 1, suppressor.callCount())
			exists, _, _ := state.activitySnapshot()
			assert.False(t, exists, "older activity cleanup must finish before readiness returns")
			state.mu.Lock()
			storedText := state.settingsCustomText
			state.mu.Unlock()
			assert.Equal(t, "original custom status", storedText)
			if test.wantRetry {
				assert.NotEmpty(t, response.Header().Get("Retry-After"))
			}
		})
	}
}
