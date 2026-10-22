package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"math/big"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/tlspin"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// The helpers Task 10's serve tests share with Task 12's transport suite.

func newKey(t *testing.T) *ecdsa.PrivateKey { return p384(t) }

func trustOf(t *testing.T, ds ...authoritytest.SignedDelegation) []byte {
	t.Helper()
	items := make([][]byte, 0, len(ds))
	for _, d := range ds {
		items = append(items, d.Signed)
	}
	b := ac.TrustEnvelope{Items: items}.Encode()
	if b == nil {
		t.Fatal("the trust file does not encode")
	}
	return b
}

func serials(t *testing.T, db *sql.DB) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT serial FROM authority.delegations ORDER BY product_root_gen, serial`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func inRecovery(v bool) func(context.Context, *sql.Conn) (bool, error) {
	return func(context.Context, *sql.Conn) (bool, error) { return v, nil }
}

// fastLoops shortens the tick and the role poll for one test.
func fastLoops(t *testing.T) {
	t.Helper()
	tick, role := store.TickInterval, store.RolePollInterval
	store.TickInterval, store.RolePollInterval = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { store.TickInterval, store.RolePollInterval = tick, role })
}

// slowLoops pushes the tick and the role poll out of a test's way, so only
// what runs before READY=1 has run.
func slowLoops(t *testing.T) {
	t.Helper()
	tick, role := store.TickInterval, store.RolePollInterval
	store.TickInterval, store.RolePollInterval = time.Hour, time.Hour
	t.Cleanup(func() { store.TickInterval, store.RolePollInterval = tick, role })
}

func baseDeps(f *authoritydb.Fixture, rl *authoritytest.Realm, trust []byte) Deps {
	return Deps{DB: f.App, RSK: rl.RSK, TLSKey: rl.TLSKey, Anchors: rl.AnchorBytes, TrustFile: trust}
}

type running struct {
	addr   string
	notes  *notes
	cancel context.CancelFunc
	done   chan error
	err    error
	ended  bool
}

// startRun runs Run over d on a loopback listener; cleanup stops it.
func startRun(t *testing.T, d Deps) *running {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &notes{}
	d.Listen, d.Notify = ln, n.add
	if d.Log == nil {
		d.Log = quiet
	}
	if d.Statfs == nil {
		d.Statfs = func() (uint64, uint64, error) { return 1 << 40, 1 << 41, nil }
	}
	if d.Clock == nil {
		d.Clock = func() bool { return true }
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{addr: ln.Addr().String(), notes: n, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- Run(ctx, d) }()
	t.Cleanup(func() { _ = r.stop(t); _ = ln.Close() })
	return r
}

// result waits for Run to return and gives its error.
func (r *running) result(t *testing.T) error {
	t.Helper()
	if !r.ended {
		select {
		case r.err = <-r.done:
			r.ended = true
		case <-time.After(30 * time.Second):
			t.Fatal("Run did not return")
		}
	}
	return r.err
}

func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	return r.result(t)
}

// ready waits for READY=1 and fails if Run returns first.
func (r *running) ready(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); r.notes.count("READY=1") == 0; time.Sleep(5 * time.Millisecond) {
		select {
		case r.err = <-r.done:
			r.ended = true
			t.Fatalf("Run returned before READY=1: %v", r.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no READY=1")
		}
	}
}

// cpClient is a CP using tlspin's client side, pinned to the given authority TLS keys.
func cpClient(t *testing.T, cpKey *ecdsa.PrivateKey, authorityTLS ...*ecdsa.PrivateKey) *http.Client {
	t.Helper()
	cert, err := tlspin.Mint(cpKey, x509.ExtKeyUsageClientAuth)
	if err != nil {
		t.Fatal(err)
	}
	points := make([][]byte, 0, len(authorityTLS))
	for _, k := range authorityTLS {
		points = append(points, authoritytest.Point(k))
	}
	cfg, err := tlspin.ClientConfig(cert, points)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
}

func getSigner(t *testing.T, c *http.Client, addr string) ac.Signer {
	t.Helper()
	resp, err := c.Get("https://" + addr + "/v1/signer")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/signer: %d %q %v", resp.StatusCode, b, err)
	}
	m, err := ac.DecodeSigner(b)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// served is one Run over a fresh realm, with its JSON log captured.
type served struct {
	f   *authoritydb.Fixture
	rl  *authoritytest.Realm
	r   *running
	log *syncBuffer
}

func serveRealm(t *testing.T, mutate func(*Deps)) *served {
	t.Helper()
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	buf := &syncBuffer{}
	d := baseDeps(f, rl, trustOf(t, rl.Current))
	d.Log = slog.New(slog.NewJSONHandler(buf, nil))
	if mutate != nil {
		mutate(&d)
	}
	r := startRun(t, d)
	r.ready(t)
	return &served{f: f, rl: rl, r: r, log: buf}
}

// clientConfig is the CP's config for cert, pinned to the realm's authority key.
func (s *served) clientConfig(t *testing.T, cert tls.Certificate) *tls.Config {
	t.Helper()
	cfg, err := tlspin.ClientConfig(cert, [][]byte{authoritytest.Point(s.rl.TLSKey)})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func mint(t *testing.T, k *ecdsa.PrivateKey, eku x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	c, err := tlspin.Mint(k, eku)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// rawCert is a self-signed certificate for key with exactly the given EKUs
// and the minted leaf's DN.
func rawCert(t *testing.T, key crypto.Signer, ekus ...x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: tlspin.ServerName},
		NotBefore: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: ekus}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// reaches reports whether cfg completes a handshake and a GET /v1/signer
// with 200. In TLS 1.3 the client finishes before the server judges its
// certificate, so a refusal shows on the first read.
func reaches(addr string, cfg *tls.Config) bool {
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, cfg)
	if err != nil {
		return false
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, "GET /v1/signer HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
		return false
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// refusedAs connects as cfg, reads once without sending a request, and
// names what the client saw (see kind): "alert" for a refusal inside the
// handshake, "reset" for the listener's bare reset after it, "open" when the
// connection stayed up, as an admitted one does.
func refusedAs(addr string, cfg *tls.Config) string {
	raw, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return "dial: " + err.Error()
	}
	defer func() { _ = raw.Close() }()
	// 5 s, not less: a revoked_keys read under -race on a loaded runner must
	// not outlast the read and pass for an admitted connection (M6).
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	var b [1]byte
	n, err := tls.Client(raw, cfg).Read(b[:]) // the handshake runs first
	var ne net.Error
	switch {
	case n > 0:
		return "data"
	case errors.As(err, &ne) && ne.Timeout():
		return "open"
	}
	return kind(err)
}

func call(t *testing.T, c *http.Client, addr, method, path string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "https://"+addr+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

// logLines decodes every JSON line of b.
func logLines(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// tlsSummaries is summaries over b's authority.tls lines alone.
func tlsSummaries(t *testing.T, b []byte) map[string]summary {
	t.Helper()
	var only bytes.Buffer
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["msg"] == "authority.tls" {
			only.Write(line)
			only.WriteByte('\n')
		}
	}
	return summaries(t, only.Bytes())
}

// checkLogConvention is carry 19's convention (DoR §3.6, B2-D32): every line carries
// op, outcome is a store.Kind as a number, and a line holds only the allowed
// fields: count on authority.tls alone, and reason and error on
// authority.exit alone.
func checkLogConvention(t *testing.T, b []byte) {
	t.Helper()
	base := []string{"level", "msg", "time", "op", "outcome", "code", "request_id", "latency_ms"}
	for _, m := range logLines(t, b) {
		allowed := slices.Clone(base)
		switch m["msg"] {
		case "authority.tls":
			allowed = append(allowed, "count")
		case "authority.exit":
			allowed = append(allowed, "reason", "step", "error")
		case "authority.purge":
			allowed = append(allowed, "complete")
		}
		for k := range maps.Keys(m) {
			if !slices.Contains(allowed, k) {
				t.Errorf("line %v carries %q", m, k)
			}
		}
		if op, _ := m["op"].(string); op == "" {
			t.Errorf("line %v carries no op", m)
		}
		if o, ok := m["outcome"].(float64); !ok || o < 0 || o > float64(store.KindOK) || o != float64(int(o)) {
			t.Errorf("line %v: outcome is not a store.Kind", m)
		}
	}
}
