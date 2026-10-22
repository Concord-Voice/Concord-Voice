package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
)

var (
	// errPromotedPending is B2-D33's stop: the restart installs the trust.
	errPromotedPending = errors.New("promoted with trust this node has not installed; restarting to install it (B2-D33)")
	// errNoSchemaCheck is N4's programming error: a promotion with no schema
	// check wired.
	errNoSchemaCheck = errors.New("server: a standby was promoted with no schema or trust check wired")
	// errTrustMoved is Sec-M2's stop: a leader installed trust after this node
	// started, so its pins and key check are stale; the restart reloads them.
	errTrustMoved = errors.New("promoted after the installed trust moved past this node's start; restarting to reload it")
)

// The disk soft floor (DoR:196, LD-25): max(2 GiB, 10 % of the volume),
// re-armed at the floor + 256 MiB. The hard floor is the store's.
const (
	diskSoftMin = 2 << 30
	diskRearm   = 256 << 20
)

// tickBudget bounds one tick's database work: inside TickInterval, and far
// inside WatchdogSec=90.
const tickBudget = 20 * time.Second

// notifyTimeout bounds one sd_notify send. The notifier runs on the tick and
// before READY=1, so an unbounded send would freeze disk_ready, the tick, the
// purge leg and start-up, not only the watchdog it carries.
const notifyTimeout = time.Second

// diskReadySetter takes disk_ready: the shared *store.Admission.
type diskReadySetter interface{ SetDiskReady(ok bool) }

// diskWatcher is disk_ready. It starts ready, as the Admission does, so the
// re-arm band holds only after a drop.
type diskWatcher struct {
	statfs  func() (avail, total uint64, err error)
	adm     diskReadySetter
	log     *slog.Logger
	ready   atomic.Bool
	statErr bool // the last Statfs failed; update alone touches it, from one goroutine at a time
}

func newDiskWatcher(statfs func() (avail, total uint64, err error), adm diskReadySetter, log *slog.Logger) *diskWatcher {
	d := &diskWatcher{statfs: statfs, adm: adm, log: log}
	d.ready.Store(true)
	return d
}

// update reads the mount and sets disk_ready on the shared Admission (B2-D18).
// A Statfs error is not ready, and its edge, the first failed read after a
// good one, is one authority.disk line: otherwise a failing Statfs
// reads exactly as a full disk.
func (d *diskWatcher) update() {
	avail, total, err := d.statfs()
	if err != nil && !d.statErr {
		d.log.Info("authority.disk", "op", "authority.disk", "outcome", int(store.KindError))
	}
	d.statErr = err != nil
	floor := max(uint64(diskSoftMin), total/10)
	ready := d.ready.Load()
	switch {
	case err != nil || avail <= floor:
		ready = false
	case avail >= floor+diskRearm:
		ready = true
	}
	d.ready.Store(ready)
	d.adm.SetDiskReady(ready)
}

// loopBackend is the store surface the loops drive.
type loopBackend interface {
	Tick(ctx context.Context, in store.TickInput) error
	Purge(ctx context.Context) (store.PurgeReport, error)
	PurgeDue(ctx context.Context) (bool, error)
	Leader(ctx context.Context) (bool, error)
}

type pinger interface {
	Ping(ctx context.Context) error
}

// loops are serve's background goroutines: the tick, with the disk watcher
// and the purge leg, and the role poll.
type loops struct {
	b         loopBackend
	lock      pinger
	srv       *Server
	disk      *diskWatcher
	notify    func(string)
	log       *slog.Logger
	fatal     func(error)
	schema    func(context.Context) error                    // N4: store.CheckSchema, run when a standby is promoted
	trust     func(context.Context) (store.TrustMark, error) // Sec-M2: the installed trust's mark, read on a promotion
	mark      store.TrustMark                                // Sec-M2: the start-time view's mark, which trust must equal
	tickEvery time.Duration
	roleEvery time.Duration
	pending   bool // B2-D33: the start-time trust view was Pending
	purging   atomic.Bool
	wg        sync.WaitGroup
	// roleFail is the code of the role read failing now, or "".
	// pollRole alone touches it, and its calls never overlap.
	roleFail string
}

// The authority.role codes: which read kept the node a standby.
const (
	roleCodeLeader = "leader"
	roleCodeSchema = "schema"
	roleCodeTrust  = "trust"
)

