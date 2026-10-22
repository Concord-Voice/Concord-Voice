package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
)

type fakeLoop struct {
	mu        sync.Mutex
	ticks     []store.TickInput
	tickErr   error
	tickGate  chan struct{} // nil: Tick returns at once; otherwise it waits, ignoring ctx (a wedged tick)
	due       bool
	dueCalls  atomic.Int32
	purgeGate chan struct{}
	purgeErr  error
	report    store.PurgeReport
	dueErr    error
	purges    atomic.Int32
	leader    bool
	leaderErr error
}

func (f *fakeLoop) Tick(_ context.Context, in store.TickInput) error {
	if f.tickGate != nil {
		<-f.tickGate
	}
	f.mu.Lock()
	f.ticks = append(f.ticks, in)
	f.mu.Unlock()
	return f.tickErr
}

func (f *fakeLoop) Purge(context.Context) (store.PurgeReport, error) {
	f.purges.Add(1)
	if f.purgeGate != nil {
		<-f.purgeGate
	}
	return f.report, f.purgeErr
}

func (f *fakeLoop) PurgeDue(context.Context) (bool, error) { f.dueCalls.Add(1); return f.due, f.dueErr }
func (f *fakeLoop) Leader(context.Context) (bool, error)   { return f.leader, f.leaderErr }

func (f *fakeLoop) tickCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.ticks) }

type fakeLock struct{ err error }

func (l fakeLock) Ping(context.Context) error { return l.err }

type notes struct {
	mu  sync.Mutex
	got []string
}

func (n *notes) add(s string) { n.mu.Lock(); n.got = append(n.got, s); n.mu.Unlock() }

func (n *notes) count(s string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, g := range n.got {
		if g == s {
			c++
		}
	}
	return c
}

// testLoops builds loops over f. The schema check passes and the installed
// trust has not moved: a test that wants another answer replaces l.schema or
// l.trust.
func testLoops(f *fakeLoop, leader bool) (*loops, *notes) {
	srv := newServer(nil, quiet, func() bool { return true }, func() bool { return true })
	srv.leader.Store(leader)
	n := &notes{}
	d := newDiskWatcher(func() (uint64, uint64, error) { return 1 << 40, 1 << 41, nil }, store.NewAdmission(), quiet)
	return &loops{b: f, lock: fakeLock{}, srv: srv, disk: d, notify: n.add, log: quiet, fatal: func(error) {},
		schema:    func(context.Context) error { return nil },
		trust:     func(context.Context) (store.TrustMark, error) { return store.TrustMark{}, nil },
		tickEvery: 10 * time.Millisecond, roleEvery: 10 * time.Millisecond}, n
}

// exitReason is the ExitError reason err carries, or "" for any other error.
func exitReason(err error) string {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Reason
	}
	return ""
}

// fatals collects what a loops value reports through l.fatal.
type fatals struct{ errs []error }

func (f *fatals) add(err error) { f.errs = append(f.errs, err) }

// LD-25: at or under max(2 GiB, 10 %), not ready; re-armed at the floor + 256 MiB.
func TestDiskWatcherHysteresis(t *testing.T) {
	const gib = uint64(1) << 30
	avail, total := 20*gib, 100*gib
	var statErr error
	d := newDiskWatcher(func() (uint64, uint64, error) { return avail, total, statErr }, store.NewAdmission(), quiet)
	for i, c := range []struct {
		avail uint64
		err   error
		want  bool
	}{
		{20 * gib, nil, true},
		{10*gib + diskRearm - 1, nil, true},  // inside the band before any drop: still ready
		{10 * gib, nil, false},               // at the floor, 10 % of 100 GiB
		{10*gib + diskRearm - 1, nil, false}, // inside the band after a drop: held
		{10*gib + diskRearm, nil, true},      // re-armed
		{20 * gib, errors.New("statfs"), false},
		{20 * gib, nil, true},
	} {
		avail, statErr = c.avail, c.err
		d.update()
		if d.ready.Load() != c.want {
			t.Errorf("step %d: ready = %v, want %v", i, !c.want, c.want)
		}
	}
	total, avail = 10*gib, 2*gib // a small volume: the 2-GiB minimum governs
	d.update()
	if d.ready.Load() {
		t.Error("2 GiB free on a 10-GiB volume passed the 2-GiB floor")
	}
}

// diskSpy records what the disk watcher tells the Admission.
type diskSpy struct{ got []bool }

func (d *diskSpy) SetDiskReady(ok bool) { d.got = append(d.got, ok) }

