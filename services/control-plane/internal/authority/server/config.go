// Package server is the account authority's serve (#3587 PR-B2, DoR §3.3):
// the environment loader, the listener, the HTTP layer, the loops and the
// startup sequence. serve makes no outbound connection.
package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"

	"github.com/lib/pq"
)

// Verb is the subcommand an environment is loaded for. keygen reads none.
type Verb int

// The verbs that read the environment.
const (
	VerbServe Verb = iota + 1
	VerbMigrate
)

type need int

const (
	unused need = iota
	required
	refused
)

// Config is serve's or migrate's environment (DoR §3.3 Config table).
type Config struct {
	ListenAddr    netip.AddrPort
	TrustPath     string
	AnchorsPath   string
	DiskWatchPath string
	// DatabaseURL is AUTHORITY_DATABASE_URL for serve and
	// AUTHORITY_MIGRATE_DATABASE_URL for migrate. It is never logged (§3.6).
	DatabaseURL string
	// DB is lib/pq's reading of DatabaseURL, as checkDBURL checked it. Dial
	// it with sql.OpenDB(pq.NewConnectorConfig(DB)), never by passing
	// DatabaseURL back to lib/pq: a second parse re-reads the environment and
	// drops the passfile sentinel (devNullChild).
	DB pq.Config
	// checked is set by setDB alone, after checkDBURL passed DB. OpenDB
	// refuses a Config without it: no other package can set it, so a DB built
	// by hand, or by pq.NewConfig from an unchecked URL, never dials (M3).
	checked        bool
	CredentialsDir string
}

type envRule struct {
	name           string
	serve, migrate need
	set            func(c *Config, v string) error
}

// envRules is the Config table, one row per variable (DoR:1641–1649). The
// variables lib/pq honours silently (B2-D14) are refused before the table, by
// envMap. PR-B3 adds the owner-CLI column.
var envRules = []envRule{
	{"AUTHORITY_LISTEN_ADDR", required, unused, setListen},
	{"AUTHORITY_TRUST_PATH", required, unused, func(c *Config, v string) error { return setAbs(&c.TrustPath, v) }},
	{"AUTHORITY_ANCHORS_PATH", required, unused, func(c *Config, v string) error { return setAbs(&c.AnchorsPath, v) }},
	{"AUTHORITY_DISK_WATCH_PATH", required, unused, setDiskWatch},
	{"AUTHORITY_DATABASE_URL", required, refused, func(c *Config, v string) error { return setDB(c, v, "authority_app") }},
	{"AUTHORITY_MIGRATE_DATABASE_URL", refused, required, func(c *Config, v string) error { return setDB(c, v, "authority_owner") }},
	{"CREDENTIALS_DIRECTORY", required, unused, func(c *Config, v string) error { return setAbs(&c.CredentialsDir, v) }},
}

// Load reads verb's environment from environ, in os.Environ's form; the
// caller passes os.Environ(). An unknown verb is refused. Every variable
// lib/pq reads is refused first, whatever its value, and checkDBURL names a
// passfile that cannot exist when the URL names none, so lib/pq's connection
// settings come from the URL and lib/pq's defaults alone (B2-D14, N1). A
// refusal names the variable, never its value, which can be a URL (§3.6).
func Load(verb Verb, environ []string) (Config, error) {
	if verb != VerbServe && verb != VerbMigrate {
		return Config{}, fmt.Errorf("unknown verb %d", verb)
	}
	env, err := envMap(environ)
	if err != nil {
		return Config{}, err
	}
	var c Config
	for _, r := range envRules {
		if err := r.apply(&c, verb, env); err != nil {
			return Config{}, err
		}
	}
	return c, nil
}

