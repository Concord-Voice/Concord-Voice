package ownership

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type ownershipRaceEffects struct {
	prepared int
	executed int
	voice    int
}

type ownershipRacePlan struct{}

func (ownershipRacePlan) HasWork() bool { return true }

type reversalCaptureLimitRace struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *reversalCaptureLimitRace) prepare() (rbac.PresenceRecheckPlan, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return nil, rbac.ErrPresenceCaptureLimited
}
func (r *reversalCaptureLimitRace) PrepareCapture(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	return r.prepare()
}
func (r *reversalCaptureLimitRace) PrepareCaptureStrict(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	return r.prepare()
}
func (*reversalCaptureLimitRace) CaptureVisibility(context.Context, *sql.Tx, rbac.PresenceRecheckPlan) error {
	return nil
}
func (*reversalCaptureLimitRace) Execute(rbac.PresenceRecheckPlan)         {}
func (*reversalCaptureLimitRace) Abandon(rbac.PresenceRecheckPlan, string) {}

type captureLimitRecheck struct{ beforeReturn func() }

func (r *captureLimitRecheck) PrepareCapture(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	return nil, rbac.ErrPresenceCaptureLimited
}
func (r *captureLimitRecheck) PrepareCaptureStrict(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	if r.beforeReturn != nil {
		r.beforeReturn()
	}
	return nil, rbac.ErrPresenceCaptureLimited
}
func (*captureLimitRecheck) CaptureVisibility(context.Context, *sql.Tx, rbac.PresenceRecheckPlan) error {
	return nil
}
func (*captureLimitRecheck) Execute(rbac.PresenceRecheckPlan)         {}
func (*captureLimitRecheck) Abandon(rbac.PresenceRecheckPlan, string) {}

func (e *ownershipRaceEffects) PrepareCapture(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	e.prepared++
	return ownershipRacePlan{}, nil
}
func (e *ownershipRaceEffects) PrepareCaptureStrict(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	e.prepared++
	return ownershipRacePlan{}, nil
}
func (*ownershipRaceEffects) CaptureVisibility(context.Context, *sql.Tx, rbac.PresenceRecheckPlan) error {
	return nil
}
func (e *ownershipRaceEffects) Execute(rbac.PresenceRecheckPlan)       { e.executed++ }
func (*ownershipRaceEffects) Abandon(rbac.PresenceRecheckPlan, string) {}
func (e *ownershipRaceEffects) RecheckUser(string, string)             { e.voice++ }
func (*ownershipRaceEffects) RecheckChannel(string, string)            {}
func (*ownershipRaceEffects) RecheckServer(string)                     {}
func (*ownershipRaceEffects) DisconnectUser(string, string)            {}

func seedOwnershipRaceServer(t *testing.T, db *sql.DB) (serverID, ownerA, ownerB, targetC uuid.UUID) {
	t.Helper()
	ownerA = dbtest.CreateUser(t, db)
	ownerB = dbtest.CreateUser(t, db)
	targetC = dbtest.CreateUser(t, db)
	serverID = uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'ownership-race', $2)`, serverID, ownerA)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member'), ($1, $4, 'member')`, serverID, ownerA, ownerB, targetC)
	require.NoError(t, err)
	return
}

func ownershipRaceRoles(t *testing.T, db *sql.DB, serverID uuid.UUID) map[uuid.UUID]string {
	t.Helper()
	rows, err := db.Query(`SELECT user_id, role FROM server_members WHERE server_id = $1`, serverID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rows.Close()) })
	roles := map[uuid.UUID]string{}
	for rows.Next() {
		var userID uuid.UUID
		var role string
		require.NoError(t, rows.Scan(&userID, &role))
		roles[userID] = role
	}
	require.NoError(t, rows.Err())
	return roles
}

func ownershipRaceContext() (*gin.Context, context.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/", nil)
	return c, c.Request.Context(), recorder
}

