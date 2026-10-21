package billing

import (
	"context"
	"errors"
	"fmt"

	"github.com/stripe/stripe-go/v87"
)

// ErrCatalogConflict marks Stripe state the bootstrap refuses to change: an
// existing object whose immutable field differs from the catalog (a price's
// amount, currency, interval, product or plan metadata; the coupon's
// percent_off or duration), an archived catalog object, or two objects that
// claim one catalog identity. The bootstrap checks every object before it
// writes anything, so a run that returns this error has made no write.
var ErrCatalogConflict = errors.New("billing: catalog conflict")

// ErrReplayedCreate reports that a create answered with an object Stripe no
// longer has. Stripe keeps an Idempotency-Key's response for 24 hours, so a
// run after an operator deleted the product or coupon an earlier run created
// is handed that earlier "created" response back while nothing exists.
var ErrReplayedCreate = errors.New("billing: create replayed an object Stripe no longer has; wait 24 hours after deleting a bootstrap object before re-running")

// foundersPercentOff is the founders coupon's discount (spec §6).
const foundersPercentOff = 50.0

// idempotencyPrefix namespaces the Idempotency-Key of every create. The rest
// of the key is the object's catalog identity, so two runs inside Stripe's
// 24-hour key window send the same key for the same object.
const idempotencyPrefix = "concord-stripe-bootstrap-v1"

// Step kinds in a BootstrapReport.
const (
	KindProduct             = "product"
	KindPrice               = "price"
	KindCoupon              = "coupon"
	KindPortalConfiguration = "portal_configuration"
)

// Action is what a run did, or in a dry run would do, to one catalog object.
type Action string

// The three actions. The bootstrap never deletes.
const (
	ActionUnchanged Action = "unchanged"
	ActionCreate    Action = "create"
	ActionUpdate    Action = "update"
)

// BootstrapOptions configures one Bootstrap run.
type BootstrapOptions struct {
	// DryRun performs the reads and checks only, and reports the writes a
	// real run would make.
	DryRun bool
}

// BootstrapStep is one catalog object's outcome.
type BootstrapStep struct {
	Kind   string // KindProduct, KindPrice, KindCoupon or KindPortalConfiguration
	Key    string // catalog identity: plan, lookup_key, coupon ID or portal metadata version
	ID     string // Stripe ID; empty for a create that a dry run only plans
	Action Action
}

// BootstrapReport lists every catalog object in provisioning order.
type BootstrapReport struct {
	DryRun bool
	Steps  []BootstrapStep
}

// Count returns the number of steps whose action is a.
func (r BootstrapReport) Count(a Action) int {
	n := 0
	for _, s := range r.Steps {
		if s.Action == a {
			n++
		}
	}
	return n
}

type catalogProduct struct {
	plan string
	name string
}

type catalogPrice struct {
	lookupKey  string
	plan       string
	unitAmount int64 // USD cents
	interval   stripe.PriceRecurringInterval
}

// catalogProducts and catalogPrices are the catalog (spec §6), in
// provisioning order. The Portal lists products and prices in this order.
var catalogProducts = []catalogProduct{
	{plan: PlanSupersonic, name: "Supersonic"},
	{plan: PlanWing, name: "Wing"},
}

var catalogPrices = []catalogPrice{
	{lookupKey: LookupKeySupersonicMonthly, plan: PlanSupersonic, unitAmount: 899, interval: stripe.PriceRecurringIntervalMonth},
	{lookupKey: LookupKeySupersonicYearly, plan: PlanSupersonic, unitAmount: 9889, interval: stripe.PriceRecurringIntervalYear},
	{lookupKey: LookupKeyWingMonthly, plan: PlanWing, unitAmount: 1499, interval: stripe.PriceRecurringIntervalMonth},
	{lookupKey: LookupKeyWingYearly, plan: PlanWing, unitAmount: 16489, interval: stripe.PriceRecurringIntervalYear},
}