// start runs the tick and the role poll until ctx ends.
func (l *loops) start(ctx context.Context) {
	l.wg.Add(2)
	go func() { defer l.wg.Done(); every(ctx, l.tickEvery, l.tick) }()
	go func() { defer l.wg.Done(); every(ctx, l.roleEvery, l.pollRole) }()
}

// wait returns once both loops and any purge have returned.
func (l *loops) wait() { l.wg.Wait() }

func every(ctx context.Context, d time.Duration, f func(context.Context)) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f(ctx)
		}
	}
}

// tick is one iteration of §3.2.4's loop. WATCHDOG=1 is sent here and only
// here, after the work, whatever the database said: a tick that wedges
// starves the watchdog and systemd restarts serve (D-203). A tick cut short
// by serve's stop is no error and logs nothing.
func (l *loops) tick(ctx context.Context) {
	l.disk.update()
	if l.srv.leader.Load() {
		tctx, cancel := context.WithTimeout(ctx, tickBudget)
		if err := l.b.Tick(tctx, store.TickInput{LastPeerContactMs: l.srv.lastPeer.Load()}); err != nil && ctx.Err() == nil {
			l.log.Info("authority.tick", "op", "authority.tick", "outcome", int(store.KindError))
		}
		l.maybePurge(ctx, tctx)
		cancel()
	}
	l.notify("WATCHDOG=1")
}

// maybePurge starts the purge leg on its own goroutine when it is due, at
// most one at a time. Its 20-s budget is the store's (PurgeBudget). A due
// test that fails, and a run that fails, are each one authority.purge error
// line; a run that ends is one KindOK line whose complete is 0 when the
// budget cut it short, so a backlog the budget never clears shows
// (LD-22). The line carries no count. A failure caused by serve's stop logs
// nothing.
// opPurge is the purge leg's log op (DoR §3.6).
const opPurge = "authority.purge"

func (l *loops) maybePurge(ctx, tctx context.Context) {
	due, err := l.b.PurgeDue(tctx)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Info(opPurge, "op", opPurge, "outcome", int(store.KindError))
		}
		return
	}
	if !due || !l.purging.CompareAndSwap(false, true) {
		return
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer l.purging.Store(false)
		rep, err := l.b.Purge(ctx)
		switch {
		case err == nil:
			l.log.Info(opPurge, "op", opPurge, "outcome", int(store.KindOK), "complete", b01(rep.Complete))
		case ctx.Err() == nil:
			l.log.Info(opPurge, "op", opPurge, "outcome", int(store.KindError))
		}
	}()
}

// pollRole is B2-D6 and B2-D21: refresh the role atomic, where an error reads
// as standby, and ping the serve lock's connection, where a failure is
// fatal unless serve is already stopping. A standby promoted with trust it
// has not installed stops instead of becoming a leader that cannot sign
// (B2-D33), and one promoted onto a schema the code was not built for stops
// too (N4); a promotion is a standby-to-leader flip of the atomic.
func (l *loops) pollRole(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, l.roleEvery)
	defer cancel()
	if err := l.lock.Ping(pctx); err != nil {
		if ctx.Err() == nil {
			l.fatal(RefuseAt(StepLock, ReasonLock, err))
		}
		return
	}
	leader, err := l.b.Leader(pctx)
	if err != nil {
		l.roleFailed(pctx, roleCodeLeader)
		leader = false
	}
	if leader && l.pending {
		l.fatal(RefuseAt(StepPromotion, ReasonTrust, errPromotedPending))
		return
	}
	if leader && !l.srv.leader.Load() && !l.promotable(pctx) {
		return
	}
	if err == nil {
		l.roleFail = ""
	}
	l.srv.leader.Store(leader)
}

// roleFailed is one authority.role error line, code the read
// that failed, on the edge into that failure, never once per poll. A read
// failing because serve is stopping (ctx cancelled) logs nothing; one that
// ran out of its own poll deadline does.
func (l *loops) roleFailed(ctx context.Context, code string) {
	if errors.Is(ctx.Err(), context.Canceled) || l.roleFail == code {
		return
	}
	l.roleFail = code
	l.log.Info("authority.role", "op", "authority.role", "outcome", int(store.KindError), "code", code)
}

