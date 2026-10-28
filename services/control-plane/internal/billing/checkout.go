package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
)

// Checkout session (#2033 spec §8 Checkout, plan Task 5). The caller names a
// plan and interval; everything else (price, Customer, discount, URLs, expiry)
// is decided here. The steps run in the spec's order.

const (
	checkoutMaxBody = 4 << 10

	// checkoutSessionTTL is how long a session stays open (F15: Stripe's
	// minimum is 30 minutes after its own creation time, so 35 leaves margin
	// for clock skew between us and Stripe).
	checkoutSessionTTL = 35 * time.Minute

	checkoutErrPurchase = "purchase_unavailable"
	checkoutErrManage   = "manage_existing"

	// stripeMsgNoSuchCustomer starts Stripe's message for an unknown Customer.
	stripeMsgNoSuchCustomer = "No such customer"

	// Historical Stripe rows survive cancellation and expiry. A prior paid
	// subscription also makes the account ineligible for its first trial.
	// Only Supersonic carries the trial (D6, Mark 2026-10-24); Wing starts paid.
	checkoutStripeHistorySQL = `SELECT
		EXISTS (SELECT 1 FROM subscriptions WHERE user_id = $1 AND source = 'stripe' AND ` + entitlements.LiveRowPredicate + `),
		EXISTS (SELECT 1 FROM subscriptions WHERE user_id = $1 AND source = 'stripe')`
	checkoutTrialDays = 30

	// checkoutLockPrefix keys the per-Customer Checkout lock.
	checkoutLockPrefix = "checkout:"
)

// checkoutSlots caps how many Checkouts hold a lock connection at once. The
// handler queries through the pool while it holds one, so without the cap
// enough concurrent Checkouts would take every pooled connection and wait on
// each other. Kept well under the pool's 25 (database.Connect).
// ponytail: process-wide cap; per-Service if a second pool ever appears.
var (
	checkoutSlots    = make(chan struct{}, 8)
	checkoutSlotWait = 5 * time.Second
)

// acquireCheckoutSlot waits up to checkoutSlotWait for a free slot and
// returns its release, or false when none freed in time.
func acquireCheckoutSlot(ctx context.Context) (func(), bool) {
	t := time.NewTimer(checkoutSlotWait)
	defer t.Stop()
	select {
	case checkoutSlots <- struct{}{}:
		return func() { <-checkoutSlots }, true
	case <-ctx.Done():
	case <-t.C:
	}
	return nil, false
}

// Checkout return URLs: code constants, never request values (spec §8 step 7).
// The launcher intercepts them before they load.
const (
	ReturnURLCheckoutSuccess = "https://concordvoice.com/billing/return?result=success"
	ReturnURLCheckoutCancel  = "https://concordvoice.com/billing/return?result=cancel"
)

type checkoutRequest struct {
	Plan     string `json:"plan"`
	Interval string `json:"interval"`
}

// CheckoutSession is the Gin handler for POST /api/v1/billing/checkout-session.
func (s *Service) CheckoutSession(c *gin.Context) {
	req, ok := bindStrictJSON(c, checkoutMaxBody, func(r checkoutRequest) bool {
		_, known := findOffer(r.Plan, r.Interval)
		return known
	})
	if !ok {
		return
	}
	userID, err := uuid.Parse(c.GetString("user_id"))
	if err != nil {
		s.Log.Error("billing: checkout request has no usable authenticated user id")
		abortWebhook(c, http.StatusUnauthorized, portalErrUnauth)
		return
	}
	if !s.purchaseAllowed(userID) {
		abortWebhook(c, http.StatusServiceUnavailable, checkoutErrPurchase)
		return
	}
	ctx := c.Request.Context()
	price, err := s.resolvePrice(ctx, req.Plan, req.Interval)
	if err != nil {
		s.logCatalogFailure(err)
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
		return
	}
	// Uncached, like the price: the coupon is what the session applies.
	if s.foundersActive() {
		if err := s.verifyFoundersCoupon(ctx); err != nil {
			s.logCatalogFailure(err)
			abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
			return
		}
	}
	eligible, ok := s.checkoutPreconditions(c, userID)
	if !ok {
		return
	}
	cus, err := s.ensureCustomer(ctx, userID)
	if err != nil {
		s.logCheckoutFailure(err, "")
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
		return
	}
	// One Checkout at a time per Customer (S9, R43): without the lock two
	// concurrent requests each expire the other's not-yet-created session and
	// both complete. The key shares Reconcile's lock domain; a "checkout:"
	// prefix never equals a subscription id.
	release, ok := acquireCheckoutSlot(ctx)
	if !ok {
		s.Log.Warn("billing: no free Checkout slot", logKeyStage, "checkout_slot")
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
		return
	}
	defer release()
	conn, err := s.lockSubscription(ctx, checkoutLockPrefix+cus)
	if err != nil {
		s.logCheckoutFailure(err, cus)
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
		return
	}
	defer s.unlockSubscription(ctx, conn, checkoutLockPrefix+cus)
	// Stripe is the authority: a completed Checkout whose webhook has not
	// landed yet is not in the database, so the advisory 409 above misses it.
	live, err := s.customerHasLiveSubscription(ctx, cus)
	if err != nil {
		s.logCheckoutFailure(err, cus)
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
		return
	}
	if live {
		abortWebhook(c, http.StatusConflict, checkoutErrManage)
		return
	}
	session, err := s.openSession(ctx, userID, cus, price, req.Plan, eligible)
	if err != nil {
		s.logCheckoutFailure(err, cus)
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"url":        session.URL,
		"expires_at": time.Unix(session.ExpiresAt, 0).UTC().Format(time.RFC3339),
	})
}

