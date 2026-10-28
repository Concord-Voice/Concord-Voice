package billing

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"
)

const planChangePeriodEnd int64 = 1900000000

func planChangeEnv(t *testing.T, plan, interval string, coupons ...string) (*testEnv, uuid.UUID) {
	t.Helper()
	fake := newFakeStripe(t)
	fake.seedCatalog()
	sub := fake.seedSubscription("sub_change", "cus_change", plan, interval, planChangePeriodEnd, coupons...)
	env := newTestService(t, fake)
	env.Svc.now = func() time.Time { return time.Unix(1800000000, 0) }
	user := newPayer(t, env, "cus_change")
	sub.Metadata[metadataKeyEnv] = env.Svc.Environment
	sub.Metadata[metadataKeyUserID] = user.String()
	_, err := env.Svc.Reconcile(t.Context(), "sub_change", "", "")
	require.NoError(t, err)
	return env, user
}

func postPlanChange(svc *Service, user uuid.UUID, body string) *httptest.ResponseRecorder {
	return postPlanChangeAsUser(svc, user.String(), body)
}

func postPlanChangeAsUser(svc *Service, userID, body string) *httptest.ResponseRecorder {
	var request map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &request) == nil && request["expected_effective"] == nil {
		preview := postPlanChangePreview(svc, userID, body)
		if preview.Code == http.StatusOK {
			var quote map[string]json.RawMessage
			if json.Unmarshal(preview.Body.Bytes(), &quote) == nil {
				request["expected_effective"] = quote["effective"]
				request["quoted_amount_due"] = quote["due_now"]
				request["proration_date"] = quote["proration_date"]
				request["expected_change_at"] = quote["change_at"]
				if encoded, err := json.Marshal(request); err == nil {
					body = string(encoded)
				}
			}
		}
	}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/billing/plan-change", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", userID)
	svc.PlanChange(c)
	return w
}

func postPlanChangePreview(svc *Service, userID, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/billing/plan-change/preview", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", userID)
	svc.PlanChangePreview(c)
	return w
}

func postPlanAction(user uuid.UUID, path string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	c.Set("user_id", user.String())
	handler(c)
	return w
}

