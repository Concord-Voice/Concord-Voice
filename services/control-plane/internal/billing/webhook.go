package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v87"
	"github.com/stripe/stripe-go/v87/webhook"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// Stripe webhook (#2033 spec §7, plan Task 5).
//
// The route is unauthenticated, so the Stripe-Signature HMAC over the raw body
// is the only authentication. Every check that does not need the database runs
// first, in this order: the body is read under a size cap (413), the signature
// and timestamp are verified (400), and the event's mode must match the key's
// (400). Nothing before verification reads the body as JSON, logs it, or
// touches the database or Stripe. A verified event is only a trigger: its body
// names one object, which Reconcile re-fetches from Stripe, so a forged or
// replayed delivery could at most cause a re-fetch of real state.
//
// Retries are Stripe's: any response other than 2xx is redelivered. So a
// failure that a retry can fix (Stripe or the database down, saturation) is a
// 5xx and writes no dedupe row, and an event that no retry can make actionable
// is recorded in stripe_webhook_events and answered 200.

const (
	// webhookMaxBody caps the body read before verification. Stripe events
	// are a few KiB; the cap bounds what an unauthenticated caller can make us
	// buffer and hash.
	webhookMaxBody = 256 << 10

	// webhookTolerance is how old a signed timestamp may be before the
	// delivery is treated as a replay.
	webhookTolerance = 300 * time.Second

	// Response error classes. They name the kind of refusal only.
	webhookErrTooLarge      = "payload_too_large"
	webhookErrUnreadable    = "unreadable_body"
	webhookErrInvalidEvent  = "invalid_event"
	webhookErrModeMismatch  = "livemode_mismatch"
	webhookErrBusy          = "busy"
	webhookErrInternal      = "internal"
	webhookSignatureHeader  = "Stripe-Signature"
	webhookTypeSubscription = "customer.subscription."
	webhookTypeCheckout     = "checkout.session.completed"
	webhookTypeCustomerDel  = "customer.deleted"

	logKeyEventID   = "event_id"
	logKeyEventType = "event_type"

	// stageDedupeCheck tags a failed read of stripe_webhook_events.
	stageDedupeCheck = "dedupe_check"
)

// webhookKind is what a verified event asks the handler to reconcile.
type webhookKind int

const (
	// webhookIgnored: a type Concord does not act on, or a completed
	// checkout that created no subscription. Recorded and acknowledged.
	webhookIgnored webhookKind = iota
	// webhookSubscription: reconcile the subscription id.
	webhookSubscription
	// webhookCustomerDeleted: reconcile every row of the customer id, then
	// drop its mapping.
	webhookCustomerDeleted
)

// Webhook is the Gin handler for POST /api/v1/billing/stripe/webhook.
func (s *Service) Webhook(c *gin.Context) {
	payload, ok := s.readWebhookBody(c)
	if !ok {
		return
	}
	ev, ok := s.verifyWebhook(c, payload)
	if !ok {
		return
	}
	s.handleEvent(c, ev)
}

// readWebhookBody reads the raw body under webhookMaxBody. The signature is
// computed over these exact bytes, which is why this handler reads the body
// itself instead of binding it with ShouldBindBodyWithJSON (the documented
// exception to the strict-JSON body rule). An over-cap body is refused before
// any verification work.
func (s *Service) readWebhookBody(c *gin.Context) ([]byte, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, webhookMaxBody)
	payload, err := io.ReadAll(c.Request.Body)
	if err == nil {
		return payload, true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		s.Log.Error("billing: webhook body over the size cap; refused", "limit_bytes", webhookMaxBody)
		abortWebhook(c, http.StatusRequestEntityTooLarge, webhookErrTooLarge)
		return nil, false
	}
	s.Log.Warn("billing: webhook body could not be read; refused")
	abortWebhook(c, http.StatusBadRequest, webhookErrUnreadable)
	return nil, false
}