// apply is r's cell for verb. A refused variable is refused when set at all,
// even to the empty string, as a lib/pq variable is.
func (r envRule) apply(c *Config, verb Verb, env map[string]string) error {
	n := r.serve
	if verb == VerbMigrate {
		n = r.migrate
	}
	v, set := env[r.name]
	switch {
	case n == refused && set:
		return fmt.Errorf("%s must not be set for this verb", r.name)
	case n == required && v == "":
		return fmt.Errorf("%s is required", r.name)
	case n == required:
		if err := r.set(c, v); err != nil {
			return fmt.Errorf("%s: %w", r.name, err)
		}
	}
	return nil
}

// libpqEnv is every variable lib/pq v1.12.3 reads outside the PG prefix, plus
// the prefixed ones it names. pq.NewConfig reads the process environment
// through the env tags on pq.Config, and two of those tags,
// SSLPGMINPROTOCOLVERSION and SSLPGMAXPROTOCOLVERSION, do not start with
// PG, so the set is derived from the tags and follows a lib/pq upgrade. It
// also holds "set", the tag on an unexported field: lib/pq fails on that
// variable itself, and refusing it here names it. PQGO_DEBUG is read at
// package init and writes the connection settings and every protocol
// message to stderr, which is serve's log (§3.6).
var libpqEnv = func() map[string]bool {
	m := map[string]bool{"PQGO_DEBUG": true}
	t := reflect.TypeFor[pq.Config]()
	for i := range t.NumField() {
		if k := t.Field(i).Tag.Get("env"); k != "" && k != "-" {
			m[k] = true
		}
	}
	return m
}()

// libpqReads reports whether lib/pq reads the variable name. Every PG name is
// refused, not only the ones lib/pq reads today: they are libpq's namespace.
func libpqReads(name string) bool {
	return strings.HasPrefix(name, "PG") || libpqEnv[name]
}

// envMap is environ as a map. It refuses a variable lib/pq reads, set even to
// the empty string, since lib/pq counts that as set, and a variable named
// twice, whose value would depend on which reader looked.
func envMap(environ []string) (map[string]string, error) {
	env := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		switch {
		case !ok:
			continue
		case libpqReads(k):
			return nil, fmt.Errorf("%s must not be set: lib/pq would read it", k)
		}
		if _, dup := env[k]; dup {
			return nil, fmt.Errorf("%s is set twice", k)
		}
		env[k] = v
	}
	return env, nil
}

// setListen is DoR:1647's rule: an IP and port, no zone, unmapped, global
// unicast or loopback, port ≠ 0, bound in the unmapped form.
func setListen(c *Config, v string) error {
	ap, err := netip.ParseAddrPort(v)
	if err != nil {
		return errors.New("must be IP:port")
	}
	if ap.Addr().Zone() != "" {
		return errors.New("must carry no zone")
	}
	a := ap.Addr().Unmap()
	if (!a.IsGlobalUnicast() && !a.IsLoopback()) || ap.Port() == 0 {
		return errors.New("must be a unicast or loopback address with a port")
	}
	c.ListenAddr = netip.AddrPortFrom(a, ap.Port())
	return nil
}

func setAbs(dst *string, v string) error {
	if !filepath.IsAbs(v) || filepath.Clean(v) != v {
		return errors.New("must be a clean absolute path")
	}
	*dst = v
	return nil
}

// setDiskWatch: the mount holding pg_wal must exist (LD-25, DoR:1649).
func setDiskWatch(c *Config, v string) error {
	if err := setAbs(&c.DiskWatchPath, v); err != nil {
		return err
	}
	fi, err := os.Stat(v)
	if err != nil {
		return fmt.Errorf("must name an existing directory: %w", pathless(err))
	}
	if !fi.IsDir() {
		return errors.New("must name an existing directory")
	}
	return nil
}

func setDB(c *Config, v, role string) error {
	cfg, err := checkDBURL(v, role)
	if err != nil {
		return err
	}
	c.DatabaseURL, c.DB, c.checked = v, cfg, true
	return nil
}