func TestInsertTransferRecordRejectsPreflightAfterProductionReversal(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, ownerB, targetC := seedOwnershipRaceServer(t, db)
	transferID := uuid.New()
	_, err := db.Exec(`INSERT INTO ownership_transfers
		(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at, completed_at)
		VALUES ($1, $2, $3, $4, 'completed', $5, NOW(), NOW() + INTERVAL '1 hour', NOW())`,
		transferID, serverID, ownerB, ownerA, uuid.NewString())
	require.NoError(t, err)

	h := &Handler{db: db, log: logger.New("ownership-race")}
	testContext, ctx, recorder := ownershipRaceContext()
	// The initiation checks pass while A still owns the server.
	require.NoError(t, h.requireServerOwner(ctx, nil, serverID.String(), ownerA.String(), "transfer ownership"))
	require.NoError(t, h.requireMembership(ctx, nil, serverID.String(), targetC.String()))
	require.NoError(t, h.requireNoPendingTransfer(ctx, nil, serverID.String()))

	_, err = h.executeReversal(ctx, &reversalRecord{
		transferID: transferID.String(), serverID: serverID.String(),
		fromUserID: ownerB.String(), toUserID: ownerA.String(), completedAt: time.Now(),
	})
	require.NoError(t, err)

	record := &transferRecord{
		id: uuid.NewString(), serverID: serverID.String(), fromUserID: ownerA.String(),
		toUserID: targetC.String(), reversalToken: uuid.NewString(),
		requestedAt: time.Now(), expiresAt: time.Now().Add(time.Hour),
	}
	err = h.insertTransferRecord(ctx, testContext, record)
	require.ErrorIs(t, err, errTransferOwnershipChanged, "stale initiation must be rejected after reversal")
	require.Equal(t, 409, recorder.Code)
	var pending int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM ownership_transfers WHERE id = $1 AND status = 'pending'`, record.id).Scan(&pending))
	require.Zero(t, pending)
}

func TestExecuteReversalCaptureLimitAfterOwnershipChangeIsReclassified(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, _, targetC := seedOwnershipRaceServer(t, db)
	_, err := db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, targetC, serverID)
	require.NoError(t, err)
	transferID := uuid.New()
	_, err = db.Exec(`INSERT INTO ownership_transfers
		(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at, completed_at)
		VALUES ($1, $2, $3, $4, 'completed', $5, NOW(), NOW() + INTERVAL '1 hour', NOW())`,
		transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)

	recheck := &reversalCaptureLimitRace{entered: make(chan struct{}), release: make(chan struct{})}
	h := &Handler{db: db, log: logger.New("ownership-race"), presenceRecheck: recheck}
	done := make(chan error, 1)
	go func() {
		_, err := h.executeReversal(context.Background(), &reversalRecord{
			transferID: transferID.String(), serverID: serverID.String(),
			fromUserID: ownerA.String(), toUserID: targetC.String(), completedAt: time.Now(),
		})
		done <- err
	}()
	<-recheck.entered
	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, probe.Ping())
	t.Cleanup(func() { require.NoError(t, probe.Close()) })
	ownerTx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := ownerTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback owner transaction: %v", rollbackErr)
		}
	})
	var ownerTxID int64
	require.NoError(t, ownerTx.QueryRow(`SELECT txid_current()`).Scan(&ownerTxID))
	_, err = ownerTx.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, ownerA, serverID)
	require.NoError(t, err)
	close(recheck.release)
	dbtest.WaitForRowLockWaiter(t, probe, ownerTxID)
	require.NoError(t, ownerTx.Commit())
	err = <-done
	require.ErrorIs(t, err, errReversalOwnershipChanged)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status))
	require.Equal(t, "completed", status)
}

func TestExecuteReversalCaptureLimitPreservesCancellationAndOwnershipChange(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, _, targetC := seedOwnershipRaceServer(t, db)
	_, err := db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, targetC, serverID)
	require.NoError(t, err)
	transferID := uuid.New()
	_, err = db.Exec(`INSERT INTO ownership_transfers
		(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at, completed_at)
		VALUES ($1, $2, $3, $4, 'completed', $5, NOW(), NOW() + INTERVAL '1 hour', NOW())`,
		transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{
		db: db, log: logger.New("ownership-race"),
		presenceRecheck: &captureLimitRecheck{beforeReturn: cancel},
	}
	_, err = h.executeReversal(ctx, &reversalRecord{
		transferID: transferID.String(), serverID: serverID.String(),
		fromUserID: ownerA.String(), toUserID: targetC.String(), completedAt: time.Now(),
	})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, errReversalOwnershipChanged)
}

func TestExecuteReversalPreservesCaptureLimitForCurrentTransfer(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, _, targetC := seedOwnershipRaceServer(t, db)
	_, err := db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, targetC, serverID)
	require.NoError(t, err)
	transferID := uuid.New()
	_, err = db.Exec(`INSERT INTO ownership_transfers
		(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at, completed_at)
		VALUES ($1, $2, $3, $4, 'completed', $5, NOW(), NOW() + INTERVAL '1 hour', NOW())`,
		transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)
	h := &Handler{db: db, log: logger.New("ownership-race"), presenceRecheck: &captureLimitRecheck{}}
	record := &reversalRecord{
		transferID: transferID.String(), serverID: serverID.String(),
		fromUserID: ownerA.String(), toUserID: targetC.String(),
	}
	_, err = h.executeReversal(context.Background(), record)
	require.ErrorIs(t, err, rbac.ErrPresenceCaptureLimited)
	require.NotErrorIs(t, err, errReversalOwnershipChanged)
}

func TestExecuteReversalCaptureLimitRejectsOriginalOwnerWhoLeftServer(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	serverID, ownerA, _, targetC := seedOwnershipRaceServer(t, db)
	_, err := db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, targetC, serverID)
	require.NoError(t, err)
	transferID := uuid.New()
	_, err = db.Exec(`INSERT INTO ownership_transfers
		(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at, completed_at)
		VALUES ($1, $2, $3, $4, 'completed', $5, NOW(), NOW() + INTERVAL '1 hour', NOW())`,
		transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, ownerA)
	require.NoError(t, err)

	h := &Handler{db: db, log: logger.New("ownership-race"), presenceRecheck: &captureLimitRecheck{}}
	_, err = h.executeReversal(context.Background(), &reversalRecord{
		transferID: transferID.String(), serverID: serverID.String(),
		fromUserID: ownerA.String(), toUserID: targetC.String(),
	})
	require.ErrorIs(t, err, errReversalOriginalOwnerNotMember)
	require.NotErrorIs(t, err, errReversalOwnershipChanged)
}

func TestExecuteReversalCaptureLimitClassifiesDisappearingState(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	serverID, ownerA, _, targetC := seedOwnershipRaceServer(t, db)
	_, err := db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, targetC, serverID)
	require.NoError(t, err)
	transferID := uuid.New()
	_, err = db.Exec(`INSERT INTO ownership_transfers
		(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at, completed_at)
		VALUES ($1, $2, $3, $4, 'completed', $5, NOW(), NOW() + INTERVAL '1 hour', NOW())`,
		transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)
	record := &reversalRecord{
		transferID: transferID.String(), serverID: serverID.String(),
		fromUserID: ownerA.String(), toUserID: targetC.String(),
	}

	var updateErr error
	h := &Handler{db: db, log: logger.New("ownership-race"), presenceRecheck: &captureLimitRecheck{
		beforeReturn: func() {
			_, updateErr = db.Exec(`UPDATE ownership_transfers SET status = 'cancelled' WHERE id = $1`, transferID)
		},
	}}
	_, err = h.executeReversal(context.Background(), record)
	require.NoError(t, updateErr)
	require.ErrorIs(t, err, errReversalOwnershipChanged)

	serverID, ownerA, _, targetC = seedOwnershipRaceServer(t, db)
	_, err = db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, targetC, serverID)
	require.NoError(t, err)
	transferID = uuid.New()
	_, err = db.Exec(`INSERT INTO ownership_transfers
		(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at, completed_at)
		VALUES ($1, $2, $3, $4, 'completed', $5, NOW(), NOW() + INTERVAL '1 hour', NOW())`,
		transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)
	record = &reversalRecord{
		transferID: transferID.String(), serverID: serverID.String(),
		fromUserID: ownerA.String(), toUserID: targetC.String(),
	}
	updateErr = nil
	h.presenceRecheck = &captureLimitRecheck{beforeReturn: func() {
		_, updateErr = db.Exec(`DELETE FROM servers WHERE id = $1`, serverID)
	}}
	_, err = h.executeReversal(context.Background(), record)
	require.NoError(t, updateErr)
	require.ErrorIs(t, err, errReversalOwnershipChanged)
}

func TestInsertTransferRecordRejectsExistingPendingTransfer(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, _, targetC := seedOwnershipRaceServer(t, db)
	_, err := db.Exec(`INSERT INTO ownership_transfers
		(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at)
		VALUES ($1, $2, $3, $4, 'pending', $5, NOW(), NOW() + INTERVAL '1 hour')`,
		uuid.New(), serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)

	h := &Handler{db: db, log: logger.New("ownership-race")}
	testContext, ctx, recorder := ownershipRaceContext()
	record := &transferRecord{
		id: uuid.NewString(), serverID: serverID.String(), fromUserID: ownerA.String(),
		toUserID: targetC.String(), reversalToken: uuid.NewString(),
		requestedAt: time.Now(), expiresAt: time.Now().Add(time.Hour),
	}
	err = h.insertTransferRecord(ctx, testContext, record)
	require.ErrorIs(t, err, errTransferAlreadyPending)
	require.Equal(t, 409, recorder.Code)
	var pending int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM ownership_transfers WHERE server_id = $1 AND status = 'pending'`, serverID).Scan(&pending))
	require.Equal(t, 1, pending)
}

