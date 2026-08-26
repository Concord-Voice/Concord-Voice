package activepresence

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// beginTx opens a transaction whose rollback is registered up front. A test
// that trips an assertion mid-transaction would otherwise leak a FOR UPDATE row
// lock, and the fixture's own truncate cleanup would then block forever -- a
// hang instead of a diagnosable failure.
func beginTx(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func insert(t *testing.T, db *sql.DB, p Plan) {
	t.Helper()
	tx := beginTx(t, db)
	require.NoError(t, InsertPlanTx(context.Background(), tx, p))
	require.NoError(t, tx.Commit())
}

func conservativePlan(subject uuid.UUID, category presence.Category) Plan {
	return Plan{
		SubjectID:   subject,
		Category:    category,
		OperationID: uuid.New(),
		Resolution:  ResolutionConservative,
		EventAt:     time.Now(),
	}
}

// The collision ratchet. Two producers for one (subject, category) must merge
// into ONE conservative row -- monotone, dropping no obligation, because the
// clear frame is generation-agnostic and keyed on exactly that pair.
func TestInsertPlanTxRatchetsCollisionsToConservative(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)

	first := Plan{
		SubjectID: subject, Category: presence.CategoryPrivateCall,
		OperationID: uuid.New(), Resolution: ResolutionExact,
		LifecycleID: uuid.New(), EventAt: time.Now().Add(-time.Minute),
	}
	second := Plan{
		SubjectID: subject, Category: presence.CategoryPrivateCall,
		OperationID: uuid.New(), Resolution: ResolutionExact,
		LifecycleID: uuid.New(), EventAt: time.Now(),
	}
	insert(t, db, first)
	insert(t, db, second)

	var count int
	var resolution string
	var lifecycle *uuid.UUID
	var operation uuid.UUID
	var attempts int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) OVER (), resolution, scope_lifecycle_id, operation_id, attempts
		FROM presence_active_pending_plans WHERE user_id = $1`, subject,
	).Scan(&count, &resolution, &lifecycle, &operation, &attempts))

	require.Equal(t, 1, count)
	require.Equal(t, "conservative", resolution)
	require.Nil(t, lifecycle, "a merged row cannot claim exactness it cannot prove")
	require.Equal(t, second.OperationID, operation,
		"the newer operation owns the ack, so the older one's in-flight ack cannot delete the merged row")
	require.Zero(t, attempts)
}

// An exact plan keeps its evidence when nothing collides with it. Without this,
// the ratchet test alone would pass against a store that never writes 'exact'.
func TestInsertPlanTxPreservesExactEvidence(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)

	plan := Plan{
		SubjectID: subject, Category: presence.CategoryServerVoice,
		OperationID: uuid.New(), Resolution: ResolutionExact,
		LifecycleID: uuid.New(), EventAt: time.Now(),
	}
	insert(t, db, plan)

	var resolution string
	var lifecycle *uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT resolution, scope_lifecycle_id
		FROM presence_active_pending_plans WHERE user_id = $1`, subject,
	).Scan(&resolution, &lifecycle))
	require.Equal(t, "exact", resolution)
	require.NotNil(t, lifecycle)
	require.Equal(t, plan.LifecycleID, *lifecycle)
}

// A plan that violates its own invariants never reaches the database. The
// CHECK is the backstop, not the gate.
func TestInsertPlanTxRejectsInvalidPlans(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)

	broken := conservativePlan(subject, presence.CategoryPrivateCall)
	broken.Category = presence.Category("screen_share")

	tx := beginTx(t, db)
	require.ErrorIs(t, InsertPlanTx(context.Background(), tx, broken), ErrInvalidPlan)
}

