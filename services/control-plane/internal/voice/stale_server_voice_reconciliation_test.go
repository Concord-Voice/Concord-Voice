package voice_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errStaleDiscovery = errors.New("stale discovery failed")
	errStaleIteration = errors.New("stale iteration failed")
	errStaleBegin     = errors.New("stale begin failed")
	errStaleLock      = errors.New("stale lock failed")
	errStaleReread    = errors.New("stale reread failed")
	errStaleDelete    = errors.New("stale delete failed")
	errStaleAffected  = errors.New("stale rows affected failed")
	errStaleCommit    = errors.New("stale commit failed")
	errStaleBusy      = errors.New("stale lifecycle lock busy")
)

const staleVoiceCandidateChannel = "11111111-1111-1111-1111-111111111111"
const staleVoiceCandidateUser = "22222222-2222-2222-2222-222222222222"
const staleVoiceCandidateServer = "33333333-3333-3333-3333-333333333333"
const staleVoiceSecondChannel = "44444444-4444-4444-4444-444444444444"
const staleVoiceSecondUser = "55555555-5555-5555-5555-555555555555"
const staleVoiceSecondServer = "66666666-6666-6666-6666-666666666666"

var (
	staleVoiceObservedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	staleVoiceCutoff     = time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)
)

type staleVoiceDriver struct{}

