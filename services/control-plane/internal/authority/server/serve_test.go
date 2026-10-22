package server

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/tlspin"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// N2 (carry 5) and B2-D10: LoadTrust's own refusals are trust, D-223's reuse
// bullet among them; a missing meta row and a database error are config.
func TestTrustReason(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: the anchors name another", store.ErrRealmMismatch), ReasonTrust},
		{fmt.Errorf("%w: gen 0 serial 2", store.ErrTrustNotYetValid), ReasonTrust},
		{errors.Join(store.ErrTrustUnusable, errors.New("halt")), ReasonTrust},
		{store.ErrRSKReused, ReasonTrust},
		{fmt.Errorf("load: %w", store.ErrRSKReused), ReasonTrust},
		{store.ErrNoMeta, ReasonConfig},
		{sql.ErrConnDone, ReasonConfig},
		{errors.New("pq: canceling statement due to statement timeout"), ReasonConfig},
	} {
		if got := trustReason(c.err); got != c.want {
			t.Errorf("trustReason(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}

// D-223 (B2-D20): this node's RSK and TLS key must be in HW. The reuse
// bullet is the store's (N2), so checkNodeKeys judges HW alone.
func TestCheckNodeKeys(t *testing.T) {
	rsk, err := signer.GenerateP384()
	if err != nil {
		t.Fatal(err)
	}
	tlsKey, other := newKey(t), newKey(t)
	hw := ac.Delegation{RSKPublicKeys: [][]byte{rsk.PublicKey()}, AuthorityTLSKeys: [][]byte{authoritytest.Point(tlsKey)}}
	if err := checkNodeKeys(hw, rsk.PublicKey(), tlsKey); err != nil {
		t.Fatalf("both keys listed: %v", err)
	}
	if err := checkNodeKeys(hw, authoritytest.Point(other), tlsKey); err == nil {
		t.Error("an RSK not in HW passed")
	}
	if err := checkNodeKeys(hw, rsk.PublicKey(), other); err == nil {
		t.Error("a TLS key not in HW passed")
	}
}

// revokedFake is the revoked_keys read pinCheck makes.
type revokedFake struct {
	revoked bool
	err     error
	boom    bool
	calls   atomic.Int32
	got     []byte
	dl      time.Duration
}

func (f *revokedFake) read(ctx context.Context, digest []byte) (bool, error) {
	f.calls.Add(1)
	f.got = digest
	if d, ok := ctx.Deadline(); ok {
		f.dl = time.Until(d)
	}
	if f.boom {
		panic("boom")
	}
	return f.revoked, f.err
}

// E1 (carry 6): the listener's check is pin membership in HW's cp_tls_pins,
// then revoked_keys, read uncached on its own read deadline. An unpinned
// peer never costs a read; a failed or panicking read refuses as store.
func TestPinCheck(t *testing.T) {
	pin, other := bytes.Repeat([]byte{1}, 48), bytes.Repeat([]byte{2}, 48)
	for _, c := range []struct {
		name  string
		f     *revokedFake
		pin   []byte
		want  error
		reads int32
	}{
		{"pinned, not revoked", &revokedFake{}, pin, nil, 1},
		{"unpinned", &revokedFake{}, other, tlspin.ReasonUnpinned, 0},
		{"pinned, revoked", &revokedFake{revoked: true}, pin, tlspin.ReasonRevoked, 1},
		{"pinned, the read failed", &revokedFake{err: errors.New("db down")}, pin, tlspin.ReasonStore, 1},
		{"pinned, the read failed and said revoked", &revokedFake{revoked: true, err: errors.New("db down")}, pin, tlspin.ReasonStore, 1},
		{"pinned, the read panicked", &revokedFake{boom: true}, pin, tlspin.ReasonStore, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			buf := &syncBuffer{}
			check := pinCheck([][]byte{pin}, c.f.read, slog.New(slog.NewJSONHandler(buf, nil)))
			var err error
			func() {
				defer func() {
					if v := recover(); v != nil {
						t.Fatalf("the check panicked: %v", v)
					}
				}()
				err = check(c.pin)
			}()
			if !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
				t.Fatalf("check = %v, want %v", err, c.want)
			}
			if n := c.f.calls.Load(); n != c.reads {
				t.Fatalf("%d revoked_keys reads, want %d", n, c.reads)
			}
			if c.reads == 1 && (!bytes.Equal(c.f.got, c.pin) || c.f.dl <= 0 || c.f.dl > store.RequestDeadlineRead) {
				t.Fatalf("read digest %x on a deadline %v, want the pin on at most %v", c.f.got, c.f.dl, store.RequestDeadlineRead)
			}
			// A recovered panic is one fixed authority.panic line, op tls.
			panics := lines(t, buf.Bytes(), "authority.panic")
			if want := b01(c.f.boom); uint64(len(panics)) != want || (want == 1 && panics[0]["op"] != opTLS) ||
				bytes.Contains(buf.Bytes(), []byte("boom")) {
				t.Fatalf("panic lines %v: want %d, op tls, no panic value", panics, want)
			}
		})
	}
	t.Run("the check holds its own copy of the pins", func(t *testing.T) {
		pins := [][]byte{bytes.Clone(pin)}
		check := pinCheck(pins, (&revokedFake{}).read, quiet)
		pins[0][0] = 9 // fixed until restart (D-166): a later write to HW's slice changes nothing
		if err := check(pin); err != nil {
			t.Fatalf("check = %v after the caller's slice changed", err)
		}
	})
}

// Carry 10: fatal is idempotent. The first records its error and cancels
// serve's context once; a loop still polling until that cancel lands calls
// it again and changes nothing. A fatal with no error still stops serve as a
// failure, never as a clean exit.
func TestStopperKeepsTheFirstFatal(t *testing.T) {
	var cancels atomic.Int32
	s := newStopper(func() { cancels.Add(1) })
	if err := s.result(); err != nil {
		t.Fatalf("result before any fatal: %v", err)
	}
	first, second := refuse(ReasonLock, errors.New("first")), refuse(ReasonConfig, errors.New("second"))
	s.fatal(first)
	s.fatal(second)
	if err := s.result(); err != first {
		t.Fatalf("result %v, want the first fatal", err)
	}
	if n := cancels.Load(); n != 1 {
		t.Fatalf("cancelled %d times, want 1", n)
	}
	s = newStopper(func() {})
	s.fatal(nil)
	if exitReason(s.result()) != ReasonConfig {
		t.Fatalf("a nil fatal gave %v, want reason config", s.result())
	}
}

// DoR:1624 and carry 1, 2, 8: the http.Server serve builds.
func TestHTTPServerParameters(t *testing.T) {
	var released []net.Conn
	h := http.NotFoundHandler()
	hs := newHTTPServer(h, func(c net.Conn) { released = append(released, c) }, quiet)
	switch {
	case hs.ReadHeaderTimeout != readHeaderTimeout || hs.ReadTimeout != readTimeout:
		// C3: bodyDeadline derives its bound from these two constants.
		t.Errorf("ReadHeaderTimeout %v, ReadTimeout %v: not the http.go constants", hs.ReadHeaderTimeout, hs.ReadTimeout)
	case readHeaderTimeout != 5*time.Second || readTimeout != 10*time.Second:
		t.Errorf("the constants are %v and %v, want DoR:1624's 5 s and 10 s", readHeaderTimeout, readTimeout)
	case hs.WriteTimeout != 15*time.Second || hs.IdleTimeout != 60*time.Second || hs.MaxHeaderBytes != 8<<10:
		t.Errorf("write %v, idle %v, header cap %d", hs.WriteTimeout, hs.IdleTimeout, hs.MaxHeaderBytes)
	case hs.TLSNextProto == nil || len(hs.TLSNextProto) != 0:
		t.Error("TLSNextProto must be empty and non-nil: HTTP/1.1 only")
	case !hs.DisableGeneralOptionsHandler:
		t.Error("OPTIONS * would be answered by net/http, past the peer check (R1)")
	case hs.ErrorLog == nil:
		t.Error("a nil ErrorLog writes net/http's text, a peer's address and a stack, to stderr (S1)")
	case hs.Handler == nil:
		t.Error("no handler")
	}
	a, b := &net.TCPConn{}, &net.TCPConn{}
	for _, s := range []http.ConnState{http.StateNew, http.StateActive, http.StateIdle} {
		hs.ConnState(a, s)
	}
	hs.ConnState(a, http.StateClosed)
	hs.ConnState(b, http.StateHijacked)
	if len(released) != 2 || released[0] != a || released[1] != b {
		t.Errorf("released %v, want the closed and the hijacked connection", released)
	}
}

// S1 (carry 2): net/http's error lines carry a peer's address and a panic's
// stack. Each becomes one fixed line that carries neither.
func TestHTTPErrorLogDropsTheText(t *testing.T) {
	buf := &syncBuffer{}
	hs := newHTTPServer(http.NotFoundHandler(), func(net.Conn) {}, slog.New(slog.NewJSONHandler(buf, nil)))
	hs.ErrorLog.Printf("http: panic serving 10.9.8.7:4321: boom\ngoroutine 7 [running]:\nmain.f()")
	hs.ErrorLog.Printf("http: superfluous response.WriteHeader call from 10.9.8.7:4321")
	got := buf.Bytes()
	for _, leak := range []string{"10.9.8.7", "4321", "goroutine", "boom", "WriteHeader"} {
		if bytes.Contains(got, []byte(leak)) {
			t.Errorf("the log carries %q: %s", leak, got)
		}
	}
	ls := logLines(t, got)
	if len(ls) != 2 || ls[0]["msg"] != "authority.http" || ls[0]["op"] != "authority.http" ||
		ls[0]["outcome"] != float64(store.KindError) {
		t.Fatalf("lines %v, want two authority.http lines", ls)
	}
	checkLogConvention(t, got)
}

// countingConnector is a database that counts dials and never connects.
type countingConnector struct{ n atomic.Int32 }

func (c *countingConnector) Connect(context.Context) (driver.Conn, error) {
	c.n.Add(1)
	return nil, errors.New("no database here")
}
func (c *countingConnector) Driver() driver.Driver { return nil }

// blockingConnector is a database whose dial waits for its context.
type blockingConnector struct{}

func (blockingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingConnector) Driver() driver.Driver { return nil }

// M2 (fix round 1): the role check, Run's first query, runs under its own
// RequestDeadlineTx. database/sql hands the caller's context to Connect, so
// without it a dial that never ends holds Run for ever.
func TestRunBoundsTheRoleCheck(t *testing.T) {
	deadline := store.RequestDeadlineTx
	store.RequestDeadlineTx = 200 * time.Millisecond
	t.Cleanup(func() { store.RequestDeadlineTx = deadline })
	rsk, err := signer.GenerateP384()
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(blockingConnector{})
	t.Cleanup(func() { _ = db.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Deps{DB: db, RSK: rsk, TLSKey: newKey(t), Anchors: []byte("a"), TrustFile: []byte("t"),
			Listen: ln, Statfs: func() (uint64, uint64, error) { return 1, 1, nil }, Clock: func() bool { return true },
			Notify: func(string) {}, Log: quiet})
	}()
	select {
	case err := <-done:
		if exitReason(err) != ReasonConfig || exitStep(err) != StepRole {
			t.Fatalf("Run: %v, want reason config at step role", err)
		}
	case <-time.After(1500 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("Run still waits 1.5 s into the role check: it has no deadline of its own")
	}
}

// Run refuses a Deps with a missing dependency as config, before any I/O,
// rather than panic on it later or serve without it.
func TestRunRefusesIncompleteDeps(t *testing.T) {
	rsk, err := signer.GenerateP384()
	if err != nil {
		t.Fatal(err)
	}
	conn := &countingConnector{}
	db := sql.OpenDB(conn)
	t.Cleanup(func() { _ = db.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	full := Deps{DB: db, RSK: rsk, TLSKey: newKey(t), Anchors: []byte("a"), TrustFile: []byte("t"), Listen: ln,
		Statfs: func() (uint64, uint64, error) { return 1, 1, nil }, Clock: func() bool { return true },
		Notify: func(string) {}, Log: quiet}
	for _, c := range []struct {
		name string
		drop func(*Deps)
	}{
		{"DB", func(d *Deps) { d.DB = nil }},
		{"RSK", func(d *Deps) { d.RSK = nil }},
		{"TLSKey", func(d *Deps) { d.TLSKey = nil }},
		{"Listen", func(d *Deps) { d.Listen = nil }},
		{"Statfs", func(d *Deps) { d.Statfs = nil }},
		{"Clock", func(d *Deps) { d.Clock = nil }},
		{"Notify", func(d *Deps) { d.Notify = nil }},
		{"Log", func(d *Deps) { d.Log = nil }},
	} {
		d := full
		c.drop(&d)
		var err error
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("%s: Run panicked: %v", c.name, v)
				}
			}()
			err = Run(context.Background(), d)
		}()
		if exitReason(err) != ReasonConfig || exitStep(err) != StepDeps {
			t.Errorf("%s missing: %v, want reason config at step deps", c.name, err)
		}
	}
	if n := conn.n.Load(); n != 0 {
		t.Fatalf("Run dialled the database %d times before refusing", n)
	}
}

// N4 and carry 15: the loops serve runs. The schema check is wired, over the
// pool serve's startup check used; the intervals are the store's, 30 s and
// 5 s; pending is the start-time trust view's.
func TestServeWiresTheLoops(t *testing.T) {
	if store.TickInterval != 30*time.Second || store.RolePollInterval != 5*time.Second {
		t.Fatalf("intervals %v and %v, want 30 s and 5 s", store.TickInterval, store.RolePollInterval)
	}
	db := sql.OpenDB(&countingConnector{})
	_ = db.Close()
	n := &node{d: Deps{DB: db, Notify: func(string) {}, Log: quiet}, pending: true,
		mark: store.TrustMark{Successions: 2, Delegations: 3}}
	lp := n.loops(nil, func(error) {})
	switch {
	case lp.trust == nil:
		t.Error("the promotion's installed-trust read is unset (Sec-M2)")
	case lp.mark != n.mark:
		t.Errorf("mark %+v, want the start-time view's %+v", lp.mark, n.mark)
	case lp.tickEvery != store.TickInterval || lp.roleEvery != store.RolePollInterval:
		t.Errorf("tick every %v, role every %v", lp.tickEvery, lp.roleEvery)
	case !lp.pending:
		t.Error("pending is not the start-time view's")
	case lp.schema == nil || lp.fatal == nil || lp.notify == nil || lp.log == nil:
		t.Error("a loop dependency is unset")
	}
	if lp.schema != nil {
		if err := lp.schema(context.Background()); err == nil || !strings.Contains(err.Error(), "database is closed") {
			t.Errorf("the schema check did not read serve's pool: %v", err)
		}
	}
}

// A stop asked for during startup is a clean stop, nil, not
// reason=config: before Run starts, and while a step waits on the database.
// TestRunBoundsTheRoleCheck is the control: a step's own deadline, with ctx
// live, still refuses.
func TestRunStoppedDuringStartupIsClean(t *testing.T) {
	deadline := store.RequestDeadlineTx
	store.RequestDeadlineTx = 10 * time.Second
	t.Cleanup(func() { store.RequestDeadlineTx = deadline })
	rsk, err := signer.GenerateP384()
	if err != nil {
		t.Fatal(err)
	}
	deps := func(db *sql.DB) Deps {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return Deps{DB: db, RSK: rsk, TLSKey: newKey(t), Anchors: []byte("a"), TrustFile: []byte("t"), Listen: ln,
			Statfs: func() (uint64, uint64, error) { return 1, 1, nil }, Clock: func() bool { return true },
			Notify: func(string) {}, Log: quiet}
	}
	db := sql.OpenDB(&countingConnector{})
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, deps(db)); err != nil {
		t.Fatalf("Run on an ended context: %v, want nil", err)
	}
	blocked := sql.OpenDB(blockingConnector{})
	t.Cleanup(func() { _ = blocked.Close() })
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, deps(blocked)) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run stopped inside the role check: %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return 3 s after the stop")
	}
}

// exitStep is the step an ExitError names, or "".
func exitStep(err error) string {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Step
	}
	return ""
}

// A D-223 refusal names how many trust-file items did not verify,
// the count alone, and still wraps the key error.
func TestWithIgnored(t *testing.T) {
	if got := withIgnored(errNodeRSK, 0); got != errNodeRSK {
		t.Fatalf("no ignored items changed the error: %v", got)
	}
	got := withIgnored(errNodeRSK, 2)
	if !errors.Is(got, errNodeRSK) || !strings.Contains(got.Error(), "(2 trust-file items did not verify)") {
		t.Fatalf("withIgnored = %v", got)
	}
}
