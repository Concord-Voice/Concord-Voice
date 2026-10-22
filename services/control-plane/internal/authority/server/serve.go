package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/tlspin"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// shutdownTimeout bounds the graceful stop: the write timeout, so an
// in-flight reply can finish. A var so a test can shorten it (B2-D34).
var shutdownTimeout = 15 * time.Second

// The http.Server's other DoR:1624 values. ReadHeaderTimeout and ReadTimeout
// are http.go's readHeaderTimeout and readTimeout, which bodyDeadline reads.
const (
	writeTimeout   = 15 * time.Second
	idleTimeout    = 60 * time.Second
	maxHeaderBytes = 8 << 10
)

var (
	errDepsIncomplete = errors.New("server: Deps is missing a dependency")
	errNoCause        = errors.New("server: serve was stopped with no cause")
	errNodeRSK        = errors.New("this node's RSK is not in the high-water delegation (D-223)")
	errNodeTLSKey     = errors.New("this node's TLS key is not in the high-water delegation (D-223)")
)

// Deps is everything Run needs, already open. Open builds it from the
// environment; the transport suite builds it over the harness (B2-D22).
type Deps struct {
	DB        *sql.DB // the authority_app pool; every startup check and the loops read it
	RSK       signer.Signer
	TLSKey    *ecdsa.PrivateKey
	Anchors   []byte
	TrustFile []byte
	Listen    net.Listener // must yield *net.TCPConn (newListener)
	Statfs    func() (avail, total uint64, err error)
	Clock     func() bool
	Notify    func(string)
	Log       *slog.Logger
	Hooks     store.Hooks            // tests only (InRecovery, DBNow); Run sets Statfs
	Probe     store.ReplicationProbe // tests only; nil is the SQL probe
}

// check refuses a Deps Run cannot serve with, before any I/O. A nil one would
// otherwise panic later, or, for Notify or Log, mid-serve.
func (d Deps) check() error {
	switch {
	case d.DB == nil, d.RSK == nil, d.TLSKey == nil, d.Listen == nil:
		return errDepsIncomplete
	case d.Statfs == nil, d.Clock == nil, d.Notify == nil, d.Log == nil:
		return errDepsIncomplete
	}
	return nil
}

// Run is serve: the startup sequence, then the HTTP server and the loops
// until ctx ends (nil) or a loop stops it (an *ExitError). Every refusal to
// start is an *ExitError (B2-D8). The order is the DoR's (§3.3, DoR:1652;
// §3.2.4, DoR:1466): the role, the serve lock, the schema under it, the
// store, the trust, this node's keys, then serving. A stop asked for during
// startup (ctx ended, a SIGTERM) is a clean stop, nil, whatever step it cut
// short: an exit 1 with reason=config would read as a fault and fail the
// unit. A step's own deadline (within) still refuses.
func Run(ctx context.Context, d Deps) error {
	if err := d.check(); err != nil {
		return RefuseAt(StepDeps, ReasonConfig, err)
	}
	if err := Within(ctx, func(c context.Context) error { return checkRole(c, d.DB) }); err != nil {
		return stopped(ctx, err)
	}
	var lock *store.ServeLock
	err := Within(ctx, func(c context.Context) (err error) {
		lock, err = store.AcquireServeLock(c, d.DB) // the lock's session outlives c: only the acquiring query uses it
		return err
	})
	if errors.Is(err, store.ErrServeLockHeld) {
		return RefuseAt(StepLock, ReasonLock, err)
	}
	if err != nil {
		return stopped(ctx, RefuseAt(StepLock, ReasonConfig, err))
	}
	defer lock.Release()
	// Under the lock: migrate tries the same key, so the schema cannot change
	// between this check and serving (B2-D21). It reads through d.DB, the
	// authority_app pool loops.schema reads on a promotion, so a grant the app
	// role lacks is a refusal here, never a node that stays a standby for
	// ever because every promotion's check fails to read.
	if err := Within(ctx, func(c context.Context) error { return store.CheckSchema(c, d.DB) }); err != nil {
		return stopped(ctx, RefuseAt(StepSchema, ReasonConfig, err))
	}
	n, err := build(ctx, d)
	if err != nil {
		return stopped(ctx, err)
	}
	return n.serve(ctx, lock)
}

// stopped is a startup step's refusal, or nil when ctx has ended: the step
// failed because a stop was asked for, and that is not a refusal to start.
// A step whose own deadline expired leaves ctx live, so it still refuses.
func stopped(ctx context.Context, err error) error {
	select {
	case <-ctx.Done():
		return nil
	default:
		return err
	}
}

