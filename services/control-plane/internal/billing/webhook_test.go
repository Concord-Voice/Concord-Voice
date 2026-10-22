package billing

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"
	"github.com/stripe/stripe-go/v87/webhook"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Webhook handler unit tests (#2033 PR-2, Task 5). Every case here is decided
// before the handler reaches the database, so the Service has a nil DB: a test
// that touched it would panic rather than pass quietly. Cases that need the
// dedupe table or Reconcile live in webhook_integration_test.go.

const (
	webhookPath       = "/webhooks/stripe"
	testWebhookSecret = "whsec_fake"  // pragma: allowlist secret -- the plan's fake test secret
	otherWebhookKey   = "whsec_other" // pragma: allowlist secret -- a second fake test secret
)

// unitWebhookService is a Service with no database and no Stripe client.
func unitWebhookService(mode config.StripeKeyMode) *Service {
	return NewService(Deps{
		Billing:     config.BillingConfig{KeyMode: mode, WebhookSecret: testWebhookSecret},
		Environment: testEnvironment,
		Log:         logger.NewWithWriter(io.Discard),
	})
}

// signHeader returns a Stripe-Signature header for body, made with secret at ts.
func signHeader(body []byte, secret string, ts time.Time) string {
	return webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: body, Secret: secret, Timestamp: ts,
	}).Header
}

// eventBody builds a realistic Stripe event whose data.object is obj.
func eventBody(t *testing.T, id, eventType string, livemode bool, apiVersion string, obj map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id":               id,
		"object":           "event",
		"api_version":      apiVersion,
		"created":          time.Now().Unix(),
		"type":             eventType,
		"livemode":         livemode,
		"pending_webhooks": 1,
		"request":          map[string]any{"id": nil, "idempotency_key": nil},
		"data":             map[string]any{"object": obj},
	})
	require.NoError(t, err)
	return body
}

// subscriptionObject is the minimal data.object of a customer.subscription.* event.
func subscriptionObject(subID, customerID string) map[string]any {
	return map[string]any{"id": subID, "object": "subscription", "customer": customerID, "status": "active"}
}

// deliver posts body with the Stripe-Signature header (omitted when empty) to
// svc.Webhook through a Gin test context and returns the recorder.
func deliver(svc *Service, body []byte, header string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body))
	if header != "" {
		c.Request.Header.Set("Stripe-Signature", header)
	}
	svc.Webhook(c)
	return w
}

// errorClassOf parses a non-200 response body and returns its error class.
func errorClassOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var got struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), "error responses are JSON: %q", w.Body.String())
	return got.Error
}

func TestWebhookConstants(t *testing.T) {
	assert.Equal(t, 256<<10, webhookMaxBody)
	assert.Equal(t, 300*time.Second, webhookTolerance)
	assert.Equal(t, 4, maxConcurrentReconciles)
	assert.Equal(t, maxConcurrentReconciles, cap(NewService(Deps{}).sem), "the semaphore is sized by the constant")
}

func TestWebhook_OverCapIs413BeforeVerification(t *testing.T) {
	svc := unitWebhookService(config.StripeKeyModeTest)
	body := bytes.Repeat([]byte("a"), webhookMaxBody+1)

	w := deliver(svc, body, "t=1,v1=00")

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code,
		"size is checked before the signature, so an unauthenticated flood never costs an HMAC")
	assert.Equal(t, webhookErrTooLarge, errorClassOf(t, w))
}

func TestWebhook_AtCapIsNotOverCap(t *testing.T) {
	svc := unitWebhookService(config.StripeKeyModeTest)
	body := bytes.Repeat([]byte("a"), webhookMaxBody)

	w := deliver(svc, body, "t=1,v1=00")

	assert.Equal(t, http.StatusBadRequest, w.Code, "a body of exactly the cap is read and then fails verification")
	assert.Equal(t, webhookErrInvalidEvent, errorClassOf(t, w))
}

func TestWebhook_BadSignature400(t *testing.T) {
	svc := unitWebhookService(config.StripeKeyModeTest)
	body := eventBody(t, "evt_sig", "customer.subscription.updated", false, stripe.APIVersion, subscriptionObject("sub_x", "cus_x"))

	cases := []struct {
		name   string
		header string
	}{
		{"missing header", ""},
		{"garbage header", "not-a-signature"},
		{"signed with another secret", signHeader(body, otherWebhookKey, time.Now())},
		{"signature of a different body", signHeader([]byte("{}"), testWebhookSecret, time.Now())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := deliver(svc, body, tc.header)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Equal(t, webhookErrInvalidEvent, errorClassOf(t, w))
			assert.NotContains(t, w.Body.String(), testWebhookSecret)
			assert.NotContains(t, w.Body.String(), "evt_sig", "the response never echoes the event")
		})
	}
}

