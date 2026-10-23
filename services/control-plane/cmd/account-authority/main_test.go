package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/server"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/tlspin"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// The sentinels the exit-line tests plant where a path, an address, a URL or
// a credential would be, and then look for.
const (
	sentinelDir  = "sentinel-dir-7q3z"
	sentinelPort = "61937"
	sentinelPass = "sentinel-pass-9x2k" // pragma: allowlist secret
	sentinelSQL  = "sentinel_sql_table"
)

// run drives runMain. Anything the process itself writes to os.Stderr while
// it runs (the flag package does, by default) is appended to stderr, so a
// stray write fails the exact-usage assertions instead of going unseen.
func run(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	stray := captureProcessStderr(func() { code = runMain(context.Background(), args, &o, &e) })
	return code, o.String(), e.String() + stray
}

func captureProcessStderr(f func()) string {
	tmp, err := os.CreateTemp("", "stderr")
	if err != nil {
		panic(err)
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	saved := os.Stderr
	os.Stderr = tmp
	func() {
		defer func() { os.Stderr = saved }()
		f()
	}()
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		panic(err)
	}
	b, err := io.ReadAll(tmp)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func reason(err error) string {
	var ee *server.ExitError
	if errors.As(err, &ee) {
		return ee.Reason
	}
	return ""
}

// unsetenv removes k for the test and restores it after. t.Setenv records the
// original value; os.Unsetenv then makes k unset rather than empty, which
// matters: server.Load refuses a variable lib/pq reads when it is set at all.
func unsetenv(t *testing.T, k string) {
	t.Helper()
	t.Setenv(k, "")
	if err := os.Unsetenv(k); err != nil {
		t.Fatal(err)
	}
}

// libpqNames is every variable lib/pq reads outside the PG prefix, derived
// from its env tags as server.Load derives it.
func libpqNames() map[string]bool {
	m := map[string]bool{"PQGO_DEBUG": true}
	typ := reflect.TypeFor[pq.Config]()
	for i := range typ.NumField() {
		if k := typ.Field(i).Tag.Get("env"); k != "" && k != "-" {
			m[k] = true
		}
	}
	return m
}

// clearEnv leaves the authority's variables and every variable lib/pq reads
// unset, whatever the host exports.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AUTHORITY_LISTEN_ADDR", "AUTHORITY_TRUST_PATH", "AUTHORITY_ANCHORS_PATH",
		"AUTHORITY_DISK_WATCH_PATH", "AUTHORITY_DATABASE_URL", "AUTHORITY_MIGRATE_DATABASE_URL", "CREDENTIALS_DIRECTORY"} {
		unsetenv(t, k)
	}
	names := libpqNames()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "PG") || names[k] {
			unsetenv(t, k)
		}
	}
}

// socketURL is a URL the loader accepts: a socket directory, no password.
func socketURL(role, dir, port string) string {
	q := url.Values{"host": {dir}}
	if port != "" {
		q.Set("port", port)
	}
	return (&url.URL{Scheme: "postgres", User: url.User(role), Path: "/db", RawQuery: q.Encode()}).String()
}

// serveEnv sets a complete serve environment around dbURL and returns the
// directory every path in it points into.
func serveEnv(t *testing.T, dbURL string) string {
	t.Helper()
	clearEnv(t)
	dir := t.TempDir()
	for k, v := range map[string]string{
		"AUTHORITY_LISTEN_ADDR": "127.0.0.1:8443", "AUTHORITY_TRUST_PATH": filepath.Join(dir, "trust"),
		"AUTHORITY_ANCHORS_PATH": filepath.Join(dir, "anchors"), "AUTHORITY_DISK_WATCH_PATH": dir,
		"CREDENTIALS_DIRECTORY": dir, "AUTHORITY_DATABASE_URL": dbURL,
	} {
		t.Setenv(k, v)
	}
	return dir
}

func migrateEnv(t *testing.T, dbURL string) {
	t.Helper()
	clearEnv(t)
	t.Setenv("AUTHORITY_MIGRATE_DATABASE_URL", dbURL)
}

