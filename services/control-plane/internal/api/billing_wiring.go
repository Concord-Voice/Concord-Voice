package api

import (
	"context"
	"database/sql"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/billing"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/subscriptions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Stripe billing wiring (#2033 PR-2). Billing is dormant by default: with no
// keys, or on a self-hosted instance, no route and no worker exists and
// /subscriptions/me reports why.

// billingState is the value /subscriptions/me carries in its billing field.
// Keys alone make it "available"; whether a purchase can start is /plans (PR-3).
func billingState(cfg *config.Config) string {
	switch {
	case config.IsSelfHostedInstance(cfg.InstanceType):
		return subscriptions.BillingSelfHosted
	case cfg.Billing.KeyMode == config.StripeKeyModeNone:
		return subscriptions.BillingNotConfigured
	default:
		return subscriptions.BillingAvailable
	}
}

// wireBilling builds the billing Service, or returns nil when billing is not
// configured or the instance is self-hosted. It starts nothing: the caller
// starts the workers with startBillingWorkers once the dependencies are live,
// so constructing a Service never spawns a goroutine against a database it
// was not given.
func wireBilling(
	_ context.Context,
	cfg *config.Config,
	db *sql.DB,
	cache *entitlements.Cache,
	notifier entitlements.SessionNotifier,
	counters *opsmetrics.Counters,
	log *logger.Logger,
) *billing.Service {
	if billingState(cfg) != subscriptions.BillingAvailable {
		return nil
	}
	return billing.NewService(billing.Deps{
		DB:          db,
		Stripe:      billing.NewClient(cfg.Billing.SecretKey, ""),
		Billing:     cfg.Billing,
		Environment: cfg.Environment,
		Changer:     billing.NewTierChanger(cache, notifier),
		Counters:    counters,
		Log:         log,
	})
}

// startBillingWorkers runs the erasure-outbox and drift workers until ctx is
// cancelled, whatever the purchase mode. Both block, so each gets a goroutine.
// A nil svc starts nothing.
func startBillingWorkers(ctx context.Context, svc *billing.Service) {
	if svc == nil {
		return
	}
	go svc.RunOutboxWorker(ctx)
	go svc.RunDriftWorker(ctx)
}
