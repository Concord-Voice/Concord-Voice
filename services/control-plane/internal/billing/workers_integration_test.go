package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Supplementary coverage for Tasks 8 and 9: the worker loops and the error
// paths the behavioural tests do not reach. None of these relaxes an
// assertion in outbox_integration_test.go or drift_integration_test.go.

// startWorker runs fn until the returned stop is called, and fails the test if
// fn does not return after its context is canceled.
func startWorker(t *testing.T, fn func(context.Context)) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(ctx)
	}()
	var stopped bool
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the worker did not stop after its context was canceled")
		}
	}
	t.Cleanup(stop)
	return stop
}

// closedDBService returns a Service whose database handle is already closed,
// so every query fails.
func closedDBService(t *testing.T, env *testEnv) *Service {
	t.Helper()
	dead := openProbeDB(t)
	require.NoError(t, dead.Close())
	deps := env.Svc.Deps
	deps.DB = dead
	return NewService(deps)
}

func TestRunOutboxWorker_ProcessesAtStartupAndStops(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	fake.seedCustomer("cus_worker")
	enqueueOutbox(t, env.DB, "cus_worker")

	stop := startWorker(t, env.Svc.RunOutboxWorker)

	require.Eventually(t, func() bool { return countTable(t, env.DB, "billing_erasure_outbox") == 0 },
		10*time.Second, 20*time.Millisecond, "the startup pass erases the Customer without waiting a tick")
	assert.False(t, fake.customerExists("cus_worker"))
	stop()
}

func TestRunOutboxWorker_LogsAFailedPass(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	svc := closedDBService(t, env)
	svc.Log = env.Svc.Log

	stop := startWorker(t, svc.RunOutboxWorker)

	require.Eventually(t, func() bool {
		return logLine(env.Logs.String(), "WARN", "billing: erasure outbox pass failed", "stage=claim_outbox") != ""
	}, 10*time.Second, 20*time.Millisecond)
	stop()
}

func TestRunDriftWorker_RunsAtStartupAndStops(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	_, err := env.DB.Exec(
		`INSERT INTO stripe_webhook_events (event_id, event_type, processed_at) VALUES ('evt_worker_old', 'x', NOW() - INTERVAL '40 days')`)
	require.NoError(t, err)

	stop := startWorker(t, env.Svc.RunDriftWorker)

	require.Eventually(t, func() bool { return dedupeCount(t, env.DB, "evt_worker_old") == 0 },
		10*time.Second, 20*time.Millisecond, "the startup pass runs without waiting a tick")
	stop()
}

func TestRunDriftWorker_LogsAFailedPass(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	svc := closedDBService(t, env)
	svc.Log = env.Svc.Log

	stop := startWorker(t, svc.RunDriftWorker)

	require.Eventually(t, func() bool {
		return logLine(env.Logs.String(), "WARN", "billing: drift pass failed", "stage=drift_select") != ""
	}, 10*time.Second, 20*time.Millisecond)
	stop()
}

func TestOutboxAndDrift_ClosedDatabaseReturnsErrors(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	svc := closedDBService(t, env)

	_, err := svc.ProcessOutbox(context.Background())
	require.Error(t, err, "a failed claim is returned")

	_, err = svc.RunDrift(context.Background())
	require.Error(t, err, "failed stage queries are returned, joined")
}

