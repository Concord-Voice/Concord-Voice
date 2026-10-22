package server

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lib/pq"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// B2-D32: a failed bind keeps its cause but never names the address.
func TestBindErrorDropsTheAddress(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	_, err = net.Listen("tcp", held.Addr().String())
	if err == nil {
		t.Fatal("a second bind of one address succeeded")
	}
	got := bindError(err).Error()
	if !strings.HasPrefix(got, "AUTHORITY_LISTEN_ADDR does not bind: ") || strings.Contains(got, "127.0.0.1") {
		t.Fatalf("%q", got)
	}
}

// openConfig is a serve Config over fresh credentials and trust files, with
// the database URL checked as Load checks it.
func openConfig(t *testing.T) Config {
	t.Helper()
	clearLibpqEnv(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a credentials directory is 0700
		t.Fatal(err)
	}
	for _, name := range []string{credRSK, credTLS} {
		der, err := x509.MarshalPKCS8PrivateKey(newKey(t))
		if err != nil {
			t.Fatal(err)
		}
		writeCred(t, dir, name, der, 0o400)
	}
	files := t.TempDir()
	for _, name := range []string{"anchors", "trust"} {
		if err := os.WriteFile(filepath.Join(files, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := checkedConfig(t, socketURL("authority_app", ""))
	c.ListenAddr, c.CredentialsDir, c.DiskWatchPath = netip.MustParseAddrPort("127.0.0.1:0"), dir, files
	c.AnchorsPath, c.TrustPath = filepath.Join(files, "anchors"), filepath.Join(files, "trust")
	return c
}

func TestOpenReadsCredentialsAndBinds(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	cfg := openConfig(t)
	d, closeFn, err := Open(cfg, quiet)
	if runtime.GOOS != "linux" {
		if exitReason(err) != "config" {
			t.Fatalf("Open on %s: %v, want reason config (B2-D16)", runtime.GOOS, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if d.DB == nil || d.RSK == nil || d.TLSKey == nil || d.Listen == nil || string(d.TrustFile) != "x" ||
		string(d.Anchors) != "x" || d.Clock == nil || d.Notify == nil || d.Log != quiet {
		t.Fatalf("%+v", d)
	}
	d.Notify("READY=1") // an empty NOTIFY_SOCKET is a no-op
	// Carry 14: Statfs is the watch path's statfs, both values.
	if avail, total, err := d.Statfs(); err != nil || total == 0 || avail > total {
		t.Fatalf("Statfs: %d of %d, %v", avail, total, err)
	}
	cfg.AnchorsPath = filepath.Join(filepath.Dir(cfg.TrustPath), "missing")
	if _, _, err := Open(cfg, quiet); exitReason(err) != "trust" {
		t.Fatalf("missing anchors: %v, want reason trust", err)
	}
}

// S3 (carry 4): the anchors and the trust file are read under the
// credentials' integrity rule, the only defence against substituted anchors
// on an empty database: the open refuses a symlink and cannot block on a
// FIFO, the open file must be a regular file owned by root or the euid that
// neither group nor other can write, in a parent neither can write, and the
// read stops at the codec's cap. A refusal names the variable, never the path.
func TestReadInputsRefusesAnUntrustedFile(t *testing.T) {
	cfg := openConfig(t)
	files := filepath.Dir(cfg.TrustPath)
	if a, tr, err := readInputs(cfg); err != nil || string(a) != "x" || string(tr) != "x" {
		t.Fatalf("readInputs: %q %q %v", a, tr, err)
	}
	if err := os.Symlink(cfg.AnchorsPath, filepath.Join(files, "link")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		set  func(*Config)
		v    string
	}{
		{"anchors through a symlink", func(c *Config) { c.AnchorsPath = filepath.Join(files, "link") }, "AUTHORITY_ANCHORS_PATH"},
		{"trust through a symlink", func(c *Config) { c.TrustPath = filepath.Join(files, "link") }, "AUTHORITY_TRUST_PATH"},
		{"anchors missing", func(c *Config) { c.AnchorsPath = filepath.Join(files, "missing") }, "AUTHORITY_ANCHORS_PATH"},
	} {
		c2 := cfg
		c.set(&c2)
		_, _, err := readInputs(c2)
		if err == nil || !strings.HasPrefix(err.Error(), c.v) || strings.Contains(err.Error(), files) {
			t.Errorf("%s: %v, want a refusal naming %s and no path", c.name, err, c.v)
		}
	}
}

func TestReadTrustFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- a public file's directory
		t.Fatal(err)
	}
	put := func(name string, b []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const limit = 64
	ok := map[string]string{
		"world-readable": put("public", []byte("ok"), 0o644),
		"owner only":     put("private", []byte("ok"), 0o600),
		"at the cap":     put("full", make([]byte, limit), 0o644),
	}
	if etc, err := filepath.EvalSymlinks("/etc"); err == nil {
		ok["owned by root"] = filepath.Join(etc, "hosts")
	}
	for name, p := range ok {
		if _, err := readTrustFile("V", p, 1<<20); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if b, err := readTrustFile("V", ok["at the cap"], limit); err != nil || len(b) != limit {
		t.Errorf("at the cap: %d bytes, %v", len(b), err)
	}
	if err := os.Symlink(ok["owner only"], filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"over the cap":        put("over", make([]byte, limit+1), 0o644),
		"group-writable":      put("gw", []byte("x"), 0o664),
		"other-writable":      put("ow", []byte("x"), 0o646),
		"a symlink":           filepath.Join(dir, "link"),
		"a directory":         filepath.Join(dir, "sub"),
		"missing":             filepath.Join(dir, "missing"),
		"a FIFO (no block)":   fifo(t, dir),
		"under a missing dir": filepath.Join(dir, "nodir", "f"),
	}
	for name, p := range bad {
		done := make(chan error, 1)
		go func() { _, err := readTrustFile("V", p, limit); done <- err }()
		select {
		case err := <-done:
			if err == nil || !strings.HasPrefix(err.Error(), "V") || strings.Contains(err.Error(), dir) {
				t.Errorf("%s: %v, want a refusal naming V and no path", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the read blocked", name)
		}
	}
	if err := os.Chmod(dir, 0o775); err != nil { // #nosec G302 -- the group-writable parent is the case under test
		t.Fatal(err)
	}
	if _, err := readTrustFile("V", ok["world-readable"], limit); err == nil {
		t.Error("a group-writable parent passed")
	}
}

// trustFileRule judges a file another uid owns, which a test cannot create.
func TestTrustFileRule(t *testing.T) {
	const euid = 1000
	for _, c := range []struct {
		name string
		mode os.FileMode
		uid  uint32
		ok   bool
	}{
		{"euid, 0644", 0o644, euid, true},
		{"root, 0444", 0o444, 0, true},
		{"another uid", 0o644, 1001, false},
		{"group write", 0o664, euid, false},
		{"other write", 0o646, euid, false},
		{"a directory", os.ModeDir | 0o755, euid, false},
	} {
		if err := trustFileRule(c.mode, c.uid, euid); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// The trust inputs' caps are the codec's: nothing it accepts is refused for size.
func TestTrustInputCapsAreTheCodecs(t *testing.T) {
	if anchorsMax != ac.CapAnchors || trustMax != ac.CapTrust {
		t.Fatalf("caps %d and %d, want the codec's %d and %d", anchorsMax, trustMax, ac.CapAnchors, ac.CapTrust)
	}
}

func fifo(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// pgSocket is a socket directory short enough for a unix socket path, which
// a t.TempDir can exceed. It sits one level below a 0700 directory in /tmp,
// because checkSocketDir refuses a parent other users can write (/tmp is 1777).
func pgSocket(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "cvpg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	sock := filepath.Join(root, "s")
	if err := os.Mkdir(sock, 0o700); err != nil {
		t.Fatal(err)
	}
	return sock
}

// sentBy dials c once through OpenDB and returns the password the fake
// server received.
func sentBy(t *testing.T, srv *fakeAuthPG, c Config) string {
	t.Helper()
	db, err := OpenDB(c)
	if err != nil {
		t.Fatal(err)
	}
	ping(t, db)
	select {
	case p := <-srv.got:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("lib/pq sent no password message")
		return ""
	}
}

// N1 (carry 3, 16): OpenDB dials the checked pq.Config, never the URL. A
// second parse of the URL drops the passfile sentinel, and lib/pq then reads
// $HOME/.pgpass on every connection. The control names a passfile through
// the same path and must see its password, so the harness can see one.
func TestOpenDBDialsTheCheckedConfig(t *testing.T) {
	clearLibpqEnv(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".pgpass"), []byte("*:*:*:authority_app:from-home\n"), 0o600); err != nil { // pragma: allowlist secret
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	sock := pgSocket(t)
	srv := newFakeAuthPG(t, filepath.Join(sock, ".s.PGSQL.5432"))
	cfg := checkedConfig(t, "postgres:///db?host="+sock+"&user=authority_app")
	if cfg.DB.Passfile != devNullChild {
		t.Fatalf("Passfile %q, want the sentinel", cfg.DB.Passfile)
	}
	if got := sentBy(t, srv, cfg); got != "" {
		t.Fatalf("OpenDB sent %q: it did not dial the checked Config's sentinel", got)
	}
	pf := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(pf, []byte("*:*:*:authority_app:from-file\n"), 0o600); err != nil { // pragma: allowlist secret
		t.Fatal(err)
	}
	control := cfg
	control.DB.Passfile = pf
	if got := sentBy(t, srv, control); got != "from-file" {
		t.Fatalf("control: OpenDB sent %q, want the named passfile's, so the harness cannot see the Config", got)
	}
}

// silentPG is a unix-socket server that accepts every connection, reads, and
// never answers: a database wedged in its startup exchange.
func silentPG(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()
}

// pingWithin pings db under a context of ctxTimeout and reports whether it
// returned within wait, and with what.
func pingWithin(db interface {
	PingContext(context.Context) error
}, ctxTimeout, wait time.Duration) (returned bool, err error) {
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
		defer cancel()
		done <- db.PingContext(ctx)
	}()
	select {
	case err := <-done:
		return true, err
	case <-time.After(wait):
		return false, nil
	}
}

// N1 (carry 3): the dial is bounded. A context deadline cannot do it: lib/pq
// v1.12.3 hands its context to the socket connect alone and runs the
// startup exchange with no deadline unless ConnectTimeout is set, and
// database/sql calls Connect synchronously, so a database that accepts and
// never answers would hold serve's start for ever. The control dials the
// same checked Config with only a context deadline and must still be
// waiting, which is what makes ConnectTimeout the bound.
func TestOpenDBBoundsTheStartupExchange(t *testing.T) {
	clearLibpqEnv(t)
	bound := dialTimeout
	dialTimeout = 200 * time.Millisecond
	t.Cleanup(func() { dialTimeout = bound })
	sock := pgSocket(t)
	silentPG(t, filepath.Join(sock, ".s.PGSQL.5432"))
	checked := checkedConfig(t, "postgres:///db?host="+sock+"&user=authority_app")
	conn, err := pq.NewConnectorConfig(checked.DB)
	if err != nil {
		t.Fatal(err)
	}
	ctxOnly := sql.OpenDB(conn)
	t.Cleanup(func() { _ = ctxOnly.Close() })
	if returned, err := pingWithin(ctxOnly, 300*time.Millisecond, 1500*time.Millisecond); returned {
		t.Fatalf("control: a context deadline alone ended the startup exchange (%v); lib/pq changed, revisit dialTimeout", err)
	}
	bounded, err := OpenDB(checked)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bounded.Close() })
	start := time.Now()
	returned, err := pingWithin(bounded, time.Hour, 3*time.Second)
	if !returned || err == nil {
		t.Fatalf("OpenDB's dial to a silent database: returned %v, %v; it is not bounded", returned, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the dial took %v, far past dialTimeout", d)
	}
}

// Sec-M3: OpenDB caps the pool at the derived figure, which the DoR records
// for #3593's max_connections; a budget change moves it and fails here until
// the DoR follows.
func TestOpenDBCapsThePool(t *testing.T) {
	clearLibpqEnv(t)
	if poolMaxOpen != 52 {
		t.Fatalf("poolMaxOpen %d: the DoR records 52 for #3593's max_connections; update both", poolMaxOpen)
	}
	db, err := OpenDB(checkedConfig(t, socketURL("authority_app", "")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got := db.Stats().MaxOpenConnections; got != poolMaxOpen {
		t.Fatalf("MaxOpenConnections %d, want %d", got, poolMaxOpen)
	}
}

// checkedConfig is a Config whose database URL went through setDB, the path
// Load takes.
func checkedConfig(t *testing.T, raw string) Config {
	t.Helper()
	var c Config
	if err := setDB(&c, raw, "authority_app"); err != nil {
		t.Fatal(err)
	}
	return c
}

// M1 (fix round 1): the URL's connect_timeout cannot lift dialTimeout. An
// operator's connect_timeout=100000000 would otherwise hold a dial to a
// silent database for three years.
func TestOpenDBCapsTheURLConnectTimeout(t *testing.T) {
	clearLibpqEnv(t)
	bound := dialTimeout
	dialTimeout = 200 * time.Millisecond
	t.Cleanup(func() { dialTimeout = bound })
	sock := pgSocket(t)
	silentPG(t, filepath.Join(sock, ".s.PGSQL.5432"))
	db, err := OpenDB(checkedConfig(t, "postgres:///db?host="+sock+"&user=authority_app&connect_timeout=100000000"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	start := time.Now()
	if returned, err := pingWithin(db, time.Hour, 3*time.Second); !returned || err == nil {
		t.Fatalf("a dial under connect_timeout=100000000: returned %v, %v; the URL lifted the bound", returned, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the dial took %v, far past dialTimeout", d)
	}
}

// OpenDB refuses a Config Load did not check: a zero pq.Config would dial
// lib/pq's defaults, TCP to localhost, with $HOME/.pgpass. The last case is
// M3's forgery: pq.NewConfig straight from a URL naming its own passfile has
// an absolute host and a passfile, which is all a field heuristic can see.
func TestOpenDBRefusesAnUncheckedConfig(t *testing.T) {
	clearLibpqEnv(t)
	forged, err := pq.NewConfig("postgres:///db?host=/run/postgresql&user=authority_app&passfile=/x")
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range []Config{
		{},
		{DatabaseURL: socketURL("authority_app", "")},
		{DB: pq.Config{Host: "/run/postgresql", User: "authority_app"}},
		{DB: forged},
	} {
		if db, err := OpenDB(c); err == nil || db != nil {
			t.Errorf("case %d: opened", i)
		}
	}
}

// Open makes the process non-dumpable before it reads a key, and
// a failure to is a refusal, reason=config, before any credential is read.
func TestOpenRefusesWhenTheProcessStaysDumpable(t *testing.T) {
	sentinel := errors.New("prctl refused")
	hook := undumpable
	t.Cleanup(func() { undumpable = hook })
	undumpable = func() error { return sentinel }
	_, _, err := Open(Config{CredentialsDir: t.TempDir()}, quiet)
	if exitReason(err) != ReasonConfig || !errors.Is(err, sentinel) {
		t.Fatalf("Open: %v, want reason config wrapping the prctl failure", err)
	}
	undumpable = func() error { return nil }
	if _, _, err := Open(Config{CredentialsDir: t.TempDir()}, quiet); errors.Is(err, sentinel) {
		t.Fatalf("control: %v", err)
	}
}
