package billing

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"
)

func wantProductForm(name, plan string) url.Values {
	return url.Values{"name": {name}, "metadata[concord_product]": {plan}}
}

// wantPriceForm is a price create's complete body. Exact equality is the
// assertion that tax_behavior and transfer_lookup_key are absent.
func wantPriceForm(lookupKey, plan, product, unitAmount, interval string) url.Values {
	return url.Values{
		"currency":               {"usd"},
		"lookup_key":             {lookupKey},
		"metadata[concord_plan]": {plan},
		"product":                {product},
		"recurring[interval]":    {interval},
		"unit_amount":            {unitAmount},
	}
}

// wantPortalCreateForm is the spec §6 Portal configuration body.
// Plan switching is disabled, so it needs no product or price IDs.
func wantPortalCreateForm() url.Values {
	return url.Values{
		"features[customer_update][allowed_updates][0]":     {"email"},
		"features[customer_update][allowed_updates][1]":     {"address"},
		"features[customer_update][enabled]":                {"true"},
		"features[invoice_history][enabled]":                {"true"},
		"features[payment_method_update][enabled]":          {"true"},
		"features[subscription_cancel][enabled]":            {"true"},
		"features[subscription_cancel][mode]":               {"at_period_end"},
		"features[subscription_cancel][proration_behavior]": {"none"},
		"features[subscription_update][enabled]":            {"false"},
		"metadata[concord_portal]":                          {"v1"},
	}
}

// wantPortalUpdateForm adds what only the update sends: reactivation.
func wantPortalUpdateForm() url.Values {
	form := wantPortalCreateForm()
	form["active"] = []string{"true"}
	return form
}

// assertNoForbiddenPortalKeys restates spec §6's negative rules on any Portal
// body: no promotion codes and no scheduled downgrades.
func assertNoForbiddenPortalKeys(t *testing.T, form url.Values) {
	t.Helper()
	for k, v := range form {
		assert.NotContains(t, k, "promotion_code", "Portal must offer no promotion codes")
		for _, value := range v {
			assert.NotEqual(t, "promotion_code", value, "Portal must offer no promotion codes (key %s)", k)
		}
		if strings.Contains(k, "schedule_at_period_end") {
			assert.Equal(t, []string{""}, v, "schedule_at_period_end may only be cleared, never set (key %s)", k)
		}
	}
}

func TestBootstrap_FreshAccountCreatesEverything(t *testing.T) {
	t.Parallel()
	f := newFakeStripe(t)

	report, err := Bootstrap(t.Context(), f.client(), BootstrapOptions{})
	require.NoError(t, err)

	assert.False(t, report.DryRun)
	assert.Equal(t, []BootstrapStep{
		{Kind: KindProduct, Key: PlanSupersonic, ID: "prod_1", Action: ActionCreate},
		{Kind: KindProduct, Key: PlanWing, ID: "prod_2", Action: ActionCreate},
		{Kind: KindPrice, Key: LookupKeySupersonicMonthly, ID: "price_1", Action: ActionCreate},
		{Kind: KindPrice, Key: LookupKeySupersonicYearly, ID: "price_2", Action: ActionCreate},
		{Kind: KindPrice, Key: LookupKeyWingMonthly, ID: "price_3", Action: ActionCreate},
		{Kind: KindPrice, Key: LookupKeyWingYearly, ID: "price_4", Action: ActionCreate},
		{Kind: KindCoupon, Key: CouponFoundersID, ID: CouponFoundersID, Action: ActionCreate},
		{Kind: KindPortalConfiguration, Key: PortalMetadataVersion, ID: "bpc_1", Action: ActionCreate},
	}, report.Steps)
	assert.Equal(t, 8, report.Count(ActionCreate))

	assert.Equal(t, []fakeRequest{
		{Method: http.MethodPost, Path: "/v1/products", IdempotencyKey: "concord-stripe-bootstrap-v1:product:supersonic",
			Form: wantProductForm("Supersonic", "supersonic")},
		{Method: http.MethodPost, Path: "/v1/products", IdempotencyKey: "concord-stripe-bootstrap-v1:product:wing",
			Form: wantProductForm("Wing", "wing")},
		{Method: http.MethodPost, Path: "/v1/prices", IdempotencyKey: "concord-stripe-bootstrap-v1:price:supersonic_monthly",
			Form: wantPriceForm("supersonic_monthly", "supersonic", "prod_1", "899", "month")},
		{Method: http.MethodPost, Path: "/v1/prices", IdempotencyKey: "concord-stripe-bootstrap-v1:price:supersonic_yearly",
			Form: wantPriceForm("supersonic_yearly", "supersonic", "prod_1", "9889", "year")},
		{Method: http.MethodPost, Path: "/v1/prices", IdempotencyKey: "concord-stripe-bootstrap-v1:price:wing_monthly",
			Form: wantPriceForm("wing_monthly", "wing", "prod_2", "1499", "month")},
		{Method: http.MethodPost, Path: "/v1/prices", IdempotencyKey: "concord-stripe-bootstrap-v1:price:wing_yearly",
			Form: wantPriceForm("wing_yearly", "wing", "prod_2", "16489", "year")},
		{Method: http.MethodPost, Path: "/v1/coupons", IdempotencyKey: "concord-stripe-bootstrap-v1:coupon:founders-50",
			Form: url.Values{"duration": {"forever"}, "id": {"founders-50"}, "percent_off": {"50.0000"}}},
		{Method: http.MethodPost, Path: "/v1/billing_portal/configurations", IdempotencyKey: "concord-stripe-bootstrap-v1:portal_configuration:v1",
			Form: wantPortalCreateForm()},
	}, f.writes())

	writes := f.writes()
	for _, w := range writes[2:6] {
		assert.NotContains(t, w.Form, "tax_behavior", "D8: tax_behavior stays unspecified")
		assert.NotContains(t, w.Form, "transfer_lookup_key")
	}
	assertNoForbiddenPortalKeys(t, writes[7].Form)
}