// A context canceled while Stripe is being called ends the pass without
// recording a failure: the lease expires and the next pass retries the row.
func TestOutbox_CancelDuringStripeCallDoesNotCountAnAttempt(t *testing.T) {
	env := newTestService(t, newFakeStripe(t))
	enqueueOutbox(t, env.DB, "cus_cancel")
	ctx, cancel := context.WithCancel(outboxCtx(t))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"object":"list","url":"/v1/checkout/sessions","has_more":false,"data":[]}`))
			return
		}
		cancel()
		<-r.Context().Done() // the client gives up; never answer
	}))
	t.Cleanup(srv.Close)
	env.Svc.Stripe = NewClient("sk_test_fake", srv.URL)

	deleted, err := env.Svc.ProcessOutbox(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, deleted)

	st, ok := loadOutboxRow(t, env.DB, "cus_cancel")
	require.True(t, ok)
	assert.Zero(t, st.Attempts, "an interrupted attempt is not a failure")
	assert.False(t, st.LastErrorClass.Valid)
}

// The Customer is gone but the row cannot be removed: the pass does not count
// it, logs it, and the next pass finishes the job on resource_missing.
func TestOutbox_RowRemovalFailureIsLoggedAndRetried(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	fake.seedCustomer("cus_rm")
	enqueueOutbox(t, env.DB, "cus_rm")
	// A unique name per run: the test database is shared, and a fixed-name
	// trigger left by a crashed run would block every later delete.
	block := uniqueSQLName("test_block_outbox_delete")
	_, err := env.DB.Exec(`
		CREATE FUNCTION ` + block + `() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'delete blocked by test'; END $$;
		CREATE TRIGGER ` + block + ` BEFORE DELETE ON billing_erasure_outbox
			FOR EACH ROW EXECUTE FUNCTION ` + block + `();`)
	require.NoError(t, err)
	dropped := false
	dropTrigger := func() {
		if dropped {
			return
		}
		dropped = true
		_, derr := env.DB.Exec(`DROP TRIGGER ` + block + ` ON billing_erasure_outbox;
			DROP FUNCTION ` + block + `()`)
		require.NoError(t, derr)
	}
	t.Cleanup(dropTrigger)

	deleted, err := env.Svc.ProcessOutbox(outboxCtx(t))
	require.NoError(t, err)
	assert.Zero(t, deleted, "a row that could not be removed is not counted")
	assert.False(t, fake.customerExists("cus_rm"))
	requireLogLine(t, env, "WARN", "billing: removing a completed erasure row failed",
		"stripe_customer_id=cus_rm", "error_class=db_P0001", "stage=delete_outbox_row")

	dropTrigger()
	_, err = env.DB.Exec(`UPDATE billing_erasure_outbox SET next_attempt_at = NOW() WHERE stripe_customer_id = 'cus_rm'`)
	require.NoError(t, err)
	deleted, err = env.Svc.ProcessOutbox(outboxCtx(t))
	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "the Customer is already gone, which is success")
	assert.Zero(t, countTable(t, env.DB, "billing_erasure_outbox"))
	assert.Len(t, fake.requestsTo("GET", customersPath+"cus_rm"), 1,
		"the second delete's resource_missing was confirmed by a retrieve")
}

func TestDrift_DiscoveryListFailureIsLoggedAndOthersContinue(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	for _, cus := range []string{"cus_dl_a", "cus_dl_b"} {
		user := newPayer(t, env, cus)
		_, err := env.DB.Exec(`UPDATE stripe_customers SET last_checkout_at = NOW() WHERE user_id = $1`, user)
		require.NoError(t, err)
	}
	fake.failOn("GET", subListPath, http.StatusServiceUnavailable, serverErrorBody)

	report, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err, "a per-customer failure is logged, not returned")

	assert.Zero(t, report.Discovered)
	assert.Len(t, fake.requestsTo("GET", subListPath), 2, "both customers were tried")
	assert.Contains(t, env.Logs.String(), "cus_dl_a")
	assert.Contains(t, env.Logs.String(), "cus_dl_b")
}

func TestDrift_DiscoveredSubscriptionThatFailsToReconcileIsSkipped(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	user := newPayer(t, env, "cus_dr")
	_, err := env.DB.Exec(`UPDATE stripe_customers SET last_checkout_at = NOW() WHERE user_id = $1`, user)
	require.NoError(t, err)
	seedFakeSub(t, fake, "sub_dr_bad", "cus_dr", nil)
	seedFakeSub(t, fake, "sub_dr_good", "cus_dr", nil)
	fake.failOn("GET", subscriptionGetPath+"sub_dr_bad", http.StatusServiceUnavailable, serverErrorBody)

	report, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)

	assert.Equal(t, 1, report.Discovered)
	assert.Zero(t, countSubsBySubID(t, env.DB, "sub_dr_bad"))
	assert.Equal(t, "active", loadSubRow(t, env.DB, "sub_dr_good").Status)
	assert.Contains(t, env.Logs.String(), "sub_dr_bad")
}
