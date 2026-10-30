package dmblock_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	dmblock "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This suite must never inherit testdb's developer-database fallback.
func familyReconcileDB(t *testing.T) *sql.DB {
	t.Helper()
	configured, err := url.Parse(os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	require.NotEmpty(t, configured.Host, "set an explicitly isolated DATABASE_URL")
	require.NotEmpty(t, configured.Path)
	require.NotEqual(t, "/concord", configured.Path)
	db, _ := dbtest.SetupTestDB(t)
	t.Cleanup(func() {
		_, err := dbtest.ExecReplica(db, `TRUNCATE voice_auth_room_closures, voice_auth_connection_closures, voice_auth_room_bindings, voice_auth_connections, voice_auth_boot_fences, login_family_revocation_targets, login_family_sids, login_families, login_family_revocations CASCADE`)
		require.NoError(t, err)
	})
	return db
}

func familyReconcileTx(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
			require.NoError(t, err)
		}
	})
	return tx
}

func familyReconcileUser(t *testing.T, tx *sql.Tx, user uuid.UUID) lf.UserAuthority {
	t.Helper()
	barrier, err := lf.AcquireWriterBarrierTx(context.Background(), tx)
	require.NoError(t, err)
	users, err := lf.LockUsersTx(context.Background(), tx, barrier, []uuid.UUID{user}, lf.UserNoKeyUpdate)
	require.NoError(t, err)
	authority, err := users.ForUser(user)
	require.NoError(t, err)
	return authority
}

func familyReconcileMint(t *testing.T, db *sql.DB, user uuid.UUID, epoch string) lf.Binding {
	t.Helper()
	tx := familyReconcileTx(t, db)
	family, err := lf.CreateFamilyTx(context.Background(), tx, familyReconcileUser(t, tx, user), epoch)
	require.NoError(t, err)
	sid := uuid.New()
	require.NoError(t, lf.BindSIDTx(context.Background(), tx, family, sid, time.Now().Add(time.Hour).UTC().Truncate(time.Second)))
	require.NoError(t, tx.Commit())
	return family.Binding()
}

func familyReconcileRevoke(t *testing.T, db *sql.DB, user uuid.UUID) lf.DeliveryOwner {
	t.Helper()
	tx := familyReconcileTx(t, db)
	targets, err := lf.CaptureUserTargetsTx(context.Background(), tx, familyReconcileUser(t, tx, user), nil)
	require.NoError(t, err)
	result, err := lf.RevokeFamiliesTx(context.Background(), tx, targets, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonExplicitRevoke})
	require.NoError(t, err)
	require.Len(t, result.Owners, 1)
	require.NoError(t, tx.Commit())
	return result.Owners[0]
}

type familyReconcileOperation struct {
	User            uuid.UUID
	Scope, Reason   string
	Created, Sealed time.Time
	Completed       sql.NullTime
	Attempts        int
	After           time.Time
}

func familyReconcileRead(t *testing.T, db *sql.DB, operation uuid.UUID) familyReconcileOperation {
	t.Helper()
	var result familyReconcileOperation
	require.NoError(t, db.QueryRow(`SELECT user_id,scope,reason,created_at,sealed_at,completed_at,attempts,reconcile_after FROM login_family_revocations WHERE operation_id=$1`, operation).Scan(&result.User, &result.Scope, &result.Reason, &result.Created, &result.Sealed, &result.Completed, &result.Attempts, &result.After))
	return result
}

func familyReconcileForceDue(t *testing.T, db *sql.DB, operation uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`UPDATE login_family_revocations SET reconcile_after=clock_timestamp() WHERE operation_id=$1`, operation)
	require.NoError(t, err)
}

func familyReconcileFrontier(t *testing.T, db *sql.DB, operation uuid.UUID) []uuid.UUID {
	t.Helper()
	rows, err := db.Query(`SELECT family_id FROM login_family_revocation_targets WHERE operation_id=$1 ORDER BY family_id`, operation)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var result []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		result = append(result, id)
	}
	require.NoError(t, rows.Err())
	return result
}