func (staleVoiceDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use OpenDB") }

type staleVoiceConnector struct {
	scenario        string
	grantProbeCount *int
	afterCommit     func()
	serverID        string
	state           *staleVoiceFixtureState
}

type staleVoiceFixtureState struct {
	deleted            bool
	successorCommitted bool
}

func (c staleVoiceConnector) Connect(context.Context) (driver.Conn, error) {
	return &staleVoiceConn{scenario: c.scenario, grantProbeCount: c.grantProbeCount, afterCommit: c.afterCommit, serverID: c.serverID, state: c.state}, nil
}
func (c staleVoiceConnector) Driver() driver.Driver { return staleVoiceDriver{} }

type staleVoiceConn struct {
	scenario        string
	grantProbeCount *int
	lockAttempts    int
	afterCommit     func()
	serverID        string
	state           *staleVoiceFixtureState
}

func (*staleVoiceConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (*staleVoiceConn) Close() error                { return nil }
func (c *staleVoiceConn) Begin() (driver.Tx, error) { return c.begin() }
func (c *staleVoiceConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.begin()
}
func (c *staleVoiceConn) begin() (driver.Tx, error) {
	if c.scenario == "begin_error" {
		return nil, errStaleBegin
	}
	return &staleVoiceTx{conn: c}, nil
}

func (c *staleVoiceConn) QueryContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	if strings.Contains(query, "FROM voice_participants AS participant") {
		candidateServer := staleVoiceCandidateServer
		if c.serverID != "" {
			candidateServer = c.serverID
		}
		if c.scenario == "limit_zero" && args[1].Value != int64(1) {
			return nil, errors.New("zero limit was not normalized")
		}
		if c.scenario == "limit_large" && args[1].Value != int64(1000) {
			return nil, errors.New("large limit was not clamped")
		}
		switch c.scenario {
		case "discovery_error":
			return nil, errStaleDiscovery
		case "scan_error":
			return staleVoiceRows(3, [][]driver.Value{{"not-a-uuid", staleVoiceCandidateUser, staleVoiceCandidateServer}}, nil), nil
		case "iteration_error":
			return staleVoiceRows(3, [][]driver.Value{{staleVoiceCandidateChannel, staleVoiceCandidateUser, staleVoiceCandidateServer}}, errStaleIteration), nil
		case "limit_zero", "limit_large":
			return staleVoiceRows(3, nil, nil), nil
		case "busy_two":
			return staleVoiceRows(3, [][]driver.Value{
				{staleVoiceCandidateChannel, staleVoiceCandidateUser, staleVoiceCandidateServer},
				{staleVoiceSecondChannel, staleVoiceSecondUser, staleVoiceSecondServer},
			}, nil), nil
		default:
			return staleVoiceRows(3, [][]driver.Value{{staleVoiceCandidateChannel, staleVoiceCandidateUser, candidateServer}}, nil), nil
		}
	}
	if strings.Contains(query, "SELECT EXISTS(") {
		if c.grantProbeCount != nil {
			*c.grantProbeCount = *c.grantProbeCount + 1
		}
		return staleVoiceRows(1, [][]driver.Value{{false}}, nil), nil
	}
	if strings.Contains(query, "SELECT 1") && strings.Contains(query, "FROM voice_participants") {
		validFirst := len(args) == 2 && args[0].Value == staleVoiceCandidateChannel && args[1].Value == staleVoiceCandidateUser
		validSecond := c.scenario == "busy_two" && len(args) == 2 && args[0].Value == staleVoiceSecondChannel && args[1].Value == staleVoiceSecondUser
		if !validFirst && !validSecond {
			return nil, errors.New("unexpected stale voice handoff recheck arguments")
		}
		if c.state != nil && c.state.deleted && c.state.successorCommitted {
			return staleVoiceRows(1, [][]driver.Value{{int64(1)}}, nil), nil
		}
		return staleVoiceRows(1, nil, nil), nil
	}
	if strings.Contains(query, "SELECT lifecycle_observed_at") {
		if len(args) != 3 {
			return nil, errors.New("unexpected stale voice reread arguments")
		}
		validFirst := args[0].Value == staleVoiceCandidateChannel && args[1].Value == staleVoiceCandidateUser
		validSecond := c.scenario == "busy_two" && args[0].Value == staleVoiceSecondChannel && args[1].Value == staleVoiceSecondUser
		if (!validFirst && !validSecond) || args[2].Value != int64(presence.ActivityStateTTL/time.Second) {
			return nil, errors.New("unexpected stale voice reread arguments")
		}
		if c.scenario == "busy_two" && validFirst {
			return nil, errors.New("busy candidate must not be reread")
		}
		switch c.scenario {
		case "reread_error":
			return nil, errStaleReread
		case "reread_no_rows":
			return staleVoiceRows(2, nil, nil), nil
		case "fresh":
			return staleVoiceRows(2, [][]driver.Value{{staleVoiceCutoff, staleVoiceObservedAt}}, nil), nil
		default:
			return staleVoiceRows(2, [][]driver.Value{{staleVoiceObservedAt, staleVoiceCutoff}}, nil), nil
		}
	}
	if strings.Contains(query, "pg_try_advisory_xact_lock") {
		if c.scenario == "lock_error" {
			return nil, errStaleLock
		}
		c.lockAttempts++
		locked := true
		if c.scenario == "busy_two" && c.lockAttempts == 1 {
			locked = false
		}
		return staleVoiceRows(1, [][]driver.Value{{locked}}, nil), nil
	}
	return nil, errors.New("unexpected stale voice query")
}

func (c *staleVoiceConn) ExecContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Result, error) {
	if strings.Contains(query, "pg_advisory_xact_lock") {
		if c.scenario == "lock_error" {
			return nil, errStaleLock
		}
		if c.scenario == "busy_two" {
			c.lockAttempts++
			if c.lockAttempts == 1 {
				return nil, errStaleBusy
			}
		}
		return staleVoiceResult{rows: 0}, nil
	}
	if strings.Contains(query, "DELETE FROM voice_participants") {
		if len(args) != 3 || args[2].Value != int64(presence.ActivityStateTTL/time.Second) {
			return nil, errors.New("unexpected stale voice delete arguments")
		}
		validFirst := args[0].Value == staleVoiceCandidateChannel && args[1].Value == staleVoiceCandidateUser
		validSecond := c.scenario == "busy_two" && args[0].Value == staleVoiceSecondChannel && args[1].Value == staleVoiceSecondUser
		if !validFirst && !validSecond {
			return nil, errors.New("unexpected stale voice delete candidate")
		}
		if c.scenario == "busy_two" && validFirst {
			return nil, errors.New("busy candidate must not be deleted")
		}
		switch c.scenario {
		case "reread_no_rows", "fresh":
			return nil, errors.New("unexpected stale voice delete for no-op candidate")
		case "delete_error":
			return nil, errStaleDelete
		case "affected_error":
			return staleVoiceResult{err: errStaleAffected}, nil
		case "affected_many":
			return staleVoiceResult{rows: 2}, nil
		case "commit_control":
			if c.state != nil {
				c.state.deleted = true
			}
			return staleVoiceResult{rows: 1}, nil
		case "busy_two":
			return staleVoiceResult{rows: 1}, nil
		}
		return staleVoiceResult{rows: 0}, nil
	}
	return nil, errors.New("unexpected stale voice exec")
}

var _ driver.QueryerContext = (*staleVoiceConn)(nil)
var _ driver.ExecerContext = (*staleVoiceConn)(nil)
var _ driver.ConnBeginTx = (*staleVoiceConn)(nil)

type staleVoiceTx struct{ conn *staleVoiceConn }

