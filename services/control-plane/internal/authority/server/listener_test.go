package server

import (
	"bytes"
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
	"maps"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/tlspin"
)

var quiet = slog.New(slog.DiscardHandler)

type tlsPair struct {
	srv, cli *tls.Config
	points   [][]byte        // the authority's TLS point, for another CP config
	cliCert  tls.Certificate // the CP's certificate and key
	pin      []byte          // the CP's pin
}

func p384(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newTLSPair(t *testing.T) tlsPair {
	t.Helper()
	auth, cp := p384(t), p384(t)
	srvCert, err := tlspin.Mint(auth, x509.ExtKeyUsageServerAuth)
	if err != nil {
		t.Fatal(err)
	}
	cliCert, err := tlspin.Mint(cp, x509.ExtKeyUsageClientAuth)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := auth.PublicKey.ECDH()
	if err != nil {
		t.Fatal(err)
	}
	points := [][]byte{pt.Bytes()}
	cli, err := tlspin.ClientConfig(cliCert, points)
	if err != nil {
		t.Fatal(err)
	}
	return tlsPair{srv: tlspin.ServerConfig(srvCert), cli: cli, points: points, cliCert: cliCert,
		pin: tlspin.Pin(cliCert.Leaf)}
}

// cpConfig is a CP config that trusts this authority and presents cert.
func (p tlsPair) cpConfig(t *testing.T, cert tls.Certificate) *tls.Config {
	t.Helper()
	cfg, err := tlspin.ClientConfig(cert, p.points)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// fakeCheck is Task 10's check reduced to its decisions: membership in pins,
// then the revoked_keys read, which fails when storeDown is set. calls counts
// every invocation, so a test can prove a peer never reached it.
type fakeCheck struct {
	pins, revoked [][]byte
	storeDown     bool
	calls         atomic.Int32
}

func (f *fakeCheck) check(pin []byte) error {
	f.calls.Add(1)
	has := func(set [][]byte) bool {
		return slices.ContainsFunc(set, func(p []byte) bool { return bytes.Equal(p, pin) })
	}
	switch {
	case !has(f.pins):
		return tlspin.ReasonUnpinned
	case f.storeDown:
		return tlspin.ReasonStore
	case has(f.revoked):
		return tlspin.ReasonRevoked
	}
	return nil
}

// echoServe accepts from l and, for each connection, answers every byte it
// reads; closing a connection releases its slot as http.Server's ConnState does.
func echoServe(l *listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer l.release(c)
			defer func() { _ = c.Close() }()
			_, _ = io.Copy(c, c)
		}()
	}
}

// acceptInto runs Accept until it fails and hands every connection to the
// returned channel, which it closes at the end.
func acceptInto(l *listener) <-chan net.Conn {
	got := make(chan net.Conn, authenticatedBudget)
	go func() {
		defer close(got)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			got <- c
		}
	}()
	return got
}

func dialCP(t *testing.T, addr string, cfg *tls.Config) *tls.Conn {
	t.Helper()
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, cfg)
	if err != nil {
		t.Fatalf("CP dial: %v", err)
	}
	return c
}

func roundTrip(t *testing.T, c *tls.Conn) {
	t.Helper()
	if !tryRoundTrip(c, 3*time.Second) {
		t.Fatal("round trip failed")
	}
}

func tryRoundTrip(c *tls.Conn, d time.Duration) bool {
	_ = c.SetDeadline(time.Now().Add(d))
	if _, err := c.Write([]byte{7}); err != nil {
		return false
	}
	var b [1]byte
	_, err := io.ReadFull(c, b[:])
	return err == nil && b[0] == 7
}

// kind names what a client saw when its read failed: "alert" for a TLS alert
// from the server, "reset", "eof", or the error itself.
func kind(err error) string {
	var op *net.OpError
	switch {
	case err == nil:
		return "data"
	case errors.As(err, &op) && op.Op == "remote error":
		return "alert"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, io.EOF):
		return "eof"
	}
	return err.Error()
}

// observe connects as cfg, reads once, and reports what the client saw (see
// kind). It returns only once the listener has dropped the connection, so the
// listener has counted the refusal by then.
func observe(t *testing.T, addr string, cfg *tls.Config) string {
	t.Helper()
	raw, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
	var b [1]byte
	n, err := tls.Client(raw, cfg).Read(b[:]) // the handshake runs first
	if n > 0 {
		return "data"
	}
	got := kind(err)
	var ne net.Error
	if _, err := io.Copy(io.Discard, raw); errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the listener kept the connection open (the client saw %s)", got)
	}
	return got
}

