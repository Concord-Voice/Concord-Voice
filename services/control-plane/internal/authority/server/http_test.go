package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/tlspin"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

const testID = "6f1c2d4e-8a3b-4c5d-9e7f-0a1b2c3d4e5f"

// fake is a backend. Its embedded Authority is nil, so any call it does not
// override panics, which the wrapper turns into 503 store.
type fake struct {
	store.Authority
	out      store.Outcome
	opErr    error
	sleep    time.Duration
	boom     any
	revoked  bool
	revErr   error
	revSleep time.Duration
	revBoom  any // a panic inside Revoked
	revCalls int
	revPin   []byte
	calls    []string
	args     []any
	noted    int
	filled   ac.Signer
}

func (f *fake) hit(name string, args ...any) store.Outcome {
	f.calls, f.args = append(f.calls, name), args
	if f.boom != nil {
		panic(f.boom)
	}
	time.Sleep(f.sleep)
	return f.out
}

func (f *fake) Submit(_ context.Context, b []byte) store.Outcome { return f.hit("Submit", len(b)) }
func (f *fake) SubmitDevice(_ context.Context, b []byte) store.Outcome {
	return f.hit("SubmitDevice", len(b))
}
func (f *fake) Head(_ context.Context, id string) store.Outcome { return f.hit("Head", id) }
func (f *fake) History(_ context.Context, id string, n uint64) store.Outcome {
	return f.hit("History", id, n)
}
func (f *fake) Devices(_ context.Context, id string, n uint64) store.Outcome {
	return f.hit("Devices", id, n)
}
func (f *fake) Statuses(_ context.Context, b []byte) store.Outcome { return f.hit("Statuses", len(b)) }
func (f *fake) PendingHeads(_ context.Context, a string) store.Outcome {
	return f.hit("PendingHeads", a)
}
func (f *fake) LegacyPopulation(_ context.Context, a string) store.Outcome {
	return f.hit("LegacyPopulation", a)
}
func (f *fake) InsertLegacyPopulation(_ context.Context, b []byte) error {
	f.hit("InsertLegacyPopulation", len(b))
	return f.opErr
}
func (f *fake) ReconcileRequired(_ context.Context, b []byte) error {
	f.hit("ReconcileRequired", len(b))
	return f.opErr
}
func (f *fake) ReconcileImport(_ context.Context, b []byte) (store.Outcome, error) {
	return f.hit("ReconcileImport", len(b)), f.opErr
}
func (f *fake) ReconcileHold(_ context.Context, b []byte) (store.Outcome, error) {
	return f.hit("ReconcileHold", len(b)), f.opErr
}
func (f *fake) Signer(_ context.Context, fill func(*ac.Signer)) store.Outcome {
	fill(&f.filled)
	return f.hit("Signer")
}
func (f *fake) Revoked(_ context.Context, pin []byte) (bool, error) {
	f.revCalls++
	f.revPin = pin
	if f.revBoom != nil {
		panic(f.revBoom)
	}
	time.Sleep(f.revSleep)
	return f.revoked, f.revErr
}
func (f *fake) NoteNonClient503() { f.noted++ }

var okOut = store.Outcome{HTTP: http.StatusOK, Kind: store.KindOK, Body: []byte(`["concord",1,"x"]`)}

func testServer(t *testing.T, f *fake) (*Server, *x509.Certificate) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tlspin.Mint(k, x509.ExtKeyUsageClientAuth)
	if err != nil {
		t.Fatal(err)
	}
	if f.out.HTTP == 0 {
		f.out = okOut
	}
	s := newServer(f, quiet, func() bool { return true }, func() bool { return true })
	s.leader.Store(true)
	return s, cert.Leaf
}

// logTo points s's log at a fresh JSON buffer, the shape pkg/logger writes.
func logTo(s *Server) *syncBuffer {
	buf := &syncBuffer{}
	s.log = slog.New(slog.NewJSONHandler(buf, nil))
	return buf
}

// lines decodes b's JSON log lines whose msg is msg.
func lines(t *testing.T, b []byte, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(ln) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ln, &m); err != nil {
			t.Fatalf("log line %q: %v", ln, err)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

func peerReq(leaf *x509.Certificate, method, target string, body []byte) *http.Request {
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	return r
}

// deadlineRecorder is an httptest.ResponseRecorder that accepts read
// deadlines through http.ResponseController, as net/http's own writer does.
// It records each one; from failFrom on (1-based), every call fails.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	failFrom  int
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	if d.failFrom > 0 && len(d.deadlines) >= d.failFrom {
		return errors.New("use of closed network connection")
	}
	return nil
}

func newDeadlineRecorder() *deadlineRecorder {
	return &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
}

// serveReq builds the Handler per call, so a test may change a deadline first.
func serveReq(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := newDeadlineRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.ResponseRecorder
}

func do(s *Server, leaf *x509.Certificate, method, target string, body []byte) *httptest.ResponseRecorder {
	return serveReq(s, peerReq(leaf, method, target, body))
}

func envelope(code ac.ErrorCode) []byte { return ac.ErrorEnvelope{Code: string(code)}.Encode() }

// catch runs fn and returns what it panicked with, or nil.
func catch(fn func()) (v any) {
	defer func() { v = recover() }()
	fn()
	return nil
}

// aborts reports whether fn panicked with http.ErrAbortHandler, which
// http.Server turns into a closed connection with no reply.
func aborts(fn func()) bool { return catch(fn) == http.ErrAbortHandler }

type routeCase struct {
	method, target, call string
	want                 int
}

