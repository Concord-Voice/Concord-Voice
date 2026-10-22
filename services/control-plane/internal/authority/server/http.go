package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/tlspin"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// backend is the store surface the HTTP layer drives.
type backend interface {
	store.Authority
	Signer(ctx context.Context, fill func(*ac.Signer)) store.Outcome
	Revoked(ctx context.Context, digest []byte) (bool, error)
	NoteNonClient503()
}

var _ backend = (*store.Store)(nil)

const (
	// bodyCapBatch is the cap of POST /v1/legacy-population and /v1/reconcile/* (DoR:1607, 1610–1611).
	bodyCapBatch = 1 << 20
	// readHeaderTimeout and readTimeout are serve's http.Server
	// ReadHeaderTimeout and ReadTimeout (DoR:1624). The http.Server must set
	// exactly these: bodyDeadline derives its bound from them (C3).
	readHeaderTimeout = handshakeTimeout // B2-D24: the same 5 s
	readTimeout       = 10 * time.Second
	// opUnrouted is the log's op for a request no §3.3 route takes.
	opUnrouted = "unrouted"
	// opTLS is authority.panic's op for a panic in the listener's pin check.
	opTLS = "tls"
)

var (
	// errRevokedPanicked stands for a revoked_keys read that panicked: a
	// failed read, so the peer is refused (S1).
	errRevokedPanicked = errors.New("server: the revoked_keys read panicked")
	// errBodyDeadline is a body read whose deadline could not be set (M4).
	errBodyDeadline = errors.New("server: the body read deadline could not be set")
)

// submitLayouts is /v1/signer's submit_layouts (B2-D4): the Class-I layouts
// accepted at N and N−1, each with the codec version (ac.Version = 1).
var submitLayouts = []string{"device-submit/1", "import-item/1", "submit/1"}

// Server is serve's HTTP layer over one backend.
type Server struct {
	b                 backend
	log               *slog.Logger
	leader            atomic.Bool   // B2-D6: written by the role loop
	lastPeer          atomic.Uint64 // D-97: ms of the last authenticated request; read by the tick
	diskReady         func() bool
	clockSynced       func() bool
	txDeadline        time.Duration
	readDeadline      time.Duration
	readHeaderTimeout time.Duration // the http.Server's, for bodyDeadline (C3)
	readTimeout       time.Duration // the http.Server's, for bodyDeadline (C3)
	drain             sync.RWMutex  // B2-D34: a read lock per request; shutdown takes the write lock
	// refused counts one checkPeer refusal by its code. serve points it at
	// the listener's aggregator before serving, so every authority.tls line
	// is a summary; newServer's own writes one summary line per
	// refusal, for a Server no listener feeds.
	refused func(code string)
}

func newServer(b backend, log *slog.Logger, diskReady, clockSynced func() bool) *Server {
	s := &Server{b: b, log: log, diskReady: diskReady, clockSynced: clockSynced,
		txDeadline: store.RequestDeadlineTx, readDeadline: store.RequestDeadlineRead,
		readHeaderTimeout: readHeaderTimeout, readTimeout: readTimeout}
	s.refused = func(code string) {
		s.log.Info("authority.tls", "op", "authority.tls", "outcome", int(store.KindRefused), "code", code, "count", 1)
	}
	return s
}

// handler runs one route under the request's deadline. A route that reads a
// request_id from its body sets *rid, which only the log line reads (C2).
type handler func(ctx context.Context, r *http.Request, rid *string) store.Outcome

// record is one request's authority.request line (DoR §3.6), filled in as the
// request goes and written once, by Handler, however the request ends.
// out's zero Kind, outcome 0, is a request dropped unanswered.
type record struct {
	start time.Time
	op    string
	out   store.Outcome
	rid   string
}

// recordKey carries the record from Handler to the route, through the mux.
type recordKey struct{}

// routeHandler is what the mux holds for a route. Handler tells it from the
// mux's own 404, 405 and redirect handlers by its type, never by pattern: for
// a path it would redirect, the mux returns the pattern it matched.
type routeHandler struct {
	s  *Server
	rt route
}

func (h routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.s.serve(w, r, h.rt) }

