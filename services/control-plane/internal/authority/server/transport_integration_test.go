package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// The B2 transport suite (DoR:1661-1669), driven through Run on a loopback
// listener so every case exercises the wiring and not only tlspin's configs.
// The handshake matrix and the revoked-pin keep-alive case are Task 10's
// TestHandshakeRefusalsThroughServe and TestRevokedPinClosesKeepAliveConnection
// in serve_integration_test.go.

// DoR:1631-1636 and X1 end to end; no resumption even when the client keeps
// a session cache (mutation 4). Each connection is a fresh one, so a second
// handshake that resumed would show DidResume.
func TestTransportParameters(t *testing.T) {
	s := serveRealm(t, nil)
	cfg := s.clientConfig(t, mint(t, s.rl.CPTLS, x509.ExtKeyUsageClientAuth))
	cfg.ClientSessionCache = tls.NewLRUClientSessionCache(4) // the server must still refuse to resume
	for i := range 3 {
		c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}}
		resp, err := c.Get("https://" + s.r.addr + "/v1/signer")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		st := resp.TLS
		suiteOK := st.CipherSuite == tls.TLS_AES_128_GCM_SHA256 || st.CipherSuite == tls.TLS_CHACHA20_POLY1305_SHA256
		if resp.Proto != "HTTP/1.1" || st.Version != tls.VersionTLS13 || st.CurveID != tls.SecP384r1MLKEM1024 ||
			!suiteOK || st.NegotiatedProtocol != "http/1.1" || st.DidResume {
			t.Fatalf("connection %d: %s, version %x, curve %v, suite %x, ALPN %q, resumed %v",
				i, resp.Proto, st.Version, st.CurveID, st.CipherSuite, st.NegotiatedProtocol, st.DidResume)
		}
	}
}

// Review Focus 1 (mutation 10): the role atomic still says leader, but the
// database is now in recovery. The store's per-request check decides: 503
// not_leader and no signed byte, on a commit and on a read. The role poll and
// the tick sit an hour away, so nothing but the per-request check can notice
// the demotion. B2 scope only: B3's operator verbs join this test with B3
// (R3).
func TestStaleLeaderAtomicStillRefusesInRecovery(t *testing.T) {
	slowLoops(t)
	var recovering atomic.Bool
	s := serveRealm(t, func(d *Deps) {
		d.Hooks.InRecovery = func(context.Context, *sql.Conn) (bool, error) { return recovering.Load(), nil }
	})
	c := cpClient(t, s.rl.CPTLS, s.rl.TLSKey)
	if m := getSigner(t, c, s.r.addr); m.Role != "leader" {
		t.Fatalf("before the demotion: role %q, want leader", m.Role)
	}
	requests := []struct {
		method, path string
		body         []byte
	}{
		{"POST", "/v1/transactions", s.rl.NewAccount(t).EnrollNew(t).Bytes},
		{"GET", "/v1/accounts/" + uuid.NewString() + "/head", nil},
	}
	// Control: while the database is a primary, neither request is refused as
	// not_leader, so the refusals below come from the demotion alone.
	for _, req := range requests {
		if _, body := call(t, c, s.r.addr, req.method, req.path, req.body); bytes.Equal(body, envelope(ac.ErrorNotLeader)) {
			t.Fatalf("%s %s on a primary: not_leader", req.method, req.path)
		}
	}
	recovering.Store(true)
	if m := getSigner(t, c, s.r.addr); m.Role != "leader" {
		t.Fatalf("the role atomic moved on its own: %q (the poll is an hour away)", m.Role)
	}
	for _, req := range requests {
		code, body := call(t, c, s.r.addr, req.method, req.path, req.body)
		if code != http.StatusServiceUnavailable || !bytes.Equal(body, envelope(ac.ErrorNotLeader)) {
			t.Errorf("%s %s in recovery: %d %q", req.method, req.path, code, body)
		}
	}
}

// DoR:1665, through serve: a standby answers /v1/signer with role standby
// and everything else with not_leader. The delegation is installed first, as
// a standby's leader would have: /v1/signer reports the installed trust and
// is 503 signer without one, on a standby as on a leader.
func TestStandbyThroughServe(t *testing.T) {
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	rl.Install(t, f.App, rl.Current)
	d := baseDeps(f, rl, trustOf(t, rl.Current))
	d.Hooks.InRecovery = inRecovery(true)
	r := startRun(t, d)
	r.ready(t)
	c := cpClient(t, rl.CPTLS, rl.TLSKey)
	if code, body := call(t, c, r.addr, "GET", "/v1/pending-heads", nil); code != http.StatusServiceUnavailable ||
		!bytes.Equal(body, envelope(ac.ErrorNotLeader)) {
		t.Fatalf("pending-heads on a standby: %d %q", code, body)
	}
	code, body := call(t, c, r.addr, "GET", "/v1/signer", nil)
	if code != http.StatusOK {
		t.Fatalf("signer on a standby: %d %q", code, body)
	}
	if m, err := ac.DecodeSigner(body); err != nil || m.Role != "standby" || m.DelegationSerial != 1 {
		t.Fatalf("signer on a standby: %+v %v", m, err)
	}
}