func routeCases() []routeCase {
	acct := "/v1/accounts/" + testID
	return []routeCase{
		{"POST", "/v1/transactions", "Submit", 200},
		{"POST", "/v1/device-transactions", "SubmitDevice", 200},
		{"GET", acct + "/head", "Head", 200},
		{"GET", acct + "/history?after_seq=7", "History", 200},
		{"GET", acct + "/devices?after_dseq=3", "Devices", 200},
		{"POST", "/v1/status", "Statuses", 200},
		{"GET", "/v1/signer", "Signer", 200},
		{"POST", "/v1/legacy-population", "InsertLegacyPopulation", 204},
		{"GET", "/v1/legacy-population?after=" + testID, "LegacyPopulation", 200},
		{"GET", "/v1/pending-heads", "PendingHeads", 200},
		{"POST", "/v1/reconcile/required", "ReconcileRequired", 204},
		{"POST", "/v1/reconcile/import", "ReconcileImport", 200},
		{"POST", "/v1/reconcile/hold", "ReconcileHold", 200},
	}
}

// The 13 routes of §3.3's table (DoR:1598–1611), each to its store call.
func TestEveryRouteReachesItsStoreCall(t *testing.T) {
	for _, c := range routeCases() {
		f := &fake{}
		s, leaf := testServer(t, f)
		w := do(s, leaf, c.method, c.target, []byte("x"))
		if w.Code != c.want || !slices.Equal(f.calls, []string{c.call}) {
			t.Errorf("%s %s: %d %v, want %d [%s]", c.method, c.target, w.Code, f.calls, c.want, c.call)
		}
	}
	f := &fake{}
	s, leaf := testServer(t, f)
	do(s, leaf, "GET", "/v1/accounts/"+testID+"/history?after_seq=7", nil)
	if !slices.Equal(f.args, []any{testID, uint64(7)}) {
		t.Errorf("History got %v", f.args)
	}
	if w := do(s, leaf, "GET", "/v1/transactions", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/transactions: %d", w.Code)
	}
}

// B2-D6: a standby answers /v1/signer and refuses everything else before the store.
func TestStandbyAnswersNotLeaderEverywhereButSigner(t *testing.T) {
	for _, c := range routeCases() {
		f := &fake{}
		s, leaf := testServer(t, f)
		s.leader.Store(false)
		w := do(s, leaf, c.method, c.target, []byte("x"))
		if c.call == "Signer" {
			if w.Code != 200 || f.filled.Role != "standby" {
				t.Errorf("signer on a standby: %d role %q", w.Code, f.filled.Role)
			}
			continue
		}
		if w.Code != 503 || !bytes.Equal(w.Body.Bytes(), envelope(ac.ErrorNotLeader)) || len(f.calls) != 0 {
			t.Errorf("%s %s on a standby: %d %q, reached %v", c.method, c.target, w.Code, w.Body.Bytes(), f.calls)
		}
	}
}

// B2-D4: the server half of /v1/signer.
func TestSignerFillsTheServerHalf(t *testing.T) {
	f := &fake{}
	s, leaf := testServer(t, f)
	s.diskReady = func() bool { return false }
	do(s, leaf, "GET", "/v1/signer", nil)
	m := f.filled
	if m.Role != "leader" || m.DiskReady != 0 || m.ClockSynced != 1 || m.ArchiveOK != 0 ||
		!slices.Equal(m.SubmitLayouts, []string{"device-submit/1", "import-item/1", "submit/1"}) {
		t.Fatalf("%+v", m)
	}
}

// B2-D4: submit_layouts is a canonical a array, which the codec sorts when it
// encodes. The declared list is already in that order, so what a client
// decodes equals it.
func TestSubmitLayoutsAreInWireOrder(t *testing.T) {
	m := ac.Signer{RealmID: testID, DelegationSerial: 1, Role: "leader", RSKDigest: make([]byte, 48),
		SubmitLayouts: submitLayouts, Trust: bytes.Repeat([]byte{1}, 10)}
	got, err := ac.DecodeSigner(m.Encode())
	if err != nil || !slices.Equal(got.SubmitLayouts, submitLayouts) {
		t.Fatalf("decoded %v (%v), declared %v", got.SubmitLayouts, err, submitLayouts)
	}
}

// B2-D7: the route's deadline bounds the body read, which http.Server's
// ReadTimeout alone does not (and this server sets none: its
// ReadHeaderTimeout bounds only the headers). A body that misses it drops the
// connection unanswered and never reaches the store.
func TestASlowBodyIsCutAtTheRouteDeadline(t *testing.T) {
	f := &fake{}
	s, _ := testServer(t, f)
	s.readDeadline = 200 * time.Millisecond
	p := newTLSPair(t)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: readHeaderTimeout}
	go func() { _ = hs.Serve(tls.NewListener(raw, p.srv)) }()
	t.Cleanup(func() { _ = hs.Close() })
	c := dialCP(t, raw.Addr().String(), p.cli)
	defer func() { _ = c.Close() }()
	start := time.Now()
	if _, err := io.WriteString(c, "POST /v1/status HTTP/1.1\r\nHost: a\r\nContent-Length: 10\r\n\r\n12345"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := c.Read(make([]byte, 1))
	var ne net.Error
	if n != 0 || err == nil || (errors.As(err, &ne) && ne.Timeout()) || time.Since(start) > 2*time.Second {
		t.Fatalf("read %d, %v after %v: want the connection closed, unanswered, near the deadline", n, err, time.Since(start))
	}
	if len(f.calls) != 0 {
		t.Fatalf("the store was reached: %v", f.calls)
	}
}

// C3: the body's read deadline is never later than net/http's whole-request
// one, ReadTimeout from the request's arrival. On production values
// (ReadHeader 5 s, Read 10 s, transactions 8 s) the arithmetic is exact: a
// transaction's body is bounded 5 s after the Handler runs, a read route's
// by its own 4 s.
func TestTheBodyDeadlineIsTheEarlierBound(t *testing.T) {
	s := newServer(&fake{}, quiet, nil, nil)
	entry := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		route time.Duration
		want  time.Duration
	}{
		{store.RequestDeadlineTx, 5 * time.Second},
		{store.RequestDeadlineRead, store.RequestDeadlineRead},
		{readTimeout - readHeaderTimeout, readTimeout - readHeaderTimeout},
	} {
		if got := s.bodyDeadline(entry, entry.Add(c.route)); !got.Equal(entry.Add(c.want)) {
			t.Errorf("route %v: body deadline %v after entry, want %v", c.route, got.Sub(entry), c.want)
		}
	}
}