// Portal values shared by create and update. stripe-go has distinct create
// and update param types, so the shared values are plain strings.
var (
	portalDefaultAllowedUpdates = []string{
		string(stripe.BillingPortalConfigurationFeaturesSubscriptionUpdateDefaultAllowedUpdatePrice),
	}
	portalCustomerAllowedUpdates = []string{
		string(stripe.BillingPortalConfigurationFeaturesCustomerUpdateAllowedUpdateEmail),
		string(stripe.BillingPortalConfigurationFeaturesCustomerUpdateAllowedUpdateAddress),
	}
)

// catalogState is what the read phase found in the account.
type catalogState struct {
	products map[string]*stripe.Product         // by plan (metadata concord_product)
	prices   map[string]*stripe.Price           // by lookup_key
	coupon   *stripe.Coupon                     // nil when absent
	portal   *stripe.BillingPortalConfiguration // nil when absent
}

// bootstrapRun carries one run's client, mode, state and report.
type bootstrapRun struct {
	c      *stripe.Client
	dryRun bool
	state  *catalogState
	report *BootstrapReport
}

// portalPlan is one product's entry in the Portal's plan switcher.
type portalPlan struct {
	product string
	prices  []string
}

// Bootstrap provisions the Stripe catalog of spec §6 idempotently: products
// Supersonic and Wing, the four prices, the founders-50 coupon and the
// Customer Portal configuration marked concord_portal=v1.
//
// It reads everything first and checks every existing object against the
// catalog. Any ErrCatalogConflict stops the run before its first write. It
// then creates only what is missing, each create carrying an idempotency key
// derived from the object's identity, and updates the owned Portal
// configuration to the desired features. It never changes an immutable field,
// never deletes, never reactivates an archived product or price, and never
// touches a Portal configuration without the concord_portal=v1 metadata.
//
// The report lists every object in provisioning order. On error it holds the
// steps completed before the failure.
func Bootstrap(ctx context.Context, c *stripe.Client, opts BootstrapOptions) (BootstrapReport, error) {
	report := BootstrapReport{DryRun: opts.DryRun}
	state, err := readCatalog(ctx, c)
	if err != nil {
		return report, err
	}
	if err = state.validate(); err != nil {
		return report, err
	}
	run := &bootstrapRun{c: c, dryRun: opts.DryRun, state: state, report: &report}
	for _, ensure := range []func(context.Context) error{
		run.ensureProducts, run.ensurePrices, run.ensureCoupon, run.ensurePortalConfiguration,
	} {
		if err = ensure(ctx); err != nil {
			return report, err
		}
	}
	return report, nil
}

func readCatalog(ctx context.Context, c *stripe.Client) (*catalogState, error) {
	products, err := findProducts(ctx, c)
	if err != nil {
		return nil, err
	}
	prices, err := findPrices(ctx, c)
	if err != nil {
		return nil, err
	}
	coupon, err := findCoupon(ctx, c)
	if err != nil {
		return nil, err
	}
	portal, err := findPortalConfiguration(ctx, c)
	if err != nil {
		return nil, err
	}
	return &catalogState{products: products, prices: prices, coupon: coupon, portal: portal}, nil
}

// findProducts lists every product, active or archived, and keeps those whose
// concord_product metadata is set. No list filter exists for metadata.
func findProducts(ctx context.Context, c *stripe.Client) (map[string]*stripe.Product, error) {
	found := make(map[string]*stripe.Product, len(catalogProducts))
	params := &stripe.ProductListParams{}
	params.Limit = stripe.Int64(100)
	for p, err := range c.V1Products.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, fmt.Errorf("billing: bootstrap: list products: %w", err)
		}
		plan := p.Metadata[ProductMetadataKey]
		if plan == "" {
			continue
		}
		if prev, dup := found[plan]; dup {
			return nil, fmt.Errorf("%w: products %s and %s both carry metadata %s=%s",
				ErrCatalogConflict, prev.ID, p.ID, ProductMetadataKey, plan)
		}
		found[plan] = p
	}
	return found, nil
}