func TestInsertTransferRecordReturnsServerErrorWhenBeginFails(t *testing.T) {
	db, err := sql.Open("postgres", "host=localhost")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	h := &Handler{db: db, log: logger.New("ownership-race")}
	testContext, ctx, recorder := ownershipRaceContext()
	err = h.insertTransferRecord(ctx, testContext, &transferRecord{
		id: uuid.NewString(), serverID: uuid.NewString(), fromUserID: uuid.NewString(),
		toUserID: uuid.NewString(), reversalToken: uuid.NewString(),
		requestedAt: time.Now(), expiresAt: time.Now().Add(time.Hour),
	})

	require.Error(t, err)
	require.Equal(t, 500, recorder.Code)
}

func TestInsertTransferRecordRejectsMissingServer(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID := uuid.New()

	h := &Handler{db: db, log: logger.New("ownership-race")}
	testContext, ctx, recorder := ownershipRaceContext()
	record := &transferRecord{
		id: uuid.NewString(), serverID: serverID.String(), fromUserID: uuid.NewString(),
		toUserID: uuid.NewString(), reversalToken: uuid.NewString(),
		requestedAt: time.Now(), expiresAt: time.Now().Add(time.Hour),
	}
	err := h.insertTransferRecord(ctx, testContext, record)

	require.ErrorIs(t, err, errTransferOwnershipChanged)
	require.Equal(t, 409, recorder.Code)
	var inserted int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM ownership_transfers WHERE id = $1`, record.id).Scan(&inserted))
	require.Zero(t, inserted)
}

func TestExecuteTransferRejectsStalePendingTransferWithoutSuccessEffects(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, ownerB, targetC := seedOwnershipRaceServer(t, db)
	_, err := db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, ownerB, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE server_members SET role = CASE WHEN user_id = $2 THEN 'owner' WHEN user_id = $3 THEN 'member' ELSE role END WHERE server_id = $1`, serverID, ownerB, ownerA)
	require.NoError(t, err)
	transferID := uuid.New()
	_, err = db.Exec(`INSERT INTO ownership_transfers (id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at) VALUES ($1, $2, $3, $4, 'pending', $5, NOW(), NOW() + INTERVAL '1 hour')`, transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)

	effects := &ownershipRaceEffects{}
	h := &Handler{db: db, log: logger.New("ownership-race"), audit: rbac.NewAuditWriter(db, logger.New("ownership-race")), voiceEnforcer: effects}
	h.SetPresenceRecheck(effects)
	err = h.executeTransfer(context.Background(), serverID.String(), transferID.String(), ownerA.String(), targetC.String())
	require.ErrorIs(t, err, errTransferOwnershipChanged)

	var status, owner string
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status))
	require.NoError(t, db.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&owner))
	require.Equal(t, "cancelled", status)
	require.Equal(t, ownerB.String(), owner)
	require.Equal(t, map[uuid.UUID]string{ownerA: "member", ownerB: "owner", targetC: "member"}, ownershipRaceRoles(t, db, serverID))
	require.Equal(t, 0, effects.executed)
	require.Equal(t, 0, effects.voice)
	var auditRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE server_id = $1 AND action = 'ownership_transferred'`, serverID).Scan(&auditRows))
	require.Zero(t, auditRows)
}

func TestExecuteTransferClassifiesStaleOwnerBeforeCaptureLimit(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, ownerB, targetC := seedOwnershipRaceServer(t, db)
	transferID := uuid.New()
	_, err := db.Exec(`INSERT INTO ownership_transfers (id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at) VALUES ($1, $2, $3, $4, 'pending', $5, NOW(), NOW() + INTERVAL '1 hour')`, transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)

	recheck := &reversalCaptureLimitRace{entered: make(chan struct{}), release: make(chan struct{})}
	h := &Handler{db: db, log: logger.New("ownership-race"), presenceRecheck: recheck}
	done := make(chan error, 1)
	go func() {
		done <- h.executeTransfer(context.Background(), serverID.String(), transferID.String(), ownerA.String(), targetC.String())
	}()
	<-recheck.entered

	// The confirmation was preflighted while A owned the server. A transfer to
	// B wins while strict presence preparation is blocked.
	_, err = db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, ownerB, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE server_members SET role = CASE WHEN user_id = $2 THEN 'owner' WHEN user_id = $3 THEN 'member' ELSE role END WHERE server_id = $1`, serverID, ownerB, ownerA)
	require.NoError(t, err)
	close(recheck.release)

	err = <-done
	require.ErrorIs(t, err, errTransferOwnershipChanged,
		"a stale owner must be classified before strict presence capture can mask it")
	require.NotErrorIs(t, err, rbac.ErrPresenceCaptureLimited)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status))
	require.Equal(t, "cancelled", status)
}