// C3, end to end: headers that arrive late leave the body less than the
// route's deadline. A stalled body must be cut at bodyDeadline's bound,
// entry + ReadTimeout − ReadHeaderTimeout (1 s after entry here), never at
// entry + the route's deadline (3 s), which would hold an authenticated slot
// past ReadTimeout, nor even at net/http's own arrival + ReadTimeout (2.5 s
// after entry). The headers leave 1.5 s of ReadHeaderTimeout unspent (M9), so
// a loaded scheduler cannot cut them first.
func TestABodyAfterSlowHeadersIsCutByTheWholeRequestDeadline(t *testing.T) {
	const (
		rht      = 2500 * time.Millisecond
		rt       = 3500 * time.Millisecond
		tx       = 3000 * time.Millisecond
		hdrDelay = 1000 * time.Millisecond
	)
	f := &fake{}
	s, _ := testServer(t, f)
	s.readHeaderTimeout, s.readTimeout, s.txDeadline = rht, rt, tx
	p := newTLSPair(t)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var entered atomic.Int64 // when the Handler ran, ns after start
	var start time.Time
	h := s.Handler()
	hs := &http.Server{ReadHeaderTimeout: rht, ReadTimeout: rt, Handler: http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			entered.Store(int64(time.Since(start)))
			h.ServeHTTP(w, r)
		})}
	go func() { _ = hs.Serve(tls.NewListener(raw, p.srv)) }()
	t.Cleanup(func() { _ = hs.Close() })
	c := dialCP(t, raw.Addr().String(), p.cli)
	defer func() { _ = c.Close() }()
	start = time.Now()
	time.Sleep(hdrDelay)
	if _, err := io.WriteString(c, "POST /v1/transactions HTTP/1.1\r\nHost: a\r\nContent-Length: 10\r\n\r\n12345"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(start.Add(6 * time.Second))
	n, err := c.Read(make([]byte, 1))
	cut := time.Since(start)
	var ne net.Error
	if n != 0 || err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("read %d, %v after %v: want the connection closed, unanswered", n, err, cut)
	}
	in := time.Duration(entered.Load())
	if in < hdrDelay-50*time.Millisecond {
		t.Fatalf("the Handler ran at %v, before the headers were sent", in)
	}
	if held, want := cut-in, rt-rht; held < want-50*time.Millisecond || held >= want+700*time.Millisecond {
		t.Fatalf("body held %v after the Handler ran (at %v): want %v; the route deadline would give %v, "+
			"net/http's own %v", held, in, want, tx, rt-in)
	}
	if len(f.calls) != 0 {
		t.Fatalf("the store was reached: %v", f.calls)
	}
}

// B2-D7: a panic is 503 store with no signed bytes; ErrAbortHandler is re-raised.
func TestPanicIsA503WithNoSignedBytes(t *testing.T) {
	f := &fake{out: store.Outcome{HTTP: 200, Kind: store.KindCommitted, Body: []byte("signed")}}
	s, leaf := testServer(t, f)
	buf := logTo(s)
	do(s, leaf, "POST", "/v1/transactions", []byte("x"))
	if got := lines(t, buf.Bytes(), "authority.panic"); len(got) != 0 {
		t.Fatalf("control: a request that did not panic logged %v", got)
	}
	f.boom = "boom"
	w := do(s, leaf, "POST", "/v1/transactions", []byte("x"))
	if w.Code != 503 || !bytes.Equal(w.Body.Bytes(), envelope(ac.ErrorStore)) || f.noted != 1 {
		t.Fatalf("%d %q noted=%d", w.Code, w.Body.Bytes(), f.noted)
	}
	// One fixed authority.panic line, op the route, nothing of the panic.
	if got := lines(t, buf.Bytes(), "authority.panic"); len(got) != 1 || got[0]["op"] != "submit" ||
		got[0]["outcome"] != float64(store.KindError) || bytes.Contains(buf.Bytes(), []byte("boom")) {
		t.Fatalf("panic lines %v; log %s", got, buf.Bytes())
	}
	checkLogConvention(t, buf.Bytes())
	f.boom = http.ErrAbortHandler
	if !aborts(func() { do(s, leaf, "GET", "/v1/pending-heads", nil) }) {
		t.Fatal("http.ErrAbortHandler was converted, not re-raised")
	}
}

// B2-D7: any result after the deadline, an error included, is dropped for 503
// store and counted; a client that left is not counted. N3 (DoR:1596, 1775):
// a late 503 replication, B1's release gate answering at the deadline, stays
// 503 replication, as a fresh envelope that carries none of the bytes it came
// with.
func TestALateResultIsReplacedAndCounted(t *testing.T) {
	committed := store.Outcome{HTTP: 200, Kind: store.KindCommitted, Body: []byte("signed")}
	malformed := store.Outcome{HTTP: 400, Kind: store.KindError, Code: "malformed", Body: envelope(ac.ErrorMalformed)}
	replication := store.Outcome{HTTP: 503, Kind: store.KindError, Code: "replication", Body: envelope(ac.ErrorReplication)}
	for _, c := range []struct {
		name, method, target string
		out                  store.Outcome
		clientGone           bool
		want, noted          int
		code                 ac.ErrorCode
	}{
		{"late commit", "POST", "/v1/transactions", committed, false, 503, 1, ac.ErrorStore},
		{"late read", "GET", "/v1/pending-heads", okOut, false, 503, 1, ac.ErrorStore},
		{"late error replaced", "POST", "/v1/transactions", malformed, false, 503, 1, ac.ErrorStore},
		{"late replication passes", "POST", "/v1/transactions", replication, false, 503, 1, ac.ErrorReplication},
		{"late replication on a read", "GET", "/v1/pending-heads", replication, false, 503, 1, ac.ErrorReplication},
		{"late replication drops its bytes", "POST", "/v1/transactions",
			store.Outcome{HTTP: 503, Kind: store.KindError, Code: "replication", Body: []byte("signed")}, false, 503, 1,
			ac.ErrorReplication},
		{"a replication code on signed bytes", "POST", "/v1/transactions",
			store.Outcome{HTTP: 200, Kind: store.KindCommitted, Code: "replication", Body: []byte("signed")}, false, 503, 1,
			ac.ErrorStore},
		{"client gone", "GET", "/v1/pending-heads", okOut, true, 200, 0, ""},
	} {
		f := &fake{out: c.out, sleep: 60 * time.Millisecond}
		s, leaf := testServer(t, f)
		s.txDeadline, s.readDeadline = 20*time.Millisecond, 20*time.Millisecond
		r := peerReq(leaf, c.method, c.target, []byte("x"))
		if c.clientGone {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			r = r.WithContext(ctx)
		}
		w := serveReq(s, r)
		if w.Code != c.want || f.noted != c.noted || (c.want == 503 && !bytes.Equal(w.Body.Bytes(), envelope(c.code))) {
			t.Errorf("%s: %d %q noted=%d", c.name, w.Code, w.Body.Bytes(), f.noted)
		}
	}
}

