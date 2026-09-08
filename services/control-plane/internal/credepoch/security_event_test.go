package credepoch

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type securityEventRecorder struct{ events []securityevent.Event }

func (r *securityEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
}

func requireSerializedEventPrivacy(t *testing.T, events []securityevent.Event) {
	t.Helper()
	require.NotEmpty(t, events)
	serialized, err := json.Marshal(events)
	require.NoError(t, err)
	for _, fixture := range []string{
		"code-fixture", "credential-fixture", "user-fixture", "session-fixture",
		"device-fixture", "token-fixture", "198.51.100.44", "email-fixture@example.test", "raw-error-fixture",
	} {
		require.NotContains(t, string(serialized), fixture)
	}
}

func TestCheckEmitsClosedFenceMismatchAndUnavailableBranches(t *testing.T) {
	f, mini := newFence(t)
	recorder := &securityEventRecorder{}
	f.SetSecurityEvents(recorder)
	require.NoError(t, mini.Set(Key("user"), "active:current"))
	require.ErrorIs(t, f.Check(context.Background(), "user", "stale"), ErrEpochMismatch)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventCredentialEpoch, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonCredentialEpochMismatch}}, recorder.events)
	requireSerializedEventPrivacy(t, recorder.events)
	recorder.events = nil
	require.NoError(t, mini.Set(Key("user"), "blocked:operation"))
	require.ErrorIs(t, f.Check(context.Background(), "user", "current"), ErrBlocked)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventCredentialEpoch, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonCredentialEpochOperationInProgress}}, recorder.events)
	requireSerializedEventPrivacy(t, recorder.events)

	deadRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { require.NoError(t, deadRedis.Close()) })
	deadDB, err := sql.Open("postgres", "host=127.0.0.1 port=1 connect_timeout=1 sslmode=disable")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, deadDB.Close()) })
	unavailable := New(deadDB, deadRedis, nopLogger{})
	unavailableRecorder := &securityEventRecorder{}
	unavailable.SetSecurityEvents(unavailableRecorder)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.ErrorIs(t, unavailable.Check(ctx, "user", "current"), ErrUnavailable)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventCredentialEpoch, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonCredentialEpochBackendUnavailable}}, unavailableRecorder.events)
	requireSerializedEventPrivacy(t, unavailableRecorder.events)
}