func (tx *staleVoiceTx) Commit() error {
	switch tx.conn.scenario {
	case "commit_error":
		return errStaleCommit
	}
	if tx.conn.afterCommit != nil {
		tx.conn.afterCommit()
	}
	return nil
}
func (*staleVoiceTx) Rollback() error { return nil }

type staleVoiceRowSet struct {
	columns []string
	values  [][]driver.Value
	index   int
	err     error
}

func staleVoiceRows(columnCount int, values [][]driver.Value, err error) driver.Rows {
	columns := []string{"channel_id", "user_id", "server_id"}
	if columnCount == 1 {
		columns = []string{"exists"}
	}
	if columnCount == 2 {
		columns = []string{"lifecycle_observed_at", "cutoff"}
	}
	return &staleVoiceRowSet{columns: columns, values: values, err: err}
}
func (r *staleVoiceRowSet) Columns() []string { return r.columns }
func (*staleVoiceRowSet) Close() error        { return nil }
func (r *staleVoiceRowSet) Next(values []driver.Value) error {
	if r.index == len(r.values) {
		if r.err != nil {
			err := r.err
			r.err = nil
			return err
		}
		return io.EOF
	}
	copy(values, r.values[r.index])
	r.index++
	return nil
}

type staleVoiceResult struct {
	rows int64
	err  error
}

func (r staleVoiceResult) LastInsertId() (int64, error) { return 0, nil }
func (r staleVoiceResult) RowsAffected() (int64, error) { return r.rows, r.err }

func openStaleVoiceDB(t *testing.T, scenario string) *sql.DB {
	t.Helper()
	db := sql.OpenDB(staleVoiceConnector{scenario: scenario})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func openStaleVoiceDBWithGrantProbeCounter(t *testing.T, scenario string, count *int) *sql.DB {
	t.Helper()
	db := sql.OpenDB(staleVoiceConnector{scenario: scenario, grantProbeCount: count})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func openStaleVoiceDBWithCommitHookForServer(
	t *testing.T, scenario, serverID string, afterCommit func(), state *staleVoiceFixtureState,
) *sql.DB {
	t.Helper()
	db := sql.OpenDB(staleVoiceConnector{
		scenario: scenario, serverID: serverID, afterCommit: afterCommit, state: state,
	})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func TestReconcileStaleServerVoiceParticipants_GuardsAndFailurePaths(t *testing.T) {
	for _, test := range []struct {
		name      string
		scenario  string
		limit     int
		wantError error
		contains  string
	}{
		{name: "normalizes zero limit", scenario: "limit_zero", limit: 0},
		{name: "clamps large limit", scenario: "limit_large", limit: 1 << 20},
		{name: "discovery error", scenario: "discovery_error", limit: 1, wantError: errStaleDiscovery},
		{name: "scan error", scenario: "scan_error", limit: 1, contains: "scan stale server voice participant"},
		{name: "iteration error", scenario: "iteration_error", limit: 1, wantError: errStaleIteration},
		{name: "begin error", scenario: "begin_error", limit: 1, wantError: errStaleBegin},
		{name: "lock error", scenario: "lock_error", limit: 1, wantError: errStaleLock},
		{name: "reread error", scenario: "reread_error", limit: 1, wantError: errStaleReread},
		{name: "missing row is a no-op", scenario: "reread_no_rows", limit: 1},
		{name: "fresh row is a no-op", scenario: "fresh", limit: 1},
		{name: "delete error", scenario: "delete_error", limit: 1, wantError: errStaleDelete},
		{name: "rows affected error", scenario: "affected_error", limit: 1, wantError: errStaleAffected},
		{name: "invalid rows affected", scenario: "affected_many", limit: 1, contains: "affected 2 rows"},
		{name: "commit error", scenario: "commit_error", limit: 1, wantError: errStaleCommit},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openStaleVoiceDB(t, test.scenario)
			sub := voice.NewNATSSubscriber(db, nil, nil, nil, nil, nil, nil)
			sub.CompleteServerVoiceCleanupGraceForTest()
			removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), test.limit)
			if test.wantError == nil && test.contains == "" {
				require.NoError(t, err)
				assert.Zero(t, removed)
				return
			}
			require.Error(t, err)
			if test.contains != "" {
				assert.Contains(t, err.Error(), test.contains)
			} else {
				assert.ErrorIs(t, err, test.wantError)
			}
		})
	}
}

