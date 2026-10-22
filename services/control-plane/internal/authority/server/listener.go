package server

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/tlspin"
)

// The listener budgets (DoR:1621, D-98, D-107), the handshake deadline
// (B2-D24, = ReadHeaderTimeout) and the refusal summary interval (N5).
const (
	preHandshakeBudget  = 32
	authenticatedBudget = 16
	handshakeTimeout    = 5 * time.Second
	// refusalSummaryEvery bounds the listener's unauthenticated log volume at
	// one line per refusal code per interval, ten codes, however many
	// strangers connect: about 30 lines in journald's default window of
	// 10000 lines per 30 s, which the authority's other lines share. A longer
	// interval buys nothing against that limit, and 10 s puts a refusal's line
	// out before the CP's next 20 s poll.
	refusalSummaryEvery = 10 * time.Second
)

// The codes the listener adds to tlspin's reasons (B2-D9). accept and
// linger count the listener's own faults, not a peer's, so they show in the
// same summary line rather than nowhere.
const (
	codeHandshake = "handshake" // the handshake failed or outran its deadline
	codeBudget    = "budget"    // every authenticated slot was held
	codeAccept    = "accept"    // the raw Accept failed and is being retried
	codeLinger    = "linger"    // a refused connection could not be reset (SO_LINGER 0)
)

// listener is the in-tree semaphore net.Listener (no x/net/netutil), and it
// owns client authentication for the authority (E1, 2026-10-16). Accept
// returns a connection only when all three hold, in this order:
//
//  1. The TLS handshake completed. cfg's VerifyConnection checks only the
//     shape of the one certificate the peer presents, and TLS 1.3
//     CertificateVerify proves the peer holds that certificate's key.
//  2. check accepted the certificate's pin (HW's cp_tls_pins, not in
//     revoked_keys). It runs only after the handshake has returned: Go's TLS
//     1.3 server runs VerifyConnection before CertificateVerify, so a check
//     there ran, revoked_keys read included, for a peer that cannot sign.
//  3. An authenticated slot was free.
//
// A connection holds a pre-handshake slot from the raw Accept until its check
// returns, so the check's revoked_keys read is bounded by the 32, and an
// authenticated slot from then until it closes. A stranger never holds an
// authenticated slot, and a warm CP connection never holds a pre-handshake one.
//
// Accepted residual (S5): nothing caps the pre-handshake slots per source.
// One source can hold all 32 for handshakeTimeout each, again and again, and
// a CP's dial (≈2 s timeout) fails meanwhile. DoR:1621's budget bounds what it
// holds, and the host firewall of #3593 closes the port to all but the CPs.
type listener struct {
	raw       net.Listener
	cfg       *tls.Config
	check     func(pin []byte) error
	timeout   time.Duration
	sum       *refusals
	pre, auth chan struct{}
	ready     chan net.Conn
	slots     sync.Map        // *tls.Conn holding an authenticated slot → struct{}
	done      <-chan struct{} // closed by Close; acceptLoop and serveConn take the context itself
	cancel    context.CancelFunc
	wg        sync.WaitGroup // acceptLoop and every connection it took, check included (B2-D34)
}

// newListener serves raw under cfg. raw must yield *net.TCPConn: a refusal
// resets the connection through SetLinger(0), and a connection without it is
// closed plainly and counted as linger (see abort).
//
// check judges a peer's pin once the handshake has proved possession of its
// key. Any non-nil error refuses; a tlspin.Reason names the refusal in the log,
// and any other error, an empty Reason included, is logged as store. check
// must bound its own work, because Close does not interrupt it and wait waits
// for it. It must not panic: a panic in a connection's goroutine ends the
// process, which fails closed but lets any key holder stop the authority.
func newListener(raw net.Listener, cfg *tls.Config, log *slog.Logger, timeout time.Duration,
	check func(pin []byte) error) *listener {
	return newListenerEvery(raw, cfg, log, timeout, check, refusalSummaryEvery)
}