// findPrices looks the four prices up by lookup_key. Stripe lists active prices
// only unless active is set (products and Portal configurations list both
// states), so it lists twice, once with active=true and once with active=false,
// and merges: an archived price must reach validate, or the bootstrap would
// treat its lookup_key as free and try to create a second price. A lookup_key
// that appears more than once across the two lists is a conflict.
func findPrices(ctx context.Context, c *stripe.Client) (map[string]*stripe.Price, error) {
	keys := make([]string, 0, len(catalogPrices))
	for _, want := range catalogPrices {
		keys = append(keys, want.lookupKey)
	}
	found := make(map[string]*stripe.Price, len(catalogPrices))
	for _, active := range []bool{true, false} {
		params := &stripe.PriceListParams{Active: stripe.Bool(active), LookupKeys: stripe.StringSlice(keys)}
		for p, err := range c.V1Prices.List(ctx, params).All(ctx) {
			if err != nil {
				return nil, fmt.Errorf("billing: bootstrap: list prices: %w", err)
			}
			if prev, dup := found[p.LookupKey]; dup {
				return nil, fmt.Errorf("%w: prices %s and %s both carry lookup_key %s",
					ErrCatalogConflict, prev.ID, p.ID, p.LookupKey)
			}
			found[p.LookupKey] = p
		}
	}
	return found, nil
}

// findCoupon retrieves founders-50. Only resource_missing means absent; any
// other error stops the run, so a Stripe outage can never look like "create".
// applies_to is expandable and absent from a plain retrieve, so it is expanded
// here: without it couponMismatch could never see a product restriction.
func findCoupon(ctx context.Context, c *stripe.Client) (*stripe.Coupon, error) {
	params := &stripe.CouponRetrieveParams{}
	params.AddExpand("applies_to")
	coupon, err := c.V1Coupons.Retrieve(ctx, CouponFoundersID, params)
	switch {
	case err == nil:
		return coupon, nil
	case isResourceMissing(err):
		return nil, nil
	default:
		return nil, fmt.Errorf("billing: bootstrap: retrieve coupon %s: %w", CouponFoundersID, err)
	}
}

// findPortalConfiguration lists every Portal configuration and keeps the one
// marked concord_portal=v1. No list filter exists for metadata.
func findPortalConfiguration(ctx context.Context, c *stripe.Client) (*stripe.BillingPortalConfiguration, error) {
	var found *stripe.BillingPortalConfiguration
	params := &stripe.BillingPortalConfigurationListParams{}
	params.Limit = stripe.Int64(100)
	for pc, err := range c.V1BillingPortalConfigurations.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, fmt.Errorf("billing: bootstrap: list portal configurations: %w", err)
		}
		if pc.Metadata[PortalMetadataKey] != PortalMetadataVersion {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: portal configurations %s and %s both carry metadata %s=%s",
				ErrCatalogConflict, found.ID, pc.ID, PortalMetadataKey, PortalMetadataVersion)
		}
		found = pc
	}
	return found, nil
}

func isResourceMissing(err error) bool {
	var se *stripe.Error
	return errors.As(err, &se) && se.Code == stripe.ErrorCodeResourceMissing
}

// validate checks every existing catalog object before any write.
func (s *catalogState) validate() error {
	for _, want := range catalogProducts {
		if p := s.products[want.plan]; p != nil && !p.Active {
			return conflict(KindProduct, p.ID, "active")
		}
	}
	for _, want := range catalogPrices {
		p := s.prices[want.lookupKey]
		if p == nil {
			continue
		}
		if field := priceMismatch(p, want, s.productID(want.plan)); field != "" {
			return conflict(KindPrice, p.ID, field)
		}
	}
	if s.coupon != nil {
		if field := couponMismatch(s.coupon); field != "" {
			return conflict(KindCoupon, s.coupon.ID, field)
		}
	}
	return nil
}

