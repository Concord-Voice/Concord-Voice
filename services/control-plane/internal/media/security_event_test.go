package media

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/require"
)

type securityEventRecorder struct{ events []securityevent.Event }

func (r *securityEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
}

type concurrentSecurityEventEmitter struct{ emitted atomic.Uint64 }

func (e *concurrentSecurityEventEmitter) Emit(_ context.Context, _ securityevent.Event) {
	e.emitted.Add(1)
}

func TestDiskWatermarkSecurityEventsReportRefusalAndRecovery(t *testing.T) {
	available := uint64(25)
	w := newDiskWatermark("/", false, func(_ string, stat *syscall.Statfs_t) error { stat.Blocks, stat.Bavail = 100, available; return nil }, logger.New("test"))
	recorder := &securityEventRecorder{}
	w.SetSecurityEvents(recorder)
	require.ErrorIs(t, w.Check(), ErrAttachmentStorageAtCapacity)
	available = 26
	require.NoError(t, w.Check())
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDiskWatermark},
		{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDiskWatermark},
	}, recorder.events)
}

func TestDiskWatermarkSecurityEventSetterAndTransitionsAreRaceSafe(t *testing.T) {
	var available atomic.Uint64
	available.Store(25)
	watermark := newDiskWatermark("/", false, func(_ string, stat *syscall.Statfs_t) error {
		stat.Blocks, stat.Bavail = 100, available.Load()
		return nil
	}, logger.New("test"))
	emitter := &concurrentSecurityEventEmitter{}
	watermark.SetSecurityEvents(emitter)
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		for range 500 {
			watermark.SetSecurityEvents(emitter)
		}
	}()
	go func() {
		defer group.Done()
		<-start
		for index := range 500 {
			if index%2 == 0 {
				available.Store(25)
			} else {
				available.Store(26)
			}
			_ = watermark.Check()
		}
	}()
	close(start)
	group.Wait()
	require.Positive(t, emitter.emitted.Load())
}
