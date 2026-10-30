package loginfamily_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// A deterministic short caller budget: cancellation follows two actual SQL
// commits, including retained units. No production hook, wall-clock performance
// threshold, or replacement context inside SweepRetiredEvidence is involved.
type retentionBudget struct {
	sync.Mutex
	remaining int
	cancel    context.CancelFunc
}
type retentionBudgetConnector struct {
	driver.Connector
	budget *retentionBudget
}

func (c retentionBudgetConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return retentionBudgetConn{Conn: conn, budget: c.budget}, nil
}

type retentionBudgetConn struct {
	driver.Conn
	budget *retentionBudget
}

// Preserve pq's direct execution path: GC intentionally executes its two
// transaction setup statements in one simple query. Falling back to Prepare
// would reject that SQL before the cancellation budget was ever consumed.
func (c retentionBudgetConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c retentionBudgetConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c retentionBudgetConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return retentionBudgetTx{Tx: tx, budget: c.budget}, nil
}
func (c retentionBudgetConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

type retentionBudgetTx struct {
	driver.Tx
	budget *retentionBudget
}

func (tx retentionBudgetTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	tx.budget.Lock()
	defer tx.budget.Unlock()
	tx.budget.remaining--
	if tx.budget.remaining == 0 {
		tx.budget.cancel()
	}
	return nil
}

func TestRetentionRepeatedShortBudgetsProgressAllStreamsAndRevisitLockedPrefix(t *testing.T) {
	db := database(t)
	retentionTransportCleanup(t, db)
	const population = 12
	operations := make([]uuid.UUID, 0, population)
	families := make([]uuid.UUID, 0, population)
	rooms := make([]uuid.UUID, 0, population)
	connections := make([]uuid.UUID, 0, 2*population)
	for range population {
		user := testdb.CreateUser(t, db)
		_, op := retentionCohort(t, db, user, 1, false)
		retentionComplete(t, db, op)
		operations = append(operations, op)
		standalone := testdb.CreateUser(t, db)
		identity := newFamily(t, db, standalone, time.Now().Add(-time.Hour).Truncate(time.Second))
		var family uuid.UUID
		require.NoError(t, db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, identity.SID).Scan(&family))
		families = append(families, family)
		closureUser := testdb.CreateUser(t, db)
		_, conn, room := retentionBoundClosures(t, db, closureUser, true)
		connections = append(connections, conn)
		rooms = append(rooms, room)
		_, conn = retentionUnregisteredClosure(t, db)
		connections = append(connections, conn)
	}
	for _, ids := range [][]uuid.UUID{operations, families, rooms, connections} {
		sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	}
	// Hold the first discoverable operation and explicit expired-family unit.
	// The room key is independently contended; GC must still acquire its
	// actual parent connection key before attempting that room key.
	blocker := transaction(t, db)
	var observed int
	require.NoError(t, blocker.QueryRow(`SELECT 1 FROM login_family_revocations WHERE operation_id=$1 FOR UPDATE`, operations[0]).Scan(&observed))
	require.NoError(t, blocker.QueryRow(`SELECT 1 FROM login_families WHERE family_id=$1 FOR UPDATE`, families[0]).Scan(&observed))
	var roomParent uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT connection_generation FROM voice_auth_room_closures WHERE session_generation=$1`, rooms[0]).Scan(&roomParent))
	_, err := blocker.Exec(`SELECT login_family_generation_lock('connection',$1)`, connections[0])
	require.NoError(t, err)
	_, err = blocker.Exec(`SELECT login_family_generation_lock('room',$1)`, rooms[0])
	require.NoError(t, err)
	connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	budget := &retentionBudget{}
	sweepDB := sql.OpenDB(retentionBudgetConnector{Connector: connector, budget: budget})
	t.Cleanup(func() { require.NoError(t, sweepDB.Close()) })
	exhausted := 0
	var committed lf.GCResult
	sweep := func() lf.GCResult {
		bounded, cancel := context.WithCancel(t.Context())
		defer cancel()
		budget.Lock()
		budget.remaining = 2
		budget.cancel = cancel
		budget.Unlock()
		got, sweepErr := lf.SweepRetiredEvidence(bounded, sweepDB, 1)
		if bounded.Err() != nil {
			exhausted++
			require.ErrorIs(t, sweepErr, context.Canceled, "cause=%#v", errors.Unwrap(sweepErr))
			reason(t, sweepErr, lf.ErrUnavailable, lf.ReasonSQLUnavailable)
			require.True(t, got.Incomplete)
		} else {
			require.NoError(t, sweepErr, "context=%v cause=%#v", bounded.Err(), errors.Unwrap(sweepErr))
		}
		committed.DeletedOperations += got.DeletedOperations
		committed.DeletedFamilies += got.DeletedFamilies
		committed.DeletedSIDs += got.DeletedSIDs
		committed.DeletedRoomClosures += got.DeletedRoomClosures
		committed.DeletedConnectionClosures += got.DeletedConnectionClosures
		return got
	}
	remaining := func(table, column string, ids []uuid.UUID) int {
		total := 0
		for _, id := range ids {
			total += int(count(t, db, `SELECT count(*) FROM `+table+` WHERE `+column+`=$1`, id))
		}
		return total
	}
	// Each candidate stream has more than two eligible units. A sweep which
	// restarted its stream/key cursors on every exhausted budget would starve.
	progressed := false
	for range 240 {
		sweep()
		ops := remaining("login_family_revocations", "operation_id", operations)
		fams := remaining("login_families", "family_id", families)
		rms := remaining("voice_auth_room_closures", "session_generation", rooms)
		conns := remaining("voice_auth_connection_closures", "connection_generation", connections)
		retainedConnections := 1
		if roomParent != connections[0] {
			retainedConnections++
		}
		// If the lowest connection is another room's parent, that room is also
		// protected. Count exact retained children rather than assuming random order.
		retainedRooms := int(count(t, db, `SELECT count(*) FROM voice_auth_room_closures WHERE session_generation=$1 OR connection_generation=$2`, rooms[0], connections[0]))
		if ops == 1 && fams == 1 && rms == retainedRooms && conns == retainedConnections {
			progressed = true
			break
		}
	}
	require.True(t, progressed, "later eligible work must progress in every candidate stream")
	require.Greater(t, exhausted, 1, "the fixture must span repeated exhausted caller budgets")
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM login_family_revocations WHERE operation_id=$1`, operations[0]))
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM login_families WHERE family_id=$1`, families[0]))
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM voice_auth_room_closures WHERE session_generation=$1`, rooms[0]))
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM voice_auth_connection_closures WHERE connection_generation=$1`, connections[0]))
	require.NoError(t, blocker.Commit())
	revisited := false
	for range 240 {
		sweep()
		if remaining("login_family_revocations", "operation_id", operations) == 0 && remaining("login_families", "family_id", families) == 0 && remaining("voice_auth_room_closures", "session_generation", rooms) == 0 && remaining("voice_auth_connection_closures", "connection_generation", connections) == 0 {
			revisited = true
			break
		}
	}
	require.True(t, revisited, "continuations must wrap and revisit each previously skipped low key after unlock")
	require.EqualValues(t, population, committed.DeletedOperations)
	require.EqualValues(t, 2*population, committed.DeletedFamilies)
	require.EqualValues(t, 2*population, committed.DeletedSIDs)
	require.EqualValues(t, population, committed.DeletedRoomClosures)
	require.EqualValues(t, 2*population, committed.DeletedConnectionClosures)
	// The bound-closure helpers intentionally leave live access families. Their
	// usable SIDs must survive this workload rather than be purged for zero counts.
	require.EqualValues(t, population, count(t, db, `SELECT count(*) FROM login_family_sids WHERE access_expires_at>clock_timestamp()`))
}
