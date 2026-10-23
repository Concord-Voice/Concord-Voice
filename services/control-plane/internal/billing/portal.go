package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
)

// Customer Portal session (#2033 spec §8 Portal, plan Task 6).
//
// The endpoint mints a short-lived hosted Portal URL for the caller's own
// Stripe Customer. The caller never names the Customer, the configuration or
// the return URL: the Customer comes from the stripe_customers mapping, the
// configuration is the one Concord's bootstrap marked, and the return URL is a
// code constant. The only input is an optional deep-link flow. A visit never
// creates a Customer: a user with no mapping has nothing to manage (404).

const (
	// portalMaxBody caps the request body. The only field is a short flow name.
	portalMaxBody = 4 << 10

	// portalConfigTTL is how long a resolved configuration id is trusted
	// before the active configurations are listed again.
	portalConfigTTL = 10 * time.Minute

	// portalListLimit is the page size of the configuration listing.
	portalListLimit = 100

	// Response error classes.
	portalErrNoAccount   = "no_billing_account"
	portalErrUnavailable = "billing_unavailable"
	portalErrInvalid     = "invalid_request"
	portalErrTooLarge    = "payload_too_large"
	portalErrUnauth      = "unauthorized"

	// portalMaxParamLog bounds a Stripe-supplied param name in a log line.
	portalMaxParamLog = 128

	// stripeParamCustomer is the Stripe error param naming the Customer.
	stripeParamCustomer = "customer"
	// stripeParamConfiguration is the Stripe error param naming the Portal configuration.
	stripeParamConfiguration = "configuration"
)

const (
	portalMappingSQL = `SELECT stripe_customer_id FROM stripe_customers WHERE user_id = $1`

	// portalCancelableSQL picks the live Stripe subscription of the caller on
	// the mapped Customer with the latest period end. Stripe refuses a
	// cancellation flow for a subscription of another Customer, so the
	// Customer is part of the match. The predicate is concatenated as a
	// constant.
	portalCancelableSQL = `SELECT stripe_subscription_id FROM subscriptions
		WHERE user_id = $1 AND source = 'stripe' AND stripe_customer_id = $2
		AND stripe_subscription_id IS NOT NULL AND ` + entitlements.LiveRowPredicate + `
		ORDER BY current_period_end DESC, id
		LIMIT 1`

	// portalDropStaleSQL deletes the stale mapping by Customer id only, so a
	// mapping the user has since re-pointed is never removed.
	portalDropStaleSQL = `DELETE FROM stripe_customers WHERE stripe_customer_id = $1`
)

// errNoLiveSubscription means the caller holds no live Stripe subscription on
// the mapped Customer.
var errNoLiveSubscription = errors.New("billing: portal: no live stripe subscription")

// errNoPortalConfiguration means no active configuration carries Concord's
// marker: the operator has not bootstrapped this Stripe account.
var errNoPortalConfiguration = errors.New("billing: portal: no active concord configuration")

// portalRequest is the request body. Flow is optional; empty means no deep link.
type portalRequest struct {
	Flow string `json:"flow"`
}

// portalConfigCache remembers the resolved Portal configuration id. Only a
// successful lookup is stored, so a failed one is retried by the next visit.
type portalConfigCache struct {
	mu      sync.Mutex
	id      string
	expires time.Time
}

func (p *portalConfigCache) get(now time.Time) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.id == "" || !now.Before(p.expires) {
		return "", false
	}
	return p.id, true
}

func (p *portalConfigCache) set(id string, expires time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.id, p.expires = id, expires
}

func (p *portalConfigCache) clear() {
	p.set("", time.Time{})
}

// PortalSession is the Gin handler for POST /api/v1/billing/portal-session.
func (s *Service) PortalSession(c *gin.Context) {
	req, ok := bindStrictJSON(c, portalMaxBody, func(r portalRequest) bool { return validPortalFlow(r.Flow) })
	if !ok {
		return
	}
	userID, err := uuid.Parse(c.GetString("user_id"))
	if err != nil {
		s.Log.Error("billing: portal request has no usable authenticated user id")
		abortWebhook(c, http.StatusUnauthorized, portalErrUnauth)
		return
	}
	ctx := c.Request.Context()

	customer, ok := s.portalCustomer(c, userID)
	if !ok {
		return
	}
	params, ok := s.portalParams(c, userID, customer, req.Flow)
	if !ok {
		return
	}
	configID, err := s.portalConfigID(ctx)
	if err != nil {
		s.logPortalConfigFailure(err)
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
		return
	}
	params.Configuration = stripe.String(configID)

	session, err := s.Stripe.V1BillingPortalSessions.Create(ctx, params)
	if err != nil {
		s.portalCreateFailed(c, customer, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"url": session.URL})
}

