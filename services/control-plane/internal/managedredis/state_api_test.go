package managedredis

import (
	"context"
	"errors"
	"testing"
	"time"

	redisauth "github.com/redis/go-redis/v9/auth"
	"github.com/stretchr/testify/require"
)

type recordingCredentialsListener struct {
	updates []redisauth.Credentials
	errors  []error
}

func (l *recordingCredentialsListener) OnNext(credentials redisauth.Credentials) {
	l.updates = append(l.updates, credentials)
}

func (l *recordingCredentialsListener) OnError(err error) { l.errors = append(l.errors, err) }

func TestRuntimeCandidateProbeMustSucceedBeforeCredentialPublication(t *testing.T) {
	scope := credentialTestScope()
	now := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", now.Add(-time.Minute), now.Add(time.Hour)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))
	listener := &recordingCredentialsListener{}
	_, _, err = runtime.Subscribe(listener)
	require.NoError(t, err)

	reader.set(credentialTestRaw(t, scope, 13, "cv_cp_13", "synthetic-next-password", "rotation-13", now.Add(-time.Second), now.Add(time.Hour)), scope.Name, nil)
	runtime.SetCandidateProbe(func(context.Context, Credential) error { return errors.New("synthetic fresh AUTH failure") })
	require.Error(t, runtime.Refresh(context.Background()))
	current, err := runtime.Snapshot(now)
	require.NoError(t, err)
	require.Equal(t, uint64(12), current.Generation)
	require.Empty(t, listener.updates)

	runtime.SetCandidateProbe(func(_ context.Context, candidate Credential) error {
		require.Equal(t, uint64(13), candidate.Generation)
		return nil
	})
	require.NoError(t, runtime.Refresh(context.Background()))
	current, err = runtime.Snapshot(now)
	require.NoError(t, err)
	require.Equal(t, uint64(13), current.Generation)
	require.Len(t, listener.updates, 1)
	username, password := listener.updates[0].BasicAuth()
	require.Equal(t, "cv_cp_13", username)
	require.Equal(t, "synthetic-next-password", password)
	require.Empty(t, listener.errors)
}

func TestRuntimeSubscribeRequiresFreshCredentialAndUnsubscribeIsIdempotent(t *testing.T) {
	scope := credentialTestScope()
	now := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)

	_, _, err = runtime.Subscribe(&recordingCredentialsListener{})
	require.Error(t, err)
	_, _, err = runtime.Subscribe(nil)
	require.Error(t, err)

	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", now.Add(-time.Minute), now.Add(time.Hour)), scope.Name, nil)
	require.NoError(t, runtime.Refresh(context.Background()))
	listener := &recordingCredentialsListener{}
	credentials, unsubscribe, err := runtime.Subscribe(listener)
	require.NoError(t, err)
	username, password := credentials.BasicAuth()
	require.Equal(t, "cv_cp_12", username)
	require.Equal(t, "synthetic-password", password)
	require.NoError(t, unsubscribe())
	require.NoError(t, unsubscribe())

	reader.set(credentialTestRaw(t, scope, 13, "cv_cp_13", "synthetic-next-password", "rotation-13", now.Add(-time.Second), now.Add(time.Hour)), scope.Name, nil)
	require.NoError(t, runtime.Refresh(context.Background()))
	require.Empty(t, listener.updates, "unsubscribed listeners must not receive later credentials")
}

func TestRuntimeNowUsesInjectedClock(t *testing.T) {
	now := credentialTestNow
	runtime, err := NewRuntime(&mutableCredentialReader{}, credentialTestScope(), "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.Equal(t, now, runtime.Now())
	now = now.Add(time.Second)
	require.Equal(t, now, runtime.Now())
}

func TestNewRuntimeRejectsInvalidConstruction(t *testing.T) {
	scope := credentialTestScope()
	validReader := &mutableCredentialReader{}
	for _, tc := range []struct {
		name    string
		reader  ExactSecretReader
		scope   SecretScope
		service string
		now     func() time.Time
	}{
		{name: "missing reader", reader: nil, scope: scope, service: "control-plane", now: time.Now},
		{name: "invalid scope", reader: validReader, scope: SecretScope{}, service: "control-plane", now: time.Now},
		{name: "unsupported service", reader: validReader, scope: scope, service: "redis", now: time.Now},
		{name: "missing clock", reader: validReader, scope: scope, service: "control-plane", now: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRuntime(tc.reader, tc.scope, tc.service, tc.now)
			require.Error(t, err)
		})
	}
}