// priceMismatch names the first field of an existing price that differs from
// the catalog, or returns "". productID is "" when the product does not exist
// yet, and then any existing price for the key belongs to another product.
func priceMismatch(p *stripe.Price, want catalogPrice, productID string) string {
	switch {
	case !p.Active:
		return "active"
	case p.Product == nil || p.Product.ID != productID:
		return "product"
	case p.Currency != stripe.CurrencyUSD:
		return "currency"
	case p.UnitAmount != want.unitAmount:
		return "unit_amount"
	case p.Recurring == nil || p.Recurring.Interval != want.interval:
		return "recurring.interval"
	case p.Recurring.IntervalCount != 1:
		return "recurring.interval_count"
	case p.Recurring.UsageType != stripe.PriceRecurringUsageTypeLicensed:
		return "recurring.usage_type"
	case p.Metadata[PlanMetadataKey] != want.plan:
		return "metadata." + PlanMetadataKey
	}
	return ""
}

// couponMismatch names the first field of the existing founders coupon that
// differs from the catalog, or returns "". Beyond the discount and duration,
// the coupon must be unrestricted (no product list, no redeem-by date, no
// redemption cap) and still redeemable: any of those would change who can use
// the founders rate, and the bootstrap never edits a coupon.
func couponMismatch(c *stripe.Coupon) string {
	switch {
	case c.PercentOff != foundersPercentOff:
		return "percent_off"
	case c.Duration != stripe.CouponDurationForever:
		return "duration"
	case c.AppliesTo != nil && len(c.AppliesTo.Products) > 0:
		return "applies_to"
	case c.RedeemBy != 0:
		return "redeem_by"
	case c.MaxRedemptions != 0:
		return "max_redemptions"
	case !c.Valid:
		return "valid"
	}
	return ""
}

func conflict(kind, id, field string) error {
	return fmt.Errorf("%w: %s %s: %s differs from the catalog, and the bootstrap never changes it",
		ErrCatalogConflict, kind, id, field)
}

func (s *catalogState) productID(plan string) string {
	if p := s.products[plan]; p != nil {
		return p.ID
	}
	return ""
}

func idempotencyKey(kind, identity string) string {
	return idempotencyPrefix + ":" + kind + ":" + identity
}

// confirmCreated reads the error from retrieving an object a create just
// returned. The read phase found the object absent, so resource_missing here
// means the create was an idempotent replay of a deleted object. Only
// products and coupons are checked: prices and Portal configurations cannot
// be deleted, only archived, so a replay of one still returns a live object.
func confirmCreated(retrieveErr error) error {
	switch {
	case retrieveErr == nil:
		return nil
	case isResourceMissing(retrieveErr):
		return ErrReplayedCreate
	default:
		return fmt.Errorf("confirm create: %w", retrieveErr)
	}
}

func (r *bootstrapRun) record(kind, key, id string, action Action) {
	r.report.Steps = append(r.report.Steps, BootstrapStep{Kind: kind, Key: key, ID: id, Action: action})
}

func (r *bootstrapRun) ensureProducts(ctx context.Context) error {
	for _, want := range catalogProducts {
		if p := r.state.products[want.plan]; p != nil {
			r.record(KindProduct, want.plan, p.ID, ActionUnchanged)
			continue
		}
		if r.dryRun {
			r.record(KindProduct, want.plan, "", ActionCreate)
			continue
		}
		params := &stripe.ProductCreateParams{
			Name:     stripe.String(want.name),
			Metadata: map[string]string{ProductMetadataKey: want.plan},
		}
		params.SetIdempotencyKey(idempotencyKey(KindProduct, want.plan))
		p, err := r.c.V1Products.Create(ctx, params)
		if err != nil {
			return fmt.Errorf("billing: bootstrap: create product %s: %w", want.plan, err)
		}
		_, err = r.c.V1Products.Retrieve(ctx, p.ID, nil)
		if err := confirmCreated(err); err != nil {
			return fmt.Errorf("billing: bootstrap: create product %s: %w", want.plan, err)
		}
		r.state.products[want.plan] = p
		r.record(KindProduct, want.plan, p.ID, ActionCreate)
	}
	return nil
}