// commit POSTs b and requires a `committed` reply, then syncs a's head (B1's
// env.commitOK, over HTTP).
func (s *served) commit(t *testing.T, c *http.Client, a *authoritytest.Account, b []byte) {
	t.Helper()
	code, body := call(t, c, s.r.addr, "POST", "/v1/transactions", b)
	if _, err := ac.DecodeCommitted(body); code != http.StatusOK || err != nil {
		t.Fatalf("want committed, got %d %q", code, body)
	}
	a.Sync(t, s.f.App)
}

// enrolledPending enrolls an account with a known genesis device (B1's
// env.enrolledWith, over HTTP) and commits a replace_root for the cancel to
// clear.
func (s *served) enrolledPending(t *testing.T, c *http.Client) *authoritytest.Account {
	t.Helper()
	a := s.rl.NewAccount(t)
	dev := authoritytest.NewDevice(t)
	s.commit(t, c, a, a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, NewDevice: dev,
		Factors: []string{ac.FactorRegistration}}).Bytes)
	a.Activated()
	a.Actor = dev
	a.Devices[dev.ID] = dev
	s.commit(t, c, a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	return a
}

// Review Focus 4 (B2-D18, LD-25), through serve's watcher and shared
// Admission: once the disk crosses the soft floor, an ordinary commit is 503
// disk, a cancel still commits, and /v1/signer answers with disk_ready = 0.
// B1 proves the cancel exemption on diskCheck alone (admission_test.go:83);
// this proves serve's watcher feeds the Admission the store reads.
func TestSoftFloorRefusesCommitsButNotCancelsOrSigner(t *testing.T) {
	fastLoops(t) // the watcher samples on each tick
	const gib = uint64(1) << 30
	var low atomic.Bool
	s := serveRealm(t, func(d *Deps) {
		d.Statfs = func() (uint64, uint64, error) {
			if low.Load() {
				return 9 * gib, 100 * gib, nil // below the soft floor max(2 GiB, total/10) = 10 GiB, above the 512 MiB hard floor
			}
			return 1 << 40, 1 << 41, nil
		}
	})
	if _, err := s.f.Owner.Exec(`UPDATE authority.meta SET legacy_sealed_at = clock_timestamp(), population_digest = $1`,
		bytes.Repeat([]byte{4}, 48)); err != nil { // enroll_new needs the seal (B1's seal helper)
		t.Fatal(err)
	}
	c := cpClient(t, s.rl.CPTLS, s.rl.TLSKey)
	a := s.enrolledPending(t, c) // commits while the disk is roomy: the control for the refusal below
	if m := getSigner(t, c, s.r.addr); m.DiskReady != 1 {
		t.Fatalf("disk_ready %d before the disk fills, want 1", m.DiskReady)
	}

	low.Store(true)
	waitFor(t, "disk_ready = 0", func() bool { return getSigner(t, c, s.r.addr).DiskReady == 0 })
	code, body := call(t, c, s.r.addr, "POST", "/v1/transactions", a.Suspend(t, ac.ActorUser).Bytes)
	if code != http.StatusServiceUnavailable || !bytes.Equal(body, envelope(ac.ErrorDisk)) {
		t.Fatalf("an ordinary commit at the soft floor: %d %q", code, body)
	}
	s.commit(t, c, a, a.CancelByRoot(t).Bytes) // D-211/LD-25: cancel_pending is exempt
}

// DoR:1666: with every pre-handshake slot held by a stranger, a warm CP
// connection still completes /v1/signer. Mutation 6 is Task 7's.
//
// stall returns when the TCP connects complete, not when the listener has
// taken the slots, so saturation is established before the warm request: a
// fresh CP handshake is dialled until one cannot finish within a short
// timeout. The accept loop is first in first out, so such a dial is accepted
// only after all 32 strangers hold their slots, and none is free for it. The
// pending probe afterwards is a second witness at the warm request itself.
func TestWarmConnectionSurvivesASaturatedHandshakeBudget(t *testing.T) {
	s := serveRealm(t, nil)
	cfg := s.clientConfig(t, mint(t, s.rl.CPTLS, x509.ExtKeyUsageClientAuth))
	warm := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	getSigner(t, warm, s.r.addr) // warms one keep-alive connection
	stall(t, s.r.addr, preHandshakeBudget)
	waitFor(t, "the pre-handshake budget to fill", func() bool {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: 500 * time.Millisecond}, "tcp", s.r.addr, cfg)
		if err == nil {
			_ = c.Close()
		}
		return err != nil // a handshake that cannot finish is waiting for a slot
	})

	probe := make(chan error, 1) // buffered: the goroutine never outlives its send
	go func() {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * handshakeTimeout}, "tcp", s.r.addr, cfg)
		if err == nil {
			_ = c.Close()
		}
		probe <- err
	}()
	// A request that waited for a slot would wait out the strangers' 5 s
	// deadline; the client's 4 s timeout fails it first.
	getSigner(t, warm, s.r.addr)
	select {
	case err := <-probe:
		t.Fatalf("a fresh CP handshake completed with every pre-handshake slot held: %v", err)
	default:
	}
}
