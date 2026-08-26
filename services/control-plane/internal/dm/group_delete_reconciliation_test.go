package dm

import (
	"context"
	"database/sql"
	"go/build"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// senderGate is a real buffer-1 channel gate, matching presencehistory's shape:
// acquire on the way in, release only on the way out, no timeout. Re-entry from
// inside the closure blocks forever, which is exactly the failure the
// already-gated entry points exist to avoid — so the fixture must not soften it
// into a mutex or a passthrough.
type senderGate struct{ slot chan struct{} }

func newSenderGate() *senderGate { return &senderGate{slot: make(chan struct{}, 1)} }

func (g *senderGate) WithSenders(ctx context.Context, _ []uuid.UUID, work func() error) error {
	select {
	case g.slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-g.slot }()
	return work()
}

// absentStateReader routes every plan to the resolver's ordinary clear arm: the
// Redis key is gone, and a plan cut moments ago is younger than the level arm,
// so viewers may still hold a copy and one proportional clear frame is owed.
type absentStateReader struct{}

func (absentStateReader) GetWithLease(
	context.Context, uuid.UUID, presence.Category,
) (presence.ActivityState, bool, error) {
	return presence.ActivityState{}, false, nil
}

type noopGenerationDeleter struct{}

func (noopGenerationDeleter) CompareAndDelete(
	context.Context, uuid.UUID, presence.Category, uuid.UUID, int64,
) (bool, error) {
	return true, nil
}

// observedClear records what the DURABLE state looked like at the instant the
// terminal fired. That is the assertion the whole task turns on: by the time a
// clear frame is delivered the plan must already be committed and the
// conversation must already be gone, which is only true if capture and deletion
// shared one transaction.
type observedClear struct {
	subject           uuid.UUID
	category          presence.Category
	plansVisible      int
	conversationsLeft int
}

type recordingDeliverer struct {
	t      *testing.T
	db     *sql.DB
	convID string

	mu          sync.Mutex
	clears      []observedClear
	disconnects int
}

func (d *recordingDeliverer) ClearSenderActiveCategory(subject uuid.UUID, category presence.Category) {
	d.t.Helper()
	var plans, conversations int
	// Read on the plain pool, from outside any transaction the rail holds: this
	// is what makes "committed" mean committed.
	if err := d.db.QueryRow(
		`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1 AND category = $2`,
		subject, string(category),
	).Scan(&plans); err != nil {
		d.t.Errorf("probe plan durability: %v", err)
	}
	if err := d.db.QueryRow(
		`SELECT count(*) FROM dm_conversations WHERE id = $1`, d.convID,
	).Scan(&conversations); err != nil {
		d.t.Errorf("probe conversation deletion: %v", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.clears = append(d.clears, observedClear{
		subject: subject, category: category,
		plansVisible: plans, conversationsLeft: conversations,
	})
}

func (d *recordingDeliverer) DisconnectAllRichPresenceClients(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disconnects++
	return nil
}

func (d *recordingDeliverer) subjectsCleared() []uuid.UUID {
	d.mu.Lock()
	defer d.mu.Unlock()
	subjects := make([]uuid.UUID, 0, len(d.clears))
	for _, clear := range d.clears {
		subjects = append(subjects, clear.subject)
	}
	return subjects
}

func newDMHandlerWithRail(t *testing.T, db *sql.DB, convID string) (*Handler, *recordingDeliverer) {
	t.Helper()
	deliverer := &recordingDeliverer{t: t, db: db, convID: convID}
	gate := newSenderGate()
	rail := activepresence.NewRail(db, gate,
		activepresence.NewReconciler(db, gate, absentStateReader{}, noopGenerationDeleter{}, deliverer, nil),
		nil)
	handler := NewHandler(HandlerDeps{
		DB:          db,
		Log:         logger.NewWithWriter(io.Discard),
		ActivePlans: rail,
	})
	return handler, deliverer
}

func seedGroupCallWithParticipants(t *testing.T, db *sql.DB, voiceCount int) (string, []uuid.UUID) {
	t.Helper()
	creator := dbtest.CreateUser(t, db)
	var convID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, name, created_by)
		VALUES (true, 'plan fixture', $1) RETURNING id`, creator).Scan(&convID))
	_, err := db.Exec(
		`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, creator)
	require.NoError(t, err)

	participants := make([]uuid.UUID, 0, voiceCount)
	for i := 0; i < voiceCount; i++ {
		participants = append(participants, joinExtraVoiceParticipant(t, db, convID))
	}
	return convID, participants
}

func joinExtraVoiceParticipant(t *testing.T, db *sql.DB, convID string) uuid.UUID {
	t.Helper()
	user := dbtest.CreateUser(t, db)
	_, err := db.Exec(
		`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, user)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, user)
	require.NoError(t, err)
	return user
}

func countRows(t *testing.T, db *sql.DB, query string, args ...interface{}) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(query, args...).Scan(&count))
	return count
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type deleteGroupResponseRecorder struct {
	ctx      *gin.Context
	recorder *httptest.ResponseRecorder
}

func newDeleteGroupResponseRecorder(t *testing.T) *deleteGroupResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	return &deleteGroupResponseRecorder{ctx: ctx, recorder: recorder}
}

func (r *deleteGroupResponseRecorder) status() int { return r.recorder.Code }

// backendPID names the connection the caller's transaction is running on, so a
// lock sample can be scoped to it. Sampling every backend would pick up the
// fixture's own connections and the sibling test packages sharing this database.
func backendPID(t *testing.T, tx *sql.Tx) int {
	t.Helper()
	var pid int
	require.NoError(t, tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	return pid
}

// sampleRelationLocks reads what ONE backend currently holds, from a second
// connection. A grep cannot verify lock order — an acquisition inside a
// conditional branch is invisible to a static scan — so the assertion has to
// observe the live lock table while the transaction is still open.
func sampleRelationLocks(t *testing.T, probe *sql.DB, pid int) []string {
	t.Helper()
	rows, err := probe.Query(`
		SELECT DISTINCT relation::regclass::text
		FROM pg_locks
		WHERE pid = $1 AND locktype = 'relation' AND relation IS NOT NULL
		  AND relation::regclass::text IN ('users', 'dm_conversations')`, pid)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var relations []string
	for rows.Next() {
		var relation string
		require.NoError(t, rows.Scan(&relation))
		relations = append(relations, relation)
	}
	require.NoError(t, rows.Err())
	return relations
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// The plan must be durable BEFORE the evidence is destroyed. The terminal probe
// is what proves it: at delivery time the plan row is committed and the
// conversation is already gone, which cannot both be true unless capture and
// deletion rode the same transaction.
func TestDeleteGroupDataCapturesAPlanPerVoiceParticipant(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, participants := seedGroupCallWithParticipants(t, db, 3)
	handler, deliverer := newDMHandlerWithRail(t, db, convID)

	require.NoError(t, handler.deleteGroupData(context.Background(), convID))

	require.ElementsMatch(t, participants, deliverer.subjectsCleared(),
		"every participant in the active call is owed exactly one clear frame")
	require.Zero(t, deliverer.disconnects,
		"the ordinary arm is proportional, never a fleet-wide disconnect")
	for _, clear := range deliverer.clears {
		require.Equal(t, presence.CategoryPrivateCall, clear.category)
		require.Equal(t, 1, clear.plansVisible,
			"the obligation must be committed before its clear is delivered")
		require.Zero(t, clear.conversationsLeft,
			"the conversation must already be deleted when the clear is delivered")
	}
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM presence_active_pending_plans`),
		"a delivered plan is acknowledged")
}