func TestLoginFamilyReconcileUnwiredRetryRetainsSealedOwnerAndFreshFamily(t *testing.T) {
	db := familyReconcileDB(t)
	user := dbtest.CreateUser(t, db)
	first := familyReconcileMint(t, db, user, "")
	second := familyReconcileMint(t, db, user, "")
	owner := familyReconcileRevoke(t, db, user)
	original := familyReconcileRead(t, db, owner.AuthOperationID)
	fresh := familyReconcileMint(t, db, user, "")
	require.ElementsMatch(t, []uuid.UUID{first.FamilyID, second.FamilyID}, familyReconcileFrontier(t, db, owner.AuthOperationID))
	// Reconstructing the worker models a process restart. Durable creation age,
	// owner and capture must survive every retry even without a delivery adapter.
	for _, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
		familyReconcileForceDue(t, db, owner.AuthOperationID)
		_, err := dmblock.New(db, nil).ReconcileDue(context.Background(), 100)
		require.NoError(t, err, "a recorded delivery failure is bounded reconciliation progress")
		current := familyReconcileRead(t, db, owner.AuthOperationID)
		assert.False(t, current.Completed.Valid, "empty media SQL cannot replace required local Hub receipt")
		assert.Equal(t, original.Created, current.Created)
		assert.Equal(t, original.Sealed, current.Sealed)
		assert.Equal(t, original.User, current.User)
		assert.Equal(t, original.Scope, current.Scope)
		assert.Equal(t, original.Reason, current.Reason)
		var retrySeconds float64
		require.NoError(t, db.QueryRow(`SELECT EXTRACT(EPOCH FROM reconcile_after-updated_at) FROM login_family_revocations WHERE operation_id=$1`, owner.AuthOperationID).Scan(&retrySeconds))
		assert.InDelta(t, delay.Seconds(), retrySeconds, 0.1)
		require.ElementsMatch(t, []uuid.UUID{first.FamilyID, second.FamilyID}, familyReconcileFrontier(t, db, owner.AuthOperationID))
		var terminal sql.NullTime
		var auth, epoch uuid.NullUUID
		require.NoError(t, db.QueryRow(`SELECT terminal_at,auth_operation_id,epoch_generation FROM login_families WHERE family_id=$1`, fresh.FamilyID).Scan(&terminal, &auth, &epoch))
		assert.False(t, terminal.Valid)
		assert.False(t, auth.Valid)
		assert.False(t, epoch.Valid)
	}
}