func logLines(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line %q is not JSON: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

// exitLine is the one authority.exit line a refused run wrote (carry 5:
// runMain writes exactly one).
func exitLine(t *testing.T, code int, stdout, wantReason string) map[string]any {
	t.Helper()
	lines := logLines(t, stdout)
	if code != 1 || len(lines) != 1 {
		t.Fatalf("exit %d with %d log lines, want 1 and exactly one: %q", code, len(lines), stdout)
	}
	m := lines[0]
	if m["msg"] != "authority.exit" || m["reason"] != wantReason {
		t.Fatalf("line %v, want authority.exit with reason %q", m, wantReason)
	}
	return m
}

func errorOf(t *testing.T, m map[string]any) string {
	t.Helper()
	s, ok := m["error"].(string)
	if !ok {
		t.Fatalf("line %v carries no error text", m)
	}
	return s
}

func TestUsageIsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		nil, {"nope"}, {"serve", "extra"}, {"migrate"}, {"migrate", "--realm"}, {"keygen"},
		{"keygen", "--out", "k", "extra"}, {"migrate", "--bogus", "x"}, {"migrate", "--realm", ""},
		{"migrate", "-h"}, {"keygen", "--help"}, {"keygen", "--out"},
	} {
		code, stdout, stderr := run(args...)
		// Review M3: exactly our one usage line, not the flag package's own
		// output beside it.
		if code != 2 || stderr != usage+"\n" || stdout != "" {
			t.Errorf("%q: code %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
}

// Review M3: a flag given twice is usage, not last-wins. keygen --out a
// --out b would otherwise write b and leave the operator believing a.
func TestARepeatedFlagIsUsage(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, args := range [][]string{
		{"keygen", "--out", a, "--out", b}, {"keygen", "--out=" + a, "--out=" + a}, {"keygen", "-out", a, "--out", b},
		{"migrate", "--realm", uuid.NewString(), "--realm", uuid.NewString()},
	} {
		if code, stdout, stderr := run(args...); code != 2 || stderr != usage+"\n" || stdout != "" {
			t.Errorf("%q: code %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	for _, f := range []string{a, b} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("a refused invocation wrote %s: %v", f, err)
		}
	}
}

// B2-D8: a refused environment exits 1 with reason=config, never quoting a value.
func TestServeRefusesItsEnvironment(t *testing.T) {
	clearEnv(t)
	code, stdout, stderr := run("serve")
	if msg := errorOf(t, exitLine(t, code, stdout, "config")); !strings.Contains(msg, "AUTHORITY_LISTEN_ADDR is required") {
		t.Fatalf("empty environment: %q", msg)
	}
	if stderr != "" {
		t.Fatalf("a refusal wrote to stderr: %q", stderr)
	}
	inline := (&url.URL{Scheme: "postgres", User: url.UserPassword("authority_app", sentinelPass), Path: "/db",
		RawQuery: "host=/run/postgresql"}).String()
	dir := serveEnv(t, inline)
	code, stdout, _ = run("serve")
	msg := errorOf(t, exitLine(t, code, stdout, "config"))
	if !strings.Contains(msg, "AUTHORITY_DATABASE_URL") || !strings.Contains(msg, "no password") || strings.Contains(stdout, sentinelPass) {
		t.Fatalf("inline password (mutation 2): %q", stdout)
	}
	// The control: the same environment without the password passes Load and
	// reaches Open, whose first read is the RSK credential.
	t.Setenv("AUTHORITY_DATABASE_URL", socketURL("authority_app", t.TempDir(), ""))
	code, stdout, _ = run("serve")
	msg = errorOf(t, exitLine(t, code, stdout, "config"))
	if !strings.Contains(msg, "credential rsk") || strings.Contains(stdout, dir) {
		t.Fatalf("a valid environment did not reach Open's credential read: %q", stdout)
	}
}

// Carry 5: the loader reads the process environment (os.Environ), so a
// variable lib/pq would read is refused for both verbs, whatever its value.
func TestVerbsPassTheProcessEnvironmentToTheLoader(t *testing.T) {
	serveEnv(t, socketURL("authority_app", t.TempDir(), ""))
	t.Setenv("PGHOST", "attacker.example")
	code, stdout, _ := run("serve")
	msg := errorOf(t, exitLine(t, code, stdout, "config"))
	if !strings.Contains(msg, "PGHOST must not be set") || strings.Contains(stdout, "attacker.example") {
		t.Fatalf("serve: %q", stdout)
	}
	migrateEnv(t, socketURL("authority_owner", t.TempDir(), ""))
	t.Setenv("PGSERVICE", "attacker-service")
	code, stdout, _ = run("migrate", "--realm", uuid.NewString())
	msg = errorOf(t, exitLine(t, code, stdout, "config"))
	if !strings.Contains(msg, "PGSERVICE must not be set") || strings.Contains(stdout, "attacker-service") {
		t.Fatalf("migrate: %q", stdout)
	}
	t.Setenv("AUTHORITY_DATABASE_URL", socketURL("authority_app", t.TempDir(), ""))
	unsetenv(t, "PGSERVICE")
	code, stdout, _ = run("migrate", "--realm", uuid.NewString())
	if msg = errorOf(t, exitLine(t, code, stdout, "config")); !strings.Contains(msg, "AUTHORITY_DATABASE_URL must not be set") {
		t.Fatalf("migrate with the serve URL: %q", msg)
	}
}

func TestMigrateRefusesABadRealm(t *testing.T) {
	migrateEnv(t, socketURL("authority_owner", t.TempDir(), ""))
	id := uuid.NewString()
	for _, realm := range []string{"x", strings.ToUpper(id), "{" + id + "}", "urn:uuid:" + id,
		strings.ReplaceAll(id, "-", "")} {
		code, stdout, _ := run("migrate", "--realm", realm)
		if msg := errorOf(t, exitLine(t, code, stdout, "config")); !strings.Contains(msg, "--realm") {
			t.Errorf("--realm %q: refused for another reason: %q", realm, msg)
		}
	}
}

// B2-D32: a network error reaches the exit line without its addresses. The
// database is the case: lib/pq returns its dial error unwrapped.
func TestExitTextDropsNetworkAddresses(t *testing.T) {
	refusedDial := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 5432},
		Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	noHost := &net.OpError{Op: "dial", Net: "tcp",
		Err: &net.DNSError{Name: "db.internal", Server: "10.0.0.53:53", Err: "no such host"}}
	plain := errors.New("AUTHORITY_TRUST_PATH cannot be read")
	for in, want := range map[error]string{
		fmt.Errorf("store: role: %w", refusedDial): "network dial: " + syscall.ECONNREFUSED.Error(),
		noHost: "network dial failed",
		plain:  plain.Error(),
	} {
		if got := exitText(in); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// exitCase is one input to the exit line's barrier. want "=" means the text
// passes through unchanged: the error has no typed slot a value can sit in.
// Every forbidden string must appear in a reduced input's raw text, so a row
// that reduces nothing cannot pass for one that reduces something.
type exitCase struct {
	name   string
	err    error
	want   string
	forbid []string
}

// dialErr dials cfg once through lib/pq's connector and returns the error.
func dialErr(t *testing.T, cfg pq.Config) error {
	t.Helper()
	c, err := pq.NewConnectorConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := c.Connect(ctx)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the dial succeeded")
	}
	return err
}

func libpqConfig(t *testing.T, raw string) pq.Config {
	t.Helper()
	clearEnv(t)
	cfg, err := pq.NewConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// shortDir is a directory under /tmp: a unix socket path is capped near 104
// bytes, which a t.TempDir can exceed, and a path over the cap fails with
// EINVAL before it can fail with ENOENT. It sits one level below a 0700
// directory, because Load refuses a socket directory whose parent other users
// can write, and /tmp is 1777.
func shortDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "cvpg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	d := filepath.Join(root, "s")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// dialUnixErr is lib/pq's real error for a socket directory that does not
// exist, and the directory and port it was given.
func dialUnixErr(t *testing.T) (dir string, err error) {
	t.Helper()
	dir = filepath.Join(shortDir(t), sentinelDir)
	return dir, dialErr(t, libpqConfig(t, socketURL("authority_owner", dir, sentinelPort)))
}

// emptySockDir is a socket directory Load accepts (it judges the directory,
// Sec-I1) with no server in it, named sentinelDir: a dial there fails with
// ENOENT on the socket file, and the exit line must name neither.
func emptySockDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(shortDir(t), sentinelDir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// closedPort is a loopback port nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	return port
}

// dialRefusedErr is lib/pq's real error for a closed local TCP port. The
// loader refuses a TCP URL, so only a direct dial can produce it.
func dialRefusedErr(t *testing.T) (port string, err error) {
	t.Helper()
	port = closedPort(t)
	return port, dialErr(t, libpqConfig(t, "postgres://authority_owner@127.0.0.1:"+port+"/db?sslmode=disable"))
}

// dialTimeoutErr is lib/pq's real error when ConnectTimeout expires on a
// server that accepts and never answers.
func dialTimeoutErr(t *testing.T) (port string, err error) {
	t.Helper()
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatal(lerr)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	cfg := libpqConfig(t, "postgres://authority_owner@127.0.0.1:"+port+"/db?sslmode=disable")
	cfg.ConnectTimeout = 300 * time.Millisecond
	return port, dialErr(t, cfg)
}

// Carry 2: the exact error lib/pq returns, captured from a real dial, is a
// *net.OpError at the top, so no wrap between lib/pq and the exit line has
// lost its type. Without that, the exit line would name the socket path.
func TestExitTextOfTheErrorsLibpqReallyReturns(t *testing.T) {
	dir, unixErr := dialUnixErr(t)
	refusedPort, refusedErr := dialRefusedErr(t)
	timeoutPort, timeoutErr := dialTimeoutErr(t)
	checkRealDial(t, "unix socket directory missing", unixErr,
		[]string{dir, sentinelDir, sentinelPort, ".s.PGSQL"}, "network dial: no such file or directory")
	checkRealDial(t, "tcp port closed", refusedErr,
		[]string{"127.0.0.1", refusedPort}, "network dial: connection refused")
	checkRealDial(t, "connect_timeout expiry", timeoutErr,
		[]string{"127.0.0.1", timeoutPort}, "network read: i/o timeout")
}

func checkRealDial(t *testing.T, name string, err error, secret []string, want string) {
	t.Helper()
	var oe *net.OpError
	if !errors.As(err, &oe) {
		t.Errorf("%s: lib/pq returned %T (%v), not a *net.OpError; the barrier keys on that type", name, err, err)
		return
	}
	for _, s := range secret {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("%s: control: the raw error does not carry %q, so this test cannot see it leak: %v", name, s, err)
		}
	}
	// As the code wraps it: Run's refuse, and a step's own context.
	got := exitText(&server.ExitError{Reason: "config", Err: fmt.Errorf("store: role: %w", err)})
	if got != want {
		t.Errorf("%s: exitText %q, want %q", name, got, want)
	}
	for _, s := range secret {
		if strings.Contains(got, s) {
			t.Errorf("%s: the exit text names %q: %q", name, s, got)
		}
	}
}

// Carry 1 (C1) and the brief's other typed inputs.
func TestExitTextReducesPathErrors(t *testing.T) {
	path := "/etc/" + sentinelDir + "/credential"
	pe := &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
	le := &os.LinkError{Op: "rename", Old: path, New: path + ".new", Err: syscall.EXDEV}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"bare", pe, "open: permission denied"},
		{"wrapped", refused("config", fmt.Errorf("x: %w", pe)), "open: permission denied"},
		{"joined", errors.Join(errors.New("other"), fmt.Errorf("y: %w", pe)), "other; open: permission denied"},
		{"link error", fmt.Errorf("z: %w", le), "rename: " + syscall.EXDEV.Error()},
		{"a network error under a path error", &os.PathError{Op: "read", Path: path,
			Err: &net.OpError{Op: "read", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 5432}, Err: syscall.ECONNRESET}},
			"network read: connection reset by peer"},
	} {
		got := exitText(c.err)
		if got != c.want || strings.Contains(got, sentinelDir) || strings.Contains(got, "10.0.0.5") {
			t.Errorf("%s: exitText %q, want %q", c.name, got, c.want)
		}
	}
}