// B2-D7: the deadline starts when the request arrives, so the revocation read
// spends from it. 30 ms of peer check plus 30 ms of handler overrun a 50-ms
// route; with a fresh deadline after the check, each half alone would fit.
func TestThePeerCheckSpendsTheRequestDeadline(t *testing.T) {
	for _, c := range []struct{ method, target string }{
		{"GET", "/v1/pending-heads"},
		{"POST", "/v1/transactions"},
	} {
		f := &fake{sleep: 30 * time.Millisecond, revSleep: 30 * time.Millisecond}
		s, leaf := testServer(t, f)
		s.txDeadline, s.readDeadline = 50*time.Millisecond, 50*time.Millisecond
		w := serveReq(s, peerReq(leaf, c.method, c.target, []byte("x")))
		if w.Code != 503 || f.noted != 1 {
			t.Errorf("%s %s: %d noted=%d, want 503 and one non-client 503", c.method, c.target, w.Code, f.noted)
		}
	}
}

// B2-D34: once shutdown holds the write lock, a new request is aborted
// before the peer check and before the store.
func TestARequestAfterShutdownBeganIsAborted(t *testing.T) {
	f := &fake{}
	s, leaf := testServer(t, f)
	s.drain.Lock()
	if !aborts(func() { do(s, leaf, "GET", "/v1/signer", nil) }) || f.revCalls != 0 || len(f.calls) != 0 {
		t.Fatal("a request after shutdown began was served")
	}
}

// Review Focus 5 (B2-D17): malformed cursors are 400 malformed and never
// reach the store.
func TestCursorValidation(t *testing.T) {
	history, devices := "/v1/accounts/"+testID+"/history", "/v1/accounts/"+testID+"/devices"
	for _, c := range []struct {
		path, query string
		ok          bool
	}{
		{"/v1/pending-heads", "", true},
		{"/v1/pending-heads", "after=", true},
		{"/v1/pending-heads", "after=" + testID, true},
		{"/v1/pending-heads", "after=ABC", false},
		{"/v1/pending-heads", "after=" + strings.ToUpper(testID), false},
		{"/v1/pending-heads", "after=%7B" + testID + "%7D", false},
		{"/v1/pending-heads", "after=urn:uuid:" + testID, false},
		{"/v1/pending-heads", "after=" + testID + "&after=" + testID, false},
		{"/v1/pending-heads", "after=%zz", false},
		{"/v1/legacy-population", "after=ABC", false},
		{history, "", true},
		{history, "after_seq=0", true},
		{history, "after_seq=18446744073709551615", true},
		{history, "after_seq=01", false},
		{history, "after_seq=-1", false},
		{history, "after_seq=%2B1", false},
		{history, "after_seq=", false},
		{history, "after_seq=1&after_seq=1", false},
		{history, "after_seq=18446744073709551616", false},
		{devices, "after_dseq=1e3", false},
		{devices, "after_dseq=2", true},
	} {
		f := &fake{}
		s, leaf := testServer(t, f)
		w := do(s, leaf, "GET", c.path+"?"+c.query, nil)
		reached := len(f.calls) == 1
		if c.ok != reached || (!c.ok && (w.Code != 400 || !bytes.Equal(w.Body.Bytes(), envelope(ac.ErrorMalformed)))) {
			t.Errorf("%s?%s: %d, reached %v", c.path, c.query, w.Code, reached)
		}
	}
}

// The body caps (DoR:1598–1611): the cap passes, one byte over is 413.
func TestBodyCaps(t *testing.T) {
	for _, c := range []struct {
		target string
		limit  int
	}{
		{"/v1/transactions", store.BodyCapTx},
		{"/v1/device-transactions", store.BodyCapDevice},
		{"/v1/status", store.BodyCapTx},
		{"/v1/legacy-population", bodyCapBatch},
		{"/v1/reconcile/required", bodyCapBatch},
		{"/v1/reconcile/import", bodyCapBatch},
		{"/v1/reconcile/hold", bodyCapBatch},
	} {
		f := &fake{}
		s, leaf := testServer(t, f)
		if w := do(s, leaf, "POST", c.target, make([]byte, c.limit)); len(f.calls) != 1 {
			t.Errorf("%s at the cap: %d, not reached", c.target, w.Code)
		}
		f.calls = nil
		w := do(s, leaf, "POST", c.target, make([]byte, c.limit+1))
		if w.Code != 413 || !bytes.Equal(w.Body.Bytes(), envelope(ac.ErrorTooLarge)) || len(f.calls) != 0 {
			t.Errorf("%s over the cap: %d, reached %v", c.target, w.Code, f.calls)
		}
	}
}