func (r *bootstrapRun) ensurePrices(ctx context.Context) error {
	for _, want := range catalogPrices {
		if p := r.state.prices[want.lookupKey]; p != nil {
			r.record(KindPrice, want.lookupKey, p.ID, ActionUnchanged)
			continue
		}
		if r.dryRun {
			r.record(KindPrice, want.lookupKey, "", ActionCreate)
			continue
		}
		p, err := r.c.V1Prices.Create(ctx, newPriceParams(want, r.state.productID(want.plan)))
		if err != nil {
			return fmt.Errorf("billing: bootstrap: create price %s: %w", want.lookupKey, err)
		}
		r.state.prices[want.lookupKey] = p
		r.record(KindPrice, want.lookupKey, p.ID, ActionCreate)
	}
	return nil
}

// newPriceParams builds a price create. TaxBehavior stays unset until D8 is
// decided: an unspecified behaviour can be set once later, but inclusive or
// exclusive can never change. TransferLookupKey stays unset, so a create can
// never take a lookup_key from an archived price (the archive-prices
// emergency stop) and quietly reopen sales; Stripe rejects the create instead.
func newPriceParams(want catalogPrice, productID string) *stripe.PriceCreateParams {
	params := &stripe.PriceCreateParams{
		Currency:   stripe.String(stripe.CurrencyUSD),
		LookupKey:  stripe.String(want.lookupKey),
		Metadata:   map[string]string{PlanMetadataKey: want.plan},
		Product:    stripe.String(productID),
		Recurring:  &stripe.PriceCreateRecurringParams{Interval: stripe.String(want.interval)},
		UnitAmount: stripe.Int64(want.unitAmount),
	}
	params.SetIdempotencyKey(idempotencyKey(KindPrice, want.lookupKey))
	return params
}

func (r *bootstrapRun) ensureCoupon(ctx context.Context) error {
	if r.state.coupon != nil {
		r.record(KindCoupon, CouponFoundersID, r.state.coupon.ID, ActionUnchanged)
		return nil
	}
	if r.dryRun {
		r.record(KindCoupon, CouponFoundersID, "", ActionCreate)
		return nil
	}
	params := &stripe.CouponCreateParams{
		Duration:   stripe.String(stripe.CouponDurationForever),
		ID:         stripe.String(CouponFoundersID),
		PercentOff: stripe.Float64(foundersPercentOff),
	}
	params.SetIdempotencyKey(idempotencyKey(KindCoupon, CouponFoundersID))
	coupon, err := r.c.V1Coupons.Create(ctx, params)
	if err != nil {
		return fmt.Errorf("billing: bootstrap: create coupon %s: %w", CouponFoundersID, err)
	}
	_, err = r.c.V1Coupons.Retrieve(ctx, coupon.ID, nil)
	if err := confirmCreated(err); err != nil {
		return fmt.Errorf("billing: bootstrap: create coupon %s: %w", CouponFoundersID, err)
	}
	r.record(KindCoupon, CouponFoundersID, coupon.ID, ActionCreate)
	return nil
}