// Carry 7: every input Task 10 listed for the exit line, none of them
// letting a path, an address, a URL or a credential through. A real error
// stands where one is cheap to produce; the rest carry the sentinels in the
// slot a value would sit in.
func taskTenCases(t *testing.T) []exitCase {
	dir, unixErr := dialUnixErr(t)
	refusedPort, refusedErr := dialRefusedErr(t)
	timeoutPort, timeoutErr := dialTimeoutErr(t)
	_, mintErr := tlspin.Mint(nil, x509.ExtKeyUsageServerAuth)
	_, uncheckedErr := server.OpenDB(server.Config{})
	depsErr := server.Run(context.Background(), server.Deps{})
	credDir := serveEnv(t, socketURL("authority_app", t.TempDir(), ""))
	cfg, err := server.Load(server.VerbServe, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	_, _, openErr := server.Open(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if openErr == nil {
		t.Fatal("Open succeeded over an empty credentials directory")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, acceptPort, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	_, acceptErr := ln.Accept()
	pgErr := &pq.Error{Severity: "ERROR", Code: "42501", Message: "permission denied for table meta",
		Detail: "Key (id)=(" + sentinelPass + ") conflicts", Where: "COPY " + sentinelDir}
	unixAddr := &net.UnixAddr{Name: "/run/postgresql/" + sentinelDir + "/.s.PGSQL.5432", Net: "unix"}
	pingErr := &net.OpError{Op: "write", Net: "unix", Source: &net.UnixAddr{Name: "@"}, Addr: unixAddr, Err: os.NewSyscallError("write", syscall.EPIPE)}
	cfgE := func(e error) error { return refused("config", e) }
	trustE := func(e error) error { return refused("trust", e) }
	realm := uuid.NewString()

	return []exitCase{
		// reason=config
		{"deps incomplete (real Run)", depsErr, "=", nil},
		{"lib/pq unix dial (real)", cfgE(unixErr), "network dial: no such file or directory", []string{dir, sentinelDir, sentinelPort}},
		{"lib/pq tcp refused (real)", cfgE(refusedErr), "network dial: connection refused", []string{"127.0.0.1", refusedPort}},
		{"connect_timeout expiry (real)", cfgE(timeoutErr), "network read: i/o timeout", []string{"127.0.0.1", timeoutPort}},
		{"pq.Error from a startup query", cfgE(pgErr), "pq: permission denied for table meta", nil},
		{"store.ErrWrongRole", cfgE(store.ErrWrongRole), "=", nil},
		{"schema: read failure over a dial", cfgE(fmt.Errorf("authority store: read schema version: %w", pingErr)), "network write: broken pipe", []string{sentinelDir, ".s.PGSQL"}},
		{"schema: dirty", cfgE(fmt.Errorf("%w at version %d", store.ErrSchemaDirty, 7)), "=", nil},
		{"schema: skew", cfgE(fmt.Errorf("%w: database %d, code %d", store.ErrSchemaSkew, 6, 7)), "=", nil},
		{"store.ErrNoMeta", cfgE(store.ErrNoMeta), "=", nil},
		{"Mint's error (real)", cfgE(mintErr), "=", nil},
		{"Serve's error (real accept on a closed listener)", cfgE(acceptErr), "network accept failed", []string{"127.0.0.1", acceptPort}},
		{"errNoSchemaCheck", cfgE(errors.New("server: a standby was promoted with no schema check wired")), "=", nil},
		{"errNoCause", cfgE(errors.New("server: serve was stopped with no cause")), "=", nil},
		{"Open: credential rsk (real)", openErr, "=", []string{credDir}},
		{"Open: errUncheckedDB (real)", cfgE(uncheckedErr), "=", nil},
		{"Open: the bind refusal", cfgE(fmt.Errorf("AUTHORITY_LISTEN_ADDR does not bind: %w", os.NewSyscallError("bind", syscall.EADDRINUSE))), "=", []string{"127.0.0.1", "8443"}},
		// reason=trust
		{"anchors do not decode", trustE(errors.New("authority store: anchors do not decode")), "=", nil},
		{"ErrRealmMismatch", trustE(fmt.Errorf("%w: %s", store.ErrRealmMismatch, realm)), "=", nil},
		{"ErrTrustNotYetValid", trustE(fmt.Errorf("%w (gen %d, serial %d)", store.ErrTrustNotYetValid, 3, 41)), "=", nil},
		{"ErrTrustUnusable joined", trustE(errors.Join(store.ErrTrustUnusable, errors.New("the high-water delegation differs"))),
			"authority store: the trust does not verify; the high-water delegation differs", nil},
		{"ErrRSKReused", trustE(store.ErrRSKReused), "=", nil},
		{"errNodeRSK", trustE(errors.New("this node's RSK is not in the high-water delegation (D-223)")), "=", nil},
		{"errNodeTLSKey", trustE(errors.New("this node's TLS key is not in the high-water delegation (D-223)")), "=", nil},
		{"errPromotedPending", trustE(errors.New("promoted with trust this node has not installed; restarting to install it (B2-D33)")), "=", nil},
		{"a trust input that is missing", trustE(errors.New("AUTHORITY_ANCHORS_PATH: open: no such file or directory")), "=", nil},
		// reason=lock
		{"ErrServeLockHeld", refused("lock", store.ErrServeLockHeld), "=", nil},
		{"the lock connection's ping", refused("lock", fmt.Errorf("authority store: serve lock: %w", pingErr)), "network write: broken pipe", []string{sentinelDir, ".s.PGSQL"}},
		// no cause at all
		{"an ExitError with no cause", &server.ExitError{Reason: "config"}, "=", nil},
	}
}

func TestExitTextOfEveryTaskTenInput(t *testing.T) {
	for _, c := range taskTenCases(t) {
		checkExitCase(t, c)
	}
}

func checkExitCase(t *testing.T, c exitCase) {
	t.Helper()
	got := exitText(c.err)
	want := c.want
	if want == "=" {
		want = c.err.Error()
	} else {
		for _, s := range c.forbid {
			if !strings.Contains(c.err.Error(), s) {
				t.Errorf("%s: control: the raw error does not carry %q, so this row cannot see it leak: %v", c.name, s, c.err)
			}
		}
	}
	if got != want {
		t.Errorf("%s: exitText %q, want %q", c.name, got, want)
	}
	for _, s := range append([]string{"postgres://", sentinelDir, sentinelPort, sentinelPass, "10.0.0.5", sentinelSQL}, c.forbid...) {
		if strings.Contains(got, s) {
			t.Errorf("%s: the exit text carries %q: %q", c.name, s, got)
		}
	}
}

// migrationOpError is the error a dropped connection leaves in the driver:
// a read on the unix socket at a sentinel path.
func migrationOpError() *net.OpError {
	return &net.OpError{Op: "read", Net: "unix", Source: &net.UnixAddr{Name: "@", Net: "unix"},
		Addr: &net.UnixAddr{Name: "/run/" + sentinelDir + "/.s.PGSQL.5432", Net: "unix"},
		Err:  os.NewSyscallError("read", syscall.ECONNRESET)}
}

// migrationCases are the errors store.MigrateEmbedded returns, in the exact
// shapes golang-migrate v4.20.1's postgres driver builds them (postgres.go
// 243-315, 358-483): database.Error and *database.Error carry the query, a
// help string that embeds pgErr.Detail, and the cause, and have no Unwrap;
// migrate.MultiError has none either. The runner wraps the result with %w.
func migrationCases() []exitCase {
	query := []byte("INSERT INTO " + sentinelSQL + " VALUES ('" + sentinelPass + "')")
	pgDup := &pq.Error{Severity: "ERROR", Code: "23505", Message: `duplicate key value violates unique constraint "k"`,
		Detail: "Key (x)=(" + sentinelPass + ") already exists.", Hint: "hint " + sentinelPass,
		Where: "where " + sentinelDir, Table: sentinelSQL, Constraint: sentinelSQL}
	help := "migration failed: " + pgDup.Message + ", " + pgDup.Detail
	runner := func(e error) error { return refused("config", fmt.Errorf("could not run migrations: %w", e)) }
	return []exitCase{
		{"database.Error over a dropped connection", runner(database.Error{OrigErr: migrationOpError(), Err: "migration failed", Query: query}),
			"database: network read: connection reset by peer", []string{sentinelDir, ".s.PGSQL", sentinelSQL}},
		{"database.Error over a pq.Error with Detail", runner(database.Error{OrigErr: pgDup, Err: help, Query: query, Line: 3}),
			`database: pq: duplicate key value violates unique constraint "k"`, []string{sentinelPass, sentinelSQL}},
		{"*database.Error", runner(&database.Error{OrigErr: migrationOpError(), Query: query}),
			"database: network read: connection reset by peer", []string{sentinelDir, ".s.PGSQL", sentinelSQL}},
		{"MultiError of a safe and an unsafe error", runner(multiError{Errs: []error{errors.New("unlock failed"),
			&database.Error{OrigErr: migrationOpError(), Query: query}}}),
			"unlock failed; database: network read: connection reset by peer", []string{sentinelDir, ".s.PGSQL", sentinelSQL}},
		{"*MultiError", runner(&multiError{Errs: []error{database.Error{OrigErr: migrationOpError(), Query: query}, errors.New("unlock failed")}}),
			"database: network read: connection reset by peer; unlock failed", []string{sentinelDir, sentinelSQL}},
		{"errors.Join of a database.Error, as v4.20.1's Up returns", runner(errors.Join(database.Error{OrigErr: migrationOpError(), Query: query}, errors.New("unlock failed"))),
			"database: network read: connection reset by peer; unlock failed", []string{sentinelDir, ".s.PGSQL", sentinelSQL}},
		{"pq.Error alone: Message only", refused("config", pgDup), `pq: duplicate key value violates unique constraint "k"`, nil},
		{"pq.Error under a wrap", refused("config", fmt.Errorf("authority store: read schema version: %w", pgDup)),
			`pq: duplicate key value violates unique constraint "k"`, nil},
		{"a database.Error with a nil cause", runner(database.Error{Err: "transaction start failed", Query: query}), "database: failed", []string{sentinelSQL}},
	}
}

// Carry C1 of the Task 11 review (CWE-532): golang-migrate's errors reach the
// exit line through exitText as database.Error, *database.Error and
// MultiError, none of which unwraps. Their text embeds the SQL, the row
// values pgErr.Detail names, and the cause's address.
func TestExitTextOfMigrationErrors(t *testing.T) {
	for _, c := range migrationCases() {
		checkExitCase(t, c)
	}
}

// Review M2: no shape of error panics exitText, nil causes and typed-nil
// pointers included.
func TestExitTextSurvivesNilCauses(t *testing.T) {
	wrapped := func(e error) error { return fmt.Errorf("x: %w", e) } // e holds a typed nil pointer
	var nilPath *os.PathError
	var nilDB *database.Error
	var nilPQ *pq.Error
	var nilOp *net.OpError
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"path error, nil cause":     {&os.PathError{Op: "open", Path: "/p"}, "open: failed"},
		"link error, nil cause":     {&os.LinkError{Op: "rename", Old: "/a", New: "/b"}, "rename: failed"},
		"database.Error, nil":       {database.Error{}, "database: failed"},
		"*database.Error, nil":      {&database.Error{}, "database: failed"},
		"multi error, a nil":        {multiError{Errs: []error{nil}}, "failed"},
		"multi error, none":         {multiError{}, "failed"},
		"op error, nil cause":       {&net.OpError{Op: "dial"}, "network dial failed"},
		"typed nil *PathError":      {wrapped(nilPath), "failed"},
		"typed nil *database.Error": {wrapped(nilDB), "failed"},
		"typed nil *pq.Error":       {wrapped(nilPQ), "failed"},
		"typed nil *net.OpError":    {wrapped(nilOp), "failed"},
		"nil":                       {nil, "failed"},
	} {
		if got := exitText(c.err); got != c.want {
			t.Errorf("%s: exitText %q, want %q", name, got, c.want)
		}
	}
}

// Carry 6 (the Task 7 ruling): every line carries op, and the exit line is
// exactly op, reason and error beside slog's own time, level and msg. It has
// no outcome: it is not a store outcome.
func TestTheExitLineIsExactlyItsFields(t *testing.T) {
	code, stdout, stderr := run("keygen", "--out", filepath.Join(t.TempDir(), "absent", "k"))
	m := exitLine(t, code, stdout, "config")
	if got, want := slices.Sorted(maps.Keys(m)), []string{"error", "level", "msg", "op", "reason", "time"}; !slices.Equal(got, want) {
		t.Errorf("fields %v, want %v", got, want)
	}
	if m["op"] != "authority.exit" || m["level"] != "INFO" || stderr != "" {
		t.Errorf("line %v, stderr %q", m, stderr)
	}
}

// Carry 5: runMain writes the exit line, and writes exactly one, for every
// reason a verb can fail with; a run that succeeds writes none.
func TestRunMainWritesOneExitLinePerFailure(t *testing.T) {
	clearEnv(t)
	for _, args := range [][]string{{"serve"}, {"migrate", "--realm", uuid.NewString()},
		{"keygen", "--out", filepath.Join(t.TempDir(), "absent", "k")}} {
		code, stdout, _ := run(args...)
		exitLine(t, code, stdout, "config")
	}
	out := filepath.Join(t.TempDir(), "k")
	if code, stdout, _ := run("keygen", "--out", out); code != 0 || strings.Contains(stdout, "authority.exit") {
		t.Errorf("a successful keygen wrote an exit line: %d %q", code, stdout)
	}
}

// Carry 1 (C1), end to end: a failed keygen's *os.PathError names the output
// path, and the exit line must not.
func TestTheExitLineNeverCarriesAPath(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, sentinelDir, "k")
	code, stdout, _ := run("keygen", "--out", out)
	m := exitLine(t, code, stdout, "config")
	if got := errorOf(t, m); got != "open: no such file or directory" || strings.Contains(stdout, sentinelDir) || strings.Contains(stdout, dir) {
		t.Fatalf("missing directory: %q", stdout)
	}
	existing := filepath.Join(dir, "existing")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = run("keygen", "--out", existing)
	if got := errorOf(t, exitLine(t, code, stdout, "config")); got != "open: file exists" || strings.Contains(stdout, dir) {
		t.Fatalf("existing file: %q", stdout)
	}
	// And as the literal wrap the carry names, through the writer runMain uses.
	pe := &os.PathError{Op: "open", Path: "/run/" + sentinelDir + "/rsk", Err: syscall.EACCES}
	var buf bytes.Buffer
	if code := exitCode(logger.NewJSON(&buf).Logger, refused("config", fmt.Errorf("x: %w", pe))); code != 1 {
		t.Fatalf("exit %d", code)
	}
	line := exitLine(t, 1, buf.String(), "config")
	if got := errorOf(t, line); got != "open: permission denied" || strings.Contains(buf.String(), sentinelDir) {
		t.Fatalf("wrapped PathError: %q", buf.String())
	}
}

// Carry 2 (I1), end to end: migrate against a socket directory with no
// server in it dials through lib/pq for real. The refusal is reason=config and
// the line names neither the directory, the port in the socket's file name,
// nor the file name.
func TestMigrateDialErrorNeverNamesItsSocket(t *testing.T) {
	dir := emptySockDir(t)
	migrateEnv(t, socketURL("authority_owner", dir, sentinelPort))
	code, stdout, _ := run("migrate", "--realm", uuid.NewString())
	m := exitLine(t, code, stdout, "config")
	if got := errorOf(t, m); got != "network dial: no such file or directory" {
		t.Fatalf("error %q: the dial error did not reach the line as a reduced *net.OpError", got)
	}
	for _, s := range []string{dir, sentinelDir, sentinelPort, ".s.PGSQL", filepath.Dir(dir)} {
		if strings.Contains(stdout, s) {
			t.Errorf("the exit line names %q: %q", s, stdout)
		}
	}
}

// Carry 2 (I1), the TCP half: the loader refuses a TCP URL before any dial,
// and its refusal quotes neither host nor port. (A real TCP dial error is
// covered by TestExitTextOfTheErrorsLibpqReallyReturns.)
func TestMigrateRefusesATCPURLWithoutQuotingIt(t *testing.T) {
	port := closedPort(t)
	migrateEnv(t, "postgres://authority_owner@127.0.0.1:"+port+"/db?sslmode=disable")
	code, stdout, _ := run("migrate", "--realm", uuid.NewString())
	m := exitLine(t, code, stdout, "config")
	if got := errorOf(t, m); !strings.Contains(got, "TCP is refused") || strings.Contains(stdout, "127.0.0.1") || strings.Contains(stdout, port) {
		t.Fatalf("TCP URL: %q", stdout)
	}
}

// Carry 2, the serve path: Run's own wrap (refuse over within over the role
// check) keeps the dial error's type too. serve cannot reach the dial on a
// host where the credential read refuses, so this drives Run directly, over
// the pool Load's Config opens.
func TestServeRunKeepsTheDialErrorsType(t *testing.T) {
	dir := emptySockDir(t)
	serveEnv(t, socketURL("authority_app", dir, sentinelPort))
	cfg, err := server.Load(server.VerbServe, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	db, err := server.OpenDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rsk, err := signer.GenerateP384()
	if err != nil {
		t.Fatal(err)
	}
	tlsKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	runErr := server.Run(context.Background(), server.Deps{DB: db, RSK: rsk, TLSKey: tlsKey, Listen: ln,
		Statfs: func() (uint64, uint64, error) { return 1 << 40, 1 << 41, nil }, Clock: func() bool { return true },
		Notify: func(string) {}, Log: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	var oe *net.OpError
	if reason(runErr) != "config" || !errors.As(runErr, &oe) {
		t.Fatalf("Run returned %T %v, want reason config over a *net.OpError", runErr, runErr)
	}
	var buf bytes.Buffer
	code := exitCode(logger.NewJSON(&buf).Logger, runErr)
	m := exitLine(t, code, buf.String(), "config")
	if got := errorOf(t, m); got != "network dial: no such file or directory" || strings.Contains(buf.String(), sentinelDir) {
		t.Fatalf("serve's exit line: %q", buf.String())
	}
}

// fakeAuthPG is a unix-socket server that asks for a cleartext password and
// reports what lib/pq sends. It is what lets a test see which pgpass lib/pq
// read.
type fakeAuthPG struct{ got chan string }

func newFakeAuthPG(t *testing.T, path string) *fakeAuthPG {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeAuthPG{got: make(chan string, 32)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.serve(c)
		}
	}()
	return f
}

func (f *fakeAuthPG) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	var hdr [5]byte
	if _, err := io.ReadFull(c, hdr[:4]); err != nil { // the startup message's length
		return
	}
	if _, err := io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(hdr[:4]))-4); err != nil {
		return
	}
	if _, err := c.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 3}); err != nil { // AuthenticationCleartextPassword
		return
	}
	if _, err := io.ReadFull(c, hdr[:]); err != nil || hdr[0] != 'p' {
		return
	}
	body := make([]byte, binary.BigEndian.Uint32(hdr[1:])-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return
	}
	f.got <- strings.TrimRight(string(body), "\x00")
}