type route struct {
	pattern string
	op      string // the log's op
	tx      bool   // the transaction deadline; otherwise the read deadline (B2-D7)
	anyRole bool   // answers on a standby: /v1/signer only (B2-D6)
	h       handler
}

// routes is §3.3's endpoint table (DoR:1598–1611).
func (s *Server) routes() []route {
	const acct = "/v1/accounts/{id}/"
	return []route{
		{"POST /v1/transactions", "submit", true, false, s.body(store.BodyCapTx, submitRequestID, s.b.Submit)},
		{"POST /v1/device-transactions", "device-submit", true, false, s.body(store.BodyCapDevice, deviceRequestID, s.b.SubmitDevice)},
		{"GET " + acct + "head", "head", false, false, func(ctx context.Context, r *http.Request, _ *string) store.Outcome {
			return s.b.Head(ctx, r.PathValue("id"))
		}},
		{"GET " + acct + "history", "history", false, false, paged("after_seq", s.b.History)},
		{"GET " + acct + "devices", "device-history", false, false, paged("after_dseq", s.b.Devices)},
		{"POST /v1/status", "status-req", false, false, s.body(store.BodyCapTx, nil, s.b.Statuses)},
		{"GET /v1/signer", "signer", false, true, func(ctx context.Context, _ *http.Request, _ *string) store.Outcome {
			return s.b.Signer(ctx, s.fill)
		}},
		{"POST /v1/legacy-population", "legacy-insert", true, false, s.body(bodyCapBatch, nil, s.noContent(s.b.InsertLegacyPopulation))},
		{"GET /v1/legacy-population", "legacy-read", false, false, after(s.b.LegacyPopulation)},
		{"GET /v1/pending-heads", "pending-heads", false, false, after(s.b.PendingHeads)},
		{"POST /v1/reconcile/required", "reconcile-required", true, false, s.body(bodyCapBatch, nil, s.noContent(s.b.ReconcileRequired))},
		{"POST /v1/reconcile/import", "reconcile-import", true, false, s.body(bodyCapBatch, nil, s.operator(s.b.ReconcileImport))},
		{"POST /v1/reconcile/hold", "reconcile-hold", true, false, s.body(bodyCapBatch, nil, s.operator(s.b.ReconcileHold))},
	}
}

// Handler is serve's http.Handler: one authority.request line per request,
// the drain read lock (B2-D34), the route's deadline from arrival
// (B2-D7) with the body read bounded by bodyDeadline, the peer check on every
// request, then the route. A request no route takes is answered here, after
// the peer check, so the mux never writes. It reads the deadlines when called.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.Handle(rt.pattern, routeHandler{s: s, rt: rt})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &record{start: time.Now(), op: opUnrouted}
		defer s.logRequest(rec)
		rt, routed := routeOf(mux, r)
		if routed {
			rec.op = rt.op
		}
		if !s.drain.TryRLock() {
			panic(http.ErrAbortHandler) // shutdown has begun
		}
		defer s.drain.RUnlock()
		dl := rec.start.Add(s.deadlineOf(rt, routed))
		ctx, cancel := context.WithDeadline(context.WithValue(r.Context(), recordKey{}, rec), dl)
		defer cancel()
		r = r.WithContext(ctx)
		r.Body = &boundedBody{ReadCloser: r.Body, rc: http.NewResponseController(w), dl: s.bodyDeadline(rec.start, dl)}
		s.checkPeer(r)
		if !routed {
			s.reply(w, rec, unroutedOut(mux, r))
			return
		}
		mux.ServeHTTP(w, r) // sets the path values, then serve
	})
}

// logRequest writes rec as the request's one authority.request line. Handler
// defers it, so a request dropped unanswered by a panic is logged too; it
// recovers nothing. latency_ms runs from Handler entry, the peer check
// included (M7). The line never names the path (DoR §3.6).
func (s *Server) logRequest(rec *record) {
	s.log.Info("authority.request", "op", rec.op, "outcome", int(rec.out.Kind), "code", rec.out.Code,
		"request_id", rec.rid, "latency_ms", time.Since(rec.start).Milliseconds())
}

