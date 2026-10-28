package billing

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

const creditCustomer = "cus_credit"

// queueCredit inserts a user, a code, its ledger row and a pending credit, and
// returns the user, code and redemption ids.
func queueCredit(t *testing.T, db *sql.DB) (userID, codeID, redemptionID uuid.UUID) {
	t.Helper()
	userID = dbtest.CreateUser(t, db)
	require.NoError(t, db.QueryRow(`INSERT INTO redemption_codes (code_hash, grant_kind) VALUES ($1, 'premium:subscription') RETURNING id`,
		uuid.NewString()).Scan(&codeID))
	require.NoError(t, db.QueryRow(`INSERT INTO code_redemptions (code_id, user_id) VALUES ($1, $2) RETURNING id`,
		codeID, userID).Scan(&redemptionID))
	_, err := db.Exec(`INSERT INTO billing_credit_outbox (redemption_id, stripe_customer_id, amount, currency, description)
		VALUES ($1, $2, 2697, 'usd', 'Concord code credit: 3 month(s) of supersonic')`, redemptionID, creditCustomer)
	require.NoError(t, err)
	return userID, codeID, redemptionID
}

func pendingCredit(t *testing.T, db *sql.DB, redemptionID uuid.UUID) (exists bool, attempts int, class sql.NullString) {
	t.Helper()
	err := db.QueryRow(`SELECT attempts, last_error_class FROM billing_credit_outbox WHERE redemption_id = $1`, redemptionID).
		Scan(&attempts, &class)
	if err == sql.ErrNoRows {
		return false, 0, class
	}
	require.NoError(t, err)
	return true, attempts, class
}

const creditPath = "/v1/customers/" + creditCustomer + "/balance_transactions"

func TestApplyCredit_PostsOnceAndRemovesTheRow(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	userID, codeID, redemptionID := queueCredit(t, env.DB)

	env.Svc.ApplyCredit(context.Background(), redemptionID)

	reqs := env.Fake.requestsTo(http.MethodPost, creditPath)
	require.Len(t, reqs, 1)
	assert.Equal(t, "-2697", reqs[0].Form.Get("amount"), "negative is credit")
	assert.Equal(t, "usd", reqs[0].Form.Get("currency"))
	assert.Equal(t, userID.String(), reqs[0].Form.Get("metadata[concord_user_id]"))
	assert.Equal(t, codeID.String(), reqs[0].Form.Get("metadata[concord_code_id]"))
	assert.Equal(t, redemptionID.String(), reqs[0].Form.Get("metadata[concord_redemption_id]"))
	assert.Equal(t, "concord-code-credit-v2:"+redemptionID.String(), reqs[0].IdempotencyKey,
		"keyed by the redemption, so a retry replays rather than credits twice")
	exists, _, _ := pendingCredit(t, env.DB, redemptionID)
	assert.False(t, exists, "an applied credit leaves the outbox")

	env.Svc.ApplyCredit(context.Background(), redemptionID)
	assert.Len(t, env.Fake.requestsTo(http.MethodPost, creditPath), 1, "nothing left to apply")
}

func TestApplyCredit_FailureIsLeftToTheWorker(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	_, _, redemptionID := queueCredit(t, env.DB)
	env.Fake.failOn(http.MethodPost, creditPath, http.StatusInternalServerError, serverErrorBody)

	env.Svc.ApplyCredit(context.Background(), redemptionID)

	exists, attempts, class := pendingCredit(t, env.DB, redemptionID)
	require.True(t, exists, "a failed credit stays queued")
	assert.Equal(t, 1, attempts)
	assert.True(t, class.Valid)
	assert.Contains(t, env.Logs.String(), "applying a code credit failed; will retry")

	// Due again: the worker pass applies it once Stripe recovers.
	_, err := env.DB.Exec(`UPDATE billing_credit_outbox SET next_attempt_at = NOW() WHERE redemption_id = $1`, redemptionID)
	require.NoError(t, err)
	env.Fake.mu.Lock()
	delete(env.Fake.failures, http.MethodPost+" "+creditPath)
	env.Fake.mu.Unlock()
	applied, err := env.Svc.ProcessCreditOutbox(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, applied)
	exists, _, _ = pendingCredit(t, env.DB, redemptionID)
	assert.False(t, exists)
	reqs := env.Fake.requestsTo(http.MethodPost, creditPath)
	require.Len(t, reqs, 2)
	assert.Equal(t, reqs[0].IdempotencyKey, reqs[1].IdempotencyKey, "the retry replays the same key")
}

func TestApplyCredit_RepeatedFailureIsAnError(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	_, _, redemptionID := queueCredit(t, env.DB)
	_, err := env.DB.Exec(`UPDATE billing_credit_outbox SET attempts = $2 WHERE redemption_id = $1`, redemptionID, creditAlertAttempts-1)
	require.NoError(t, err)
	env.Fake.failOn(http.MethodPost, creditPath, http.StatusInternalServerError, serverErrorBody)

	env.Svc.ApplyCredit(context.Background(), redemptionID)
	assert.Contains(t, env.Logs.String(), "keeps failing at Stripe")
}

func TestApplyCredit_DeletedCustomerDropsTheCredit(t *testing.T) {
	fake := newFakeStripe(t)
	fake.deletedCustomers[creditCustomer] = true
	env := newTestService(t, fake)
	_, _, redemptionID := queueCredit(t, env.DB)
	env.Fake.failOn(http.MethodPost, creditPath, http.StatusNotFound, customerMissingParamBody)

	env.Svc.ApplyCredit(context.Background(), redemptionID)

	exists, _, _ := pendingCredit(t, env.DB, redemptionID)
	assert.False(t, exists, "nothing can hold a credit for a deleted Customer")
	assert.Contains(t, env.Logs.String(), "dropping a code credit")
}

