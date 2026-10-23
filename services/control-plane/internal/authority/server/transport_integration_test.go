package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"errors"
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
// the demotion. The operator routes join it (B3 R3): legacy-population,
// reconcile/required, /import and /hold.
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
	// The operator routes (B3, R3). Each body must DECODE: the prologue decodes
	// before it takes a connection, so a body that does not decode is 400 on a
	// leader and in recovery alike and says nothing about the leader check. The
	// seal and the committed account give them one: a legacy-batch row, and the
	// account's seq-1 bundle as a hold-req bundle and as an import-item.
	if _, err := s.f.Owner.Exec(`UPDATE authority.meta SET legacy_sealed_at = clock_timestamp(), population_digest = $1`,
		bytes.Repeat([]byte{4}, 48)); err != nil {
		t.Fatal(err)
	}
	acct := s.rl.NewAccount(t)
	s.commit(t, c, acct, acct.EnrollNew(t).Bytes)
	var bundle, evidence, devBundle []byte
	var op string
	if err := s.f.App.QueryRow(`SELECT bundle, cp_evidence, op FROM authority.account_events WHERE account_id = $1 AND seq = 1`,
		acct.ID).Scan(&bundle, &evidence, &op); err != nil {
		t.Fatal(err)
	}
	// enroll_new is an attached op: its import-item carries the device entry too.
	if err := s.f.App.QueryRow(`SELECT bundle FROM authority.device_events WHERE account_id = $1 AND chain_seq = 1 AND op = $2`,
		acct.ID, op).Scan(&devBundle); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	legacy := ac.LegacyBatch{Rows: []string{uuid.NewString() + "." + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 48))}}.Encode()
	holdReq := ac.HoldReq{Bundles: [][]byte{bundle}}.Encode()
	importBatch := ac.ImportBatch{Items: [][]byte{ac.ImportItem{Bundle: bundle, DeviceBundle: devBundle, CPEvidence: evidence}.Encode()}}.Encode()
	if legacy == nil || holdReq == nil || importBatch == nil {
		t.Fatal("a test operator body does not encode")
	}
	if _, err := ac.CheckHoldReq(holdReq); err != nil {
		t.Fatalf("the test hold-req does not pass the prologue's decode: %v", err)
	}
	if _, err := ac.CheckImportBatch(importBatch); err != nil {
		t.Fatalf("the test import-batch does not pass the prologue's decode: %v", err)
	}
	requests := []struct {
		method, path string
		body         []byte
		control      int // the real code on a primary; 0 = only "not not_leader" is asserted
	}{
		{"POST", "/v1/transactions", s.rl.NewAccount(t).EnrollNew(t).Bytes, 0},
		{"GET", "/v1/accounts/" + uuid.NewString() + "/head", nil, 0},
		{"POST", "/v1/legacy-population", legacy, http.StatusConflict},
		{"POST", "/v1/reconcile/required", holdReq, http.StatusBadRequest},
		{"POST", "/v1/reconcile/import", importBatch, http.StatusConflict},
		{"POST", "/v1/reconcile/hold", holdReq, http.StatusConflict},
	}
	// Control: while the database is a primary, no request is refused as
	// not_leader, so the refusals below come from the demotion alone. The four
	// operator routes also answer their real code, so a body the prologue
	// rejects before the leader check cannot pass for the demotion's refusal.
	for _, req := range requests {
		code, body := call(t, c, s.r.addr, req.method, req.path, req.body)
		if bytes.Equal(body, envelope(ac.ErrorNotLeader)) {
			t.Fatalf("%s %s on a primary: not_leader", req.method, req.path)
		}
		if req.control != 0 && code != req.control {
			t.Fatalf("%s %s on a primary: %d %q, want %d", req.method, req.path, code, body, req.control)
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