// B2-D18: the watcher's verdict reaches the Admission the handlers share, not
// only its own flag.
func TestDiskWatcherSetsTheSharedAdmission(t *testing.T) {
	const gib = uint64(1) << 30
	avail := 20 * gib
	spy := &diskSpy{}
	d := newDiskWatcher(func() (uint64, uint64, error) { return avail, 100 * gib, nil }, spy, quiet)
	d.update()
	avail = gib
	d.update()
	avail = 20 * gib
	d.update()
	if want := []bool{true, false, true}; !slices.Equal(spy.got, want) {
		t.Fatalf("the Admission was told %v, want %v", spy.got, want)
	}
}

// §3.2.4: every tick ends with WATCHDOG=1 whatever the database said.
func TestTickSendsTheWatchdogWhateverTheDatabaseSaid(t *testing.T) {
	f := &fakeLoop{tickErr: errors.New("db down")}
	l, n := testLoops(f, true)
	l.srv.lastPeer.Store(12345)
	l.tick(context.Background())
	if n.count("WATCHDOG=1") != 1 || f.tickCount() != 1 || f.ticks[0].LastPeerContactMs != 12345 {
		t.Fatalf("watchdog %d, ticks %v", n.count("WATCHDOG=1"), f.ticks)
	}
}

// D-203 and mutation 11: a wedged tick sends no WATCHDOG, and nothing else does.
func TestWedgedTickStarvesTheWatchdog(t *testing.T) {
	f := &fakeLoop{tickGate: make(chan struct{}), leader: true} // the role poll runs too, and must keep it a leader
	l, n := testLoops(f, true)
	ctx, cancel := context.WithCancel(context.Background())
	l.start(ctx)
	time.Sleep(200 * time.Millisecond)
	if c := n.count("WATCHDOG=1"); c != 0 {
		t.Fatalf("%d WATCHDOG while the tick was wedged", c)
	}
	close(f.tickGate)
	waitFor(t, "a WATCHDOG after the tick returns", func() bool { return n.count("WATCHDOG=1") > 0 })
	cancel()
	l.wait()
}

// A standby neither ticks nor purges, and still feeds the watchdog.
func TestStandbyNeitherTicksNorPurges(t *testing.T) {
	f := &fakeLoop{due: true}
	l, n := testLoops(f, false)
	l.tick(context.Background())
	l.wait()
	if f.tickCount() != 0 || f.dueCalls.Load() != 0 || f.purges.Load() != 0 || n.count("WATCHDOG=1") != 1 {
		t.Fatalf("ticks %d, due %d, purges %d, watchdog %d", f.tickCount(), f.dueCalls.Load(), f.purges.Load(), n.count("WATCHDOG=1"))
	}
}

// tickAsync runs one tick off the test goroutine, so a tick that blocks fails
// the test at waitFor's deadline instead of hanging it to go test's.
func tickAsync(ctx context.Context, l *loops) {
	go l.tick(ctx)
}

// holdPurge makes a purge block on f.purgeGate until the returned release,
// which is safe to call twice, runs. A test that fails first still releases it,
// so no goroutine outlives the test blocked.
func holdPurge(t *testing.T, f *fakeLoop) (release func()) {
	t.Helper()
	f.purgeGate = make(chan struct{})
	release = sync.OnceFunc(func() { close(f.purgeGate) })
	t.Cleanup(release)
	return release
}

// §3.2.4: the purge runs on its own goroutine, at most one at a time, and
// never delays the watchdog. Both ticks run off the test goroutine: a purge
// run on the tick goroutine would block the first and withhold its WATCHDOG.
func TestPurgeRunsAloneOffTheTickGoroutine(t *testing.T) {
	f := &fakeLoop{due: true}
	release := holdPurge(t, f)
	l, n := testLoops(f, true)
	ctx := context.Background()
	tickAsync(ctx, l)
	waitFor(t, "the first tick's WATCHDOG while its purge is held", func() bool { return n.count("WATCHDOG=1") == 1 })
	tickAsync(ctx, l) // the first purge is still running
	waitFor(t, "the second tick's WATCHDOG while the first purge is held", func() bool { return n.count("WATCHDOG=1") == 2 })
	waitFor(t, "the purge to start", func() bool { return f.purges.Load() == 1 })
	release()
	l.wait()
	if f.purges.Load() != 1 {
		t.Fatalf("%d purges ran at once, want 1", f.purges.Load())
	}
	l.tick(ctx) // the latch is released
	l.wait()
	if f.purges.Load() != 2 {
		t.Fatalf("purges %d after the first finished, want 2", f.purges.Load())
	}
}

