package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v87"
)

// Plan catalog (#2033 spec §8, plan Task 2). GET /billing/plans shows the four
// sellable prices; Checkout resolves the one price it sells. Both read Stripe
// by lookup_key with active=true, so archiving the prices (the emergency stop,
// runbook) stops Checkout at once and /plans within catalogCacheTTL.

const (
	// catalogCacheTTL is how long /plans trusts a successful catalog read
	// (Mark, Phase-2 gate P3-A). Checkout never uses the cache.
	catalogCacheTTL = 60 * time.Second

	// catalogCurrency is the only currency sold (Phase-2 gate P3-C).
	catalogCurrency = "usd"

	// foundersPercent is founders-50's discount, as integer percent.
	foundersPercent = 50

	plansErrCatalog = "catalog_unavailable"
)

// errPriceUnavailable means the price for a plan and interval is not active in
// Stripe, or does not carry what Concord sells under its lookup_key.
var errPriceUnavailable = errors.New("billing: catalog price unavailable")

// errUnknownPlan means the plan and interval name nothing Concord sells.
var errUnknownPlan = errors.New("billing: unknown plan or interval")

// findOffer returns the catalog entry (bootstrap's catalogPrices, the one
// table of what Concord sells) for plan and interval.
func findOffer(plan, interval string) (catalogPrice, bool) {
	for _, o := range catalogPrices {
		if o.plan == plan && string(o.interval) == interval {
			return o, true
		}
	}
	return catalogPrice{}, false
}

// sellablePrice is one verified catalog entry as Stripe holds it now.
type sellablePrice struct {
	plan, interval, currency string
	unitAmount               int64
}

// catalogCache remembers the last successful catalog. Like the Portal
// configuration cache, only a success is stored.
type catalogCache struct {
	mu      sync.Mutex
	prices  []sellablePrice
	expires time.Time
}

func (cc *catalogCache) get(now time.Time) ([]sellablePrice, bool) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.prices == nil || !now.Before(cc.expires) {
		return nil, false
	}
	return cc.prices, true
}

func (cc *catalogCache) set(prices []sellablePrice, expires time.Time) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.prices, cc.expires = prices, expires
}

// foundersUnitAmount is the founders price of unit, rounded DOWN to a whole
// cent (Mark, Phase-2 gate P3-B): the price shown is never more than what the
// user pays, and no fraction of a cent is hidden.
func foundersUnitAmount(unit int64) int64 {
	return unit * (100 - foundersPercent) / 100
}

// foundersActive reports whether founders pricing applies now: strictly before
// FOUNDERS_PRICING_ENDS_AT, and never when it is unset.
func (s *Service) foundersActive() bool {
	ends := s.Billing.FoundersPricingEndsAt
	return !ends.IsZero() && s.now().Before(ends)
}

// listActivePrices lists the active prices carrying the given lookup keys.
func (s *Service) listActivePrices(ctx context.Context, keys []string) (map[string]*stripe.Price, error) {
	params := &stripe.PriceListParams{Active: stripe.Bool(true), LookupKeys: stripe.StringSlice(keys)}
	found := make(map[string]*stripe.Price, len(keys))
	for p, err := range s.Stripe.V1Prices.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, withStage("list_prices", fmt.Errorf("billing: list catalog prices: %w", err))
		}
		found[p.LookupKey] = p
	}
	return found, nil
}

// verifyPrice checks that p is what o sells. A price that is missing or
// disagrees is errPriceUnavailable, naming the lookup key (not secret).
func verifyPrice(o catalogPrice, p *stripe.Price) error {
	switch {
	case p == nil:
		return fmt.Errorf("%w: no active price has lookup_key %s", errPriceUnavailable, o.lookupKey)
	case p.Metadata[PlanMetadataKey] != o.plan,
		p.Recurring == nil || p.Recurring.Interval != o.interval || p.Recurring.IntervalCount != 1,
		p.Recurring.UsageType != stripe.PriceRecurringUsageTypeLicensed,
		string(p.Currency) != catalogCurrency,
		p.UnitAmount != o.unitAmount:
		return fmt.Errorf("%w: price %s (lookup_key %s) does not match the catalog", errPriceUnavailable, p.ID, o.lookupKey)
	}
	return nil
}