func TestPlanChange_UpgradeInvoicesProratedDifferenceAndKeepsRenewal(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "month", CouponFoundersID)
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"effective":"immediate"}`, w.Body.String())
	updates := env.Fake.requestsTo(http.MethodPost, "/v1/subscriptions/sub_change")
	require.Len(t, updates, 1)
	assert.Equal(t, "always_invoice", updates[0].Form.Get("proration_behavior"))
	assert.Equal(t, "pending_if_incomplete", updates[0].Form.Get("payment_behavior"))
	previews := env.Fake.requestsTo(http.MethodPost, "/v1/invoices/create_preview")
	require.Len(t, previews, 2)
	assert.Equal(t, previews[1].Form.Get("subscription_details[proration_date]"), updates[0].Form.Get("proration_date"))
	assert.Equal(t, "unchanged", updates[0].Form.Get("billing_cycle_anchor"))
	assert.Equal(t, "price_WM", updates[0].Form.Get("items[0][price]"))
	assert.NotContains(t, updates[0].Form, "discounts", "existing founders discount stays on the subscription")
	var plan string
	var amount int64
	require.NoError(t, env.DB.QueryRow(`SELECT plan, stripe_unit_amount FROM subscriptions WHERE stripe_subscription_id=$1`, "sub_change").Scan(&plan, &amount))
	assert.Equal(t, PlanWing, plan)
	assert.EqualValues(t, 749, amount, "1499 cents at 50% rounds down")
}

func TestPlanChange_MonthToYearCreditsUnusedMonth(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "month")
	w := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"year"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	updates := env.Fake.requestsTo(http.MethodPost, "/v1/subscriptions/sub_change")
	require.Len(t, updates, 1)
	assert.Equal(t, "now", updates[0].Form.Get("billing_cycle_anchor"))
	assert.Equal(t, "always_invoice", updates[0].Form.Get("proration_behavior"))
	assert.Equal(t, "price_SY", updates[0].Form.Get("items[0][price]"))
}

func TestPlanChange_YearToMonthBooksScheduleAndCancelClearsIt(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "year", CouponFoundersID)
	env.Fake.subscriptions[0].AutomaticTax = &stripe.SubscriptionAutomaticTax{
		Enabled:   true,
		Liability: &stripe.SubscriptionAutomaticTaxLiability{Type: "account", Account: &stripe.Account{ID: "acct_tax"}},
	}
	w := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"month"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"effective":"scheduled","change_at":"2030-03-17T17:46:40Z"}`, w.Body.String())
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscriptions/sub_change"))
	creates := env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules")
	require.Len(t, creates, 1)
	assert.Equal(t, "sub_change", creates[0].Form.Get("from_subscription"))
	scheduleID := env.Fake.subscriptions[0].Schedule
	updates := env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/"+scheduleID)
	require.Len(t, updates, 1)
	assert.Equal(t, "release", updates[0].Form.Get("end_behavior"))
	assert.Equal(t, "none", updates[0].Form.Get("proration_behavior"))
	assert.Equal(t, "price_SM", updates[0].Form.Get("phases[1][items][0][price]"))
	assert.Equal(t, "di_sub_change_0", updates[0].Form.Get("phases[0][discounts][0][discount]"))
	assert.Equal(t, "di_sub_change_0", updates[0].Form.Get("phases[1][discounts][0][discount]"))
	assert.Equal(t, "true", updates[0].Form.Get("phases[0][automatic_tax][enabled]"))
	assert.Equal(t, "true", updates[0].Form.Get("phases[1][automatic_tax][enabled]"))
	assert.Equal(t, "account", updates[0].Form.Get("phases[0][automatic_tax][liability][type]"))
	assert.Equal(t, "acct_tax", updates[0].Form.Get("phases[1][automatic_tax][liability][account]"))
	var pendingPlan, pendingInterval string
	require.NoError(t, env.DB.QueryRow(`SELECT pending_plan, pending_billing_interval FROM subscriptions WHERE stripe_subscription_id=$1`, "sub_change").Scan(&pendingPlan, &pendingInterval))
	assert.Equal(t, PlanSupersonic, pendingPlan)
	assert.Equal(t, "month", pendingInterval)
	portal := postPortal(env.Svc, user, `{"flow":"subscription_cancel"}`)
	require.Equal(t, http.StatusCreated, portal.Code, portal.Body.String())
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/"+scheduleID+"/release"), "opening Portal must preserve the booking")
	cancel := postPlanAction(user, "/billing/subscription-cancel", env.Svc.CancelSubscription)
	require.Equal(t, http.StatusOK, cancel.Code, cancel.Body.String())
	assert.True(t, env.Fake.subscriptions[0].CancelAtPeriodEnd)
	releases := env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/"+scheduleID+"/release")
	require.Len(t, releases, 1)
	assert.Equal(t, "true", releases[0].Form.Get("preserve_cancel_date"))
	var pending sql.NullString
	require.NoError(t, env.DB.QueryRow(`SELECT pending_plan FROM subscriptions WHERE stripe_subscription_id=$1`, "sub_change").Scan(&pending))
	assert.False(t, pending.Valid, "cancel clears the booked change in Reconcile")
}

