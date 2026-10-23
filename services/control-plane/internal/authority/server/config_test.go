package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/binary"
	"encoding/pem"
	"io"
	"io/fs"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lib/pq"
)

// testSockDir is the socket directory the unit tests' URLs name. Load judges
// the directory and every ancestor (checkSocketDir), so it must exist under a
// parent closed to other's write, with every ancestor owned by root or the
// euid: TestMain makes both, 0700, one level below os.TempDir, which is /tmp
// (1777, root, sticky) on Linux, and removes them.
var testSockDir string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "cvsock")
	if err != nil {
		panic(err)
	}
	testSockDir = filepath.Join(root, "sock")
	if err := os.Mkdir(testSockDir, 0o700); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

// socketURL is a valid socket-host URL for role, with extra query appended.
// URLs that carry a password are built through url.URL, so no literal
// credential appears in source.
func socketURL(role, extra string) string {
	return "postgres:///concord_authority?host=" + testSockDir + "&user=" + role + extra
}

func validEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	return map[string]string{
		"AUTHORITY_LISTEN_ADDR":          "10.0.0.5:8443",
		"AUTHORITY_TRUST_PATH":           filepath.Join(dir, "trust"),
		"AUTHORITY_ANCHORS_PATH":         filepath.Join(dir, "anchors"),
		"AUTHORITY_DISK_WATCH_PATH":      dir,
		"AUTHORITY_DATABASE_URL":         socketURL("authority_app", ""),
		"AUTHORITY_MIGRATE_DATABASE_URL": socketURL("authority_owner", ""),
		"CREDENTIALS_DIRECTORY":          dir,
	}
}

// environOf is m in os.Environ's form.
func environOf(m map[string]string) []string {
	env := make([]string, 0, len(m))
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	return env
}

// One case per (verb, variable) cell of DoR:1641–1649: unset, and set.
func TestLoadEveryCell(t *testing.T) {
	clearLibpqEnv(t)
	type cell struct {
		verb    Verb
		name    string
		unsetOK bool // the verb runs with the variable unset
		setOK   bool // the verb runs with a valid value set
	}
	table := []cell{
		{VerbServe, "AUTHORITY_LISTEN_ADDR", false, true},
		{VerbServe, "AUTHORITY_TRUST_PATH", false, true},
		{VerbServe, "AUTHORITY_ANCHORS_PATH", false, true},
		{VerbServe, "AUTHORITY_DISK_WATCH_PATH", false, true},
		{VerbServe, "AUTHORITY_DATABASE_URL", false, true},
		{VerbServe, "AUTHORITY_MIGRATE_DATABASE_URL", true, false}, // refuse if set
		{VerbServe, "CREDENTIALS_DIRECTORY", false, true},
		{VerbMigrate, "AUTHORITY_LISTEN_ADDR", true, true},
		{VerbMigrate, "AUTHORITY_TRUST_PATH", true, true},
		{VerbMigrate, "AUTHORITY_ANCHORS_PATH", true, true},
		{VerbMigrate, "AUTHORITY_DISK_WATCH_PATH", true, true},
		{VerbMigrate, "AUTHORITY_DATABASE_URL", true, false}, // refuse
		{VerbMigrate, "AUTHORITY_MIGRATE_DATABASE_URL", false, true},
		{VerbMigrate, "CREDENTIALS_DIRECTORY", true, true},
		// The owner-CLI column (DoR:1658-1666, #3587 PR-B3, B3-D1): the owner
		// URL and the trust and anchors paths are required, the serve URL is
		// refused, and the rest are never required: LISTEN and DISK_WATCH are
		// ignored when set, and CREDENTIALS_DIRECTORY is read when set, for
		// clear-held's RSK (TestOwnerLoadReadsTheCredentialsDirectoryWhenSet).
		{VerbOwner, "AUTHORITY_LISTEN_ADDR", true, true},
		{VerbOwner, "AUTHORITY_TRUST_PATH", false, true},
		{VerbOwner, "AUTHORITY_ANCHORS_PATH", false, true},
		{VerbOwner, "AUTHORITY_DISK_WATCH_PATH", true, true},
		{VerbOwner, "AUTHORITY_DATABASE_URL", true, false}, // refuse if set
		{VerbOwner, "AUTHORITY_MIGRATE_DATABASE_URL", false, true},
		{VerbOwner, "CREDENTIALS_DIRECTORY", true, true},
	}
	// B2-D14 and N1: every variable lib/pq reads is refused for every verb.
	// TestLoadRefusesEveryVariableLibpqReads covers the rest of pq.Config.
	libpq := []string{
		"PGPASSWORD", "PGPASSFILE", "PGSERVICE", "PGSERVICEFILE",
		"PGHOSTADDR", "PGOPTIONS", "PGTARGETSESSIONATTRS", "PGHOST", "PGPORT",
		"SSLPGMINPROTOCOLVERSION", "PQGO_DEBUG",
		// PG names lib/pq does not read today (it refuses PGREQUIRESSL itself):
		// the whole prefix is refused, so a lib/pq upgrade cannot widen it.
		"PGSYSCONFDIR", "PGREQUIRESSL",
	}
	cells := make([]cell, 0, len(table)+3*len(libpq))
	cells = append(cells, table...)
	for _, pg := range libpq {
		cells = append(cells, cell{VerbServe, pg, true, false}, cell{VerbMigrate, pg, true, false}, cell{VerbOwner, pg, true, false})
	}
	for _, c := range cells {
		base := validEnv(t)
		if c.verb == VerbServe {
			delete(base, "AUTHORITY_MIGRATE_DATABASE_URL")
		} else {
			delete(base, "AUTHORITY_DATABASE_URL")
		}
		unset, set := map[string]string{}, map[string]string{}
		for k, v := range base {
			unset[k], set[k] = v, v
		}
		delete(unset, c.name)
		if _, ok := set[c.name]; !ok {
			set[c.name] = validEnv(t)[c.name]
			if set[c.name] == "" {
				set[c.name] = "x"
			}
		}
		_, errU := Load(c.verb, environOf(unset))
		_, errS := Load(c.verb, environOf(set))
		if (errU == nil) != c.unsetOK || (errS == nil) != c.setOK {
			t.Errorf("verb %d %s: unset err %v (ok=%v), set err %v (ok=%v)", c.verb, c.name, errU, c.unsetOK, errS, c.setOK)
		}
		for _, err := range []error{errU, errS} {
			if err != nil && !strings.Contains(err.Error(), c.name) {
				t.Errorf("%s: the error does not name the variable: %v", c.name, err)
			}
		}
	}
}