func TestExecuteTransferPreservesCaptureLimitForCurrentOwner(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, _, targetC := seedOwnershipRaceServer(t, db)
	transferID := uuid.New()
	_, err := db.Exec(`INSERT INTO ownership_transfers (id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at) VALUES ($1, $2, $3, $4, 'pending', $5, NOW(), NOW() + INTERVAL '1 hour')`, transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)

	h := &Handler{db: db, log: logger.New("ownership-race"), presenceRecheck: &captureLimitRecheck{}}
	err = h.executeTransfer(context.Background(), serverID.String(), transferID.String(), ownerA.String(), targetC.String())
	require.ErrorIs(t, err, rbac.ErrPresenceCaptureLimited)

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status))
	require.Equal(t, "pending", status)
}

func TestCompleteExpiredTransfersCancelsStaleTransferWithoutRevival(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, ownerB, targetC := seedOwnershipRaceServer(t, db)
	_, err := db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, ownerB, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE server_members SET role = CASE WHEN user_id = $2 THEN 'owner' WHEN user_id = $3 THEN 'member' ELSE role END WHERE server_id = $1`, serverID, ownerB, ownerA)
	require.NoError(t, err)
	transferID := uuid.New()
	_, err = db.Exec(`INSERT INTO ownership_transfers (id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at) VALUES ($1, $2, $3, $4, 'pending', $5, NOW() - INTERVAL '1 hour', NOW() - INTERVAL '1 second')`, transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)

	effects := &ownershipRaceEffects{}
	h := &Handler{db: db, log: logger.New("ownership-race"), voiceEnforcer: effects}
	h.SetPresenceRecheck(effects)
	h.CompleteExpiredTransfers(context.Background())

	var status, owner string
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status))
	require.NoError(t, db.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&owner))
	require.Equal(t, "cancelled", status)
	require.Equal(t, ownerB.String(), owner)
	require.Equal(t, map[uuid.UUID]string{ownerA: "member", ownerB: "owner", targetC: "member"}, ownershipRaceRoles(t, db, serverID))
	require.Equal(t, 0, effects.executed)
	require.Equal(t, 0, effects.voice)

	_, err = db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, ownerA, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE server_members SET role = CASE WHEN user_id = $2 THEN 'owner' WHEN user_id = $3 THEN 'member' ELSE role END WHERE server_id = $1`, serverID, ownerA, ownerB)
	require.NoError(t, err)
	h.CompleteExpiredTransfers(context.Background())
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status))
	require.Equal(t, "cancelled", status)
	require.Equal(t, map[uuid.UUID]string{ownerA: "owner", ownerB: "member", targetC: "member"}, ownershipRaceRoles(t, db, serverID))
}