func TestPlanChange_CancelAfterBookedPhaseStarts(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "month")
	book := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"year"}`)
	require.Equal(t, http.StatusOK, book.Code, book.Body.String())
	sub := env.Fake.subscriptions[0]
	scheduleID := sub.Schedule
	schedule := env.Fake.schedules[scheduleID]
	for _, price := range env.Fake.prices {
		if price.ID == "price_SY" {
			sub.Items.Data[0].Price = price
			break
		}
	}
	require.Equal(t, "price_SY", sub.Items.Data[0].Price.ID)
	sub.Items.Data[0].CurrentPeriodEnd = planChangePeriodEnd + 365*24*3600
	schedule.CurrentPhase.StartDate = planChangePeriodEnd
	schedule.CurrentPhase.EndDate = sub.Items.Data[0].CurrentPeriodEnd
	env.Svc.now = func() time.Time { return time.Unix(planChangePeriodEnd+1, 0) }
	_, err := env.Svc.Reconcile(t.Context(), sub.ID, "", "")
	require.NoError(t, err)
	var attached sql.NullString
	var pending sql.NullString
	require.NoError(t, env.DB.QueryRow(`SELECT stripe_schedule_id, pending_plan FROM subscriptions
		WHERE stripe_subscription_id=$1`, sub.ID).Scan(&attached, &pending))
	assert.Equal(t, scheduleID, attached.String)
	assert.False(t, pending.Valid, "booked change has taken effect")

	cancel := postPlanAction(user, "/billing/subscription-cancel", env.Svc.CancelSubscription)
	require.Equal(t, http.StatusOK, cancel.Code, cancel.Body.String())
	assert.True(t, sub.CancelAtPeriodEnd)
	assert.Len(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/"+scheduleID+"/release"), 1)
	require.NoError(t, env.DB.QueryRow(`SELECT stripe_schedule_id FROM subscriptions
		WHERE stripe_subscription_id=$1`, sub.ID).Scan(&attached))
	assert.False(t, attached.Valid)
}

func TestPlanChange_BookedTargetMustBeWithdrawnBeforeAnotherChange(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "year", CouponFoundersID)
	first := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"year"}`)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	scheduleID := env.Fake.subscriptions[0].Schedule
	preview := postPlanChangePreview(env.Svc, user.String(), `{"plan":"wing","interval":"month"}`)
	requireError(t, preview, http.StatusConflict, "pending_plan_change")
	second := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month","expected_effective":"scheduled","quoted_amount_due":0}`)
	requireError(t, second, http.StatusConflict, "pending_plan_change")
	assert.Equal(t, scheduleID, env.Fake.subscriptions[0].Schedule)
	assert.Len(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/"+scheduleID), 1)
	keep := postPlanAction(user, "/billing/plan-change/cancel", env.Svc.KeepCurrentPlan)
	require.Equal(t, http.StatusOK, keep.Code, keep.Body.String())
	second = postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	assert.NotEqual(t, scheduleID, env.Fake.subscriptions[0].Schedule)
	assert.Len(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules"), 2)
	updates := env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/"+env.Fake.subscriptions[0].Schedule)
	require.Len(t, updates, 1)
	assert.Equal(t, "price_WM", updates[0].Form.Get("phases[1][items][0][price]"))
	var pending string
	require.NoError(t, env.DB.QueryRow(`SELECT pending_billing_interval FROM subscriptions WHERE stripe_subscription_id=$1`, "sub_change").Scan(&pending))
	assert.Equal(t, "month", pending)
}

func TestPlanChange_KeepCurrentPlanBeforeImmediateChange(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "month")
	book := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"month"}`)
	require.Equal(t, http.StatusOK, book.Code, book.Body.String())
	scheduleID := env.Fake.subscriptions[0].Schedule
	keep := postPlanAction(user, "/billing/plan-change/cancel", env.Svc.KeepCurrentPlan)
	require.Equal(t, http.StatusOK, keep.Code, keep.Body.String())
	assert.Empty(t, env.Fake.subscriptions[0].Schedule)
	assert.Len(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/"+scheduleID+"/release"), 1)
	var pending sql.NullString
	require.NoError(t, env.DB.QueryRow(`SELECT pending_plan FROM subscriptions WHERE stripe_subscription_id=$1`, "sub_change").Scan(&pending))
	assert.False(t, pending.Valid)

	book = postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"month"}`)
	require.Equal(t, http.StatusOK, book.Code, book.Body.String())
	scheduleID = env.Fake.subscriptions[0].Schedule
	change := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"year","expected_effective":"immediate","quoted_amount_due":500,"proration_date":1800000000}`)
	requireError(t, change, http.StatusConflict, "pending_plan_change")
	assert.Equal(t, scheduleID, env.Fake.subscriptions[0].Schedule)
	keep = postPlanAction(user, "/billing/plan-change/cancel", env.Svc.KeepCurrentPlan)
	require.Equal(t, http.StatusOK, keep.Code, keep.Body.String())
	change = postPlanChange(env.Svc, user, `{"plan":"wing","interval":"year"}`)
	require.Equal(t, http.StatusOK, change.Code, change.Body.String())
	assert.Empty(t, env.Fake.subscriptions[0].Schedule)
	assert.Len(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/"+scheduleID+"/release"), 1)
	assert.Equal(t, "price_WY", env.Fake.subscriptions[0].Items.Data[0].Price.ID)
}

func TestPlanChange_KeepCurrentPlanNeedsAppBooking(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "month")
	w := postPlanAction(user, "/billing/plan-change/cancel", env.Svc.KeepCurrentPlan)
	requireError(t, w, http.StatusConflict, "plan_change_conflict")
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules"))
}

func TestPlanChange_BookedTargetSurvivesRejectedNewChange(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "month")
	book := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"month"}`)
	require.Equal(t, http.StatusOK, book.Code, book.Body.String())
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"year","expected_effective":"immediate","quoted_amount_due":500,"proration_date":1800000000}`)
	requireError(t, w, http.StatusConflict, "pending_plan_change")
	require.NotEmpty(t, env.Fake.subscriptions[0].Schedule)
	assert.Equal(t, PlanSupersonic, env.Fake.schedules[env.Fake.subscriptions[0].Schedule].Metadata[planChangePlan])
}

func TestPlanChange_MixedUpgradeNowAndIntervalAtPeriodEnd(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "year")
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"effective":"immediate_then_scheduled","change_at":"2030-03-17T17:46:40Z"}`, w.Body.String())
	updates := env.Fake.requestsTo(http.MethodPost, "/v1/subscriptions/sub_change")
	require.Len(t, updates, 2)
	assert.Equal(t, "price_WY", updates[0].Form.Get("items[0][price]"))
	assert.Equal(t, "unchanged", updates[0].Form.Get("billing_cycle_anchor"))
	assert.Equal(t, "", updates[1].Form.Get("metadata[concord_mixed_month_at]"))
	schedule := env.Fake.subscriptions[0].Schedule
	phase := env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/"+schedule)
	require.Len(t, phase, 1)
	assert.Equal(t, "price_WM", phase[0].Form.Get("phases[1][items][0][price]"))
}

