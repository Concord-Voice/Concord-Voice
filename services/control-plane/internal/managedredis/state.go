package managedredis

import (
	"context"
	"errors"
	"sync"
	"time"

	redisauth "github.com/redis/go-redis/v9/auth"
)

// StaleCredentialAge is how long after the last validated provider fetch the
// runtime reports its credential as stale. A stale credential keeps serving
// until its ValidUntil, so a provider outage raises an alert, not an app outage;
// Redis ACL revocation still cuts it off.
const StaleCredentialAge = 120 * time.Second

const exactReadTimeout = 5 * time.Second
const providerLoginTimeout = 10 * time.Second
const providerReadTimeout = providerLoginTimeout + exactReadTimeout

// ExactSecretReader fetches only one scoped named secret.
type ExactSecretReader interface {
	ReadExact(ctx context.Context, scope SecretScope) (value []byte, returnedName string, err error)
}

// Runtime publishes monotonically increasing credentials to Redis clients.
type Runtime struct {
	reader         ExactSecretReader
	scope          SecretScope
	service        string
	now            func() time.Time
	mu             sync.RWMutex
	current        Credential
	fetched        time.Time
	listeners      map[redisauth.CredentialsListener]struct{}
	candidateProbe func(context.Context, Credential) error
}

var errUnavailable = errors.New("managed Redis credential unavailable")

// NewRuntime validates the service and scope without fetching a secret.
func NewRuntime(reader ExactSecretReader, scope SecretScope, service string, now func() time.Time) (*Runtime, error) {
	if reader == nil || now == nil || !validScope(scope) ||
		(service != "control-plane" && service != "media-plane") {
		return nil, errInvalid
	}
	return &Runtime{reader: reader, scope: scope, service: service, now: now}, nil
}

// Refresh atomically publishes a fresh monotonic credential after its probe.
func (r *Runtime) Refresh(ctx context.Context) error {
	readCtx, cancel := context.WithTimeout(ctx, providerReadTimeout)
	raw, name, err := r.reader.ReadExact(readCtx, r.scope)
	cancel()
	if err != nil || ctx.Err() != nil || name != r.scope.Name {
		return errUnavailable
	}
	now := r.now()
	credential, err := ParseCredential(raw, r.scope, r.service, now)
	if err != nil {
		return errUnavailable
	}
	credential.ValidFrom = credential.ValidFrom.UTC()
	credential.ValidUntil = credential.ValidUntil.UTC()
	r.mu.RLock()
	prior := r.current
	probe := r.candidateProbe
	r.mu.RUnlock()
	if prior.Generation != credential.Generation && probe != nil {
		if err := probe(ctx, credential); err != nil {
			return errUnavailable
		}
	}
	r.mu.Lock()
	publishedAt := r.now()
	if ctx.Err() != nil || publishedAt.Before(credential.ValidFrom) || !publishedAt.Before(credential.ValidUntil) ||
		r.current.Generation > credential.Generation ||
		(r.current.Generation == credential.Generation && r.current != credential) {
		r.mu.Unlock()
		return errUnavailable
	}
	changed := r.current.Generation != credential.Generation
	r.current = credential
	r.fetched = now
	listeners := make([]redisauth.CredentialsListener, 0, len(r.listeners))
	if changed {
		for listener := range r.listeners {
			listeners = append(listeners, listener)
		}
	}
	r.mu.Unlock()
	if changed {
		for _, listener := range listeners {
			listener.OnNext(redisauth.NewBasicCredentials(credential.Username, credential.Password))
		}
	}
	return nil
}

// SetCandidateProbe installs the fresh AUTH/PING check before publishing a new
// generation. Configure this before the first Refresh or worker starts.
func (r *Runtime) SetCandidateProbe(probe func(context.Context, Credential) error) {
	r.mu.Lock()
	r.candidateProbe = probe
	r.mu.Unlock()
}

// Subscribe is the pinned go-redis streaming provider contract. A repeated
// subscription of the same listener replaces no state and either returned
// unsubscribe function removes it exactly once.
func (r *Runtime) Subscribe(listener redisauth.CredentialsListener) (redisauth.Credentials, redisauth.UnsubscribeFunc, error) {
	if listener == nil {
		return nil, nil, errUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.freshLocked(r.now()); err != nil {
		return nil, nil, err
	}
	if r.listeners == nil {
		r.listeners = make(map[redisauth.CredentialsListener]struct{})
	}
	r.listeners[listener] = struct{}{}
	return redisauth.NewBasicCredentials(r.current.Username, r.current.Password), func() error {
		r.mu.Lock()
		delete(r.listeners, listener)
		r.mu.Unlock()
		return nil
	}, nil
}

// Fresh rejects cold or expired runtime state.
func (r *Runtime) Fresh(now time.Time) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.freshLocked(now)
}

// Now returns the runtime clock so client hooks and tests share one deadline.
func (r *Runtime) Now() time.Time { return r.now() }

func (r *Runtime) freshLocked(now time.Time) error {
	if r.current.Generation == 0 || now.Before(r.current.ValidFrom) ||
		!now.Before(r.current.ValidUntil) {
		return errUnavailable
	}
	return nil
}

// Stale reports that the last validated provider fetch is older than
// StaleCredentialAge. It stays true after ValidUntil, so the alert keeps firing
// for the whole outage; Snapshot refuses the expired credential separately.
func (r *Runtime) Stale(now time.Time) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current.Generation != 0 && !now.Before(r.fetched.Add(StaleCredentialAge))
}

// Snapshot returns an immutable credential only while runtime state is usable.
func (r *Runtime) Snapshot(now time.Time) (Credential, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if err := r.freshLocked(now); err != nil {
		return Credential{}, err
	}
	return r.current, nil
}