// checkoutPreconditions writes the advisory 409s (spec §8 step 3): a caller
// who already has a live Stripe subscription, or (PR-5) holds an active Wing
// seat that entitles. It takes no lock: Stripe's one-subscription limit and
// Reconcile (its holder_subscribed transition) are the backstops (R1, R43, F5).
func (s *Service) checkoutPreconditions(c *gin.Context, userID uuid.UUID) (bool, bool) {
	var live, prior bool
	if err := s.DB.QueryRowContext(c.Request.Context(), checkoutStripeHistorySQL, userID).Scan(&live, &prior); err != nil {
		s.Log.Error("billing: checkout Stripe history check failed",
			logKeyErrorClass, errorClass(err), logKeyStage, "checkout_history_check")
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
		return false, false
	}
	if live {
		abortWebhook(c, http.StatusConflict, checkoutErrManage)
		return false, false
	}
	seated, err := s.holdsLiveSeat(c.Request.Context(), userID)
	if err != nil {
		s.Log.Error("billing: checkout seat check failed",
			logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
		return false, false
	}
	if seated {
		abortWebhook(c, http.StatusConflict, checkoutErrSeatMember)
		return false, false
	}
	return !prior, true
}

// customerHasLiveSubscription reports whether Stripe holds any subscription of
// cus that is not ended. Stripe's default listing already omits canceled ones.
func (s *Service) customerHasLiveSubscription(ctx context.Context, cus string) (bool, error) {
	params := &stripe.SubscriptionListParams{Customer: stripe.String(cus)}
	for sub, err := range s.Stripe.V1Subscriptions.List(ctx, params).All(ctx) {
		if err != nil {
			return false, withStage("list_subscriptions", fmt.Errorf("billing: checkout: list subscriptions: %w", err))
		}
		if sub.Status != stripe.SubscriptionStatusIncompleteExpired && sub.Status != stripe.SubscriptionStatusCanceled {
			return true, nil
		}
	}
	return false, nil
}

// openSession expires the Customer's open sessions and creates the new one.
// If Stripe says the Customer is missing and a retrieve confirms it was
// deleted, the mapping is replaced and the create retried once (F18).
func (s *Service) openSession(ctx context.Context, userID uuid.UUID, cus string, price *stripe.Price, plan string, trialEligible bool) (*stripe.CheckoutSession, error) {
	session, err := s.createSession(ctx, userID, cus, price, plan, trialEligible)
	if err == nil || !isCustomerMissing(err) {
		return session, err
	}
	if err := s.confirmCustomerDeleted(ctx, cus); err != nil {
		return nil, fmt.Errorf("billing: checkout: customer %s missing: %w", cus, err)
	}
	fresh, err := s.replaceStaleCustomer(ctx, userID, cus)
	if err != nil {
		return nil, err
	}
	return s.createSession(ctx, userID, fresh, price, plan, trialEligible)
}

func (s *Service) createSession(ctx context.Context, userID uuid.UUID, cus string, price *stripe.Price, plan string, trialEligible bool) (*stripe.CheckoutSession, error) {
	// Kept: S9 (2026-10-24) showed Stripe's one-subscription limit does not
	// block concurrent completions (spec §8 step 5, R43).
	if err := s.expireOpenSessions(ctx, cus); err != nil {
		return nil, err
	}
	session, err := s.Stripe.V1CheckoutSessions.Create(ctx, s.sessionParams(userID, cus, price, plan, trialEligible))
	if err != nil {
		return nil, withStage("create_checkout_session", fmt.Errorf("billing: checkout: create session: %w", err))
	}
	// An erasure (or a self-heal) that committed after ensureCustomer has
	// dropped the mapping and queued the Customer; never hand out a session
	// for it. Expire it now rather than leave it payable until the outbox
	// worker runs.
	if !s.mappingHeld(ctx, userID, cus) {
		s.expireUnheldSession(ctx, session.ID, cus)
		return nil, withStage(stageEnsureCustomer, errMappingChanged)
	}
	return session, nil
}

// expireUnheldSession expires a session created for a Customer the caller no
// longer maps. A failure is logged with the ids: the outbox still expires the
// Customer's open sessions before deleting it.
func (s *Service) expireUnheldSession(ctx context.Context, sessionID, cus string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mapTimeout)
	defer cancel()
	if _, err := s.Stripe.V1CheckoutSessions.Expire(ctx, sessionID, nil); err != nil {
		s.Log.Error("billing: checkout could not expire a session for an unmapped Customer",
			logKeySessionID, sessionID, logKeyCustomerID, cus, logKeyErrorClass, errorClass(err))
	}
}