// bindStrictJSON applies the byte cap and the strict full-document read this
// repo requires of every JSON handler (compare dm's expiration request):
// MaxBytesReader alone is not enough, because the decoder may stop after the
// first value without reading the rest, so the cached body is re-checked with
// json.Valid. 413 is decided before 400. valid checks the decoded fields; a
// false is the same 400. The Portal and Checkout share it.
func bindStrictJSON[T any](c *gin.Context, limit int64, valid func(T) bool) (T, bool) {
	var req, zero T
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			abortWebhook(c, http.StatusRequestEntityTooLarge, portalErrTooLarge)
			return zero, false
		}
		abortWebhook(c, http.StatusBadRequest, portalErrInvalid)
		return zero, false
	}
	body, _ := c.Get(gin.BodyBytesKey)
	raw, isBytes := body.([]byte)
	if !isBytes || !json.Valid(raw) || !valid(req) {
		abortWebhook(c, http.StatusBadRequest, portalErrInvalid)
		return zero, false
	}
	return req, true
}

// validPortalFlow reports whether flow is empty or one Concord offers.
func validPortalFlow(flow string) bool {
	switch flow {
	case "",
		string(stripe.BillingPortalSessionFlowTypePaymentMethodUpdate),
		string(stripe.BillingPortalSessionFlowTypeSubscriptionCancel):
		return true
	}
	return false
}

// portalCustomer returns the caller's mapped Stripe Customer. None is a 404
// (written here); a database failure is a 503.
func (s *Service) portalCustomer(c *gin.Context, userID uuid.UUID) (string, bool) {
	var customer string
	err := s.DB.QueryRowContext(c.Request.Context(), portalMappingSQL, userID).Scan(&customer)
	switch {
	case err == nil:
		return customer, true
	case errors.Is(err, sql.ErrNoRows):
		abortWebhook(c, http.StatusNotFound, portalErrNoAccount)
	default:
		s.Log.Error("billing: portal mapping lookup failed",
			logKeyErrorClass, errorClass(err), logKeyStage, "portal_mapping")
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
	}
	return "", false
}

// portalParams builds the session create params for the Customer and flow,
// without the configuration. A cancellation flow needs the caller's live
// subscription; without one it is a 404 (written here).
func (s *Service) portalParams(c *gin.Context, userID uuid.UUID, customer, flow string) (*stripe.BillingPortalSessionCreateParams, bool) {
	params := &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(customer),
		ReturnURL: stripe.String(ReturnURLPortal),
	}
	switch stripe.BillingPortalSessionFlowType(flow) {
	case stripe.BillingPortalSessionFlowTypePaymentMethodUpdate:
		params.FlowData = &stripe.BillingPortalSessionCreateFlowDataParams{Type: stripe.String(flow)}
	case stripe.BillingPortalSessionFlowTypeSubscriptionCancel:
		subID, err := s.cancelableSubscription(c.Request.Context(), userID, customer)
		switch {
		case errors.Is(err, errNoLiveSubscription):
			abortWebhook(c, http.StatusNotFound, portalErrNoAccount)
			return nil, false
		case err != nil:
			s.Log.Error("billing: portal subscription lookup failed",
				logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
			abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
			return nil, false
		}
		params.FlowData = &stripe.BillingPortalSessionCreateFlowDataParams{
			Type: stripe.String(flow),
			SubscriptionCancel: &stripe.BillingPortalSessionCreateFlowDataSubscriptionCancelParams{
				Subscription: stripe.String(subID),
			},
		}
	}
	return params, true
}

// cancelableSubscription returns the caller's live Stripe subscription on the
// Customer with the latest period end.
func (s *Service) cancelableSubscription(ctx context.Context, userID uuid.UUID, customer string) (string, error) {
	var subID string
	err := s.DB.QueryRowContext(ctx, portalCancelableSQL, userID, customer).Scan(&subID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNoLiveSubscription
	}
	if err != nil {
		return "", withStage("portal_live_subscription", fmt.Errorf("billing: portal: read live subscription: %w", err))
	}
	return subID, nil
}