func TestBootstrap_SecondRunCreatesNothing(t *testing.T) {
	t.Parallel()
	f := newFakeStripe(t)
	c := f.client()
	_, err := Bootstrap(t.Context(), c, BootstrapOptions{})
	require.NoError(t, err)
	firstRunWrites := len(f.writes())

	report, err := Bootstrap(t.Context(), c, BootstrapOptions{})
	require.NoError(t, err)

	second := f.writes()[firstRunWrites:]
	require.Len(t, second, 1, "a second run may only re-assert the Portal configuration")
	assert.Equal(t, http.MethodPost, second[0].Method)
	assert.Equal(t, "/v1/billing_portal/configurations/bpc_1", second[0].Path)
	assert.Equal(t, wantPortalUpdateForm(), second[0].Form)
	assert.NotEmpty(t, second[0].IdempotencyKey, "the SDK sends its own random key on an update")
	assert.False(t, strings.HasPrefix(second[0].IdempotencyKey, idempotencyPrefix),
		"an update must not reuse an identity key: its body changes when a price ID does")
	assertNoForbiddenPortalKeys(t, second[0].Form)

	assert.Equal(t, 0, report.Count(ActionCreate))
	assert.Equal(t, 7, report.Count(ActionUnchanged))
	assert.Equal(t, 1, report.Count(ActionUpdate))
}