// A Customer this key cannot see (a key for another account, say) is not
// proof of deletion: the credit stays queued.
func TestApplyCredit_UnverifiableCustomerKeepsTheCredit(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	_, _, redemptionID := queueCredit(t, env.DB)
	env.Fake.failOn(http.MethodPost, creditPath, http.StatusNotFound, customerMissingParamBody)

	env.Svc.ApplyCredit(context.Background(), redemptionID)

	exists, attempts, _ := pendingCredit(t, env.DB, redemptionID)
	assert.True(t, exists, "the credit is kept for retry")
	assert.Equal(t, 1, attempts)
	var class string
	require.NoError(t, env.DB.QueryRow(`SELECT last_error_class FROM billing_credit_outbox WHERE redemption_id = $1`, redemptionID).Scan(&class))
	assert.Equal(t, classCustomerUnverifiable, class)
	assert.NotContains(t, env.Logs.String(), "dropping a code credit")
}

// Past Stripe's idempotency window, a retry first looks for the credit, so an
// accepted attempt whose row survived is never posted twice.
func TestApplyCredit_OldRowAlreadyHeldIsNotPostedAgain(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	_, _, redemptionID := queueCredit(t, env.DB)
	_, err := env.DB.Exec(`UPDATE billing_credit_outbox SET created_at = NOW() - INTERVAL '25 hours' WHERE redemption_id = $1`, redemptionID)
	require.NoError(t, err)
	env.Fake.seedBalanceTxn(creditCustomer, redemptionID.String())

	env.Svc.ApplyCredit(context.Background(), redemptionID)

	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, creditPath), "already held: no second credit")
	exists, _, _ := pendingCredit(t, env.DB, redemptionID)
	assert.False(t, exists)
}

func TestApplyCredit_OldRowNotHeldIsPosted(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	_, _, redemptionID := queueCredit(t, env.DB)
	_, err := env.DB.Exec(`UPDATE billing_credit_outbox SET created_at = NOW() - INTERVAL '25 hours' WHERE redemption_id = $1`, redemptionID)
	require.NoError(t, err)
	env.Fake.seedBalanceTxn(creditCustomer, uuid.NewString()) // another redemption's credit

	env.Svc.ApplyCredit(context.Background(), redemptionID)

	assert.Len(t, env.Fake.requestsTo(http.MethodPost, creditPath), 1)
	exists, _, _ := pendingCredit(t, env.DB, redemptionID)
	assert.False(t, exists)
}

func TestApplyCredit_OldRowListFailureKeepsTheCredit(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	_, _, redemptionID := queueCredit(t, env.DB)
	_, err := env.DB.Exec(`UPDATE billing_credit_outbox SET created_at = NOW() - INTERVAL '25 hours' WHERE redemption_id = $1`, redemptionID)
	require.NoError(t, err)
	env.Fake.failOn(http.MethodGet, creditPath, http.StatusInternalServerError, serverErrorBody)

	env.Svc.ApplyCredit(context.Background(), redemptionID)

	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, creditPath), "never posts without knowing")
	exists, attempts, _ := pendingCredit(t, env.DB, redemptionID)
	assert.True(t, exists)
	assert.Equal(t, 1, attempts)
}

// An old row's replay check meets the deleted Customer first; it is dropped
// like a young row's post would be, not retried forever.
func TestApplyCredit_OldRowDeletedCustomerDropsTheCredit(t *testing.T) {
	fake := newFakeStripe(t)
	fake.deletedCustomers[creditCustomer] = true
	env := newTestService(t, fake)
	_, _, redemptionID := queueCredit(t, env.DB)
	_, err := env.DB.Exec(`UPDATE billing_credit_outbox SET created_at = NOW() - INTERVAL '25 hours' WHERE redemption_id = $1`, redemptionID)
	require.NoError(t, err)
	env.Fake.failOn(http.MethodGet, creditPath, http.StatusNotFound, customerMissingParamBody)

	env.Svc.ApplyCredit(context.Background(), redemptionID)

	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, creditPath))
	exists, _, _ := pendingCredit(t, env.DB, redemptionID)
	assert.False(t, exists, "nothing can hold a credit for a deleted Customer")
	assert.Contains(t, env.Logs.String(), "dropping a code credit")
}

func TestProcessCreditOutbox_SkipsRowsNotYetDue(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	_, _, redemptionID := queueCredit(t, env.DB)
	_, err := env.DB.Exec(`UPDATE billing_credit_outbox SET next_attempt_at = NOW() + INTERVAL '1 hour' WHERE redemption_id = $1`, redemptionID)
	require.NoError(t, err)

	applied, err := env.Svc.ProcessCreditOutbox(context.Background())
	require.NoError(t, err)
	assert.Zero(t, applied)
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, creditPath))
}

func TestProcessCreditOutbox_ClosedDatabase(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	svc := closedDBService(t, env)
	_, err := svc.ProcessCreditOutbox(context.Background())
	require.Error(t, err)
	assert.Equal(t, "claim_credit", errorStage(err))

	svc.ApplyCredit(context.Background(), uuid.New())
	assert.Contains(t, env.Logs.String(), "the outbox worker will apply it")
}