// routeOf is the §3.3 route the mux would run r on, if any. HEAD has none:
// Go's GET patterns match it, but §3.3 lists no HEAD (M5).
func routeOf(mux *http.ServeMux, r *http.Request) (route, bool) {
	if r.Method == http.MethodHead {
		return route{}, false
	}
	h, _ := mux.Handler(r)
	rh, ok := h.(routeHandler)
	return rh.rt, ok
}

// deadlineOf is the request's deadline (B2-D7): a route's, or the read
// deadline for a request no route takes.
func (s *Server) deadlineOf(rt route, routed bool) time.Duration {
	if routed && rt.tx {
		return s.txDeadline
	}
	return s.readDeadline
}

// unroutedOut is the answer to a request no route takes (M5, M6): 405 when
// its path is a route's under another method, HEAD included, otherwise 404,
// a path the mux would redirect among them. Either is malformed, since §3.3
// defines no such request.
func unroutedOut(mux *http.ServeMux, r *http.Request) store.Outcome {
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		probe := r.WithContext(r.Context())
		probe.Method = m
		if _, ok := routeOf(mux, probe); ok {
			return errOut(http.StatusMethodNotAllowed, ac.ErrorMalformed)
		}
	}
	return errOut(http.StatusNotFound, ac.ErrorMalformed)
}

// bodyDeadline is when a body read is cut (B2-D7, C3): the earlier of the
// route's deadline and the earliest instant net/http's own whole-request read
// deadline can fall, so arming it extends net/http's by at most the dispatch
// gap. net/http's is ReadTimeout from when it began reading the request,
// which the Handler cannot see. The headers end within ReadHeaderTimeout of
// that instant, so it is no earlier than entry − ReadHeaderTimeout, and
// net/http's deadline no earlier than entry + ReadTimeout − ReadHeaderTimeout,
// up to the dispatch gap: the time between net/http reading the last header
// byte and the Handler running. On DoR:1624's values a transaction's body is
// cut 5 s after entry, not 8; a read route keeps its 4 s.
func (s *Server) bodyDeadline(entry, route time.Time) time.Time {
	if whole := entry.Add(s.readTimeout - s.readHeaderTimeout); whole.Before(route) {
		return whole
	}
	return route
}

// boundedBody makes the route's deadline bound the body read too (B2-D7):
// io.ReadAll does not watch the context, and http.Server's own ReadTimeout
// runs from before the headers. The first read moves the connection's read
// deadline to dl, bodyDeadline's bound, and reaching EOF clears it again
// (net/http's background read clears it at EOF as well; clearing here keeps
// that off an internal). It must not outlast the read: Go cancels a
// connection's context after any failed read, its background read included,
// and every later request on that connection then starts cancelled (measured
// on go1.26.6; the keep-alive tests re-check it on go.mod's toolchain). So it
// is armed on the first read and never earlier, since a request with no body
// has its background read running before the handler starts. A deadline
// that cannot be set, armed or cleared, fails the read with errBodyDeadline
// (M4): unarmed, nothing bounds the body; uncleared, it would outlast the
// read. A clear can fail only once the connection is closed on this side, so
// no later request meets the deadline it leaves. On the 413 path the
// handler stops one byte past the cap, before EOF, so the armed deadline
// survives into net/http's own discard of the unread body after the handler
// returns. That is benign: a discard the deadline cuts short closes the
// connection, as net/http does with any body it cannot finish, and one that
// completes leaves no stale deadline, because net/http sets a fresh read
// deadline before it reads the next request.
type boundedBody struct {
	io.ReadCloser
	rc    *http.ResponseController
	dl    time.Time
	armed bool
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if !b.armed {
		if b.rc.SetReadDeadline(b.dl) != nil {
			return 0, errBodyDeadline
		}
		b.armed = true
	}
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) && b.rc.SetReadDeadline(time.Time{}) != nil {
		return n, errBodyDeadline
	}
	return n, err
}

