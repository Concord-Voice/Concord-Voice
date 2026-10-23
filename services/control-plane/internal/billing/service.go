package billing

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// maxConcurrentReconciles caps how many Stripe-bound reconciles run at once, so a
// burst of webhooks cannot exhaust database connections or Stripe's rate limit.
const maxConcurrentReconciles = 4

// TierChanger is the convergence point a subscription change calls once its row
// is committed: it invalidates the entitlement cache and tells the user's live
// sessions. Tests inject a recorder.
type TierChanger interface {
	OnTierChange(ctx context.Context, userID uuid.UUID, oldTier, newTier string) error
}

// tierChangeAdapter binds TierChanger to entitlements.OnTierChange, as the
// subscriptions package does for its expiry sweep.
type tierChangeAdapter struct {
	cache    *entitlements.Cache
	notifier entitlements.SessionNotifier
}

func (a tierChangeAdapter) OnTierChange(ctx context.Context, userID uuid.UUID, oldTier, newTier string) error {
	return entitlements.OnTierChange(ctx, a.cache, a.notifier, userID, oldTier, newTier)
}

// NewTierChanger returns the production TierChanger over the shared cache and
// WebSocket notifier.
func NewTierChanger(cache *entitlements.Cache, notifier entitlements.SessionNotifier) TierChanger {
	return tierChangeAdapter{cache: cache, notifier: notifier}
}

// Deps is what NewService needs; a struct because the list would otherwise be
// seven parameters.
type Deps struct {
	DB          *sql.DB
	Stripe      *stripe.Client
	Billing     config.BillingConfig
	Environment string // cfg.Environment; compared with a subscription's metadata concord_env
	Changer     TierChanger
	Counters    *opsmetrics.Counters // nil-safe
	Log         *logger.Logger
}

// Service reconciles Stripe subscriptions into Concord's entitlement rows and
// serves the billing endpoints. Build it only when keys are configured.
type Service struct {
	Deps

	// sem bounds concurrent Stripe-bound reconciles.
	sem chan struct{}
	now func() time.Time

	// portalConfig caches the resolved Customer Portal configuration id (Task 6).
	portalConfig portalConfigCache

	// catalogCache holds the last successful /plans catalog (PR-3).
	catalogCache catalogCache

	// discovery remembers when the drift worker last checked each Stripe
	// customer for subscriptions it holds no row for, so every candidate gets a
	// turn. It lives in process memory (replicas are pinned to 1; a restart only
	// causes an early re-check) and discoveryMu guards it, because RunDrift can
	// be called concurrently.
	discoveryMu sync.Mutex
	discovery   map[string]time.Time
}

// NewService builds a Service over d.
func NewService(d Deps) *Service {
	return &Service{
		Deps:      d,
		sem:       make(chan struct{}, maxConcurrentReconciles),
		now:       time.Now,
		discovery: make(map[string]time.Time),
	}
}

// runTicker calls tick once at startup and then every interval until ctx is
// canceled, shaped like subscriptions.StartExpirySweepWorker. It blocks; the
// caller starts it with go. A tick's failure is its own to log, never fatal.
func runTicker(ctx context.Context, interval time.Duration, tick func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick(ctx)
		}
	}
}