// B3-D6: each operator route maps the store's own refusals itself; a sentinel
// wrapped with %w maps the same.
func TestOperatorErrorMapping(t *testing.T) {
	type want struct {
		code  int
		body  []byte // nil: an empty body
		noted int
	}
	cases := []struct {
		name string
		err  error
		want want
	}{
		{"sealed", store.ErrSealed, want{409, nil, 0}},
		{"reconciled", store.ErrReconciled, want{409, nil, 0}},
		{"bad authz", store.ErrBadAuthz, want{400, envelope(ac.ErrorBadAuthz), 0}},
		{"other error", errors.New("db down"), want{503, envelope(ac.ErrorStore), 1}},
	}
	targets := []string{"/v1/legacy-population", "/v1/reconcile/required", "/v1/reconcile/import", "/v1/reconcile/hold"}
	for _, target := range targets {
		for _, c := range cases {
			for _, wrapped := range []bool{false, true} {
				err := c.err
				if wrapped {
					err = fmt.Errorf("x: %w", c.err)
				}
				f := &fake{opErr: err}
				s, leaf := testServer(t, f)
				w := do(s, leaf, "POST", target, []byte("x"))
				bodyOK := bytes.Equal(w.Body.Bytes(), c.want.body)
				if c.want.body == nil {
					bodyOK = w.Body.Len() == 0
				}
				if w.Code != c.want.code || !bodyOK || f.noted != c.want.noted {
					t.Errorf("%s, %s (wrapped %v): %d %q noted=%d, want %d %q noted=%d",
						target, c.name, wrapped, w.Code, w.Body.Bytes(), f.noted, c.want.code, c.want.body, c.want.noted)
				}
			}
		}
	}
}

// B2-D25 (D-207): the pin is read against revoked_keys before every request,
// on any path and any role; a revoked pin or a failed read aborts, and is
// logged as authority.tls with its reason in the listener's line shape.
func TestThePeerIsCheckedBeforeEveryRequest(t *testing.T) {
	f := &fake{}
	s, leaf := testServer(t, f)
	buf := logTo(s)
	do(s, leaf, "GET", "/v1/signer", nil)
	if f.revCalls != 1 || !bytes.Equal(f.revPin, tlspin.Pin(leaf)) || s.lastPeer.Load() == 0 {
		t.Fatalf("calls %d, pin matches %v, lastPeer %d", f.revCalls, bytes.Equal(f.revPin, tlspin.Pin(leaf)), s.lastPeer.Load())
	}
	for _, c := range []struct {
		name string
		set  func()
		code tlspin.Reason
	}{
		{"revoked", func() { f.revoked = true }, tlspin.ReasonRevoked},
		{"read failed", func() { f.revErr = errors.New("db down") }, tlspin.ReasonStore},
		{"read panicked", func() { f.revBoom = "boom" }, tlspin.ReasonStore},
	} {
		f.revoked, f.revErr, f.revBoom, f.calls = false, nil, nil, nil
		s.lastPeer.Store(0)
		c.set()
		for _, target := range []string{"/v1/signer", "/v1/pending-heads", "/nope"} {
			before := len(lines(t, buf.Bytes(), "authority.tls"))
			if v := catch(func() { do(s, leaf, "GET", target, nil) }); v != http.ErrAbortHandler || len(f.calls) != 0 ||
				s.lastPeer.Load() != 0 {
				t.Errorf("%s, %s: %v, not aborted before the store", c.name, target, v)
			}
			got := lines(t, buf.Bytes(), "authority.tls")
			if len(got) != before+1 || got[before]["op"] != "authority.tls" || got[before]["code"] != string(c.code) ||
				got[before]["outcome"] != float64(store.KindRefused) {
				t.Errorf("%s, %s: logged %v", c.name, target, got[before:])
			}
		}
	}
	if bytes.Contains(buf.Bytes(), []byte("boom")) {
		t.Error("a panic value reached the log")
	}
	// Only the panicking read logs authority.panic, once per request, op its route.
	panics := lines(t, buf.Bytes(), "authority.panic")
	ops := make([]string, 0, len(panics))
	for _, m := range panics {
		op, _ := m["op"].(string)
		ops = append(ops, op)
	}
	if want := []string{"signer", "pending-heads", opUnrouted}; !slices.Equal(ops, want) {
		t.Errorf("authority.panic ops %v, want %v", ops, want)
	}
	f.revoked, f.revErr, f.revBoom, f.revCalls = false, nil, nil, 0
	r := httptest.NewRequest("GET", "/v1/signer", nil) // no TLS state
	if !aborts(func() { s.Handler().ServeHTTP(httptest.NewRecorder(), r) }) {
		t.Error("a request with no TLS state was served")
	}
	for _, certs := range [][]*x509.Certificate{nil, {leaf, leaf}} {
		r := peerReq(leaf, "GET", "/v1/signer", nil)
		r.TLS.PeerCertificates = certs
		if !aborts(func() { serveReq(s, r) }) || f.revCalls != 0 || len(f.calls) != 0 {
			t.Errorf("%d peer certificates: served, or read revoked_keys", len(certs))
		}
	}
}

// M3: a client that leaves during the revoked_keys read is no refusal: the
// request is still dropped, but no authority.tls reason is logged for it, so
// it never counts as a store fault. The test is the request context's
// cancellation, not the error's shape: lib/pq reports a cancelled query in
// its own words. A deadline or a real error stays store.
func TestAClientLeavingDuringThePeerCheckIsNoStoreRefusal(t *testing.T) {
	for _, c := range []struct {
		name   string
		gone   bool
		err    error
		logged bool
	}{
		{"client left", true, context.Canceled, false},
		{"client left, the driver's own words", true, errors.New("pq: canceling statement due to user request"), false},
		{"a cancel that is not the request's", false, context.Canceled, true},
		{"the deadline", false, context.DeadlineExceeded, true},
		{"a real error", false, errors.New("db down"), true},
	} {
		f := &fake{revErr: c.err}
		s, leaf := testServer(t, f)
		buf := logTo(s)
		r := peerReq(leaf, "GET", "/v1/signer", nil)
		if c.gone {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			r = r.WithContext(ctx)
		}
		if c.err == context.DeadlineExceeded {
			s.readDeadline, f.revSleep = time.Millisecond, 10*time.Millisecond
		}
		if !aborts(func() { serveReq(s, r) }) || len(f.calls) != 0 {
			t.Errorf("%s: not aborted before the store", c.name)
		}
		got := lines(t, buf.Bytes(), "authority.tls")
		if c.logged != (len(got) == 1 && got[0]["code"] == string(tlspin.ReasonStore)) || (!c.logged && len(got) != 0) {
			t.Errorf("%s: logged %v", c.name, got)
		}
	}
}