func TestLoginFamilyReconcileCanceledSQLCannotCompleteOwner(t *testing.T) {
	db := familyReconcileDB(t)
	user := dbtest.CreateUser(t, db)
	familyReconcileMint(t, db, user, "")
	owner := familyReconcileRevoke(t, db, user)
	original := familyReconcileRead(t, db, owner.AuthOperationID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := dmblock.New(db, nil).ReconcileDue(ctx, 100)
	require.Error(t, err)
	after := familyReconcileRead(t, db, owner.AuthOperationID)
	assert.Equal(t, original, after, "unvisited/canceled SQL must not fabricate progress or completion")
}

func TestLoginFamilyReconcileLostAcknowledgementReplaysOriginalOperation(t *testing.T) {
	db := familyReconcileDB(t)
	user := dbtest.CreateUser(t, db)
	first := familyReconcileMint(t, db, user, "")
	owner := familyReconcileRevoke(t, db, user)
	original := familyReconcileRead(t, db, owner.AuthOperationID)
	fresh := familyReconcileMint(t, db, user, "")
	var delivered []dmblock.AuthRevocation
	for attempt := 0; attempt < 2; attempt++ {
		familyReconcileForceDue(t, db, owner.AuthOperationID)
		worker := dmblock.New(db, nil)
		worker.SetAuthRevocationEject(func(_ context.Context, operation dmblock.AuthRevocation) error {
			delivered = append(delivered, operation)
			if attempt == 0 {
				return errors.New("acknowledgement lost after exact delivery")
			}
			return nil
		})
		_, err := worker.ReconcileDue(context.Background(), 100)
		require.NoError(t, err)
		state := familyReconcileRead(t, db, owner.AuthOperationID)
		assert.Equal(t, attempt == 1, state.Completed.Valid)
		assert.Equal(t, original.Created, state.Created)
		assert.Equal(t, original.Sealed, state.Sealed)
		require.Equal(t, []uuid.UUID{first.FamilyID}, familyReconcileFrontier(t, db, owner.AuthOperationID))
	}
	require.Len(t, delivered, 2)
	for _, operation := range delivered {
		assert.Equal(t, owner.AuthOperationID, operation.OperationID)
		assert.Equal(t, user, operation.UserID)
		assert.Equal(t, original.Created, operation.CreatedAt)
		assert.Equal(t, original.Reason, operation.Reason)
		assert.Equal(t, original.Scope, operation.Scope)
	}
	var live bool
	require.NoError(t, db.QueryRow(`SELECT terminal_at IS NULL FROM login_families WHERE family_id=$1`, fresh.FamilyID).Scan(&live))
	assert.True(t, live, "replaying an operation must not recapture later families")
}

func familyReconcileEpoch(t *testing.T, db *sql.DB, user uuid.UUID) lf.EpochOwner {
	t.Helper()
	tx := familyReconcileTx(t, db)
	targets, err := lf.CaptureEpochTargetsTx(context.Background(), tx, familyReconcileUser(t, tx, user), "")
	require.NoError(t, err)
	owner := lf.EpochOwner{UserID: user, Generation: uuid.New(), CredentialEpoch: "11111111111111111111111111111111"}
	_, err = tx.Exec(`INSERT INTO credential_epoch_voice_ejections(user_id,credential_epoch,superseded_credential_epoch,generation) VALUES($1,$2,'',$3)`, user, owner.CredentialEpoch, owner.Generation)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE users SET credential_epoch=$2 WHERE id=$1`, user, owner.CredentialEpoch)
	require.NoError(t, err)
	result, err := lf.RevokeFamiliesForEpochTx(context.Background(), tx, targets, owner)
	require.NoError(t, err)
	require.Equal(t, []lf.DeliveryOwner{{Kind: lf.OwnerEpoch, UserID: user, EpochGeneration: owner.Generation}}, result.Owners)
	require.NoError(t, tx.Commit())
	return owner
}

func TestLoginFamilyReconcileFailureDoesNotStarveEpochOrDMCategory(t *testing.T) {
	db := familyReconcileDB(t)
	user := dbtest.CreateUser(t, db)
	familyReconcileMint(t, db, user, "")
	auth := familyReconcileRevoke(t, db, user)
	epochUser := dbtest.CreateUser(t, db)
	family := familyReconcileMint(t, db, epochUser, "")
	epoch := familyReconcileEpoch(t, db, epochUser)
	conversation := uuid.New()
	tx := familyReconcileTx(t, db)
	require.NoError(t, dmblock.EnqueueVoiceEjectionsTx(context.Background(), tx, conversation.String(), []uuid.UUID{user}))
	require.NoError(t, tx.Commit())
	worker := dmblock.New(db, nil)
	var authCalls, epochCalls, dmCalls int
	worker.SetAuthRevocationEject(func(_ context.Context, operation dmblock.AuthRevocation) error {
		authCalls++
		assert.Equal(t, auth.AuthOperationID, operation.OperationID)
		return errors.New("media subscriber unavailable")
	})
	worker.SetCredentialEpochEjectV2(func(_ context.Context, id uuid.UUID, current, old string, generation uuid.UUID) error {
		epochCalls++
		assert.Equal(t, epoch.UserID, id)
		assert.Equal(t, epoch.Generation, generation)
		assert.Equal(t, epoch.CredentialEpoch, current)
		assert.Empty(t, old)
		return errors.New("epoch subscriber unavailable")
	})
	worker.SetVoiceEjectV2(func(_ context.Context, id string, subject, generation uuid.UUID) error {
		dmCalls++
		assert.Equal(t, conversation.String(), id)
		assert.Equal(t, user, subject)
		assert.NotEqual(t, uuid.Nil, generation)
		return errors.New("DM subscriber unavailable")
	})
	_, err := worker.ReconcileDue(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, authCalls)
	assert.Equal(t, 1, epochCalls)
	assert.Equal(t, 1, dmCalls)
	var parents, authParents int
	var generation uuid.UUID
	var retrySeconds float64
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM credential_epoch_voice_ejections WHERE user_id=$1`, epochUser).Scan(&parents))
	assert.Equal(t, 1, parents)
	require.NoError(t, db.QueryRow(`SELECT generation,EXTRACT(EPOCH FROM reconcile_after-updated_at) FROM credential_epoch_voice_ejections WHERE user_id=$1`, epochUser).Scan(&generation, &retrySeconds))
	assert.Equal(t, epoch.Generation, generation)
	assert.InDelta(t, 60, retrySeconds, 0.1, "epoch retry policy must remain one minute")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM login_family_revocations WHERE user_id=$1`, epochUser).Scan(&authParents))
	assert.Zero(t, authParents, "epoch terminalization must not invent an auth parent")
	require.NoError(t, db.QueryRow(`SELECT epoch_generation FROM login_families WHERE family_id=$1`, family.FamilyID).Scan(&generation))
	assert.Equal(t, epoch.Generation, generation)
}

func TestLoginFamilyReconcileAuthorityFailureLeavesOwnerAndRoundUnfresh(t *testing.T) {
	db := familyReconcileDB(t)
	user := dbtest.CreateUser(t, db)
	familyReconcileMint(t, db, user, "")
	owner := familyReconcileRevoke(t, db, user)
	original := familyReconcileRead(t, db, owner.AuthOperationID)
	worker := dmblock.New(db, nil)
	require.True(t, worker.SecurityRoundCompletedAt().IsZero(), "startup cannot reuse durable SQL timestamps as local progress")
	worker.SetAuthRevocationEject(func(context.Context, dmblock.AuthRevocation) error { return dmblock.ErrSecurityAuthorityUnavailable })
	_, err := worker.ReconcileDue(context.Background(), 100)
	require.Error(t, err)
	assert.True(t, worker.SecurityRoundCompletedAt().IsZero())
	state := familyReconcileRead(t, db, owner.AuthOperationID)
	assert.False(t, state.Completed.Valid)
	assert.Equal(t, original.Created, state.Created)
	require.Len(t, familyReconcileFrontier(t, db, owner.AuthOperationID), 1)
}

func TestLoginFamilyReconcileRecordedFailedPrefixVisitsLaterOwners(t *testing.T) {
	db := familyReconcileDB(t)
	expected := make(map[uuid.UUID]bool)
	for range 3 {
		user := dbtest.CreateUser(t, db)
		familyReconcileMint(t, db, user, "")
		expected[familyReconcileRevoke(t, db, user).AuthOperationID] = false
	}
	worker := dmblock.New(db, nil)
	worker.SetAuthRevocationEject(func(_ context.Context, operation dmblock.AuthRevocation) error {
		_, exists := expected[operation.OperationID]
		require.True(t, exists)
		require.False(t, expected[operation.OperationID], "one bounded round must not spend its page retrying a failed prefix")
		expected[operation.OperationID] = true
		return errors.New("recorded delivery failure")
	})
	before := time.Now()
	_, err := worker.ReconcileDue(context.Background(), 100)
	require.NoError(t, err)
	for operation, visited := range expected {
		assert.True(t, visited, "later sealed owner was starved: %s", operation)
		state := familyReconcileRead(t, db, operation)
		assert.False(t, state.Completed.Valid)
		assert.Equal(t, 1, state.Attempts)
	}
	assert.False(t, worker.SecurityRoundCompletedAt().Before(before), "recorded delivery failures may count toward completed bounded progress")
	restarted := dmblock.New(db, nil)
	assert.True(t, restarted.SecurityRoundCompletedAt().IsZero(), "restart must not fabricate fresh security progress")
}

func TestLoginFamilyReconcileEpochRetryKeepsExistingHourBackoff(t *testing.T) {
	db := familyReconcileDB(t)
	user := dbtest.CreateUser(t, db)
	binding := familyReconcileMint(t, db, user, "")
	owner := familyReconcileEpoch(t, db, user)
	var originalCreated time.Time
	require.NoError(t, db.QueryRow(`SELECT created_at FROM credential_epoch_voice_ejections WHERE generation=$1`, owner.Generation).Scan(&originalCreated))
	_, err := db.Exec(`UPDATE credential_epoch_voice_ejections SET attempts=9,reconcile_after=clock_timestamp() WHERE generation=$1`, owner.Generation)
	require.NoError(t, err)
	worker := dmblock.New(db, nil)
	worker.SetCredentialEpochEjectV2(func(context.Context, uuid.UUID, string, string, uuid.UUID) error {
		return errors.New("epoch delivery unavailable")
	})
	require.NoError(t, worker.DeliverCredentialEpochVoiceEjection(context.Background(), user.String(), owner.CredentialEpoch, ""))
	var created time.Time
	var retrySeconds float64
	var attempts int
	require.NoError(t, db.QueryRow(`SELECT created_at,attempts,EXTRACT(EPOCH FROM reconcile_after-updated_at) FROM credential_epoch_voice_ejections WHERE generation=$1`, owner.Generation).Scan(&created, &attempts, &retrySeconds))
	assert.Equal(t, originalCreated, created)
	assert.Equal(t, 10, attempts)
	assert.InDelta(t, 3600, retrySeconds, 0.1)
	var generation uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT epoch_generation FROM login_families WHERE family_id=$1`, binding.FamilyID).Scan(&generation))
	assert.Equal(t, owner.Generation, generation)
}