// TestBootstrap_PartiallyProvisionedAccount is Review Focus RF5: products
// present, two of four prices missing, and a Portal configuration without the
// concord_portal metadata (the Dashboard default). Only the missing objects
// are created, and the unrelated configuration is never written.
func TestBootstrap_PartiallyProvisionedAccount(t *testing.T) {
	t.Parallel()
	f := newFakeStripe(t)
	f.seedProduct("prod_S", PlanSupersonic)
	f.seedProduct("prod_W", PlanWing)
	f.seedProduct("prod_unrelated", "")
	f.seedPrice("price_SM", LookupKeySupersonicMonthly, PlanSupersonic, "prod_S", 899, "month")
	f.seedPrice("price_WY", LookupKeyWingYearly, PlanWing, "prod_W", 16489, "year")
	f.seedCoupon()
	f.seedPortal("bpc_dashboard_default", map[string]string{}).IsDefault = true

	report, err := Bootstrap(t.Context(), f.client(), BootstrapOptions{})
	require.NoError(t, err)

	assert.Equal(t, []BootstrapStep{
		{Kind: KindProduct, Key: PlanSupersonic, ID: "prod_S", Action: ActionUnchanged},
		{Kind: KindProduct, Key: PlanWing, ID: "prod_W", Action: ActionUnchanged},
		{Kind: KindPrice, Key: LookupKeySupersonicMonthly, ID: "price_SM", Action: ActionUnchanged},
		{Kind: KindPrice, Key: LookupKeySupersonicYearly, ID: "price_1", Action: ActionCreate},
		{Kind: KindPrice, Key: LookupKeyWingMonthly, ID: "price_2", Action: ActionCreate},
		{Kind: KindPrice, Key: LookupKeyWingYearly, ID: "price_WY", Action: ActionUnchanged},
		{Kind: KindCoupon, Key: CouponFoundersID, ID: CouponFoundersID, Action: ActionUnchanged},
		{Kind: KindPortalConfiguration, Key: PortalMetadataVersion, ID: "bpc_1", Action: ActionCreate},
	}, report.Steps)

	writes := f.writes()
	require.Len(t, writes, 3)
	assert.Equal(t, "/v1/prices", writes[0].Path)
	assert.Equal(t, wantPriceForm("supersonic_yearly", "supersonic", "prod_S", "9889", "year"), writes[0].Form)
	assert.Equal(t, "/v1/prices", writes[1].Path)
	assert.Equal(t, wantPriceForm("wing_monthly", "wing", "prod_W", "1499", "month"), writes[1].Form)
	assert.Equal(t, "/v1/billing_portal/configurations", writes[2].Path)
	assert.Equal(t, wantPortalCreateForm(), writes[2].Form)
	for _, w := range writes {
		assert.NotEqual(t, "/v1/billing_portal/configurations/bpc_dashboard_default", w.Path,
			"a configuration without concord_portal=v1 is never changed")
		assert.NotEqual(t, "/v1/products", w.Path, "existing products are never re-created")
	}
}

func TestBootstrap_RefusesConflictsWithoutWriting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mutate    func(f *fakeStripe, s seededCatalog)
		wantParts []string
	}{
		{"price unit_amount", func(_ *fakeStripe, s seededCatalog) { s.prices[LookupKeySupersonicMonthly].UnitAmount = 999 },
			[]string{"price price_SM", "unit_amount"}},
		{"price currency", func(_ *fakeStripe, s seededCatalog) { s.prices[LookupKeySupersonicMonthly].Currency = "eur" },
			[]string{"price price_SM", "currency"}},
		{"price interval", func(_ *fakeStripe, s seededCatalog) { s.prices[LookupKeyWingMonthly].Recurring.Interval = "year" },
			[]string{"price price_WM", "recurring.interval"}},
		{"price interval_count", func(_ *fakeStripe, s seededCatalog) { s.prices[LookupKeyWingYearly].Recurring.IntervalCount = 2 },
			[]string{"price price_WY", "recurring.interval_count"}},
		{"price product", func(_ *fakeStripe, s seededCatalog) { s.prices[LookupKeySupersonicYearly].Product = "prod_W" },
			[]string{"price price_SY", "product"}},
		{"price plan metadata", func(_ *fakeStripe, s seededCatalog) {
			s.prices[LookupKeyWingMonthly].Metadata[PlanMetadataKey] = PlanSupersonic
		},
			[]string{"price price_WM", "metadata.concord_plan"}},
		{"archived price", func(_ *fakeStripe, s seededCatalog) { s.prices[LookupKeySupersonicMonthly].Active = false },
			[]string{"price price_SM", "active"}},
		{"archived product", func(_ *fakeStripe, s seededCatalog) { s.wing.Active = false },
			[]string{"product prod_W", "active"}},
		{"price whose product lost its catalog metadata", func(_ *fakeStripe, s seededCatalog) { delete(s.wing.Metadata, ProductMetadataKey) },
			[]string{"price price_WM", "product"}},
		{"price usage_type", func(_ *fakeStripe, s seededCatalog) { s.prices[LookupKeyWingYearly].Recurring.UsageType = "metered" },
			[]string{"price price_WY", "recurring.usage_type"}},
		{"duplicate price lookup key, one archived", func(f *fakeStripe, _ seededCatalog) {
			f.seedPrice("price_SM_old", LookupKeySupersonicMonthly, PlanSupersonic, "prod_S", 899, "month").Active = false
		}, []string{"price_SM", "price_SM_old", "lookup_key supersonic_monthly"}},
		{"coupon percent_off", func(_ *fakeStripe, s seededCatalog) { s.coupon.PercentOff = 40 },
			[]string{"coupon founders-50", "percent_off"}},
		{"coupon duration", func(_ *fakeStripe, s seededCatalog) { s.coupon.Duration = "once" },
			[]string{"coupon founders-50", "duration"}},
		{"coupon restricted to products", func(_ *fakeStripe, s seededCatalog) {
			s.coupon.AppliesTo = &fakeCouponAppliesTo{Products: []string{"prod_S"}}
		}, []string{"coupon founders-50", "applies_to"}},
		{"coupon redeem_by", func(_ *fakeStripe, s seededCatalog) { s.coupon.RedeemBy = 1893456000 },
			[]string{"coupon founders-50", "redeem_by"}},
		{"coupon max_redemptions", func(_ *fakeStripe, s seededCatalog) { s.coupon.MaxRedemptions = 100 },
			[]string{"coupon founders-50", "max_redemptions"}},
		{"coupon no longer valid", func(_ *fakeStripe, s seededCatalog) { s.coupon.Valid = false },
			[]string{"coupon founders-50", "valid"}},
		{"duplicate product identity", func(f *fakeStripe, _ seededCatalog) { f.seedProduct("prod_S2", PlanSupersonic) },
			[]string{"prod_S", "prod_S2", "concord_product=supersonic"}},
		{"duplicate portal identity", func(f *fakeStripe, _ seededCatalog) {
			f.seedPortal("bpc_copy", map[string]string{PortalMetadataKey: PortalMetadataVersion})
		}, []string{"bpc_concord", "bpc_copy", "concord_portal=v1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, dryRun := range []bool{false, true} {
				f := newFakeStripe(t)
				tc.mutate(f, f.seedCatalog())

				_, err := Bootstrap(t.Context(), f.client(), BootstrapOptions{DryRun: dryRun})

				require.ErrorIs(t, err, ErrCatalogConflict, "dry run %v", dryRun)
				for _, part := range tc.wantParts {
					assert.ErrorContains(t, err, part, "dry run %v", dryRun)
				}
				assert.Empty(t, f.writes(), "a conflict must stop the run before its first write (dry run %v)", dryRun)
			}
		})
	}
}