// C2 test material: a realm, an account and a request_id that a body names.
const (
	testRealm   = "0e7c5b1a-2f3d-4c8b-9a6e-1d2c3b4a5f60"
	testAccount = "7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"
	testRID     = "3c1f9e2a-7b4d-4e8f-a1b2-c3d4e5f60718"
)

func mustEncode(t *testing.T, what string, b []byte) []byte {
	t.Helper()
	if b == nil {
		t.Fatalf("%s does not encode", what)
	}
	return b
}

// submitBody is a `submit` whose intent names testRID. Only its layout is
// valid; no signature in it verifies.
func submitBody(t *testing.T) []byte {
	in := mustEncode(t, "intent", ac.Intent{RealmID: testRealm, AccountID: testAccount, RequestID: testRID,
		Op: ac.OpSuspend, ExpiresAtMs: 1}.Encode())
	return mustEncode(t, "submit", ac.Submit{Intent: in, CPAuthz: []byte{1}, CPAuthzSig: make([]byte, 96),
		CPEvidence: []byte{1}}.Encode())
}

// deviceBody is a `device-submit` naming testRID through its device-intent,
// or on the sheet form through B alone.
func deviceBody(t *testing.T, sheet bool) []byte {
	ds := ac.DeviceSubmit{CPAuthz: []byte{1}, CPAuthzSig: make([]byte, 96), CPEvidence: []byte{1}}
	if sheet {
		ds.RecoveryBind = mustEncode(t, "recovery-bind", ac.RecoveryBind{RealmID: testRealm, AccountID: testAccount,
			RequestID: testRID, Kind: ac.BindKindCancel, ExpiresAtMs: 1}.Encode())
	} else {
		ds.DeviceIntent = mustEncode(t, "device-intent", ac.DeviceIntent{RealmID: testRealm, AccountID: testAccount,
			RequestID: testRID, Op: ac.OpAddDevice, Salt: make([]byte, 32)}.Encode())
	}
	return mustEncode(t, "device-submit", ds.Encode())
}

// C2 (DoR §3.6): authority.request carries the request_id the body names, so
// a held refusal is logged keyed by it. Only the two transaction routes read
// one; nothing else of the body, and never the path, reaches the line.
func TestAHeldRefusalLogsItsRequestID(t *testing.T) {
	held := store.Outcome{HTTP: 200, Kind: store.KindRefused, Code: string(ac.RefusalHeld),
		Body: ac.Refused{RequestID: testRID, Code: string(ac.RefusalHeld)}.Encode()}
	for _, c := range []struct {
		name, target string
		body         []byte
		boom         any
		code, want   string
	}{
		{"submit", "/v1/transactions", submitBody(t), nil, "held", testRID},
		{"device-intent", "/v1/device-transactions", deviceBody(t, false), nil, "held", testRID},
		{"sheet cancel", "/v1/device-transactions", deviceBody(t, true), nil, "held", testRID},
		{"a panic after the decode", "/v1/transactions", submitBody(t), "boom", "store", testRID},
		{"no decodable body", "/v1/transactions", []byte("x"), nil, "held", ""},
		{"a device body on the submit route", "/v1/transactions", deviceBody(t, false), nil, "held", ""},
		{"a submit body on the device route", "/v1/device-transactions", submitBody(t), nil, "held", ""},
		{"status reads none", "/v1/status", submitBody(t), nil, "held", ""},
	} {
		f := &fake{out: held, boom: c.boom}
		s, leaf := testServer(t, f)
		buf := logTo(s)
		do(s, leaf, "POST", c.target, c.body)
		got := lines(t, buf.Bytes(), "authority.request")
		if len(got) != 1 || got[0]["request_id"] != c.want || got[0]["code"] != c.code {
			t.Errorf("%s: logged %v, want request_id %q code %q", c.name, got, c.want, c.code)
		}
		for _, leak := range []string{testAccount, testRealm, "/v1/"} {
			if bytes.Contains(buf.Bytes(), []byte(leak)) {
				t.Errorf("%s: %q reached the log", c.name, leak)
			}
		}
	}
}

// S1: a status net/http cannot send as a final reply, B1's zero Outcome{}
// sentinel first among them, is 503 store and a non-client 503. WriteHeader(0)
// would panic into net/http's own recover, which logs the peer's address and a
// stack; a 1xx would go out as an informational reply with the body after it
// under an implicit 200.
func TestAnUnsendableStatusIsA503Store(t *testing.T) {
	for _, status := range []int{0, -1, 99, 100, 102, 199, 600, 999, 1000} {
		f := &fake{}
		s, leaf := testServer(t, f)
		f.out = store.Outcome{HTTP: status, Kind: store.KindCommitted, Body: []byte("signed")}
		if status == 0 {
			f.out = store.Outcome{}
		}
		var w *httptest.ResponseRecorder
		if v := catch(func() { w = do(s, leaf, "GET", "/v1/pending-heads", nil) }); v != nil {
			t.Errorf("status %d: panicked with %v", status, v)
			continue
		}
		if w.Code != 503 || !bytes.Equal(w.Body.Bytes(), envelope(ac.ErrorStore)) || f.noted != 1 {
			t.Errorf("status %d: %d %q noted=%d", status, w.Code, w.Body.Bytes(), f.noted)
		}
	}
	for _, status := range []int{200, 599} {
		f := &fake{out: store.Outcome{HTTP: status, Kind: store.KindError, Code: "x", Body: []byte("y")}}
		s, leaf := testServer(t, f)
		if w := do(s, leaf, "GET", "/v1/pending-heads", nil); w.Code != status || f.noted != 0 {
			t.Errorf("status %d: %d noted=%d", status, w.Code, f.noted)
		}
	}
}