func TestListenAddrRule(t *testing.T) {
	for _, c := range []struct {
		v    string
		want string // "" = refused
	}{
		{"10.0.0.5:8443", "10.0.0.5:8443"},
		{"127.0.0.1:8443", "127.0.0.1:8443"},
		{"[::ffff:10.0.0.5]:8443", "10.0.0.5:8443"}, // bound in the unmapped form
		{"[::1]:8443", "[::1]:8443"},
		{"[2001:db8::1]:8443", "[2001:db8::1]:8443"}, // B2-D29
		{"0.0.0.0:8443", ""},
		{"[::]:8443", ""},
		{"[::ffff:0.0.0.0]:8443", ""},
		{"10.0.0.5:0", ""},
		{"authority.internal:8443", ""},
		{"[fe80::1%eth0]:8443", ""},
		{"169.254.1.1:8443", ""},
		{"224.0.0.1:8443", ""},
	} {
		var cfg Config
		err := setListen(&cfg, c.v)
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s: accepted", c.v)
		case c.want != "" && (err != nil || cfg.ListenAddr != netip.MustParseAddrPort(c.want)):
			t.Errorf("%s: %v %v, want %s", c.v, cfg.ListenAddr, err, c.want)
		}
	}
}

func TestDatabaseURLRule(t *testing.T) {
	// Off Linux a named passfile is refused: its ACL cannot be read (B2-D16).
	onLinux := runtime.GOOS == "linux"
	sock := testSockDir
	withPassword := (&url.URL{Scheme: "postgres", User: url.UserPassword("authority_app", "x"), Path: "/db",
		RawQuery: "host=" + sock}).String()
	pf := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(pf, []byte("*:*:*:authority_app:x\n"), 0o600); err != nil { // pragma: allowlist secret
		t.Fatal(err)
	}
	loose := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(loose, []byte("x"), 0o640); err != nil { // #nosec G306 -- the loose mode is the case under test
		t.Fatal(err)
	}
	clearLibpqEnv(t)
	type urlCase struct {
		name, v string
		ok      bool
	}
	table := []urlCase{
		{"socket host, user in the query", socketURL("authority_app", ""), true},
		{"socket host, user in the userinfo", "postgresql://authority_app@/db?host=" + sock, true},
		{"a passfile (D-173)", socketURL("authority_app", "&passfile="+pf), onLinux},
		{"TCP host", "postgres://authority_app@localhost:5432/db", false},
		{"no host: lib/pq would dial localhost over TCP", "postgres:///db?user=authority_app", false},
		{"relative socket directory", "postgres:///db?host=run/postgresql&user=authority_app", false},
		{"password in the userinfo", withPassword, false},
		{"password in the query", socketURL("authority_app", "&pass"+"word=x"), false},
		{"a service", socketURL("authority_app", "&service=authority"), false},
		{"a repeated key", socketURL("authority_app", "&host=/tmp"), false},
		{"the owner role", socketURL("authority_owner", ""), false},
		{"no user", "postgres:///db?host=" + sock, false},
		{"user named twice", "postgres://authority_app@/db?host=" + sock + "&user=authority_app", false},
		{"another scheme", "mysql:///db?host=" + sock + "&user=authority_app", false},
		{"a group-readable passfile", socketURL("authority_app", "&passfile="+loose), false},
		{"a relative passfile", socketURL("authority_app", "&passfile=pgpass"), false},
		// Fix round 1, item 2: exactly one database.
		{"the database in the dbname key", "postgres://?host=" + sock + "&user=authority_app&dbname=db", true},
		{"no database: an empty path", "postgres:///?host=" + sock + "&user=authority_app", false},
		{"no database: no path", "postgresql://?host=" + sock + "&user=authority_app", false},
		// Fix round 1, item 3: the query keys are an allowlist.
		{"every allowed key", socketURL("authority_app",
			"&port=5433&connect_timeout=5&application_name=authority&sslmode=disable&passfile="+pf), onLinux},
		{"every allowed key but the passfile", socketURL("authority_app",
			"&port=5433&connect_timeout=5&application_name=authority&sslmode=disable"), true},
		{"a known key outside the allowlist: binary_parameters", socketURL("authority_app", "&binary_parameters=yes"), false},
		{"a known key outside the allowlist: sslrootcert", socketURL("authority_app", "&sslrootcert=system"), false},
		{"a known key outside the allowlist: target_session_attrs", socketURL("authority_app", "&target_session_attrs=any"), false},
		{"a known key outside the allowlist: krbsrvname", socketURL("authority_app", "&krbsrvname=postgres"), false},
	}
	// N1: URLs net/url reads as a socket login and lib/pq reads otherwise.
	// TestBypassVectorsAreLive proves lib/pq's reading of each.
	bypass := bypassURLs()
	cases := make([]urlCase, 0, len(table)+len(bypass))
	cases = append(cases, table...)
	for _, b := range bypass {
		cases = append(cases, urlCase{b.name, b.v, false})
	}
	for _, c := range cases {
		_, err := checkDBURL(c.v, "authority_app")
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
		if err != nil && (strings.Contains(err.Error(), "x@") || strings.Contains(err.Error(), c.v) ||
			strings.Contains(err.Error(), sock)) {
			t.Errorf("%s: the error quotes the URL: %v", c.name, err)
		}
	}
}

// The inline-password refusal is held by two layers, net/url's reading in
// parseDBURL and lib/pq's reading in checkLibpqView, and TestDatabaseURLRule
// passes with either deleted alone. These two tests pin each layer on its own,
// so neither can erode while the other hides it (Task 14, row 2). Neither
// prints the password: both assert the error is non-nil and carries a fixed
// fragment.
const inlineSentinel = "sentinel-7q3z" // pragma: allowlist secret

func TestParseDBURLRefusesAnInlinePassword(t *testing.T) {
	raw := (&url.URL{Scheme: "postgres", User: url.UserPassword("authority_app", inlineSentinel), Path: "/db",
		RawQuery: "host=/run/postgresql"}).String()
	if _, err := parseDBURL(raw, "authority_app"); err == nil {
		t.Fatal("parseDBURL accepted an inline password")
	} else if !strings.Contains(err.Error(), "no password") || strings.Contains(err.Error(), inlineSentinel) {
		t.Errorf("parseDBURL's refusal is not the fixed text, or quotes the secret")
	}
	// The control: the same URL without the password is read.
	clean := (&url.URL{Scheme: "postgres", User: url.User("authority_app"), Path: "/db",
		RawQuery: "host=/run/postgresql"}).String()
	if _, err := parseDBURL(clean, "authority_app"); err != nil {
		t.Errorf("control: parseDBURL refused the URL without a password: %v", err)
	}
}

func TestCheckLibpqViewRefusesAPassword(t *testing.T) {
	v := dbURLView{host: "/run/postgresql", user: "authority_app", database: "db"}
	cfg := pq.Config{Host: "/run/postgresql", User: "authority_app", Database: "db", Password: inlineSentinel}
	if err := checkLibpqView(cfg, v, "authority_app"); err == nil {
		t.Fatal("checkLibpqView accepted a Config carrying a password")
	} else if !strings.Contains(err.Error(), "no password") || strings.Contains(err.Error(), inlineSentinel) {
		t.Errorf("checkLibpqView's refusal is not the fixed text, or quotes the secret")
	}
	// The control: the same Config without the password passes.
	cfg.Password = ""
	if err := checkLibpqView(cfg, v, "authority_app"); err != nil {
		t.Errorf("control: checkLibpqView refused a Config without a password: %v", err)
	}
}