// garbage is a stranger that sends bytes that are not TLS. It returns once
// the listener has dropped it.
func garbage(t *testing.T, addr string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	var ne net.Error
	if _, err := io.Copy(io.Discard, c); errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the listener kept a stranger that sent no TLS")
	}
}

func listen(t *testing.T, p tlsPair, timeout time.Duration, check func([]byte) error, log *slog.Logger,
	every time.Duration) *listener {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := newListenerEvery(raw, p.srv, log, timeout, check, every)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// startListener serves p's CP, pinned, through echoServe.
func startListener(t *testing.T, p tlsPair, timeout time.Duration) *listener {
	t.Helper()
	l := listen(t, p, timeout, (&fakeCheck{pins: [][]byte{p.pin}}).check, quiet, refusalSummaryEvery)
	go echoServe(l)
	return l
}

// stall opens n TCP connections that never send a ClientHello.
func stall(t *testing.T, addr string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
	}
}

// inFlight waits until n handshakes hold pre-handshake slots. Below the
// budget the accept loop holds one more, taken before it waits in Accept.
func inFlight(t *testing.T, l *listener, n int) {
	t.Helper()
	waitFor(t, "the handshakes in flight", func() bool { return len(l.pre) == n+1 })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// summary is what the listener logged for one refusal code: how many lines,
// and the sum of their counts.
type summary struct{ lines, total int }

// summaries reads the listener's log. It fails on any line that is not an
// authority.tls refusal summary carrying exactly DoR §3.6's fields plus count,
// with outcome typed as every authority line types it: never the certificate,
// the peer's address or its pin.
func summaries(t *testing.T, b []byte) map[string]summary {
	t.Helper()
	want := []string{"code", "count", "level", "msg", "op", "outcome", "time"}
	out := map[string]summary{}
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		code, _ := m["code"].(string)
		n, _ := m["count"].(float64)
		if !slices.Equal(slices.Sorted(maps.Keys(m)), want) || m["msg"] != "authority.tls" ||
			m["op"] != "authority.tls" || m["outcome"] != float64(store.KindRefused) || code == "" || n < 1 {
			t.Fatalf("not a refusal summary: %s", line)
		}
		s := out[code]
		s.lines++
		s.total += int(n)
		out[code] = s
	}
	return out
}

// The budget split (D-204, AC14). With 15 warm CP connections and 17
// strangers stalled in their handshakes, a new CP connection still completes
// at once, because warm connections hold no pre-handshake slot. A single
// budget held for a connection's lifetime would be full at 32 and make the
// new connection wait out a stranger's deadline: mutation 6. The strangers'
// deadline is 4 s and the bound 2 s, so a loaded -race runner has headroom
// and the mutant still waits about twice the bound.
func TestBudgetSplitKeepsCPConnectionsUnderAFlood(t *testing.T) {
	p := newTLSPair(t)
	l := startListener(t, p, 4*time.Second)
	addr := l.Addr().String()
	warm := make([]*tls.Conn, 15)
	for i := range warm {
		warm[i] = dialCP(t, addr, p.cli)
		roundTrip(t, warm[i])
		t.Cleanup(func() { _ = warm[i].Close() })
	}
	stall(t, addr, 17)
	// At least 17 slots held under either budget, so the mutant reaches the
	// timing assertion below: 18 when split (the accept loop holds the next).
	waitFor(t, "the strangers' handshakes", func() bool { return len(l.pre) >= 17 })
	start := time.Now()
	c := dialCP(t, addr, p.cli)
	defer func() { _ = c.Close() }()
	roundTrip(t, c)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a new CP connection waited %v behind stalled strangers", d)
	}
	for _, w := range warm {
		roundTrip(t, w)
	}
}

// A stranger holds its pre-handshake slot for at most the handshake deadline.
func TestStrangersAreDroppedAtTheHandshakeDeadline(t *testing.T) {
	p := newTLSPair(t)
	l := startListener(t, p, time.Second)
	stall(t, l.Addr().String(), preHandshakeBudget)
	waitFor(t, "every pre-handshake slot held", func() bool { return len(l.pre) == preHandshakeBudget })
	c := dialCP(t, l.Addr().String(), p.cli) // its 3 s dial outlasts only the strangers' deadline
	defer func() { _ = c.Close() }()
	roundTrip(t, c)
}