// Within runs one startup query under its own store.RequestDeadlineTx, read
// at call time (M2); cmd/account-authority's migrate runs its steps through
// it too. dialTimeout bounds a dial, but a database that authenticates and
// then wedges a query would hold Run until systemd's
// TimeoutStartSec. An expired deadline is the step's own error, so it keeps
// the step's exit reason. The role poll before READY=1 is bounded by the
// loops (roleEvery), not here.
func Within(ctx context.Context, step func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, store.RequestDeadlineTx)
	defer cancel()
	return step(ctx)
}

// checkRole is the startup role check (DoR:1652).
func checkRole(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return RefuseAt(StepRole, ReasonConfig, err)
	}
	err = store.CheckAppRole(ctx, conn)
	_ = conn.Close()
	if err != nil {
		return RefuseAt(StepRole, ReasonConfig, err)
	}
	return nil
}

// node is a serve whose startup checks have passed.
type node struct {
	d       Deps
	st      *store.Store
	srv     *Server
	disk    *diskWatcher
	cert    tls.Certificate
	pins    [][]byte        // HW's cp_tls_pins; pinCheck copies them, fixed until restart (D-166)
	pending bool            // B2-D33: a standby holding file trust it did not install
	mark    store.TrustMark // the installed trust the view was built from (Sec-M2)
}

// build opens the store on one Admission, which the disk watcher feeds too
// (B2-D18), loads the trust and installs it on the leader (B2-D20), checks
// this node's keys against HW (D-223) and mints the certificate.
func build(ctx context.Context, d Deps) (*node, error) {
	adm := store.NewAdmission()
	hooks, statfs := d.Hooks, d.Statfs
	hooks.Statfs = func() (uint64, error) { a, _, err := statfs(); return a, err }
	st, err := store.Open(store.Config{DB: d.DB, Signer: d.RSK, Anchors: d.Anchors, Probe: d.Probe,
		Admission: adm, Hooks: hooks, Logger: d.Log})
	if err != nil {
		return nil, RefuseAt(StepStore, ReasonTrust, err) // with every pointer set, Open refuses only anchors that do not decode
	}
	view, err := loadTrust(ctx, st, d.TrustFile)
	if err != nil {
		return nil, err
	}
	if err := checkNodeKeys(view.HW, d.RSK.PublicKey(), d.TLSKey); err != nil {
		return nil, RefuseAt(StepKeys, ReasonTrust, withIgnored(err, view.Ignored))
	}
	cert, err := tlspin.Mint(d.TLSKey, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, RefuseAt(StepCert, ReasonConfig, err)
	}
	disk := newDiskWatcher(statfs, adm, d.Log)
	return &node{d: d, st: st, srv: newServer(st, d.Log, disk.ready.Load, d.Clock), disk: disk, cert: cert,
		pins: view.HW.CPTLSPins, pending: view.Pending, mark: view.Mark}, nil
}

// loadTrust reads the role and loads the trust, installing it on the leader
// (B2-D20). D-223's reuse bullet runs inside LoadTrust, before its commit
// (N2), so nothing here repeats it.
func loadTrust(ctx context.Context, st *store.Store, file []byte) (store.TrustView, error) {
	var leader bool
	err := Within(ctx, func(c context.Context) (err error) {
		leader, err = st.Leader(c)
		return err
	})
	if err != nil {
		return store.TrustView{}, RefuseAt(StepLeader, ReasonConfig, err)
	}
	var view store.TrustView
	err = Within(ctx, func(c context.Context) (err error) {
		view, err = st.LoadTrust(c, file, leader)
		return err
	})
	if err != nil {
		return store.TrustView{}, RefuseAt(StepTrust, trustReason(err), err)
	}
	return view, nil
}

// trustRefusals are LoadTrust's refusals of the trust itself (B2-D10, N2).
var trustRefusals = []error{store.ErrRealmMismatch, store.ErrTrustNotYetValid, store.ErrTrustUnusable, store.ErrRSKReused}

// trustReason maps a LoadTrust error: the trust's own refusals are trust; a
// missing meta row and a database error are config.
func trustReason(err error) string {
	for _, e := range trustRefusals {
		if errors.Is(err, e) {
			return ReasonTrust
		}
	}
	return ReasonConfig
}

// withIgnored names how many trust-file items did not verify on a D-223
// refusal: an RSK missing from HW because the delegation listing it
// was forged, or signed by keys the anchors do not hold, reads as a key
// fault otherwise. It carries the count alone, never an item.
func withIgnored(err error, n int) error {
	if n == 0 {
		return err
	}
	return fmt.Errorf("%w (%d trust-file items did not verify)", err, n)
}