// checkDBURL is DoR:1646's rule plus B2-D14 and N1, and returns the Config the
// caller dials. lib/pq does not read a URL as net/url does: it rewrites the
// URL into a key=value string, writing query keys unescaped, and it fills
// whatever the URL leaves unsaid from the environment and a service file. So
// the rule reads the URL both ways, and each reading is refused on its own:
//   - net/url's, by parseDBURL: the brief's rule, plus the query-key alphabet
//     and the database named once;
//   - lib/pq's, by checkLibpqView on pq.NewConfig's result: one absolute
//     socket directory, no hostaddr, password, service, options or run-time
//     parameter, the user role, and every field the URL names read back as
//     net/url read it.
//
// The process environment pq.NewConfig reads is checked just before it reads
// it. The socket directory must pass checkSocketDir. A passfile must be the
// euid's alone, or lib/pq skips it with a warning, in a directory only root
// or the euid can write; an unnamed one becomes devNullChild.
func checkDBURL(raw, role string) (pq.Config, error) {
	v, err := parseDBURL(raw, role)
	if err != nil {
		return pq.Config{}, err
	}
	if _, err := envMap(os.Environ()); err != nil {
		return pq.Config{}, err
	}
	cfg, err := pq.NewConfig(raw)
	if err != nil {
		// lib/pq's text can quote the URL's values (§3.6).
		return pq.Config{}, errors.New("lib/pq refuses the URL")
	}
	if err := checkLibpqView(cfg, v, role); err != nil {
		return pq.Config{}, err
	}
	if err := checkSocketDir(cfg.Host); err != nil {
		return pq.Config{}, err
	}
	if cfg.Passfile == "" {
		cfg.Passfile = devNullChild
	} else if err := checkPassfile(cfg.Passfile); err != nil {
		return pq.Config{}, err
	}
	return cfg, nil
}

// devNullChild is the passfile a checked Config names when the URL names
// none: a child of /dev/null. Unnamed, lib/pq reads $HOME/.pgpass on every
// connection. A path under /dev/null cannot exist (ENOTDIR), so lib/pq's
// stat fails and it uses no passfile, silently: it warns only about a file
// it found (internal/pqutil/path.go:78–81).
const devNullChild = "/dev/null/pgpass"

// dbURLView is net/url's reading of the fields lib/pq must read identically.
type dbURLView struct{ host, user, database, passfile string }