// checkPeer is B2-D25 (D-207): the connection's pin is read against
// revoked_keys before every request, uncached, on the request's own deadline.
// A revoked pin or a failed read aborts with no reply; http.Server recovers
// ErrAbortHandler and closes the connection. It runs outside the route's
// recover, so nothing converts it. The refusal is counted through refused,
// into the listener's authority.tls summary under serve. A read that failed
// because the client left (the request's context cancelled, M3) is dropped
// with no reason logged: nothing was judged, and no code in tlspin or the
// listener names a client that left. A deadline or any other error is store.
func (s *Server) checkPeer(r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
		panic(http.ErrAbortHandler)
	}
	revoked, err := s.revoked(r.Context(), tlspin.Pin(r.TLS.PeerCertificates[0]))
	switch {
	case err == nil && !revoked:
		s.lastPeer.Store(uint64(time.Now().UnixMilli()))
		return
	case err != nil && errors.Is(r.Context().Err(), context.Canceled):
		// The client left mid-read (M3): dropped, with no reason to log.
	case err != nil:
		s.logRefusal(tlspin.ReasonStore)
	default:
		s.logRefusal(tlspin.ReasonRevoked)
	}
	panic(http.ErrAbortHandler)
}

func (s *Server) logRefusal(code tlspin.Reason) { s.refused(string(code)) }

// revoked is the revoked_keys read under recover (S1): a panic is a failed
// read and refuses like one, and never reaches net/http's recover, which logs
// the peer's address and a stack. The panic value is never logged; the
// authority.panic line names the request's route.
func (s *Server) revoked(ctx context.Context, pin []byte) (revoked bool, err error) {
	defer func() {
		if recover() != nil {
			logPanic(s.log, opOf(ctx))
			revoked, err = false, errRevokedPanicked
		}
	}()
	return s.b.Revoked(ctx, pin)
}

// serve runs one route: the standby fast path (B2-D6), run's recover and
// deadline (B2-D7), then the single write, all recorded in the request's
// record. Only Handler reaches it, through the mux, which sets the path
// values; without a record it drops the request.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, rt route) {
	rec, ok := r.Context().Value(recordKey{}).(*record)
	if !ok {
		panic(http.ErrAbortHandler)
	}
	out := errOut(http.StatusServiceUnavailable, ac.ErrorNotLeader)
	if rt.anyRole || s.leader.Load() {
		out = s.run(rt.h, r, rec)
	}
	s.reply(w, rec, out)
}

// reply is the request's single write (S1): sendable, then respond. A write
// that panicked may have sent part of the reply, so the connection is
// dropped, counted and recorded as 503 store.
func (s *Server) reply(w http.ResponseWriter, rec *record, out store.Outcome) {
	rec.out = s.sendable(out)
	if !respond(w, rec.out) {
		logPanic(s.log, rec.op)
		s.b.NoteNonClient503()
		rec.out = errOut(http.StatusServiceUnavailable, ac.ErrorStore)
		panic(http.ErrAbortHandler)
	}
}

// run is B2-D7: the handler under the request's deadline, which Handler set,
// and recover. Only the caller writes, so a panic or a late result never
// sends a partial or a signed body. rec.rid gets the request_id the handler
// read, which a panic leaves in place. A panic is logged as one fixed
// authority.panic line, op the route; the wire answer stays 503
// store.
func (s *Server) run(h handler, r *http.Request, rec *record) (out store.Outcome) {
	ctx := r.Context()
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		if v == http.ErrAbortHandler {
			panic(v)
		}
		logPanic(s.log, rec.op)
		s.b.NoteNonClient503() // the panic value is never logged
		out = errOut(http.StatusServiceUnavailable, ac.ErrorStore)
	}()
	out = h(ctx, r, &rec.rid)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		out = s.late(out)
	}
	return out
}

// late is a result that came after the deadline (B2-D7): it is dropped, an
// error included, for 503 store, and counted. Only DeadlineExceeded reaches
// here, so a client that left is not a non-client 503. The one code kept is
// 503 replication (N3, DoR:1596, 1775): B1's release gate returns it exactly
// when the deadline ends its wait, and it tells the CP to re-POST to the
// leader. It goes out as a fresh envelope, so no byte the late result carried
// is sent.
func (s *Server) late(out store.Outcome) store.Outcome {
	s.b.NoteNonClient503()
	if out.Kind == store.KindError && out.Code == string(ac.ErrorReplication) {
		return errOut(http.StatusServiceUnavailable, ac.ErrorReplication)
	}
	return errOut(http.StatusServiceUnavailable, ac.ErrorStore)
}