func listed(set [][]byte, k []byte) bool {
	return slices.ContainsFunc(set, func(p []byte) bool { return bytes.Equal(p, k) })
}

// checkNodeKeys is D-223 for this node: its RSK and its TLS key are in the
// merged high-water delegation (B2-D20). The reuse bullet is the store's.
func checkNodeKeys(hw ac.Delegation, rsk []byte, tlsKey *ecdsa.PrivateKey) error {
	pt, err := tlsKey.PublicKey.ECDH()
	if err != nil {
		return errNodeTLSKey
	}
	switch {
	case !listed(hw.RSKPublicKeys, rsk):
		return errNodeRSK
	case !listed(hw.AuthorityTLSKeys, pt.Bytes()):
		return errNodeTLSKey
	}
	return nil
}

// pinCheck is the listener's check (E1, DoR:1633), run once the handshake
// has proved possession: the pin is in HW's cp_tls_pins, copied here and
// fixed until restart (D-166), and then not in revoked_keys, read uncached
// on its own read deadline (D-207). tlspin.ServerConfig judges only the
// certificate's shape. An unpinned peer costs no read. A read that fails, or
// panics, refuses as store: the listener requires a check that never panics.
// A panic is logged as one fixed authority.panic line, op tls.
func pinCheck(pins [][]byte, revoked func(context.Context, []byte) (bool, error), log *slog.Logger) func([]byte) error {
	own := make([][]byte, len(pins))
	for i, p := range pins {
		own[i] = bytes.Clone(p)
	}
	return func(pin []byte) (err error) {
		if !listed(own, pin) {
			return tlspin.ReasonUnpinned
		}
		defer func() {
			if recover() != nil {
				logPanic(log, opTLS)
				err = tlspin.ReasonStore // the panic value is never logged
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), store.RequestDeadlineRead)
		defer cancel()
		r, rerr := revoked(ctx, pin)
		switch {
		case rerr != nil:
			return tlspin.ReasonStore
		case r:
			return tlspin.ReasonRevoked
		}
		return nil
	}
}

// stopper is serve's one stop. The first fatal records its error
// and cancels serve's context; the loops keep polling until that cancel
// lands, and a later fatal changes nothing. A fatal with no error is still a
// failure: exit 0 would read as a clean stop that systemd need not restart.
type stopper struct {
	once   sync.Once
	cancel func()
	mu     sync.Mutex
	err    error
}

func newStopper(cancel func()) *stopper { return &stopper{cancel: cancel} }

func (s *stopper) fatal(err error) {
	if err == nil {
		err = refuse(ReasonConfig, errNoCause)
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		s.cancel()
	})
}

// result is the first fatal's error, or nil while there is none.
func (s *stopper) result() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// loops are serve's tick and role poll (§3.2.4) at the store's intervals,
// 30 s and 5 s (DoR:199, 1466). The schema check a promotion runs reads
// d.DB, as Run's did (N4); pending and the trust mark a promotion
// compares are the start-time view's (B2-D33, Sec-M2).
func (n *node) loops(lock *store.ServeLock, fatal func(error)) *loops {
	db := n.d.DB
	return &loops{b: n.st, lock: lock, srv: n.srv, disk: n.disk, notify: n.d.Notify, log: n.d.Log, fatal: fatal,
		schema: func(ctx context.Context) error { return store.CheckSchema(ctx, db) },
		trust:  n.st.InstalledTrustMark, mark: n.mark,
		tickEvery: store.TickInterval, roleEvery: store.RolePollInterval, pending: n.pending}
}

// serve serves until ctx ends or a fatal stops it. Before READY=1 (carry
// 11), with pending and the schema check already wired: one disk sample, so
// disk_ready is the mount's from the first request rather than the
// Admission's optimistic start for a whole tick; and one role poll, the only
// writer of the role atomic, so a node that is the leader at start becomes
// one now, through the flip's pending and schema checks. A fatal on that poll
// ends serve before the listener exists. Then the listener with HW's pins,
// the HTTP server and the loops, and READY=1, which does not wait for
// leadership (DoR:1466).
func (n *node) serve(ctx context.Context, lock *store.ServeLock) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := newStopper(cancel)
	lp := n.loops(lock, stop.fatal)
	n.disk.update()
	lp.pollRole(ctx)
	if err := stop.result(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil // stopped during the first poll: no listener, no READY=1
	}
	n.logPending()
	l := newListener(n.d.Listen, tlspin.ServerConfig(n.cert), n.d.Log, handshakeTimeout, pinCheck(n.pins, n.st.Revoked, n.d.Log))
	n.srv.refused = l.sum.add // before Serve: every authority.tls line is the listener's summary (M2)
	hs := newHTTPServer(n.srv.Handler(), l.release, n.d.Log)
	lp.start(ctx)
	served := make(chan error, 1)
	go func() { served <- hs.Serve(l) }()
	n.d.Notify("READY=1")
	select {
	case err := <-served:
		stop.fatal(RefuseAt(StepServe, ReasonConfig, err))
	case <-ctx.Done():
	}
	n.shutdown(hs, l, lp)
	return stop.result()
}