// drain returns every password the server has received.
func (f *fakeAuthPG) drain() []string {
	var out []string
	for {
		select {
		case p := <-f.got:
			out = append(out, p)
		default:
			return out
		}
	}
}

// Carry 3 (N1): migrate dials through server.OpenDB, never lib/pq's own parse
// of the URL. That parse reads $HOME/.pgpass whenever the URL names no
// passfile. The control dials the URL the way sql.Open would and must see the
// home file's password, so the harness can see the difference.
func TestMigrateDialsOnlyThroughOpenDB(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".pgpass"), []byte("*:*:*:authority_owner:from-home\n"), 0o600); err != nil { // pragma: allowlist secret
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	sock := shortDir(t)
	srv := newFakeAuthPG(t, filepath.Join(sock, ".s.PGSQL.5432"))
	raw := socketURL("authority_owner", sock, "")

	_ = dialErr(t, libpqConfig(t, raw)) // sql.Open's reading: the home file is used
	if got := srv.drain(); !slices.Contains(got, "from-home") {
		t.Fatalf("control: lib/pq sent %q, so the harness cannot see the home fallback", got)
	}

	migrateEnv(t, raw)
	code, stdout, _ := run("migrate", "--realm", uuid.NewString())
	exitLine(t, code, stdout, "config") // the fake server hangs up after the password
	got := srv.drain()
	if len(got) == 0 {
		t.Fatal("migrate never dialled the fake server")
	}
	if slices.Contains(got, "from-home") {
		t.Fatalf("migrate sent the home pgpass password (%q): it dialled the URL, not the checked Config", got)
	}
}

