package api // revive:disable-line:var-naming

import (
	"context"
	"database/sql"

	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/attestation"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
)

// buildAttestationHandler wires the attestation package (#677, ADR-0010).
//
// Initialization is best-effort when REQUIRE_CLIENT_ATTESTATION=false (self-
// hosted default): if OIDC discovery or cache hydration fails we log a
// warning and return a Handler whose verify endpoint will be reachable but
// will reject all payloads. The RequireAttestation middleware (registered
// separately) is a pass-through in this mode so existing routes are
// unaffected by a degraded attestation surface.
//
// When REQUIRE_CLIENT_ATTESTATION=true (hosted concordvoice.chat),
// initialization failures are fatal — matches the fail-closed posture
// required by ADR-0010 D2.
func buildAttestationHandler(
	db *sql.DB,
	rdb *redis.Client,
	nc *natsclient.Client,
	cfg *config.Config,
	log *logger.Logger,
) *attestation.Handler {
	ctx := context.Background()
	repo := attestation.NewRepository(db)
	oidcVerifier := buildOIDCVerifier(ctx, cfg, log)
	cache := attestation.NewCache(repo, nc, rdb, log)
	hydrateCache(ctx, cache, cfg, log)
	startCache(ctx, cache, cfg, log)
	// Normalize typed-nil to interface-nil: in degraded mode buildOIDCVerifier
	// returns a nil *OIDCVerifier, which passed directly to NewHandler would
	// produce a typed-nil interface (h.oidc != nil but method dispatch panics
	// on the nil receiver). The explicit nil branch hands the constructor a
	// real interface-nil so PublishSPA/PublishBinary can guard via h.oidc==nil
	// and refuse cleanly with 503.
	if oidcVerifier == nil {
		return attestation.NewHandler(repo, cache, nil, nc, rdb, log)
	}
	return attestation.NewHandler(repo, cache, oidcVerifier, nc, rdb, log)
}

// buildOIDCVerifier constructs the per-axis OIDC verifier. On failure,
// fatal-exits when cfg.RequireClientAttestation is true, otherwise logs a
// warning and returns nil so the Handler's publish endpoint refuses all
// requests (degraded mode — verify endpoint still issues UNKNOWN_RELEASE
// rejections at the expected rate while the operator triages the config).
func buildOIDCVerifier(ctx context.Context, cfg *config.Config, log *logger.Logger) *attestation.OIDCVerifier {
	oidcCfg := attestation.OIDCConfig{
		Issuer:         cfg.OIDCIssuer,
		Audience:       cfg.OIDCAudience,
		SubjectPrefix:  cfg.OIDCSubjectPrefix,
		SPAWorkflow:    cfg.OIDCSPAWorkflow,
		SPARef:         cfg.OIDCSPARef,
		BinaryWorkflow: cfg.OIDCBinaryWorkflow,
		BinaryRef:      cfg.OIDCBinaryRef,
	}
	verifier, err := attestation.NewOIDCVerifier(ctx, oidcCfg)
	if err == nil {
		return verifier
	}
	if cfg.RequireClientAttestation {
		log.Fatal("attestation OIDC verifier init failed", "error", err)
	}
	log.Warn("attestation OIDC verifier init failed; publish endpoint disabled", "error", err)
	return nil
}

// hydrateCache calls Cache.Hydrate and applies the same fatal-or-warn
// posture as buildOIDCVerifier. Returns no value because the caller doesn't
// care whether hydrate succeeded — a degraded cache produces UNKNOWN_RELEASE
// at verify time, which is the correct posture on the disabled path.
func hydrateCache(ctx context.Context, cache *attestation.Cache, cfg *config.Config, log *logger.Logger) {
	err := cache.Hydrate(ctx)
	if err == nil {
		return
	}
	if cfg.RequireClientAttestation {
		log.Fatal("attestation cache hydrate failed", "error", err)
	}
	log.Warn("attestation cache hydrate failed; cache empty until first refresh", "error", err)
}

// startCache calls Cache.Start (which subscribes to NATS + spawns the poll
// loop) and applies the same fatal-or-warn posture. The returned NATS
// subscriptions are intentionally dropped — the cache's poll-fallback covers
// the case where Start succeeds but a subscription is later torn down.
func startCache(ctx context.Context, cache *attestation.Cache, cfg *config.Config, log *logger.Logger) {
	_, err := cache.Start(ctx)
	if err == nil {
		return
	}
	if cfg.RequireClientAttestation {
		log.Fatal("attestation cache NATS subscribe failed", "error", err)
	}
	log.Warn("attestation cache NATS subscribe failed; poll-fallback only", "error", err)
}