// users must be locked BEFORE dm_conversations (#2447). Sampled from a second
// connection while the transaction is open, scoped to the transaction's own
// backend.
func TestDeleteGroupDataLocksUsersBeforeConversations(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, _ := seedGroupCallWithParticipants(t, db, 2)
	handler, _ := newDMHandlerWithRail(t, db, convID)

	observed := make(chan []string, 1)
	handler.afterUsersLockHook = func(tx *sql.Tx) {
		observed <- sampleRelationLocks(t, db, backendPID(t, tx))
	}

	require.NoError(t, handler.deleteGroupData(context.Background(), convID))

	select {
	case relations := <-observed:
		require.Contains(t, relations, "users")
		require.NotContains(t, relations, "dm_conversations",
			"dm_conversations must not be locked before users")
	case <-time.After(10 * time.Second):
		t.Fatal("the users-lock hook never fired")
	}
}

// A participant joining the call between the candidate read and the conversation
// lock is a conflict, not a server fault. Fail closed, delete nothing, and never
// lock the extra users row mid-transaction.
func TestDeleteGroupDataFailsClosedOnCandidateDrift(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, _ := seedGroupCallWithParticipants(t, db, 2)
	handler, deliverer := newDMHandlerWithRail(t, db, convID)

	var once sync.Once
	handler.afterCandidateReadHook = func() {
		once.Do(func() { joinExtraVoiceParticipant(t, db, convID) })
	}

	err := handler.deleteGroupData(context.Background(), convID)
	require.ErrorIs(t, err, errCandidateSetDrifted)

	require.Equal(t, 1,
		countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID),
		"nothing may be deleted when the candidate set drifts")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM presence_active_pending_plans`),
		"a rolled-back mutation captures no obligation")
	require.Empty(t, deliverer.subjectsCleared())
}

// The drift arm maps to 409, never 500: the caller may retry the same request.
func TestDeleteGroupDriftMapsToConflict(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	handler, _ := newDMHandlerWithRail(t, db, uuid.NewString())

	recorder := newDeleteGroupResponseRecorder(t)
	handler.respondDeleteGroupError(recorder.ctx, errCandidateSetDrifted)
	require.Equal(t, 409, recorder.status())

	recorder = newDeleteGroupResponseRecorder(t)
	handler.respondDeleteGroupError(recorder.ctx, activepresence.ErrDeliveryIncomplete)
	require.Equal(t, 503, recorder.status(),
		"a committed deletion whose delivery failed must not invite a destructive retry")

	recorder = newDeleteGroupResponseRecorder(t)
	handler.respondDeleteGroupError(recorder.ctx, sql.ErrConnDone)
	require.Equal(t, 500, recorder.status())
}

// A conversation with no active call takes no plan and no gate.
func TestDeleteGroupDataWithoutAnActiveCallCapturesNothing(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, _ := seedGroupCallWithParticipants(t, db, 0)
	handler, deliverer := newDMHandlerWithRail(t, db, convID)

	require.NoError(t, handler.deleteGroupData(context.Background(), convID))

	require.Zero(t, countRows(t, db, `SELECT count(*) FROM presence_active_pending_plans`))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID))
	require.Empty(t, deliverer.subjectsCleared())
}

// The bound fails CLOSED. An oversized candidate set is a derivation bug, and
// deleting the conversation anyway would drop every one of those obligations
// silently — which is the pre-#2448 behaviour this task exists to remove.
func TestDeleteGroupDataFailsClosedAboveTheSubjectBound(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, _ := seedGroupCallWithParticipants(t, db, maxGroupVoiceCandidates+1)
	handler, deliverer := newDMHandlerWithRail(t, db, convID)

	err := handler.deleteGroupData(context.Background(), convID)
	require.ErrorIs(t, err, activepresence.ErrTooManySubjects)

	require.Equal(t, 1,
		countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID),
		"an oversized set must delete nothing")
	require.Empty(t, deliverer.subjectsCleared())
}

// The bound is checked BEFORE the unwired-rail branch, and only this test can
// see that. On the WIRED path the rail enforces its own identical bound, so a
// test there passes with the handler's check deleted; on an UNWIRED replica
// nothing else is looking, and without the handler's check an oversized call is
// deleted with every obligation dropped on the floor.
func TestDeleteGroupDataFailsClosedAboveTheBoundWithNoRail(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, _ := seedGroupCallWithParticipants(t, db, maxGroupVoiceCandidates+1)
	handler := NewHandler(HandlerDeps{DB: db, Log: logger.NewWithWriter(io.Discard)})
	require.False(t, handler.HasActivePlanRail())

	err := handler.deleteGroupData(context.Background(), convID)
	require.ErrorIs(t, err, activepresence.ErrTooManySubjects)
	require.Equal(t, 1,
		countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID),
		"an unwired replica must not delete an oversized call either")
}

// An unwired replica keeps the pre-#2448 behaviour: the deletion succeeds and
// presence degrades to its TTL. Without this the no-rail branch could be
// broken — or removed in favour of an unconditional rail call — with every
// other test in this file still green.
func TestDeleteGroupDataWithoutARailStillDeletes(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, _ := seedGroupCallWithParticipants(t, db, 2)
	handler := NewHandler(HandlerDeps{DB: db, Log: logger.NewWithWriter(io.Discard)})

	require.NoError(t, handler.deleteGroupData(context.Background(), convID))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM presence_active_pending_plans`))
}

