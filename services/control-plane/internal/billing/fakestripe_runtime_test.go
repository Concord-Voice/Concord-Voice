package billing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"
)

// stripeAPIError unwraps err to the *stripe.Error the SDK returns.
func stripeAPIError(t *testing.T, err error) *stripe.Error {
	t.Helper()
	var se *stripe.Error
	require.ErrorAs(t, err, &se)
	return se
}

func TestFakeStripe_RuntimeRoutes(t *testing.T) {
	t.Parallel()
	fake := newFakeStripe(t)
	sc := fake.client()
	ctx := context.Background()
	const periodEnd int64 = 1_900_000_000

	t.Run("retrieve a seeded subscription", func(t *testing.T) {
		fake.seedSubscription("sub_r", "cus_r", PlanWing, "year", periodEnd, CouponFoundersID)

		params := &stripe.SubscriptionRetrieveParams{}
		params.AddExpand("items.data.price")
		params.AddExpand("discounts")
		sub, err := sc.V1Subscriptions.Retrieve(ctx, "sub_r", params)
		require.NoError(t, err)
		assert.Equal(t, "sub_r", sub.ID)
		assert.Equal(t, stripe.SubscriptionStatusActive, sub.Status)
		require.NotNil(t, sub.Customer)
		assert.Equal(t, "cus_r", sub.Customer.ID)
		require.NotNil(t, sub.Items)
		require.Len(t, sub.Items.Data, 1)
		item := sub.Items.Data[0]
		assert.Equal(t, periodEnd, item.CurrentPeriodEnd)
		assert.EqualValues(t, 1, item.Quantity)
		require.NotNil(t, item.Price)
		assert.Equal(t, PlanWing, item.Price.Metadata[PlanMetadataKey])
		require.NotNil(t, item.Price.Recurring)
		assert.Equal(t, stripe.PriceRecurringIntervalYear, item.Price.Recurring.Interval)
		require.Len(t, sub.Discounts, 1)
		require.NotNil(t, sub.Discounts[0].Source, "expand[]=discounts returns the discount object")
		require.NotNil(t, sub.Discounts[0].Source.Coupon)
		assert.Equal(t, CouponFoundersID, sub.Discounts[0].Source.Coupon.ID)

		bare, err := sc.V1Subscriptions.Retrieve(ctx, "sub_r", nil)
		require.NoError(t, err)
		require.Len(t, bare.Discounts, 1)
		assert.Nil(t, bare.Discounts[0].Source, "without the expand a discount is a bare id, as in Stripe")
	})

	t.Run("retrieve a missing subscription", func(t *testing.T) {
		_, err := sc.V1Subscriptions.Retrieve(ctx, "sub_nope", nil)
		require.Error(t, err)
		se := stripeAPIError(t, err)
		assert.Equal(t, stripe.ErrorCodeResourceMissing, se.Code)
		assert.Equal(t, 404, se.HTTPStatusCode)
	})

	t.Run("an injected failure surfaces as a Stripe error", func(t *testing.T) {
		fake.failOn("GET", "/v1/subscriptions/sub_r", 500, serverErrorBody)
		_, err := sc.V1Subscriptions.Retrieve(ctx, "sub_r", nil)
		require.Error(t, err)
		assert.Equal(t, stripe.ErrorTypeAPI, stripeAPIError(t, err).Type)
	})

	t.Run("list by customer", func(t *testing.T) {
		fake.seedSubscription("sub_l1", "cus_l", PlanWing, "month", periodEnd)
		ended := fake.seedSubscription("sub_l2", "cus_l", PlanSupersonic, "month", periodEnd)
		ended.Status = "canceled"
		fake.seedSubscription("sub_other", "cus_other", PlanWing, "month", periodEnd)

		ids := func(params *stripe.SubscriptionListParams) []string {
			var out []string
			for sub, err := range sc.V1Subscriptions.List(ctx, params).All(ctx) {
				require.NoError(t, err)
				out = append(out, sub.ID)
			}
			return out
		}
		assert.ElementsMatch(t, []string{"sub_l1", "sub_l2"}, ids(&stripe.SubscriptionListParams{
			Customer: stripe.String("cus_l"), Status: stripe.String("all"),
		}), "status=all includes canceled subscriptions of that customer only")
		assert.Equal(t, []string{"sub_l1"}, ids(&stripe.SubscriptionListParams{
			Customer: stripe.String("cus_l"),
		}), "the default omits canceled subscriptions, as Stripe does")
	})

	t.Run("delete a customer", func(t *testing.T) {
		fake.seedCustomer("cus_d")
		require.True(t, fake.customerExists("cus_d"))
		deleted, err := sc.V1Customers.Delete(ctx, "cus_d", nil)
		require.NoError(t, err)
		assert.True(t, deleted.Deleted)
		assert.False(t, fake.customerExists("cus_d"))

		_, err = sc.V1Customers.Delete(ctx, "cus_d", nil)
		require.Error(t, err)
		se := stripeAPIError(t, err)
		assert.Equal(t, stripe.ErrorCodeResourceMissing, se.Code, "a second delete is resource_missing")
		assert.Equal(t, 404, se.HTTPStatusCode)
	})

	t.Run("retrieve a customer", func(t *testing.T) {
		fake.seedCustomer("cus_live")
		live, err := sc.V1Customers.Retrieve(ctx, "cus_live", nil)
		require.NoError(t, err)
		assert.Equal(t, "cus_live", live.ID)
		assert.False(t, live.Deleted, "a live customer carries no deleted flag")

		fake.seedDeletedCustomer("cus_was")
		gone, err := sc.V1Customers.Retrieve(ctx, "cus_was", nil)
		require.NoError(t, err, "Stripe still retrieves a deleted customer")
		assert.True(t, gone.Deleted)
		_, err = sc.V1Customers.Delete(ctx, "cus_was", nil)
		assert.Equal(t, stripe.ErrorCodeResourceMissing, stripeAPIError(t, err).Code, "deleting it again is resource_missing")

		_, err = sc.V1Customers.Delete(ctx, "cus_live", nil)
		require.NoError(t, err)
		deleted, err := sc.V1Customers.Retrieve(ctx, "cus_live", nil)
		require.NoError(t, err)
		assert.True(t, deleted.Deleted, "a customer deleted through the fake retrieves as deleted")

		_, err = sc.V1Customers.Retrieve(ctx, "cus_never", nil)
		se := stripeAPIError(t, err)
		assert.Equal(t, stripe.ErrorCodeResourceMissing, se.Code, "an id this account never held is resource_missing")
		assert.Equal(t, 404, se.HTTPStatusCode)
	})

	t.Run("list and expire open checkout sessions", func(t *testing.T) {
		fake.seedSession("cs_open", "cus_s", "open")
		fake.seedSession("cs_done", "cus_s", "complete")
		fake.seedSession("cs_elsewhere", "cus_other", "open")

		var open []string
		for cs, err := range sc.V1CheckoutSessions.List(ctx, &stripe.CheckoutSessionListParams{
			Customer: stripe.String("cus_s"), Status: stripe.String("open"),
		}).All(ctx) {
			require.NoError(t, err)
			open = append(open, cs.ID)
		}
		assert.Equal(t, []string{"cs_open"}, open, "filtered by customer and status")

		expired, err := sc.V1CheckoutSessions.Expire(ctx, "cs_open", nil)
		require.NoError(t, err)
		assert.Equal(t, stripe.CheckoutSessionStatusExpired, expired.Status)
		assert.Equal(t, "expired", fake.sessionStatus("cs_open"))

		_, err = sc.V1CheckoutSessions.Expire(ctx, "cs_done", nil)
		require.Error(t, err, "a session that is no longer open is refused")
		se := stripeAPIError(t, err)
		assert.Equal(t, stripe.ErrorTypeInvalidRequest, se.Type)
		assert.Empty(t, se.Code, "the refusal carries no resource_missing code")
		assert.Equal(t, "complete", fake.sessionStatus("cs_done"))

		_, err = sc.V1CheckoutSessions.Expire(ctx, "cs_nope", nil)
		require.Error(t, err)
		assert.Equal(t, stripe.ErrorCodeResourceMissing, stripeAPIError(t, err).Code)
	})

	t.Run("create a portal session", func(t *testing.T) {
		const returnURL = "https://concordvoice.com/billing/return?result=portal"
		session, err := sc.V1BillingPortalSessions.Create(ctx, &stripe.BillingPortalSessionCreateParams{
			Configuration: stripe.String("bpc_concord"),
			Customer:      stripe.String("cus_p"),
			ReturnURL:     stripe.String(returnURL),
			FlowData: &stripe.BillingPortalSessionCreateFlowDataParams{
				Type: stripe.String("subscription_cancel"),
				SubscriptionCancel: &stripe.BillingPortalSessionCreateFlowDataSubscriptionCancelParams{
					Subscription: stripe.String("sub_r"),
				},
			},
		})
		require.NoError(t, err)
		assert.NotEmpty(t, session.URL)
		require.NotNil(t, session.Configuration)
		assert.Equal(t, "bpc_concord", session.Configuration.ID)
		assert.Equal(t, "cus_p", session.Customer)
		assert.Equal(t, returnURL, session.ReturnURL)
		require.NotNil(t, session.Flow)
		assert.Equal(t, stripe.BillingPortalSessionFlowType("subscription_cancel"), session.Flow.Type)

		reqs := fake.requestsTo("POST", "/v1/billing_portal/sessions")
		require.Len(t, reqs, 1)
		assert.Equal(t, "subscription_cancel", reqs[0].Form.Get("flow_data[type]"))
		assert.Equal(t, "sub_r", reqs[0].Form.Get("flow_data[subscription_cancel][subscription]"))

		plain, err := sc.V1BillingPortalSessions.Create(ctx, &stripe.BillingPortalSessionCreateParams{
			Customer: stripe.String("cus_p"),
		})
		require.NoError(t, err)
		assert.Nil(t, plain.Flow, "no flow_data means no flow")
	})
}