func TestPlanChange_MixedBookingFailureReportsAppliedUpgrade(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "year")
	env.Fake.failOn(http.MethodPost, "/v1/subscription_schedules/sub_sched_1", http.StatusInternalServerError, serverErrorBody)
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"effective":"immediate","booking_failed":true}`, w.Body.String())
	assert.Equal(t, "price_WY", env.Fake.subscriptions[0].Items.Data[0].Price.ID)
	assert.Empty(t, env.Fake.subscriptions[0].Schedule)
}

func TestPlanChange_MixedDowngradeWaitsEntirely(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "month")
	w := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"year"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscriptions/sub_change"))
	phase := env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/"+env.Fake.subscriptions[0].Schedule)
	require.Len(t, phase, 1)
	assert.Equal(t, "price_SY", phase[0].Form.Get("phases[1][items][0][price]"))
}

func TestPlanChange_PreviewsExactChargeAndRejectsChangedQuote(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "month")
	preview := postPlanChangePreview(env.Svc, user.String(), `{"plan":"wing","interval":"month"}`)
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	assert.JSONEq(t, `{"effective":"immediate","due_now":500,"currency":"usd","proration_date":1800000000}`, preview.Body.String())
	previews := env.Fake.requestsTo(http.MethodPost, "/v1/invoices/create_preview")
	require.Len(t, previews, 1)
	assert.Equal(t, "always_invoice", previews[0].Form.Get("subscription_details[proration_behavior]"))
	assert.Equal(t, "unchanged", previews[0].Form.Get("subscription_details[billing_cycle_anchor][type]"))
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month","expected_effective":"immediate","quoted_amount_due":999,"proration_date":1800000000}`)
	requireError(t, w, http.StatusConflict, "plan_change_conflict")
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscriptions/sub_change"))
}

func TestPlanChange_PreviewRejectsExpiredPaidPeriod(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "month")
	env.Svc.now = func() time.Time { return time.Unix(planChangePeriodEnd+1, 0) }
	w := postPlanChangePreview(env.Svc, user.String(), `{"plan":"supersonic","interval":"month"}`)
	requireError(t, w, http.StatusConflict, "plan_change_conflict")
}

