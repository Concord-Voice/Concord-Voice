package attestation

import (
	"context"
	"sync"

	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	cpnats "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

// tokenVerifier is the subset of OIDCVerifier used by handlers. Defined here
// so tests can inject a fake without spinning up a JWKS server.
//
// W1 (per-axis OIDC config, #677): each publish axis verifies against its own
// expected workflow + ref, so the interface exposes axis-specific methods
// rather than a shared Verify. A token minted by the SPA-publishing workflow
// will be rejected by VerifyBinary at the OIDC layer (axis-bound identity),
// and vice versa — see oidc.go.
type tokenVerifier interface {
	VerifySPA(ctx context.Context, raw string) (string, error)
	VerifyBinary(ctx context.Context, raw string) (string, error)
}

// Handler bundles dependencies for attestation HTTP handlers.
type Handler struct {
	repo     *Repository
	cache    *Cache
	oidc     tokenVerifier
	nc       *cpnats.Client
	rdb      *redis.Client
	log      *logger.Logger
	eventsMu sync.RWMutex
	events   securityevent.Emitter
}

// NewHandler wires a Handler against the given dependencies.
func NewHandler(repo *Repository, cache *Cache, oidc tokenVerifier, nc *cpnats.Client, rdb *redis.Client, log *logger.Logger) *Handler {
	return &Handler{
		repo:   repo,
		cache:  cache,
		oidc:   oidc,
		nc:     nc,
		rdb:    rdb,
		log:    log,
		events: securityevent.Discard,
	}
}

// SetSecurityEvents injects bounded Nightwatch telemetry without widening the
// attestation constructor.
func (h *Handler) SetSecurityEvents(events securityevent.Emitter) {
	if events == nil {
		events = securityevent.Discard
	}
	h.eventsMu.Lock()
	h.events = events
	h.eventsMu.Unlock()
	if h.cache != nil {
		h.cache.SetSecurityEvents(events)
	}
}

func (h *Handler) securityEventEmitter() securityevent.Emitter {
	h.eventsMu.RLock()
	events := h.events
	h.eventsMu.RUnlock()
	return events
}

func (h *Handler) emit(ctx context.Context, event securityevent.Event) {
	h.securityEventEmitter().Emit(ctx, event)
}