// sendable is S1: only a final status, 200–599, reaches WriteHeader. Any
// other, B1's zero Outcome{} sentinel among them, is 503 store and a
// non-client 503. WriteHeader panics below 100 and above 999, into net/http's
// own recover, which logs the peer's address and a stack; a 1xx is no final
// reply (net/http sends most as informational, and the body would follow
// under an implicit 200); and 600–999 is no status a client reads.
func (s *Server) sendable(out store.Outcome) store.Outcome {
	if out.HTTP >= 200 && out.HTTP <= 599 {
		return out
	}
	s.b.NoteNonClient503()
	return errOut(http.StatusServiceUnavailable, ac.ErrorStore)
}

// opOf is the route of the request whose context ctx is, as Handler
// recorded it, or unrouted.
func opOf(ctx context.Context) string {
	if rec, ok := ctx.Value(recordKey{}).(*record); ok {
		return rec.op
	}
	return opUnrouted
}

// logPanic is one recovered panic's line: authority.panic, op the
// route or tls, and nothing of the panic, value or stack. Before it, a panic
// read only as a 503 store, which a database fault also produces.
func logPanic(log *slog.Logger, op string) {
	log.Info("authority.panic", "op", op, "outcome", int(store.KindError))
}

// respond is the request's single write, under its own recover (S1). After
// sendable nothing in it should panic, but a panic here must not reach
// net/http's recover either. It reports false when the write panicked, when
// part of the reply may already be out.
func respond(w http.ResponseWriter, out store.Outcome) (sent bool) {
	defer func() {
		if recover() != nil {
			sent = false
		}
	}()
	if len(out.Body) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(out.HTTP)
	// A failed write is the client's connection failing after the status
	// went out; nothing can be answered on it. A committed result is not lost
	// with it: the CP's identical re-POST converges on the stored result
	// (§3.5).
	_, _ = w.Write(out.Body)
	return true
}

// fill is /v1/signer's server half (B2-D4).
func (s *Server) fill(m *ac.Signer) {
	m.Role = "standby"
	if s.leader.Load() {
		m.Role = "leader"
	}
	m.DiskReady, m.ClockSynced, m.ArchiveOK = b01(s.diskReady()), b01(s.clockSynced()), 0
	m.SubmitLayouts = submitLayouts
}