// panicWriter panics in WriteHeader, or in Write once WriteHeader went through.
type panicWriter struct {
	http.ResponseWriter
	inWrite bool
}

func (p panicWriter) WriteHeader(code int) {
	if !p.inWrite {
		panic("header")
	}
	p.ResponseWriter.WriteHeader(code)
}

func (p panicWriter) Write(b []byte) (int, error) {
	if p.inWrite {
		panic("write")
	}
	return p.ResponseWriter.Write(b)
}

// S1: the single write runs under recover too. A panic there may have sent
// part of the reply, so the connection is dropped (ErrAbortHandler, which
// net/http does not log), counted, and logged as 503 store.
func TestAPanicInTheWriteDropsTheConnection(t *testing.T) {
	for _, inWrite := range []bool{false, true} {
		f := &fake{}
		s, leaf := testServer(t, f)
		buf := logTo(s)
		w := panicWriter{ResponseWriter: httptest.NewRecorder(), inWrite: inWrite}
		v := catch(func() { s.Handler().ServeHTTP(w, peerReq(leaf, "GET", "/v1/pending-heads", nil)) })
		got := lines(t, buf.Bytes(), "authority.request")
		if v != http.ErrAbortHandler || f.noted != 1 || len(got) != 1 || got[0]["code"] != string(ac.ErrorStore) {
			t.Errorf("panic in Write %v: %v noted=%d logged %v", inWrite, v, f.noted, got)
		}
		if p := lines(t, buf.Bytes(), "authority.panic"); len(p) != 1 || p[0]["op"] != "pending-heads" {
			t.Errorf("panic in Write %v: authority.panic lines %v, want one, op pending-heads", inWrite, p)
		}
	}
}

// M4: a body deadline that cannot be armed, or cleared, fails closed: 503
// store and a non-client 503 before any store call, logged once. A writer
// that cannot take a read deadline at all is the same failure; only the
// fixtures' deadlineRecorder, like net/http's own writer, takes one.
func TestABodyDeadlineThatCannotBeSetIsA503Store(t *testing.T) {
	failing := func(from int) http.ResponseWriter {
		w := newDeadlineRecorder()
		w.failFrom = from
		return w
	}
	for _, c := range []struct {
		name string
		w    http.ResponseWriter
	}{
		{"a writer without read deadlines", httptest.NewRecorder()},
		{"the arm fails", failing(1)},
		{"the clear fails", failing(2)},
	} {
		f := &fake{}
		s, leaf := testServer(t, f)
		buf := logTo(s)
		s.Handler().ServeHTTP(c.w, peerReq(leaf, "POST", "/v1/transactions", submitBody(t)))
		rec, ok := c.w.(*httptest.ResponseRecorder)
		if !ok {
			rec = c.w.(*deadlineRecorder).ResponseRecorder
		}
		got := lines(t, buf.Bytes(), "authority.request")
		if rec.Code != 503 || !bytes.Equal(rec.Body.Bytes(), envelope(ac.ErrorStore)) || f.noted != 1 ||
			len(f.calls) != 0 || len(got) != 1 || got[0]["code"] != string(ac.ErrorStore) {
			t.Errorf("%s: %d %q noted=%d reached %v logged %v", c.name, rec.Code, rec.Body.Bytes(), f.noted, f.calls, got)
		}
	}
	// The control: a writer that takes deadlines gets the body armed to
	// bodyDeadline's bound on the first read, and cleared at EOF.
	f := &fake{}
	s, leaf := testServer(t, f)
	w := newDeadlineRecorder()
	before := time.Now()
	s.Handler().ServeHTTP(w, peerReq(leaf, "POST", "/v1/transactions", []byte("x")))
	bound := readTimeout - readHeaderTimeout
	if w.Code != 200 || len(w.deadlines) != 2 || w.deadlines[0].Before(before.Add(bound)) ||
		w.deadlines[0].After(time.Now().Add(bound)) || !w.deadlines[1].IsZero() {
		t.Errorf("served %d with deadlines %v, want one armed %v out and one cleared", w.Code, w.deadlines, bound)
	}
}

// M5: HEAD is no §3.3 method. Go's GET patterns match it, so it is refused
// with 405 before any store call; like every request, after the peer check.
// M8: a 405 is peer-checked too.
func TestHeadAndOtherMethodsAreRefusedAfterThePeerCheck(t *testing.T) {
	var targets []string
	for _, c := range routeCases() {
		if c.method == "GET" {
			targets = append(targets, "HEAD "+c.target)
		}
	}
	targets = append(targets, "GET /v1/transactions", "PUT /v1/signer", "DELETE /v1/pending-heads")
	for _, target := range targets {
		method, path, _ := strings.Cut(target, " ")
		f := &fake{}
		s, leaf := testServer(t, f)
		w := do(s, leaf, method, path, nil)
		if w.Code != 405 || !bytes.Equal(w.Body.Bytes(), envelope(ac.ErrorMalformed)) || len(f.calls) != 0 ||
			f.revCalls != 1 {
			t.Errorf("%s: %d %q, reached %v, peer checks %d", target, w.Code, w.Body.Bytes(), f.calls, f.revCalls)
		}
		f.revoked = true
		if !aborts(func() { do(s, leaf, method, path, nil) }) || len(f.calls) != 0 {
			t.Errorf("%s from a revoked peer: not aborted", target)
		}
	}
}

// failingBody fails every read, as a client that leaves mid-body does.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingBody) Close() error             { return nil }