// Carry 3: an OpenDB refusal is reason=config. migrateWith is the seam that
// takes a Config, so a Config Load did not check can reach it.
func TestMigrateRefusesAnUncheckedConfig(t *testing.T) {
	err := migrateWith(context.Background(), server.Config{}, uuid.NewString())
	if err == nil || reason(err) != "config" || !strings.Contains(exitText(err), "not checked") {
		t.Fatalf("an unchecked Config: %v (reason %q)", err, reason(err))
	}
}

// B2-D11: keygen writes PKCS#8 DER P-384 at 0400 with O_EXCL|O_NOFOLLOW and
// prints only the point and the pin.
func TestKeygen(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "k")
	code, stdout, _ := run("keygen", "--out", out)
	if code != 0 {
		t.Fatalf("keygen: %d %q", code, stdout)
	}
	if fi, err := os.Stat(out); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o400 {
		t.Fatalf("mode %v, want 0400", fi.Mode())
	}
	der, err := os.ReadFile(out) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatal(err)
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	ek, ok := k.(*ecdsa.PrivateKey)
	if err != nil || !ok || ek.Curve != elliptic.P384() {
		t.Fatalf("not a P-384 PKCS#8 key: %v", err)
	}
	pt, err := ek.PublicKey.ECDH()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := ac.TLSPin(pt.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("point=%x\npin=%x\n", pt.Bytes(), pin); stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	if code, _, _ := run("keygen", "--out", out); code != 1 {
		t.Fatal("keygen overwrote an existing key")
	}
	if again, _ := os.ReadFile(out); !bytes.Equal(again, der) { //nolint:gosec // test-controlled temp path
		t.Fatal("the existing key changed")
	}
	link, target := filepath.Join(dir, "link"), filepath.Join(dir, "target")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := run("keygen", "--out", link); code != 1 {
		t.Fatal("keygen followed a symlink")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("keygen wrote through a symlink")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

// B2-D11: a key whose point and pin could not be printed is removed, so the
// ceremony can run keygen again (O_EXCL refuses an existing file).
func TestKeygenRemovesTheKeyWhenOutputFails(t *testing.T) {
	out := filepath.Join(t.TempDir(), "k")
	if err := keygen(out, failingWriter{}); err == nil {
		t.Fatal("keygen succeeded with no output")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("the key was left behind: %v", err)
	}
	if code, _, _ := run("keygen", "--out", out); code != 0 {
		t.Fatal("a retry failed")
	}
}

// breakingWriter fails the write that prints the point and the pin, after
// making the key's directory unwritable, so the removal that follows fails too.
type breakingWriter struct{ dir string }

func (w breakingWriter) Write([]byte) (int, error) {
	_ = os.Chmod(w.dir, 0o500) //nolint:gosec // a directory needs its search bit
	return 0, errors.New("stdout closed")
}

// Review M4: a key that could not be printed AND could not be removed says
// so. The caller learns the key is still there (a retry would be refused by
// O_EXCL), and the removal's *os.PathError is pathless after exitText.
func TestKeygenReportsAFailedRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a file from an unwritable directory")
	}
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // a directory needs its search bit
	out := filepath.Join(dir, "k")
	err := keygen(out, breakingWriter{dir})
	var pe *os.PathError
	if err == nil || !errors.As(err, &pe) || pe.Op != "remove" {
		t.Fatalf("keygen returned %v, want the print error joined with the failed removal", err)
	}
	if _, serr := os.Stat(out); serr != nil {
		t.Fatalf("the key is gone, so the removal did not fail as the test needs: %v", serr)
	}
	if got, want := exitText(err), "stdout closed; remove: "+syscall.EACCES.Error(); got != want || strings.Contains(got, dir) {
		t.Fatalf("exitText %q, want %q", got, want)
	}
}