func b01(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func errOut(status int, code ac.ErrorCode) store.Outcome {
	return store.Outcome{HTTP: status, Kind: store.KindError, Code: string(code),
		Body: ac.ErrorEnvelope{Code: string(code)}.Encode()}
}

func malformedOut() store.Outcome { return errOut(http.StatusBadRequest, ac.ErrorMalformed) }

// body reads at most limit bytes; one byte over is 413 too_large. A read that
// fails drops the connection unanswered: the body missed its deadline
// (boundedBody) or the client left, and Go has cancelled the connection's
// context either way, so it must not be reused. A client fault is not a
// non-client 503. A body deadline that could not be set is this side's fault
// and nothing is written yet, so it is 503 store, counted, before any store
// call (M4). peek, when set, takes the request_id from the bytes for the log
// line (C2) before the store sees them; it decides nothing.
func (s *Server) body(limit int, peek func([]byte) string, f func(context.Context, []byte) store.Outcome) handler {
	return func(ctx context.Context, r *http.Request, rid *string) store.Outcome {
		b, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
		switch {
		case errors.Is(err, errBodyDeadline):
			s.b.NoteNonClient503()
			return errOut(http.StatusServiceUnavailable, ac.ErrorStore)
		case err != nil:
			panic(http.ErrAbortHandler)
		case len(b) > limit:
			return errOut(http.StatusRequestEntityTooLarge, ac.ErrorTooLarge)
		}
		if peek != nil {
			*rid = peek(b)
		}
		return f(ctx, b)
	}
}

// submitRequestID is C2's peek at a `submit`: its intent's request_id (#5),
// or "" when either layout does not decode. The codec has checked it is a
// canonical v4 UUID, so nothing else of the body reaches the log through it.
// It verifies no signature: the request_id of a request the store refuses
// is logged too.
func submitRequestID(b []byte) string {
	sub, err := ac.DecodeSubmit(b)
	if err != nil {
		return ""
	}
	in, err := ac.DecodeIntent(sub.Intent)
	if err != nil {
		return ""
	}
	return in.RequestID
}

// deviceRequestID is the same peek at a `device-submit`: its device-intent's
// request_id, or on the sheet form, which has none, B's, where B1 takes it.
func deviceRequestID(b []byte) string {
	ds, err := ac.DecodeDeviceSubmit(b)
	if err != nil {
		return ""
	}
	if len(ds.DeviceIntent) > 0 {
		di, err := ac.DecodeDeviceIntent(ds.DeviceIntent)
		if err != nil {
			return ""
		}
		return di.RequestID
	}
	bind, err := ac.DecodeRecoveryBind(ds.RecoveryBind)
	if err != nil {
		return ""
	}
	return bind.RequestID
}

func paged(key string, f func(context.Context, string, uint64) store.Outcome) handler {
	return func(ctx context.Context, r *http.Request, _ *string) store.Outcome {
		q, err := url.ParseQuery(r.URL.RawQuery)
		n, ok := cursorUint(q, key)
		if err != nil || !ok {
			return malformedOut()
		}
		return f(ctx, r.PathValue("id"), n)
	}
}

func after(f func(context.Context, string) store.Outcome) handler {
	return func(ctx context.Context, r *http.Request, _ *string) store.Outcome {
		q, err := url.ParseQuery(r.URL.RawQuery)
		id, ok := cursorID(q, "after")
		if err != nil || !ok {
			return malformedOut()
		}
		return f(ctx, id)
	}
}

// cursorUint is B2-D17's after_seq and after_dseq: absent is 0; otherwise
// exactly one canonical decimal uint64.
func cursorUint(q url.Values, key string) (uint64, bool) {
	vs, ok := q[key]
	if !ok {
		return 0, true
	}
	if len(vs) != 1 {
		return 0, false
	}
	n, err := strconv.ParseUint(vs[0], 10, 64)
	return n, err == nil && strconv.FormatUint(n, 10) == vs[0]
}

// cursorID is B2-D17's after: absent or empty is the start; otherwise exactly
// one canonical lowercase UUID.
func cursorID(q url.Values, key string) (string, bool) {
	vs, ok := q[key]
	switch {
	case !ok:
		return "", true
	case len(vs) != 1:
		return "", false
	case vs[0] == "":
		return "", true
	}
	id, err := uuid.Parse(vs[0])
	return vs[0], err == nil && id.String() == vs[0]
}

// noContent adapts a PR-B3 verb with no reply body: nil is 204.
func (s *Server) noContent(f func(context.Context, []byte) error) func(context.Context, []byte) store.Outcome {
	return func(ctx context.Context, b []byte) store.Outcome {
		if err := f(ctx, b); err != nil {
			return s.operatorErr(err)
		}
		return store.Outcome{HTTP: http.StatusNoContent, Kind: store.KindOK}
	}
}

func (s *Server) operator(f func(context.Context, []byte) (store.Outcome, error)) func(context.Context, []byte) store.Outcome {
	return func(ctx context.Context, b []byte) store.Outcome {
		out, err := f(ctx, b)
		if err != nil {
			return s.operatorErr(err)
		}
		return out
	}
}

// operatorErr is B2-D5: ErrNotImplemented is 501 with no body; any other
// error is 503 store until PR-B3 maps its own codes.
func (s *Server) operatorErr(err error) store.Outcome {
	if errors.Is(err, store.ErrNotImplemented) {
		return store.Outcome{HTTP: http.StatusNotImplemented, Kind: store.KindError, Code: "not_implemented"}
	}
	s.b.NoteNonClient503()
	return errOut(http.StatusServiceUnavailable, ac.ErrorStore)
}
