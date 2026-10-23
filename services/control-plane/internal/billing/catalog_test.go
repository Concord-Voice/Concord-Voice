package billing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Plans endpoint and purchase gate tests (#2033 PR-3, plan Tasks 2 and 3).
// They need no database: GET /billing/plans reads only Stripe and config.

const pricesPath = "/v1/prices"

// testCutoff is a fixed founders cutoff; tests move the clock around it.
var testCutoff = time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)

// planEnv is a Service over the fake with a seeded catalog, a clock the test
// sets, and a captured log.
type planEnv struct {
	Svc  *Service
	Fake *fakeStripe
	Now  *time.Time
	Logs *syncBuffer
}

func newPlanEnv(t *testing.T, billing config.BillingConfig) *planEnv {
	t.Helper()
	fake := newFakeStripe(t)
	fake.seedCatalog()
	logs := &syncBuffer{}
	billing.KeyMode = config.StripeKeyModeTest
	svc := NewService(Deps{Stripe: fake.client(), Billing: billing, Environment: testEnvironment, Log: logger.NewWithWriter(logs)})
	now := testCutoff.Add(-24 * time.Hour)
	svc.now = func() time.Time { return now }
	return &planEnv{Svc: svc, Fake: fake, Now: &now, Logs: logs}
}

func foundersConfig(mode config.BillingPurchaseMode, allowlist ...string) config.BillingConfig {
	return config.BillingConfig{PurchaseMode: mode, PurchaseAllowlist: allowlist, FoundersPricingEndsAt: testCutoff}
}

func getPlans(svc *Service, userID uuid.UUID) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/billing/plans", nil)
	c.Set("user_id", userID.String())
	svc.Plans(c)
	return w
}

type plansBody struct {
	PurchaseAvailable bool `json:"purchase_available"`
	Founders          struct {
		Active bool    `json:"active"`
		EndsAt *string `json:"ends_at"`
	} `json:"founders"`
	Plans []struct {
		Plan               string `json:"plan"`
		Interval           string `json:"interval"`
		Currency           string `json:"currency"`
		UnitAmount         int64  `json:"unit_amount"`
		FoundersUnitAmount *int64 `json:"founders_unit_amount"`
	} `json:"plans"`
}

func decodePlans(t *testing.T, w *httptest.ResponseRecorder) plansBody {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var b plansBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	return b
}

func TestFoundersUnitAmount_RoundsDown(t *testing.T) {
	for unit, want := range map[int64]int64{899: 449, 9889: 4944, 1499: 749, 16489: 8244, 1000: 500, 1: 0} {
		assert.Equal(t, want, foundersUnitAmount(unit), "unit %d", unit)
	}
}

func TestPurchaseAllowed(t *testing.T) {
	user := uuid.New()
	other := uuid.New()
	tests := []struct {
		name string
		cfg  config.BillingConfig
		want bool
	}{
		{"off", config.BillingConfig{PurchaseMode: config.BillingPurchaseModeOff, PurchaseAllowlist: []string{user.String()}}, false},
		{"empty mode is off", config.BillingConfig{}, false},
		{"on", config.BillingConfig{PurchaseMode: config.BillingPurchaseModeOn}, true},
		{"allowlisted", config.BillingConfig{PurchaseMode: config.BillingPurchaseModeAllowlist, PurchaseAllowlist: []string{other.String(), user.String()}}, true},
		{"allowlisted, upper case entry", config.BillingConfig{PurchaseMode: config.BillingPurchaseModeAllowlist, PurchaseAllowlist: []string{strings.ToUpper(user.String())}}, true},
		{"not allowlisted", config.BillingConfig{PurchaseMode: config.BillingPurchaseModeAllowlist, PurchaseAllowlist: []string{other.String()}}, false},
		{"unknown mode", config.BillingConfig{PurchaseMode: "maybe"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewService(Deps{Billing: tt.cfg})
			assert.Equal(t, tt.want, s.purchaseAllowed(user))
		})
	}
}