// resolvePrice returns the active price Checkout sells for plan and interval,
// read from Stripe on every call (spec §8 Checkout step 2: uncached).
func (s *Service) resolvePrice(ctx context.Context, plan, interval string) (*stripe.Price, error) {
	o, ok := findOffer(plan, interval)
	if !ok {
		return nil, errUnknownPlan
	}
	found, err := s.listActivePrices(ctx, []string{o.lookupKey})
	if err != nil {
		return nil, err
	}
	p := found[o.lookupKey]
	if err := verifyPrice(o, p); err != nil {
		return nil, err
	}
	return p, nil
}

// catalog returns all four verified prices, from the cache when fresh. Any
// price missing or mismatched fails the whole catalog: /plans never shows a
// partial one. Entries keep catalogPrices' order.
func (s *Service) catalog(ctx context.Context) ([]sellablePrice, error) {
	if prices, ok := s.catalogCache.get(s.now()); ok {
		return prices, nil
	}
	keys := make([]string, 0, len(catalogPrices))
	for _, o := range catalogPrices {
		keys = append(keys, o.lookupKey)
	}
	found, err := s.listActivePrices(ctx, keys)
	if err != nil {
		return nil, err
	}
	prices := make([]sellablePrice, 0, len(catalogPrices))
	for _, o := range catalogPrices {
		p := found[o.lookupKey]
		if err := verifyPrice(o, p); err != nil {
			return nil, err
		}
		prices = append(prices, sellablePrice{plan: o.plan, interval: string(o.interval), currency: string(p.Currency), unitAmount: p.UnitAmount})
	}
	s.catalogCache.set(prices, s.now().Add(catalogCacheTTL))
	return prices, nil
}

// planEntry is one element of the /plans response.
type planEntry struct {
	Plan               string `json:"plan"`
	Interval           string `json:"interval"`
	Currency           string `json:"currency"`
	UnitAmount         int64  `json:"unit_amount"`
	FoundersUnitAmount *int64 `json:"founders_unit_amount,omitempty"`
}

type foundersState struct {
	Active bool    `json:"active"`
	EndsAt *string `json:"ends_at,omitempty"`
}

type plansResponse struct {
	PurchaseAvailable bool          `json:"purchase_available"`
	Founders          foundersState `json:"founders"`
	Plans             []planEntry   `json:"plans"`
}

// Plans is the Gin handler for GET /api/v1/billing/plans.
func (s *Service) Plans(c *gin.Context) {
	userID, err := uuid.Parse(c.GetString("user_id"))
	if err != nil {
		s.Log.Error("billing: plans request has no usable authenticated user id")
		abortWebhook(c, http.StatusUnauthorized, portalErrUnauth)
		return
	}
	prices, err := s.catalog(c.Request.Context())
	if err != nil {
		s.logCatalogFailure(err)
		abortWebhook(c, http.StatusServiceUnavailable, plansErrCatalog)
		return
	}
	resp := plansResponse{PurchaseAvailable: s.purchaseAllowed(userID), Plans: make([]planEntry, 0, len(prices))}
	founders := s.foundersActive()
	if founders {
		ends := s.Billing.FoundersPricingEndsAt.UTC().Format(time.RFC3339)
		resp.Founders = foundersState{Active: true, EndsAt: &ends}
	}
	for _, p := range prices {
		entry := planEntry{Plan: p.plan, Interval: p.interval, Currency: p.currency, UnitAmount: p.unitAmount}
		if founders {
			amount := foundersUnitAmount(p.unitAmount)
			entry.FoundersUnitAmount = &amount
		}
		resp.Plans = append(resp.Plans, entry)
	}
	// purchase_available is per caller.
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, resp)
}

// logCatalogFailure logs why the catalog could not be read. A mismatch names
// the lookup key so the operator can fix the catalog (runbook); a Stripe
// failure logs its class and stage.
func (s *Service) logCatalogFailure(err error) {
	if errors.Is(err, errPriceUnavailable) {
		s.Log.Error("billing: the Stripe price catalog is incomplete or does not match; run the Stripe bootstrap or check for archived prices",
			"detail", err.Error())
		return
	}
	s.Log.Error("billing: reading the Stripe price catalog failed",
		logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
}