// The authenticated budget is 16: a 17th CP connection is closed, never
// queued, and a slot frees when a connection closes.
func TestAuthenticatedBudgetIsSixteen(t *testing.T) {
	p := newTLSPair(t)
	var buf syncBuffer
	l := listen(t, p, 2*time.Second, (&fakeCheck{pins: [][]byte{p.pin}}).check,
		slog.New(slog.NewJSONHandler(&buf, nil)), refusalSummaryEvery)
	go echoServe(l)
	addr := l.Addr().String()
	conns := make([]*tls.Conn, authenticatedBudget)
	for i := range conns {
		conns[i] = dialCP(t, addr, p.cli)
		roundTrip(t, conns[i])
	}
	if got := observe(t, addr, p.cli); got == "data" {
		t.Fatal("a 17th authenticated connection was served")
	}
	_ = conns[0].Close()
	waitFor(t, "the closed connection's slot", func() bool {
		c := dialCP(t, addr, p.cli)
		if !tryRoundTrip(c, time.Second) {
			_ = c.Close()
			return false
		}
		t.Cleanup(func() { _ = c.Close() })
		return true
	})
	for _, c := range conns[1:] {
		_ = c.Close()
	}
	_ = l.Close()
	l.wait()
	if s := summaries(t, buf.Bytes()); s["budget"].total < 1 {
		t.Fatalf("no budget refusal logged: %v", s)
	}
}

// N5: refusals are aggregated. Three unpinned CPs and two strangers that send
// no TLS make one line per code, each carrying its count, and nothing that
// identifies a peer.
func TestRefusedHandshakeIsLoggedWithItsReason(t *testing.T) {
	p := newTLSPair(t)
	var buf syncBuffer
	l := listen(t, p, 2*time.Second, (&fakeCheck{}).check, slog.New(slog.NewJSONHandler(&buf, nil)), time.Hour)
	accepted := acceptInto(l)
	addr := l.Addr().String()
	for range 3 {
		if got := observe(t, addr, p.cli); got == "data" {
			t.Fatal("an unpinned CP was served")
		}
	}
	for range 2 {
		garbage(t, addr)
	}
	_ = l.Close()
	l.wait()
	got := summaries(t, buf.Bytes())
	want := map[string]summary{"unpinned": {1, 3}, "handshake": {1, 2}}
	if !maps.Equal(got, want) {
		t.Fatalf("summaries %v, want %v; log:\n%s", got, want, buf.Bytes())
	}
	if bytes.Contains(buf.Bytes(), []byte("127.0.0.1")) {
		t.Fatalf("a peer address was logged: %s", buf.Bytes())
	}
	if _, ok := <-accepted; ok {
		t.Fatal("Accept returned a refused connection")
	}
}

// N5: the summary is emitted each interval without waiting for Close, a
// window's count starts from zero, and an empty window emits nothing.
func TestRefusalSummaryIsEmittedEachInterval(t *testing.T) {
	p := newTLSPair(t)
	var buf syncBuffer
	l := listen(t, p, 2*time.Second, (&fakeCheck{}).check, slog.New(slog.NewJSONHandler(&buf, nil)),
		50*time.Millisecond)
	addr := l.Addr().String()
	_ = observe(t, addr, p.cli)
	waitFor(t, "the first summary", func() bool { return summaries(t, buf.Bytes())["unpinned"] == summary{1, 1} })
	_ = observe(t, addr, p.cli)
	waitFor(t, "the second summary", func() bool { return summaries(t, buf.Bytes())["unpinned"] == summary{2, 2} })
	before := len(buf.Bytes())
	_ = l.Close()
	l.wait()
	if after := buf.Bytes(); len(after) != before {
		t.Fatalf("Close emitted a summary of nothing: %s", after[before:])
	}
}