// verifyWebhook checks the signature and timestamp and the event's mode. Only
// the reason class is logged: never the body, the header or the secret.
func (s *Service) verifyWebhook(c *gin.Context, payload []byte) (*stripe.Event, bool) {
	// IgnoreAPIVersionMismatch: the event body is only a trigger. Reconcile
	// re-fetches the subscription through the client pinned to our API
	// version, so an event rendered at an older endpoint version is still a
	// valid trigger and must not be refused (Stripe would retry it forever).
	// The signature check is unaffected by this option.
	ev, err := webhook.ConstructEventWithOptions(payload, c.GetHeader(webhookSignatureHeader),
		s.Billing.WebhookSecret, webhook.ConstructEventOptions{
			Tolerance:                webhookTolerance,
			IgnoreAPIVersionMismatch: true,
		})
	if err == nil && ev.ID == "" {
		err = errEventWithoutID
	}
	if err != nil {
		class := verifyFailureClass(err)
		if class == verifyClassNotConfigured {
			s.Log.Error("billing: webhook secret is not configured; every delivery is refused")
		} else {
			s.Log.Warn("billing: webhook delivery failed verification; refused", logKeyReason, class)
		}
		abortWebhook(c, http.StatusBadRequest, webhookErrInvalidEvent)
		return nil, false
	}
	if ev.Livemode != (s.Billing.KeyMode == config.StripeKeyModeLive) {
		s.Log.Error("billing: webhook event mode does not match the key mode; refused",
			logKeyEventID, ev.ID, "event_livemode", ev.Livemode, "key_mode", string(s.Billing.KeyMode))
		abortWebhook(c, http.StatusBadRequest, webhookErrModeMismatch)
		return nil, false
	}
	return &ev, true
}

// errEventWithoutID refuses a verified event with no id: it could be neither
// deduplicated nor traced.
var errEventWithoutID = errors.New("billing: webhook event has no id")

const (
	verifyClassSignature     = "signature"
	verifyClassTimestamp     = "timestamp"
	verifyClassNotConfigured = "not_configured"
	verifyClassPayload       = "payload"
)

// verifyFailureClass names why ConstructEventWithOptions refused a delivery.
func verifyFailureClass(err error) string {
	switch {
	case errors.Is(err, webhook.ErrTooOld):
		return verifyClassTimestamp
	case errors.Is(err, webhook.ErrEmptySecret):
		return verifyClassNotConfigured
	case errors.Is(err, webhook.ErrNotSigned),
		errors.Is(err, webhook.ErrInvalidHeader),
		errors.Is(err, webhook.ErrNoValidSignature):
		return verifyClassSignature
	default:
		return verifyClassPayload
	}
}