// parseDBURL is the URL rule on net/url's reading. lib/pq takes a URL only
// under these exact, case-sensitive prefixes and parses anything else,
// "POSTGRES://" included, as a key=value string.
func parseDBURL(raw, role string) (dbURLView, error) {
	if !strings.HasPrefix(raw, "postgres://") && !strings.HasPrefix(raw, "postgresql://") {
		return dbURLView{}, errors.New("must be a postgres:// URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return dbURLView{}, errors.New("must be a postgres:// URL")
	}
	// A TCP host is refused (DoR:1646). lib/pq's socket host is the host
	// query key, because a percent-encoded socket in the authority fails.
	if u.Host != "" {
		return dbURLView{}, errors.New("must use a socket host; TCP is refused")
	}
	if _, set := u.User.Password(); set {
		return dbURLView{}, errors.New("must carry no password")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return dbURLView{}, errors.New("the query does not parse")
	}
	if err := checkQueryKeys(q); err != nil {
		return dbURLView{}, err
	}
	return viewOf(u, q, role)
}

// urlKeys are the query keys the URL may carry. Every other key is refused
// at load: password, service, hostaddr and options; lib/pq's other options,
// such as sslrootcert or binary_parameters; and unknown keys, which lib/pq
// would send as run-time parameters.
var urlKeys = map[string]bool{
	"host": true, "port": true, "user": true, "dbname": true, "passfile": true,
	"connect_timeout": true, "application_name": true, "sslmode": true,
}

// checkQueryKeys refuses a repeated key, because lib/pq takes the first; a
// key outside lib/pq's own key alphabet, [a-z_], because lib/pq writes keys
// into its key=value string unescaped, so a key holding '=', a quote, a
// backslash or whitespace can carry whole pairs (N1); and any key not in
// urlKeys. A refusal does not quote the key: it is part of the URL.
func checkQueryKeys(q url.Values) error {
	for k, vs := range q {
		switch {
		case len(vs) != 1:
			return errors.New("a query key repeats")
		case !isKeyName(k):
			return errors.New("a query key holds a character outside [a-z_]")
		case !urlKeys[k]:
			return errors.New("a query key is not one the URL may carry")
		}
	}
	return nil
}

// isKeyName reports whether k is non-empty and spelled from lib/pq's key
// alphabet, lower-case ASCII letters and '_'.
func isKeyName(k string) bool {
	return k != "" && !strings.ContainsFunc(k, func(r rune) bool { return (r < 'a' || r > 'z') && r != '_' })
}

// viewOf reads the host, user, database and passfile the URL names.
func viewOf(u *url.URL, q url.Values, role string) (dbURLView, error) {
	v := dbURLView{host: q.Get("host"), user: q.Get("user"), passfile: q.Get("passfile")}
	if !filepath.IsAbs(v.host) || strings.Contains(v.host, ",") {
		return dbURLView{}, errors.New("host must be one absolute socket directory")
	}
	if name := u.User.Username(); name != "" {
		if v.user != "" {
			return dbURLView{}, errors.New("the user is named twice")
		}
		v.user = name
	}
	if v.user != role {
		return dbURLView{}, fmt.Errorf("the user must be %s", role)
	}
	var err error
	if v.database, err = urlDatabase(u, q); err != nil {
		return dbURLView{}, err
	}
	return v, nil
}

// urlDatabase is the database the URL names, exactly once. lib/pq reads the
// path, a dbname key and a database key, and which wins depends on how its
// key=value string sorts, so the database is named once, as the user is.
// Unnamed, the server would choose it.
func urlDatabase(u *url.URL, q url.Values) (string, error) {
	var db string
	named := 0
	for _, d := range []string{strings.TrimPrefix(u.Path, "/"), q.Get("dbname"), q.Get("database")} {
		if d != "" {
			db, named = d, named+1
		}
	}
	if named != 1 {
		return "", errors.New("must name exactly one database")
	}
	return db, nil
}

// checkLibpqView is the URL rule on lib/pq's reading, cfg, of the URL net/url
// read as v. lib/pq dials TCP when Hostaddr is valid or Host is not an
// absolute path, and tries each Multi host in turn.
func checkLibpqView(cfg pq.Config, v dbURLView, role string) error {
	switch {
	case cfg.Hostaddr.IsValid(), len(cfg.Multi) > 0, !filepath.IsAbs(cfg.Host), strings.Contains(cfg.Host, ","):
		return errors.New("lib/pq would not dial exactly one socket directory")
	case cfg.Password != "":
		return errors.New("must carry no password")
	case cfg.Service != "":
		return errors.New("must name no service")
	case cfg.Options != "":
		return errors.New("must carry no options")
	case len(cfg.Runtime) > 0:
		return errors.New("must carry no run-time parameter")
	case cfg.User != role:
		return fmt.Errorf("the user must be %s", role)
	case cfg.Host != v.host, cfg.User != v.user, cfg.Database != v.database, cfg.Passfile != v.passfile:
		return errors.New("lib/pq reads the URL differently from net/url")
	}
	return nil
}

// checkSocketDir is the socket directory's rule (Sec-I1), applied at Load to
// the one directory lib/pq dials. Peer authentication sends no secret, so
// whatever listens at <dir>/.s.PGSQL.<port> is the authority's database: it
// would pass the role check, answer revoked_keys and choose the heads the RSK
// signs. So a writer that could bind there is refused as the credentials'
// parents refuse one: the directory must exist, must be a directory and not
// a symlink, and must be closed to other's write, which also refuses /tmp
// (1777). Group write is allowed, and so is any owner, because the database
// owns the directory: Debian's /var/run/postgresql is 2775, postgres:postgres.
// The path must be clean, since Lstat follows a symlinked directory named
// with a trailing slash or a final "." (lib/pq cleans the socket path, so it
// would dial through one). Then the path the kernel dials through is judged
// twice: as it resolves (checkSocketAncestors, on filepath.EvalSymlinks'
// reading), and as it is looked up, link by link (walkSocketPath). The two
// readings must lead to the same directory, or the walk judged another path
// than the one dialled. Only at Load: a check of the peer on every dial
// (SO_PEERCRED) is a #3593 residual. Errors carry no path.
func checkSocketDir(dir string) error {
	if filepath.Clean(dir) != dir {
		return errors.New("the socket directory must be a clean path")
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the socket directory: %w", pathless(err))
	}
	if err := socketDirRule(fi.Mode()); err != nil {
		return fmt.Errorf("the socket directory is %w", err)
	}
	euid, err := geteuid()
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("the socket directory: %w", pathless(err))
	}
	if err := checkSocketAncestors(resolved, euid); err != nil {
		return err
	}
	walked, err := walkSocketPath(dir, euid)
	if err != nil {
		return err
	}
	if walked != resolved {
		return errSocketPathMoved
	}
	return nil
}