// logPending makes B2-D33's standby visible: it holds file trust
// it has not installed, so a promotion stops it with reason=trust and its
// restart, as the leader, installs that trust. Its leader refusing the same
// file shows on that node as its own authority.exit; this line is the
// standby's half, so an operator sees both.
func (n *node) logPending() {
	if n.pending {
		n.d.Log.Info("authority.trust", "op", "authority.trust", "outcome", int(store.KindRefused), "code", "pending")
	}
}

// shutdown is B2-D34's order. Run releases the serve lock only after it
// returns, so migrate (D-171) never runs under a request, a pin check, a
// tick or a purge:
//  1. STOPPING=1, then Shutdown: no new connection, idle ones closed, and up
//     to shutdownTimeout for the active ones. Past it every connection is
//     closed, but a handler that ignores its context runs on.
//  2. The drain lock: every request has returned, and none can start.
//  3. The listener's wait: every connection it took has returned. Close cuts
//     a handshake short but not a pin check already running, which reads
//     revoked_keys on its own RequestDeadlineRead, so this can
//     wait one read deadline. The checks run at once, at most
//     preHandshakeBudget of them, so it is one deadline, not one per check.
//  4. The loops' wait: the tick, the role poll and any purge, each on serve's
//     context, which is cancelled by now.
func (n *node) shutdown(hs *http.Server, l *listener, lp *loops) {
	n.d.Notify("STOPPING=1")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		if sctx.Err() != nil {
			n.d.Log.Info("authority.shutdown", "op", "authority.shutdown", "outcome", int(store.KindError), "code", "timeout")
		}
		_ = hs.Close() // a request outlived the budget and may still commit: close its connection, then wait for it
	}
	n.srv.drain.Lock()
	_ = l.Close() // Shutdown closed it already; a second Close is harmless
	l.wait()
	lp.wait()
}

// newHTTPServer is DoR:1624: HTTP/1.1 only, the four timeouts and the header
// cap. ReadHeaderTimeout and ReadTimeout are http.go's constants, because
// bodyDeadline derives its bound from them (C3), and a zero
// ReadHeaderTimeout would fall back to ReadTimeout and break it. net/http's
// own OPTIONS * handler is off, so that request reaches Handler and its peer
// check like any other (R1); without that, a revoked pin's keep-alive
// connection would still be answered. ConnState returns a closed or hijacked
// connection's authenticated slot.
func newHTTPServer(h http.Handler, release func(net.Conn), log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:                      h,
		DisableGeneralOptionsHandler: true,
		ReadHeaderTimeout:            readHeaderTimeout,
		ReadTimeout:                  readTimeout,
		WriteTimeout:                 writeTimeout,
		IdleTimeout:                  idleTimeout,
		MaxHeaderBytes:               maxHeaderBytes,
		TLSNextProto:                 map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ErrorLog:                     httpErrorLog(log),
		ConnState: func(c net.Conn, s http.ConnState) {
			if s == http.StateClosed || s == http.StateHijacked {
				release(c)
			}
		},
	}
}

// httpErrorLog is the http.Server's ErrorLog (S1). net/http writes a peer's
// address into its lines, and a recovered panic's stack, so each line becomes
// one fixed authority.http line that carries neither. It is never nil: a nil
// ErrorLog is the standard logger, which writes that text to stderr, which is
// serve's log.
func httpErrorLog(log *slog.Logger) *stdlog.Logger { return stdlog.New(fixedLine{log}, "", 0) }

// fixedLine discards what net/http wrote and logs one fixed line in its place.
type fixedLine struct{ log *slog.Logger }

func (f fixedLine) Write(p []byte) (int, error) {
	f.log.Info("authority.http", "op", "authority.http", "outcome", int(store.KindError))
	return len(p), nil
}