// wantPriceListForm is one price list request: the four lookup keys plus the
// active filter. Stripe lists active prices only unless active is set, so the
// bootstrap must ask for both states to see an archived price.
func wantPriceListForm(active string) url.Values {
	return url.Values{
		"active":         {active},
		"lookup_keys[0]": {LookupKeySupersonicMonthly},
		"lookup_keys[1]": {LookupKeySupersonicYearly},
		"lookup_keys[2]": {LookupKeyWingMonthly},
		"lookup_keys[3]": {LookupKeyWingYearly},
	}
}

func TestBootstrap_ListsActiveAndArchivedPrices(t *testing.T) {
	t.Parallel()
	f := newFakeStripe(t)
	f.seedCatalog()

	_, err := Bootstrap(t.Context(), f.client(), BootstrapOptions{DryRun: true})
	require.NoError(t, err)

	lists := f.requestsTo(http.MethodGet, "/v1/prices")
	require.Len(t, lists, 2, "one list per active state")
	assert.Equal(t, wantPriceListForm("true"), lists[0].Form)
	assert.Equal(t, wantPriceListForm("false"), lists[1].Form)
}

// TestBootstrap_UnrestrictedCouponAppliesToIsNotAConflict is the positive
// control for the applies_to rule: an empty product list restricts nothing.
func TestBootstrap_UnrestrictedCouponAppliesToIsNotAConflict(t *testing.T) {
	t.Parallel()
	f := newFakeStripe(t)
	s := f.seedCatalog()
	s.coupon.AppliesTo = &fakeCouponAppliesTo{Products: []string{}}

	report, err := Bootstrap(t.Context(), f.client(), BootstrapOptions{DryRun: true})

	require.NoError(t, err)
	assert.Equal(t, 7, report.Count(ActionUnchanged))
}

// TestBootstrap_RetrievesCouponWithAppliesToExpanded pins the request: Stripe
// leaves applies_to out of a plain coupon retrieve, so without the expand the
// restriction rule has nothing to read.
func TestBootstrap_RetrievesCouponWithAppliesToExpanded(t *testing.T) {
	t.Parallel()
	f := newFakeStripe(t)
	f.seedCatalog()

	_, err := Bootstrap(t.Context(), f.client(), BootstrapOptions{DryRun: true})
	require.NoError(t, err)

	gets := f.requestsTo(http.MethodGet, "/v1/coupons/"+CouponFoundersID)
	require.Len(t, gets, 1)
	assert.Equal(t, url.Values{"expand[0]": {"applies_to"}}, gets[0].Form)
}