// §3.2.4 and serve's shutdown: wait returns only once an in-flight purge has,
// so serve never exits under a purge that is still writing.
func TestWaitWaitsForAnInFlightPurge(t *testing.T) {
	f := &fakeLoop{due: true, leader: true}
	release := holdPurge(t, f)
	l, _ := testLoops(f, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.start(ctx)
	waitFor(t, "the purge to start", func() bool { return f.purges.Load() == 1 })
	cancel() // both loops return; the purge is still held
	waited := make(chan struct{})
	go func() { l.wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("wait returned while a purge was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not return after the purge finished")
	}
}

// B2-D6 and B2-D21: the role poll sets the atomic, reads an error as standby,
// and a failed lock ping is fatal with reason=lock unless serve is stopping.
func TestRolePoll(t *testing.T) {
	f := &fakeLoop{leader: true}
	l, _ := testLoops(f, false)
	ctx := context.Background()
	l.pollRole(ctx)
	if !l.srv.leader.Load() {
		t.Fatal("leader not set")
	}
	f.leaderErr = errors.New("db down")
	l.pollRole(ctx)
	if l.srv.leader.Load() {
		t.Fatal("a role-poll error left the node a leader")
	}
	var got error
	l.fatal = func(err error) { got = err }
	l.lock = fakeLock{err: errors.New("connection reset")}
	l.pollRole(ctx)
	var ee *ExitError
	if !errors.As(got, &ee) || ee.Reason != "lock" {
		t.Fatalf("lost lock: %v", got)
	}
	got = nil
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	l.pollRole(stopped)
	if got != nil {
		t.Fatalf("a ping failing while serve stops was fatal: %v", got)
	}
}

// B2-D33: a standby whose start-time view was Pending stops with
// reason=trust when promoted, and never sets the atomic; while it stays a
// standby it keeps running.
func TestPromotionWithPendingTrustStops(t *testing.T) {
	f := &fakeLoop{}
	l, _ := testLoops(f, false)
	l.pending = true
	var got error
	l.fatal = func(err error) { got = err }
	l.pollRole(context.Background())
	if got != nil || l.srv.leader.Load() {
		t.Fatalf("a pending standby: fatal %v, leader %v", got, l.srv.leader.Load())
	}
	f.leader = true
	l.pollRole(context.Background())
	var ee *ExitError
	if !errors.As(got, &ee) || ee.Reason != "trust" || l.srv.leader.Load() {
		t.Fatalf("promoted with pending trust: %v, leader %v", got, l.srv.leader.Load())
	}
}

// N4: a standby promoted onto a schema the code was not built for stops with
// reason=config and never sets the atomic. The startup check alone misses a
// migration applied while this node stood by. The errors are the shapes
// store.CheckSchema returns.
func TestPromotionRefusesASkewedOrDirtySchema(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"skew", store.ErrSchemaSkew},
		{"skew as CheckSchema words it", fmt.Errorf("%w: database %d, code %d", store.ErrSchemaSkew, 3, 4)},
		{"dirty", store.ErrSchemaDirty},
		{"dirty as CheckSchema words it", fmt.Errorf("%w at version %d", store.ErrSchemaDirty, 3)},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeLoop{leader: true}
			l, _ := testLoops(f, false)
			var calls int
			l.schema = func(context.Context) error { calls++; return c.err }
			var fatal fatals
			l.fatal = fatal.add
			l.pollRole(context.Background())
			if len(fatal.errs) != 1 || exitReason(fatal.errs[0]) != ReasonConfig || !errors.Is(fatal.errs[0], c.err) {
				t.Fatalf("fatal %v, want one reason=config wrapping %v", fatal.errs, c.err)
			}
			if l.srv.leader.Load() {
				t.Fatal("a node on a skewed schema became a leader")
			}
			if calls != 1 {
				t.Fatalf("%d schema checks, want 1", calls)
			}
		})
	}
}

// N4: a schema read that fails is not a mismatch. The node stays a standby for
// that poll, says nothing, and the next poll tries again.
func TestPromotionStaysStandbyWhenTheSchemaReadFails(t *testing.T) {
	f := &fakeLoop{leader: true}
	l, _ := testLoops(f, false)
	buf := &syncBuffer{}
	l.log = slog.New(slog.NewJSONHandler(buf, nil))
	readErr := errors.New("db down")
	var calls int
	l.schema = func(context.Context) error { calls++; return readErr }
	var fatal fatals
	l.fatal = fatal.add
	ctx := context.Background()
	l.pollRole(ctx)
	if len(fatal.errs) != 0 || l.srv.leader.Load() || calls != 1 {
		t.Fatalf("a failed read: fatal %v, leader %v, calls %d", fatal.errs, l.srv.leader.Load(), calls)
	}
	roleLines(t, buf, "schema") // the edge into the failure is one line
	l.pollRole(ctx)             // still failing: still a standby, and no second line
	if len(fatal.errs) != 0 || l.srv.leader.Load() || calls != 2 {
		t.Fatalf("a second failed read: fatal %v, leader %v, calls %d", fatal.errs, l.srv.leader.Load(), calls)
	}
	roleLines(t, buf, "schema")
	readErr = nil
	l.pollRole(ctx)
	if len(fatal.errs) != 0 || !l.srv.leader.Load() || calls != 3 {
		t.Fatalf("a passing check: fatal %v, leader %v, calls %d", fatal.errs, l.srv.leader.Load(), calls)
	}
	roleLines(t, buf, "schema")
}

