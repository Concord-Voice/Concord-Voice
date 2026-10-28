package billing

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This database double models the subscription row and the session advisory
// lock. Real Reconcile, Stripe fetches and tier notifications exercise ordering
// without requiring a PostgreSQL service.
type notificationOrderConnector struct{ s *notificationOrderStore }

func (c notificationOrderConnector) Connect(context.Context) (driver.Conn, error) {
	return &notificationOrderConn{c.s}, nil
}
func (c notificationOrderConnector) Driver() driver.Driver { return notificationOrderDriver(c) }

type notificationOrderDriver struct{ s *notificationOrderStore }
type notificationOrderStore struct {
	mu        sync.Mutex
	lock      chan struct{}
	user      string
	status    string
	tier      string
	end       time.Time
	attempted chan bool
}
type notificationOrderConn struct{ s *notificationOrderStore }
type notificationOrderTx struct{}
type notificationOrderRows struct {
	cols []string
	vals [][]driver.Value
	at   int
}

func (d notificationOrderDriver) Open(string) (driver.Conn, error) {
	return &notificationOrderConn{d.s}, nil
}
func (c *notificationOrderConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("unexpected Prepare")
}
func (c *notificationOrderConn) Close() error              { return nil }
func (c *notificationOrderConn) Begin() (driver.Tx, error) { return notificationOrderTx{}, nil }
func (notificationOrderTx) Commit() error                  { return nil }
func (notificationOrderTx) Rollback() error                { return nil }
func (r *notificationOrderRows) Columns() []string         { return r.cols }
func (r *notificationOrderRows) Close() error              { return nil }
func (r *notificationOrderRows) Next(dest []driver.Value) error {
	if r.at == len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.at])
	r.at++
	return nil
}
func notificationOrderResult(v ...driver.Value) driver.Rows {
	cols := make([]string, len(v))
	for i := range cols {
		cols[i] = fmt.Sprint(i)
	}
	return &notificationOrderRows{cols: cols, vals: [][]driver.Value{v}}
}
func (c *notificationOrderConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	if q == advisoryLockSQL {
		select {
		case c.s.lock <- struct{}{}:
			if c.s.attempted != nil {
				c.s.attempted <- true
			}
		default:
			if c.s.attempted != nil {
				c.s.attempted <- false
			}
			select {
			case c.s.lock <- struct{}{}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return driver.RowsAffected(1), nil
	}
	if q == upsertRowSQL {
		tier, tierOK := a[1].Value.(string)
		status, statusOK := a[2].Value.(string)
		if !tierOK || !statusOK {
			return nil, fmt.Errorf("subscription upsert requires string tier and status")
		}
		c.s.mu.Lock()
		c.s.tier = tier
		c.s.status = status
		c.s.mu.Unlock()
		return driver.RowsAffected(1), nil
	}
	return nil, fmt.Errorf("unexpected Exec %s", q)
}
func (c *notificationOrderConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if q == advisoryUnlockSQL {
		<-c.s.lock
		return notificationOrderResult(true), nil
	}
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	switch q {
	case payerByCustomerSQL:
		return notificationOrderResult(c.s.user), nil
	case lockUsersAscSQL:
		return notificationOrderResult(c.s.user), nil
	case seatPartiesSQL, activeHolderSQL, ownOpenSeatSQL, heldActiveSeatSQL:
		return &notificationOrderRows{cols: []string{"id"}}, nil
	case selectStoredRowSQL:
		return notificationOrderResult(c.s.user, c.s.tier, c.s.status, "cus_scan", c.s.end, false, "wing", "year", false, nil,
			int64(16489), nil, nil, nil, nil), nil
	case countLiveStripeRowsSQL:
		if c.s.status == "active" {
			return notificationOrderResult(int64(1)), nil
		}
		return notificationOrderResult(int64(0)), nil
	}
	if strings.HasPrefix(q, "SELECT tier FROM") {
		if c.s.status == "active" {
			return notificationOrderResult(c.s.tier), nil
		}
		return &notificationOrderRows{cols: []string{"tier"}}, nil
	}
	return nil, fmt.Errorf("unexpected Query %s", q)
}

type notificationOrderChanger struct {
	entered   chan struct{}
	release   chan struct{}
	mu        sync.Mutex
	delivered []string
	fail      bool
}

func (c *notificationOrderChanger) OnTierChange(_ context.Context, _ uuid.UUID, oldTier, newTier string) error {
	if oldTier == "premium" && newTier == "free" {
		close(c.entered)
		<-c.release
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delivered = append(c.delivered, newTier)
	if c.fail {
		return fmt.Errorf("injected notification failure")
	}
	return nil
}
func TestReconcile_NotificationsFollowCommitOrder(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("callback_error=", fail), func(t *testing.T) {
			user := uuid.New()
			store := &notificationOrderStore{user: user.String(), status: "active", tier: "premium", end: time.Unix(testPeriodEnd, 0), lock: make(chan struct{}, 1), attempted: make(chan bool, 8)}
			db := sql.OpenDB(notificationOrderConnector{store})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			fake := newFakeStripe(t)
			fake.seedSubscription("sub_scan", "cus_scan", PlanWing, "year", testPeriodEnd)
			editFakeSub(t, fake, "sub_scan", func(s *fakeSubscription) { s.Status = "unpaid" })
			changer := &notificationOrderChanger{entered: make(chan struct{}), release: make(chan struct{}), fail: fail}
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(changer.release) }) })
			svc := NewService(Deps{DB: db, Stripe: fake.client(), Changer: changer, Log: logger.NewWithWriter(&syncBuffer{})})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			first := make(chan error, 1)
			go func() { _, err := svc.Reconcile(ctx, "sub_scan", "", ""); first <- err }()
			select {
			case <-changer.entered:
			case err := <-first:
				t.Fatalf("first reconcile failed: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			require.True(t, <-store.attempted, "first reconcile acquired the advisory lock")
			editFakeSub(t, fake, "sub_scan", func(s *fakeSubscription) { s.Status = "active" })
			second := make(chan error, 1)
			go func() { _, err := svc.Reconcile(ctx, "sub_scan", "", ""); second <- err }()
			acquired := <-store.attempted
			// The driver's handshake distinguishes an acquired key from a waiter, without
			// a scheduler-dependent negative timeout. An acquired second reconcile must
			// finish before the paused first notification is released.
			if acquired {
				require.NoError(t, <-second)
			}
			release.Do(func() { close(changer.release) })
			require.NoError(t, <-first)
			if !acquired {
				select {
				case err := <-second:
					require.NoError(t, err)
				case <-ctx.Done():
					t.Fatal("lock not released after notification", ctx.Err())
				}
			}
			changer.mu.Lock()
			defer changer.mu.Unlock()
			t.Logf("latest database status=%s; delivered tiers=%v", store.status, changer.delivered)
			assert.Equal(t, "active", store.status, "newer reconcile committed the restored subscription")
			assert.Equal(t, []string{"free", "premium"}, changer.delivered, "stale downgrade must not be delivered after the upgrade")
		})
	}
}
func TestReconcile_FetchFailureReleasesNotificationLock(t *testing.T) {
	store := &notificationOrderStore{user: uuid.NewString(), status: "active", tier: "premium", end: time.Unix(testPeriodEnd, 0), lock: make(chan struct{}, 1)}
	db := sql.OpenDB(notificationOrderConnector{store})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	fake := newFakeStripe(t)
	fake.seedSubscription("sub_scan_error", "cus_scan", PlanWing, "year", testPeriodEnd)
	svc := NewService(Deps{DB: db, Stripe: fake.client(), Changer: &recordingChanger{}, Log: logger.NewWithWriter(&syncBuffer{})})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := svc.Reconcile(ctx, "sub_missing", "", ""); err == nil {
		t.Fatal("missing subscription should fail")
	}
	done := make(chan error, 1)
	go func() { _, err := svc.Reconcile(ctx, "sub_scan_error", "", ""); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("fetch failure leaked the advisory lock")
	}
}

