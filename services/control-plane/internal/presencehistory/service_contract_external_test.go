package presencehistory_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type plannedCustomTextRecorder interface {
	RecordCustomTextTransition(
		context.Context,
		*sql.Tx,
		uuid.UUID,
		presencehistory.CustomTextState,
		presencehistory.CustomTextState,
	) error
}

type plannedPresenceReconciler interface {
	BindDelivery(presencehistory.Delivery) error
	WithReadySender(context.Context, uuid.UUID, func() error) error
	WithReadySenderMode(context.Context, uuid.UUID, presencehistory.OperationMode, func() error) error
	BeginForcedSecurityClear(context.Context, *sql.Tx, uuid.UUID) (presencehistory.ForcedClearResult, error)
	CompleteForcedSecurityClear(context.Context, *sql.Tx, presencehistory.ForcedClearResult) presencehistory.ForcedClearCompletion
	ClaimAndDeliver(context.Context, presencehistory.DeliveryPlan) error
	CompleteClaim(context.Context, presencehistory.DeliveryPlan) presencehistory.ClaimCompletion
	EmergencyReset(context.Context, presencehistory.DeliveryPlan) error
	ReconcilePending(context.Context, int) (presencehistory.ReconcileStats, error)
	ReconcileStaleDisclosure(context.Context) (int64, error)
	RunPendingReconciler(context.Context)
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	CommitTx(*sql.Tx) error
	RollbackTx(*sql.Tx) error
}

var _ plannedCustomTextRecorder = (*presencehistory.Service)(nil)
var _ plannedPresenceReconciler = (*presencehistory.Service)(nil)

func TestServiceRecorderFailsClosedWithoutConstruction(t *testing.T) {
	var service *presencehistory.Service
	err := service.RecordCustomTextTransition(
		context.Background(),
		nil,
		uuid.New(),
		presencehistory.CustomTextState{},
		presencehistory.CustomTextState{Text: "not recorded"},
	)
	require.Error(t, err)

	service = presencehistory.NewService(nil, presencehistory.DisclosureState{}, false)
	err = service.RecordCustomTextTransition(
		context.Background(),
		nil,
		uuid.New(),
		presencehistory.CustomTextState{},
		presencehistory.CustomTextState{Text: "not recorded"},
	)
	require.Error(t, err)
}
