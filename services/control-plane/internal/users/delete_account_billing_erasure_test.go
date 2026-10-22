package users

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// #2033 spec section 10 step 1: deleteAccountTx records the erased user's
// Stripe Customer in billing_erasure_outbox before the users delete cascades
// the stripe_customers mapping away. These tests drive the real
// DeleteAccount path against PostgreSQL.

func mapStripeCustomer(t *testing.T, db *sql.DB, userID uuid.UUID) string {
	t.Helper()
	customerID := "cus_test_" + uuid.NewString()
	_, err := db.Exec(`INSERT INTO stripe_customers (user_id, stripe_customer_id) VALUES ($1, $2)`, userID, customerID)
	require.NoError(t, err)
	return customerID
}

func outboxRows(t *testing.T, db *sql.DB, customerID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM billing_erasure_outbox WHERE stripe_customer_id = $1`, customerID).Scan(&n))
	return n
}

func mappingRows(t *testing.T, db *sql.DB, customerID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM stripe_customers WHERE stripe_customer_id = $1`, customerID).Scan(&n))
	return n
}

func TestDeleteAccountBillingErasure_EnqueuesTheMappedCustomer(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	erased := testdb.CreateUser(t, db)
	customerID := mapStripeCustomer(t, db, erased)

	require.NoError(t, NewAccountService(db, logger.New("test")).DeleteAccount(context.Background(), erased.String()))

	assert.Zero(t, countUsers(t, db, erased))
	assert.Equal(t, 1, outboxRows(t, db, customerID), "the erased user's Customer is queued for Stripe erasure")
	assert.Zero(t, mappingRows(t, db, customerID), "the users cascade removed the mapping")

	// The outbox keeps no user identifier: its columns are the contract.
	rows, err := db.Query(`SELECT column_name FROM information_schema.columns WHERE table_name = 'billing_erasure_outbox'`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var col string
		require.NoError(t, rows.Scan(&col))
		assert.NotContains(t, col, "user", "billing_erasure_outbox must carry no user id column")
	}
	require.NoError(t, rows.Err())

	var attempts int
	require.NoError(t, db.QueryRow(
		`SELECT attempts FROM billing_erasure_outbox WHERE stripe_customer_id = $1`, customerID).Scan(&attempts))
	assert.Zero(t, attempts)
}

func TestDeleteAccountBillingErasure_DoesNotTouchAnotherUsersMapping(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	erased := testdb.CreateUser(t, db)
	bystander := testdb.CreateUser(t, db)
	erasedCustomer := mapStripeCustomer(t, db, erased)
	bystanderCustomer := mapStripeCustomer(t, db, bystander)

	require.NoError(t, NewAccountService(db, logger.New("test")).DeleteAccount(context.Background(), erased.String()))

	assert.Equal(t, 1, outboxRows(t, db, erasedCustomer))
	assert.Zero(t, outboxRows(t, db, bystanderCustomer), "another user's Customer must not be enqueued")
	assert.Equal(t, 1, mappingRows(t, db, bystanderCustomer), "another user's mapping must survive")
	assert.Equal(t, 1, countUsers(t, db, bystander))
}

func TestDeleteAccountBillingErasure_UnmappedUserEnqueuesNothing(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	erased := testdb.CreateUser(t, db)
	bystander := testdb.CreateUser(t, db)
	bystanderCustomer := mapStripeCustomer(t, db, bystander)
	var before int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM billing_erasure_outbox`).Scan(&before))

	require.NoError(t, NewAccountService(db, logger.New("test")).DeleteAccount(context.Background(), erased.String()))

	var after int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM billing_erasure_outbox`).Scan(&after))
	assert.Equal(t, before, after, "a user with no mapping inserts nothing")
	assert.Equal(t, 1, mappingRows(t, db, bystanderCustomer))
}

func TestDeleteAccountBillingErasure_ExistingObligationIsKeptOnce(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	erased := testdb.CreateUser(t, db)
	customerID := mapStripeCustomer(t, db, erased)
	// Checkout's 23503 path may already have queued this Customer.
	_, err := db.Exec(`INSERT INTO billing_erasure_outbox (stripe_customer_id, attempts) VALUES ($1, 3)`, customerID)
	require.NoError(t, err)

	require.NoError(t, NewAccountService(db, logger.New("test")).DeleteAccount(context.Background(), erased.String()),
		"ON CONFLICT DO NOTHING keeps erasure from failing on an already-queued Customer")

	assert.Zero(t, countUsers(t, db, erased))
	assert.Equal(t, 1, outboxRows(t, db, customerID), "still exactly one obligation")
	var attempts int
	require.NoError(t, db.QueryRow(
		`SELECT attempts FROM billing_erasure_outbox WHERE stripe_customer_id = $1`, customerID).Scan(&attempts))
	assert.Equal(t, 3, attempts, "the existing row is left untouched")
}

