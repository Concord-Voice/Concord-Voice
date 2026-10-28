package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v87"
)

const (
	planChangeMaxBody  = 4 << 10
	planChangeMarker   = "concord_plan_change"
	planChangePlan     = "concord_pending_plan"
	planChangeInterval = "concord_pending_interval"
	planChangeAt       = "concord_pending_at"
	planChangeMixedAt  = "concord_mixed_month_at"
)

var errForeignSchedule = errors.New("billing: subscription has an unmanaged schedule")
var errUnchangeable = errors.New("billing: subscription cannot change plan")
var errPendingPlanChange = errors.New("billing: withdraw booked change before choosing another")
var errPaymentPending = errors.New("billing: complete the pending plan payment before another change")

type planChangeRequest struct {
	Plan              string `json:"plan"`
	Interval          string `json:"interval"`
	ExpectedEffective string `json:"expected_effective,omitempty"`
	ExpectedChangeAt  string `json:"expected_change_at,omitempty"`
	QuotedAmountDue   *int64 `json:"quoted_amount_due,omitempty"`
	ProrationDate     *int64 `json:"proration_date,omitempty"`
}

// A mixed upgrade/year-to-month first upgrades at the existing interval, then
// books the interval change. A mixed downgrade/month-to-year waits entirely.
func changeTiming(oldPlan, oldInterval, newPlan, newInterval string) (string, error) {
	if oldPlan == newPlan && oldInterval == newInterval {
		return "", errUnchangeable
	}
	if oldPlan == PlanSupersonic && oldInterval == "year" && newPlan == PlanWing && newInterval == "month" {
		return "immediate_then_scheduled", nil
	}
	if (oldPlan == PlanWing && newPlan == PlanSupersonic) || (oldInterval == "year" && newInterval == "month") {
		return "scheduled", nil
	}
	if (oldPlan == PlanSupersonic && newPlan == PlanWing) || (oldInterval == "month" && newInterval == "year") {
		return "immediate", nil
	}
	return "scheduled", nil
}