func TestPlanChange_ScheduledQuoteRejectsChangedPeriod(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "year")
	preview := postPlanChangePreview(env.Svc, user.String(), `{"plan":"supersonic","interval":"month"}`)
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	assert.Contains(t, preview.Body.String(), `"change_at":"2030-03-17T17:46:40Z"`)
	env.Fake.subscriptions[0].Items.Data[0].CurrentPeriodEnd += 365 * 24 * 3600
	w := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"month","expected_effective":"scheduled","expected_change_at":"2030-03-17T17:46:40Z"}`)
	requireError(t, w, http.StatusConflict, "plan_change_conflict")
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules"))
}

func TestPlanChange_RestoresBookingAfterRequestCanceled(t *testing.T) {
	fake := newFakeStripe(t)
	fake.seedCatalog()
	sub := fake.seedSubscription("sub_restore", "cus_restore", PlanSupersonic, "year", planChangePeriodEnd)
	schedule := &fakeSchedule{
		ID: "sub_sched_restore", Object: "subscription_schedule", Status: "active",
		Metadata: map[string]string{
			planChangeMarker: "v1", planChangePlan: PlanSupersonic,
			planChangeInterval: "month", planChangeAt: "1900000000",
		},
	}
	schedule.CurrentPhase.StartDate = 1800000000
	schedule.CurrentPhase.EndDate = planChangePeriodEnd
	fake.schedules[schedule.ID] = schedule
	sub.Schedule = schedule.ID
	svc := NewService(Deps{Stripe: fake.client()})
	svc.now = func() time.Time { return time.Unix(1800000001, 0) }
	ctx, cancel := context.WithCancel(t.Context())
	stripeSub, err := svc.Stripe.V1Subscriptions.Retrieve(ctx, sub.ID, nil)
	require.NoError(t, err)
	prior, err := svc.releaseAppSchedule(ctx, stripeSub)
	require.NoError(t, err)
	require.NotNil(t, prior)
	require.Empty(t, sub.Schedule)
	cancel()
	require.NoError(t, svc.restoreBookedChange(ctx, sub.ID, prior))
	require.NotEmpty(t, sub.Schedule)
	assert.Equal(t, stripe.SubscriptionScheduleStatusActive, stripe.SubscriptionScheduleStatus(fake.schedules[sub.Schedule].Status))
}

func TestPlanChange_DeclinedCardGetsSpecificError(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "month")
	env.Fake.failOn(http.MethodPost, "/v1/subscriptions/sub_change", http.StatusPaymentRequired,
		`{"error":{"type":"card_error","code":"card_declined","decline_code":"insufficient_funds","message":"Card declined"}}`)
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	requireError(t, w, http.StatusPaymentRequired, "card_declined")
}

func TestPlanChange_AuthenticationRequiredOpensPendingInvoice(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "month")
	env.Fake.subscriptions[0].pendingPaymentStatus = "requires_action"
	oldPrice := env.Fake.subscriptions[0].Items.Data[0].Price.ID
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.JSONEq(t, `{"effective":"payment_pending","payment_status":"requires_action","payment_url":"https://invoice.stripe.com/i/test_sub_change"}`, w.Body.String())
	assert.Equal(t, oldPrice, env.Fake.subscriptions[0].Items.Data[0].Price.ID)
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules"))
	preview := postPlanChangePreview(env.Svc, user.String(), `{"plan":"wing","interval":"month"}`)
	requireError(t, preview, http.StatusConflict, "payment_pending")
}

func TestPlanChange_DeclinedPendingPaymentKeepsOldPlan(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "month")
	env.Fake.subscriptions[0].pendingPaymentStatus = "requires_payment_method"
	oldPrice := env.Fake.subscriptions[0].Items.Data[0].Price.ID
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"payment_status":"card_declined"`)
	assert.Equal(t, oldPrice, env.Fake.subscriptions[0].Items.Data[0].Price.ID)
}