// sessionParams builds the session create (spec §8 step 6). It never sets
// allow_promotion_codes.
func (s *Service) sessionParams(userID uuid.UUID, cus string, price *stripe.Price, plan string, trialEligible bool) *stripe.CheckoutSessionCreateParams {
	params := &stripe.CheckoutSessionCreateParams{
		Mode:                      stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:                  stripe.String(cus),
		ClientReferenceID:         stripe.String(userID.String()),
		LineItems:                 []*stripe.CheckoutSessionCreateLineItemParams{{Price: stripe.String(price.ID), Quantity: stripe.Int64(1)}},
		AllowedPaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		PaymentMethodCollection:   stripe.String("always"),
		// USD only (decision P3-C): no converted local-currency price, whatever
		// the Dashboard default says.
		AdaptivePricing: &stripe.CheckoutSessionCreateAdaptivePricingParams{Enabled: stripe.Bool(false)},
		SubscriptionData: &stripe.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: map[string]string{metadataKeyUserID: userID.String(), metadataKeyEnv: s.Environment, PlanMetadataKey: plan},
		},
		ExpiresAt:  stripe.Int64(s.now().Add(checkoutSessionTTL).Unix()),
		SuccessURL: stripe.String(ReturnURLCheckoutSuccess),
		CancelURL:  stripe.String(ReturnURLCheckoutCancel),
	}
	if trialEligible && plan == PlanSupersonic {
		params.SubscriptionData.TrialPeriodDays = stripe.Int64(checkoutTrialDays)
	}
	if s.foundersActive() {
		params.Discounts = []*stripe.CheckoutSessionCreateDiscountParams{{Coupon: stripe.String(CouponFoundersID)}}
	}
	if s.Billing.AutomaticTax {
		params.AutomaticTax = &stripe.CheckoutSessionCreateAutomaticTaxParams{Enabled: stripe.Bool(true)}
		params.BillingAddressCollection = stripe.String(string(stripe.CheckoutSessionBillingAddressCollectionRequired))
		params.CustomerUpdate = &stripe.CheckoutSessionCreateCustomerUpdateParams{Address: stripe.String("auto")}
	}
	return params
}

// isCustomerMissing reports a resource_missing about the Customer: param
// customer, or Stripe's "No such customer" message when no param is named.
// A false positive costs one retrieve: confirmCustomerDeleted still needs
// deleted:true before the mapping is dropped.
func isCustomerMissing(err error) bool {
	var se *stripe.Error
	if !isResourceMissing(err) || !errors.As(err, &se) {
		return false
	}
	return se.Param == stripeParamCustomer || strings.HasPrefix(se.Msg, stripeMsgNoSuchCustomer)
}

// logCheckoutFailure logs why Customer set-up or the session create failed.
// cus is the mapped Customer when one was known, so an operator can look it
// up; an invalid request names the bounded param (spec §8: "error log with
// the param name"), never Stripe's message.
func (s *Service) logCheckoutFailure(err error, cus string) {
	if errors.Is(err, errUserErased) {
		if errors.Is(err, errErasureNotQueued) {
			s.Log.Warn("billing: checkout found the account erased mid-request; its new Customer could not be queued for deletion (see the error line naming it)")
			return
		}
		s.Log.Warn("billing: checkout found the account erased mid-request; its new Customer is queued for deletion")
		return
	}
	class := errorClass(err)
	if errors.Is(err, errCustomerUnverifiable) {
		class = classCustomerUnverifiable
	}
	attrs := []any{logKeyErrorClass, class, logKeyStage, errorStage(err)}
	if cus != "" {
		attrs = append(attrs, logKeyCustomerID, cus)
	}
	var se *stripe.Error
	if errors.As(err, &se) && se.Type == stripe.ErrorTypeInvalidRequest {
		attrs = append(attrs, "param", boundString(se.Param, portalMaxParamLog))
	}
	s.Log.Error("billing: checkout failed", attrs...)
}
