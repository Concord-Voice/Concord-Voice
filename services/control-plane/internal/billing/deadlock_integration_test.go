package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/redemption"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
)

// Deadlock probe for the lock order every writer of the entitlement rows shares:
// advisory key, then users (ascending), then stripe_customers, then
// subscriptions (#2033 PR-2, Task 10). Redemption (users then subscriptions),
// Reconcile (advisory key, users, stripe_customers, subscriptions) and account
// erasure (users then everything that references it) all touch the same rows;
// if any of them took two of those locks in the opposite order, PostgreSQL
// would abort one with SQLSTATE 40P01 under this contention.

const (
	deadlockProbeRounds  = 20
	deadlockProbeTimeout = 10 * time.Second
	// pgDeadlockDetected is PostgreSQL's deadlock_detected SQLSTATE.
	pgDeadlockDetected = "40P01"
)

// isDeadlock reports whether err is, or wraps, a PostgreSQL deadlock abort.
func isDeadlock(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && string(pqErr.Code) == pgDeadlockDetected
}

// probeResult is what one goroutine of a round returned.
type probeResult struct {
	name string
	err  error
}

// issuePremiumCode mints one single-use one-month premium code.
func issuePremiumCode(t *testing.T, db *sql.DB, batch string) string {
	t.Helper()
	one := 1
	codes, err := redemption.NewIssuer(db, redemption.NewCatalog(), redemption.NewDBAuditSink()).Issue(
		context.Background(), redemption.IssueSpec{
			GrantKind:   redemption.GrantPremiumSubscription,
			GrantParams: map[string]any{"months": 1},
			Count:       1,
			SingleUse:   true,
			MaxRedeems:  &one,
			BatchID:     batch,
		})
	require.NoError(t, err)
	require.Len(t, codes, 1)
	return codes[0].Plaintext
}

// runProbeRound releases every operation of one round at once and returns each
// one's result. It fails the test if any goroutine is still running after
// deadlockProbeTimeout: a hung writer is a deadlock the server did not detect.
func runProbeRound(t *testing.T, ops map[string]func(context.Context) error) []probeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadlockProbeTimeout)
	defer cancel()

	start := make(chan struct{})
	results := make(chan probeResult, len(ops))
	var wg sync.WaitGroup
	for name, op := range ops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- probeResult{name: name, err: op(ctx)}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	close(start)
	select {
	case <-done:
	case <-time.After(deadlockProbeTimeout):
		cancel()
		t.Fatalf("an operation did not return within %s: probable undetected deadlock", deadlockProbeTimeout)
	}
	close(results)

	var out []probeResult
	for r := range results {
		out = append(out, r)
	}
	return out
}

// Per round, two users each redeem a premium code and have their Stripe
// subscription reconciled, the first user redeems a second code at the same
// time (two same-user redemptions contend on one users row), and the first user
// is erased. A third user, C, has no mapping: C's reconcile self-heals it (the
// users lock, then the stripe_customers insert) while C is erased. All of it is
// released at once, and none may deadlock. Errors other than 40P01 are
// expected for A and C (a writer may reach their rows after erasure) and are
// not asserted; B is never erased, so every B operation must succeed, which
// proves each round ran its writers rather than failing them early.
//
// The same-user pair used to deadlock inside internal/redemption on its own
// (the ledger insert took a key-share lock on the users row, then the grant
// upgraded it with FOR UPDATE, #1306); the engine now takes the users row FOR
// UPDATE before anything else, which is what keeps it in this probe's order.
func TestDeadlockProbe_RedemptionReconcileErasure(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	engine := redemption.NewEngine(env.DB, redemption.NewCatalog(), env.Changer)
	// A redemption that lands after its user's reconcile takes the code-credit
	// path (#2033 D1), which locks the Stripe row and queues the credit in the
	// outbox inside the transaction; it must keep the same order.
	engine.SetCrediter(env.Svc)
	accounts := users.NewAccountService(env.DB, env.Svc.Log)

	erased, healed := 0, 0
	for round := 0; round < deadlockProbeRounds; round++ {
		userA := newPayer(t, env, fmt.Sprintf("cus_dl_a_%d", round))
		userB := newPayer(t, env, fmt.Sprintf("cus_dl_b_%d", round))
		userC := dbtest.CreateUser(t, env.DB) // unmapped: its reconcile self-heals
		subA := fmt.Sprintf("sub_dl_a_%d", round)
		subB := fmt.Sprintf("sub_dl_b_%d", round)
		subC := fmt.Sprintf("sub_dl_c_%d", round)
		seedFakeSub(t, fake, subA, fmt.Sprintf("cus_dl_a_%d", round), nil)
		seedFakeSub(t, fake, subB, fmt.Sprintf("cus_dl_b_%d", round), nil)
		seedFakeSub(t, fake, subC, fmt.Sprintf("cus_dl_c_%d", round), map[string]string{
			subMetaUserID: userC.String(), subMetaEnv: testEnvironment,
		})
		codeOne := issuePremiumCode(t, env.DB, fmt.Sprintf("dl-%d-1", round))
		codeTwo := issuePremiumCode(t, env.DB, fmt.Sprintf("dl-%d-2", round))
		codeThree := issuePremiumCode(t, env.DB, fmt.Sprintf("dl-%d-3", round))

		results := runProbeRound(t, map[string]func(context.Context) error{
			"redeem-a":    redeemOp(engine, userA, codeOne),
			"redeem-b":    redeemOp(engine, userB, codeTwo),
			"redeem-a2":   redeemOp(engine, userA, codeThree),
			"reconcile-a": func(ctx context.Context) error { _, err := env.Svc.Reconcile(ctx, subA, "", ""); return err },
			"reconcile-b": func(ctx context.Context) error { _, err := env.Svc.Reconcile(ctx, subB, "", ""); return err },
			"erase-a":     func(ctx context.Context) error { return accounts.DeleteAccount(ctx, userA.String()) },
			"reconcile-c": func(ctx context.Context) error { _, err := env.Svc.Reconcile(ctx, subC, "", ""); return err },
			"erase-c":     func(ctx context.Context) error { return accounts.DeleteAccount(ctx, userC.String()) },
		})
		for _, r := range results {
			assert.Falsef(t, isDeadlock(r.err), "round %d: %s hit SQLSTATE %s: %v", round, r.name, pgDeadlockDetected, r.err)
			switch {
			case r.name == "redeem-b" || r.name == "reconcile-b":
				require.NoErrorf(t, r.err, "round %d: %s must succeed: B is never erased", round, r.name)
			case r.name == "erase-a" && r.err == nil:
				erased++
			case r.name == "reconcile-c" && r.err == nil:
				healed++
			}
		}
	}
	assert.Positive(t, erased, "the erasure must succeed in at least one round, or the probe never exercised it")
	assert.Positive(t, healed, "C's self-heal must commit in at least one round, or the probe never raced it against erasure")
	t.Logf("erasures of A: %d/%d; self-heals of C: %d/%d", erased, deadlockProbeRounds, healed, deadlockProbeRounds)
}

func redeemOp(engine *redemption.Engine, userID uuid.UUID, code string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := engine.RedeemConfirmed(ctx, userID, code, true) // a probe sub may be on another plan
		return err
	}
}