// N4: the check is a flip's, not every poll's. A leader polling as a leader
// makes no schema call; a node demoted and promoted again is checked again.
func TestSchemaCheckRunsOnTheFlipOnly(t *testing.T) {
	t.Run("leader to leader", func(t *testing.T) {
		f := &fakeLoop{leader: true}
		l, _ := testLoops(f, true)
		var calls int
		l.schema = func(context.Context) error { calls++; return nil }
		for range 3 {
			l.pollRole(context.Background())
		}
		if calls != 0 || !l.srv.leader.Load() {
			t.Fatalf("%d schema checks for a node that was already a leader, leader %v", calls, l.srv.leader.Load())
		}
	})
	t.Run("one check per promotion", func(t *testing.T) {
		f := &fakeLoop{leader: true}
		l, _ := testLoops(f, false)
		var calls int
		l.schema = func(context.Context) error { calls++; return nil }
		ctx := context.Background()
		l.pollRole(ctx)
		l.pollRole(ctx)
		if calls != 1 || !l.srv.leader.Load() {
			t.Fatalf("promotion: %d checks, leader %v", calls, l.srv.leader.Load())
		}
		f.leader = false
		l.pollRole(ctx)
		if l.srv.leader.Load() || calls != 1 {
			t.Fatalf("demotion: %d checks, leader %v", calls, l.srv.leader.Load())
		}
		f.leader = true
		l.pollRole(ctx)
		if calls != 2 || !l.srv.leader.Load() {
			t.Fatalf("second promotion: %d checks, leader %v", calls, l.srv.leader.Load())
		}
	})
	t.Run("standby", func(t *testing.T) {
		f := &fakeLoop{}
		l, _ := testLoops(f, false)
		var calls int
		l.schema = func(context.Context) error { calls++; return nil }
		l.pollRole(context.Background())
		if calls != 0 {
			t.Fatalf("%d schema checks for a node that stayed a standby", calls)
		}
	})
}

// N4: a promotion with no schema check wired is a programming error, and it
// stops the node rather than promoting it unchecked.
func TestPromotionWithoutASchemaCheckIsFatal(t *testing.T) {
	f := &fakeLoop{leader: true}
	l, _ := testLoops(f, false)
	l.schema = nil
	var fatal fatals
	l.fatal = fatal.add
	l.pollRole(context.Background())
	if len(fatal.errs) != 1 || exitReason(fatal.errs[0]) != ReasonConfig || !errors.Is(fatal.errs[0], errNoSchemaCheck) {
		t.Fatalf("fatal %v, want one reason=config wrapping errNoSchemaCheck", fatal.errs)
	}
	if l.srv.leader.Load() {
		t.Fatal("a node with no schema check became a leader")
	}
}

// N4: a leader that polls as a leader needs no check, so a nil one there is
// not an error. Only a flip needs the wiring.
func TestNoSchemaCheckIsFineWhileALeaderStaysOne(t *testing.T) {
	f := &fakeLoop{leader: true}
	l, _ := testLoops(f, true)
	l.schema = nil
	var fatal fatals
	l.fatal = fatal.add
	l.pollRole(context.Background())
	if len(fatal.errs) != 0 || !l.srv.leader.Load() {
		t.Fatalf("fatal %v, leader %v", fatal.errs, l.srv.leader.Load())
	}
}

// B2-D33 and N4: the trust check comes first. A promoted standby with pending
// trust stops for trust and never reads the schema.
func TestPendingTrustIsCheckedBeforeTheSchema(t *testing.T) {
	f := &fakeLoop{leader: true}
	l, _ := testLoops(f, false)
	l.pending = true
	var calls int
	l.schema = func(context.Context) error { calls++; return store.ErrSchemaSkew }
	var fatal fatals
	l.fatal = fatal.add
	l.pollRole(context.Background())
	if len(fatal.errs) != 1 || exitReason(fatal.errs[0]) != ReasonTrust || calls != 0 || l.srv.leader.Load() {
		t.Fatalf("fatal %v, schema calls %d, leader %v", fatal.errs, calls, l.srv.leader.Load())
	}
}

