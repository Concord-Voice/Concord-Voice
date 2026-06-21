package attestation

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	cpnats "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
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
	repo  *Repository
	cache *Cache
	oidc  tokenVerifier
	nc    *cpnats.Client
	rdb   *redis.Client
	log   *logger.Logger
}

// NewHandler wires a Handler against the given dependencies.
func NewHandler(repo *Repository, cache *Cache, oidc tokenVerifier, nc *cpnats.Client, rdb *redis.Client, log *logger.Logger) *Handler {
	return &Handler{
		repo:  repo,
		cache: cache,
		oidc:  oidc,
		nc:    nc,
		rdb:   rdb,
		log:   log,
	}
}