type notificationDeadlineChanger struct {
	entered chan context.Context
}

func (c notificationDeadlineChanger) OnTierChange(ctx context.Context, _ uuid.UUID, oldTier, newTier string) error {
	if oldTier == "premium" && newTier == "free" {
		c.entered <- ctx
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func TestReconcile_NotificationDeadlineReleasesLock(t *testing.T) {
	store := &notificationOrderStore{user: uuid.NewString(), status: "active", tier: "premium", end: time.Unix(testPeriodEnd, 0), lock: make(chan struct{}, 1), attempted: make(chan bool, 8)}
	db := sql.OpenDB(notificationOrderConnector{store})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	fake := newFakeStripe(t)
	fake.seedSubscription("sub_deadline", "cus_scan", PlanWing, "year", testPeriodEnd)
	editFakeSub(t, fake, "sub_deadline", func(s *fakeSubscription) { s.Status = "unpaid" })
	changer := notificationDeadlineChanger{entered: make(chan context.Context, 1)}
	logs := &syncBuffer{}
	svc := NewService(Deps{DB: db, Stripe: fake.client(), Changer: changer, Log: logger.NewWithWriter(logs)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := svc.Reconcile(ctx, "sub_deadline", "", ""); first <- err }()
	var notifyCtx context.Context
	select {
	case notifyCtx = <-changer.entered:
	case err := <-first:
		t.Fatalf("first reconcile failed before notification: %v", err)
	case <-time.After(time.Second):
		t.Fatal("notification did not begin")
	}
	cancel()
	require.NoError(t, notifyCtx.Err(), "committed notification is detached from request cancellation")
	deadline, bounded := notifyCtx.Deadline()
	require.True(t, bounded, "post-commit notification must have a deadline")
	assert.WithinDuration(t, time.Now().Add(tierNotificationTimeout), deadline, time.Second)
	require.True(t, <-store.attempted)
	editFakeSub(t, fake, "sub_deadline", func(s *fakeSubscription) { s.Status = "active" })
	second := make(chan error, 1)
	go func() { _, err := svc.Reconcile(context.Background(), "sub_deadline", "", ""); second <- err }()
	require.False(t, <-store.attempted, "later reconcile must wait for the earlier notification")
	for _, done := range []chan error{first, second} {
		select {
		case err := <-done:
			require.NoError(t, err, "notification failure cannot undo the committed row")
		case <-time.After(tierNotificationTimeout + time.Second):
			t.Fatal("notification deadline did not release the subscription lock")
		}
	}
	assert.Equal(t, 0, db.Stats().InUse, "notification timeout must release the dedicated connection")
	assert.Contains(t, logs.String(), "billing: tier change push failed after reconcile")
	store.mu.Lock()
	defer store.mu.Unlock()
	assert.Equal(t, "active", store.status, "later reconcile can commit after the notification times out")
}

type recipientBudgetChanger struct {
	contexts []context.Context
	active   []error
}

func (c *recipientBudgetChanger) OnTierChange(ctx context.Context, _ uuid.UUID, _, _ string) error {
	c.contexts = append(c.contexts, ctx)
	c.active = append(c.active, ctx.Err())
	return nil
}

func TestPushTier_EachRecipientGetsNotificationBudget(t *testing.T) {
	changer := &recipientBudgetChanger{}
	svc := NewService(Deps{Changer: changer, Log: logger.NewWithWriter(&syncBuffer{})})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.pushTier(ctx, "sub_recipients", uuid.New(), "premium", "free")
	svc.pushTier(ctx, "sub_recipients", uuid.New(), "premium", "free")
	require.Len(t, changer.contexts, 2)
	for i, notification := range changer.contexts {
		assert.NoError(t, changer.active[i], "each recipient starts with an active independent budget")
		deadline, bounded := notification.Deadline()
		require.True(t, bounded)
		assert.WithinDuration(t, time.Now().Add(tierNotificationTimeout), deadline, time.Second)
		assert.ErrorIs(t, notification.Err(), context.Canceled, "each recipient cleans up its own timer")
	}
	assert.True(t, changer.contexts[0] != changer.contexts[1], "the owner's budget cannot reuse the payer's expired context")
}

func TestAfterSeatCommit_RequestCancellationStillPushesCurrentTier(t *testing.T) {
	for _, requestState := range []string{"live", "canceled", "deadline expired"} {
		for _, currentTier := range []string{"free", "premium"} {
			t.Run(requestState+" current tier "+currentTier, func(t *testing.T) {
				holder := uuid.MustParse("11111111-1111-4111-8111-111111111111")
				staleTier := "premium"
				status := "canceled"
				if currentTier == "premium" {
					staleTier = "free"
					status = "active"
				}
				store := &notificationOrderStore{user: holder.String(), status: status, tier: currentTier}
				db := sql.OpenDB(notificationOrderConnector{store})
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				current, err := entitlements.ResolveTierChecked(t.Context(), db, holder.String())
				require.NoError(t, err)
				require.Equal(t, currentTier, current, "fixture must resolve the later committed tier before cancellation")
				changer := &recordingChanger{}
				svc := NewService(Deps{DB: db, Changer: changer, Log: logger.NewWithWriter(&syncBuffer{})})
				ctx := t.Context()
				switch requestState {
				case "canceled":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
					require.ErrorIs(t, ctx.Err(), context.Canceled)
				case "deadline expired":
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, time.Unix(1, 0))
					defer cancel()
					require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
				}

				svc.afterSeatCommit(ctx, seatOutcome{tiers: []tierMove{{user: holder, before: currentTier, after: staleTier}}})

				calls := changer.Calls()
				require.Len(t, calls, 1, "a committed seat change must still notify its holder")
				assert.Equal(t, current, calls[0].New, "notification must use the current database tier instead of the stale seat outcome")
			})
		}
	}
}
