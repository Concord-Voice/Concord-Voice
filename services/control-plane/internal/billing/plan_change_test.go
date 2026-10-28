package billing

import (
	"database/sql"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"
)

func TestChangeTiming(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, oldPlan, oldInterval, newPlan, newInterval, want string
		wantErr                                                bool
	}{
		{"upgrade monthly", PlanSupersonic, "month", PlanWing, "month", "immediate", false},
		{"upgrade yearly", PlanSupersonic, "year", PlanWing, "year", "immediate", false},
		{"month to year", PlanSupersonic, "month", PlanSupersonic, "year", "immediate", false},
		{"upgrade and month to year", PlanSupersonic, "month", PlanWing, "year", "immediate", false},
		{"downgrade monthly", PlanWing, "month", PlanSupersonic, "month", "scheduled", false},
		{"year to month", PlanWing, "year", PlanWing, "month", "scheduled", false},
		{"upgrade and year to month", PlanSupersonic, "year", PlanWing, "month", "immediate_then_scheduled", false},
		{"downgrade and month to year", PlanWing, "month", PlanSupersonic, "year", "scheduled", false},
		{"same price", PlanWing, "year", PlanWing, "year", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := changeTiming(tt.oldPlan, tt.oldInterval, tt.newPlan, tt.newInterval)
			if tt.wantErr {
				require.ErrorIs(t, err, errUnchangeable)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestClassifySubscriberAmount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		amount   int64
		coupon   string
		currency stripe.Currency
		want     sql.NullInt64
	}{
		{"regular", 899, "", stripe.CurrencyUSD, sql.NullInt64{Int64: 899, Valid: true}},
		{"founders rounds down", 899, CouponFoundersID, stripe.CurrencyUSD, sql.NullInt64{Int64: 449, Valid: true}},
		{"other currency omitted", 899, "", stripe.CurrencyEUR, sql.NullInt64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sub := validClassifySub()
			sub.Items.Data[0].Price.UnitAmount = tc.amount
			sub.Items.Data[0].Price.Currency = tc.currency
			if tc.coupon != "" {
				sub.Discounts = []*stripe.Discount{{Source: &stripe.DiscountSource{Coupon: &stripe.Coupon{ID: tc.coupon}}}}
			}
			got, err := classify(sub)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.UnitAmount)
		})
	}
}

func TestParsePendingAt(t *testing.T) {
	t.Parallel()
	got, valid := parsePendingAt("1900000000")
	require.True(t, valid)
	assert.Equal(t, time.Unix(1900000000, 0).UTC(), got)
	for _, bad := range []string{"", "0", "bad", "1900000000 trailing"} {
		_, valid := parsePendingAt(bad)
		require.False(t, valid)
	}
}

func TestClassifyPendingChange(t *testing.T) {
	fake := newFakeStripe(t)
	fake.schedules["sub_sched_test"] = &fakeSchedule{
		ID: "sub_sched_test", Object: "subscription_schedule", Status: "active",
		Metadata: map[string]string{planChangeMarker: "v1", planChangePlan: PlanWing,
			planChangeInterval: "month", planChangeAt: "1900000000"},
	}
	fake.schedules["sub_sched_test"].CurrentPhase.EndDate = 1900000000
	svc := NewService(Deps{Stripe: fake.client()})
	svc.now = func() time.Time { return time.Unix(1800000000, 0) }
	fields := rowFields{ScheduleID: sql.NullString{String: "sub_sched_test", Valid: true}}
	require.NoError(t, svc.classifyPendingChange(t.Context(), &fields))
	assert.Equal(t, PlanWing, fields.PendingPlan.String)
	assert.Equal(t, "month", fields.PendingInterval.String)
	assert.Equal(t, time.Unix(1900000000, 0).UTC(), fields.PendingChangeAt.Time)

	for _, tc := range []struct {
		name   string
		change func()
	}{
		{"foreign schedule", func() { fake.schedules["sub_sched_test"].Metadata[planChangeMarker] = "" }},
		{"past date", func() { svc.now = func() time.Time { return time.Unix(2000000000, 0) } }},
		{"invalid target", func() { fake.schedules["sub_sched_test"].Metadata[planChangePlan] = "unknown" }},
		{"inactive schedule", func() { fake.schedules["sub_sched_test"].Status = "released" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake.schedules["sub_sched_test"].Status = "active"
			fake.schedules["sub_sched_test"].Metadata[planChangeMarker] = "v1"
			fake.schedules["sub_sched_test"].Metadata[planChangePlan] = PlanWing
			svc.now = func() time.Time { return time.Unix(1800000000, 0) }
			tc.change()
			f := rowFields{ScheduleID: fields.ScheduleID}
			require.NoError(t, svc.classifyPendingChange(t.Context(), &f))
			assert.False(t, f.PendingPlan.Valid)
		})
	}
	f := rowFields{}
	require.NoError(t, svc.classifyPendingChange(t.Context(), &f))
	assert.False(t, f.PendingPlan.Valid)
	fake.failOn("GET", "/v1/subscription_schedules/sub_sched_test", 500, serverErrorBody)
	f.ScheduleID = fields.ScheduleID
	require.Error(t, svc.classifyPendingChange(t.Context(), &f))
}

func TestFetchSubscription_MixedRecoveryRequiresEnvironmentOwnership(t *testing.T) {
	for _, tc := range []struct {
		name        string
		environment string
		owned       bool
	}{
		{"foreign environment makes no writes", "other-dev-box", false},
		{"missing environment makes no writes", "", false},
		{"local environment books schedule", "this-dev-box", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeStripe(t)
			fake.seedCatalog()
			sub := fake.seedSubscription("sub_mixed_environment", "cus_mixed_environment", PlanWing, "year", 1900000000)
			sub.Metadata = map[string]string{
				metadataKeyUserID: "11111111-1111-4111-8111-111111111111",
				planChangeMixedAt: time.Unix(1900000000, 0).UTC().Format(time.RFC3339),
			}
			if tc.environment != "" {
				sub.Metadata[metadataKeyEnv] = tc.environment
			}
			svc := NewService(Deps{Stripe: fake.client(), Environment: "this-dev-box", Log: logger.NewWithWriter(&syncBuffer{})})
			svc.now = func() time.Time { return time.Unix(1800000000, 0) }

			fields, metadata, err := svc.fetchSubscription(t.Context(), sub.ID)
			require.NoError(t, err)
			_, payerErr := svc.payerFromMetadata(reconcileInput{subID: sub.ID, fields: fields, metadata: metadata})
			writes := fake.requestsTo("POST", "/v1/subscriptions/"+sub.ID)
			creates := fake.requestsTo("POST", "/v1/subscription_schedules")
			if tc.owned {
				require.NoError(t, payerErr)
				assert.Len(t, creates, 1, "owned mixed recovery must create its schedule")
				assert.NotEmpty(t, sub.Schedule, "created schedule must attach to the subscription")
			} else {
				require.ErrorIs(t, payerErr, ErrUnactionable)
				assert.Empty(t, writes, "environment refusal must precede subscription metadata mutation")
				assert.Empty(t, creates, "environment refusal must precede schedule creation")
			}
		})
	}
}
