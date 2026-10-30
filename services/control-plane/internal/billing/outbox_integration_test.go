package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
)

// Task 8 (spec section 10, refinements R-b, R-c, R-d): the erasure outbox
// worker deletes the Stripe Customer of an erased account. These tests drive
// ProcessOutbox against PostgreSQL and the Stripe fake.

const (
	sessionsListPath = "/v1/checkout/sessions"
	customersPath    = "/v1/customers/"
	// rateLimitBody is Stripe's 429 body: type invalid_request_error, code
	// rate_limit. A retry decision keyed on the type alone would treat it as the
	// "session already completed" refusal, which is the bug this pins.
	rateLimitBody = `{"error":{"type":"invalid_request_error","code":"rate_limit","message":"Too many requests"}}`
)

func outboxCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func expirePath(sessionID string) string { return sessionsListPath + "/" + sessionID + "/expire" }

// enqueueOutbox inserts one erasure obligation due now.
func enqueueOutbox(t *testing.T, db *sql.DB, customerID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO billing_erasure_outbox (stripe_customer_id) VALUES ($1)`, customerID)
	require.NoError(t, err, "enqueue outbox row")
}

// outboxRowState is the observable state of one outbox row.
type outboxRowState struct {
	Attempts       int
	NextAttemptIn  time.Duration // next_attempt_at minus the database clock
	LastErrorClass sql.NullString
}

// loadOutboxRow reads the row for customerID; the bool is false when absent.
func loadOutboxRow(t *testing.T, db *sql.DB, customerID string) (outboxRowState, bool) {
	t.Helper()
	var st outboxRowState
	var secs float64
	err := db.QueryRow(
		`SELECT attempts, EXTRACT(EPOCH FROM (next_attempt_at - NOW())), last_error_class
		   FROM billing_erasure_outbox WHERE stripe_customer_id = $1`, customerID,
	).Scan(&st.Attempts, &secs, &st.LastErrorClass)
	if errors.Is(err, sql.ErrNoRows) {
		return st, false
	}
	require.NoError(t, err)
	st.NextAttemptIn = time.Duration(secs * float64(time.Second))
	return st, true
}

func TestOutbox_DeletesCustomerAndRow(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := outboxCtx(t)
	fake.seedCustomer("cus_ob1")
	fake.seedSession("cs_ob1", "cus_ob1", "open")
	fake.seedSession("cs_ob1_done", "cus_ob1", "complete")
	enqueueOutbox(t, env.DB, "cus_ob1")

	deleted, err := env.Svc.ProcessOutbox(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	assert.Equal(t, "expired", fake.sessionStatus("cs_ob1"), "the open session is expired first")
	assert.Equal(t, "complete", fake.sessionStatus("cs_ob1_done"), "a completed session is left alone")
	assert.False(t, fake.customerExists("cus_ob1"), "the Customer is deleted at Stripe")
	assert.Zero(t, countTable(t, env.DB, "billing_erasure_outbox"), "the row is removed once the Customer is gone")

	lists := fake.requestsTo("GET", sessionsListPath)
	require.Len(t, lists, 1)
	assert.Equal(t, "cus_ob1", lists[0].Form.Get("customer"))
	assert.Equal(t, "open", lists[0].Form.Get("status"))

	writes := fake.writes()
	order := make([]string, 0, len(writes))
	for _, w := range writes {
		order = append(order, w.Method+" "+w.Path)
	}
	assert.Equal(t, []string{"POST " + expirePath("cs_ob1"), "DELETE " + customersPath + "cus_ob1"}, order,
		"expire first, then delete the Customer")
}

// A Customer another user is still mapped to is not the erased user's to
// delete: an erased user's subscription row can sit on it, because a row keeps
// the user it was first written for. No Stripe call is made and the row is
// kept as customer_mapped, logged at error, so the obligation is not lost.
// Once no user is mapped to the Customer, the row erases it as usual.
// TestOutbox_DeletesCustomerAndRow is the unmapped control.
func TestOutbox_CustomerMappedToAnotherUserIsKept(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	fake.seedCustomer("cus_other_user")
	fake.seedSession("cs_other_user", "cus_other_user", "open")
	other := newPayer(t, env, "cus_other_user")
	enqueueOutbox(t, env.DB, "cus_other_user")

	deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
	require.NoError(t, err, "a per-row failure is recorded on the row, not returned")
	assert.Zero(t, deleted)

	st, ok := loadOutboxRow(t, env.DB, "cus_other_user")
	require.True(t, ok, "the erasure obligation is kept")
	assert.Equal(t, 1, st.Attempts)
	assert.Equal(t, "customer_mapped", st.LastErrorClass.String)
	assert.True(t, fake.customerExists("cus_other_user"), "the other user's Customer is kept")
	assert.Equal(t, "open", fake.sessionStatus("cs_other_user"), "their open session is left alone")
	assert.Empty(t, fake.writes(), "no Stripe write")
	requireLogLine(t, env, "ERROR", "billing: the Stripe customer is mapped to another user; erasure row kept for operator review",
		"stripe_customer_id=cus_other_user", "attempts=1", "error_class=customer_mapped", "stage=check_mapping")

	// The mapping goes (the other user is erased too, or an operator removes it).
	_, err = env.DB.Exec(`DELETE FROM stripe_customers WHERE user_id = $1`, other)
	require.NoError(t, err)
	_, err = env.DB.Exec(`UPDATE billing_erasure_outbox SET next_attempt_at = NOW() WHERE stripe_customer_id = 'cus_other_user'`)
	require.NoError(t, err)

	deleted, err = env.Svc.ProcessOutbox(outboxCtx(t))
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	assert.False(t, fake.customerExists("cus_other_user"), "the unmapped Customer is erased")
	_, ok = loadOutboxRow(t, env.DB, "cus_other_user")
	assert.False(t, ok, "the row is removed")
}

func TestOutbox_NothingDueIsANoOp(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)

	deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
	require.NoError(t, err)
	assert.Zero(t, deleted)
	assert.Empty(t, fake.requestsTo("GET", sessionsListPath), "an empty outbox makes no Stripe call")
}

func TestOutbox_ResourceMissingIsSuccess(t *testing.T) {
	t.Run("the customer is already gone", func(t *testing.T) {
		fake := newFakeStripe(t)
		env := newTestService(t, fake)
		// Deleted earlier: delete answers resource_missing, retrieve deleted: true.
		fake.seedDeletedCustomer("cus_gone")
		enqueueOutbox(t, env.DB, "cus_gone")

		deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
		require.NoError(t, err)
		assert.Equal(t, 1, deleted)
		assert.Zero(t, countTable(t, env.DB, "billing_erasure_outbox"))
		assert.Len(t, fake.requestsTo("DELETE", customersPath+"cus_gone"), 1)
		assert.Len(t, fake.requestsTo("GET", customersPath+"cus_gone"), 1,
			"resource_missing on the delete is confirmed by one retrieve")
	})
	t.Run("listing the sessions says resource_missing", func(t *testing.T) {
		fake := newFakeStripe(t)
		env := newTestService(t, fake)
		fake.seedCustomer("cus_list_missing")
		fake.failOn("GET", sessionsListPath, http.StatusNotFound, resourceMissingBody)
		enqueueOutbox(t, env.DB, "cus_list_missing")

		deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
		require.NoError(t, err)
		assert.Equal(t, 1, deleted)
		assert.False(t, fake.customerExists("cus_list_missing"), "nothing to expire; the Customer is still deleted")
		assert.Zero(t, countTable(t, env.DB, "billing_erasure_outbox"))
	})
	t.Run("expiring a session says resource_missing", func(t *testing.T) {
		fake := newFakeStripe(t)
		env := newTestService(t, fake)
		fake.seedCustomer("cus_exp_missing")
		fake.seedSession("cs_exp_missing", "cus_exp_missing", "open")
		fake.failOn("POST", expirePath("cs_exp_missing"), http.StatusNotFound, resourceMissingBody)
		enqueueOutbox(t, env.DB, "cus_exp_missing")

		deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
		require.NoError(t, err)
		assert.Equal(t, 1, deleted)
		assert.False(t, fake.customerExists("cus_exp_missing"))
		assert.Zero(t, countTable(t, env.DB, "billing_erasure_outbox"))
	})
}

// B1: resource_missing on the delete is what a key from another Stripe account
// gets for a Customer that is alive in the right one. Only a retrieve that
// returns deleted: true ends the erasure obligation; anything else keeps the
// row, so the real Customer is not left holding the person's data.
func TestOutbox_ResourceMissingWithoutDeletedProofKeepsRow(t *testing.T) {
	const unverifiableMsg = "billing: Stripe cannot confirm the Customer was deleted; erasure row kept. Check that the Stripe key belongs to the account holding the Customer"

	t.Run("a customer this key never held is unverifiable", func(t *testing.T) {
		fake := newFakeStripe(t)
		env := newTestService(t, fake) // neither held nor deleted: both calls say resource_missing
		enqueueOutbox(t, env.DB, "cus_other_account")

		deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
		require.NoError(t, err, "a per-row failure is recorded on the row, not returned")
		assert.Zero(t, deleted)

		st, ok := loadOutboxRow(t, env.DB, "cus_other_account")
		require.True(t, ok, "the erasure obligation is kept")
		assert.Equal(t, 1, st.Attempts)
		require.True(t, st.LastErrorClass.Valid)
		assert.Equal(t, "customer_unverifiable", st.LastErrorClass.String)
		assert.Len(t, fake.requestsTo("GET", customersPath+"cus_other_account"), 1)
		requireLogLine(t, env, "ERROR", unverifiableMsg, "stripe_customer_id=cus_other_account",
			"attempts=1", "error_class=customer_unverifiable", "stage=retrieve_customer")
		assert.Empty(t, logLine(env.Logs.String(), "WARN", "billing: erasing a Stripe customer failed; will retry"),
			"an unverifiable Customer is an error, not a routine retry")
	})

	t.Run("a customer that still exists is unverifiable", func(t *testing.T) {
		fake := newFakeStripe(t)
		env := newTestService(t, fake)
		fake.seedCustomer("cus_still_here")
		fake.failOn("DELETE", customersPath+"cus_still_here", http.StatusNotFound, resourceMissingBody)
		enqueueOutbox(t, env.DB, "cus_still_here")

		deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
		require.NoError(t, err)
		assert.Zero(t, deleted)
		st, ok := loadOutboxRow(t, env.DB, "cus_still_here")
		require.True(t, ok)
		assert.Equal(t, "customer_unverifiable", st.LastErrorClass.String)
		assert.True(t, fake.customerExists("cus_still_here"))
		requireLogLine(t, env, "ERROR", unverifiableMsg, "error_class=customer_unverifiable")
	})

	t.Run("a failed retrieve is retried as a Stripe failure", func(t *testing.T) {
		fake := newFakeStripe(t)
		env := newTestService(t, fake)
		fake.seedDeletedCustomer("cus_retrieve_down")
		fake.failOn("GET", customersPath+"cus_retrieve_down", http.StatusServiceUnavailable, serverErrorBody)
		enqueueOutbox(t, env.DB, "cus_retrieve_down")

		deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
		require.NoError(t, err)
		assert.Zero(t, deleted)
		st, ok := loadOutboxRow(t, env.DB, "cus_retrieve_down")
		require.True(t, ok)
		assert.Equal(t, "api_error", st.LastErrorClass.String)
		requireLogLine(t, env, "WARN", "billing: erasing a Stripe customer failed; will retry",
			"stripe_customer_id=cus_retrieve_down", "error_class=api_error", "stage=retrieve_customer")
	})
}

// RF5: the customer completed Checkout between the session list and the
// expire, so Stripe refuses the expire with invalid_request_error. The
// session is no longer open, so the refusal counts as done (R-d) and the
// Customer is still deleted in the same pass.
func TestOutbox_ExpireRefusedStillDeletesCustomer(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	fake.seedCustomer("cus_rf5")
	fake.seedSession("cs_rf5", "cus_rf5", "open")
	fake.failOn("POST", expirePath("cs_rf5"), http.StatusBadRequest, invalidRequestBody)
	enqueueOutbox(t, env.DB, "cus_rf5")

	deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
	require.NoError(t, err)

	assert.Equal(t, 1, deleted, "the row is removed in the same pass")
	assert.Len(t, fake.requestsTo("POST", expirePath("cs_rf5")), 1, "the expire was attempted")
	assert.Len(t, fake.requestsTo("DELETE", customersPath+"cus_rf5"), 1)
	assert.False(t, fake.customerExists("cus_rf5"), "the Customer is deleted despite the refused expire")
	assert.Zero(t, countTable(t, env.DB, "billing_erasure_outbox"))
	// B4: the tolerated refusal leaves a trace with the session and Stripe's
	// code (this refusal carries none), never Stripe's message.
	requireLogLine(t, env, "INFO", "billing: Stripe refused to expire a Checkout session; treating it as no longer open",
		"checkout_session_id=cs_rf5", "stripe_error_code=none")
	assert.NotContains(t, env.Logs.String(), "is not open and cannot be expired", "Stripe's message is never logged")
}

// R-d's other half: an expire that fails for a reason that does not mean "the
// session is no longer open" must retry the row and must not delete the
// Customer, or a still-open Checkout could complete against a deleted Customer.
func TestOutbox_ExpireTransientFailureRetriesRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "5xx", status: http.StatusServiceUnavailable, body: serverErrorBody},
		{name: "429 rate limit", status: http.StatusTooManyRequests, body: rateLimitBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeStripe(t)
			env := newTestService(t, fake)
			fake.seedCustomer("cus_exp_retry")
			fake.seedSession("cs_exp_retry", "cus_exp_retry", "open")
			fake.failOn("POST", expirePath("cs_exp_retry"), tc.status, tc.body)
			enqueueOutbox(t, env.DB, "cus_exp_retry")

			deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
			require.NoError(t, err, "a per-row failure is recorded on the row, not returned")
			assert.Zero(t, deleted)

			assert.True(t, fake.customerExists("cus_exp_retry"), "the Customer must survive a failed expire")
			assert.Empty(t, fake.requestsTo("DELETE", customersPath+"cus_exp_retry"))
			st, ok := loadOutboxRow(t, env.DB, "cus_exp_retry")
			require.True(t, ok, "the row is kept")
			assert.Equal(t, 1, st.Attempts)
			assert.True(t, st.LastErrorClass.Valid)
		})
	}
}

func TestOutbox_StripeOutageBacksOff(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := outboxCtx(t)
	fake.seedCustomer("cus_out")
	fake.failOn("DELETE", customersPath+"cus_out", http.StatusServiceUnavailable, serverErrorBody)
	enqueueOutbox(t, env.DB, "cus_out")

	deleted, err := env.Svc.ProcessOutbox(ctx)
	require.NoError(t, err)
	assert.Zero(t, deleted)

	st, ok := loadOutboxRow(t, env.DB, "cus_out")
	require.True(t, ok, "the row is kept for a retry")
	assert.Equal(t, 1, st.Attempts)
	require.True(t, st.LastErrorClass.Valid)
	assert.Equal(t, "api_error", st.LastErrorClass.String, "a class, never an error message")
	assert.LessOrEqual(t, len(st.LastErrorClass.String), 32)
	// Failure 1 backs off one minute (outboxBackoff(attempts) with the
	// incremented attempts), replacing the 5-minute claim lease.
	assert.Greater(t, st.NextAttemptIn, 45*time.Second)
	assert.Less(t, st.NextAttemptIn, 75*time.Second)
	assert.True(t, fake.customerExists("cus_out"))
	requireLogLine(t, env, "WARN", "billing: erasing a Stripe customer failed; will retry",
		"stripe_customer_id=cus_out", "attempts=1", "error_class=api_error", "stage=delete_customer")

	before := len(fake.requestsTo("DELETE", customersPath+"cus_out"))
	deleted, err = env.Svc.ProcessOutbox(ctx)
	require.NoError(t, err)
	assert.Zero(t, deleted)
	assert.Len(t, fake.requestsTo("DELETE", customersPath+"cus_out"), before, "a second immediate run claims nothing")
	again, _ := loadOutboxRow(t, env.DB, "cus_out")
	assert.Equal(t, 1, again.Attempts, "and does not count another attempt")
}

func TestOutbox_RecoversAfterOutage(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := outboxCtx(t)
	fake.seedCustomer("cus_rec")
	fake.failOn("DELETE", customersPath+"cus_rec", http.StatusServiceUnavailable, serverErrorBody)
	enqueueOutbox(t, env.DB, "cus_rec")
	_, err := env.Svc.ProcessOutbox(ctx)
	require.NoError(t, err)

	fake.mu.Lock()
	delete(fake.failures, "DELETE "+customersPath+"cus_rec")
	fake.mu.Unlock()
	_, err = env.DB.Exec(`UPDATE billing_erasure_outbox SET next_attempt_at = NOW() WHERE stripe_customer_id = 'cus_rec'`)
	require.NoError(t, err)

	deleted, err := env.Svc.ProcessOutbox(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	assert.False(t, fake.customerExists("cus_rec"))
	assert.Zero(t, countTable(t, env.DB, "billing_erasure_outbox"))
}

func TestOutbox_BatchIsBounded(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	const total = outboxBatch + 3
	for i := range total {
		id := fmt.Sprintf("cus_batch_%02d", i)
		fake.seedCustomer(id)
		enqueueOutbox(t, env.DB, id)
	}

	deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
	require.NoError(t, err)
	assert.Equal(t, outboxBatch, deleted, "one pass handles at most outboxBatch rows")
	assert.Equal(t, total-outboxBatch, countTable(t, env.DB, "billing_erasure_outbox"))
}

func TestOutbox_FutureRowsAreNotClaimed(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	fake.seedCustomer("cus_later")
	_, err := env.DB.Exec(
		`INSERT INTO billing_erasure_outbox (stripe_customer_id, next_attempt_at) VALUES ('cus_later', NOW() + INTERVAL '1 hour')`)
	require.NoError(t, err)

	deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
	require.NoError(t, err)
	assert.Zero(t, deleted)
	assert.True(t, fake.customerExists("cus_later"))
	assert.Equal(t, 1, countTable(t, env.DB, "billing_erasure_outbox"))
}

func TestOutbox_CanceledContextReturnsError(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := env.Svc.ProcessOutbox(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// R-b: the claim is a short transaction that leases the row by pushing
// next_attempt_at about five minutes ahead, then Stripe is called with no
// transaction open. The Stripe stand-in below inspects the database while the
// Customer delete is in flight.
func TestOutbox_ClaimLeasesRowAndHoldsNoLockAcrossStripeCall(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	enqueueOutbox(t, env.DB, "cus_lease")

	var (
		mu       sync.Mutex
		leaseIn  time.Duration
		lockErr  error
		observed bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Stripe-Should-Retry", "false")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == sessionsListPath:
			_, _ = w.Write([]byte(`{"object":"list","url":"/v1/checkout/sessions","has_more":false,"data":[]}`))
		case r.Method == http.MethodDelete && r.URL.Path == customersPath+"cus_lease":
			st, ok := loadOutboxRow(t, env.DB, "cus_lease")
			tx, txErr := env.DB.Begin()
			var nowaitErr error
			if txErr == nil {
				var id uuid.UUID
				nowaitErr = tx.QueryRow(
					`SELECT id FROM billing_erasure_outbox WHERE stripe_customer_id = 'cus_lease' FOR UPDATE NOWAIT`).Scan(&id)
				_ = tx.Rollback()
			} else {
				nowaitErr = txErr
			}
			mu.Lock()
			observed = ok
			leaseIn = st.NextAttemptIn
			lockErr = nowaitErr
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"cus_lease","object":"customer","deleted":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(resourceMissingBody))
		}
	}))
	t.Cleanup(srv.Close)
	env.Svc.Stripe = NewClient("sk_test_fake", srv.URL)

	deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	mu.Lock()
	defer mu.Unlock()
	require.True(t, observed, "the row existed while the Customer delete was in flight")
	assert.Greater(t, leaseIn, outboxLease-time.Minute, "the claim pushed next_attempt_at about outboxLease ahead")
	assert.LessOrEqual(t, leaseIn, outboxLease+time.Second)
	assert.NoError(t, lockErr, "no transaction holds the row lock across the Stripe call")
}

func TestOutboxBackoff(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{0, 30 * time.Second},
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{6, 32 * time.Minute},
		{7, time.Hour},
		{40, time.Hour},
		{1000, time.Hour},
		{math.MaxInt32, time.Hour},
	} {
		assert.Equal(t, tc.want, outboxBackoff(tc.attempts), "attempts=%d", tc.attempts)
	}
	prev := time.Duration(0)
	for attempts := 0; attempts < 200; attempts++ {
		got := outboxBackoff(attempts)
		assert.Positive(t, got, "attempts=%d must not overflow to zero or negative", attempts)
		assert.LessOrEqual(t, got, outboxMaxBackoff)
		assert.GreaterOrEqual(t, got, prev, "backoff never shrinks as attempts grow")
		prev = got
	}
	assert.Equal(t, 5*time.Minute, outboxLease)
	assert.Equal(t, 10, outboxBatch)
	assert.Equal(t, time.Hour, outboxMaxBackoff)
}

func TestOutbox_SkipLockedTwoWorkers(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	second := NewService(env.Svc.Deps)
	const total = 2 * outboxBatch
	for i := range total {
		id := fmt.Sprintf("cus_two_%02d", i)
		fake.seedCustomer(id)
		enqueueOutbox(t, env.DB, id)
	}

	var (
		start    = make(chan struct{})
		wg       sync.WaitGroup
		mu       sync.Mutex
		sum      int
		firstErr error
	)
	for _, svc := range []*Service{env.Svc, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 5 { // a pass handles one batch; loop until the outbox is drained
				n, err := svc.ProcessOutbox(context.Background())
				mu.Lock()
				sum += n
				if err != nil && firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				if n == 0 {
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	require.NoError(t, firstErr)
	assert.Equal(t, total, sum, "every row is processed exactly once across the two workers")
	assert.Zero(t, countTable(t, env.DB, "billing_erasure_outbox"))
	for i := range total {
		id := fmt.Sprintf("cus_two_%02d", i)
		assert.Len(t, fake.requestsTo("DELETE", customersPath+id), 1, "Customer %s is deleted exactly once", id)
		assert.False(t, fake.customerExists(id))
	}
}

// End to end, spec section 15's erasure row (the non-seat half): the real
// users account deletion enqueues the obligation without a user id, and the
// worker then erases the Customer at Stripe. internal/users does not import
// billing, so package billing's tests can call it without an import cycle.
func TestOutbox_ErasureEndToEnd(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := outboxCtx(t)
	userID := newPayer(t, env, "cus_e2e")
	fake.seedCustomer("cus_e2e")
	fake.seedSession("cs_e2e", "cus_e2e", "open")

	require.NoError(t, users.NewAccountService(env.DB, env.Svc.Log).DeleteAccount(dbtest.ActorContext(ctx, t, env.DB, userID.String()), userID.String()))

	assert.Zero(t, scalarInt(t, env.DB, `SELECT COUNT(*) FROM users WHERE id = $1`, userID), "the account is gone")
	assert.Empty(t, mappedCustomer(t, env.DB, userID), "the mapping cascaded away with the user")
	assert.Equal(t, 1, scalarInt(t, env.DB,
		`SELECT COUNT(*) FROM billing_erasure_outbox WHERE stripe_customer_id = 'cus_e2e'`),
		"erasure queued the Customer")
	rows, err := env.DB.Query(`SELECT column_name FROM information_schema.columns WHERE table_name = 'billing_erasure_outbox'`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var col string
		require.NoError(t, rows.Scan(&col))
		assert.False(t, strings.Contains(col, "user"), "the outbox row carries no user id (column %q)", col)
	}
	require.NoError(t, rows.Err())
	assert.True(t, fake.customerExists("cus_e2e"), "erasure itself makes no Stripe call")

	deleted, err := env.Svc.ProcessOutbox(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	assert.False(t, fake.customerExists("cus_e2e"), "the worker erased the Customer at Stripe")
	assert.Equal(t, "expired", fake.sessionStatus("cs_e2e"))
	assert.Zero(t, countTable(t, env.DB, "billing_erasure_outbox"))
}