func TestFoundersActive_Boundary(t *testing.T) {
	s := NewService(Deps{Billing: config.BillingConfig{FoundersPricingEndsAt: testCutoff}})
	for offset, want := range map[time.Duration]bool{-time.Second: true, 0: false, time.Second: false} {
		at := testCutoff.Add(offset)
		s.now = func() time.Time { return at }
		assert.Equal(t, want, s.foundersActive(), "offset %s", offset)
	}
	unset := NewService(Deps{})
	assert.False(t, unset.foundersActive(), "no cutoff configured means no founders pricing")
}

func TestPlans_FullCatalogInFixedOrder(t *testing.T) {
	env := newPlanEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	b := decodePlans(t, getPlans(env.Svc, uuid.New()))

	assert.True(t, b.PurchaseAvailable)
	assert.True(t, b.Founders.Active)
	require.NotNil(t, b.Founders.EndsAt)
	assert.Equal(t, "2027-05-01T00:00:00Z", *b.Founders.EndsAt)
	require.Len(t, b.Plans, 4)
	want := []struct {
		plan, interval string
		unit, founders int64
	}{
		{PlanSupersonic, "month", 899, 449},
		{PlanSupersonic, "year", 9889, 4944},
		{PlanWing, "month", 1499, 749},
		{PlanWing, "year", 16489, 8244},
	}
	for i, w := range want {
		p := b.Plans[i]
		assert.Equal(t, w.plan, p.Plan)
		assert.Equal(t, w.interval, p.Interval)
		assert.Equal(t, "usd", p.Currency)
		assert.Equal(t, w.unit, p.UnitAmount)
		require.NotNil(t, p.FoundersUnitAmount)
		assert.Equal(t, w.founders, *p.FoundersUnitAmount)
	}
}

func TestPlans_FoundersBoundary(t *testing.T) {
	for offset, want := range map[time.Duration]bool{-time.Second: true, 0: false, time.Second: false} {
		env := newPlanEnv(t, foundersConfig(config.BillingPurchaseModeOn))
		*env.Now = testCutoff.Add(offset)
		b := decodePlans(t, getPlans(env.Svc, uuid.New()))
		assert.Equal(t, want, b.Founders.Active, "offset %s", offset)
		assert.Equal(t, want, b.Founders.EndsAt != nil, "ends_at only while active, offset %s", offset)
		for _, p := range b.Plans {
			assert.Equal(t, want, p.FoundersUnitAmount != nil, "founders amount only while active, offset %s", offset)
		}
	}
}

func TestPlans_ShapeWhenFoundersInactive(t *testing.T) {
	env := newPlanEnv(t, config.BillingConfig{PurchaseMode: config.BillingPurchaseModeOn})
	w := getPlans(env.Svc, uuid.New())
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "founders_unit_amount")
	assert.NotContains(t, w.Body.String(), "ends_at")
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
}

func TestPlans_PurchaseAvailableFollowsGate(t *testing.T) {
	user := uuid.New()
	tests := map[string]struct {
		cfg  config.BillingConfig
		want bool
	}{
		"off":             {foundersConfig(config.BillingPurchaseModeOff), false},
		"allowlisted":     {foundersConfig(config.BillingPurchaseModeAllowlist, user.String()), true},
		"not allowlisted": {foundersConfig(config.BillingPurchaseModeAllowlist, uuid.NewString()), false},
		"on":              {foundersConfig(config.BillingPurchaseModeOn), true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := newPlanEnv(t, tt.cfg)
			b := decodePlans(t, getPlans(env.Svc, user))
			assert.Equal(t, tt.want, b.PurchaseAvailable)
			assert.Len(t, b.Plans, 4, "the catalog is shown whatever the mode")
		})
	}
}

func TestPlans_PartialCatalogIs503(t *testing.T) {
	env := newPlanEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	env.Fake.archivePrice(LookupKeyWingYearly)
	w := getPlans(env.Svc, uuid.New())
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error":"catalog_unavailable"}`, w.Body.String())
	assert.Contains(t, env.Logs.String(), LookupKeyWingYearly, "the log names the missing lookup key")
}