// Review M1: the first signal cancels the context and releases the signal
// handler, so a second one reaches the default action and ends a process
// whose step (MigrateEmbedded) cannot see the context. notify is a stand-in
// for signal.NotifyContext: cancelling its context is "a signal arrived", and
// its stop is the call that restores the default handler.
func TestTheFirstSignalReleasesTheHandler(t *testing.T) {
	var released atomic.Int32
	var arrive context.CancelFunc
	var gotSigs []os.Signal
	notify := func(parent context.Context, sigs ...os.Signal) (context.Context, context.CancelFunc) {
		gotSigs = sigs
		ctx, cancel := context.WithCancel(parent)
		arrive = cancel
		return ctx, func() { released.Add(1); cancel() }
	}
	ctx, stop := signalContext(context.Background(), notify, syscall.SIGTERM, os.Interrupt)
	defer stop()
	if !slices.Equal(gotSigs, []os.Signal{syscall.SIGTERM, os.Interrupt}) {
		t.Fatalf("signals %v", gotSigs)
	}
	time.Sleep(50 * time.Millisecond)
	if released.Load() != 0 || ctx.Err() != nil {
		t.Fatalf("released %d times, ctx err %v, before any signal", released.Load(), ctx.Err())
	}
	arrive()
	deadline := time.Now().Add(2 * time.Second)
	for released.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if released.Load() == 0 {
		t.Fatal("the first signal did not release the handler: a second SIGINT or SIGTERM would be swallowed")
	}
}

// TestMain lets a test run the real main() in a child process: the test binary
// re-executed with childEnv set becomes the authority, with childArgs as its
// command line.
func TestMain(m *testing.M) {
	if os.Getenv("ACCOUNT_AUTHORITY_TEST_MAIN") == "1" {
		os.Args = append([]string{"account-authority"}, strings.Fields(os.Getenv("ACCOUNT_AUTHORITY_TEST_ARGS"))...)
		main()
	}
	os.Exit(m.Run())
}