// errSocketPathMoved is a socket path whose lookup and resolution disagree.
var errSocketPathMoved = errors.New("the socket directory's path changed while it was judged")

// ancestorOwner is the uid the socket directory's walks judge for the
// directory or link at path. It is a test seam, as swapHook is: an ancestor
// owned by another user cannot be made without root.
var ancestorOwner = func(_ string, fi fs.FileInfo) uint32 { return ownerOf(fi) }

// maxSocketLinks bounds the links walkSocketPath follows, as Linux's
// MAXSYMLINKS (40) bounds one lookup.
const maxSocketLinks = 40

// walkSocketPath looks dir up as the kernel does, one component at a time
// from "/", following each link, and returns where it leads. A link owned by
// another user can be removed and made again by that user, even in a sticky
// directory such as /tmp, and so re-point the dial (/tmp/<their link>/pg).
// So every link followed must be owned by root or the euid, and every
// directory a name is looked up in, a link's included even when the link
// leads off the resolved path, must pass ancestorRule as a directory above
// the parent. The parent's stricter rule is checkSocketAncestors'. A ".."
// moves to the physical parent, which needs no check of its own: it was
// looked up in on the way down. Debian's /var/run, a root-owned link to /run
// in root's /var, passes.
func walkSocketPath(dir string, euid uint32) (string, error) {
	cur, rest, links := "/", pathParts(dir), 0
	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]
		if c == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		if err := lookupDirRule(cur, euid); err != nil {
			return "", err
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(next)
		if err != nil {
			return "", fmt.Errorf("the socket directory's path: %w", pathless(err))
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		target, err := followLink(next, fi, euid, &links)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			cur = "/"
		}
		rest = append(pathParts(target), rest...)
	}
	return cur, nil
}

// pathParts is p's components, without the empty and "." ones.
func pathParts(p string) []string {
	return slices.DeleteFunc(strings.Split(p, "/"), func(c string) bool { return c == "" || c == "." })
}

// lookupDirRule judges a directory walkSocketPath looks a name up in. cur is
// a resolved path, so Lstat reads the directory itself.
func lookupDirRule(cur string, euid uint32) error {
	di, err := os.Lstat(cur)
	if err != nil {
		return fmt.Errorf("an ancestor of the socket directory: %w", pathless(err))
	}
	if err := ancestorRule(di.Mode(), ancestorOwner(cur, di), euid, false); err != nil {
		return fmt.Errorf("an ancestor of the socket directory is %w", err)
	}
	return nil
}