func TestPlans_MismatchedPriceIs503(t *testing.T) {
	for name, edit := range map[string]func(*fakePrice){
		"plan metadata": func(p *fakePrice) { p.Metadata[PlanMetadataKey] = PlanWing },
		"interval":      func(p *fakePrice) { p.Recurring.Interval = "year" },
		"currency":      func(p *fakePrice) { p.Currency = "eur" },
		"amount":        func(p *fakePrice) { p.UnitAmount = 0 },
		// A positive amount that is not the catalog's is refused too, so a
		// Dashboard edit cannot change what Checkout charges.
		"other amount": func(p *fakePrice) { p.UnitAmount = 900 },
		// A metered price bills usage, not the advertised fixed amount.
		"usage type": func(p *fakePrice) { p.Recurring.UsageType = "metered" },
	} {
		t.Run(name, func(t *testing.T) {
			env := newPlanEnv(t, foundersConfig(config.BillingPurchaseModeOn))
			for _, p := range env.Fake.prices {
				if p.LookupKey == LookupKeySupersonicMonthly {
					edit(p)
				}
			}
			assert.Equal(t, http.StatusServiceUnavailable, getPlans(env.Svc, uuid.New()).Code)
		})
	}
}

func TestPlans_StripeErrorIs503(t *testing.T) {
	env := newPlanEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	env.Fake.failOn(http.MethodGet, pricesPath, http.StatusInternalServerError, serverErrorBody)
	w := getPlans(env.Svc, uuid.New())
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error":"catalog_unavailable"}`, w.Body.String())
}

func TestPlans_CacheServesWithinTTLAndNeverCachesFailure(t *testing.T) {
	env := newPlanEnv(t, foundersConfig(config.BillingPurchaseModeOn))

	// A failure is not cached: the next request lists again and succeeds.
	env.Fake.failOn(http.MethodGet, pricesPath, http.StatusInternalServerError, serverErrorBody)
	require.Equal(t, http.StatusServiceUnavailable, getPlans(env.Svc, uuid.New()).Code)
	env.Fake.mu.Lock()
	delete(env.Fake.failures, http.MethodGet+" "+pricesPath)
	env.Fake.mu.Unlock()
	require.Equal(t, http.StatusOK, getPlans(env.Svc, uuid.New()).Code)
	listed := len(env.Fake.requestsTo(http.MethodGet, pricesPath))

	// Within the TTL a second visit is served from the cache, even after the
	// catalog changed in Stripe.
	env.Fake.archivePrice(LookupKeyWingYearly)
	*env.Now = env.Now.Add(catalogCacheTTL - time.Second)
	require.Equal(t, http.StatusOK, getPlans(env.Svc, uuid.New()).Code)
	assert.Len(t, env.Fake.requestsTo(http.MethodGet, pricesPath), listed, "served from the cache")

	// At the TTL it lists again and sees the archived price.
	*env.Now = env.Now.Add(time.Second)
	assert.Equal(t, http.StatusServiceUnavailable, getPlans(env.Svc, uuid.New()).Code)
}

func TestPlans_NoUserIs401(t *testing.T) {
	env := newPlanEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/billing/plans", nil)
	env.Svc.Plans(c)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, env.Fake.requestsTo(http.MethodGet, pricesPath), "no Stripe call without a user")
}

func TestResolvePrice_UncachedAndActiveOnly(t *testing.T) {
	env := newPlanEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	p, err := env.Svc.resolvePrice(t.Context(), PlanWing, "month")
	require.NoError(t, err)
	assert.Equal(t, "price_WM", p.ID)

	env.Fake.archivePrice(LookupKeyWingMonthly)
	_, err = env.Svc.resolvePrice(t.Context(), PlanWing, "month")
	assert.ErrorIs(t, err, errPriceUnavailable, "an archived price is never sold, with no cache in the way")

	reqs := env.Fake.requestsTo(http.MethodGet, pricesPath)
	require.Len(t, reqs, 2)
	assert.Equal(t, "true", reqs[1].Form.Get("active"))
	assert.Equal(t, LookupKeyWingMonthly, reqs[1].Form.Get("lookup_keys[0]"))

	_, err = env.Svc.resolvePrice(t.Context(), "platinum", "month")
	assert.ErrorIs(t, err, errUnknownPlan)
}
