package billing

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
)

// classifyPeriodEnd is the unix second every valid fixture subscription ends on.
const classifyPeriodEnd int64 = 1_900_000_000

// classifyItem builds one subscription item whose price carries concord_plan
// metadata and a recurring interval. lookupKey is deliberately free: the plan
// comes from price metadata, never from the lookup key (spec F11).
func classifyItem(plan string, interval stripe.PriceRecurringInterval, lookupKey string) *stripe.SubscriptionItem {
	return &stripe.SubscriptionItem{
		CurrentPeriodEnd: classifyPeriodEnd,
		Quantity:         1,
		Price: &stripe.Price{
			LookupKey: lookupKey,
			Metadata:  map[string]string{PlanMetadataKey: plan},
			Recurring: &stripe.PriceRecurring{Interval: interval, IntervalCount: 1},
		},
	}
}

// classifySub builds a subscription around items with sensible defaults.
func classifySub(items ...*stripe.SubscriptionItem) *stripe.Subscription {
	return &stripe.Subscription{
		ID:       "sub_classify",
		Status:   stripe.SubscriptionStatusActive,
		Customer: &stripe.Customer{ID: "cus_classify"},
		Items:    &stripe.SubscriptionItemList{Data: items},
	}
}

func validClassifySub() *stripe.Subscription {
	return classifySub(classifyItem(PlanWing, stripe.PriceRecurringIntervalMonth, LookupKeyWingMonthly))
}

func TestClassify_StatusPassThrough(t *testing.T) {
	t.Parallel()
	statuses := []stripe.SubscriptionStatus{
		stripe.SubscriptionStatusActive,
		stripe.SubscriptionStatusCanceled,
		stripe.SubscriptionStatusIncomplete,
		stripe.SubscriptionStatusIncompleteExpired,
		stripe.SubscriptionStatusPastDue,
		stripe.SubscriptionStatusPaused,
		stripe.SubscriptionStatusTrialing,
		stripe.SubscriptionStatusUnpaid,
	}
	require.Len(t, statuses, 8, "Stripe has eight subscription statuses")
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			sub := validClassifySub()
			sub.Status = status
			got, err := classify(sub)
			require.NoError(t, err)
			assert.Equal(t, string(status), got.Status, "the status is stored as Stripe sent it; the live predicate decides what entitles")
			assert.Empty(t, got.Malformed)
			assert.Equal(t, "cus_classify", got.CustomerID)
			assert.True(t, got.PeriodEnd.Equal(time.Unix(classifyPeriodEnd, 0)), "period end comes from items.data[0].current_period_end")
		})
	}
}

func TestClassify_PlanFromPriceMetadata(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		plan         string
		interval     stripe.PriceRecurringInterval
		lookupKey    string
		wantInterval string
	}{
		{"wing monthly", PlanWing, stripe.PriceRecurringIntervalMonth, LookupKeyWingMonthly, "month"},
		{"wing yearly", PlanWing, stripe.PriceRecurringIntervalYear, LookupKeyWingYearly, "year"},
		{"supersonic monthly", PlanSupersonic, stripe.PriceRecurringIntervalMonth, LookupKeySupersonicMonthly, "month"},
		{"supersonic yearly", PlanSupersonic, stripe.PriceRecurringIntervalYear, LookupKeySupersonicYearly, "year"},
		{"lookup key transferred away", PlanWing, stripe.PriceRecurringIntervalYear, "", "year"},
		{"lookup key from another price", PlanSupersonic, stripe.PriceRecurringIntervalMonth, LookupKeyWingYearly, "month"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := classify(classifySub(classifyItem(tt.plan, tt.interval, tt.lookupKey)))
			require.NoError(t, err)
			assert.Equal(t, sql.NullString{String: tt.plan, Valid: true}, got.Plan)
			assert.Equal(t, sql.NullString{String: tt.wantInterval, Valid: true}, got.Interval)
			assert.Equal(t, entitlements.TierPremium, got.Tier, "a price whose lookup_key was moved still entitles (F11)")
			assert.Empty(t, got.Malformed)
		})
	}
}