// Review M1, end to end: main() in a child process, stuck in a database dial
// that no context can interrupt (the server accepts and never answers; the
// dial's own bound is 4 s). The first SIGINT is asked politely and does not
// end it; the second reaches the default handler and does, long before the
// 4 s bound. Without signalContext's release the second is swallowed and the
// child lives until the dial gives up.
func TestASecondSignalEndsAStuckMigrate(t *testing.T) {
	sock := shortDir(t)
	ln, err := net.Listen("unix", filepath.Join(sock, ".s.PGSQL.5432"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{}, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	var out bytes.Buffer
	cmd := exec.Command(os.Args[0]) //nolint:gosec // the test binary re-executed as the authority
	cmd.Env = []string{"ACCOUNT_AUTHORITY_TEST_MAIN=1", "ACCOUNT_AUTHORITY_TEST_ARGS=migrate --realm " + uuid.NewString(),
		"AUTHORITY_MIGRATE_DATABASE_URL=" + socketURL("authority_owner", sock, "")}
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatalf("the child never dialled: %q", out.String())
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("the first signal ended the child (%v): the dial is not stuck, so this test cannot see the second", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("the second signal did not end the child within 3 s: %q", out.String())
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Fatalf("the child did not die of SIGINT: %v, %q", cmd.ProcessState, out.String())
	}
}

// B2-D11: the mode is 0400 whatever the umask. The directory is made before
// the umask changes, so the test can still write into it.
func TestKeygenModeIgnoresTheUmask(t *testing.T) {
	out := filepath.Join(t.TempDir(), "k")
	old := syscall.Umask(0o777)
	t.Cleanup(func() { syscall.Umask(old) })
	if code, stdout, _ := run("keygen", "--out", out); code != 0 {
		t.Fatalf("keygen: %d %q", code, stdout)
	}
	if fi, err := os.Stat(out); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o400 {
		t.Fatalf("mode %v under umask 0777, want 0400", fi.Mode())
	}
}

// Code-M3: the reason is in reason alone. An ExitError whose cause has no
// typed slot reached the line through ExitError.Error, so its error read
// "config: …" while a typed cause's did not; now every error is the cause's
// text, whatever wraps the ExitError.
func TestExitLineCarriesTheReasonOnce(t *testing.T) {
	for _, c := range []struct {
		name         string
		err          error
		reason, want string
	}{
		{"a loader refusal", refused("config", errors.New("AUTHORITY_LISTEN_ADDR is required")), "config",
			"AUTHORITY_LISTEN_ADDR is required"},
		{"a trust refusal", refused("trust", errors.New("the trust does not verify")), "trust", "the trust does not verify"},
		{"a lock refusal with no cause", refused("lock", nil), "lock", noText},
		{"a wrapped ExitError", fmt.Errorf("serve: %w", refused("trust", errors.New("moved"))), "trust", "moved"},
		{"a typed cause", refused("config", &os.PathError{Op: "open", Path: "/" + sentinelDir, Err: syscall.EACCES}),
			"config", "open: permission denied"},
		{"no ExitError", errors.New("plain"), "config", "plain"},
	} {
		var buf bytes.Buffer
		exitCode(logger.NewJSON(&buf).Logger, c.err)
		if got := errorOf(t, exitLine(t, 1, buf.String(), c.reason)); got != c.want {
			t.Errorf("%s: error %q, want %q", c.name, got, c.want)
		}
	}
	clearEnv(t)
	code, stdout, _ := run("serve")
	if got := errorOf(t, exitLine(t, code, stdout, "config")); got != "AUTHORITY_LISTEN_ADDR is required" {
		t.Errorf("serve with an empty environment: error %q, want the loader's text alone", got)
	}
}

// A refusal names its step on the exit line, from server's closed set;
// a loader refusal, which no step owns, carries none.
func TestTheExitLineNamesTheStep(t *testing.T) {
	code, stdout, _ := run("migrate", "--realm", "NOT-A-UUID")
	if m := exitLine(t, code, stdout, "config"); m["step"] != server.StepRealm {
		t.Fatalf("line %v, want step %q", m, server.StepRealm)
	}
	var buf bytes.Buffer
	exitCode(logger.NewJSON(&buf).Logger, server.RefuseAt(server.StepLock, server.ReasonLock, store.ErrServeLockHeld))
	if m := logLines(t, buf.String())[0]; m["step"] != server.StepLock || m["reason"] != "lock" {
		t.Fatalf("line %v, want step lock", m)
	}
}

// refused is a stepless ExitError for reason, as the server package builds
// one; the exitText tables use it.
func refused(reason string, err error) error { return server.RefuseAt("", reason, err) }

// ---- Task 11: the owner verbs (B3-D9, B3-D12) ----

const ownerUsage = "usage: account-authority serve | migrate --realm <uuid> | keygen --out <path> | restore-begin | " +
	"reconcile-complete | clear-held [--dry-run] <account>... | seal-legacy --cp-digest <hex> | " +
	"end-drain <gen>:<serial> | revoke-key <cak|operator|cp_tls> <hex> | replication-gate <on|off>"

// ownerEnv is a complete owner-verb environment: the owner URL (a checked
// socket directory), absolute trust and anchors paths, no credentials
// directory. It returns the directory holding the trust and anchors paths.
func ownerEnv(t *testing.T) string {
	t.Helper()
	clearEnv(t)
	dir := t.TempDir()
	t.Setenv("AUTHORITY_MIGRATE_DATABASE_URL", socketURL("authority_owner", shortDir(t), ""))
	t.Setenv("AUTHORITY_TRUST_PATH", filepath.Join(dir, "trust"))
	t.Setenv("AUTHORITY_ANCHORS_PATH", filepath.Join(dir, "anchors"))
	return dir
}

func ownerAccounts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = uuid.NewString()
	}
	return out
}

// The contract's usage line is pinned literally, so the malformed-form cases
// below cannot pass on the old usage text.
func TestOwnerUsageText(t *testing.T) {
	if usage != ownerUsage {
		t.Fatalf("usage = %q, want %q", usage, ownerUsage)
	}
}

func TestOwnerVerbUsage(t *testing.T) {
	h96 := strings.Repeat("ab", 48)
	u := uuid.NewString()
	cases := map[string][]string{
		"restore-begin extra":            {"restore-begin", "x"},
		"reconcile-complete extra":       {"reconcile-complete", "x"},
		"clear-held no account":          {"clear-held"},
		"clear-held dry-run only":        {"clear-held", "--dry-run"},
		"clear-held 65 accounts":         append([]string{"clear-held"}, ownerAccounts(65)...),
		"clear-held duplicate":           {"clear-held", u, u},
		"clear-held uppercase uuid":      {"clear-held", strings.ToUpper(u)},
		"clear-held non-hyphenated uuid": {"clear-held", strings.ReplaceAll(u, "-", "")},
		"clear-held not a uuid":          {"clear-held", "nope"},
		"clear-held unknown flag":        {"clear-held", "--bogus", u},
		"seal-legacy no flag":            {"seal-legacy"},
		"seal-legacy empty digest":       {"seal-legacy", "--cp-digest", ""},
		"seal-legacy 95 hex":             {"seal-legacy", "--cp-digest", h96[:95]},
		"seal-legacy 97 hex":             {"seal-legacy", "--cp-digest", h96 + "a"},
		"seal-legacy 96 non-hex":         {"seal-legacy", "--cp-digest", strings.Repeat("zz", 48)},
		"seal-legacy extra positional":   {"seal-legacy", "--cp-digest", h96, "x"},
		"seal-legacy repeated flag":      {"seal-legacy", "--cp-digest", h96, "--cp-digest", h96},
		"end-drain none":                 {"end-drain"},
		"end-drain one number":           {"end-drain", "1"},
		"end-drain empty serial":         {"end-drain", "1:"},
		"end-drain empty gen":            {"end-drain", ":2"},
		"end-drain letters":              {"end-drain", "a:b"},
		"end-drain three parts":          {"end-drain", "1:2:3"},
		"end-drain negative":             {"end-drain", "-1:2"},
		"end-drain fraction":             {"end-drain", "1.5:2"},
		"end-drain uint64 overflow":      {"end-drain", "99999999999999999999:1"},
		"end-drain extra":                {"end-drain", "1:2", "x"},
		"revoke-key none":                {"revoke-key"},
		"revoke-key no digest":           {"revoke-key", "cak"},
		"revoke-key unknown kind":        {"revoke-key", "rsk", h96},
		"revoke-key short digest":        {"revoke-key", "cak", h96[:94]},
		"revoke-key non-hex digest":      {"revoke-key", "operator", strings.Repeat("zz", 48)},
		"revoke-key extra":               {"revoke-key", "cp_tls", h96, "x"},
		"replication-gate none":          {"replication-gate"},
		"replication-gate ON":            {"replication-gate", "ON"},
		"replication-gate true":          {"replication-gate", "true"},
		"replication-gate two args":      {"replication-gate", "on", "off"},
		"replication-gate empty":         {"replication-gate", ""},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			clearEnv(t) // a form that reached Load would exit 1, not 2
			if code, stdout, stderr := run(args...); code != 2 || stderr != ownerUsage+"\n" || stdout != "" {
				t.Errorf("%q: code %d, stdout %q, stderr %q", args, code, stdout, stderr)
			}
		})
	}
}

// The positive control for TestOwnerVerbUsage: every well-formed form gets
// past parsing (and so is NOT usage), and with an empty environment dies at the
// loader as config. Without it the malformed cases would pass today, when every
// owner verb is an unknown verb and so exit 2.
func TestOwnerVerbWellFormedIsNotUsage(t *testing.T) {
	h96 := strings.Repeat("cd", 48)
	forms := [][]string{
		{"restore-begin"}, {"reconcile-complete"},
		{"clear-held", uuid.NewString()}, {"clear-held", "--dry-run", uuid.NewString(), uuid.NewString()},
		append([]string{"clear-held"}, ownerAccounts(64)...),
		{"seal-legacy", "--cp-digest", h96},
		{"end-drain", "0:1"}, {"end-drain", "18446744073709551615:7"},
		{"revoke-key", "cak", h96}, {"revoke-key", "operator", h96}, {"revoke-key", "cp_tls", h96},
		{"replication-gate", "on"}, {"replication-gate", "off"},
	}
	for _, args := range forms {
		clearEnv(t)
		code, stdout, stderr := run(args...)
		if code == 2 || stderr != "" {
			t.Errorf("%q: code %d, stderr %q: a well-formed form was refused as usage", args[:1], code, stderr)
			continue
		}
		exitLine(t, code, stdout, "config")
	}
}