// handleEvent acts on a verified event: dedupe, target, then reconcile under
// the concurrency cap.
func (s *Service) handleEvent(c *gin.Context, ev *stripe.Event) {
	ctx := c.Request.Context()
	seen, err := s.eventSeen(ctx, ev.ID)
	if err != nil {
		s.Log.Error("billing: webhook dedupe check failed; Stripe will retry",
			logKeyEventID, ev.ID, logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
		abortWebhook(c, http.StatusInternalServerError, webhookErrInternal)
		return
	}
	if seen {
		s.Log.Debug("billing: webhook event already processed", logKeyEventID, ev.ID)
		received(c)
		return
	}

	kind, id := webhookTarget(ev)
	switch {
	case kind == webhookIgnored:
		s.acknowledge(c, ev)
		return
	case id == "":
		s.Log.Error("billing: webhook event names no object id; recorded and acknowledged",
			logKeyEventID, ev.ID, logKeyEventType, string(ev.Type))
		s.acknowledge(c, ev)
		return
	}

	select {
	case s.sem <- struct{}{}:
	default:
		s.Log.Warn("billing: webhook reconcile capacity full; Stripe will retry",
			logKeyEventID, ev.ID, logKeyEventType, string(ev.Type))
		abortWebhook(c, http.StatusServiceUnavailable, webhookErrBusy)
		return
	}
	defer func() { <-s.sem }()

	s.finishEvent(c, ev, s.dispatch(ctx, kind, id, ev))
}

// dispatch runs the reconcile the event's kind calls for. Both write the dedupe
// row themselves when they succeed.
func (s *Service) dispatch(ctx context.Context, kind webhookKind, id string, ev *stripe.Event) error {
	if kind == webhookCustomerDeleted {
		return s.reconcileCustomerDeleted(ctx, id, ev.ID, string(ev.Type))
	}
	_, err := s.Reconcile(ctx, id, ev.ID, string(ev.Type))
	return err
}

// finishEvent answers a reconcile's outcome: success is 200; an unactionable
// object is recorded and acknowledged (its reason was logged where it was
// found); anything else is a 500 with no dedupe row, so Stripe retries.
func (s *Service) finishEvent(c *gin.Context, ev *stripe.Event, err error) {
	switch {
	case err == nil:
		received(c)
	case errors.Is(err, ErrUnactionable):
		s.Log.Info("billing: webhook event is unactionable; recorded and acknowledged",
			logKeyEventID, ev.ID, logKeyEventType, string(ev.Type))
		s.acknowledge(c, ev)
	default:
		s.Log.Error("billing: webhook reconcile failed; Stripe will retry",
			logKeyEventID, ev.ID, logKeyEventType, string(ev.Type),
			logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
		abortWebhook(c, http.StatusInternalServerError, webhookErrInternal)
	}
}

// acknowledge records the event as processed and answers 200. If the record
// cannot be written the answer is 500, and Stripe's retry finds the same
// outcome.
func (s *Service) acknowledge(c *gin.Context, ev *stripe.Event) {
	if err := insertDedupe(c.Request.Context(), s.DB, ev.ID, string(ev.Type)); err != nil {
		s.Log.Error("billing: webhook dedupe write failed; Stripe will retry",
			logKeyEventID, ev.ID, logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
		abortWebhook(c, http.StatusInternalServerError, webhookErrInternal)
		return
	}
	received(c)
}

const eventSeenSQL = `SELECT EXISTS (SELECT 1 FROM stripe_webhook_events WHERE event_id = $1)`

// eventSeen reports whether eventID was already processed.
func (s *Service) eventSeen(ctx context.Context, eventID string) (bool, error) {
	var seen bool
	if err := s.DB.QueryRowContext(ctx, eventSeenSQL, eventID).Scan(&seen); err != nil {
		return false, withStage(stageDedupeCheck, fmt.Errorf("billing: webhook dedupe check: %w", err))
	}
	return seen, nil
}

// webhookTarget names the object a verified event asks to reconcile. A
// subscribed type whose object id is missing or not a string returns its kind
// with an empty id; the handler records and acknowledges it, since no retry
// can supply the id.
func webhookTarget(ev *stripe.Event) (kind webhookKind, id string) {
	var obj map[string]any
	if ev.Data != nil {
		obj = ev.Data.Object
	}
	eventType := string(ev.Type)
	switch {
	case strings.HasPrefix(eventType, webhookTypeSubscription):
		return webhookSubscription, stringField(obj, "id")
	case eventType == webhookTypeCheckout:
		// A session in payment or setup mode, or one whose subscription is
		// null, created no subscription: nothing to reconcile.
		subID := stringField(obj, "subscription")
		if subID == "" {
			return webhookIgnored, ""
		}
		return webhookSubscription, subID
	case eventType == webhookTypeCustomerDel:
		return webhookCustomerDeleted, stringField(obj, "id")
	default:
		return webhookIgnored, ""
	}
}

// stringField returns obj[key] when it is a string, else "".
func stringField(obj map[string]any, key string) string {
	v, _ := obj[key].(string)
	return v
}

// received is the one success response.
func received(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"received": true})
}

// abortWebhook writes a refusal that carries only its class: never the body,
// the signature, the secret or the event id.
func abortWebhook(c *gin.Context, status int, class string) {
	c.AbortWithStatusJSON(status, gin.H{"error": class})
}