// portalConfigID returns the id of Concord's active Portal configuration,
// listing the active configurations only when the cached id is absent or older
// than portalConfigTTL. The lock is not held across the Stripe call, so two
// concurrent misses may both list; both store the same id.
func (s *Service) portalConfigID(ctx context.Context) (string, error) {
	if id, ok := s.portalConfig.get(s.now()); ok {
		return id, nil
	}
	params := &stripe.BillingPortalConfigurationListParams{Active: stripe.Bool(true)}
	params.Limit = stripe.Int64(portalListLimit)
	for pc, err := range s.Stripe.V1BillingPortalConfigurations.List(ctx, params).All(ctx) {
		if err != nil {
			return "", withStage("list_portal_configurations", fmt.Errorf("billing: portal: list configurations: %w", err))
		}
		if pc.Metadata[PortalMetadataKey] == PortalMetadataVersion {
			s.portalConfig.set(pc.ID, s.now().Add(portalConfigTTL))
			return pc.ID, nil
		}
	}
	return "", errNoPortalConfiguration
}

// logPortalConfigFailure logs why no configuration could be resolved. Both
// causes need an operator, so both are errors.
func (s *Service) logPortalConfigFailure(err error) {
	if errors.Is(err, errNoPortalConfiguration) {
		s.Log.Error("billing: no active Customer Portal configuration is marked for Concord; run the Stripe bootstrap",
			"marker_key", PortalMetadataKey, "marker_value", PortalMetadataVersion)
		return
	}
	s.Log.Error("billing: Customer Portal configuration lookup failed",
		logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
}

// portalCreateFailed answers a failed session create. A Customer Stripe
// reports missing goes to customerMissing; every other failure is a 503. A
// resource_missing about anything else (the cached configuration was
// archived) keeps the mapping and drops the cached configuration id.
func (s *Service) portalCreateFailed(c *gin.Context, customer string, err error) {
	const stage = "create_portal_session"
	var se *stripe.Error
	if !errors.As(err, &se) {
		s.Log.Error("billing: portal session create failed", logKeyErrorClass, errorClass(err), logKeyStage, stage)
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
		return
	}
	if isResourceMissing(err) && se.Param == stripeParamCustomer {
		s.customerMissing(c, customer)
		return
	}
	if isResourceMissing(err) && se.Param == stripeParamConfiguration {
		s.portalConfig.clear()
	}
	attrs := []any{logKeyErrorClass, errorClass(err), logKeyStage, stage}
	if se.Type == stripe.ErrorTypeInvalidRequest || isResourceMissing(err) {
		attrs = append(attrs, "param", boundString(se.Param, portalMaxParamLog))
	}
	s.Log.Error("billing: portal session create failed", attrs...)
	abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
}

// customerMissing answers a session create Stripe refused because the mapped
// Customer is missing. resource_missing alone is not proof of deletion: a key
// from another Stripe account gets the same answer, and dropping the mapping
// then would leave erasure nothing to enqueue for a Customer that is alive.
// So the mapping goes (404) only when confirmCustomerDeleted confirms it;
// otherwise the mapping stays and the answer is the 503 of a Stripe failure.
func (s *Service) customerMissing(c *gin.Context, customer string) {
	err := s.confirmCustomerDeleted(c.Request.Context(), customer)
	switch {
	case err == nil:
		s.dropStaleCustomer(c, customer)
		return
	case errors.Is(err, errCustomerUnverifiable):
		s.Log.Error("billing: Stripe reports the mapped Customer missing but cannot confirm it was deleted; mapping kept. Check that the Stripe key belongs to the account holding the Customer",
			logKeyCustomerID, customer, logKeyErrorClass, classCustomerUnverifiable, logKeyStage, errorStage(err))
	default:
		s.Log.Error("billing: confirming a missing Customer failed; mapping kept",
			logKeyCustomerID, customer, logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
	}
	abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
}

// dropStaleCustomer deletes the mapping of a Customer Stripe confirmed deleted
// and answers 404. No Customer is created: a Portal visit never creates one.
func (s *Service) dropStaleCustomer(c *gin.Context, customer string) {
	if _, err := s.DB.ExecContext(c.Request.Context(), portalDropStaleSQL, customer); err != nil {
		s.Log.Error("billing: dropping the stale Customer mapping failed",
			logKeyErrorClass, errorClass(err), logKeyStage, "drop_stale_mapping")
		abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
		return
	}
	s.Log.Warn("billing: Stripe no longer has the mapped Customer; mapping removed", logKeyCustomerID, customer)
	abortWebhook(c, http.StatusNotFound, portalErrNoAccount)
}

// boundString cuts s to at most limit bytes.
func boundString(s string, limit int) string {
	if len(s) > limit {
		return s[:limit]
	}
	return s
}