func TestPlanChange_PendingInvoiceUsesLaterActionablePayment(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "month")
	sub := env.Fake.subscriptions[0]
	sub.pendingPriorPaymentStatus = "canceled"
	sub.pendingPaymentStatus = "requires_payment_method"
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"payment_status":"card_declined"`)
}

func TestPlanChange_ScheduledChangeClearsOrphanedMixedMarker(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "year")
	sub := env.Fake.subscriptions[0]
	// Stripe expired an unpaid mixed upgrade, leaving its metadata marker.
	sub.Metadata[planChangeMixedAt] = time.Unix(planChangePeriodEnd, 0).UTC().Format(time.RFC3339)
	w := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"month"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotEmpty(t, sub.Schedule)
	assert.Empty(t, sub.Metadata[planChangeMixedAt], "the later booking supersedes the expired mixed request")
}

func TestPlanChange_RejectsUnsafeHostedInvoiceURL(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "month")
	sub := env.Fake.subscriptions[0]
	sub.pendingPaymentStatus = "requires_action"
	sub.pendingInvoiceURL = "https://invoice.stripe.com.attacker.example/i/redirect"
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	requireError(t, w, http.StatusServiceUnavailable, "billing_unavailable")
	assert.NotNil(t, sub.PendingUpdate, "Stripe still owns the unpaid pending update")
}

func TestPlanChange_AuthenticatedMixedUpgradeBooksIntervalAfterPayment(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "year")
	sub := env.Fake.subscriptions[0]
	sub.pendingPaymentStatus = "requires_action"
	oldPrice := sub.Items.Data[0].Price.ID
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Equal(t, oldPrice, sub.Items.Data[0].Price.ID)
	assert.Empty(t, sub.Schedule)
	for _, price := range env.Fake.prices {
		if price.ID == sub.pendingPriceID {
			sub.Items.Data[0].Price = price
			break
		}
	}
	for key, value := range sub.pendingMetadata {
		sub.Metadata[key] = value
	}
	sub.PendingUpdate = nil // Stripe applies the pending update after authentication.
	_, err := env.Svc.Reconcile(t.Context(), sub.ID, "", "")
	require.NoError(t, err)
	require.NotEmpty(t, sub.Schedule)
	assert.Equal(t, PlanWing, env.Fake.schedules[sub.Schedule].Metadata[planChangePlan])
	assert.Equal(t, "month", env.Fake.schedules[sub.Schedule].Metadata[planChangeInterval])
}

func TestPlanChange_AuthenticatedMixedQuoteDoesNotMoveAfterRenewal(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "year")
	sub := env.Fake.subscriptions[0]
	sub.pendingPaymentStatus = "requires_action"
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	for _, price := range env.Fake.prices {
		if price.ID == sub.pendingPriceID {
			sub.Items.Data[0].Price = price
			break
		}
	}
	for key, value := range sub.pendingMetadata {
		sub.Metadata[key] = value
	}
	sub.PendingUpdate = nil
	sub.Items.Data[0].CurrentPeriodEnd += 365 * 24 * 3600
	_, err := env.Svc.Reconcile(t.Context(), sub.ID, "", "")
	require.NoError(t, err)
	assert.Empty(t, sub.Schedule, "the subscriber did not accept a change a year later")
	assert.Empty(t, sub.Metadata[planChangeMixedAt])
}

func TestPlanChange_FailuresDoNotChangeStoredPlan(t *testing.T) {
	for _, tc := range []struct{ name, method, path string }{
		{"price missing", "", ""},
		{"subscription update rejected", http.MethodPost, "/v1/subscriptions/sub_change"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, user := planChangeEnv(t, PlanSupersonic, "month")
			if tc.method == "" {
				for _, price := range env.Fake.prices {
					if price.LookupKey == LookupKeyWingMonthly {
						price.Active = false
					}
				}
			} else {
				env.Fake.failOn(tc.method, tc.path, http.StatusInternalServerError, serverErrorBody)
			}
			body := `{"plan":"wing","interval":"month"}`
			if tc.method == "" {
				body = `{"plan":"wing","interval":"month","expected_effective":"immediate","quoted_amount_due":500,"proration_date":1800000000}`
			}
			w := postPlanChange(env.Svc, user, body)
			requireError(t, w, http.StatusServiceUnavailable, "billing_unavailable")
			var plan string
			require.NoError(t, env.DB.QueryRow(`SELECT plan FROM subscriptions WHERE stripe_subscription_id=$1`, "sub_change").Scan(&plan))
			assert.Equal(t, PlanSupersonic, plan)
		})
	}
}

func TestPlanChange_ScheduleUpdateFailureReleasesIncompleteSchedule(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "month")
	// The fake's first schedule id is deterministic in this fixture.
	env.Fake.failOn(http.MethodPost, "/v1/subscription_schedules/sub_sched_1", http.StatusInternalServerError, serverErrorBody)
	w := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"month"}`)
	requireError(t, w, http.StatusServiceUnavailable, "billing_unavailable")
	assert.Len(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/sub_sched_1/release"), 1)
	assert.Empty(t, env.Fake.subscriptions[0].Schedule)
}