// DoR §3.6: the loops' lines carry op and a typed outcome, never the error's
// text (a driver error can carry a host or a role name).
func TestLoopLogLines(t *testing.T) {
	secret := errors.New("SENTINEL-driver-error-text")
	f := &fakeLoop{tickErr: secret, purgeErr: secret, due: true}
	l, _ := testLoops(f, true)
	buf := &syncBuffer{}
	l.log = slog.New(slog.NewJSONHandler(buf, nil))
	l.tick(context.Background())
	l.wait()
	absent := filepath.Join(t.TempDir(), "n") // nothing listens: the send fails
	notifier(absent, l.log)("WATCHDOG=1")

	for msg, want := range map[string][]string{
		"authority.tick":   {"level", "msg", "op", "outcome", "time"},
		"authority.purge":  {"level", "msg", "op", "outcome", "time"},
		"authority.notify": {"code", "level", "msg", "op", "outcome", "time"}, // the state not sent
	} {
		got := lines(t, buf.Bytes(), msg)
		if len(got) != 1 {
			t.Fatalf("%s: %d lines, want 1", msg, len(got))
		}
		keys := make([]string, 0, len(got[0]))
		for k := range got[0] {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, want) {
			t.Errorf("%s: fields %v, want %v", msg, keys, want)
		}
		if got[0]["op"] != msg || got[0]["outcome"] != float64(store.KindError) {
			t.Errorf("%s: op %v, outcome %v", msg, got[0]["op"], got[0]["outcome"])
		}
	}
	for _, leak := range []string{"SENTINEL", absent} {
		if bytes.Contains(buf.Bytes(), []byte(leak)) {
			t.Errorf("a loop line carried %q: %s", leak, buf.Bytes())
		}
	}
}

func TestExitError(t *testing.T) {
	cause := errors.New("the cause")
	err := refuse(ReasonTrust, cause)
	if exitReason(err) != "trust" || !errors.Is(err, cause) || err.Error() != "trust: the cause" {
		t.Fatalf("refuse: reason %q, error %q", exitReason(err), err)
	}
	if got := (&ExitError{Reason: ReasonLock}).Error(); got != "lock" {
		t.Errorf("an ExitError with no cause reads %q, want %q", got, "lock")
	}
	for _, r := range []string{ReasonConfig, ReasonTrust, ReasonLock} {
		if r == "" {
			t.Error("an empty exit reason")
		}
	}
	if ReasonConfig == ReasonTrust || ReasonTrust == ReasonLock || ReasonConfig == ReasonLock {
		t.Error("two exit reasons are equal")
	}
}

func TestNotifierSendsTheState(t *testing.T) {
	dir, err := os.MkdirTemp("", "sd") // short: sun_path holds 104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "n")
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	notifier(sock, quiet)("READY=1")
	buf := make([]byte, 64)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := c.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "READY=1" {
		t.Fatalf("got %q, %v", buf[:n], err)
	}
	notifier("", quiet)("READY=1") // no NOTIFY_SOCKET: a no-op
}

// stuckConn is a datagram conn whose peer never drains. Write blocks until its
// write deadline has been set, then fails with a timeout at once; with none
// set it blocks until release closes. That is a full receive queue on Linux,
// where a non-blocking send waits for room. Darwin answers ENOBUFS instead, so
// the real-socket test below cannot show the block there.
type stuckConn struct {
	net.Conn // nil: only the methods below are used
	mu       sync.Mutex
	deadline time.Time
	setErr   error
	writes   atomic.Int32
	closed   atomic.Bool
	release  chan struct{}
}

func newStuckConn(t *testing.T) *stuckConn {
	t.Helper()
	c := &stuckConn{release: make(chan struct{})}
	t.Cleanup(sync.OnceFunc(func() { close(c.release) }))
	return c
}

func (c *stuckConn) SetWriteDeadline(d time.Time) error {
	if c.setErr != nil {
		return c.setErr
	}
	c.mu.Lock()
	c.deadline = d
	c.mu.Unlock()
	return nil
}

func (c *stuckConn) Write([]byte) (int, error) {
	c.writes.Add(1)
	c.mu.Lock()
	d := c.deadline
	c.mu.Unlock()
	if d.IsZero() {
		<-c.release
		return 0, errors.New("released")
	}
	return 0, os.ErrDeadlineExceeded
}

func (c *stuckConn) Close() error { c.closed.Store(true); return nil }

// sendWithin runs sendState off the test goroutine and fails the test if it
// has not returned in 3 s, so a blocked send is a failure, not a hang.
func sendWithin(t *testing.T, c net.Conn) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- sendState(c, "WATCHDOG=1") }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("sendState was still blocked after 3 s")
		return nil
	}
}