// PlanChange changes only the authenticated subscriber's mapped subscription.
// The per-subscription advisory lock serializes Stripe writes with Reconcile
// and with other plan changes; neither the Customer nor subscription id comes
// from the request body.
func (s *Service) PlanChange(c *gin.Context) {
	req, ok := bindStrictJSON(c, planChangeMaxBody, func(r planChangeRequest) bool {
		_, known := findOffer(r.Plan, r.Interval)
		return known
	})
	if !ok {
		return
	}
	conn, subID, customer, ok := s.planChangeAccount(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	defer func() {
		if conn != nil {
			s.unlockSubscription(ctx, conn, subID)
		}
	}()
	mode, end, err := s.changeStripePlan(ctx, subID, customer, req)
	if mode == "partial" {
		s.Log.Error("billing: immediate upgrade succeeded but booking failed", logKeyErrorClass, errorClass(err), logKeyStage, "mixed_plan_booking")
		s.unlockSubscription(ctx, conn, subID)
		conn = nil
		s.reconcilePlanChange(ctx, subID)
		c.JSON(http.StatusOK, gin.H{"effective": "immediate", "booking_failed": true})
		return
	}
	if mode == "payment_pending" && err == nil {
		paymentStatus, paymentURL, paymentErr := s.pendingPlanPayment(ctx, subID)
		if paymentErr != nil {
			s.planChangeFailure(c, paymentErr)
			return
		}
		s.unlockSubscription(ctx, conn, subID)
		conn = nil
		s.reconcilePlanChange(ctx, subID)
		c.JSON(http.StatusAccepted, gin.H{"effective": "payment_pending", "payment_status": paymentStatus, "payment_url": paymentURL})
		return
	}
	if err != nil && mode != "" {
		// A failed charge can follow a released booking; restoration may have
		// created a new schedule id, so refresh the local row before replying.
		s.unlockSubscription(ctx, conn, subID)
		conn = nil
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		s.reconcilePlanChange(reconcileCtx, subID)
		cancel()
	}
	switch {
	case errors.Is(err, errPendingPlanChange):
		abortWebhook(c, http.StatusConflict, "pending_plan_change")
		return
	case errors.Is(err, errPaymentPending):
		abortWebhook(c, http.StatusConflict, "payment_pending")
		return
	case errors.Is(err, errUnchangeable), errors.Is(err, errForeignSchedule):
		abortWebhook(c, http.StatusConflict, "plan_change_conflict")
		return
	case err != nil:
		var cardErr *stripe.CardError
		if errors.As(err, &cardErr) {
			abortWebhook(c, http.StatusPaymentRequired, "card_declined")
			return
		}
		s.planChangeFailure(c, err)
		return
	}
	s.unlockSubscription(ctx, conn, subID)
	conn = nil
	s.reconcilePlanChange(ctx, subID)
	switch mode {
	case "scheduled", "immediate_then_scheduled":
		c.JSON(http.StatusOK, gin.H{"effective": mode, "change_at": end.UTC().Format(time.RFC3339)})
	default:
		c.JSON(http.StatusOK, gin.H{"effective": mode})
	}
}

// planChangeAccount locks the authenticated user's mapped live subscription.
// The caller owns the returned connection until its Stripe mutation finishes.
func (s *Service) planChangeAccount(c *gin.Context) (*sql.Conn, string, string, bool) {
	userID, err := uuid.Parse(c.GetString("user_id"))
	if err != nil {
		abortWebhook(c, http.StatusUnauthorized, portalErrUnauth)
		return nil, "", "", false
	}
	ctx := c.Request.Context()
	customer, ok := s.portalCustomer(c, userID)
	if !ok {
		return nil, "", "", false
	}
	subID, err := s.cancelableSubscription(ctx, userID, customer)
	if errors.Is(err, errNoLiveSubscription) {
		abortWebhook(c, http.StatusNotFound, portalErrNoAccount)
		return nil, "", "", false
	}
	if err != nil {
		s.planChangeFailure(c, err)
		return nil, "", "", false
	}
	conn, err := s.lockSubscription(ctx, subID)
	if err != nil {
		s.planChangeFailure(c, err)
		return nil, "", "", false
	}
	// Re-check ownership under the lock; a concurrent erasure or mapping change
	// must not authorize a mutation based on the earlier advisory lookup.
	var owned bool
	err = conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM subscriptions WHERE user_id=$1
		AND stripe_customer_id=$2 AND stripe_subscription_id=$3 AND source='stripe'
		AND EXISTS (SELECT 1 FROM stripe_customers WHERE user_id=$1 AND stripe_customer_id=$2))`, userID, customer, subID).Scan(&owned)
	if err != nil {
		s.planChangeFailure(c, err)
		s.unlockSubscription(ctx, conn, subID)
		return nil, "", "", false
	}
	if !owned {
		abortWebhook(c, http.StatusNotFound, portalErrNoAccount)
		s.unlockSubscription(ctx, conn, subID)
		return nil, "", "", false
	}
	return conn, subID, customer, true
}

func (s *Service) reconcilePlanChange(ctx context.Context, subID string) {
	if _, err := s.Reconcile(ctx, subID, "", ""); err != nil {
		s.Log.Error("billing: plan change succeeded but reconcile failed", logKeyErrorClass, errorClass(err), logKeyStage, "plan_change_reconcile")
	}
}

// PlanChangePreview asks Stripe for the invoice due from this exact proposed
// update. The returned proration date is reused in the confirming mutation.
func (s *Service) PlanChangePreview(c *gin.Context) {
	req, ok := bindStrictJSON(c, planChangeMaxBody, func(r planChangeRequest) bool {
		_, known := findOffer(r.Plan, r.Interval)
		return known
	})
	if !ok {
		return
	}
	conn, subID, customer, ok := s.planChangeAccount(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	defer s.unlockSubscription(ctx, conn, subID)
	params := &stripe.SubscriptionRetrieveParams{}
	params.AddExpand("items.data.price")
	params.AddExpand("discounts")
	sub, err := s.Stripe.V1Subscriptions.Retrieve(ctx, subID, params)
	if err != nil {
		s.planChangeFailure(c, err)
		return
	}
	if sub.Customer == nil || sub.Customer.ID != customer || sub.Status != stripe.SubscriptionStatusActive || sub.CancelAtPeriodEnd || sub.CancelAt != 0 || len(sub.Items.Data) != 1 {
		abortWebhook(c, http.StatusConflict, "plan_change_conflict")
		return
	}
	fields, err := classify(sub)
	if err != nil || fields.Malformed != "" || sub.Items.Data[0].ID == "" {
		abortWebhook(c, http.StatusConflict, "plan_change_conflict")
		return
	}
	if sub.Schedule != nil {
		schedule, err := s.Stripe.V1SubscriptionSchedules.Retrieve(ctx, sub.Schedule.ID, nil)
		if err != nil {
			s.planChangeFailure(c, err)
			return
		}
		if schedule.Status != stripe.SubscriptionScheduleStatusActive || schedule.Metadata[planChangeMarker] != "v1" {
			abortWebhook(c, http.StatusConflict, "plan_change_conflict")
			return
		}
		if _, pending := pendingScheduleAt(schedule, s.now()); pending {
			abortWebhook(c, http.StatusConflict, "pending_plan_change")
			return
		}
	}
	if sub.PendingUpdate != nil {
		abortWebhook(c, http.StatusConflict, "payment_pending")
		return
	}
	mode, err := changeTiming(fields.Plan.String, fields.Interval.String, req.Plan, req.Interval)
	if err != nil {
		abortWebhook(c, http.StatusConflict, "plan_change_conflict")
		return
	}
	target, err := s.resolvePrice(ctx, req.Plan, req.Interval)
	if err != nil {
		s.planChangeFailure(c, err)
		return
	}
	if mode == "scheduled" {
		if !fields.PeriodEnd.After(s.now()) {
			abortWebhook(c, http.StatusConflict, "plan_change_conflict")
			return
		}
		c.JSON(http.StatusOK, gin.H{"effective": mode, "due_now": 0, "currency": "usd", "change_at": fields.PeriodEnd.UTC().Format(time.RFC3339)})
		return
	}
	price := target
	if mode == "immediate_then_scheduled" {
		price, err = s.resolvePrice(ctx, req.Plan, "year")
		if err != nil {
			s.planChangeFailure(c, err)
			return
		}
	}
	prorationDate := s.now().Unix()
	amount, err := s.previewCharge(ctx, sub, price.ID, fields.Interval.String == "month" && req.Interval == "year", prorationDate)
	if err != nil {
		s.planChangeFailure(c, err)
		return
	}
	response := gin.H{"effective": mode, "due_now": amount, "currency": "usd", "proration_date": prorationDate}
	if mode == "immediate_then_scheduled" {
		response["change_at"] = fields.PeriodEnd.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, response)
}

func (s *Service) previewCharge(ctx context.Context, sub *stripe.Subscription, targetPrice string, resetAnchor bool, at int64) (int64, error) {
	anchor := "unchanged"
	if resetAnchor {
		anchor = "now"
	}
	preview, err := s.Stripe.V1Invoices.CreatePreview(ctx, &stripe.InvoiceCreatePreviewParams{
		Customer: stripe.String(sub.Customer.ID), Subscription: stripe.String(sub.ID),
		SubscriptionDetails: &stripe.InvoiceCreatePreviewSubscriptionDetailsParams{
			Items: []*stripe.InvoiceCreatePreviewSubscriptionDetailsItemParams{{
				ID: stripe.String(sub.Items.Data[0].ID), Price: stripe.String(targetPrice), Quantity: stripe.Int64(1),
			}},
			BillingCycleAnchor: &stripe.InvoiceCreatePreviewSubscriptionDetailsBillingCycleAnchorParams{Type: stripe.String(anchor)},
			ProrationBehavior:  stripe.String("always_invoice"), ProrationDate: stripe.Int64(at),
		},
	})
	if err != nil {
		return 0, err
	}
	if preview.Currency != stripe.CurrencyUSD || preview.AmountDue < 0 {
		return 0, errUnchangeable
	}
	return preview.AmountDue, nil
}

func (s *Service) planChangeFailure(c *gin.Context, err error) {
	s.Log.Error("billing: plan change failed", logKeyErrorClass, errorClass(err), logKeyStage, "plan_change")
	abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
}

// pendingPlanPayment reads only the invoice Stripe just created for this
// subscription. Hosted invoice URLs are bearer links and must never be logged.
func (s *Service) pendingPlanPayment(ctx context.Context, subID string) (string, string, error) {
	params := &stripe.SubscriptionRetrieveParams{}
	params.AddExpand("latest_invoice")
	sub, err := s.Stripe.V1Subscriptions.Retrieve(ctx, subID, params)
	if err != nil {
		return "", "", err
	}
	if sub.PendingUpdate == nil || sub.LatestInvoice == nil || sub.LatestInvoice.ID == "" {
		return "", "", errUnchangeable
	}
	invoiceParams := &stripe.InvoiceRetrieveParams{}
	invoiceParams.AddExpand("payments.data.payment.payment_intent")
	invoice, err := s.Stripe.V1Invoices.Retrieve(ctx, sub.LatestInvoice.ID, invoiceParams)
	if err != nil {
		return "", "", err
	}
	u, err := url.Parse(invoice.HostedInvoiceURL)
	if err != nil || u == nil {
		return "", "", errUnchangeable
	}
	host := strings.ToLower(u.Hostname())
	hostAllowed := host == "pay.concordvoice.com" || host == "stripe.com" || strings.HasSuffix(host, ".stripe.com")
	if u.Scheme != "https" || u.User != nil || u.Port() != "" ||
		!hostAllowed {
		return "", "", errUnchangeable
	}
	status := "processing"
	if invoice.Payments != nil {
		for _, payment := range invoice.Payments.Data {
			if payment == nil || payment.Payment == nil || payment.Payment.PaymentIntent == nil {
				continue
			}
			switch payment.Payment.PaymentIntent.Status {
			case stripe.PaymentIntentStatusRequiresPaymentMethod:
				return "card_declined", u.String(), nil
			case stripe.PaymentIntentStatusRequiresAction:
				return "requires_action", u.String(), nil
			}
		}
	}
	return status, u.String(), nil
}

func (s *Service) clearMixedPlanMarker(ctx context.Context, subID string) error {
	_, err := s.Stripe.V1Subscriptions.Update(ctx, subID, &stripe.SubscriptionUpdateParams{
		Metadata: map[string]string{planChangeMixedAt: ""},
	})
	return err
}

// A successful authenticated mixed upgrade applies the yearly plan first.
// Reconcile then books its monthly phase while holding the subscription lock.
func (s *Service) completeAuthenticatedMixedChange(ctx context.Context, sub *stripe.Subscription) (bool, error) {
	quotedEnd := sub.Metadata[planChangeMixedAt]
	if quotedEnd == "" || sub.PendingUpdate != nil {
		return false, nil
	}
	if sub.Schedule != nil {
		// The marker is cleared before our own booking is attached. Stripe does
		// not permit a direct subscription update while a schedule is attached.
		return false, nil
	}
	if sub.Status != stripe.SubscriptionStatusActive || sub.CancelAtPeriodEnd || sub.CancelAt != 0 {
		return false, nil
	}
	fields, err := classify(sub)
	if err != nil {
		return false, err
	}
	if fields.Plan.String != PlanWing || fields.Interval.String != "year" ||
		quotedEnd != fields.PeriodEnd.UTC().Format(time.RFC3339) || !fields.PeriodEnd.After(s.now()) {
		// A quote must not silently move to a later renewal after authentication.
		s.Log.Error("billing: authenticated mixed booking no longer matches quoted period", logKeySubID, sub.ID, logKeyStage, "mixed_quote_expired")
		return false, s.clearMixedPlanMarker(ctx, sub.ID)
	}
	target, err := s.resolvePrice(ctx, PlanWing, "month")
	if err != nil {
		return false, err
	}
	if err := s.clearMixedPlanMarker(ctx, sub.ID); err != nil {
		return false, err
	}
	if err := s.bookPlanChange(ctx, sub, target.ID, planChangeRequest{Plan: PlanWing, Interval: "month"}, fields.PeriodEnd); err != nil {
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, restoreErr := s.Stripe.V1Subscriptions.Update(restoreCtx, sub.ID, &stripe.SubscriptionUpdateParams{
			Metadata: map[string]string{planChangeMixedAt: quotedEnd},
		})
		cancel()
		if restoreErr != nil {
			s.Log.Error("billing: could not restore mixed booking marker", logKeyErrorClass, errorClass(restoreErr), logKeyStage, "restore_mixed_marker")
		}
		return false, err
	}
	return true, nil
}

func (s *Service) changeStripePlan(ctx context.Context, subID, customer string, req planChangeRequest) (string, time.Time, error) {
	params := &stripe.SubscriptionRetrieveParams{}
	params.AddExpand("items.data.price")
	params.AddExpand("discounts")
	sub, err := s.Stripe.V1Subscriptions.Retrieve(ctx, subID, params)
	if err != nil {
		return "", time.Time{}, err
	}
	if sub.Customer == nil || sub.Customer.ID != customer || sub.Status != stripe.SubscriptionStatusActive || sub.CancelAtPeriodEnd || sub.CancelAt != 0 {
		return "", time.Time{}, errUnchangeable
	}
	fields, err := classify(sub)
	if err != nil || fields.Malformed != "" || len(sub.Items.Data) != 1 || sub.Items.Data[0].ID == "" || sub.Items.Data[0].Price == nil || sub.Items.Data[0].Price.ID == "" {
		return "", time.Time{}, errUnchangeable
	}
	if sub.Schedule != nil {
		schedule, err := s.Stripe.V1SubscriptionSchedules.Retrieve(ctx, sub.Schedule.ID, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		if schedule.Status != stripe.SubscriptionScheduleStatusActive || schedule.Metadata[planChangeMarker] != "v1" {
			return "", time.Time{}, errForeignSchedule
		}
		if _, pending := pendingScheduleAt(schedule, s.now()); pending {
			return "", time.Time{}, errPendingPlanChange
		}
	}
	if sub.PendingUpdate != nil {
		return "", time.Time{}, errPaymentPending
	}
	mode, err := changeTiming(fields.Plan.String, fields.Interval.String, req.Plan, req.Interval)
	if err != nil {
		return "", time.Time{}, err
	}
	if req.ExpectedEffective != mode {
		return "", time.Time{}, errUnchangeable
	}
	if mode == "scheduled" || mode == "immediate_then_scheduled" {
		if req.ExpectedChangeAt != fields.PeriodEnd.UTC().Format(time.RFC3339) || !fields.PeriodEnd.After(s.now()) {
			return "", time.Time{}, errUnchangeable
		}
	}
	currentTarget := req
	if mode == "immediate_then_scheduled" {
		currentTarget.Interval = "year"
	}
	price, err := s.resolvePrice(ctx, currentTarget.Plan, currentTarget.Interval)
	if err != nil {
		return "", time.Time{}, err
	}
	if mode == "immediate" || mode == "immediate_then_scheduled" {
		if req.ProrationDate == nil || req.QuotedAmountDue == nil || *req.ProrationDate > s.now().Unix() || s.now().Unix()-*req.ProrationDate > 300 {
			return "", time.Time{}, errUnchangeable
		}
		amount, err := s.previewCharge(ctx, sub, price.ID, fields.Interval.String == "month" && req.Interval == "year", *req.ProrationDate)
		if err != nil {
			return "", time.Time{}, err
		}
		if amount != *req.QuotedAmountDue {
			return "", time.Time{}, errUnchangeable
		}
		var previous *stripe.SubscriptionSchedule
		if sub.Schedule != nil {
			previous, err = s.releaseAppSchedule(ctx, sub)
			if err != nil {
				return "", time.Time{}, err
			}
			if previous == nil {
				return "", time.Time{}, errForeignSchedule
			}
		}
		update := &stripe.SubscriptionUpdateParams{
			Items:             []*stripe.SubscriptionUpdateItemParams{{ID: stripe.String(sub.Items.Data[0].ID), Price: stripe.String(price.ID), Quantity: stripe.Int64(1)}},
			ProrationBehavior: stripe.String("always_invoice"),
			PaymentBehavior:   stripe.String("pending_if_incomplete"),
			ProrationDate:     req.ProrationDate,
			Metadata:          map[string]string{planChangeMixedAt: ""},
		}
		if mode == "immediate_then_scheduled" {
			update.Metadata[planChangeMixedAt] = req.ExpectedChangeAt
		}
		if fields.Interval.String == "month" && req.Interval == "year" {
			update.BillingCycleAnchorNow = stripe.Bool(true)
		} else {
			update.BillingCycleAnchorUnchanged = stripe.Bool(true)
		}
		updated, err := s.Stripe.V1Subscriptions.Update(ctx, subID, update)
		if err != nil {
			if previous != nil {
				if restoreErr := s.restoreBookedChange(ctx, subID, previous); restoreErr != nil {
					s.Log.Error("billing: could not restore booked change after update failure", logKeyErrorClass, errorClass(restoreErr), logKeyStage, "restore_plan_change")
				}
			}
			return mode, time.Time{}, err
		}
		if updated.PendingUpdate != nil {
			return "payment_pending", time.Time{}, nil
		}
		if mode == "immediate" {
			return mode, time.Time{}, err
		}
		fresh, err := s.Stripe.V1Subscriptions.Retrieve(ctx, subID, params)
		if err != nil {
			return "partial", time.Time{}, err
		}
		freshFields, err := classify(fresh)
		if err != nil || freshFields.Malformed != "" || freshFields.Plan.String != PlanWing || freshFields.Interval.String != "year" {
			return "partial", time.Time{}, errUnchangeable
		}
		target, err := s.resolvePrice(ctx, req.Plan, req.Interval)
		if err != nil {
			return "partial", time.Time{}, err
		}
		if err := s.clearMixedPlanMarker(ctx, subID); err != nil {
			return "partial", time.Time{}, err
		}
		if err := s.bookPlanChange(ctx, fresh, target.ID, req, freshFields.PeriodEnd); err != nil {
			return "partial", time.Time{}, err
		}
		return mode, freshFields.PeriodEnd, nil
	}
	end := fields.PeriodEnd
	if !end.After(s.now()) {
		return "", time.Time{}, errUnchangeable
	}
	if sub.Metadata[planChangeMixedAt] != "" {
		if err := s.clearMixedPlanMarker(ctx, subID); err != nil {
			return "", time.Time{}, err
		}
	}
	if err := s.bookPlanChange(ctx, sub, price.ID, req, end); err != nil {
		return "", time.Time{}, err
	}
	return mode, end, nil
}

func (s *Service) bookPlanChange(ctx context.Context, sub *stripe.Subscription, targetPrice string, req planChangeRequest, end time.Time) error {
	scheduleID := ""
	created := false
	complete := false
	defer func() {
		if !created || complete {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, releaseErr := s.Stripe.V1SubscriptionSchedules.Release(cleanupCtx, scheduleID, nil); releaseErr != nil {
			s.Log.Error("billing: failed to release unfinished plan schedule", logKeyErrorClass, errorClass(releaseErr), logKeyStage, "release_incomplete_schedule")
		}
	}()
	if sub.Schedule != nil {
		scheduleID = sub.Schedule.ID
	} else {
		createdSchedule, err := s.Stripe.V1SubscriptionSchedules.Create(ctx, &stripe.SubscriptionScheduleCreateParams{FromSubscription: stripe.String(sub.ID)})
		if err != nil {
			return fmt.Errorf("create schedule: %w", err)
		}
		scheduleID = createdSchedule.ID
		created = true
	}
	schedule, err := s.Stripe.V1SubscriptionSchedules.Retrieve(ctx, scheduleID, nil)
	if err != nil {
		return fmt.Errorf("retrieve schedule: %w", err)
	}
	if !created && schedule.Metadata[planChangeMarker] != "v1" {
		return errForeignSchedule
	}
	if schedule.CurrentPhase == nil || schedule.CurrentPhase.StartDate <= 0 || schedule.CurrentPhase.EndDate != end.Unix() {
		return errUnchangeable
	}
	discounts := make([]*stripe.SubscriptionScheduleUpdatePhaseDiscountParams, 0, len(sub.Discounts))
	for _, discount := range sub.Discounts {
		if discount == nil || discount.ID == "" {
			return errUnchangeable
		}
		discounts = append(discounts, &stripe.SubscriptionScheduleUpdatePhaseDiscountParams{Discount: stripe.String(discount.ID)})
	}
	current := &stripe.SubscriptionScheduleUpdatePhaseParams{
		StartDate: stripe.Int64(schedule.CurrentPhase.StartDate), EndDate: stripe.Int64(end.Unix()),
		Items:     []*stripe.SubscriptionScheduleUpdatePhaseItemParams{{Price: stripe.String(sub.Items.Data[0].Price.ID), Quantity: stripe.Int64(1)}},
		Discounts: discounts,
	}
	next := &stripe.SubscriptionScheduleUpdatePhaseParams{
		StartDate: stripe.Int64(end.Unix()),
		Duration:  &stripe.SubscriptionScheduleUpdatePhaseDurationParams{Interval: stripe.String(req.Interval), IntervalCount: stripe.Int64(1)},
		Items:     []*stripe.SubscriptionScheduleUpdatePhaseItemParams{{Price: stripe.String(targetPrice), Quantity: stripe.Int64(1)}},
		Discounts: discounts, ProrationBehavior: stripe.String("none"),
	}
	if sub.AutomaticTax != nil {
		for _, phase := range []*stripe.SubscriptionScheduleUpdatePhaseParams{current, next} {
			phase.AutomaticTax = &stripe.SubscriptionScheduleUpdatePhaseAutomaticTaxParams{
				Enabled: stripe.Bool(sub.AutomaticTax.Enabled),
			}
			if liability := sub.AutomaticTax.Liability; liability != nil {
				phase.AutomaticTax.Liability = &stripe.SubscriptionScheduleUpdatePhaseAutomaticTaxLiabilityParams{
					Type: stripe.String(string(liability.Type)),
				}
				if liability.Account != nil {
					phase.AutomaticTax.Liability.Account = stripe.String(liability.Account.ID)
				}
			}
		}
	}
	update := &stripe.SubscriptionScheduleUpdateParams{
		EndBehavior: stripe.String("release"), ProrationBehavior: stripe.String("none"),
		Phases: []*stripe.SubscriptionScheduleUpdatePhaseParams{current, next},
		Metadata: map[string]string{planChangeMarker: "v1", planChangePlan: req.Plan,
			planChangeInterval: req.Interval, planChangeAt: fmt.Sprint(end.Unix())},
	}
	_, err = s.Stripe.V1SubscriptionSchedules.Update(ctx, scheduleID, update)
	complete = err == nil
	return err
}

// classifyPendingChange reads the schedule Stripe links to the subscription.
// The metadata is merely a display hint; the active Stripe phase is authority.
func (s *Service) classifyPendingChange(ctx context.Context, fields *rowFields) error {
	if !fields.ScheduleID.Valid {
		return nil
	}
	schedule, err := s.Stripe.V1SubscriptionSchedules.Retrieve(ctx, fields.ScheduleID.String, nil)
	if err != nil {
		return err
	}
	at, pending := pendingScheduleAt(schedule, s.now())
	if !pending {
		return nil
	}
	plan, interval := schedule.Metadata[planChangePlan], schedule.Metadata[planChangeInterval]
	if _, ok := findOffer(plan, interval); !ok {
		return nil
	}
	fields.PendingPlan = sql.NullString{String: plan, Valid: true}
	fields.PendingInterval = sql.NullString{String: interval, Valid: true}
	fields.PendingChangeAt = sql.NullTime{Time: at, Valid: true}
	return nil
}

func pendingScheduleAt(schedule *stripe.SubscriptionSchedule, now time.Time) (time.Time, bool) {
	if schedule.Status != stripe.SubscriptionScheduleStatusActive || schedule.Metadata[planChangeMarker] != "v1" {
		return time.Time{}, false
	}
	at, valid := parsePendingAt(schedule.Metadata[planChangeAt])
	if !valid || !at.After(now) || schedule.CurrentPhase == nil || schedule.CurrentPhase.EndDate != at.Unix() {
		return time.Time{}, false
	}
	return at, true
}

func parsePendingAt(raw string) (time.Time, bool) {
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}

// releaseAppSchedule is called only while the subscription advisory lock is
// held. It never touches a schedule that another actor owns.
func (s *Service) releaseAppSchedule(ctx context.Context, sub *stripe.Subscription) (*stripe.SubscriptionSchedule, error) {
	if sub.Schedule == nil {
		return nil, nil
	}
	schedule, err := s.Stripe.V1SubscriptionSchedules.Retrieve(ctx, sub.Schedule.ID, nil)
	if err != nil {
		return nil, err
	}
	if schedule.Status != stripe.SubscriptionScheduleStatusActive {
		return nil, nil
	}
	if schedule.Metadata[planChangeMarker] != "v1" {
		return nil, errForeignSchedule
	}
	_, err = s.Stripe.V1SubscriptionSchedules.Release(ctx, schedule.ID, &stripe.SubscriptionScheduleReleaseParams{PreserveCancelDate: stripe.Bool(true)})
	if err != nil {
		return nil, err
	}
	return schedule, nil
}

func (s *Service) restoreBookedChange(ctx context.Context, subID string, schedule *stripe.SubscriptionSchedule) error {
	// Compensation must survive a client disconnect after Stripe released the
	// schedule. The caller still holds the per-subscription advisory lock.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	plan, interval := schedule.Metadata[planChangePlan], schedule.Metadata[planChangeInterval]
	if _, known := findOffer(plan, interval); !known {
		return errUnchangeable
	}
	params := &stripe.SubscriptionRetrieveParams{}
	params.AddExpand("items.data.price")
	params.AddExpand("discounts")
	sub, err := s.Stripe.V1Subscriptions.Retrieve(ctx, subID, params)
	if err != nil {
		return err
	}
	fields, err := classify(sub)
	if err != nil || fields.Malformed != "" {
		return errUnchangeable
	}
	price, err := s.resolvePrice(ctx, plan, interval)
	if err != nil {
		return err
	}
	return s.bookPlanChange(ctx, sub, price.ID, planChangeRequest{Plan: plan, Interval: interval}, fields.PeriodEnd)
}

// KeepCurrentPlan withdraws this app's booked next-period change.
func (s *Service) KeepCurrentPlan(c *gin.Context) {
	conn, subID, customer, ok := s.planChangeAccount(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	defer func() {
		if conn != nil {
			s.unlockSubscription(ctx, conn, subID)
		}
	}()
	sub, err := s.Stripe.V1Subscriptions.Retrieve(ctx, subID, nil)
	if err != nil {
		s.planChangeFailure(c, err)
		return
	}
	if sub.Customer == nil || sub.Customer.ID != customer {
		abortWebhook(c, http.StatusConflict, "plan_change_conflict")
		return
	}
	if sub.Metadata[planChangeMixedAt] != "" {
		if err := s.clearMixedPlanMarker(ctx, subID); err != nil {
			s.planChangeFailure(c, err)
			return
		}
	}
	released, err := s.releaseAppSchedule(ctx, sub)
	if errors.Is(err, errForeignSchedule) || released == nil && err == nil {
		abortWebhook(c, http.StatusConflict, "plan_change_conflict")
		return
	}
	if err != nil {
		s.planChangeFailure(c, err)
		return
	}
	s.unlockSubscription(ctx, conn, subID)
	conn = nil
	s.reconcilePlanChange(ctx, subID)
	c.JSON(http.StatusOK, gin.H{"effective": "kept"})
}

// CancelSubscription commits a period-end cancellation in-app while an app
// schedule is attached, including after its booked phase has started. Opening
// a Portal page alone never withdraws the booked change.
func (s *Service) CancelSubscription(c *gin.Context) {
	conn, subID, customer, ok := s.planChangeAccount(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	defer func() {
		if conn != nil {
			s.unlockSubscription(ctx, conn, subID)
		}
	}()
	sub, err := s.Stripe.V1Subscriptions.Retrieve(ctx, subID, nil)
	if err != nil {
		s.planChangeFailure(c, err)
		return
	}
	if sub.Customer == nil || sub.Customer.ID != customer || sub.Status != stripe.SubscriptionStatusActive || sub.CancelAtPeriodEnd || sub.CancelAt != 0 {
		abortWebhook(c, http.StatusConflict, "plan_change_conflict")
		return
	}
	if sub.Metadata[planChangeMixedAt] != "" {
		if err := s.clearMixedPlanMarker(ctx, subID); err != nil {
			s.planChangeFailure(c, err)
			return
		}
	}
	previous, err := s.releaseAppSchedule(ctx, sub)
	if err != nil {
		if errors.Is(err, errForeignSchedule) {
			abortWebhook(c, http.StatusConflict, "plan_change_conflict")
		} else {
			s.planChangeFailure(c, err)
		}
		return
	}
	_, err = s.Stripe.V1Subscriptions.Update(ctx, subID, &stripe.SubscriptionUpdateParams{
		CancelAtPeriodEnd: stripe.Bool(true),
		ProrationBehavior: stripe.String("none"),
	})
	if err != nil {
		if previous != nil {
			if restoreErr := s.restoreBookedChange(ctx, subID, previous); restoreErr != nil {
				s.Log.Error("billing: could not restore booked change after cancel failure", logKeyErrorClass, errorClass(restoreErr), logKeyStage, "restore_cancel_booking")
			}
			s.unlockSubscription(ctx, conn, subID)
			conn = nil
			reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			s.reconcilePlanChange(reconcileCtx, subID)
			cancel()
		}
		s.planChangeFailure(c, err)
		return
	}
	s.unlockSubscription(ctx, conn, subID)
	conn = nil
	s.reconcilePlanChange(ctx, subID)
	c.JSON(http.StatusOK, gin.H{"canceling": true})
}