// ensurePortalConfiguration creates the owned configuration, or updates it to
// the desired features on every run. Updates carry no identity key: the
// desired body changes whenever a price ID does, and a reused key with a new
// body is an idempotency error. The SDK adds a random key per call instead.
func (r *bootstrapRun) ensurePortalConfiguration(ctx context.Context) error {
	existing := r.state.portal
	switch {
	case r.dryRun && existing != nil:
		r.record(KindPortalConfiguration, PortalMetadataVersion, existing.ID, ActionUpdate)
		return nil
	case r.dryRun:
		r.record(KindPortalConfiguration, PortalMetadataVersion, "", ActionCreate)
		return nil
	case existing != nil:
		pc, err := r.c.V1BillingPortalConfigurations.Update(ctx, existing.ID, newPortalUpdateParams(r.portalPlans()))
		if err != nil {
			return fmt.Errorf("billing: bootstrap: update portal configuration %s: %w", existing.ID, err)
		}
		r.record(KindPortalConfiguration, PortalMetadataVersion, pc.ID, ActionUpdate)
		return nil
	}
	pc, err := r.c.V1BillingPortalConfigurations.Create(ctx, newPortalCreateParams(r.portalPlans()))
	if err != nil {
		return fmt.Errorf("billing: bootstrap: create portal configuration: %w", err)
	}
	r.record(KindPortalConfiguration, PortalMetadataVersion, pc.ID, ActionCreate)
	return nil
}

// portalPlans returns both products, each with its prices in catalog order
// (monthly, then yearly). It runs after every product and price exists.
func (r *bootstrapRun) portalPlans() []portalPlan {
	plans := make([]portalPlan, 0, len(catalogProducts))
	for _, product := range catalogProducts {
		plan := portalPlan{product: r.state.productID(product.plan)}
		for _, price := range catalogPrices {
			if price.plan == product.plan {
				plan.prices = append(plan.prices, r.state.prices[price.lookupKey].ID)
			}
		}
		plans = append(plans, plan)
	}
	return plans
}

// newPortalCreateParams builds the spec §6 configuration: plan switching
// between the four prices with quantity fixed, always_invoice proration and
// no scheduled downgrades (D16(b)); cancel at period end without proration;
// payment method update; invoice history; email and address updates. Price
// is the only allowed update, so the Portal offers no promotion codes. No
// default_return_url: every Portal session passes its own return_url.
func newPortalCreateParams(plans []portalPlan) *stripe.BillingPortalConfigurationCreateParams {
	products := make([]*stripe.BillingPortalConfigurationCreateFeaturesSubscriptionUpdateProductParams, 0, len(plans))
	for _, plan := range plans {
		products = append(products, &stripe.BillingPortalConfigurationCreateFeaturesSubscriptionUpdateProductParams{
			AdjustableQuantity: &stripe.BillingPortalConfigurationCreateFeaturesSubscriptionUpdateProductAdjustableQuantityParams{
				Enabled: stripe.Bool(false),
			},
			Prices:  stripe.StringSlice(plan.prices),
			Product: stripe.String(plan.product),
		})
	}
	params := &stripe.BillingPortalConfigurationCreateParams{
		Features: &stripe.BillingPortalConfigurationCreateFeaturesParams{
			CustomerUpdate: &stripe.BillingPortalConfigurationCreateFeaturesCustomerUpdateParams{
				AllowedUpdates: stripe.StringSlice(portalCustomerAllowedUpdates),
				Enabled:        stripe.Bool(true),
			},
			InvoiceHistory:      &stripe.BillingPortalConfigurationCreateFeaturesInvoiceHistoryParams{Enabled: stripe.Bool(true)},
			PaymentMethodUpdate: &stripe.BillingPortalConfigurationCreateFeaturesPaymentMethodUpdateParams{Enabled: stripe.Bool(true)},
			SubscriptionCancel: &stripe.BillingPortalConfigurationCreateFeaturesSubscriptionCancelParams{
				Enabled:           stripe.Bool(true),
				Mode:              stripe.String(stripe.BillingPortalConfigurationFeaturesSubscriptionCancelModeAtPeriodEnd),
				ProrationBehavior: stripe.String(stripe.BillingPortalConfigurationFeaturesSubscriptionCancelProrationBehaviorNone),
			},
			SubscriptionUpdate: &stripe.BillingPortalConfigurationCreateFeaturesSubscriptionUpdateParams{
				DefaultAllowedUpdates: stripe.StringSlice(portalDefaultAllowedUpdates),
				Enabled:               stripe.Bool(true),
				Products:              products,
				ProrationBehavior:     stripe.String(stripe.BillingPortalConfigurationFeaturesSubscriptionUpdateProrationBehaviorAlwaysInvoice),
			},
		},
		Metadata: map[string]string{PortalMetadataKey: PortalMetadataVersion},
	}
	params.SetIdempotencyKey(idempotencyKey(KindPortalConfiguration, PortalMetadataVersion))
	return params
}

