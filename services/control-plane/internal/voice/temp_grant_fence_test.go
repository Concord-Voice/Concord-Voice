package voice

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/stretchr/testify/require"
)

type blockingTempGrantPreparation struct {
	entered chan struct{}
	release chan struct{}
}

func (r *blockingTempGrantPreparation) PrepareCapture(
	context.Context, string, []string, *string,
) (rbac.PresenceRecheckPlan, error) {
	close(r.entered)
	<-r.release
	return nil, errors.New("injected preparation failure")
}

func (*blockingTempGrantPreparation) CaptureVisibility(context.Context, *sql.Tx, rbac.PresenceRecheckPlan) error {
	return nil
}

func (*blockingTempGrantPreparation) Execute(rbac.PresenceRecheckPlan) {}

func (*blockingTempGrantPreparation) Abandon(rbac.PresenceRecheckPlan, string) {}

func TestDeleteTemporaryGrantWithCapture_DoesNotFencePreparation(t *testing.T) {
	hub := websocket.NewHub(nil, nil)
	recheck := &blockingTempGrantPreparation{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager := &tempGrantManager{hub: hub, presenceRecheck: recheck}
	result := make(chan error, 1)

	release := func() {
		select {
		case <-recheck.release:
		default:
			close(recheck.release)
		}
	}
	t.Cleanup(release)

	go func() {
		_, _, err := manager.deleteTemporaryGrantWithCapture(context.Background(), "server", "channel", "user")
		result <- err
	}()

	select {
	case <-recheck.entered:
	case <-time.After(time.Second):
		t.Fatal("preparation did not start")
	}
	require.Zero(t, hub.PresenceAuthzOpenForTest(), "preparation must not hold the delivery fence")

	release()
	require.Error(t, <-result)
	require.Zero(t, hub.PresenceAuthzOpenForTest())
}