// followLink is one link on walkSocketPath's way: counted, owned by root or
// the euid, and read.
func followLink(link string, fi fs.FileInfo, euid uint32, links *int) (string, error) {
	if *links++; *links > maxSocketLinks {
		return "", errors.New("the socket directory's path holds too many links")
	}
	if uid := ancestorOwner(link, fi); uid != 0 && uid != euid {
		return "", errors.New("the socket directory's path holds a link not owned by root or the euid")
	}
	target, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("the socket directory's path: %w", pathless(err))
	}
	return target, nil
}

// checkSocketAncestors walks the resolved socket directory's ancestors, its
// parent first, up to "/". An ancestor another user can write, or own and so
// rename, lets that user put a directory of its own in the socket
// directory's place, or create it while it is absent (/tmp/<name> after a
// reboot), and pass every check above. So each ancestor must be a directory
// owned by root or the euid, and closed to group and other write: the parent
// strictly, sticky or not, and a higher one unless its sticky bit is set, so
// /tmp (1777) above a directory the euid owns passes, while
// /tmp/<another user's>/<dir> does not. A database-owned ancestor
// (/srv/pg/<dir>) is refused by design: the database may own the socket
// directory itself, never a directory above it.
func checkSocketAncestors(resolved string, euid uint32) error {
	parent := true
	for p := filepath.Dir(resolved); ; p = filepath.Dir(p) {
		ai, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("an ancestor of the socket directory: %w", pathless(err))
		}
		if err := ancestorRule(ai.Mode(), ancestorOwner(p, ai), euid, parent); err != nil {
			if parent {
				return fmt.Errorf("the socket directory's parent is %w", err)
			}
			return fmt.Errorf("an ancestor of the socket directory is %w", err)
		}
		if p == filepath.Dir(p) {
			return nil
		}
		parent = false
	}
}

// ancestorRule is the rule for one directory above the socket directory. A
// symlink is not a directory here: both walks judge resolved directories.
// Group write is refused as other write is (the socket directory alone may
// be group-writable): a member of the group can rename what the directory
// holds, unless the sticky bit stops it on a directory above the parent.
func ancestorRule(mode fs.FileMode, uid, euid uint32, parent bool) error {
	switch {
	case !mode.IsDir():
		return errors.New("not a directory")
	case uid != 0 && uid != euid:
		return errors.New("not owned by root or the euid")
	case mode.Perm()&0o022 == 0:
		return nil
	case parent:
		return errors.New("writable by group or other")
	case mode&fs.ModeSticky == 0:
		return errors.New("writable by group or other without the sticky bit")
	}
	return nil
}

// socketDirRule is checkSocketDir's mode half: a directory, not a symlink,
// with no other-write bit.
func socketDirRule(mode fs.FileMode) error {
	switch {
	case mode&fs.ModeSymlink != 0:
		return errors.New("a symlink")
	case !mode.IsDir():
		return errors.New("not a directory")
	case mode.Perm()&0o002 != 0:
		return errors.New("writable by other")
	}
	return nil
}

// checkPassfile is the passfile's rule at Load (Sec-M1): a clean absolute
// path, opened as the credentials are, by openJudged, so its immediate parent
// passes dirRule as opened and no writer but root or the euid can swap the
// file; the open file passes fileRule (not a symlink) and the ACL rule,
// aclOnlyOwner, on its descriptor (a named entry the mode bits do
// not show could let another user read the password). The path must be clean
// so that the parent judged is the one the kernel resolves. Off Linux the ACL
// cannot be read, so a named passfile is refused there, as a credential is
// (B2-D16). lib/pq still re-reads the file by path on every connection, after
// this check (a #3593 residual).
func checkPassfile(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return errors.New("the passfile must be a clean absolute path")
	}
	euid, err := geteuid()
	if err != nil {
		return err
	}
	f, fi, err := openJudged(filepath.Dir(p), filepath.Base(p), euid)
	if err != nil {
		return fmt.Errorf("the passfile: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := fileRule(fi.Mode(), ownerOf(fi), euid, false); err != nil {
		return fmt.Errorf("the passfile is %w", err)
	}
	if err := aclOnlyOwner(f, euid); err != nil {
		return fmt.Errorf("the passfile: %w", err)
	}
	return nil
}