func TestCompleteExpiredTransferCancelsStaleTransferWhenCaptureIsLimited(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, ownerB, targetC := seedOwnershipRaceServer(t, db)
	_, err := db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, ownerB, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE server_members SET role = CASE WHEN user_id = $2 THEN 'owner' WHEN user_id = $3 THEN 'member' ELSE role END WHERE server_id = $1`, serverID, ownerB, ownerA)
	require.NoError(t, err)
	transferID := uuid.New()
	_, err = db.Exec(`INSERT INTO ownership_transfers (id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at) VALUES ($1, $2, $3, $4, 'pending', $5, NOW() - INTERVAL '1 hour', NOW() - INTERVAL '1 second')`, transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)

	h := &Handler{db: db, log: logger.New("ownership-race"), presenceRecheck: &captureLimitRecheck{}}
	changed, err := h.completeExpiredTransfer(context.Background(), expiredTransfer{
		id: transferID.String(), serverID: serverID.String(), fromUserID: ownerA.String(), toUserID: targetC.String(),
	})
	require.NoError(t, err)
	require.False(t, changed)

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status))
	require.Equal(t, "cancelled", status,
		"an expired stale transfer must be terminal before capture-limit handling returns")

	// Even if ownership later returns to the original initiator, the stale
	// transfer must not be eligible to revive.
	_, err = db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, ownerA, serverID)
	require.NoError(t, err)
	h.presenceRecheck = nil
	changed, err = h.completeExpiredTransfer(context.Background(), expiredTransfer{
		id: transferID.String(), serverID: serverID.String(), fromUserID: ownerA.String(), toUserID: targetC.String(),
	})
	require.NoError(t, err)
	require.False(t, changed)
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status))
	require.Equal(t, "cancelled", status)
}

func TestCompleteExpiredTransferPreservesCaptureLimitForCurrentOwner(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, _, targetC := seedOwnershipRaceServer(t, db)
	transferID := uuid.New()
	_, err := db.Exec(`INSERT INTO ownership_transfers (id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at) VALUES ($1, $2, $3, $4, 'pending', $5, NOW() - INTERVAL '1 hour', NOW() - INTERVAL '1 second')`, transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)

	h := &Handler{db: db, log: logger.New("ownership-race"), presenceRecheck: &captureLimitRecheck{}}
	changed, err := h.completeExpiredTransfer(context.Background(), expiredTransfer{
		id: transferID.String(), serverID: serverID.String(), fromUserID: ownerA.String(), toUserID: targetC.String(),
	})
	require.ErrorIs(t, err, rbac.ErrPresenceCaptureLimited)
	require.False(t, changed)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status))
	require.Equal(t, "pending", status)
}

// TestExpiredPendingTransferCannotReviveAcrossOwnershipEpochs proves that a
// pending transfer retained after a current-owner capture failure cannot be
// completed after ownership leaves and returns to its original initiator.
func TestExpiredPendingTransferCannotReviveAcrossOwnershipEpochs(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	serverID, ownerA, ownerB, targetC := seedOwnershipRaceServer(t, db)
	transferAB, transferBA, pendingAC := uuid.New(), uuid.New(), uuid.New()
	for _, transfer := range []struct {
		id, from, to uuid.UUID
	}{
		{transferAB, ownerA, ownerB},
		{transferBA, ownerB, ownerA},
	} {
		_, err := db.Exec(`INSERT INTO ownership_transfers
			(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at, completed_at)
			VALUES ($1, $2, $3, $4, 'completed', $5, NOW(), NOW() + INTERVAL '1 hour', NOW())`,
			transfer.id, serverID, transfer.from, transfer.to, uuid.NewString())
		require.NoError(t, err)
	}
	_, err := db.Exec(`INSERT INTO ownership_transfers
		(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at)
		VALUES ($1, $2, $3, $4, 'pending', $5, NOW() - INTERVAL '1 hour', NOW() - INTERVAL '1 second')`,
		pendingAC, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)

	h := &Handler{db: db, hub: websocket.NewHub(nil, nil), log: logger.New("ownership-race"), presenceRecheck: &captureLimitRecheck{}}
	changed, err := h.completeExpiredTransfer(context.Background(), expiredTransfer{
		id: pendingAC.String(), serverID: serverID.String(), fromUserID: ownerA.String(), toUserID: targetC.String(),
	})
	require.ErrorIs(t, err, rbac.ErrPresenceCaptureLimited)
	require.False(t, changed)
	h.presenceRecheck = nil

	_, err = h.executeReversal(context.Background(), &reversalRecord{
		transferID: transferBA.String(), serverID: serverID.String(), fromUserID: ownerB.String(), toUserID: ownerA.String(),
	})
	require.NoError(t, err)
	_, err = h.executeReversal(context.Background(), &reversalRecord{
		transferID: transferAB.String(), serverID: serverID.String(), fromUserID: ownerA.String(), toUserID: ownerB.String(),
	})
	require.NoError(t, err)

	changed, err = h.completeExpiredTransfer(context.Background(), expiredTransfer{
		id: pendingAC.String(), serverID: serverID.String(), fromUserID: ownerA.String(), toUserID: targetC.String(),
	})
	require.NoError(t, err)
	require.False(t, changed, "a transfer spanning an ownership epoch must not revive")

	var status, finalOwner string
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, pendingAC).Scan(&status))
	require.NoError(t, db.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&finalOwner))
	require.Equal(t, "cancelled", status)
	require.Equal(t, ownerA.String(), finalOwner)
}

func TestCancelTransferStaleOwnerCannotCancelNewOwnerTransfer(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	serverID, ownerA, ownerB, targetC := seedOwnershipRaceServer(t, db)
	transferID := uuid.New()
	_, err := db.Exec(`INSERT INTO ownership_transfers
		(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at)
		VALUES ($1, $2, $3, $4, 'pending', $5, NOW(), NOW() + INTERVAL '1 hour')`,
		transferID, serverID, ownerA, targetC, uuid.NewString())
	require.NoError(t, err)

	// Lock the server before the transfer row, matching the ownership write
	// order. Cancellation must wait on the server row and recheck the owner
	// after this transaction changes both the owner and pending transfer.
	barrierTx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := barrierTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback cancellation barrier: %v", rollbackErr)
		}
	})
	var barrierTxID int64
	require.NoError(t, barrierTx.QueryRow(`SELECT txid_current()`).Scan(&barrierTxID))
	var owner string
	require.NoError(t, barrierTx.QueryRow(`SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&owner))
	require.Equal(t, ownerA.String(), owner)
	var lockedTransferID string
	require.NoError(t, barrierTx.QueryRow(`SELECT id FROM ownership_transfers WHERE id = $1 FOR UPDATE`, transferID).Scan(&lockedTransferID))

	h := &Handler{db: db, hub: websocket.NewHub(nil, nil), audit: rbac.NewAuditWriter(db, logger.New("ownership-race")), log: logger.New("ownership-race")}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("DELETE", "/", nil)
	c.Set("user_id", ownerA.String())
	c.Params = gin.Params{{Key: "id", Value: serverID.String()}}
	done := make(chan struct{})
	go func() {
		h.CancelTransfer(c)
		close(done)
	}()

	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, probe.Ping())
	t.Cleanup(func() { require.NoError(t, probe.Close()) })
	dbtest.WaitForRowLockWaiter(t, probe, barrierTxID)

	_, err = barrierTx.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, ownerB, serverID)
	require.NoError(t, err)
	_, err = barrierTx.Exec(`UPDATE server_members SET role = CASE WHEN user_id = $2 THEN 'owner' WHEN user_id = $3 THEN 'member' ELSE role END WHERE server_id = $1`, serverID, ownerB, ownerA)
	require.NoError(t, err)
	_, err = barrierTx.Exec(`UPDATE ownership_transfers SET from_user_id = $1 WHERE id = $2`, ownerB, transferID)
	require.NoError(t, err)
	require.NoError(t, barrierTx.Commit())
	<-done

	// The stale A request must preserve the existing forbidden response and
	// leave B's pending transfer untouched.
	require.Equal(t, 403, recorder.Code)
	require.JSONEq(t, `{"error":"Only the server owner can cancel the transfer"}`, recorder.Body.String())
	var status, fromUserID string
	require.NoError(t, db.QueryRow(`SELECT status, from_user_id FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status, &fromUserID))
	require.Equal(t, "pending", status)
	require.Equal(t, ownerB.String(), fromUserID)
	var auditRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE server_id = $1 AND action = 'ownership_transfer_cancelled'`, serverID).Scan(&auditRows))
	require.Zero(t, auditRows)
}