// M1: one send is bounded, so a systemd that stops reading cannot freeze the
// tick behind its WATCHDOG, nor serve behind its READY.
func TestNotifierSendIsBoundedByAWriteDeadline(t *testing.T) {
	c := newStuckConn(t)
	err := sendWithin(t, c)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a stuck send returned %v, want the deadline error", err)
	}
	c.mu.Lock()
	left := time.Until(c.deadline)
	c.mu.Unlock()
	if left <= 0 || left > notifyTimeout {
		t.Errorf("the write deadline was set %v ahead, want Within (0, %v]", left, notifyTimeout)
	}
	if !c.closed.Load() {
		t.Error("the conn was not closed")
	}
}

// M1: a deadline that cannot be set leaves the write unbounded, so nothing is
// written; the failure is returned to be logged.
func TestNotifierWritesNothingWhenTheDeadlineCannotBeSet(t *testing.T) {
	c := newStuckConn(t)
	c.setErr = errors.New("no deadline")
	if err := sendWithin(t, c); !errors.Is(err, c.setErr) {
		t.Fatalf("returned %v, want the SetWriteDeadline error", err)
	}
	if c.writes.Load() != 0 || !c.closed.Load() {
		t.Errorf("writes %d, closed %v", c.writes.Load(), c.closed.Load())
	}
}

// M1 on a real socket: a receiver that never reads and whose queue is full.
// notify returns, and logs its one error line. On Linux an unbounded send
// blocks here; on Darwin it fails at once, so this passes there either way.
//
// "Full" must mean full for a FRESH sender, because notifier dials a new
// socket per send. Linux charges a unix datagram to the sender's send buffer
// until it is read and bounds the receiver by queue length (max_dgram_qlen),
// not by its read buffer, so one filler stops when its own send buffer fills
// while a new socket still gets through. So fillers are added until a fresh
// one's first write fails.
func TestNotifierGivesUpOnAReceiverThatNeverReads(t *testing.T) {
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "n")
	addr := &net.UnixAddr{Name: sock, Net: "unixgram"}
	rc, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if err := rc.SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	full := false
	for f := 0; f < 64 && !full; f++ {
		filler, err := net.DialUnix("unixgram", nil, addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = filler.Close() })
		for i := 0; i < 10000; i++ {
			_ = filler.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
			if _, werr := filler.Write([]byte("WATCHDOG=1")); werr != nil {
				full = i == 0 // a fresh sender cannot send: the receiver is full
				break
			}
		}
	}
	if !full {
		t.Fatal("the receiver's queue never filled for a fresh sender")
	}
	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	done := make(chan struct{})
	go func() { notifier(sock, log)("WATCHDOG=1"); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("notify was still blocked after 3 s")
	}
	got := lines(t, buf.Bytes(), "authority.notify")
	if len(got) != 1 || got[0]["op"] != "authority.notify" || got[0]["outcome"] != float64(store.KindError) {
		t.Fatalf("notify lines %v, want one error line", got)
	}
}

// Sec-M2: a standby's pins and its D-223 key check are its start-time view's,
// so a promotion onto trust a leader installed since stops it with
// reason=trust, after the schema check, and never sets the atomic; systemd's
// restart reloads the trust. An unmoved mark promotes.
func TestPromotionChecksTheInstalledTrust(t *testing.T) {
	start := store.TrustMark{Successions: 1, Delegations: 2}
	for _, c := range []struct {
		name   string
		now    store.TrustMark
		leader bool
		reason string
	}{
		{"unmoved", start, true, ""},
		{"a delegation installed since", store.TrustMark{Successions: 1, Delegations: 3}, false, ReasonTrust},
		{"a succession installed since", store.TrustMark{Successions: 2, Delegations: 2}, false, ReasonTrust},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, _ := testLoops(&fakeLoop{leader: true}, false)
			l.mark = start
			var order []string
			l.schema = func(context.Context) error { order = append(order, "schema"); return nil }
			l.trust = func(ctx context.Context) (store.TrustMark, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("the trust read has no deadline")
				}
				order = append(order, "trust")
				return c.now, nil
			}
			var fatal fatals
			l.fatal = fatal.add
			l.pollRole(context.Background())
			if l.srv.leader.Load() != c.leader || fmt.Sprint(order) != "[schema trust]" {
				t.Fatalf("leader %v (want %v), checks %v", l.srv.leader.Load(), c.leader, order)
			}
			switch {
			case c.reason == "" && len(fatal.errs) != 0:
				t.Fatalf("an unmoved mark stopped the node: %v", fatal.errs)
			case c.reason != "" && (len(fatal.errs) != 1 || exitReason(fatal.errs[0]) != c.reason || !errors.Is(fatal.errs[0], errTrustMoved)):
				t.Fatalf("fatal %v, want one reason=%s wrapping errTrustMoved", fatal.errs, c.reason)
			}
		})
	}
}

