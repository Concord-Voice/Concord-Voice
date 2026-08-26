// services/control-plane/internal/users/delete_account_active_plan_test.go

package users

import (
	"context"
	"database/sql"
	"errors"
	"go/build"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// gatedErasureBound bounds DeleteAccount when the rail is wired. It is the ONLY
// assertion that can see a gate re-entry: presencehistory's sender gate is a
// buffered-1 channel with no timeout and no detector, so a drain that reached
// Rail.WithGatedTx instead of Rail.DrainAlreadyGated would block forever rather
// than fail. `go test -race` cannot see that class -- wall clock can.
const gatedErasureBound = 15 * time.Second

// Without a drain the FK RESTRICT turns an erasure into an opaque 23503. This
// test is what keeps the RESTRICT meaningful rather than merely obstructive: if
// it ever stops failing, migration 000111 has been softened to CASCADE and the
// drain below has quietly become decoration.
func TestDeleteUserWithoutDrainRaisesForeignKeyViolation(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "private_call")

	_, err := db.Exec(`DELETE FROM users WHERE id = $1`, userID)

	require.Error(t, err)
	require.Contains(t, err.Error(), "presence_active_pending_plans")
}

// The erasure succeeds, the row is drained, and the obligation is transferred to
// exactly one clear frame per drained category.
func TestDeleteAccountDrainsPlansAndTransfersTheObligation(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "private_call")
	insertPlanRow(t, db, userID, "server_voice")
	service, deliverer := newAccountServiceWithRail(t, db)

	require.NoError(t, service.DeleteAccount(context.Background(), userID.String()))

	assert.Zero(t, countUsers(t, db, userID))
	assert.Zero(t, countPlansForUser(t, db, userID))
	assert.ElementsMatch(t,
		[]presence.Category{presence.CategoryPrivateCall, presence.CategoryServerVoice},
		deliverer.categoriesFor(userID))
}

// A presence delivery failure must NEVER fail the erasure. The account is gone;
// only the frame did not settle. The seam enforces this structurally --
// ClearDrained returns nothing -- so this test pins the SHAPE: an implementer
// who widens it to return an error and propagates that error fails here.
func TestDeleteAccountSucceedsWhenTheClearFails(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "private_call")
	service, deliverer := newAccountServiceWithRail(t, db)
	deliverer.failEveryClear = true

	require.NoError(t, service.DeleteAccount(context.Background(), userID.String()))

	assert.Zero(t, countUsers(t, db, userID))
	assert.Equal(t, 1, deliverer.failureCount())
}

// The drain runs BEFORE DELETE FROM users, so a drain failure fails the erasure
// with a diagnosable error instead of Postgres's opaque 23503 -- migration
// 000110's fail-closed-with-a-diagnosable-error precedent. Move the drain after
// the DELETE and the surfaced error becomes the constraint violation.
func TestDeleteAccountDrainFailureRetainsUserWithDiagnosableError(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "private_call")
	drainErr := errors.New("forced active-category drain failure")
	drain := &stubActivePlanDrain{drainErr: drainErr}
	service := newAccountServiceWithDrain(t, db, drain)

	err := service.DeleteAccount(context.Background(), userID.String())

	require.ErrorIs(t, err, drainErr)
	require.Contains(t, err.Error(), "drain presence plans")
	require.NotContains(t, err.Error(), "23503",
		"an erasure must not fail on a raw foreign-key violation")
	assert.Equal(t, 1, countUsers(t, db, userID))
	assert.Equal(t, 1, countPlansForUser(t, db, userID))
	assert.Zero(t, drain.clearCount(),
		"a rolled-back erasure owes no clear frame")
}

// The transfer is POST-commit. The probe runs inside the clear frame and asks
// the database, on its own connection, whether the user is already gone. Hoist
// the transfer into the transaction and the probe either sees the user alive or
// blocks on the FOR UPDATE lock the erasure still holds -- both fail here, and
// the bounded probe context keeps the second one a failure rather than a hang.
func TestDeleteAccountTransfersTheObligationAfterCommit(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "server_voice")
	service, deliverer := newAccountServiceWithRail(t, db)
	var probed int
	var probeErr error
	deliverer.onClear = func() {
		ctx, cancelProbe := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelProbe()
		probeErr = db.QueryRowContext(ctx,
			`SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&probed)
	}

	require.NoError(t, service.DeleteAccount(context.Background(), userID.String()))

	require.NoError(t, probeErr, "the clear frame ran while the erasure still held its locks")
	assert.Zero(t, probed, "the clear frame ran before the erasure committed")
}

// The drain runs inside a sender gate the caller ALREADY holds, and the rail
// shares that exact gate. Re-entering it deadlocks with no timeout and no
// detector, so the bound is the assertion.
func TestDeleteAccountWithRailCompletesInsideTheHeldSenderGate(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "private_call")
	service, _ := newAccountServiceWithRail(t, db)

	done := make(chan error, 1)
	go func() { done <- service.DeleteAccount(context.Background(), userID.String()) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(gatedErasureBound):
		t.Fatalf("erasure did not complete within %s: the drain re-entered the sender gate",
			gatedErasureBound)
	}
}

// The drain seam is declared at the consumer so this package depends on the two
// methods it uses, not on the rail. Without that the erasure path would carry an
// internal/activepresence import purely for a type name.
func TestUsersPackageGainsNoActivePresenceImport(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	require.NoError(t, err)

	for _, imported := range pkg.Imports {
		require.False(t,
			strings.HasSuffix(imported, "/internal/activepresence"),
			"internal/users must reach the rail through its own ActivePlanDrain interface")
	}
}

// --- fixtures -------------------------------------------------------------

func insertPlanRow(t *testing.T, db *sql.DB, userID uuid.UUID, category string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO presence_active_pending_plans (
			user_id, category, operation_id, resolution, scope_event_at
		) VALUES ($1, $2, $3, 'conservative', now())`,
		userID, category, uuid.New())
	require.NoError(t, err)
}