// newPortalUpdateParams sends the same features as newPortalCreateParams,
// reactivates the configuration, and clears any schedule_at_period_end
// conditions someone added in the Dashboard: a scheduled downgrade blocks
// cancellation (D16).
func newPortalUpdateParams(plans []portalPlan) *stripe.BillingPortalConfigurationUpdateParams {
	products := make([]*stripe.BillingPortalConfigurationUpdateFeaturesSubscriptionUpdateProductParams, 0, len(plans))
	for _, plan := range plans {
		products = append(products, &stripe.BillingPortalConfigurationUpdateFeaturesSubscriptionUpdateProductParams{
			AdjustableQuantity: &stripe.BillingPortalConfigurationUpdateFeaturesSubscriptionUpdateProductAdjustableQuantityParams{
				Enabled: stripe.Bool(false),
			},
			Prices:  stripe.StringSlice(plan.prices),
			Product: stripe.String(plan.product),
		})
	}
	schedule := &stripe.BillingPortalConfigurationUpdateFeaturesSubscriptionUpdateScheduleAtPeriodEndParams{}
	schedule.AddUnsetField(stripe.BillingPortalConfigurationUpdateFeaturesSubscriptionUpdateScheduleAtPeriodEndParamsUnsetFieldConditions)
	return &stripe.BillingPortalConfigurationUpdateParams{
		Active: stripe.Bool(true),
		Features: &stripe.BillingPortalConfigurationUpdateFeaturesParams{
			CustomerUpdate: &stripe.BillingPortalConfigurationUpdateFeaturesCustomerUpdateParams{
				AllowedUpdates: stripe.StringSlice(portalCustomerAllowedUpdates),
				Enabled:        stripe.Bool(true),
			},
			InvoiceHistory:      &stripe.BillingPortalConfigurationUpdateFeaturesInvoiceHistoryParams{Enabled: stripe.Bool(true)},
			PaymentMethodUpdate: &stripe.BillingPortalConfigurationUpdateFeaturesPaymentMethodUpdateParams{Enabled: stripe.Bool(true)},
			SubscriptionCancel: &stripe.BillingPortalConfigurationUpdateFeaturesSubscriptionCancelParams{
				Enabled:           stripe.Bool(true),
				Mode:              stripe.String(stripe.BillingPortalConfigurationFeaturesSubscriptionCancelModeAtPeriodEnd),
				ProrationBehavior: stripe.String(stripe.BillingPortalConfigurationFeaturesSubscriptionCancelProrationBehaviorNone),
			},
			SubscriptionUpdate: &stripe.BillingPortalConfigurationUpdateFeaturesSubscriptionUpdateParams{
				DefaultAllowedUpdates: stripe.StringSlice(portalDefaultAllowedUpdates),
				Enabled:               stripe.Bool(true),
				Products:              products,
				ProrationBehavior:     stripe.String(stripe.BillingPortalConfigurationFeaturesSubscriptionUpdateProrationBehaviorAlwaysInvoice),
				ScheduleAtPeriodEnd:   schedule,
			},
		},
		Metadata: map[string]string{PortalMetadataKey: PortalMetadataVersion},
	}
}