func TestReconcileStaleServerVoiceParticipants_ArmsStartupGraceBeforeDiscovery(t *testing.T) {
	// #2907: migration 000132 can stamp pre-existing rows at transaction start;
	// startup must give live rooms a fresh-heartbeat window before the first destructive sweep.
	db := openStaleVoiceDB(t, "discovery_error")
	sub := voice.NewNATSSubscriber(db, nil, nil, nil, nil, nil, nil)
	firstCallBefore := time.Now()

	for _, name := range []string{"first cleanup call", "immediate repeated cleanup call"} {
		t.Run(name, func(t *testing.T) {
			removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
			require.NoError(t, err, "startup grace must skip discovery while the lease is fresh")
			assert.Zero(t, removed)
		})
	}
	firstCallAfter := time.Now()
	readyAt := sub.CompleteServerVoiceCleanupGraceForTest()
	assert.False(t, readyAt.Before(firstCallBefore.Add(presence.ActivityStateTTL)),
		"startup grace deadline must not precede the first cleanup call bound")
	assert.False(t, readyAt.After(firstCallAfter.Add(presence.ActivityStateTTL)),
		"startup grace deadline must not exceed the first cleanup call bound")
	for _, name := range []string{"post-grace cleanup call", "post-grace repeated call"} {
		t.Run(name, func(t *testing.T) {
			_, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
			require.ErrorIs(t, err, errStaleDiscovery,
				"once grace expires, discovery must run and the guard must not re-arm")
		})
	}
}

func TestReconcileStaleServerVoiceParticipants_SkipsBusyCandidate(t *testing.T) {
	// #2907: a busy first candidate must not block a later free candidate in the ordered sweep.
	db := openStaleVoiceDB(t, "busy_two")
	sub := voice.NewNATSSubscriber(
		db, logger.NewWithWriter(io.Discard), websocket.NewHub(nil, nil), nil, nil, nil, nil,
	)
	sub.CompleteServerVoiceCleanupGraceForTest()

	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 2)
	require.NoError(t, err, "a busy candidate must be skipped, not surfaced as a sweep failure")
	assert.Equal(t, 1, removed, "the later free candidate must still be cleaned")
}

func TestReconcileStaleServerVoiceParticipants_DoesNotProbeTemporaryGrant(t *testing.T) {
	// #2907: stale participant convergence does not own the temporary-grant backstop.
	var grantProbeCount int
	db := openStaleVoiceDBWithGrantProbeCounter(t, "commit_control", &grantProbeCount)
	sub := voice.NewNATSSubscriber(
		db, logger.NewWithWriter(io.Discard), websocket.NewHub(nil, nil), nil, nil, nil, nil,
	)
	sub.CompleteServerVoiceCleanupGraceForTest()

	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "the stale participant must still be removed")
	assert.Zero(t, grantProbeCount,
		"stale participant convergence must not probe or revoke temporary grants")
}

func TestStaleVoiceTemporaryGrantProbeCounterControl(t *testing.T) {
	var grantProbeCount int
	db := openStaleVoiceDBWithGrantProbeCounter(t, "probe_control", &grantProbeCount)
	rows, err := db.QueryContext(context.Background(), "SELECT EXISTS(")
	require.NoError(t, err)
	require.NoError(t, rows.Close())
	assert.Equal(t, 1, grantProbeCount,
		"the scripted driver must expose temporary-grant probe calls")
}

func TestStaleVoiceDiscoveryErrorFakeIsObservable(t *testing.T) {
	db := openStaleVoiceDB(t, "discovery_error")
	rows, err := db.QueryContext(
		context.Background(),
		"SELECT * FROM voice_participants AS participant",
	)
	if rows != nil {
		t.Cleanup(func() { require.NoError(t, rows.Close()) })
	}
	require.ErrorIs(t, err, errStaleDiscovery,
		"the scripted discovery failure must be observable independently of the grace assertion")
}