// fileRule is F2's file half (DoR:1649): a regular file owned by euid with no
// group or other bit, and, for a credential, no write bit.
func fileRule(mode fs.FileMode, uid, euid uint32, noWrite bool) error {
	switch {
	case !mode.IsRegular():
		return errors.New("not a regular file")
	case uid != euid:
		return errors.New("not owned by the euid")
	case mode.Perm()&0o077 != 0:
		return errors.New("open to group or other")
	case noWrite && mode.Perm()&0o222 != 0:
		return errors.New("writable")
	}
	return nil
}

// dirRule is F2's parent half: only the immediate parent, owned by root or
// euid, with no group or other write bit. systemd's credential directory,
// compose's /run/secrets (0755, root) and a t.TempDir() all pass.
func dirRule(mode fs.FileMode, uid, euid uint32) error {
	switch {
	case !mode.IsDir():
		return errors.New("not a directory")
	case uid != 0 && uid != euid:
		return errors.New("not owned by root or the euid")
	case mode.Perm()&0o022 != 0:
		return errors.New("writable by group or other")
	}
	return nil
}

// geteuid is the effective uid as the kernel's uid_t. A value outside it,
// which no Unix kernel reports, is an error, never an owner a rule compares.
func geteuid() (uint32, error) {
	id := os.Geteuid()
	if id < 0 || id > math.MaxUint32 {
		return 0, errors.New("the euid is outside uid_t")
	}
	return uint32(id), nil
}

// ownerOf is the file's uid; an OS that reports none yields an owner no rule
// accepts.
func ownerOf(fi fs.FileInfo) uint32 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Uid
	}
	return math.MaxUint32
}

// The credentials serve reads from $CREDENTIALS_DIRECTORY (F2, DoR:1649).
// credMax caps a read: a P-384 PKCS#8 DER key is 185 bytes, and the cap
// leaves room for any encoding of one without reading an unbounded file.
const (
	credRSK = "rsk"
	credTLS = "tls-key"
	credMax = 4096
)

// swapHook is a test seam between a check and the open it guards.
var swapHook = func(string) {
	// Production does nothing between the check and the open; tests swap the path here.
}

var (
	errDirSwapped  = errors.New("the directory changed between its check and its open")
	errFileSwapped = errors.New("the file changed between its check and its open")
)

// openJudged opens dir/name read-only for a reader that judges the open file
// (F2, S3) and returns it with its fstat; the caller applies the file rule.
// The parent must pass dirRule as Lstat sees it, so a symlinked parent is
// refused. It is then opened as an os.Root that must be the same directory,
// judged again on its own stat, so a parent swapped between the check and
// the open is refused rather than read through (M4). name must not be a
// symlink, and the open file must be the one Lstat saw: os.Root follows an
// in-root symlink that O_NOFOLLOW alone would refuse. The open cannot block
// (O_NONBLOCK): a plain open(2) of a FIFO waits for a writer before any
// check runs, and serve would hang before READY=1 (S4). O_NOCTTY keeps a
// terminal from becoming serve's controlling one. Errors carry no path.
func openJudged(dir, name string, euid uint32) (*os.File, fs.FileInfo, error) {
	root, err := openDir(dir, euid)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }() // a file opened through root outlives it
	li, err := root.Lstat(name)
	if err != nil {
		return nil, nil, pathless(err)
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, errors.New("a symlink")
	}
	swapHook("file")
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, pathless(err)
	}
	fi, err := f.Stat()
	if err == nil && !os.SameFile(li, fi) {
		err = errFileSwapped
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, pathless(err)
	}
	return f, fi, nil
}