// TestFakeStripe_CouponAppliesToNeedsExpand pins the stand-in's own fidelity:
// the coupon conflict case for applies_to is only meaningful while a retrieve
// without the expand hides the restriction, as Stripe does.
func TestFakeStripe_CouponAppliesToNeedsExpand(t *testing.T) {
	t.Parallel()
	f := newFakeStripe(t)
	f.seedCatalog().coupon.AppliesTo = &fakeCouponAppliesTo{Products: []string{"prod_S"}}
	c := f.client()

	plain, err := c.V1Coupons.Retrieve(t.Context(), CouponFoundersID, nil)
	require.NoError(t, err)
	assert.Nil(t, plain.AppliesTo, "applies_to is not included by default")

	params := &stripe.CouponRetrieveParams{}
	params.AddExpand("applies_to")
	expanded, err := c.V1Coupons.Retrieve(t.Context(), CouponFoundersID, params)
	require.NoError(t, err)
	require.NotNil(t, expanded.AppliesTo)
	assert.Equal(t, []string{"prod_S"}, expanded.AppliesTo.Products)
}

// TestFakeStripe_CreatePriceRefusesHeldLookupKey pins the stand-in's own
// fidelity: a lookup_key held by an archived price blocks a create unless the
// request transfers it. The archived-price conflict case relies on this, so a
// stand-in that quietly allowed the create would hide a bootstrap that failed
// to see the archived price.
func TestFakeStripe_CreatePriceRefusesHeldLookupKey(t *testing.T) {
	t.Parallel()
	f := newFakeStripe(t)
	f.seedProduct("prod_S", PlanSupersonic)
	archived := f.seedPrice("price_old", LookupKeySupersonicMonthly, PlanSupersonic, "prod_S", 899, "month")
	archived.Active = false
	c := f.client()

	_, err := c.V1Prices.Create(t.Context(), newPriceParams(catalogPrices[0], "prod_S"))
	var se *stripe.Error
	require.True(t, errors.As(err, &se), "a held lookup_key must be refused with a Stripe error, got %v", err)
	assert.Equal(t, http.StatusBadRequest, se.HTTPStatusCode)

	transfer := newPriceParams(catalogPrices[0], "prod_S")
	transfer.TransferLookupKey = stripe.Bool(true)
	created, err := c.V1Prices.Create(t.Context(), transfer)
	require.NoError(t, err)
	assert.Equal(t, LookupKeySupersonicMonthly, created.LookupKey)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Empty(t, archived.LookupKey, "a transfer moves the key off its previous holder")
}

func TestBootstrap_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	t.Run("fresh account", func(t *testing.T) {
		t.Parallel()
		f := newFakeStripe(t)

		report, err := Bootstrap(t.Context(), f.client(), BootstrapOptions{DryRun: true})
		require.NoError(t, err)

		assert.True(t, report.DryRun)
		assert.Equal(t, []BootstrapStep{
			{Kind: KindProduct, Key: PlanSupersonic, Action: ActionCreate},
			{Kind: KindProduct, Key: PlanWing, Action: ActionCreate},
			{Kind: KindPrice, Key: LookupKeySupersonicMonthly, Action: ActionCreate},
			{Kind: KindPrice, Key: LookupKeySupersonicYearly, Action: ActionCreate},
			{Kind: KindPrice, Key: LookupKeyWingMonthly, Action: ActionCreate},
			{Kind: KindPrice, Key: LookupKeyWingYearly, Action: ActionCreate},
			{Kind: KindCoupon, Key: CouponFoundersID, Action: ActionCreate},
			{Kind: KindPortalConfiguration, Key: PortalMetadataVersion, Action: ActionCreate},
		}, report.Steps)
		assert.Empty(t, f.writes())
	})
	t.Run("provisioned account", func(t *testing.T) {
		t.Parallel()
		f := newFakeStripe(t)
		f.seedCatalog()

		report, err := Bootstrap(t.Context(), f.client(), BootstrapOptions{DryRun: true})
		require.NoError(t, err)

		assert.Equal(t, 7, report.Count(ActionUnchanged))
		assert.Equal(t, []BootstrapStep{
			{Kind: KindPortalConfiguration, Key: PortalMetadataVersion, ID: "bpc_concord", Action: ActionUpdate},
		}, report.Steps[7:])
		assert.Empty(t, f.writes())
	})
}