func TestReconcileStaleServerVoiceParticipants_DoesNotLeaveSuccessorStale(t *testing.T) {
	// #2907: a successor may commit and announce after cleanup's commit but before
	// cleanup's terminal handoff; that live successor must not receive a stale left.
	for _, test := range []struct {
		name          string
		successorJoin bool
		wantLeft      bool
	}{
		{name: "successor joined after cleanup commit", successorJoin: true},
		{name: "ordinary expired cleanup announces leave", wantLeft: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			owner := ts.CreateTestUser(t, "stale-wire-owner")
			viewer := ts.CreateTestUser(t, "stale-wire-viewer")
			serverID := ts.CreateTestServer(t, owner.ID, "stale-wire-server")
			ts.AddMemberToServer(t, serverID, viewer.ID, "member")
			hub, baseURL := newVoiceReplicaHub(t, ts)
			conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
			require.NoError(t, conn.WriteJSON(map[string]interface{}{
				"type": "subscribe_server",
				"data": map[string]interface{}{"server_id": serverID},
			}))
			synchronizeVoiceWireClient(t, conn)

			state := &staleVoiceFixtureState{}
			var afterCommit func()
			if test.successorJoin {
				afterCommit = func() {
					state.successorCommitted = true
					hub.BroadcastToServer(uuid.MustParse(serverID), websocket.OutgoingMessage{
						Type: "voice_state_update",
						Data: map[string]interface{}{
							"channel_id": staleVoiceCandidateChannel,
							"user_id":    staleVoiceCandidateUser,
							"action":     "joined",
							"server_id":  serverID,
						},
					})
				}
			}
			db := openStaleVoiceDBWithCommitHookForServer(t, "commit_control", serverID, afterCommit, state)
			sub := voice.NewNATSSubscriber(
				db, logger.NewWithWriter(io.Discard), hub, nil, nil, nil, nil,
			)
			sub.CompleteServerVoiceCleanupGraceForTest()
			removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
			require.NoError(t, err)
			require.Equal(t, 1, removed)
			require.True(t, state.deleted, "the expired participant must be absent after cleanup commit")
			require.Equal(t, test.successorJoin, state.successorCommitted,
				"the successor state must be committed before cleanup's terminal handoff")

			hub.BroadcastToServer(uuid.MustParse(serverID), websocket.OutgoingMessage{
				Type: "stale_cleanup_sentinel",
				Data: map[string]interface{}{},
			})
			sawJoined := false
			sawLeft := false
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
			for {
				var envelope voiceWireEnvelope
				require.NoError(t, conn.ReadJSON(&envelope))
				if envelope.Type == "voice_state_update" {
					switch envelope.Data["action"] {
					case "joined":
						sawJoined = true
					case "left":
						sawLeft = true
					}
				}
				if envelope.Type == "stale_cleanup_sentinel" {
					break
				}
			}
			assert.Equal(t, test.successorJoin, sawJoined,
				"the scripted commit boundary must expose the successor join before the terminal handoff")
			assert.Equal(t, test.wantLeft, sawLeft,
				"stale cleanup must not enqueue a later left after a same-channel successor joins")
		})
	}
}

// Regression for #2907: a dropped terminal event leaves a durable participant
// row. The lease pass removes only a bounded, deterministic oldest batch.
func TestReconcileStaleServerVoiceParticipants_ProcessesBoundedLeaseBatch(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	owner := ts.CreateTestUser(t, "reconcile-lease-owner")
	server := ts.CreateTestServer(t, owner.ID, "reconcile-lease-server")
	channel := ts.CreateVoiceChannel(t, server, "reconcile-lease-channel")
	users := []testhelpers.TestUser{
		ts.CreateTestUser(t, "reconcile-lease-user-a"),
		ts.CreateTestUser(t, "reconcile-lease-user-b"),
		ts.CreateTestUser(t, "reconcile-lease-user-c"),
	}
	for _, user := range users {
		ts.AddMemberToServer(t, server, user.ID, "member")
		insertVoiceParticipant(t, ts.DB, channel, user.ID)
	}

	observedAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := ts.DB.Exec(`UPDATE voice_participants SET lifecycle_observed_at = $1 WHERE channel_id = $2`, observedAt, channel)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE voice_participants SET lifecycle_event_at = lifecycle_event_at WHERE channel_id = $1`, channel)
	require.NoError(t, err)
	var replayObservedAt time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT lifecycle_observed_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, channel, users[0].ID).Scan(&replayObservedAt))
	require.Equal(t, observedAt, replayObservedAt, "an exact lifecycle replay must not extend the lease")

	sort.Slice(users, func(left, right int) bool { return users[left].ID < users[right].ID })
	removed, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 2)
	require.NoError(t, err)
	require.Zero(t, removed, "startup grace must preserve real expired rows for one lease")
	for _, user := range users {
		assert.True(t, voiceParticipantExists(t, ts.DB, channel, user.ID))
	}
	sub.CompleteServerVoiceCleanupGraceForTest()
	removed, err = sub.ReconcileStaleServerVoiceParticipants(context.Background(), 2)
	require.NoError(t, err)
	require.Equal(t, 2, removed)
	for _, user := range users[:2] {
		assert.False(t, voiceParticipantExists(t, ts.DB, channel, user.ID))
	}
	assert.True(t, voiceParticipantExists(t, ts.DB, channel, users[2].ID))

	removed, err = sub.ReconcileStaleServerVoiceParticipants(context.Background(), 2)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.False(t, voiceParticipantExists(t, ts.DB, channel, users[2].ID))
}