func TestFileAndDirRules(t *testing.T) {
	const euid = 1000
	for _, c := range []struct {
		name    string
		mode    fs.FileMode
		uid     uint32
		noWrite bool
		ok      bool
	}{
		{"0400 owned", 0o400, euid, true, true},
		{"owned by another user", 0o400, 1001, true, false},
		{"owned by root", 0o400, 0, true, false},
		{"group-readable", 0o440, euid, true, false},
		{"other-readable", 0o404, euid, true, false},
		{"writable credential", 0o600, euid, true, false},
		{"writable passfile", 0o600, euid, false, true},
		{"a directory", fs.ModeDir | 0o700, euid, true, false},
		{"a symlink", fs.ModeSymlink | 0o400, euid, true, false},
	} {
		if err := fileRule(c.mode, c.uid, euid, c.noWrite); (err == nil) != c.ok {
			t.Errorf("file %s: %v", c.name, err)
		}
	}
	for _, c := range []struct {
		name string
		mode fs.FileMode
		uid  uint32
		ok   bool
	}{
		{"0700 owned by the euid", fs.ModeDir | 0o700, euid, true},
		{"0755 owned by root (compose /run/secrets)", fs.ModeDir | 0o755, 0, true},
		{"owned by another user", fs.ModeDir | 0o700, 1001, false},
		{"group-writable", fs.ModeDir | 0o770, euid, false},
		{"other-writable", fs.ModeDir | fs.ModeSticky | 0o777, 0, false},
		{"not a directory", 0o700, euid, false},
	} {
		if err := dirRule(c.mode, c.uid, euid); (err == nil) != c.ok {
			t.Errorf("dir %s: %v", c.name, err)
		}
	}
}

// chmodExact sets mode on p and fails unless the kernel kept every bit: a
// setgid bit the kernel silently cleared would make an accepting case vacuous.
func chmodExact(t *testing.T, p string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode() & (fs.ModePerm | fs.ModeSetgid | fs.ModeSticky); got != mode&(fs.ModePerm|fs.ModeSetgid|fs.ModeSticky) {
		t.Fatalf("chmod %v: the kernel kept %v", mode, got)
	}
}

// Sec-I1: the socket directory lib/pq dials is judged at Load. Peer
// authentication sends no secret, so a directory another user can bind in
// would let that user be the authority's database.
func TestSocketDirRule(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, mode fs.FileMode) string {
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		chmodExact(t, p, mode)
		return p
	}
	good := mk("good", 0o700)
	link := filepath.Join(root, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A parent another user can write lets that user create the directory
	// itself while it is absent; a parent reached through a link is judged at
	// the link's target, as the kernel dials through it (Debian's /var/run).
	sticky := mk("tmp", fs.ModeSticky|0o777)
	under := filepath.Join(sticky, "pgsock")
	if err := os.Mkdir(under, 0o700); err != nil {
		t.Fatal(err)
	}
	chmodExact(t, under, 0o755)
	closedParent := mk("run", 0o755)
	if err := os.Mkdir(filepath.Join(closedParent, "postgresql"), 0o700); err != nil {
		t.Fatal(err)
	}
	varRun := filepath.Join(root, "varrun")
	if err := os.Symlink(closedParent, varRun); err != nil {
		t.Fatal(err)
	}
	tmpLink := filepath.Join(root, "tmplink")
	if err := os.Symlink(sticky, tmpLink); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, dir, want string // want "" = accepted
	}{
		{"0700 (accepted)", good, ""},
		{"2775, Debian's /var/run/postgresql (accepted)", mk("debian", fs.ModeSetgid|0o775), ""},
		{"group-writable 0770 (accepted)", mk("group", 0o770), ""},
		{"missing", filepath.Join(root, "absent"), "no such file"},
		{"a symlink to a good directory", link, "a symlink"},
		{"a symlink named with a trailing slash", link + "/", "clean"},
		{"a symlink named with a final dot", link + "/.", "clean"},
		{"a file", file, "not a directory"},
		{"0777", mk("open", 0o777), "writable by other"},
		{"1777, like /tmp", mk("sticky", fs.ModeSticky|0o777), "writable by other"},
		{"0703: other-write alone", mk("other", 0o703), "writable by other"},
		{"a 0755 directory under a 1777 parent, like /tmp/<name>", under, "parent is writable by group or other"},
		{"a parent reached through a link to a closed directory (accepted)", filepath.Join(varRun, "postgresql"), ""},
		{"a parent reached through a link to a 1777 directory", filepath.Join(tmpLink, "pgsock"), "parent is writable by group or other"},
	} {
		checkSocketCase(t, c.name, c.dir, c.want, root)
	}
}

