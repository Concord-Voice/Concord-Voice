package users_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errForcedAmbiguousKeyResetCommit = errors.New("forced ambiguous key-reset commit")

type keyResetFailClosedObserver struct {
	events []string
	plans  []presencehistory.DeliveryPlan
}

func (o *keyResetFailClosedObserver) DeliverCustomText(
	_ context.Context,
	plan presencehistory.DeliveryPlan,
) (presencehistory.DeliveryAck, error) {
	o.plans = append(o.plans, plan)
	o.events = append(o.events, "delivery_ack")
	return presencehistory.DeliveryAck{OperationID: plan.OperationID}, nil
}

func (o *keyResetFailClosedObserver) DisconnectUser(uuid.UUID) {
	o.events = append(o.events, "disconnect")
}

func TestReplaceMyKeysCommitAmbiguityAcknowledgesClearThenDisconnects(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replacekeysambiguous")
	target := ts.CreateTestUser(t, "replacekeysambiguoustarget")
	seedPresenceOverrideStateForKeyRotation(t, ts, user.ID, target.ID)
	observer := &keyResetFailClosedObserver{}
	service := presencehistory.NewService(ts.DB, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(observer))
	commitCalls := 0
	restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Commit: func(tx *sql.Tx) error {
			commitCalls++
			if commitCalls == 1 {
				require.NoError(t, tx.Commit())
				return errForcedAmbiguousKeyResetCommit
			}
			return tx.Commit()
		},
	})
	t.Cleanup(restore)
	handler := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil, testCredFence(t, ts.DB), nil)
	handler.SetPresenceHistory(service)
	users.SetKeyResetSessionDisconnectorForTest(handler, observer)

	response := invokeReplaceMyKeysWithHandler(t, handler, user)
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	require.Len(t, observer.plans, 1)
	assert.Equal(t, presencehistory.DeliveryConservativeReset, observer.plans[0].Mode)
	assert.Equal(t, []string{"delivery_ack", "disconnect"}, observer.events)
	var pending int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, user.ID,
	).Scan(&pending))
	assert.Zero(t, pending)
}

func TestReplaceMyKeysConfirmedRollbackSendsNothingAndKeepsSession(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replacekeysrollback")
	observer := &keyResetFailClosedObserver{}
	service := presencehistory.NewService(ts.DB, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(observer))
	restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Commit: func(*sql.Tx) error { return errForcedAmbiguousKeyResetCommit },
	})
	t.Cleanup(restore)
	handler := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil, testCredFence(t, ts.DB), nil)
	handler.SetPresenceHistory(service)
	users.SetKeyResetSessionDisconnectorForTest(handler, observer)

	response := invokeReplaceMyKeysWithHandler(t, handler, user)
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.Empty(t, observer.plans)
	assert.Empty(t, observer.events)
}