// promotable is N4 and Sec-M2: whether a standby may become the leader, which
// the startup checks alone cannot say. A migration can be applied while a
// node stands by, and a leader can install trust that replicates here: this
// node's pins and its D-223 key check are its start-time view's, fixed until
// restart (D-166), so a promotion onto moved trust would admit a pin the new
// trust dropped. A schema mismatch is fatal with reason=config, and moved
// trust with reason=trust, the mirror of B2-D33: systemd's restart reloads
// it. A check that could not read leaves the node a standby for this poll,
// and the next poll tries again; the edge into that is one authority.role
// line. A missing check is a programming error and fails loudly.
// In each refusal the atomic is not set.
func (l *loops) promotable(ctx context.Context) bool {
	if l.schema == nil || l.trust == nil {
		l.fatal(RefuseAt(StepPromotion, ReasonConfig, errNoSchemaCheck))
		return false
	}
	code, err := roleCodeSchema, l.schema(ctx)
	if err == nil {
		code, err = roleCodeTrust, l.trustUnmoved(ctx)
	}
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrSchemaSkew) || errors.Is(err, store.ErrSchemaDirty):
		l.fatal(RefuseAt(StepPromotion, ReasonConfig, err))
	case errors.Is(err, errTrustMoved):
		l.fatal(RefuseAt(StepPromotion, ReasonTrust, err))
	default:
		l.roleFailed(ctx, code)
		// Defensive: this branch is reached only from a flip, so the atomic is
		// already false, and pollRole is its only writer. The line pins "a failed
		// read is a standby" here, where it would otherwise rest on that
		// reasoning alone.
		l.srv.leader.Store(false)
	}
	return false
}

// trustUnmoved reads the installed trust's mark under its own deadline
// (within) and returns errTrustMoved when it differs from the start view's.
func (l *loops) trustUnmoved(ctx context.Context) error {
	var now store.TrustMark
	if err := Within(ctx, func(c context.Context) (err error) { now, err = l.trust(c); return err }); err != nil {
		return err
	}
	if now != l.mark {
		return errTrustMoved
	}
	return nil
}

// notifier is sd_notify over the unixgram socket systemd names in
// NOTIFY_SOCKET (hand-written: no go-systemd, AC5). Go maps a leading '@' to
// the abstract namespace. An empty socket is a no-op; a send error is logged,
// never fatal, because a missed WATCHDOG is systemd's to act on. The line's
// code names the state that was not sent.
func notifier(socket string, log *slog.Logger) func(string) {
	if socket == "" {
		return func(string) {
			// No NOTIFY_SOCKET: not run by systemd, so there is no one to tell.
		}
	}
	addr := &net.UnixAddr{Name: socket, Net: "unixgram"}
	return func(state string) {
		c, err := net.DialUnix("unixgram", nil, addr)
		if err == nil {
			err = sendState(c, state)
		}
		if err != nil {
			log.Info("authority.notify", "op", "authority.notify", "outcome", int(store.KindError), "code", notifyCode(state))
		}
	}
}

// notifyCode is authority.notify's code for a state: a closed set, never
// the state's own text.
func notifyCode(state string) string {
	switch state {
	case "READY=1":
		return "ready"
	case "WATCHDOG=1":
		return "watchdog"
	case "STOPPING=1":
		return "stopping"
	}
	return ""
}

// clockWatch is /v1/signer's clock_synced over clockSynced (DoR:1786): a
// read that fails is unsynced, as before, and the edge into failing reads
// is one authority.clock line. A unit that filters adjtimex
// (ProtectClock=, the @clock seccomp set) would otherwise read as a clock
// that never syncs, with nothing to say why. It is read by request
// handlers at once, so its state is atomic.
type clockWatch struct {
	read    func() (bool, error)
	log     *slog.Logger
	failing atomic.Bool
}

func (c *clockWatch) synced() bool {
	ok, err := c.read()
	if err != nil {
		if c.failing.CompareAndSwap(false, true) {
			c.log.Info("authority.clock", "op", "authority.clock", "outcome", int(store.KindError))
		}
		return false
	}
	c.failing.Store(false)
	return ok
}

// sendState writes one datagram to c and closes it, in at most notifyTimeout.
// A deadline that cannot be set leaves the write unbounded, so nothing is
// written and the failure is returned: a missed state is logged, never waited
// on.
func sendState(c net.Conn, state string) error {
	defer func() { _ = c.Close() }()
	if err := c.SetWriteDeadline(time.Now().Add(notifyTimeout)); err != nil {
		return err
	}
	_, err := c.Write([]byte(state))
	return err
}