// N5: Close flushes and stops the interval timer; a refusal counted after
// Close arms nothing, and wait's flush emits it.
func TestRefusalsAfterCloseAreFlushedByWait(t *testing.T) {
	var buf syncBuffer
	r := newRefusals(slog.New(slog.NewJSONHandler(&buf, nil)), time.Hour)
	r.add("eku")
	r.close()
	if s := summaries(t, buf.Bytes()); s["eku"] != (summary{1, 1}) {
		t.Fatalf("Close did not flush: %v", s)
	}
	r.add("eku")
	r.mu.Lock()
	armed := r.timer != nil
	r.mu.Unlock()
	if armed {
		t.Fatal("a refusal after Close armed the interval timer")
	}
	r.flush()
	if s := summaries(t, buf.Bytes()); s["eku"] != (summary{2, 2}) {
		t.Fatalf("a refusal after Close was lost: %v", s)
	}
}

// listenerStacks returns the stacks of every goroutine running the
// listener's or the summary's code. A count against a baseline would be
// skewed by other tests' goroutines still exiting; a frame names its owner.
func listenerStacks() []string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "server.(*listener).") || strings.Contains(g, "server.(*refusals).") {
			out = append(out, g)
		}
	}
	return out
}

// No goroutine outlives Close and wait: not the accept loop, not a handshake
// cut short, and no summary timer is left armed.
func TestCloseLeavesNoGoroutineBehind(t *testing.T) {
	p := newTLSPair(t)
	l := listen(t, p, time.Hour, (&fakeCheck{}).check, quiet, time.Hour) // only Close ends a stalled handshake
	addr := l.Addr().String()
	_ = observe(t, addr, p.cli) // a refusal arms the summary timer
	stall(t, addr, 3)
	inFlight(t, l, 3)
	_ = l.Close()
	waited := make(chan struct{})
	go func() { l.wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("Close left a handshake running")
	}
	l.sum.mu.Lock()
	armed := l.sum.timer != nil
	l.sum.mu.Unlock()
	if armed {
		t.Fatal("Close left the summary timer armed")
	}
	deadline := time.Now().Add(3 * time.Second)
	for g := listenerStacks(); len(g) > 0; g = listenerStacks() {
		if time.Now().After(deadline) {
			t.Fatalf("%d listener goroutines outlived Close and wait:\n%s", len(g), strings.Join(g, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A check that passes after Close admits nothing (admit's ctx.Done arm): no
// Accept returns the connection, and its authenticated slot is returned.
func TestACheckPassingAfterCloseAdmitsNothing(t *testing.T) {
	p := newTLSPair(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	check := func([]byte) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}
	l := listen(t, p, 2*time.Second, check, quiet, time.Hour)
	accepted := acceptInto(l)
	c := dialCP(t, l.Addr().String(), p.cli) // the client's side ends before the check runs
	defer func() { _ = c.Close() }()
	<-entered
	_ = l.Close() // Accept has returned net.ErrClosed before the check passes
	close(release)
	waited := make(chan struct{})
	go func() { l.wait(); close(waited) }()
	select { // a wait that never returns fails here rather than at the go test timeout
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not return within 3 s of Close and the check's release")
	}
	if a, ok := <-accepted; ok {
		_ = a.Close()
		t.Fatal("Accept returned a connection whose check passed after Close")
	}
	held := 0
	l.slots.Range(func(any, any) bool { held++; return true })
	if n := len(l.auth); n != 0 || held != 0 {
		t.Fatalf("%d authenticated slots and %d slot records held after wait", n, held)
	}
}

// E1 (2026-10-16): the pin check runs only once the handshake has proved the
// peer holds the certificate's key. Go's TLS 1.3 server runs VerifyConnection
// before CertificateVerify, so a check there ran, revoked_keys read included,
// for a peer that cannot sign.
func TestPossessionIsProvedBeforeThePinCheck(t *testing.T) {
	p := newTLSPair(t)
	t.Run("a pinned certificate presented without its key is never checked", func(t *testing.T) {
		var buf syncBuffer
		f := &fakeCheck{pins: [][]byte{p.pin}} // pinned, so a check would pass and read revoked_keys
		l := listen(t, p, 2*time.Second, f.check, slog.New(slog.NewJSONHandler(&buf, nil)), time.Hour)
		accepted := acceptInto(l)
		stolen := tls.Certificate{Certificate: p.cliCert.Certificate, PrivateKey: p384(t), Leaf: p.cliCert.Leaf}
		if got := observe(t, l.Addr().String(), p.cpConfig(t, stolen)); got != "alert" {
			t.Fatalf("the client saw %q, want the handshake's alert", got)
		}
		_ = l.Close()
		l.wait()
		if n := f.calls.Load(); n != 0 {
			t.Fatalf("the check ran %d times for a peer that cannot sign", n)
		}
		if got := summaries(t, buf.Bytes()); !maps.Equal(got, map[string]summary{"handshake": {1, 1}}) {
			t.Fatalf("summaries %v, want only the failed handshake", got)
		}
		if _, ok := <-accepted; ok {
			t.Fatal("Accept returned a peer that cannot sign")
		}
	})
	t.Run("an unpinned certificate with its key is checked once and refused", func(t *testing.T) {
		var buf syncBuffer
		f := &fakeCheck{}
		l := listen(t, p, 2*time.Second, f.check, slog.New(slog.NewJSONHandler(&buf, nil)), time.Hour)
		accepted := acceptInto(l)
		if got := observe(t, l.Addr().String(), p.cli); got == "data" {
			t.Fatal("an unpinned CP was served")
		}
		_ = l.Close()
		l.wait()
		if n := f.calls.Load(); n != 1 {
			t.Fatalf("the check ran %d times, want 1", n)
		}
		if got := summaries(t, buf.Bytes()); !maps.Equal(got, map[string]summary{"unpinned": {1, 1}}) {
			t.Fatalf("summaries %v, want one unpinned refusal", got)
		}
		if _, ok := <-accepted; ok {
			t.Fatal("Accept returned an unpinned peer")
		}
	})
}

// The refusals after a successful handshake (E1 ruling 3, and the two
// server-side cases Task 5 moved here): each is refused, logged with its
// reason and never accepted, and the three look the same to the peer, a reset
// with no alert and no bytes.
func TestPinRefusalsAreLoggedAndLookAlike(t *testing.T) {
	p := newTLSPair(t)
	cases := []struct {
		name string
		f    *fakeCheck
		code tlspin.Reason
	}{
		{"an unpinned client is refused with reason unpinned", &fakeCheck{}, tlspin.ReasonUnpinned},
		{"a revoked pin is refused with reason revoked",
			&fakeCheck{pins: [][]byte{p.pin}, revoked: [][]byte{p.pin}}, tlspin.ReasonRevoked},
		{"a failed revoked_keys read is refused with reason store",
			&fakeCheck{pins: [][]byte{p.pin}, storeDown: true}, tlspin.ReasonStore},
	}
	for _, tc := range cases { // each must be the same bare reset, so the three look alike
		t.Run(tc.name, func(t *testing.T) {
			var buf syncBuffer
			l := listen(t, p, 2*time.Second, tc.f.check, slog.New(slog.NewJSONHandler(&buf, nil)), time.Hour)
			accepted := acceptInto(l)
			got := observe(t, l.Addr().String(), p.cli)
			_ = l.Close()
			l.wait()
			if got != "reset" {
				t.Fatalf("the client saw %q, want a bare reset", got)
			}
			want := map[string]summary{string(tc.code): {1, 1}}
			if s := summaries(t, buf.Bytes()); !maps.Equal(s, want) {
				t.Fatalf("summaries %v, want %v", s, want)
			}
			if n := tc.f.calls.Load(); n != 1 {
				t.Fatalf("the check ran %d times, want 1", n)
			}
			if _, ok := <-accepted; ok {
				t.Fatal("Accept returned a refused connection")
			}
		})
	}
}

// Admission is decided on the check's error alone (CWE-636). tlspin.Reason is
// a string, so a zero Reason, bare or wrapped, is a non-nil error whose reason
// is empty: it refuses, and is counted as store.
func TestAZeroReasonStillRefuses(t *testing.T) {
	p := newTLSPair(t)
	cases := []struct {
		name string
		err  error
	}{
		{"a zero Reason", tlspin.Reason("")},
		{"a wrapped zero Reason", fmt.Errorf("x: %w", tlspin.Reason(""))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf syncBuffer
			l := listen(t, p, 2*time.Second, func([]byte) error { return tc.err },
				slog.New(slog.NewJSONHandler(&buf, nil)), time.Hour)
			accepted := acceptInto(l)
			c := dialCP(t, l.Addr().String(), p.cli)
			_ = c.SetDeadline(time.Now().Add(time.Second))
			var b [1]byte
			_, _ = c.Read(b[:]) // a reset once refused; a timeout if admitted
			_ = c.Close()
			_ = l.Close()
			l.wait()
			if a, ok := <-accepted; ok {
				_ = a.Close()
				t.Fatal("a check that returned an error admitted its peer")
			}
			if s := summaries(t, buf.Bytes()); !maps.Equal(s, map[string]summary{"store": {1, 1}}) {
				t.Fatalf("summaries %v, want one store refusal", s)
			}
		})
	}
}

// The listener pins exactly one certificate even when cfg lets another shape
// through: a chain or no certificate at all is refused as chain_len, never
// checked, and never a panic.
func TestListenerRefusesAPeerItCannotPin(t *testing.T) {
	p := newTLSPair(t)
	two := tls.Certificate{Certificate: [][]byte{p.cliCert.Certificate[0], p.cliCert.Certificate[0]},
		PrivateKey: p.cliCert.PrivateKey}
	none := p.cli.Clone()
	none.Certificates = nil
	cases := []struct {
		name       string
		clientAuth tls.ClientAuthType
		cli        *tls.Config
	}{
		{"two certificates", tls.RequireAnyClientCert, p.cpConfig(t, two)},
		{"no certificate", tls.RequestClientCert, none},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := p
			q.srv = p.srv.Clone()
			q.srv.VerifyConnection = nil // as if cfg's shape check were lost
			q.srv.ClientAuth = tc.clientAuth
			var buf syncBuffer
			f := &fakeCheck{pins: [][]byte{p.pin}}
			l := listen(t, q, 2*time.Second, f.check, slog.New(slog.NewJSONHandler(&buf, nil)), time.Hour)
			accepted := acceptInto(l)
			if got := observe(t, l.Addr().String(), tc.cli); got != "reset" {
				t.Fatalf("the client saw %q, want a reset", got)
			}
			_ = l.Close()
			l.wait()
			if n := f.calls.Load(); n != 0 {
				t.Fatalf("the check ran %d times", n)
			}
			if s := summaries(t, buf.Bytes()); !maps.Equal(s, map[string]summary{"chain_len": {1, 1}}) {
				t.Fatalf("summaries %v, want one chain_len refusal", s)
			}
			if _, ok := <-accepted; ok {
				t.Fatal("Accept returned a peer it could not pin")
			}
		})
	}
}

// A listener without a check would admit no one; it refuses to be built.
func TestNewListenerRefusesANilCheck(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	defer func() {
		if recover() == nil {
			t.Fatal("newListener accepted a nil check")
		}
	}()
	_ = newListener(raw, newTLSPair(t).srv, quiet, handshakeTimeout, nil)
}

// The check runs inside the pre-handshake budget (E1 ruling 2): a connection
// keeps its pre-handshake slot until its check returns, so at most 32
// revoked_keys reads are ever in flight from the listener.
func TestThePinCheckRunsInsideThePreHandshakeBudget(t *testing.T) {
	p := newTLSPair(t)
	var entered atomic.Int32
	release := make(chan struct{})
	check := func([]byte) error {
		entered.Add(1)
		<-release // a slow revoked_keys read
		return nil
	}
	l := listen(t, p, 5*time.Second, check, quiet, refusalSummaryEvery)
	accepted := acceptInto(l)
	for range preHandshakeBudget {
		c := dialCP(t, l.Addr().String(), p.cli) // the client's side of the handshake ends before the check
		t.Cleanup(func() { _ = c.Close() })
	}
	waitFor(t, "every connection inside its check", func() bool { return entered.Load() == preHandshakeBudget })
	if n := len(l.pre); n != preHandshakeBudget {
		t.Fatalf("%d pre-handshake slots held while %d checks ran", n, preHandshakeBudget)
	}
	close(release)
	_ = l.Close()
	l.wait()
	for c := range accepted {
		_ = c.Close()
	}
}

// fakeRaw is a raw listener whose Accept fails once with a transient error
// and then reports itself closed, as if closed under the listener.
type fakeRaw struct {
	net.Listener
	calls atomic.Int32
}

func (f *fakeRaw) Accept() (net.Conn, error) {
	if f.calls.Add(1) == 1 {
		return nil, errors.New("accept: too many open files")
	}
	return nil, net.ErrClosed
}

func (f *fakeRaw) Close() error { return nil }

// A transient Accept error is retried and counted as accept, and a raw
// listener closed under the listener closes it, so Accept returns instead of
// waiting for ever.
func TestAcceptEndsWhenTheRawListenerCloses(t *testing.T) {
	raw := &fakeRaw{}
	var buf syncBuffer
	l := newListenerEvery(raw, newTLSPair(t).srv, slog.New(slog.NewJSONHandler(&buf, nil)), time.Second,
		(&fakeCheck{}).check, time.Hour)
	done := make(chan error, 1)
	go func() { _, err := l.Accept(); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept: %v, want net.ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Accept waited on a raw listener that had closed")
	}
	l.wait()
	if n := raw.calls.Load(); n != 2 {
		t.Fatalf("raw Accept called %d times, want a retry after the transient error", n)
	}
	if s := summaries(t, buf.Bytes()); !maps.Equal(s, map[string]summary{"accept": {1, 1}}) {
		t.Fatalf("summaries %v, want the failed Accept counted", s)
	}
}

// plainConns yields connections that hide *net.TCPConn, so SetLinger is
// unavailable, as with a raw listener that is not TCP.
type plainConns struct{ net.Listener }

func (p plainConns) Accept() (net.Conn, error) {
	c, err := p.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return struct{ net.Conn }{c}, nil
}

// A refusal that cannot reset its connection still closes it, and is counted
// as linger, so a FIN that leaks the check's latency cannot return unseen.
func TestARefusalWithoutAResetIsCounted(t *testing.T) {
	p := newTLSPair(t)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var buf syncBuffer
	l := newListenerEvery(plainConns{tcp}, p.srv, slog.New(slog.NewJSONHandler(&buf, nil)), 2*time.Second,
		(&fakeCheck{}).check, time.Hour)
	t.Cleanup(func() { _ = l.Close() })
	if got := observe(t, l.Addr().String(), p.cli); got == "data" {
		t.Fatal("an unpinned CP was served")
	}
	_ = l.Close()
	l.wait()
	want := map[string]summary{"unpinned": {1, 1}, "linger": {1, 1}}
	if s := summaries(t, buf.Bytes()); !maps.Equal(s, want) {
		t.Fatalf("summaries %v, want %v", s, want)
	}
}

// B2-D34: wait returns only after every connection's post-handshake check
// has, because the check reads revoked_keys. Close alone does not stop a
// check already running.
func TestWaitCoversAHandshakeInsideItsPinCheck(t *testing.T) {
	p := newTLSPair(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	check := func([]byte) error {
		once.Do(func() { close(entered) })
		<-release // a slow revoked_keys read
		return nil
	}
	l := listen(t, p, 2*time.Second, check, quiet, refusalSummaryEvery)
	go func() {
		if c, err := tls.Dial("tcp", l.Addr().String(), p.cli); err == nil {
			var b [1]byte
			_, _ = c.Read(b[:])
			_ = c.Close()
		}
	}()
	<-entered
	_ = l.Close()
	waited := make(chan struct{})
	go func() { l.wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("wait returned while a pin check ran")
	case <-time.After(200 * time.Millisecond): // an absence: only a bounded wait can show it
	}
	close(release)
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not return after the pin check ended")
	}
}

// syncBuffer is a bytes.Buffer safe for the listener's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

// Deferred #16: reasonOf forwards only the six tlspin constants, so a check
// that returned any other Reason cannot add a code to the summary.
func TestReasonOfForwardsOnlyTheTLSReasons(t *testing.T) {
	for _, r := range []tlspin.Reason{tlspin.ReasonChainLen, tlspin.ReasonNotP384, tlspin.ReasonEKU,
		tlspin.ReasonUnpinned, tlspin.ReasonRevoked, tlspin.ReasonStore} {
		if got := reasonOf(fmt.Errorf("wrapped: %w", r), "fallback"); got != string(r) {
			t.Errorf("%s: %q", r, got)
		}
	}
	for name, err := range map[string]error{
		"an unknown reason":  tlspin.Reason("unknown-7q3z"),
		"an empty reason":    tlspin.Reason(""),
		"no reason":          errors.New("handshake: eof"),
		"a nil error":        nil,
		"a wrapped unknown":  fmt.Errorf("x: %w", tlspin.Reason("store ")),
		"a reason-like text": errors.New("unpinned"),
	} {
		if got := reasonOf(err, "fallback"); got != "fallback" {
			t.Errorf("%s: %q, want the fallback", name, got)
		}
	}
}