// openDir is openJudged's parent half: Lstat and dirRule, then the open,
// which must be the directory Lstat saw and pass dirRule on its own stat.
func openDir(dir string, euid uint32) (*os.Root, error) {
	di, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("its directory: %w", pathless(err))
	}
	if err := dirRule(di.Mode(), ownerOf(di), euid); err != nil {
		return nil, fmt.Errorf("its directory is %w", err)
	}
	swapHook("dir")
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("its directory: %w", pathless(err))
	}
	ri, err := root.Stat(".")
	switch {
	case err != nil:
		err = fmt.Errorf("its directory: %w", pathless(err))
	case !os.SameFile(di, ri):
		err = errDirSwapped
	default:
		if rerr := dirRule(ri.Mode(), ownerOf(ri), euid); rerr != nil {
			err = fmt.Errorf("its directory is %w", rerr)
		}
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// readCredential reads dir/name under F2, opened by openJudged. The checks
// judge the open file, and the read stops at credMax. Errors carry no path,
// which is CREDENTIALS_DIRECTORY's value (§3.6).
func readCredential(dir, name string) ([]byte, error) {
	euid, err := geteuid()
	if err != nil {
		return nil, err
	}
	f, fi, err := openJudged(dir, name, euid)
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	if err := fileRule(fi.Mode(), ownerOf(fi), euid, true); err != nil {
		return nil, fmt.Errorf("credential %s: %w", name, err)
	}
	if err := aclOnlyOwner(f, euid); err != nil {
		return nil, fmt.Errorf("credential %s: %w", name, err)
	}
	b, err := io.ReadAll(io.LimitReader(f, credMax+1))
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", name, pathless(err))
	}
	if len(b) > credMax {
		return nil, fmt.Errorf("credential %s is larger than %d bytes", name, credMax)
	}
	return b, nil
}

// pathless reduces an *fs.PathError to its Op and Err. A loader error names
// the variable, never its value (C1, §3.6), and a path is a value.
func pathless(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}

// parseKey reads an unencrypted PKCS#8 DER ECDSA P-384 key (B2-D11).
func parseKey(der []byte) (*ecdsa.PrivateKey, error) {
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("not a PKCS#8 DER key")
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok || ek.Curve != elliptic.P384() {
		return nil, errors.New("not an ECDSA P-384 key")
	}
	return ek, nil
}

// The POSIX ACL xattr layout (linux/posix_acl_xattr.h): a u32 version, then
// entries of u16 tag, u16 perm, u32 id, little-endian.
const (
	aclXattr       = "system.posix_acl_access"
	aclVersion     = 2
	aclTagUser     = 0x02
	aclTagGroupObj = 0x04
	aclTagGroup    = 0x08
)

// checkACL is F2's ACL rule: no named user but euid, no named group, and not
// the owning group (ACL_GROUP_OBJ) holds any permission. The mask is ignored,
// because a later chmod can widen it: under an extended ACL the mode's group
// bits are the mask, so group::r-- with mask::--- reads as no group access
// and fileRule passes it, and a later chmod g+r grants the owning group the
// read (CWE-732).
func checkACL(b []byte, euid uint32) error {
	if len(b) < 4 || (len(b)-4)%8 != 0 || binary.LittleEndian.Uint32(b) != aclVersion {
		return errors.New("the ACL does not parse")
	}
	for e := b[4:]; len(e) > 0; e = e[8:] {
		tag, perm, id := binary.LittleEndian.Uint16(e), binary.LittleEndian.Uint16(e[2:]), binary.LittleEndian.Uint32(e[4:])
		if perm != 0 && (tag == aclTagGroup || tag == aclTagGroupObj || tag == aclTagUser && id != euid) {
			return errors.New("an ACL entry grants another principal")
		}
	}
	return nil
}