func countUsers(t *testing.T, db *sql.DB, userID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&count))
	return count
}

// Scoped to one subject on purpose: concurrent sessions share this database, so
// a global count would make the assertion someone else's flake.
func countPlansForUser(t *testing.T, db *sql.DB, userID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`,
		userID).Scan(&count))
	return count
}

// newAccountServiceWithRail wires a REAL *activepresence.Rail over the SAME
// presencehistory coordinator that gates DeleteAccount. Sharing the gate is what
// makes the wall-clock bound above meaningful: a rail with its own coordinator
// would take a different gate array and never reproduce the deadlock.
func newAccountServiceWithRail(
	t *testing.T,
	db *sql.DB,
) (*AccountService, *recordingActiveDeliverer) {
	t.Helper()
	coordinator := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	deliverer := &recordingActiveDeliverer{}
	rail := activepresence.NewRail(db, coordinator,
		activepresence.NewReconciler(db, coordinator, nil, nil, deliverer, nil), nil)
	return configuredAccountServiceWithDrain(t, db, coordinator, rail), deliverer
}

func newAccountServiceWithDrain(t *testing.T, db *sql.DB, drain ActivePlanDrain) *AccountService {
	t.Helper()
	coordinator := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	return configuredAccountServiceWithDrain(t, db, coordinator, drain)
}

func configuredAccountServiceWithDrain(
	t *testing.T,
	db *sql.DB,
	coordinator *presencehistory.Service,
	drain ActivePlanDrain,
) *AccountService {
	t.Helper()
	handler := &Handler{
		db:                 db,
		presenceHistory:    coordinator,
		activitySuppressor: &replayActivitySettingsSuppressor{},
	}
	service := NewAccountService(db, logger.New("test"))
	service.SetActivitySettingsCleanupHandler(handler)
	require.False(t, service.HasActivePlanDrain(),
		"an unwired service must report the drain missing, or the boot guard is a tautology")
	service.SetActivePlanRail(drain)
	require.True(t, service.HasActivePlanDrain())
	return service
}

// --- doubles --------------------------------------------------------------

type clearedFrame struct {
	subject  uuid.UUID
	category presence.Category
}

// recordingActiveDeliverer is the rail's terminal. failEveryClear models a frame
// that never settled -- the terminal returns nothing, so a dropped frame is
// invisible to the erasure by construction, which is the property under test.
type recordingActiveDeliverer struct {
	mu             sync.Mutex
	cleared        []clearedFrame
	failures       int
	failEveryClear bool
	onClear        func()
}

func (d *recordingActiveDeliverer) ClearSenderActiveCategory(
	subject uuid.UUID,
	category presence.Category,
) {
	if d.onClear != nil {
		d.onClear()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failEveryClear {
		d.failures++
		return
	}
	d.cleared = append(d.cleared, clearedFrame{subject: subject, category: category})
}

func (d *recordingActiveDeliverer) DisconnectAllRichPresenceClients(context.Context) error {
	return errors.New("the erasure drain must never reach the fleet-wide disconnect")
}

func (d *recordingActiveDeliverer) categoriesFor(subject uuid.UUID) []presence.Category {
	d.mu.Lock()
	defer d.mu.Unlock()
	categories := make([]presence.Category, 0, len(d.cleared))
	for _, frame := range d.cleared {
		if frame.subject == subject {
			categories = append(categories, frame.category)
		}
	}
	return categories
}

func (d *recordingActiveDeliverer) failureCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failures
}

// stubActivePlanDrain injects a drain fault the real rail cannot be made to
// produce on demand.
type stubActivePlanDrain struct {
	mu       sync.Mutex
	drainErr error
	cleared  int
}

func (s *stubActivePlanDrain) DrainAlreadyGated(
	context.Context, *sql.Tx, uuid.UUID,
) ([]presence.Category, error) {
	return nil, s.drainErr
}

func (s *stubActivePlanDrain) ClearDrained(context.Context, uuid.UUID, []presence.Category) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleared++
}

func (s *stubActivePlanDrain) clearCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleared
}