func TestBootstrap_WrapsStripeErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		seeded     bool
		method     string
		path       string
		status     int
		body       string
		wantPrefix string
	}{
		{"list products", false, http.MethodGet, "/v1/products", http.StatusInternalServerError, serverErrorBody,
			"billing: bootstrap: list products: "},
		{"list prices", false, http.MethodGet, "/v1/prices", http.StatusInternalServerError, serverErrorBody,
			"billing: bootstrap: list prices: "},
		{"retrieve coupon outage is not absence", false, http.MethodGet, "/v1/coupons/founders-50", http.StatusInternalServerError, serverErrorBody,
			"billing: bootstrap: retrieve coupon founders-50: "},
		{"list portal configurations", false, http.MethodGet, "/v1/billing_portal/configurations", http.StatusInternalServerError, serverErrorBody,
			"billing: bootstrap: list portal configurations: "},
		{"create product without permission", false, http.MethodPost, "/v1/products", http.StatusForbidden, permissionErrorBody,
			"billing: bootstrap: create product supersonic: "},
		{"confirm created product outage is not a replay", false, http.MethodGet, "/v1/products/prod_1", http.StatusInternalServerError, serverErrorBody,
			"billing: bootstrap: create product supersonic: confirm create: "},
		{"create price", false, http.MethodPost, "/v1/prices", http.StatusInternalServerError, serverErrorBody,
			"billing: bootstrap: create price supersonic_monthly: "},
		{"create coupon without permission", false, http.MethodPost, "/v1/coupons", http.StatusForbidden, permissionErrorBody,
			"billing: bootstrap: create coupon founders-50: "},
		{"create portal configuration", false, http.MethodPost, "/v1/billing_portal/configurations", http.StatusInternalServerError, serverErrorBody,
			"billing: bootstrap: create portal configuration: "},
		{"update portal configuration without permission", true, http.MethodPost, "/v1/billing_portal/configurations/bpc_concord", http.StatusForbidden, permissionErrorBody,
			"billing: bootstrap: update portal configuration bpc_concord: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeStripe(t)
			if tc.seeded {
				f.seedCatalog()
			}
			f.failOn(tc.method, tc.path, tc.status, tc.body)

			_, err := Bootstrap(t.Context(), f.client(), BootstrapOptions{})

			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), tc.wantPrefix), "error %q lacks prefix %q", err.Error(), tc.wantPrefix)
			var se *stripe.Error
			require.True(t, errors.As(err, &se), "the *stripe.Error must stay reachable through the wrap")
			assert.Equal(t, tc.status, se.HTTPStatusCode)
			assert.NotErrorIs(t, err, ErrCatalogConflict)
			assert.NotErrorIs(t, err, ErrReplayedCreate, "only resource_missing on the confirm means a replay")
			if tc.method == http.MethodGet && !strings.Contains(tc.wantPrefix, "confirm create") {
				assert.Empty(t, f.writes(), "a failed read must stop the run before any write")
			}
		})
	}
}

// TestBootstrap_ReplayedCreateOfDeletedObjectFails covers an operator who
// deleted a bootstrap object and re-ran inside Stripe's 24-hour idempotency
// window: Stripe answers the create with the cached "created" response while
// no object exists. The run must fail instead of reporting success.
func TestBootstrap_ReplayedCreateOfDeletedObjectFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		path       string
		seed       func(*fakeStripe)
		wantPrefix string
		wantWrites int
	}{
		{"product", "/v1/products", func(*fakeStripe) {},
			"billing: bootstrap: create product supersonic: ", 1},
		{"coupon", "/v1/coupons", func(f *fakeStripe) {
			s := f.seedCatalog()
			delete(f.coupons, s.coupon.ID)
		}, "billing: bootstrap: create coupon founders-50: ", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeStripe(t)
			tc.seed(f)
			f.replayDeletedOn(tc.path)

			_, err := Bootstrap(t.Context(), f.client(), BootstrapOptions{})

			require.ErrorIs(t, err, ErrReplayedCreate)
			assert.True(t, strings.HasPrefix(err.Error(), tc.wantPrefix), "error %q lacks prefix %q", err.Error(), tc.wantPrefix)
			assert.Len(t, f.writes(), tc.wantWrites, "the run must stop at the replayed create")
		})
	}
}