func TestDeleteAccountBillingErasure_EnqueueFaultFailsErasureAndKeepsTheUser(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	erased := testdb.CreateUser(t, db)
	customerID := mapStripeCustomer(t, db, erased)

	hook, wrapped := stmthook.Open(t)
	hook.Arm([]string{"INSERT INTO billing_erasure_outbox"}, nil, stmthook.ErrInjected)

	err := NewAccountService(wrapped, logger.New("test")).DeleteAccount(context.Background(), erased.String())

	require.Error(t, err)
	require.ErrorIs(t, err, stmthook.ErrInjected)
	assert.Contains(t, err.Error(), "enqueue billing erasure")
	seen, betweenErr := hook.Report()
	require.NoError(t, betweenErr)
	require.Equal(t, 1, seen, "the fault fired at the outbox INSERT, not at another statement")
	assert.Equal(t, 1, countUsers(t, db, erased), "a failed enqueue rolls the whole erasure back")
	assert.Equal(t, 1, mappingRows(t, db, customerID), "the mapping survives the rollback")
	assert.Zero(t, outboxRows(t, db, customerID))
}

// seedStripeSubscription inserts a source='stripe' row for userID on customerID
// and returns the customer id.
func seedStripeSubscription(t *testing.T, db *sql.DB, userID uuid.UUID, customerID string) string {
	t.Helper()
	_, err := db.Exec(`INSERT INTO subscriptions
		(user_id, tier, status, source, stripe_customer_id, stripe_subscription_id, current_period_end, plan, billing_interval)
		VALUES ($1, 'premium', 'active', 'stripe', $2, $3, NOW() + INTERVAL '30 days', 'wing', 'month')`,
		userID, customerID, "sub_test_"+uuid.NewString())
	require.NoError(t, err)
	return customerID
}

// B3: Reconcile can write a user's Stripe row on a Customer the mapping does
// not name. Erasure queues every such Customer too, each exactly once, and
// never another user's.
func TestDeleteAccountBillingErasure_EnqueuesSubscriptionCustomers(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	erased := testdb.CreateUser(t, db)
	bystander := testdb.CreateUser(t, db)
	mapped := mapStripeCustomer(t, db, erased)
	seedStripeSubscription(t, db, erased, mapped) // on the mapped Customer: queued once, not twice
	unmapped := seedStripeSubscription(t, db, erased, "cus_test_"+uuid.NewString())
	seedStripeSubscription(t, db, erased, unmapped) // two rows, one Customer
	bystanderCustomer := seedStripeSubscription(t, db, bystander, "cus_test_"+uuid.NewString())
	_, err := db.Exec(`INSERT INTO subscriptions (user_id, tier, status, source) VALUES ($1, 'premium', 'active', 'code')`, erased)
	require.NoError(t, err, "a code row carries no Customer and adds nothing")
	var before int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM billing_erasure_outbox`).Scan(&before))

	require.NoError(t, NewAccountService(db, logger.New("test")).DeleteAccount(context.Background(), erased.String()))

	assert.Equal(t, 1, outboxRows(t, db, mapped), "the mapped Customer is queued once")
	assert.Equal(t, 1, outboxRows(t, db, unmapped), "the unmapped Customer holding the user's Stripe rows is queued once")
	assert.Zero(t, outboxRows(t, db, bystanderCustomer), "another user's Customer is not queued")
	var after int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM billing_erasure_outbox`).Scan(&after))
	assert.Equal(t, before+2, after, "exactly the two Customers are queued")
}

func TestDeleteAccountBillingErasure_UnmappedUserWithStripeRowsIsQueued(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	erased := testdb.CreateUser(t, db)
	customer := seedStripeSubscription(t, db, erased, "cus_test_"+uuid.NewString())

	require.NoError(t, NewAccountService(db, logger.New("test")).DeleteAccount(context.Background(), erased.String()))

	assert.Zero(t, countUsers(t, db, erased))
	assert.Equal(t, 1, outboxRows(t, db, customer), "a Customer known only from a subscription row is queued")
}

func TestDeleteAccountBillingErasure_SubscriptionEnqueueFaultFailsErasure(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	erased := testdb.CreateUser(t, db)
	customer := seedStripeSubscription(t, db, erased, "cus_test_"+uuid.NewString())

	hook, wrapped := stmthook.Open(t)
	hook.Arm([]string{"SELECT DISTINCT stripe_customer_id FROM subscriptions"}, nil, stmthook.ErrInjected)

	err := NewAccountService(wrapped, logger.New("test")).DeleteAccount(context.Background(), erased.String())

	require.ErrorIs(t, err, stmthook.ErrInjected)
	assert.Contains(t, err.Error(), "enqueue billing erasure of subscription customers")
	assert.Equal(t, 1, countUsers(t, db, erased), "a failed enqueue rolls the whole erasure back")
	assert.Zero(t, outboxRows(t, db, customer))
}