// The lock-order regression, staged deterministically rather than raced.
//
// T2 holds users FOR SHARE — the lock credepoch.GuardTx takes at the top of
// updateDMMessageCiphertext — and then asks for dm_conversations. T1 is
// deleteGroupData. Under the canonical order T1 waits on users while holding
// NOTHING, so T2 sails through and both settle. Under the inverted order T1
// holds dm_conversations FOR UPDATE while waiting on users, T2 waits on
// dm_conversations, and Postgres breaks the cycle with 40P01 — a failed group
// deletion OR a failed E2EE message edit, under ordinary concurrency.
//
// WALL-CLOCK BOUNDED ON PURPOSE. `go test -race` cannot see this: it observes
// conflicting memory accesses, and a row-lock cycle is neither a data race nor,
// once the detector fires, even a hang. The bounds turn a stall into a named
// failure; do not delete them as noise.
func TestDeleteGroupDataDoesNotDeadlockAgainstMessageEdit(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, participants := seedGroupCallWithParticipants(t, db, 2)
	handler, _ := newDMHandlerWithRail(t, db, convID)
	editor := participants[0]

	ctx := context.Background()
	editTx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = editTx.Rollback() }()

	// The message-edit path's FIRST lock: users, before the conversation.
	require.NoError(t, credepoch.GuardTx(ctx, editTx, editor.String(), ""))

	deleteErr := make(chan error, 1)
	go func() { deleteErr <- handler.deleteGroupData(ctx, convID) }()

	// Both orders reach this same wait; only what T1 holds while waiting differs.
	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR NO KEY UPDATE%'
		)`).Scan(&waiting)
		return queryErr == nil && waiting
	}, 15*time.Second, 20*time.Millisecond, "group deletion never reached its users lock")

	// The message-edit path's SECOND lock. This is the edge that closes the
	// cycle when the deletion holds the conversation.
	var lockedConversationID string
	editLockErr := editTx.QueryRowContext(ctx,
		`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, convID,
	).Scan(&lockedConversationID)
	require.NotContains(t, errText(editLockErr), "deadlock detected",
		"the E2EE message edit lost a lock-order cycle against group deletion")
	require.NoError(t, editLockErr)
	require.NoError(t, editTx.Commit())

	select {
	case err := <-deleteErr:
		require.NotContains(t, errText(err), "deadlock detected",
			"group deletion lost a lock-order cycle against the E2EE message edit")
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("group deletion never completed; suspected lock-order cycle")
	}
}

// internal/dm gains exactly ONE new import for #2448 — internal/activepresence —
// and no presence package. The rail is the leaf that owns that dependency
// (spec section 7), which is why plans name their category through
// activepresence.CategoryPrivateCall rather than presence.CategoryPrivateCall.
//
// Read from go/build rather than shelled out to `go list`: same answer, no
// toolchain process, and it is scoped to the package's NON-test imports, so
// this file's own presence import is correctly invisible to it.
func TestDMPackageGainsNoPresenceImport(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	require.NoError(t, err)

	for _, imported := range pkg.Imports {
		require.NotEqual(t, "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence",
			imported, "internal/dm must reach presence only through the rail")
		require.False(t, strings.HasSuffix(imported, "/internal/presencehistory"),
			"internal/dm must not import presencehistory")
	}
	require.Contains(t, pkg.Imports,
		"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence",
		"the rail import is the seam this test bounds; without it the test is vacuous")
}