// M6 (DoR §3.6): every request that reaches Handler leaves exactly one
// authority.request line, however it ends: routed, the mux's 404, 405 and
// redirect cases (which Handler answers itself, so the mux never writes),
// OPTIONS *, a body read that fails, a refused peer, a request after shutdown
// began. A request dropped unanswered logs outcome 0 and no code. No line
// carries the path: no "/" reaches the log at all.
func TestEveryRequestLogsExactlyOneLine(t *testing.T) {
	malformed := float64(store.KindError)
	for _, c := range []struct {
		name, method, target string
		setup                func(*Server, *fake, *http.Request)
		status               int // 0: dropped unanswered
		op, code             string
		outcome              float64
	}{
		{"routed", "GET", "/v1/pending-heads", nil, 200, "pending-heads", "", float64(store.KindOK)},
		{"404", "GET", "/nope", nil, 404, "unrouted", "malformed", malformed},
		{"405", "GET", "/v1/transactions", nil, 405, "unrouted", "malformed", malformed},
		{"HEAD", "HEAD", "/v1/signer", nil, 405, "unrouted", "malformed", malformed},
		{"a doubled slash the mux would redirect", "GET", "/v1//signer", nil, 404, "unrouted", "malformed", malformed},
		{"a dot segment the mux would redirect", "GET", "/v1/./signer", nil, 404, "unrouted", "malformed", malformed},
		{"a trailing slash", "GET", "/v1/signer/", nil, 404, "unrouted", "malformed", malformed},
		{"OPTIONS *", "OPTIONS", "*", nil, 404, "unrouted", "malformed", malformed},
		{"a body read that fails", "POST", "/v1/transactions",
			func(_ *Server, _ *fake, r *http.Request) { r.Body = failingBody{} }, 0, "submit", "", 0},
		{"a refused peer", "GET", "/v1/pending-heads",
			func(_ *Server, f *fake, _ *http.Request) { f.revoked = true }, 0, "pending-heads", "", 0},
		{"after shutdown began", "GET", "/v1/signer",
			func(s *Server, _ *fake, _ *http.Request) { s.drain.Lock() }, 0, "signer", "", 0},
	} {
		f := &fake{}
		s, leaf := testServer(t, f)
		buf := logTo(s)
		r := peerReq(leaf, c.method, c.target, []byte("x"))
		if c.setup != nil {
			c.setup(s, f, r)
		}
		w := newDeadlineRecorder()
		v := catch(func() { s.Handler().ServeHTTP(w, r) })
		if (c.status == 0) != (v == http.ErrAbortHandler) || (c.status != 0 && w.Code != c.status) {
			t.Errorf("%s: %d, panicked %v, want %d", c.name, w.Code, v, c.status)
		}
		got := lines(t, buf.Bytes(), "authority.request")
		if len(got) != 1 || got[0]["op"] != c.op || got[0]["outcome"] != c.outcome || got[0]["code"] != c.code {
			t.Errorf("%s: logged %v, want one line op %q outcome %v code %q", c.name, got, c.op, c.outcome, c.code)
		}
		if bytes.ContainsAny(buf.Bytes(), "/*") || bytes.Contains(buf.Bytes(), []byte("nope")) {
			t.Errorf("%s: the path reached the log: %s", c.name, buf.Bytes())
		}
	}
}

// M7: latency_ms runs from Handler entry, so it counts the revoked_keys read.
func TestLatencyIncludesThePeerCheck(t *testing.T) {
	f := &fake{revSleep: 40 * time.Millisecond}
	s, leaf := testServer(t, f)
	buf := logTo(s)
	do(s, leaf, "GET", "/v1/pending-heads", nil)
	got := lines(t, buf.Bytes(), "authority.request")
	if len(got) != 1 {
		t.Fatalf("logged %v, want one line", got)
	}
	if ms, ok := got[0]["latency_ms"].(float64); !ok || ms < 40 {
		t.Fatalf("logged %v, want latency_ms of at least the 40-ms peer check", got)
	}
}

// M8 (DoR §3.6): the account routes name the account in their path, and
// neither reaches the log.
func TestAccountRoutesNeverLogTheAccount(t *testing.T) {
	for _, target := range []string{"/head", "/history?after_seq=7", "/devices?after_dseq=3"} {
		f := &fake{}
		s, leaf := testServer(t, f)
		buf := logTo(s)
		if w := do(s, leaf, "GET", "/v1/accounts/"+testAccount+target, nil); w.Code != 200 || len(f.calls) != 1 {
			t.Fatalf("%s: %d, reached %v", target, w.Code, f.calls)
		}
		if bytes.Contains(buf.Bytes(), []byte(testAccount)) || bytes.ContainsAny(buf.Bytes(), "/") {
			t.Errorf("%s: logged %s", target, buf.Bytes())
		}
	}
}

// The operator routes' prologue refusals (not_leader, disk, busy, replication)
// reach the wire with the store's own status and ac code, wrapped or not,
// never flattened to 503 store. The store has already set its own tick leg
// (hardFloorCheck, replicationError), and the server counts nothing for a
// store-returned 503 outcome on Submit either (serve's standby gate, reply,
// sendable count only a panic, a late result or an invalid status), so noted
// stays 0 here too.
func TestOperatorRoutesKeepStoreCodes(t *testing.T) {
	targets := []string{"/v1/legacy-population", "/v1/reconcile/required", "/v1/reconcile/import", "/v1/reconcile/hold"}
	codes := []ac.ErrorCode{ac.ErrorNotLeader, ac.ErrorDisk, ac.ErrorBusy, ac.ErrorReplication}
	for _, target := range targets {
		for _, code := range codes {
			for _, wrapped := range []bool{false, true} {
				var err error = store.OutcomeError{Outcome: store.Outcome{HTTP: http.StatusServiceUnavailable,
					Kind: store.KindError, Code: string(code), Body: envelope(code)}}
				if wrapped {
					err = fmt.Errorf("x: %w", err)
				}
				f := &fake{opErr: err}
				s, leaf := testServer(t, f)
				w := do(s, leaf, "POST", target, []byte("x"))
				if w.Code != 503 || !bytes.Equal(w.Body.Bytes(), envelope(code)) || f.noted != 0 {
					t.Errorf("%s, %s (wrapped %v): %d %q noted=%d, want 503 %q noted=0",
						target, code, wrapped, w.Code, w.Body.Bytes(), f.noted, envelope(code))
				}
			}
		}
	}
}