func TestPlanChange_CancelFailsClosedWhenScheduleCannotBeReleased(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "month")
	w := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"month"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	scheduleID := env.Fake.subscriptions[0].Schedule
	env.Fake.failOn(http.MethodPost, "/v1/subscription_schedules/"+scheduleID+"/release", http.StatusInternalServerError, serverErrorBody)
	cancel := postPlanAction(user, "/billing/subscription-cancel", env.Svc.CancelSubscription)
	requireError(t, cancel, http.StatusServiceUnavailable, "billing_unavailable")
	assert.False(t, env.Fake.subscriptions[0].CancelAtPeriodEnd)
}

func TestPlanChange_FailedCancellationRestoresBooking(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "month")
	w := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"month"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env.Fake.failOn(http.MethodPost, "/v1/subscriptions/sub_change", http.StatusInternalServerError, serverErrorBody)
	cancel := postPlanAction(user, "/billing/subscription-cancel", env.Svc.CancelSubscription)
	requireError(t, cancel, http.StatusServiceUnavailable, "billing_unavailable")
	require.NotEmpty(t, env.Fake.subscriptions[0].Schedule)
	assert.Equal(t, PlanSupersonic, env.Fake.schedules[env.Fake.subscriptions[0].Schedule].Metadata[planChangePlan])
}

func TestPlanChange_RejectsMissingOrUnownedSubscription(t *testing.T) {
	env, owner := planChangeEnv(t, PlanWing, "month")
	w := postPlanChange(env.Svc, owner, `{"plan":"gold","interval":"month"}`)
	requireError(t, w, http.StatusBadRequest, "invalid_request")
	w = postPlanChange(env.Svc, owner, `{"plan":"wing","interval":"month"}`)
	requireError(t, w, http.StatusConflict, "plan_change_conflict")
	other := uuid.New()
	w = postPlanChange(env.Svc, other, `{"plan":"supersonic","interval":"month"}`)
	requireError(t, w, http.StatusNotFound, "no_billing_account")
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscriptions/sub_change"))
}

func TestPlanChange_RejectsUnauthenticatedAndStaleSubscriptions(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "month")
	w := postPlanChangeAsUser(env.Svc, "", `{"plan":"supersonic","interval":"month"}`)
	requireError(t, w, http.StatusUnauthorized, "unauthorized")
	env.Fake.subscriptions[0].Status = "past_due"
	w = postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"month"}`)
	requireError(t, w, http.StatusConflict, "plan_change_conflict")
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscriptions/sub_change"))
}

func TestPlanChange_RejectsUnmanagedSchedule(t *testing.T) {
	env, user := planChangeEnv(t, PlanSupersonic, "month")
	env.Fake.subscriptions[0].Schedule = "sub_foreign"
	env.Fake.schedules["sub_foreign"] = &fakeSchedule{ID: "sub_foreign", Object: "subscription_schedule", Status: "active"}
	w := postPlanChange(env.Svc, user, `{"plan":"wing","interval":"month"}`)
	requireError(t, w, http.StatusConflict, "plan_change_conflict")
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscriptions/sub_change"))
	cancel := postPlanAction(user, "/billing/subscription-cancel", env.Svc.CancelSubscription)
	requireError(t, cancel, http.StatusConflict, "plan_change_conflict")
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/sub_foreign/release"))
}

func TestPlanChange_ScheduleRetrieveFailureReleasesIncompleteSchedule(t *testing.T) {
	env, user := planChangeEnv(t, PlanWing, "month")
	env.Fake.failOn(http.MethodGet, "/v1/subscription_schedules/sub_sched_1", http.StatusInternalServerError, serverErrorBody)
	w := postPlanChange(env.Svc, user, `{"plan":"supersonic","interval":"month"}`)
	requireError(t, w, http.StatusServiceUnavailable, "billing_unavailable")
	assert.Len(t, env.Fake.requestsTo(http.MethodPost, "/v1/subscription_schedules/sub_sched_1/release"), 1)
}