// checkSocketCase runs checkSocketDir on dir: want "" is accepted, otherwise a
// refusal saying want, which names the socket directory and no path under root.
func checkSocketCase(t *testing.T, name, dir, want, root string) {
	t.Helper()
	resolvedRoot, rerr := filepath.EvalSymlinks(root)
	if rerr != nil {
		t.Fatal(rerr)
	}
	err := checkSocketDir(dir)
	switch {
	case want == "" && err != nil:
		t.Errorf("%s: refused: %v", name, err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Errorf("%s: %v, want a refusal saying %q", name, err, want)
	case err != nil && (strings.Contains(err.Error(), root) || strings.Contains(err.Error(), resolvedRoot) ||
		!strings.Contains(err.Error(), "socket directory")):
		t.Errorf("%s: the refusal names the path, or not the socket directory: %v", name, err)
	}
}

// CWE-379: every ancestor of the socket directory, on the path
// the kernel resolves, is owned by root or the euid, and a directory above the
// parent may be other-writable only when sticky. Another user's /tmp/<a>/<b>,
// both made after a reboot, passed when only the directory and its parent's
// mode were judged. Another user's ownership is simulated through
// ancestorOwner, since making it needs root.
func TestSocketDirAncestorRule(t *testing.T) {
	root := t.TempDir()
	mkAll := func(mode fs.FileMode, parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		chmodExact(t, filepath.Join(root, parts[0]), mode)
		return p
	}
	theirs := mkAll(0o755, "theirs", "pg")                       // /tmp/<theirs>/<dir>: the parent is another user's
	foreign := mkAll(0o755, "foreign", "a", "sock")              // /srv/pg/<a>/<dir>: a grandparent is another user's
	openGrand := mkAll(0o777, "open", "a", "sock")               // a grandparent other-writable, not sticky
	stickyGrand := mkAll(fs.ModeSticky|0o777, "st", "a", "sock") // /tmp/<yours>/<dir>
	linkForeign := filepath.Join(root, "linkforeign")
	if err := os.Symlink(filepath.Join(root, "foreign"), linkForeign); err != nil {
		t.Fatal(err)
	}
	other := map[string]bool{}
	for _, name := range []string{"theirs", "foreign"} {
		p, err := filepath.EvalSymlinks(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		other[p] = true
	}
	hook := ancestorOwner
	t.Cleanup(func() { ancestorOwner = hook })
	ancestorOwner = func(p string, fi fs.FileInfo) uint32 {
		if other[p] {
			return 4242
		}
		return hook(p, fi)
	}
	for _, c := range []struct{ name, dir, want string }{
		{"a parent another user owns, like /tmp/<theirs>/<dir>", theirs, "parent is not owned by root or the euid"},
		{"a grandparent another user owns, like /srv/pg/<a>/<dir>", foreign, "ancestor of the socket directory is not owned by root or the euid"},
		{"a link through to a grandparent another user owns", filepath.Join(linkForeign, "a", "sock"), "ancestor of the socket directory is not owned by root or the euid"},
		{"a grandparent other-writable without the sticky bit", openGrand, "ancestor of the socket directory is writable by group or other without the sticky bit"},
		{"a sticky other-writable grandparent, like /tmp/<yours>/<dir> (accepted)", stickyGrand, ""},
	} {
		checkSocketCase(t, c.name, c.dir, c.want, root)
	}
	ancestorOwner = hook
	checkSocketCase(t, "control: the same parent, owned by the euid (accepted)", theirs, "", root)
}

// A link on the socket directory's path
// is judged where it is looked up, not only where it leads. One another user
// owns can be removed and made again by that user, even in a sticky /tmp,
// and so re-point the dial; and the directory holding a link can lie off the
// resolved path. Above the socket directory group write is refused as other
// write is: on the parent always, higher up unless sticky. The socket
// directory itself stays group-writable (TestSocketDirRule's 2775 case).
func TestSocketPathLinksAndGroupWrite(t *testing.T) {
	root := t.TempDir()
	rroot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	at := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	mkdirs := func(modes map[string]fs.FileMode, parts ...string) string {
		for i := range parts {
			p := at(parts[:i+1]...)
			if err := os.Mkdir(p, 0o700); err != nil && !os.IsExist(err) {
				t.Fatal(err)
			}
		}
		for i := range parts {
			if m, ok := modes[parts[i]]; ok {
				chmodExact(t, at(parts[:i+1]...), m)
			}
		}
		return at(parts...)
	}
	link := func(target string, parts ...string) {
		if err := os.Symlink(target, at(parts...)); err != nil {
			t.Fatal(err)
		}
	}
	mkdirs(nil, "real", "sock")
	link(at("real"), "theirlink")
	link(at("real"), "ownlink")
	link("real", "rel")
	link("..", "real", "up")
	mkdirs(nil, "open")
	link(at("real"), "open", "l")
	chmodExact(t, at("open"), 0o777)
	mkdirs(nil, "st")
	link(at("real"), "st", "l")
	chmodExact(t, at("st"), fs.ModeSticky|0o777)
	groupGrand := mkdirs(map[string]fs.FileMode{"gw": 0o775}, "gw", "a", "sock")
	groupParent := mkdirs(map[string]fs.FileMode{"gp": 0o770}, "gp", "sock")
	stickyGroup := mkdirs(map[string]fs.FileMode{"gs": fs.ModeSticky | 0o770}, "gs", "a", "sock")

	hook := ancestorOwner
	t.Cleanup(func() { ancestorOwner = hook })
	theirs := filepath.Join(rroot, "theirlink")
	ancestorOwner = func(p string, fi fs.FileInfo) uint32 {
		if p == theirs {
			return 4242
		}
		return hook(p, fi)
	}
	for _, c := range []struct{ name, dir, want string }{
		{"a link another user owns, like /tmp/<their link>/<dir>", at("theirlink", "sock"), "path holds a link not owned by root or the euid"},
		{"a link the euid owns, the /var/run -> /run shape (accepted)", at("ownlink", "sock"), ""},
		{"a relative link (accepted)", at("rel", "sock"), ""},
		{"a link to .. (accepted)", at("real", "up", "real", "sock"), ""},
		{"a link held by an other-writable directory off the resolved path", at("open", "l", "sock"),
			"ancestor of the socket directory is writable by group or other without the sticky bit"},
		{"a link the euid owns in a sticky directory (accepted)", at("st", "l", "sock"), ""},
		{"a 0775 grandparent the euid owns", groupGrand, "ancestor of the socket directory is writable by group or other without the sticky bit"},
		{"a 0770 parent the euid owns", groupParent, "parent is writable by group or other"},
		{"a sticky group-writable grandparent (accepted)", stickyGroup, ""},
	} {
		checkSocketCase(t, c.name, c.dir, c.want, root)
	}
	ancestorOwner = hook
	checkSocketCase(t, "control: the same link, owned by the euid (accepted)", at("theirlink", "sock"), "", root)
}

// The rule is reached from Load: checkDBURL refuses an other-writable socket
// directory, without the path, and accepts the same URL once it is closed.
func TestDatabaseURLRuleJudgesTheSocketDirectory(t *testing.T) {
	clearLibpqEnv(t)
	dir := t.TempDir()
	chmodExact(t, dir, fs.ModeSticky|0o777)
	raw := "postgres:///db?host=" + dir + "&user=authority_app"
	if _, err := checkDBURL(raw, "authority_app"); err == nil || !strings.Contains(err.Error(), "socket directory") ||
		strings.Contains(err.Error(), dir) {
		t.Fatalf("a 1777 socket directory: %v, want a refusal without the path", err)
	}
	chmodExact(t, dir, 0o700)
	if _, err := checkDBURL(raw, "authority_app"); err != nil {
		t.Fatalf("control: the closed directory was refused: %v", err)
	}
	env := validEnv(t)
	delete(env, "AUTHORITY_MIGRATE_DATABASE_URL")
	env["AUTHORITY_DATABASE_URL"] = "postgres:///db?host=" + filepath.Join(dir, "absent") + "&user=authority_app"
	if _, err := Load(VerbServe, environOf(env)); err == nil || !strings.Contains(err.Error(), "AUTHORITY_DATABASE_URL") {
		t.Fatalf("Load with a missing socket directory: %v, want a refusal naming the variable", err)
	}
}

// Sec-M1: the passfile's parent passes dirRule, so no writer but root or the
// euid can swap the file lib/pq re-reads by path. The path must be clean, so
// the parent judged is the one the kernel resolves.
func TestPassfileParentRule(t *testing.T) {
	put := func(mode fs.FileMode) (dir, pf string) {
		dir = t.TempDir()
		pf = filepath.Join(dir, "pgpass")
		if err := os.WriteFile(pf, []byte("*:*:*:authority_app:x\n"), 0o600); err != nil { // pragma: allowlist secret
			t.Fatal(err)
		}
		chmodExact(t, dir, fs.ModeDir|mode)
		return dir, pf
	}
	_, good := put(0o700)
	if err := checkPassfile(good); runtime.GOOS == "linux" && err != nil {
		t.Fatalf("a 0700 parent: %v", err)
	} else if runtime.GOOS != "linux" && (err == nil || !strings.Contains(err.Error(), "only on Linux")) {
		t.Fatalf("off Linux a named passfile passed, or not for its ACL: %v (B2-D16)", err)
	}
	missing := filepath.Join(filepath.Dir(good), "absent")
	if err := checkPassfile(missing); err == nil || !strings.Contains(err.Error(), "no such file") ||
		strings.Contains(err.Error(), filepath.Dir(good)) {
		t.Errorf("a missing passfile: %v, want its cause without the path", err)
	}
	realDir, _ := put(0o700)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	_, groupW := put(0o770)
	_, otherW := put(0o703)
	for _, c := range []struct{ name, p, want string }{
		{"a group-writable parent", groupW, "writable by group or other"},
		{"an other-writable parent", otherW, "writable by group or other"},
		{"a symlinked parent", filepath.Join(link, "pgpass"), "not a directory"},
		{"an unclean path", filepath.Dir(good) + "/./pgpass", "clean"},
	} {
		err := checkPassfile(c.p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal saying %q", c.name, err, c.want)
		} else if strings.Contains(err.Error(), filepath.Dir(c.p)) {
			t.Errorf("%s: the refusal names the path: %v", c.name, err)
		}
	}
	clearLibpqEnv(t)
	if _, err := checkDBURL(socketURL("authority_app", "&passfile="+groupW), "authority_app"); err == nil {
		t.Error("checkDBURL accepted a passfile in a group-writable directory")
	}
}

func writeCred(t *testing.T, dir, name string, b []byte, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestReadCredential(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a credentials directory is 0700
		t.Fatal(err)
	}
	writeCred(t, dir, "rsk", []byte("key"), 0o400)
	b, err := readCredential(dir, "rsk")
	if runtime.GOOS == "linux" {
		if err != nil || string(b) != "key" {
			t.Fatalf("a valid credential: %q %v", b, err)
		}
	} else if err == nil {
		t.Fatal("serve's credential check passed on a non-Linux OS (B2-D16)")
	}
	writeCred(t, dir, "loose", []byte("key"), 0o440)
	writeCred(t, dir, "big", make([]byte, credMax+1), 0o400)
	if err := os.Symlink(filepath.Join(dir, "rsk"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"loose", "big", "link", "missing"} {
		if _, err := readCredential(dir, name); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	etc, _ := filepath.EvalSymlinks("/etc")
	if os.Geteuid() != 0 {
		if _, err := readCredential(etc, "hosts"); err == nil || !strings.Contains(err.Error(), "owned") {
			t.Errorf("a root-owned file: %v, want an ownership refusal", err)
		}
	}
	if err := os.Chmod(dir, 0o770); err != nil { // #nosec G302 -- the group-writable parent is the case under test
		t.Fatal(err)
	}
	if _, err := readCredential(dir, "rsk"); err == nil {
		t.Error("a group-writable parent passed")
	}
}

// u16 narrows an ACL tag or permission; the fixtures hold only small values.
func u16(v uint32) uint16 {
	if v > math.MaxUint16 {
		panic("an ACL tag or permission is a u16")
	}
	return uint16(v)
}

func aclBytes(entries ...[3]uint32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, aclVersion)
	for _, e := range entries {
		b = binary.LittleEndian.AppendUint16(b, u16(e[0]))
		b = binary.LittleEndian.AppendUint16(b, u16(e[1]))
		b = binary.LittleEndian.AppendUint32(b, e[2])
	}
	return b
}

func TestCheckACL(t *testing.T) {
	const euid, undef = 1000, 0xffffffff
	obj := [][3]uint32{{0x01, 4, undef}, {0x04, 0, undef}, {0x10, 0, undef}, {0x20, 0, undef}}
	with := func(extra ...[3]uint32) []byte { return aclBytes(append(append([][3]uint32{}, obj...), extra...)...) }
	for _, c := range []struct {
		name string
		b    []byte
		ok   bool
	}{
		{"no named entries", with(), true},
		{"the euid named", with([3]uint32{aclTagUser, 4, euid}), true},
		{"another user with read", with([3]uint32{aclTagUser, 4, 1001}), false},
		{"another user with no permission", with([3]uint32{aclTagUser, 0, 1001}), true},
		{"a named group with read", with([3]uint32{aclTagGroup, 4, 50}), false},
		// group::r-- under mask::--- shows no group bit in the
		// mode, and a later chmod g+r grants the owning group the read.
		{"the owning group with read under an empty mask",
			aclBytes([3]uint32{0x01, 4, undef}, [3]uint32{aclTagGroupObj, 4, undef}, [3]uint32{0x10, 0, undef}, [3]uint32{0x20, 0, undef}), false},
		{"wrong version", append([]byte{1, 0, 0, 0}, with()[4:]...), false},
		{"truncated", with()[:9], false},
	} {
		if err := checkACL(c.b, euid); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestParseKey(t *testing.T) {
	der := func(k any) []byte {
		b, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	k256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	krsa, _ := rsa.GenerateKey(rand.Reader, 2048)
	if k, err := parseKey(der(k384)); err != nil || !k.Equal(k384) {
		t.Fatalf("P-384 PKCS#8: %v", err)
	}
	pemForm := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der(k384)})
	for name, b := range map[string][]byte{"P-256": der(k256), "RSA": der(krsa), "PEM": pemForm, "garbage": []byte("x")} {
		if _, err := parseKey(b); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// bypassURL is a URL net/url reads as a clean socket login for authority_app,
// with live reporting whether lib/pq v1.12.3 reads it some other way.
type bypassURL struct {
	name, v string
	live    func(pq.Config) bool
}

// bypassURLs are N1's vectors (the red team's PoC URLs) and their relatives.
// Each passes a check that reads only net/url's view of the URL.
func bypassURLs() []bypassURL {
	const base = "postgres:///db?host=/run/postgresql&user=authority_app"
	// lib/pq writes query KEYS into its key=value string unescaped, so one
	// encoded key can carry whole key=value pairs. lib/pq sorts the pairs and
	// the last one named wins: a "zz" prefix sorts the smuggled pairs last.
	smuggle := func(pairs string) string { return base + "&" + url.QueryEscape("zz='b' "+pairs+" z") + "=x" }
	return []bypassURL{
		{"hostaddr key", base + "&hostaddr=127.0.0.1&sslmode=disable",
			func(c pq.Config) bool { return c.Hostaddr.IsValid() }},
		{"comma host list", "postgres:///db?host=/run/postgresql,127.0.0.1&user=authority_app&sslmode=disable",
			func(c pq.Config) bool { return len(c.Multi) == 1 && c.Multi[0].Host == "127.0.0.1" }},
		{"DSN key injection through an encoded query key (PoC)", base + "&" +
			url.QueryEscape("target_session_attrs='any' pass"+"word='x' host='127.0.0.1' sslmode") + "=disable",
			func(c pq.Config) bool { return c.Password == "x" && c.Host == "127.0.0.1" }},
		{"an encoded key smuggling a passfile", smuggle("passfile='/tmp/pgpass'"),
			func(c pq.Config) bool { return c.Passfile == "/tmp/pgpass" }},
		{"an encoded key smuggling a service", smuggle("service='authority'"),
			func(c pq.Config) bool { return c.Service == "authority" }},
		{"an encoded key smuggling a superuser login that switches to the app role",
			smuggle("user='postgres' options='-c role=authority_app'"),
			func(c pq.Config) bool { return c.User == "postgres" && c.Options == "-c role=authority_app" }},
		{"options switching the role", base + "&options=" + url.QueryEscape("-c role=authority_app"),
			func(c pq.Config) bool { return c.Options == "-c role=authority_app" }},
		{"options under a percent-encoded key name", base + "&%6Fptions=" + url.QueryEscape("-c role=authority_app"),
			func(c pq.Config) bool { return c.Options == "-c role=authority_app" }},
		{"unknown keys become startup GUCs (PoC)", base + "&synchronous_commit=off&search_path=public",
			func(c pq.Config) bool { return c.Runtime["synchronous_commit"] == "off" }},
		{"an upper-case scheme: lib/pq reads a key=value string and dials localhost over TCP",
			"POSTGRES:///db?host=/run/postgresql&user=authority_app",
			func(c pq.Config) bool { return c.Host == "localhost" }},
		{"the database named by the path and the database key", base + "&database=postgres",
			func(c pq.Config) bool { return c.Database == "postgres" }},
		{"the database named by the path and the dbname key", base + "&dbname=postgres",
			func(c pq.Config) bool { return c.Database == "postgres" }},
		{"a key holding a quote", base + "&" + url.QueryEscape("a'b") + "=x",
			func(c pq.Config) bool { return len(c.Runtime) > 0 }},
		{"a key holding a backslash", base + "&" + url.QueryEscape(`a\b`) + "=x",
			func(c pq.Config) bool { return len(c.Runtime) > 0 }},
		{"a key holding '='", base + "&" + url.QueryEscape("a=b") + "=x",
			func(c pq.Config) bool { return len(c.Runtime) > 0 }},
		{"a key whose tabs separate smuggled pairs", base + "&" + url.QueryEscape("zz=b\tuser=postgres\tz") + "=x",
			func(c pq.Config) bool { return c.User == "postgres" }},
	}
}

// clearLibpqEnv removes, for the test, every variable lib/pq reads, by the
// rule's own predicate. CI's Go job sets PGHOST, PGUSER and PGPASSWORD, and
// pq.NewConfig reads them.
func clearLibpqEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if libpqReads(k) {
			t.Setenv(k, "") // registers the restore
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The vectors are not vacuous: lib/pq v1.12.3 reads each one the dangerous
// way. And each layer of the rule refuses each one on its own, so neither
// layer is dead code behind the other.
func TestBypassVectorsAreLive(t *testing.T) {
	clearLibpqEnv(t)
	// The service row needs a service file to name, as an attacker's would.
	sf := filepath.Join(t.TempDir(), "pg_service.conf")
	if err := os.WriteFile(sf, []byte("[authority]\ndbname=db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", sf)
	clean := dbURLView{host: "/run/postgresql", user: "authority_app", database: "db"}
	for _, b := range bypassURLs() {
		if _, err := parseDBURL(b.v, "authority_app"); err == nil {
			t.Errorf("%s: net/url's layer accepted it", b.name)
		}
		c, err := pq.NewConfig(b.v)
		if err != nil {
			t.Errorf("%s: lib/pq refuses it, so the row proves nothing: %v", b.name, err)
			continue
		}
		if !b.live(c) {
			t.Errorf("%s: lib/pq no longer reads it the dangerous way; re-derive the row", b.name)
		}
		if err := checkLibpqView(c, clean, "authority_app"); err == nil {
			t.Errorf("%s: lib/pq's layer accepted it", b.name)
		}
	}
}

// Each clause of lib/pq's layer refuses on its own, from a reading that
// passes.
func TestLibpqViewRule(t *testing.T) {
	clearLibpqEnv(t)
	good, err := pq.NewConfig(socketURL("authority_app", ""))
	if err != nil {
		t.Fatal(err)
	}
	v := dbURLView{host: testSockDir, user: "authority_app", database: "concord_authority"}
	if err := checkLibpqView(good, v, "authority_app"); err != nil {
		t.Fatalf("the clean reading: %v", err)
	}
	for name, mutate := range map[string]func(c *pq.Config){
		"a hostaddr":                       func(c *pq.Config) { c.Hostaddr = netip.MustParseAddr("127.0.0.1") },
		"a second host":                    func(c *pq.Config) { c.Multi = []pq.ConfigMultihost{{Host: "/tmp"}} },
		"a TCP host":                       func(c *pq.Config) { c.Host = "localhost" },
		"an abstract socket":               func(c *pq.Config) { c.Host = "@authority" },
		"a comma in the host":              func(c *pq.Config) { c.Host = testSockDir + ",/tmp" },
		"a password":                       func(c *pq.Config) { c.Password = "x" },
		"a service":                        func(c *pq.Config) { c.Service = "authority" },
		"options":                          func(c *pq.Config) { c.Options = "-c role=authority_app" },
		"a run-time parameter":             func(c *pq.Config) { c.Runtime = map[string]string{"synchronous_commit": "off"} },
		"another user":                     func(c *pq.Config) { c.User = "postgres" },
		"another socket directory":         func(c *pq.Config) { c.Host = "/tmp" },
		"another database":                 func(c *pq.Config) { c.Database = "postgres" },
		"a passfile the URL does not name": func(c *pq.Config) { c.Passfile = "/tmp/pgpass" },
	} {
		c := good.Clone()
		mutate(&c)
		if err := checkLibpqView(c, v, "authority_app"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// lib/pq writes query keys unescaped, so a key is held to lib/pq's own key
// alphabet, and then to the allowlist.
func TestQueryKeyRule(t *testing.T) {
	for _, c := range []struct {
		key string
		ok  bool
	}{
		{"host", true}, {"port", true}, {"user", true}, {"dbname", true}, {"passfile", true},
		{"connect_timeout", true}, {"application_name", true}, {"sslmode", true},
		{"target_session_attrs", false}, {"binary_parameters", false}, {"disable_prepared_binary_result", false},
		{"sslrootcert", false}, {"sslnegotiation", false}, {"krbsrvname", false}, {"krbspn", false},
		{"hostaddr", false}, {"options", false}, {"database", false}, {"load_balance_hosts", false},
		{"synchronous_commit", false}, {"password", false}, {"service", false},
	} {
		if err := checkQueryKeys(url.Values{c.key: {"x"}}); (err == nil) != c.ok {
			t.Errorf("%q: %v", c.key, err)
		}
	}
	// Keys outside [a-z_]: each is refused by the alphabet rule, whose text
	// says why, and the refusal never quotes the key. The allowlist would
	// refuse them too, so the text is what proves the alphabet rule ran.
	for _, key := range []string{"Host", "a=b", "a'", `a\`, "a b", "", "a'b", `a\b`, "a\tb", "a\nb", "a\x00b", "port2"} {
		err := checkQueryKeys(url.Values{key: {"x"}})
		if err == nil || !strings.Contains(err.Error(), "[a-z_]") {
			t.Errorf("%q: %v, want the alphabet refusal", key, err)
		} else if key != "" && strings.Contains(err.Error(), key) {
			t.Errorf("%q: the error quotes the key", key)
		}
	}
	if err := checkQueryKeys(url.Values{"host": {"/a", "/b"}}); err == nil {
		t.Error("a repeated key passed")
	}
}

// Load hands back lib/pq's checked reading of the URL, the one Task 10 dials.
func TestLoadReturnsTheCheckedDialConfig(t *testing.T) {
	clearLibpqEnv(t)
	env := validEnv(t)
	delete(env, "AUTHORITY_MIGRATE_DATABASE_URL")
	c, err := Load(VerbServe, environOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseURL != env["AUTHORITY_DATABASE_URL"] || c.DB.Host != testSockDir || c.DB.User != "authority_app" ||
		c.DB.Database != "concord_authority" || c.DB.Hostaddr.IsValid() || len(c.DB.Runtime) != 0 ||
		c.DB.Passfile != devNullChild {
		t.Fatalf("DB: host %q user %q database %q passfile %q", c.DB.Host, c.DB.User, c.DB.Database, c.DB.Passfile)
	}
	if !c.checked {
		t.Fatal("Load's Config is not marked checked; OpenDB would refuse it")
	}
}

// A variable set to the empty string is set: lib/pq counts an empty PG*
// variable as set, and a refused variable set empty is refused (fix round 1,
// item 5). A variable named twice has no one value.
func TestLoadRefusesEmptyAndRepeatedVariables(t *testing.T) {
	clearLibpqEnv(t)
	serve, migrate := validEnv(t), validEnv(t)
	delete(serve, "AUTHORITY_MIGRATE_DATABASE_URL")
	delete(migrate, "AUTHORITY_DATABASE_URL")
	for _, c := range []struct {
		name, want string
		verb       Verb
		environ    []string
	}{
		{"an empty PG* variable", "PGOPTIONS", VerbServe, append(environOf(serve), "PGOPTIONS=")},
		{"a repeated variable", "AUTHORITY_DATABASE_URL", VerbServe,
			append(environOf(serve), "AUTHORITY_DATABASE_URL="+socketURL("authority_owner", ""))},
		{"serve with an empty migrate URL", "AUTHORITY_MIGRATE_DATABASE_URL", VerbServe,
			append(environOf(serve), "AUTHORITY_MIGRATE_DATABASE_URL=")},
		{"migrate with an empty serve URL", "AUTHORITY_DATABASE_URL", VerbMigrate,
			append(environOf(migrate), "AUTHORITY_DATABASE_URL=")},
	} {
		if _, err := Load(c.verb, c.environ); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal naming %s", c.name, err, c.want)
		}
	}
}

// Fix round 1, item 4: a verb Load does not know is refused, not read as
// serve.
func TestLoadRefusesAnUnknownVerb(t *testing.T) {
	clearLibpqEnv(t)
	env := validEnv(t)
	delete(env, "AUTHORITY_MIGRATE_DATABASE_URL")
	for _, v := range []Verb{0, VerbOwner + 1, -1} {
		if _, err := Load(v, environOf(env)); err == nil {
			t.Errorf("verb %d: loaded", v)
		}
	}
}

// Fix round 1, item 1: when the URL names no passfile, lib/pq falls back to
// $HOME/.pgpass on every connection. The checked Config names a passfile
// that cannot exist instead, so nothing resolves to the home file. A fake
// server on the socket asks for a cleartext password and records what lib/pq
// sends, and the positive control shows it would have sent the home file's.
func TestUnnamedPassfileNeverFallsBackToHome(t *testing.T) {
	clearLibpqEnv(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".pgpass"), []byte("*:*:*:authority_app:from-home\n"), 0o600); err != nil { // pragma: allowlist secret
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	sock := pgSocket(t)
	srv := newFakeAuthPG(t, filepath.Join(sock, ".s.PGSQL.5432"))
	cfg, err := checkDBURL("postgres:///db?host="+sock+"&user=authority_app", "authority_app")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Passfile != devNullChild {
		t.Errorf("Passfile %q, want the sentinel", cfg.Passfile)
	}
	control := cfg
	control.Passfile = ""
	if got := srv.passwordSent(t, control); got != "from-home" {
		t.Fatalf("positive control: lib/pq sent %q, so the harness cannot see the home fallback", got)
	}
	if got := srv.passwordSent(t, cfg); got != "" {
		t.Fatalf("the checked Config sent a password from the home pgpass: %q", got)
	}
}

// fakeAuthPG is a unix-socket server that asks for a cleartext password and
// reports the password lib/pq sends.
type fakeAuthPG struct{ got chan string }

func newFakeAuthPG(t *testing.T, path string) *fakeAuthPG {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeAuthPG{got: make(chan string, 4)}
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
	// AuthenticationCleartextPassword: 'R', length 8, code 3.
	if _, err := c.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 3}); err != nil {
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

// passwordSent dials cfg once and returns the password the server received.
func (f *fakeAuthPG) passwordSent(t *testing.T, cfg pq.Config) string {
	t.Helper()
	conn, err := pq.NewConnectorConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ping(t, sql.OpenDB(conn))
	select {
	case p := <-f.got:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("lib/pq sent no password message")
		return ""
	}
}

// pq.NewConfig reads the process environment, so the URL rule refuses a URL
// whenever a variable lib/pq reads is set (the PoC's PGHOSTADDR case).
func TestDatabaseURLRuleRefusesTheLibpqEnvironment(t *testing.T) {
	for _, kv := range [][2]string{
		{"PGHOSTADDR", "127.0.0.1"}, {"PGOPTIONS", "-c role=authority_app"}, {"PGSERVICE", "authority"},
		{"PGPASSWORD", "x"}, {"PGTARGETSESSIONATTRS", "read-write"}, {"PGPORT", "6432"},
		{"SSLPGMINPROTOCOLVERSION", "TLSv1.3"}, {"PQGO_DEBUG", "1"},
	} {
		t.Run(kv[0], func(t *testing.T) {
			clearLibpqEnv(t)
			t.Setenv(kv[0], kv[1])
			_, err := checkDBURL(socketURL("authority_app", ""), "authority_app")
			if err == nil || !strings.Contains(err.Error(), kv[0]) {
				t.Fatalf("%s set: %v, want a refusal naming it", kv[0], err)
			}
		})
	}
}

// The harness sees a TCP dial (the positive control), and the socket-only URL
// the rule accepts makes none.
func TestAcceptedURLNeverDialsTCP(t *testing.T) {
	clearLibpqEnv(t)
	srv := newFakePG(t)
	// The socket directory exists, as Load requires, and holds no server.
	control := "postgres:///db?host=" + t.TempDir() + "&port=" + srv.port() + "&user=authority_app&sslmode=disable"
	cfg, err := checkDBURL(control, "authority_app")
	if err != nil {
		t.Fatalf("control refused: %v", err)
	}
	// Dialled as Task 10 dials it: the checked Config, not the URL.
	conn, err := pq.NewConnectorConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ping(t, sql.OpenDB(conn))
	if srv.count() != 0 {
		t.Fatalf("the accepted URL dialled TCP %d time(s)", srv.count())
	}
	bypass := control + "&hostaddr=127.0.0.1"
	if _, err := checkDBURL(bypass, "authority_app"); err == nil {
		t.Error("the hostaddr bypass was accepted")
	}
	db, err := sql.Open("postgres", bypass)
	if err != nil {
		t.Fatal(err)
	}
	ping(t, db)
	if srv.count() == 0 {
		t.Fatal("positive control: lib/pq made no TCP dial for hostaddr, so the harness proves nothing")
	}
}

// fakePG is a loopback TCP listener that counts PostgreSQL startup attempts.
type fakePG struct {
	ln net.Listener
	n  atomic.Int32
}

func newFakePG(t *testing.T) *fakePG {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePG{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.n.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakePG) port() string { return strconv.Itoa(f.ln.Addr().(*net.TCPAddr).Port) }
func (f *fakePG) count() int32 { return f.n.Load() }

// ping makes one connection attempt and closes db.
func ping(t *testing.T, db *sql.DB) {
	t.Helper()
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = db.PingContext(ctx)
}

// Every variable lib/pq's Config reads from the environment is refused by
// Load, for every verb. A lib/pq upgrade that adds one fails here.
func TestLoadRefusesEveryVariableLibpqReads(t *testing.T) {
	clearLibpqEnv(t)
	typ := reflect.TypeFor[pq.Config]()
	for i := range typ.NumField() {
		name := typ.Field(i).Tag.Get("env")
		if name == "" || name == "-" {
			continue
		}
		for _, verb := range []Verb{VerbServe, VerbMigrate, VerbOwner} {
			env := validEnv(t)
			if verb == VerbServe {
				delete(env, "AUTHORITY_MIGRATE_DATABASE_URL")
			} else {
				delete(env, "AUTHORITY_DATABASE_URL")
			}
			env[name] = "x"
			if _, err := Load(verb, environOf(env)); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("verb %d %s: %v, want a refusal naming it", verb, name, err)
			}
		}
	}
}

// S4: a FIFO in place of a credential blocks a plain open(2) until a writer
// appears, before any file-type check runs, so serve would hang before READY=1.
func TestReadCredentialRefusesAFIFOPromptly(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a credentials directory is 0700
		t.Fatal(err)
	}
	p := filepath.Join(dir, "rsk")
	if err := syscall.Mkfifo(p, 0o400); err != nil {
		t.Fatal(err)
	}
	// A reader stuck in open(2) is released when a writer opens the FIFO.
	t.Cleanup(func() {
		if w, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil { // #nosec G304 -- the test's own FIFO
			_ = w.Close()
		}
	})
	done := make(chan error, 1)
	go func() { _, err := readCredential(dir, "rsk"); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular") {
			t.Fatalf("a FIFO: %v, want a regular-file refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readCredential hung on a FIFO")
	}
}

// C1 and §3.6: a loader error names the variable, never its value, and a
// credential path is CREDENTIALS_DIRECTORY's value.
func TestReadCredentialErrorsCarryNoPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a credentials directory is 0700
		t.Fatal(err)
	}
	writeCred(t, dir, "loose", []byte("key"), 0o440)
	if err := os.Symlink(filepath.Join(dir, "loose"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing", "loose", "link"} {
		_, err := readCredential(dir, name)
		if err == nil {
			t.Fatalf("%s: read", name)
		}
		if strings.Contains(err.Error(), dir) {
			t.Errorf("%s: the error carries the path: %v", name, err)
		}
	}
	if _, err := readCredential(filepath.Join(dir, "absent"), "rsk"); err == nil || strings.Contains(err.Error(), dir) {
		t.Errorf("a missing directory: %v, want a refusal without the path", err)
	}
}

// The cap holds a PKCS#8 P-384 key many times over.
func TestCredentialCapHoldsAP384Key(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("a P-384 PKCS#8 DER key is %d bytes; credMax is %d", len(der), credMax)
	if len(der)*8 > credMax {
		t.Fatalf("a P-384 PKCS#8 key is %d bytes; credMax %d leaves under 8x headroom", len(der), credMax)
	}
}

// B2-D16: off Linux every platform seam refuses, so serve has nothing to run
// on; on Linux they read the kernel (TestStatfsAndClock goes further).
func TestPlatformSeams(t *testing.T) {
	_, total, err := statfs(t.TempDir())
	synced, cerr := clockSynced()
	if runtime.GOOS == "linux" {
		if err != nil || total == 0 {
			t.Fatalf("statfs: %d bytes, %v", total, err)
		}
		return
	}
	if err == nil || synced || cerr == nil {
		t.Fatalf("off Linux: statfs err %v, clockSynced %v %v; want refusals and false", err, synced, cerr)
	}
}

// A disk-watch path that cannot be read keeps its cause, without the
// path, rather than reading as a missing directory whatever the cause.
func TestDiskWatchRefusalKeepsTheCause(t *testing.T) {
	dir := t.TempDir()
	var c Config
	missing := filepath.Join(dir, "absent")
	if err := setDiskWatch(&c, missing); err == nil || !strings.Contains(err.Error(), "no such file") ||
		strings.Contains(err.Error(), dir) {
		t.Fatalf("a missing path: %v, want its cause without the path", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setDiskWatch(&c, file); err == nil || !strings.Contains(err.Error(), "existing directory") {
		t.Fatalf("a file: %v, want a refusal", err)
	}
	if err := setDiskWatch(&c, dir); err != nil {
		t.Fatalf("control: a directory: %v", err)
	}
}