// Sec-M2, as N4: a trust read that fails keeps the node a standby for that
// poll, silently, and the next poll that reads promotes. A schema refusal
// stops the node before the trust is read.
func TestPromotionStaysStandbyWhenTheTrustReadFails(t *testing.T) {
	l, _ := testLoops(&fakeLoop{leader: true}, false)
	buf := &syncBuffer{}
	l.log = slog.New(slog.NewJSONHandler(buf, nil))
	readErr := errors.New("db down")
	calls := 0
	l.trust = func(context.Context) (store.TrustMark, error) { calls++; return store.TrustMark{}, readErr }
	var fatal fatals
	l.fatal = fatal.add
	ctx := context.Background()
	l.pollRole(ctx)
	if len(fatal.errs) != 0 || l.srv.leader.Load() || calls != 1 {
		t.Fatalf("a failed trust read: fatal %v, leader %v, calls %d", fatal.errs, l.srv.leader.Load(), calls)
	}
	roleLines(t, buf, "trust")
	readErr = nil
	l.pollRole(ctx)
	if len(fatal.errs) != 0 || !l.srv.leader.Load() || calls != 2 {
		t.Fatalf("the next read: fatal %v, leader %v, calls %d", fatal.errs, l.srv.leader.Load(), calls)
	}

	l2, _ := testLoops(&fakeLoop{leader: true}, false)
	l2.schema = func(context.Context) error { return store.ErrSchemaSkew }
	l2.trust = func(context.Context) (store.TrustMark, error) {
		t.Error("the trust was read after a schema refusal")
		return store.TrustMark{}, nil
	}
	var fatal2 fatals
	l2.fatal = fatal2.add
	l2.pollRole(ctx)
	if len(fatal2.errs) != 1 || exitReason(fatal2.errs[0]) != ReasonConfig {
		t.Fatalf("schema skew: %v, want one reason=config", fatal2.errs)
	}
}

// A promotion with no trust check wired is the same programming error as one
// with no schema check: it stops the node rather than promote it unchecked.
func TestPromotionWithNoTrustCheckStops(t *testing.T) {
	l, _ := testLoops(&fakeLoop{leader: true}, false)
	l.trust = nil
	var fatal fatals
	l.fatal = fatal.add
	l.pollRole(context.Background())
	if len(fatal.errs) != 1 || exitReason(fatal.errs[0]) != ReasonConfig || !errors.Is(fatal.errs[0], errNoSchemaCheck) || l.srv.leader.Load() {
		t.Fatalf("fatal %v, leader %v", fatal.errs, l.srv.leader.Load())
	}
}

// roleLines requires exactly one authority.role error line in buf so far, with code.
func roleLines(t *testing.T, buf *syncBuffer, code string) {
	t.Helper()
	got := lines(t, buf.Bytes(), "authority.role")
	if len(got) != 1 || got[0]["op"] != "authority.role" || got[0]["code"] != code || got[0]["outcome"] != float64(store.KindError) {
		t.Fatalf("authority.role lines %v, want exactly one with code %s", got, code)
	}
}

// A Leader() error demotes the node to a standby, and the edge into
// that failure is one authority.role line, code leader, never one per poll.
// A recovery and a second failure give a second line. A poll failing because
// serve is stopping logs nothing.
func TestARoleReadFailureIsLoggedOnItsEdge(t *testing.T) {
	f := &fakeLoop{leader: true, leaderErr: errors.New("db down")}
	l, _ := testLoops(f, true)
	buf := &syncBuffer{}
	l.log = slog.New(slog.NewJSONHandler(buf, nil))
	ctx := context.Background()
	for range 3 {
		l.pollRole(ctx)
	}
	if l.srv.leader.Load() {
		t.Fatal("a failed leader read left the node a leader")
	}
	roleLines(t, buf, "leader")
	f.leaderErr = nil
	l.pollRole(ctx)
	roleLines(t, buf, "leader") // recovered: no line
	f.leaderErr = errors.New("db down")
	l.pollRole(ctx)
	if got := lines(t, buf.Bytes(), "authority.role"); len(got) != 2 {
		t.Fatalf("a second failure after a recovery: %d lines, want 2", len(got))
	}
	f.leaderErr = nil
	l.pollRole(ctx)
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	f.leaderErr = context.Canceled
	l.pollRole(stopped)
	if got := lines(t, buf.Bytes(), "authority.role"); len(got) != 2 {
		t.Fatalf("a poll cut short by the stop logged: %d lines, want 2", len(got))
	}
	checkLogConvention(t, buf.Bytes())
}

