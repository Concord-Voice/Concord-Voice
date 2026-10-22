package managedredis

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type mutableCredentialReader struct {
	mu       sync.Mutex
	value    []byte
	name     string
	err      error
	calls    int
	scope    SecretScope
	deadline time.Time
}

func (r *mutableCredentialReader) ReadExact(ctx context.Context, scope SecretScope) ([]byte, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.scope = scope
	r.deadline, _ = ctx.Deadline()
	return append([]byte(nil), r.value...), r.name, r.err
}

func (r *mutableCredentialReader) set(value []byte, name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.value, r.name, r.err = append([]byte(nil), value...), name, err
}

func (r *mutableCredentialReader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *mutableCredentialReader) lastScope() SecretScope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.scope
}

func TestRuntimeRefreshPublishesValidatedCredential(t *testing.T) {
	scope := credentialTestScope()
	now := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", now.Add(-time.Minute), now.Add(time.Hour)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)

	require.NoError(t, runtime.Refresh(context.Background()))
	require.Equal(t, scope, reader.lastScope())
	got, err := runtime.Snapshot(now)
	require.NoError(t, err)
	require.Equal(t, uint64(12), got.Generation)
	require.NoError(t, runtime.Fresh(now))
}

func TestRuntimeRefreshRejectsWrongReturnedSecretName(t *testing.T) {
	scope := credentialTestScope()
	reader := &mutableCredentialReader{}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", credentialTestNow.Add(-time.Minute), credentialTestNow.Add(time.Hour)), "OTHER_SECRET", nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return credentialTestNow })
	require.NoError(t, err)

	require.Error(t, runtime.Refresh(context.Background()))
	require.Error(t, runtime.Fresh(credentialTestNow))
}

func TestRuntimeRejectsEqualGenerationValueChangeAndPreservesLastGood(t *testing.T) {
	scope := credentialTestScope()
	now := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", now.Add(-time.Minute), now.Add(time.Hour)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "changed-password", "rotation-12", now.Add(-time.Minute), now.Add(time.Hour)), scope.Name, nil)

	require.Error(t, runtime.Refresh(context.Background()))
	got, err := runtime.Snapshot(now)
	require.NoError(t, err)
	require.Equal(t, "synthetic-password", got.Password)
}

func TestRuntimeRefreshAcceptsEquivalentTimestampOffsets(t *testing.T) {
	scope := credentialTestScope()
	now := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	offset := time.FixedZone("half-hour", 5*3600+30*60)
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12",
		now.Add(-time.Minute).In(offset), now.Add(time.Hour).In(offset)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))
	now = now.Add(119 * time.Second)
	require.NoError(t, runtime.Refresh(context.Background()), "the unchanged generation must renew freshness")
	require.NoError(t, runtime.Fresh(now.Add(time.Second)))
}

func TestRuntimeRefreshReservesCandidateProbeBudget(t *testing.T) {
	scope := credentialTestScope()
	now := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", now.Add(-time.Minute), now.Add(time.Hour)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	var probeDeadline time.Time
	runtime.SetCandidateProbe(func(ctx context.Context, _ Credential) error {
		probeDeadline, _ = ctx.Deadline()
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	require.NoError(t, runtime.Refresh(ctx))
	require.False(t, reader.deadline.IsZero())
	require.False(t, probeDeadline.IsZero())
	require.Greater(t, probeDeadline.Sub(reader.deadline), 9*time.Second,
		"the provider fetch must leave the documented ten seconds for candidate AUTH")
}

func TestRuntimeRefreshRejectsCandidateExpiredDuringProbe(t *testing.T) {
	scope := credentialTestScope()
	now := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", now.Add(-time.Minute), now.Add(time.Hour)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))
	reader.set(credentialTestRaw(t, scope, 13, "cv_cp_13", "synthetic-next-password", "rotation-13", now.Add(-time.Minute), now.Add(5*time.Second)), scope.Name, nil)
	runtime.SetCandidateProbe(func(context.Context, Credential) error {
		now = now.Add(6 * time.Second)
		return nil
	})

	require.Error(t, runtime.Refresh(context.Background()))
	current, err := runtime.Snapshot(now)
	require.NoError(t, err)
	require.Equal(t, uint64(12), current.Generation)
}

func TestRuntimeRejectsGenerationReplayAndPreservesLastGood(t *testing.T) {
	scope := credentialTestScope()
	now := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", now.Add(-time.Minute), now.Add(time.Hour)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))
	reader.set(credentialTestRaw(t, scope, 11, "cv_cp_11", "older-password", "rotation-11", now.Add(-time.Hour), now.Add(time.Hour)), scope.Name, nil)

	require.Error(t, runtime.Refresh(context.Background()))
	got, err := runtime.Snapshot(now)
	require.NoError(t, err)
	require.Equal(t, uint64(12), got.Generation)
	require.Equal(t, "synthetic-password", got.Password)
}

func TestRuntimeFreshnessExpiresAtEarlierOfCredentialAndLastSuccessfulFetch(t *testing.T) {
	scope := credentialTestScope()
	fetchAt := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", fetchAt.Add(-time.Minute), fetchAt.Add(10*time.Minute)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return fetchAt })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))

	require.NoError(t, runtime.Fresh(fetchAt.Add(119*time.Second)))
	require.Error(t, runtime.Fresh(fetchAt.Add(120*time.Second)))
}

func TestRuntimeCredentialValidityCanShortenFetchFreshness(t *testing.T) {
	scope := credentialTestScope()
	fetchAt := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", fetchAt.Add(-time.Minute), fetchAt.Add(20*time.Second)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return fetchAt })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))

	require.NoError(t, runtime.Fresh(fetchAt.Add(19*time.Second)))
	require.Error(t, runtime.Fresh(fetchAt.Add(20*time.Second)))
}

func TestRuntimeTokenRenewalDoesNotResetCredentialFreshness(t *testing.T) {
	scope := credentialTestScope()
	fetchAt := credentialTestNow
	now := fetchAt
	reader := &mutableCredentialReader{name: scope.Name}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", fetchAt.Add(-time.Minute), fetchAt.Add(time.Hour)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))

	// Token renewal has no credential-reader call; only a validated provider fetch may reset freshness.
	now = fetchAt.Add(120 * time.Second)
	require.Error(t, runtime.Fresh(now))
	require.Equal(t, 1, reader.callCount())
}

func TestRuntimeFailedRefreshDoesNotReplaceLastGoodCredential(t *testing.T) {
	scope := credentialTestScope()
	now := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", now.Add(-time.Minute), now.Add(time.Hour)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))
	reader.set(nil, "", context.DeadlineExceeded)

	require.Error(t, runtime.Refresh(context.Background()))
	got, err := runtime.Snapshot(now)
	require.NoError(t, err)
	require.Equal(t, "synthetic-password", got.Password)
}