// ownerExit is exitCode over err with a JSON log on a fresh buffer: the code
// and the one exit line it wrote.
func ownerExit(t *testing.T, err error) (int, map[string]any) {
	t.Helper()
	var out bytes.Buffer
	code := exitCode(logger.NewJSON(&out).Logger, err)
	if code == 0 {
		if out.Len() != 0 {
			t.Fatalf("exit 0 wrote %q", out.String())
		}
		return 0, nil
	}
	lines := logLines(t, out.String())
	if len(lines) != 1 || lines[0]["msg"] != "authority.exit" {
		t.Fatalf("exit %d wrote %q, want exactly one authority.exit line", code, out.String())
	}
	return code, lines[0]
}

// B3-D12's mapping, the hand-off's trust-file sentinels included: every
// owner-verb error exits 1 at step owner with its reason, and a replication
// failure carries DoR:1743's text in place of its cause, wrapped or not.
func TestOwnerErrMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason string
	}{
		{"serve lock held", store.ErrServeLockHeld, server.ReasonLock},
		{"lock wait ran out (55P03)", fmt.Errorf("authority store: reconcile-complete: %w", &pq.Error{Code: "55P03"}), server.ReasonLock},
		{"another database error stays config", &pq.Error{Code: "57014"}, server.ReasonConfig},
		{"serve lock lost mid-verb, ahead of replication", errors.Join(store.ErrServeLockLost,
			fmt.Errorf("%w: %w", store.ErrNotReplicated, context.Canceled)), server.ReasonLock},
		{"standby", store.ErrNotLeader, server.ReasonNotLeader},
		{"a failover during clear-held's row 22 (N1)", fmt.Errorf("authority store: row 22 refused before BEGIN: %w", store.ErrNotLeader), server.ReasonNotLeader},
		{"not replicated", store.ErrNotReplicated, server.ReasonReplication},
		{"not replicated, with a cause", fmt.Errorf("%w: %w", store.ErrNotReplicated, errors.New("probe: x")), server.ReasonReplication},
		{"refused", fmt.Errorf("%w: revoke-key: unknown", store.ErrRefused), server.ReasonRefused},
		{"trust unusable", errors.Join(store.ErrTrustUnusable, errors.New("x")), server.ReasonRefused},
		{"rsk reused", store.ErrRSKReused, server.ReasonRefused},
		{"no meta, as serve maps it (B2-D10)", store.ErrNoMeta, server.ReasonConfig},
		{"realm mismatch, as migrate maps it", store.ErrRealmMismatch, server.ReasonTrust},
		{"a halted installed trust (T4)", fmt.Errorf("authority store: trust halt: %w", errors.Join(errors.New("signer"), &ac.HaltError{Rule: "T4"})), server.ReasonTrust},
		{"anything else", errors.New("db down"), server.ReasonConfig},
		{"a deadline", fmt.Errorf("authority store: owner connection: %w", context.DeadlineExceeded), server.ReasonConfig},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, m := ownerExit(t, ownerErr(c.err))
			if code != 1 || m["reason"] != c.reason || m["step"] != server.StepOwner {
				t.Fatalf("exit %d, line %v; want 1, reason %s, step owner", code, m, c.reason)
			}
			if c.reason == server.ReasonReplication && errorOf(t, m) != "committed locally, not replicated, re-run" {
				t.Fatalf("error %q, want DoR:1743's text", errorOf(t, m))
			}
		})
	}
	if ownerErr(nil) != nil {
		t.Fatal("ownerErr(nil) is not nil")
	}
	exit := server.RefuseAt(server.StepInputs, server.ReasonTrust, errors.New("AUTHORITY_TRUST_PATH: x"))
	if got := ownerErr(exit); got != exit {
		t.Fatalf("an ExitError was remapped to %v", got)
	}
}

// clear-held's exit decision, at the narrowest seam: finishClear, everything
// the verb does after its store call. A per-account refusal through runMain
// needs a held head the commit core refuses for row 22, which only the store
// package's internal fixtures build, so this drives the seam directly.
func TestClearHeldRefusedResultExitsOne(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	refusedOut := store.ClearRefused + "clear-held: the commit core refused it"
	results := []store.ClearResult{{Account: a, Outcome: store.ClearUnverified}, {Account: b, Outcome: refusedOut}}

	var out bytes.Buffer
	code := exitCode(logger.NewJSON(&out).Logger, ownerErr(finishClear(&out, results, nil)))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if code != 1 || len(lines) != 3 {
		t.Fatalf("exit %d with %d lines %q, want 1 and two results then one exit line", code, len(lines), out.String())
	}
	if want := []string{"account=" + a + " outcome=unverified", "account=" + b + " outcome=" + refusedOut}; !slices.Equal(lines[:2], want) {
		t.Fatalf("results %q, want %q, in input order", lines[:2], want)
	}
	m := exitLine(t, 1, lines[2], server.ReasonRefused)
	if m["step"] != server.StepOwner || strings.Contains(lines[2], a) || strings.Contains(lines[2], b) {
		t.Fatalf("exit line %s: want step owner and no account id", lines[2])
	}

	// The control: the same results with no refusal exit 0 and log nothing.
	out.Reset()
	results[1].Outcome = store.ClearRestoreCancel
	if code := exitCode(logger.NewJSON(&out).Logger, ownerErr(finishClear(&out, results, nil))); code != 0 ||
		strings.Contains(out.String(), "authority.exit") {
		t.Fatalf("no refusal: exit %d, stdout %q; want 0 and no exit line", code, out.String())
	}

	// The store call's own error wins, and the results still print.
	out.Reset()
	results[1].Outcome = refusedOut
	if code, m := ownerExit(t, ownerErr(finishClear(&out, results, store.ErrNotReplicated))); code != 1 ||
		m["reason"] != server.ReasonReplication {
		t.Fatalf("exit %d, line %v; want replication", code, m)
	}
	if n := strings.Count(out.String(), "account="); n != 2 {
		t.Fatalf("%d result lines printed beside a store error, want 2", n)
	}
}

// Codex round 2, G5 at the CLI: a clear-held run the store stopped on a database
// failure prints the lines of the accounts before it and nothing for the one it
// stopped at, and exits by the failure's own reason, never refused.
func TestClearHeldStoppedRunExitsByItsReason(t *testing.T) {
	a := uuid.NewString()
	results := []store.ClearResult{{Account: a, Outcome: store.ClearUnverified}}
	for _, c := range []struct {
		name   string
		err    error
		reason string
	}{
		{"a lock wait that ran out", fmt.Errorf("authority store: clear-held: head: %w", &pq.Error{Code: "55P03"}), server.ReasonLock},
		{"a failover", fmt.Errorf("row 22: %w", store.ErrNotLeader), server.ReasonNotLeader},
		{"a database failure beside an unreplicated commit", errors.Join(errors.New("db down"), store.ErrNotReplicated), server.ReasonReplication},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			code := exitCode(logger.NewJSON(&out).Logger, ownerErr(finishClear(&out, results, c.err)))
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if code != 1 || len(lines) != 2 || lines[0] != "account="+a+" outcome=unverified" {
				t.Fatalf("exit %d, output %q; want 1, the one result line, then one exit line", code, out.String())
			}
			if m := exitLine(t, 1, lines[1], c.reason); m["step"] != server.StepOwner {
				t.Fatalf("exit line %s: want step owner", lines[1])
			}
		})
	}
}