func TestWebhook_StaleTimestamp400(t *testing.T) {
	svc := unitWebhookService(config.StripeKeyModeTest)
	body := eventBody(t, "evt_old", "customer.subscription.updated", false, stripe.APIVersion, subscriptionObject("sub_x", "cus_x"))

	w := deliver(svc, body, signHeader(body, testWebhookSecret, time.Now().Add(-(webhookTolerance+time.Second))))

	assert.Equal(t, http.StatusBadRequest, w.Code, "a validly signed delivery older than the tolerance is a replay")
	assert.Equal(t, webhookErrInvalidEvent, errorClassOf(t, w))
}

func TestWebhook_UnparseablePayload400(t *testing.T) {
	svc := unitWebhookService(config.StripeKeyModeTest)
	body := []byte("this is not json")

	w := deliver(svc, body, signHeader(body, testWebhookSecret, time.Now()))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, webhookErrInvalidEvent, errorClassOf(t, w))
}

func TestWebhook_LivemodeMismatch400(t *testing.T) {
	cases := []struct {
		name     string
		mode     config.StripeKeyMode
		livemode bool
	}{
		{"test-mode key, live event", config.StripeKeyModeTest, true},
		{"live-mode key, test event", config.StripeKeyModeLive, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := unitWebhookService(tc.mode)
			body := eventBody(t, "evt_mode", "customer.subscription.updated", tc.livemode, stripe.APIVersion,
				subscriptionObject("sub_x", "cus_x"))

			w := deliver(svc, body, signHeader(body, testWebhookSecret, time.Now()))

			assert.Equal(t, http.StatusBadRequest, w.Code,
				"a correctly signed event for the other mode is refused before the database is touched")
			assert.Equal(t, webhookErrModeMismatch, errorClassOf(t, w))
		})
	}
}

// RF2's building block: the header carries both signatures during Stripe's
// dual-secret window. Built here so the integration test can reuse it.
func rotationHeader(body []byte, ts time.Time, secrets ...string) string {
	parts := make([]string, 0, 1+len(secrets))
	parts = append(parts, fmt.Sprintf("t=%d", ts.Unix()))
	for _, secret := range secrets { // pragma: allowlist secret -- fake test secrets
		sig := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret, Timestamp: ts}) // pragma: allowlist secret -- fake test secrets
		parts = append(parts, "v1="+hex.EncodeToString(sig.Signature))
	}
	return strings.Join(parts, ",")
}

func TestWebhook_RotationHeaderWithNoValidSignature400(t *testing.T) {
	svc := unitWebhookService(config.StripeKeyModeTest)
	body := eventBody(t, "evt_rot0", "customer.subscription.updated", false, stripe.APIVersion, subscriptionObject("sub_x", "cus_x"))

	w := deliver(svc, body, rotationHeader(body, time.Now(), otherWebhookKey, "whsec_third"))

	assert.Equal(t, http.StatusBadRequest, w.Code, "two v1 signatures are not a bypass: one must match our secret")
	assert.Equal(t, webhookErrInvalidEvent, errorClassOf(t, w))
}

func TestWebhookTarget(t *testing.T) {
	event := func(eventType string, obj map[string]any) *stripe.Event {
		return &stripe.Event{ID: "evt_t", Type: stripe.EventType(eventType), Data: &stripe.EventData{Object: obj}}
	}
	cases := []struct {
		name     string
		ev       *stripe.Event
		wantKind webhookKind
		wantID   string
	}{
		{"subscription created", event("customer.subscription.created", map[string]any{"id": "sub_1"}), webhookSubscription, "sub_1"},
		{"subscription updated", event("customer.subscription.updated", map[string]any{"id": "sub_2"}), webhookSubscription, "sub_2"},
		{"subscription deleted", event("customer.subscription.deleted", map[string]any{"id": "sub_3"}), webhookSubscription, "sub_3"},
		{"subscription paused", event("customer.subscription.paused", map[string]any{"id": "sub_4"}), webhookSubscription, "sub_4"},
		{"subscription trial will end", event("customer.subscription.trial_will_end", map[string]any{"id": "sub_5"}), webhookSubscription, "sub_5"},
		{"checkout completed carries its subscription", event("checkout.session.completed", map[string]any{"id": "cs_1", "subscription": "sub_6"}), webhookSubscription, "sub_6"},
		{"customer deleted", event("customer.deleted", map[string]any{"id": "cus_1"}), webhookCustomerDeleted, "cus_1"},
		{"invoice paid is not subscribed", event("invoice.paid", map[string]any{"id": "in_1", "subscription": "sub_7"}), webhookIgnored, ""},
		{"customer updated is not subscribed", event("customer.updated", map[string]any{"id": "cus_2"}), webhookIgnored, ""},
		{"checkout with a null subscription", event("checkout.session.completed", map[string]any{"id": "cs_2", "subscription": nil}), webhookIgnored, ""},
		{"checkout with a non-string subscription", event("checkout.session.completed", map[string]any{"id": "cs_3", "subscription": 42}), webhookIgnored, ""},
		{"checkout with no subscription key", event("checkout.session.completed", map[string]any{"id": "cs_4"}), webhookIgnored, ""},
		{"subscription event with no id", event("customer.subscription.updated", map[string]any{"object": "subscription"}), webhookSubscription, ""},
		{"subscription event with a non-string id", event("customer.subscription.updated", map[string]any{"id": 7}), webhookSubscription, ""},
		{"customer deleted with no id", event("customer.deleted", map[string]any{}), webhookCustomerDeleted, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, id := webhookTarget(tc.ev)
			assert.Equal(t, tc.wantKind, kind)
			assert.Equal(t, tc.wantID, id, "an empty id with a non-ignored kind means a malformed object: dedupe, error log, 200")
		})
	}
}