func TestClassify_FailClosed(t *testing.T) {
	t.Parallel()
	validItem := func() *stripe.SubscriptionItem {
		return classifyItem(PlanWing, stripe.PriceRecurringIntervalMonth, LookupKeyWingMonthly)
	}
	withQuantity := func(q int64) *stripe.SubscriptionItem {
		it := validItem()
		it.Quantity = q
		return it
	}
	tests := []struct {
		name         string
		sub          *stripe.Subscription
		wantPlan     sql.NullString
		wantInterval sql.NullString
		checkColumns bool
	}{
		{
			name:         "unknown concord_plan",
			sub:          classifySub(classifyItem("gold", stripe.PriceRecurringIntervalMonth, "")),
			wantPlan:     sql.NullString{},
			wantInterval: sql.NullString{String: "month", Valid: true},
			checkColumns: true,
		},
		{
			name:         "missing concord_plan",
			sub:          classifySub(&stripe.SubscriptionItem{CurrentPeriodEnd: classifyPeriodEnd, Quantity: 1, Price: &stripe.Price{Recurring: &stripe.PriceRecurring{Interval: stripe.PriceRecurringIntervalYear}}}),
			wantPlan:     sql.NullString{},
			wantInterval: sql.NullString{String: "year", Valid: true},
			checkColumns: true,
		},
		{name: "two items", sub: classifySub(validItem(), validItem())},
		{
			name:         "quantity two",
			sub:          classifySub(withQuantity(2)),
			wantPlan:     sql.NullString{String: PlanWing, Valid: true},
			wantInterval: sql.NullString{String: "month", Valid: true},
			checkColumns: true,
		},
		{
			name:         "interval week",
			sub:          classifySub(classifyItem(PlanWing, stripe.PriceRecurringIntervalWeek, "")),
			wantPlan:     sql.NullString{String: PlanWing, Valid: true},
			wantInterval: sql.NullString{},
			checkColumns: true,
		},
		{
			name:         "interval day",
			sub:          classifySub(classifyItem(PlanSupersonic, stripe.PriceRecurringIntervalDay, "")),
			wantPlan:     sql.NullString{String: PlanSupersonic, Valid: true},
			wantInterval: sql.NullString{},
			checkColumns: true,
		},
		{
			name: "item without a price",
			sub:  classifySub(&stripe.SubscriptionItem{CurrentPeriodEnd: classifyPeriodEnd, Quantity: 1}),
		},
		{
			name: "price without recurring",
			sub: classifySub(&stripe.SubscriptionItem{
				CurrentPeriodEnd: classifyPeriodEnd, Quantity: 1,
				Price: &stripe.Price{Metadata: map[string]string{PlanMetadataKey: PlanWing}},
			}),
			wantPlan:     sql.NullString{String: PlanWing, Valid: true},
			wantInterval: sql.NullString{},
			checkColumns: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := classify(tt.sub)
			require.NoError(t, err, "a malformed shape fails closed to free; it is not unactionable")
			assert.Equal(t, entitlements.TierFree, got.Tier)
			assert.NotEmpty(t, got.Malformed, "the reason feeds the error log")
			if tt.checkColumns {
				assert.Equal(t, tt.wantPlan, got.Plan, "plan is NULL unless it is one of the CHECK values")
				assert.Equal(t, tt.wantInterval, got.Interval, "billing_interval is NULL unless it is month or year")
			}
		})
	}
}

// TestClassify_ZeroItems pins the one fail-closed case with no period to store.
// With no items there is no items.data[0].current_period_end, so there is
// nothing to write: the result is ErrUnactionable (F4), never a premium row or
// a panic.
func TestClassify_ZeroItems(t *testing.T) {
	t.Parallel()
	for name, sub := range map[string]*stripe.Subscription{
		"empty item list": classifySub(),
		"nil item list":   {ID: "sub_classify", Status: stripe.SubscriptionStatusActive, Customer: &stripe.Customer{ID: "cus_classify"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := classify(sub)
			require.ErrorIs(t, err, ErrUnactionable, "no item means no period end to store (F4)")
		})
	}
}

func TestClassify_AbsentPeriod(t *testing.T) {
	t.Parallel()
	sub := validClassifySub()
	sub.Items.Data[0].CurrentPeriodEnd = 0
	_, err := classify(sub)
	require.ErrorIs(t, err, ErrUnactionable, "no usable period end means no write (F4)")

	t.Run("a period on a later item does not stand in", func(t *testing.T) {
		t.Parallel()
		two := classifySub(validClassifySub().Items.Data[0], validClassifySub().Items.Data[0])
		two.Items.Data[0].CurrentPeriodEnd = 0
		_, err := classify(two)
		require.ErrorIs(t, err, ErrUnactionable, "the period is read from items.data[0] only")
	})
}

func TestClassify_FlexibleCancel(t *testing.T) {
	t.Parallel()
	cancelAt := classifyPeriodEnd + 86400

	t.Run("cancel_at without cancel_at_period_end", func(t *testing.T) {
		t.Parallel()
		sub := validClassifySub()
		sub.CancelAt = cancelAt
		sub.CancelAtPeriodEnd = false
		got, err := classify(sub)
		require.NoError(t, err)
		require.True(t, got.CancelAt.Valid, "a flexible-mode cancel sets cancel_at only (F9)")
		assert.True(t, got.CancelAt.Time.Equal(time.Unix(cancelAt, 0)))
		assert.False(t, got.CancelAtPeriodEnd)
	})

	t.Run("cancel_at_period_end", func(t *testing.T) {
		t.Parallel()
		sub := validClassifySub()
		sub.CancelAtPeriodEnd = true
		got, err := classify(sub)
		require.NoError(t, err)
		assert.True(t, got.CancelAtPeriodEnd)
		assert.False(t, got.CancelAt.Valid, "cancel_at is NULL when Stripe sent none")
	})

	t.Run("not canceling", func(t *testing.T) {
		t.Parallel()
		got, err := classify(validClassifySub())
		require.NoError(t, err)
		assert.False(t, got.CancelAtPeriodEnd)
		assert.Equal(t, sql.NullTime{}, got.CancelAt)
	})
}

func TestClassify_FoundersRate(t *testing.T) {
	t.Parallel()
	discount := func(couponID string) *stripe.Discount {
		return &stripe.Discount{Source: &stripe.DiscountSource{Type: stripe.DiscountSourceTypeCoupon, Coupon: &stripe.Coupon{ID: couponID}}}
	}
	tests := []struct {
		name      string
		discounts []*stripe.Discount
		want      bool
	}{
		{"founders coupon", []*stripe.Discount{discount(CouponFoundersID)}, true},
		{"founders among others", []*stripe.Discount{discount("other-10"), discount(CouponFoundersID)}, true},
		{"another coupon", []*stripe.Discount{discount("other-10")}, false},
		{"nil source", []*stripe.Discount{{}}, false},
		{"nil coupon", []*stripe.Discount{{Source: &stripe.DiscountSource{Type: stripe.DiscountSourceTypeCoupon}}}, false},
		{"nil discount", []*stripe.Discount{nil}, false},
		{"no discounts", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sub := validClassifySub()
			sub.Discounts = tt.discounts
			got, err := classify(sub)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.FoundersRate)
		})
	}
}