func newListenerEvery(raw net.Listener, cfg *tls.Config, log *slog.Logger, timeout time.Duration,
	check func(pin []byte) error, every time.Duration) *listener {
	if check == nil {
		panic("server: a listener needs a pin check") // without one it could admit no peer
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &listener{raw: raw, cfg: cfg, check: check, timeout: timeout, sum: newRefusals(log, every),
		pre: make(chan struct{}, preHandshakeBudget), auth: make(chan struct{}, authenticatedBudget),
		ready: make(chan net.Conn), done: ctx.Done(), cancel: cancel}
	l.wg.Add(1)
	go l.acceptLoop(ctx)
	return l
}

// acceptLoop takes a pre-handshake slot before each raw Accept, so at most 32
// connections are between Accept and the end of their check; further
// connections wait in the kernel backlog. It is itself counted in wg, so its
// Add for each connection never races wait.
func (l *listener) acceptLoop(ctx context.Context) {
	defer l.wg.Done()
	for {
		select {
		case l.pre <- struct{}{}:
		case <-ctx.Done():
			return
		}
		c, err := l.raw.Accept()
		if err != nil {
			<-l.pre
			if errors.Is(err, net.ErrClosed) {
				if ctx.Err() == nil {
					_ = l.Close() // closed under us: Accept must not wait for ever
				}
				return
			}
			l.sum.add(codeAccept)             // a listener that only fails shows in the summary
			time.Sleep(10 * time.Millisecond) // EMFILE and the like: back off, never spin
			continue
		}
		l.wg.Add(1)
		go l.serveConn(ctx, c)
	}
}

// serveConn takes one connection from the raw Accept to Accept on l. Its
// pre-handshake slot returns once the handshake has failed or the check has
// run, whatever the outcome.
func (l *listener) serveConn(ctx context.Context, raw net.Conn) {
	defer l.wg.Done()
	tc := tls.Server(raw, l.cfg)
	hctx, cancel := context.WithTimeout(ctx, l.timeout) // cancelled by Close: cuts the handshake short
	err := tc.HandshakeContext(hctx)
	cancel()
	if err != nil {
		<-l.pre
		if !errors.Is(err, context.Canceled) { // Canceled is Close cutting it short, not a refusal
			l.sum.add(reasonOf(err, codeHandshake))
		}
		_ = raw.Close() // a failed handshake ends as TLS ends it, its alert already sent
		return
	}
	err = l.judge(tc)
	<-l.pre
	if err != nil { // admission turns on the error alone; its code only names it (CWE-636)
		l.refuse(raw, reasonOf(err, string(tlspin.ReasonStore)))
		return
	}
	l.admit(raw, tc)
}

// judge is the post-handshake check (E1): possession is proved, so the pin is
// judged now. A non-nil error refuses the peer.
func (l *listener) judge(tc *tls.Conn) error {
	peers := tc.ConnectionState().PeerCertificates
	if len(peers) != 1 {
		return tlspin.ReasonChainLen // cfg refuses this shape; never pin around it
	}
	return l.check(tlspin.Pin(peers[0]))
}

// admit takes an authenticated slot for tc and hands it to Accept. A full
// budget refuses the connection, never queues it.
func (l *listener) admit(raw net.Conn, tc *tls.Conn) {
	select {
	case l.auth <- struct{}{}:
	default:
		l.refuse(raw, codeBudget)
		return
	}
	l.slots.Store(net.Conn(tc), struct{}{})
	select {
	case l.ready <- tc:
	case <-l.done:
		l.release(tc)
		l.abort(raw)
	}
}

// refuse counts code, then drops a connection whose handshake completed.
// Every such refusal (unpinned, revoked, store, budget) closes the same way,
// so the peer cannot tell them apart (E1 ruling 3). Counting first means a
// peer that sees the reset has been counted. chain_len reaches here only
// through the post-handshake length guard, which is unreachable defence: the
// TLS config's VerifyConnection refuses a chain of any other length with a
// TLS alert during the handshake (DoR §3.3, as built).
func (l *listener) refuse(raw net.Conn, code string) {
	l.sum.add(code)
	l.abort(raw)
}

// abort closes raw with a reset (SO_LINGER 0) and sends nothing first: never
// tls.Conn.Close, whose close_notify gives a refused peer bytes. A plain
// close sends a FIN or a reset depending on whether the peer's first request
// arrived before the check returned, and that would leak the check's latency:
// a revoked_keys read takes longer than an unpinned miss. A connection that
// cannot be reset is still closed, and counted as linger, so the leak cannot
// return unnoticed.
func (l *listener) abort(raw net.Conn) {
	s, ok := raw.(interface{ SetLinger(sec int) error })
	if !ok || s.SetLinger(0) != nil {
		l.sum.add(codeLinger)
	}
	_ = raw.Close()
}

// tlsReasons are the codes reasonOf forwards: the six tlspin.Reason
// constants, and nothing else, so the summary's code set, and with it the
// log volume refusalSummaryEvery bounds, is fixed by construction, whatever a
// check returns.
var tlsReasons = map[tlspin.Reason]bool{
	tlspin.ReasonChainLen: true, tlspin.ReasonNotP384: true, tlspin.ReasonEKU: true,
	tlspin.ReasonUnpinned: true, tlspin.ReasonRevoked: true, tlspin.ReasonStore: true,
}

// reasonOf names a refusal for the log: err's tlspin.Reason when it is one of
// tlsReasons, or fallback when it carries none, an empty one or any other. It
// never decides admission.
func reasonOf(err error, fallback string) string {
	var r tlspin.Reason
	if errors.As(err, &r) && tlsReasons[r] {
		return string(r)
	}
	return fallback
}

// release returns c's authenticated slot, once. http.Server's ConnState
// calls it at StateClosed and StateHijacked.
func (l *listener) release(c net.Conn) {
	if _, ok := l.slots.LoadAndDelete(c); ok {
		<-l.auth
	}
}

// Accept returns the next connection whose handshake and check passed. It
// holds an authenticated slot until release.
func (l *listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ready:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting, cuts every handshake in flight short and flushes the
// refusal summary. A check already running finishes first; wait covers it. A
// second Close returns the raw listener's error and changes nothing.
func (l *listener) Close() error {
	l.cancel()
	err := l.raw.Close()
	l.sum.close()
	return err
}

// wait returns once the accept loop and every connection it took have
// returned, each check included, then flushes the refusals counted after
// Close. A check reads revoked_keys, so serve releases its lock only after
// this (B2-D34). Call it after Close.
func (l *listener) wait() {
	l.wg.Wait()
	l.sum.flush()
}

func (l *listener) Addr() net.Addr { return l.raw.Addr() }

// refusals aggregates the listener's refusals into one line per code per
// interval (N5), so a flood of strangers cannot push the authority's other
// lines out through journald's rate limit. Codes come from a closed set
// (tlspin's reasons and the listener's four), so counts stays small. A line
// carries op, outcome, code and count, never the certificate, the peer's
// address or its pin (DoR §3.6). outcome is store.KindRefused as an int, the
// type every authority line gives that key.
type refusals struct {
	log    *slog.Logger
	every  time.Duration
	mu     sync.Mutex
	counts map[string]uint64
	timer  *time.Timer // armed by the first refusal after a flush; never armed once closed
	closed bool
}

func newRefusals(log *slog.Logger, every time.Duration) *refusals {
	return &refusals{log: log, every: every, counts: map[string]uint64{}}
}

// add counts one refusal. The first after a flush arms the interval timer.
func (r *refusals) add(code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[code]++
	if r.timer == nil && !r.closed {
		r.timer = time.AfterFunc(r.every, r.tick)
	}
}

func (r *refusals) tick() {
	r.mu.Lock()
	r.timer = nil
	counts := r.takeLocked()
	r.mu.Unlock()
	r.emit(counts)
}

// close stops the timer for good and flushes. Nothing is armed afterwards,
// so no goroutine outlives it; flush emits what is counted later.
func (r *refusals) close() {
	r.mu.Lock()
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	counts := r.takeLocked()
	r.mu.Unlock()
	r.emit(counts)
}

func (r *refusals) flush() {
	r.mu.Lock()
	counts := r.takeLocked()
	r.mu.Unlock()
	r.emit(counts)
}

func (r *refusals) takeLocked() map[string]uint64 {
	if len(r.counts) == 0 {
		return nil
	}
	counts := r.counts
	r.counts = map[string]uint64{}
	return counts
}

// emit logs outside the lock, so a slow journald write never blocks add.
func (r *refusals) emit(counts map[string]uint64) {
	for _, code := range slices.Sorted(maps.Keys(counts)) {
		r.log.Info("authority.tls", "op", "authority.tls", "outcome", int(store.KindRefused), "code", code,
			"count", counts[code])
	}
}