// Two categories for one subject are independent obligations. A (user_id) PK
// would reproduce #2448's own founding defect one level down.
func TestInsertPlanTxKeepsCategoriesIndependent(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)

	for _, category := range []presence.Category{
		presence.CategoryPrivateCall, presence.CategoryServerVoice,
	} {
		insert(t, db, conservativePlan(subject, category))
	}

	var count int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`, subject,
	).Scan(&count))
	require.Equal(t, 2, count)
}

// Advancing reconcile_after under the row lock IS the claim. A second claimant
// must find the row not due and skip it, so no plan is delivered twice.
func TestClaimThenRecordAttemptMakesThePlanUndue(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	plan := conservativePlan(subject, presence.CategoryPrivateCall)
	insert(t, db, plan)

	due, err := DiscoverDue(context.Background(), db, maxPlanBatch)
	require.NoError(t, err)
	require.Len(t, due, 1)

	tx := beginTx(t, db)
	claimed, found, err := ClaimTx(context.Background(), tx, due[0])
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, plan.OperationID, claimed.OperationID)
	require.Equal(t, presence.CategoryPrivateCall, claimed.Category)
	require.Equal(t, ResolutionConservative, claimed.Resolution)
	require.Equal(t, uuid.Nil, claimed.LifecycleID)
	require.NoError(t, RecordAttemptTx(context.Background(), tx, claimed, FailureNone, time.Minute))
	require.NoError(t, tx.Commit())

	due, err = DiscoverDue(context.Background(), db, maxPlanBatch)
	require.NoError(t, err)
	require.Empty(t, due, "the claim must make the plan undue for the backoff window")
}

// A claim taken against a row another replica already advanced must report
// found=false. Acting on the discovery snapshot is the bug this guards.
func TestClaimTxSkipsAPlanThatIsNoLongerDue(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	plan := conservativePlan(subject, presence.CategoryServerVoice)
	insert(t, db, plan)

	key := PlanKey{SubjectID: subject, Category: presence.CategoryServerVoice}

	first := beginTx(t, db)
	claimed, found, err := ClaimTx(context.Background(), first, key)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, RecordAttemptTx(context.Background(), first, claimed, FailureNone, time.Minute))
	require.NoError(t, first.Commit())

	second := beginTx(t, db)
	_, found, err = ClaimTx(context.Background(), second, key)
	require.NoError(t, err)
	require.False(t, found, "a plan another replica claimed must not be delivered twice")
}

// A row that is simply gone -- another replica finished it -- is not an error.
func TestClaimTxReportsAMissingPlanAsNotFound(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)

	tx := beginTx(t, db)
	_, found, err := ClaimTx(context.Background(), tx,
		PlanKey{SubjectID: subject, Category: presence.CategoryPrivateCall})
	require.NoError(t, err)
	require.False(t, found)
}

// FailureNone has NO member in the failure_class vocabulary -- its String()
// renders the fail-closed "plan_invalid". Persisting that string would file a
// SUCCESSFUL reconcile as a plan_invalid failure, and the CHECK would accept
// the row happily. A success must write SQL NULL.
func TestRecordAttemptTxStoresNullForFailureNone(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	plan := conservativePlan(subject, presence.CategoryPrivateCall)
	insert(t, db, plan)

	// Prove the premise rather than assume it: the enum really does render as
	// a legal-but-wrong class.
	require.Equal(t, "plan_invalid", FailureNone.String())

	tx := beginTx(t, db)
	claimed, found, err := ClaimTx(context.Background(), tx,
		PlanKey{SubjectID: subject, Category: presence.CategoryPrivateCall})
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, RecordAttemptTx(context.Background(), tx, claimed, FailureNone, time.Minute))
	require.NoError(t, tx.Commit())

	var class *string
	require.NoError(t, db.QueryRow(
		`SELECT failure_class FROM presence_active_pending_plans WHERE user_id = $1`, subject,
	).Scan(&class))
	require.Nil(t, class, "a successful reconcile must not be recorded as a failure class")
}

// operation_id is the acknowledgement AUTHORIZATION. A plan rewritten between
// claim and ack survives the stale ack.
func TestDeletePlanTxRequiresTheMatchingOperation(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	plan := conservativePlan(subject, presence.CategoryPrivateCall)
	insert(t, db, plan)

	stale := plan
	stale.OperationID = uuid.New()

	staleTx := beginTx(t, db)
	require.NoError(t, DeletePlanTx(context.Background(), staleTx, stale))
	require.NoError(t, staleTx.Commit())

	var count int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`, subject,
	).Scan(&count))
	require.Equal(t, 1, count, "a stale operation id must not acknowledge a rewritten plan")

	owningTx := beginTx(t, db)
	require.NoError(t, DeletePlanTx(context.Background(), owningTx, plan))
	require.NoError(t, owningTx.Commit())

	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`, subject,
	).Scan(&count))
	require.Zero(t, count, "the owning operation must discharge its own obligation")
}

// Quarantine is a slow retry, never a delete. The row IS the evidence; deleting
// it fails open.
func TestRecordAttemptTxQuarantinesWithoutDeleting(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	plan := conservativePlan(subject, presence.CategoryPrivateCall)
	plan.Attempts = maxPlanAttempts - 1
	insert(t, db, plan)

	tx := beginTx(t, db)
	claimed, _, err := ClaimTx(context.Background(), tx,
		PlanKey{SubjectID: subject, Category: presence.CategoryPrivateCall})
	require.NoError(t, err)
	require.Equal(t, maxPlanAttempts-1, claimed.Attempts,
		"the claim must read the durable attempt count, not restart it")
	require.NoError(t, RecordAttemptTx(
		context.Background(), tx, claimed, FailureStateRead, time.Second))
	require.NoError(t, tx.Commit())

	var attempts int
	var class string
	var reconcileAfter time.Time
	require.NoError(t, db.QueryRow(`
		SELECT attempts, failure_class, reconcile_after
		FROM presence_active_pending_plans WHERE user_id = $1`, subject,
	).Scan(&attempts, &class, &reconcileAfter))
	require.Equal(t, maxPlanAttempts, attempts)
	require.Equal(t, "state_read", class)
	require.True(t, reconcileAfter.After(time.Now().Add(quarantineInterval/2)),
		"a quarantined plan retries on the slow interval")
}

func TestDrainSubjectTxRemovesEveryCategoryAndReportsThem(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	other := dbtest.CreateUser(t, db)
	for _, category := range []presence.Category{
		presence.CategoryPrivateCall, presence.CategoryServerVoice,
	} {
		insert(t, db, conservativePlan(subject, category))
	}
	insert(t, db, conservativePlan(other, presence.CategoryPrivateCall))

	tx := beginTx(t, db)
	drained, err := DrainSubjectTx(context.Background(), tx, subject)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	require.ElementsMatch(t,
		[]presence.Category{presence.CategoryPrivateCall, presence.CategoryServerVoice}, drained)

	var count int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`, subject,
	).Scan(&count))
	require.Zero(t, count)

	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`, other,
	).Scan(&count))
	require.Equal(t, 1, count, "the drain is subject-anchored and must not touch another principal")

	// The FK is ON DELETE RESTRICT, so erasing this subject is exactly what the
	// drain has just made possible.
	_, err = db.Exec(`DELETE FROM users WHERE id = $1`, subject)
	require.NoError(t, err, "a drained subject must be erasable")
}

// A drain over a subject that owes nothing is a no-op, not an error.
func TestDrainSubjectTxIsANoOpForACleanSubject(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)

	tx := beginTx(t, db)
	drained, err := DrainSubjectTx(context.Background(), tx, subject)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Empty(t, drained)
}

// Every entry point refuses a nil handle rather than panicking. A nil handle is
// a wiring bug, and a panic inside a reconciler pass takes the worker down.
func TestStoreEntryPointsRefuseANilHandle(t *testing.T) {
	ctx := context.Background()
	plan := conservativePlan(uuid.New(), presence.CategoryPrivateCall)

	require.Error(t, InsertPlanTx(ctx, nil, plan))
	_, _, err := ClaimTx(ctx, nil, PlanKey{SubjectID: plan.SubjectID, Category: plan.Category})
	require.Error(t, err)
	require.Error(t, RecordAttemptTx(ctx, nil, plan, FailureNone, time.Second))
	require.Error(t, DeletePlanTx(ctx, nil, plan))
	_, err = DrainSubjectTx(ctx, nil, plan.SubjectID)
	require.Error(t, err)
	_, err = DiscoverDue(ctx, nil, maxPlanBatch)
	require.Error(t, err)
}