// A callback reached only after the shared deadline has expired is not a
// meaningful delivery attempt. An always-due slow prefix must not consume every
// future round while healthy sealed owners only acquire abandoned SQL leases.
func TestLoginFamilyReconcileDeadlineConsumingOwnerDoesNotStarveLaterOwners(t *testing.T) {
	db := familyReconcileDB(t)
	originals := make(map[uuid.UUID]familyReconcileOperation)
	for range 3 {
		user := dbtest.CreateUser(t, db)
		familyReconcileMint(t, db, user, "")
		owner := familyReconcileRevoke(t, db, user)
		originals[owner.AuthOperationID] = familyReconcileRead(t, db, owner.AuthOperationID)
	}
	worker := dmblock.New(db, nil)
	var mu sync.Mutex
	blocked := uuid.Nil
	meaningful := make(map[uuid.UUID]int)
	worker.SetAuthRevocationEject(func(ctx context.Context, operation dmblock.AuthRevocation) error {
		// Reaching the callback with an already canceled context does not prove
		// scheduler progress: no subscriber request could have started here.
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		if blocked == uuid.Nil {
			blocked = operation.OperationID
		}
		isBlocked := blocked == operation.OperationID
		meaningful[operation.OperationID]++
		mu.Unlock()
		if isBlocked {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
	for range 4 {
		// Requeue ordinary mutable scheduling state only. Keep the previously
		// selected blocked owner first and every healthy later owner due, so an
		// incidental SQL lease/backoff cannot masquerade as delivery fairness.
		_, err := db.Exec(`UPDATE login_family_revocations SET reconcile_after=CASE WHEN operation_id=$1 THEN created_at ELSE clock_timestamp() END WHERE completed_at IS NULL`, blocked)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		_, roundErr := worker.ReconcileDue(ctx, 100)
		if roundErr != nil {
			t.Logf("bounded round outcome: %v", roundErr)
		}
		cancel()
	}
	mu.Lock()
	blockedOwner := blocked
	attempts := make(map[uuid.UUID]int, len(meaningful))
	for owner, count := range meaningful {
		attempts[owner] = count
	}
	mu.Unlock()
	require.NotEqual(t, uuid.Nil, blockedOwner, "fixture must exercise a live slow callback")
	assert.Positive(t, attempts[blockedOwner])
	healthyAttempts, healthyCompleted := 0, 0
	for owner, original := range originals {
		current := familyReconcileRead(t, db, owner)
		assert.Equal(t, original.Created, current.Created)
		assert.Equal(t, original.Sealed, current.Sealed)
		assert.Equal(t, original.User, current.User)
		assert.Equal(t, original.Scope, current.Scope)
		assert.Equal(t, original.Reason, current.Reason)
		require.Len(t, familyReconcileFrontier(t, db, owner), 1)
		if owner == blockedOwner {
			assert.False(t, current.Completed.Valid, "timed-out owner must remain owned")
			continue
		}
		healthyAttempts += attempts[owner]
		if current.Completed.Valid {
			healthyCompleted++
		}
	}
	assert.Positive(t, healthyAttempts, "later healthy owners never received a delivery attempt with a live context across bounded rounds")
	assert.Positive(t, healthyCompleted, "a deadline-consuming prefix starved every later healthy owner across bounded rounds")
}

func TestLoginFamilyReconcileLockedFirstOwnerDoesNotStarveLaterOwner(t *testing.T) {
	db := familyReconcileDB(t)
	originals := make(map[uuid.UUID]familyReconcileOperation)
	for range 2 {
		user := dbtest.CreateUser(t, db)
		familyReconcileMint(t, db, user, "")
		owner := familyReconcileRevoke(t, db, user)
		originals[owner.AuthOperationID] = familyReconcileRead(t, db, owner.AuthOperationID)
	}
	// Lock exactly the first candidate the public reconciler will discover.
	// Discovery can still see this committed row, while its later SKIP LOCKED
	// claim must leave it for another round and continue to the next owner.
	var lockedOwner uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT operation_id FROM login_family_revocations WHERE completed_at IS NULL ORDER BY attempts,reconcile_after,operation_id LIMIT 1`).Scan(&lockedOwner))
	var healthyOwner uuid.UUID
	for owner := range originals {
		if owner != lockedOwner {
			healthyOwner = owner
		}
	}
	held := familyReconcileTx(t, db)
	var locked uuid.UUID
	require.NoError(t, held.QueryRow(`SELECT operation_id FROM login_family_revocations WHERE operation_id=$1 FOR UPDATE`, lockedOwner).Scan(&locked))
	require.Equal(t, lockedOwner, locked)
	worker := dmblock.New(db, nil)
	var mu sync.Mutex
	attempted := make(map[uuid.UUID]int)
	worker.SetAuthRevocationEject(func(ctx context.Context, operation dmblock.AuthRevocation) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		attempted[operation.OperationID]++
		mu.Unlock()
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, roundErr := worker.ReconcileDue(ctx, 100)
	if roundErr != nil {
		t.Logf("bounded round outcome while first owner locked: %v", roundErr)
	}
	// Read through a different connection while the external row lock remains
	// held: successful later-owner progress must not depend on releasing it.
	healthy := familyReconcileRead(t, db, healthyOwner)
	pending := familyReconcileRead(t, db, lockedOwner)
	mu.Lock()
	lockedAttempts, healthyAttempts := attempted[lockedOwner], attempted[healthyOwner]
	mu.Unlock()
	assert.Zero(t, lockedAttempts, "an externally locked owner must not be delivered")
	assert.Equal(t, 1, healthyAttempts, "a lost SKIP LOCKED claim must not stop later healthy candidates")
	assert.True(t, healthy.Completed.Valid, "the later healthy owner must complete while the first row remains locked")
	assert.Equal(t, originals[lockedOwner], pending, "unclaimed locked ownership, scheduling and original age must remain untouched")
	require.Len(t, familyReconcileFrontier(t, db, lockedOwner), 1)
	require.NoError(t, held.Rollback())
}

func TestLoginFamilyReconcileOverdueRetryIsNotDisplacedByFreshPage(t *testing.T) {
	db := familyReconcileDB(t)
	user := dbtest.CreateUser(t, db)
	familyReconcileMint(t, db, user, "")
	old := familyReconcileRevoke(t, db, user)
	_, err := db.Exec(`UPDATE login_family_revocations SET attempts=7,reconcile_after=created_at WHERE operation_id=$1`, old.AuthOperationID)
	require.NoError(t, err)
	original := familyReconcileRead(t, db, old.AuthOperationID)
	frontier := familyReconcileFrontier(t, db, old.AuthOperationID)
	for range 100 {
		nextUser := dbtest.CreateUser(t, db)
		familyReconcileMint(t, db, nextUser, "")
		familyReconcileRevoke(t, db, nextUser)
	}
	worker := dmblock.New(db, nil)
	var mu sync.Mutex
	calls, oldCalls := 0, 0
	worker.SetAuthRevocationEject(func(ctx context.Context, operation dmblock.AuthRevocation) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		calls++
		isOld := operation.OperationID == old.AuthOperationID
		if isOld {
			oldCalls++
		}
		mu.Unlock()
		if isOld {
			return errors.New("retained overdue retry")
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = worker.ReconcileDue(ctx, 100)
	require.NoError(t, err)
	mu.Lock()
	gotCalls, gotOldCalls := calls, oldCalls
	mu.Unlock()
	assert.Equal(t, 1, gotOldCalls, "100 fresh attempt-zero owners must not displace the older overdue retry")
	assert.Equal(t, 100, gotCalls, "delivery must respect the bounded100 owner page")
	current := familyReconcileRead(t, db, old.AuthOperationID)
	assert.False(t, current.Completed.Valid)
	assert.Equal(t, original.Created, current.Created)
	assert.Equal(t, original.Sealed, current.Sealed)
	assert.Equal(t, original.User, current.User)
	assert.Equal(t, original.Scope, current.Scope)
	assert.Equal(t, original.Reason, current.Reason)
	assert.Equal(t, frontier, familyReconcileFrontier(t, db, old.AuthOperationID))
}

func TestLoginFamilyReconcileLockedPageDoesNotHideHealthy101stOwner(t *testing.T) {
	db := familyReconcileDB(t)
	originals := make(map[uuid.UUID]familyReconcileOperation)
	frontiers := make(map[uuid.UUID][]uuid.UUID)
	for range 100 {
		user := dbtest.CreateUser(t, db)
		familyReconcileMint(t, db, user, "")
		owner := familyReconcileRevoke(t, db, user)
		originals[owner.AuthOperationID] = familyReconcileRead(t, db, owner.AuthOperationID)
		frontiers[owner.AuthOperationID] = familyReconcileFrontier(t, db, owner.AuthOperationID)
	}
	healthyUser := dbtest.CreateUser(t, db)
	familyReconcileMint(t, db, healthyUser, "")
	healthyOwner := familyReconcileRevoke(t, db, healthyUser)
	// All first100 due owners remain locked throughout delivery and assertions.
	// Discovery must skip this prefix before applying its bounded100 scan limit.
	held := familyReconcileTx(t, db)
	rows, err := held.Query(`SELECT operation_id FROM login_family_revocations WHERE completed_at IS NULL ORDER BY reconcile_after,attempts,operation_id LIMIT 100 FOR UPDATE`)
	require.NoError(t, err)
	lockedCount := 0
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		_, expected := originals[id]
		require.True(t, expected, "fixture must lock the100 earlier owners only")
		lockedCount++
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, 100, lockedCount)
	worker := dmblock.New(db, nil)
	var mu sync.Mutex
	attempted := make(map[uuid.UUID]int)
	worker.SetAuthRevocationEject(func(ctx context.Context, operation dmblock.AuthRevocation) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		attempted[operation.OperationID]++
		mu.Unlock()
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, roundErr := worker.ReconcileDue(ctx, 100)
	if roundErr != nil {
		t.Logf("bounded round outcome with locked100-owner prefix: %v", roundErr)
	}
	healthy := familyReconcileRead(t, db, healthyOwner.AuthOperationID)
	mu.Lock()
	assert.Equal(t, 1, attempted[healthyOwner.AuthOperationID], "the healthy101st owner must receive a live attempt while the first100 remain locked")
	assert.Len(t, attempted, 1, "none of the externally locked owners may be delivered")
	mu.Unlock()
	assert.True(t, healthy.Completed.Valid, "the bounded discovery page must skip locked rows before limiting candidates")
	for owner, original := range originals {
		assert.Equal(t, original, familyReconcileRead(t, db, owner), "locked owner metadata must remain unchanged")
		assert.Equal(t, frontiers[owner], familyReconcileFrontier(t, db, owner))
	}
	require.NoError(t, held.Rollback())
}

func TestLoginFamilyReconcileClaimMissAfterDiscoveryContinuesLaterOwner(t *testing.T) {
	db := familyReconcileDB(t)
	for range 3 {
		user := dbtest.CreateUser(t, db)
		familyReconcileMint(t, db, user, "")
		familyReconcileRevoke(t, db, user)
	}
	rows, err := db.Query(`SELECT operation_id FROM login_family_revocations ORDER BY reconcile_after,attempts,operation_id`)
	require.NoError(t, err)
	var ordered []uuid.UUID
	for rows.Next() {
		var owner uuid.UUID
		require.NoError(t, rows.Scan(&owner))
		ordered = append(ordered, owner)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, ordered, 3)
	first, lockedOwner, later := ordered[0], ordered[1], ordered[2]
	originalLocked := familyReconcileRead(t, db, lockedOwner)
	originalFrontier := familyReconcileFrontier(t, db, lockedOwner)
	held := familyReconcileTx(t, db)
	worker := dmblock.New(db, nil)
	var mu sync.Mutex
	attempted := make(map[uuid.UUID]int)
	var lockErr error
	worker.SetAuthRevocationEject(func(ctx context.Context, operation dmblock.AuthRevocation) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		attempted[operation.OperationID]++
		mu.Unlock()
		if operation.OperationID == first {
			// Discovery has returned its complete candidate page before the first
			// callback. Locking the second owner here exercises its exact claim race,
			// even when discovery itself correctly uses SKIP LOCKED.
			var locked uuid.UUID
			err := held.QueryRowContext(ctx, `SELECT operation_id FROM login_family_revocations WHERE operation_id=$1 FOR UPDATE`, lockedOwner).Scan(&locked)
			mu.Lock()
			lockErr = err
			mu.Unlock()
			if err != nil {
				return err
			}
			if locked != lockedOwner {
				return errors.New("fixture locked unexpected owner")
			}
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, roundErr := worker.ReconcileDue(ctx, 100)
	if roundErr != nil {
		t.Logf("bounded round outcome after discovery claim miss: %v", roundErr)
	}
	mu.Lock()
	gotLockErr := lockErr
	firstAttempts, lockedAttempts, laterAttempts := attempted[first], attempted[lockedOwner], attempted[later]
	mu.Unlock()
	require.NoError(t, gotLockErr)
	require.Equal(t, 1, firstAttempts, "fixture must reach first callback and lock the already-listed second owner")
	assert.Zero(t, lockedAttempts)
	assert.Equal(t, 1, laterAttempts, "an exact claim miss must continue the later candidate in the discovered page")
	assert.True(t, familyReconcileRead(t, db, later).Completed.Valid)
	assert.Equal(t, originalLocked, familyReconcileRead(t, db, lockedOwner))
	assert.Equal(t, originalFrontier, familyReconcileFrontier(t, db, lockedOwner))
	assert.True(t, worker.SecurityRoundCompletedAt().IsZero(), "an unobserved claim candidate cannot establish complete round freshness")
	require.NoError(t, held.Rollback())
}