// closedDB is a *sql.DB whose every call fails, for the database-error paths.
func closedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", "postgres://closed.invalid/none?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return db
}

// failingBody is a request body whose read fails partway, as a dropped
// connection does.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (failingBody) Close() error             { return nil }

func TestWebhook_UnreadableBody400(t *testing.T) {
	svc := unitWebhookService(config.StripeKeyModeTest)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, webhookPath, failingBody{})

	svc.Webhook(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, webhookErrUnreadable, errorClassOf(t, w))
}

// An unset secret must never verify anything: HMAC accepts a zero-length key,
// so a signature forged with "" would otherwise pass.
func TestWebhook_SecretUnsetRefusesEverything400(t *testing.T) {
	var logs bytes.Buffer
	svc := NewService(Deps{
		Billing: config.BillingConfig{KeyMode: config.StripeKeyModeTest},
		Log:     logger.NewWithWriter(&logs),
	})
	body := eventBody(t, "evt_nosecret", "customer.subscription.updated", false, stripe.APIVersion,
		subscriptionObject("sub_x", "cus_x"))

	w := deliver(svc, body, signHeader(body, "", time.Now()))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, webhookErrInvalidEvent, errorClassOf(t, w))
	assert.Contains(t, logs.String(), "level=ERROR", "a missing secret is a configuration fault, not caller noise")
	assert.Contains(t, logs.String(), "billing: webhook secret is not configured; every delivery is refused")
	assert.NotContains(t, w.Body.String(), "secret", "the response does not reveal the misconfiguration")
}

func TestWebhook_EventWithoutID400(t *testing.T) {
	svc := unitWebhookService(config.StripeKeyModeTest)
	body := eventBody(t, "", "customer.subscription.updated", false, stripe.APIVersion,
		subscriptionObject("sub_x", "cus_x"))

	w := deliver(svc, body, signHeader(body, testWebhookSecret, time.Now()))

	assert.Equal(t, http.StatusBadRequest, w.Code, "an event with no id can be neither deduplicated nor traced")
	assert.Equal(t, webhookErrInvalidEvent, errorClassOf(t, w))
}

func TestWebhook_DedupeReadFailure500(t *testing.T) {
	var logs bytes.Buffer
	svc := unitWebhookService(config.StripeKeyModeTest)
	svc.Log = logger.NewWithWriter(&logs)
	svc.DB = closedDB(t)
	body := eventBody(t, "evt_dbdown", "customer.subscription.updated", false, stripe.APIVersion,
		subscriptionObject("sub_x", "cus_x"))

	w := deliver(svc, body, signHeader(body, testWebhookSecret, time.Now()))

	assert.Equal(t, http.StatusInternalServerError, w.Code, "a database fault is retried by Stripe")
	assert.Equal(t, webhookErrInternal, errorClassOf(t, w))
	assert.NotContains(t, w.Body.String(), "evt_dbdown")
	assert.Empty(t, svc.sem, "no token is taken before the dedupe check")
	assert.Contains(t, logs.String(), "billing: webhook dedupe check failed; Stripe will retry")
	assert.Contains(t, logs.String(), "stage="+stageDedupeCheck)
}

func TestWebhook_AcknowledgeWriteFailure500(t *testing.T) {
	var logs bytes.Buffer
	svc := unitWebhookService(config.StripeKeyModeTest)
	svc.Log = logger.NewWithWriter(&logs)
	svc.DB = closedDB(t)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, webhookPath, nil)

	svc.acknowledge(c, &stripe.Event{ID: "evt_ackfail", Type: "invoice.paid"})

	assert.Equal(t, http.StatusInternalServerError, w.Code, "an unrecorded event must be redelivered")
	assert.Equal(t, webhookErrInternal, errorClassOf(t, w))
	assert.Contains(t, logs.String(), "billing: webhook dedupe write failed; Stripe will retry")
	assert.Contains(t, logs.String(), "stage="+stageDedupeInsert)
}

func TestVerifyFailureClass(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{webhook.ErrTooOld, verifyClassTimestamp},
		{webhook.ErrEmptySecret, verifyClassNotConfigured},
		{webhook.ErrNotSigned, verifyClassSignature},
		{webhook.ErrInvalidHeader, verifyClassSignature},
		{webhook.ErrNoValidSignature, verifyClassSignature},
		{errEventWithoutID, verifyClassPayload},
		{errors.New("failed to parse webhook body json"), verifyClassPayload},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, verifyFailureClass(tc.err), "%v", tc.err)
	}
}