// A purge run that ends is one KindOK authority.purge line
// with complete 0 or 1, and no count; a PurgeDue that fails is an error line;
// and nothing is logged for a tick or a purge that serve's stop cut short.
func TestPurgeAndTickLines(t *testing.T) {
	for _, complete := range []bool{false, true} {
		f := &fakeLoop{due: true, report: store.PurgeReport{Pruned: 7, Purged: 9, Complete: complete}}
		l, _ := testLoops(f, true)
		buf := &syncBuffer{}
		l.log = slog.New(slog.NewJSONHandler(buf, nil))
		l.tick(context.Background())
		l.wait()
		got := lines(t, buf.Bytes(), "authority.purge")
		if len(got) != 1 || got[0]["outcome"] != float64(store.KindOK) || got[0]["complete"] != float64(b01(complete)) {
			t.Fatalf("complete %v: purge lines %v, want one KindOK line", complete, got)
		}
		if bytes.Contains(buf.Bytes(), []byte("7")) && bytes.Contains(buf.Bytes(), []byte(`"pruned"`)) {
			t.Fatalf("the purge line carried a count: %s", buf.Bytes())
		}
		checkLogConvention(t, buf.Bytes())
	}
	f := &fakeLoop{due: true, dueErr: errors.New("db down")}
	l, _ := testLoops(f, true)
	buf := &syncBuffer{}
	l.log = slog.New(slog.NewJSONHandler(buf, nil))
	l.tick(context.Background())
	l.wait()
	if got := lines(t, buf.Bytes(), "authority.purge"); len(got) != 1 || got[0]["outcome"] != float64(store.KindError) || f.purges.Load() != 0 {
		t.Fatalf("a failed due test: lines %v, purges %d; want one error line and no run", got, f.purges.Load())
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	for _, f := range []*fakeLoop{
		{tickErr: context.Canceled, dueErr: context.Canceled},
		{tickErr: context.Canceled, due: true, purgeErr: context.Canceled},
	} {
		l, _ := testLoops(f, true)
		buf := &syncBuffer{}
		l.log = slog.New(slog.NewJSONHandler(buf, nil))
		l.tick(stopped)
		l.wait()
		if len(buf.Bytes()) != 0 {
			t.Fatalf("a tick cut short by the stop logged %s", buf.Bytes())
		}
	}
}

// A Statfs that fails reads as a full disk, and the edge into failing
// is one authority.disk line; a recovery and a second failure give a second.
func TestDiskStatfsFailureIsLoggedOnItsEdge(t *testing.T) {
	var statErr error
	buf := &syncBuffer{}
	d := newDiskWatcher(func() (uint64, uint64, error) { return 1 << 40, 1 << 41, statErr }, store.NewAdmission(),
		slog.New(slog.NewJSONHandler(buf, nil)))
	d.update()
	statErr = errors.New("EACCES")
	d.update()
	d.update()
	if got := lines(t, buf.Bytes(), "authority.disk"); len(got) != 1 || got[0]["outcome"] != float64(store.KindError) || d.ready.Load() {
		t.Fatalf("lines %v, ready %v; want one error line and not ready", got, d.ready.Load())
	}
	statErr = nil
	d.update()
	statErr = errors.New("EACCES")
	d.update()
	if got := lines(t, buf.Bytes(), "authority.disk"); len(got) != 2 {
		t.Fatalf("%d lines after a recovery and a second failure, want 2", len(got))
	}
	checkLogConvention(t, buf.Bytes())
}

// clock_synced reads a failed adjtimex as unsynced, as before, and
// the edge into failing is one authority.clock line.
func TestClockReadFailureIsLoggedOnItsEdge(t *testing.T) {
	var readErr error
	buf := &syncBuffer{}
	c := &clockWatch{read: func() (bool, error) { return true, readErr }, log: slog.New(slog.NewJSONHandler(buf, nil))}
	if !c.synced() || len(buf.Bytes()) != 0 {
		t.Fatalf("a good read: %s", buf.Bytes())
	}
	readErr = errors.New("EPERM")
	for range 2 {
		if c.synced() {
			t.Fatal("a failed read reads as synced")
		}
	}
	if got := lines(t, buf.Bytes(), "authority.clock"); len(got) != 1 || got[0]["outcome"] != float64(store.KindError) {
		t.Fatalf("lines %v, want one error line", got)
	}
	readErr = nil
	_ = c.synced()
	readErr = errors.New("EPERM")
	_ = c.synced()
	if got := lines(t, buf.Bytes(), "authority.clock"); len(got) != 2 {
		t.Fatalf("%d lines after a recovery and a second failure, want 2", len(got))
	}
	checkLogConvention(t, buf.Bytes())
}

// authority.notify's code names the state, from a closed set.
func TestNotifyCode(t *testing.T) {
	for state, want := range map[string]string{"READY=1": "ready", "WATCHDOG=1": "watchdog", "STOPPING=1": "stopping", "X=1": ""} {
		if got := notifyCode(state); got != want {
			t.Errorf("notifyCode(%q) = %q, want %q", state, got, want)
		}
	}
}
