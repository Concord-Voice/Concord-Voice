// Package redistest gives every Go test process a private Redis logical
// database, so that one process's FLUSHDB can never reach another's keys.
//
// Why this exists (#2680): the previous helper pinned DB 1 whenever REDIS_URL
// was unset and ran FLUSHDB SYNC on every setup call. Because `go test`
// compiles one binary per package and runs those binaries concurrently, a
// package that flushed (internal/presence) erased the live fixtures of a
// package that did not (internal/rbac) mid-test. The same collision happened
// across two worktrees. Measured before the fix: internal/rbac passed 3/3 alone
// and failed 3/3 beside its siblings, with zero data races.
//
// One OS process gets one index, so per-process is simultaneously per-package
// (within a run) and per-session (across worktrees). One mechanism, both bugs.
//
// This package is a LEAF by requirement, not by preference: internal/testhelpers
// imports internal/rbac, so internal/rbac cannot import internal/testhelpers.
// Anything imported from internal/ here would re-create that cycle for some
// caller. stdlib + go-redis + testing only.
package redistest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
)

const (
	// seqKey lives in DB 0, which is never allocated to a test process. A
	// hand-run `redis-cli -n 0 FLUSHDB` resets it and the next allocation
	// returns index 1, which may be live — that is the only reset vector and
	// it is documented in [internal]rules/tests.md.
	seqKey = "concord:testredis:seq"

	// fallbackPoolSize is databases-1 for a stock Redis (16). Used only when
	// CONFIG GET databases is refused or unparseable. A wrong pool size costs
	// a suboptimal spread, never a wrong-DB write: allocation re-PINGs on the
	// resolved index and fails there if the pool was too large.
	fallbackPoolSize = 15

	// dbOverrideEnv pins an index and skips allocation entirely (no INCR).
	// For redistest's own tests and for a developer deliberately pinning
	// during a debugging session.
	dbOverrideEnv = "CONCORD_TEST_REDIS_DB"
)

// Assembled from parts, matching internal/testhelpers/testdb/testdb.go: a
// literal URL containing the password trips detect-secrets and SonarQube
// S6698/S2068. This is the docker-compose dev default, not a production
// credential — it reaches only a local container.
var devRedisVal = "concord_dev_redis" //nolint:gosec // pragma: allowlist secret

var defaultDevRedisURL = "redis://:" + devRedisVal + "@localhost:6379" //nolint:gosec // dev-only default

var (
	allocOnce sync.Once
	allocURL  string
	allocDB   int
	allocAddr string
	allocErr  error
)

// runAllocate is the ONLY closure passed to allocOnce. Every reader of the
// alloc* globals goes through it, which is what establishes the happens-before
// edge sync.Once provides — a reader that called allocOnce.Do(func(){}) instead
// would get the edge but permanently mark the Once done, so allocation could
// never run at all.
func runAllocate() {
	allocURL, allocDB, allocErr = allocate()
	if allocErr != nil {
		return
	}
	// Addr is derived here rather than returned from allocate() so the function's
	// signature (and its direct callers in the tests) stay put. It is what lets
	// resetGuard refuse a flush aimed at a DIFFERENT SERVER on a matching index.
	o, parseErr := redis.ParseURL(allocURL)
	if parseErr != nil {
		// An initialization that cannot finish must fail the initialization.
		// Dropping this left allocErr nil with allocAddr "", so every later Reset
		// refused with "this process allocated its database on ." — naming an
		// empty server, which describes nothing. Fail here, where the cause is known.
		allocErr = errURLWithheld
		return
	}
	allocAddr = o.Addr
}

// resolveIndex maps a monotonic ticket onto 1..poolSize.
//
// Monotonic INCR rather than pid-mod-N: any poolSize consecutive tickets map to
// poolSize distinct indices, so a collision needs more than poolSize
// SIMULTANEOUSLY LIVE processes. pid-mod-N collides by birthday at two.
func resolveIndex(ticket int64, poolSize int) (int, error) {
	if poolSize < 1 {
		return 0, errors.New(
			"redistest: redis reports databases=1, so SELECT is unavailable — " +
				"this is Redis cluster mode, where per-process logical-DB isolation cannot work")
	}
	idx := 1 + int((ticket-1)%int64(poolSize))
	if idx < 1 {
		// Reachable, despite how it reads: ticket >= 1 is an assumption about INCR,
		// not a guarantee. The counter lives in DB 0 and this package's own header
		// notes it is hand-mutable — INCR on a key SET to a negative value returns a
		// negative ticket, and Go's % keeps the sign, so idx goes negative too.
		// Print what was actually computed; the old text said "index 0" regardless
		// and sent the reader hunting for an off-by-one in the `1 +`.
		return 0, fmt.Errorf(
			"redistest: computed out-of-range index %d from ticket %d, pool %d — "+
				"the %s counter in DB 0 may have been set to a negative value",
			idx, ticket, poolSize, seqKey)
	}
	return idx, nil
}

// poolSizeFrom converts a CONFIG GET databases reply into a usable pool size.
// This is the ONLY fail-open path in the package.
func poolSizeFrom(cfg map[string]string, err error) int {
	if err != nil {
		return fallbackPoolSize
	}
	raw, ok := cfg["databases"]
	if !ok {
		return fallbackPoolSize
	}
	n, convErr := strconv.Atoi(raw)
	if convErr != nil || n < 1 {
		return fallbackPoolSize
	}
	// n-1 because index 0 is reserved. n == 1 yields 0, which resolveIndex
	// converts into the named cluster-mode error.
	return n - 1
}

// errURLWithheld is returned instead of anything derived from the URL.
// The URL carries the dev password ([internal]rules/observability.md #1).
var errURLWithheld = errors.New(
	"redistest: the base Redis URL is not a valid redis:// URL " +
		"(value withheld — it carries a password); check REDIS_URL")

// rewriteURL replaces the URL's database selector with db.
func rewriteURL(base string, db int) (string, error) {
	// ParseURL gates validity: it rejects malformed URLs and treats two or more
	// path segments as an error. Validate through it, then edit through net/url
	// so the rest of the URL survives byte-for-byte.
	if _, err := redis.ParseURL(base); err != nil {
		return "", errURLWithheld
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", errURLWithheld
	}

	// ParseURL does NOT reject every non-redis scheme — it also accepts unix://,
	// where u.Path is the SOCKET ADDRESS, not a database selector. Rewriting the
	// path there overwrites /var/run/redis.sock with /5, the dial fails, and the
	// probe below reports "index not selectable (pool size may exceed databases)"
	// — a diagnosis that names the wrong cause entirely. Reject explicitly. The
	// scheme itself is not secret; the rest of the URL is withheld.
	if u.Scheme != "redis" && u.Scheme != "rediss" {
		return "", fmt.Errorf(
			"redistest: unsupported Redis URL scheme %q — only redis:// and rediss:// carry a "+
				"per-process database index in the URL path (rest of value withheld — it carries "+
				"a password)", u.Scheme)
	}

	u.Path = "/" + strconv.Itoa(db)

	// go-redis applies a `db` QUERY parameter AFTER parsing the path, and the
	// query wins — verified: redis://h:6379/3?db=0 resolves to DB 0. Leaving one
	// in place silently defeats this whole rewrite, so strip exactly that key and
	// leave every other parameter (dial_timeout, protocol, …) untouched.
	if q := u.Query(); q.Has("db") {
		q.Del("db")
		u.RawQuery = q.Encode()
	}

	out := u.String()

	// Assert the postcondition instead of enumerating URL shapes. This function
	// has exactly one contract — the URL it returns selects db — and until now
	// nothing checked it.
	//
	// The shape that broke it: an OPAQUE URL. `redis:6379`, with no "//", parses
	// with the authority in u.Opaque and an EMPTY u.Path, and URL.String() emits
	// Opaque INSTEAD of Path — so the assignment above is silently discarded and
	// ParseURL resolves the result to DB 0. `redis:6379` is a natural typo when
	// the compose service is literally named `redis`, and the consequence was the
	// whole of #2680 restored invisibly: allocate() reports success with index N,
	// its probe PINGs the URL and passes (having selected DB 0, not N), and every
	// client in the process then reads and writes DB 0 — the dev app's live data
	// and this package's own ticket counter. resetGuard still refuses the flush,
	// but six packages call Client without ever calling Reset, so for those
	// nothing fires at all.
	//
	// Checking the outcome rather than the input also covers the next shape
	// nobody predicted, and any future change to URL.String or ParseURL.
	check, parseErr := redis.ParseURL(out)
	if parseErr != nil {
		return "", errURLWithheld
	}
	if check.DB != db {
		return "", fmt.Errorf(
			"redistest: could not place database index %d in the Redis URL — it still resolves to %d. "+
				"REDIS_URL must be of the form redis://[:password@]host:port, WITH the \"//\" "+
				"(rest of value withheld — it carries a password)", db, check.DB)
	}

	return out, nil
}

func baseURL() string {
	if fromEnv := os.Getenv("REDIS_URL"); fromEnv != "" {
		return fromEnv
	}
	return defaultDevRedisURL
}

// allocate runs at most once per process.
func allocate() (string, int, error) {
	base := baseURL()

	if pin := os.Getenv(dbOverrideEnv); pin != "" {
		n, err := strconv.Atoi(pin)
		if err != nil || n < 1 {
			return "", 0, fmt.Errorf(
				"redistest: %s must be a positive integer (0 is reserved), got %q", dbOverrideEnv, pin)
		}
		u, err := rewriteURL(base, n)
		if err != nil {
			return "", 0, err
		}
		return u, n, nil
	}

	// Step 1: dial DB 0 to read the pool size and take a ticket.
	zeroURL, err := rewriteURL(base, 0)
	if err != nil {
		return "", 0, err
	}
	zeroOpts, err := redis.ParseURL(zeroURL)
	if err != nil {
		return "", 0, errURLWithheld
	}
	coordinator := redis.NewClient(zeroOpts)
	defer func() { _ = coordinator.Close() }()

	ctx := context.Background()
	if pingErr := coordinator.Ping(ctx).Err(); pingErr != nil {
		return "", 0, errors.New(pingFailureMessage(pingErr))
	}

	poolSize := poolSizeFrom(coordinator.ConfigGet(ctx, "databases").Result())

	ticket, incrErr := coordinator.Incr(ctx, seqKey).Result()
	if incrErr != nil {
		// Deliberately NOT falling back to a fixed index. A fixed fallback is
		// exactly the fail-open that produced #2680.
		return "", 0, fmt.Errorf("redistest: failed to allocate a test Redis DB index: %w", incrErr)
	}

	idx, err := resolveIndex(ticket, poolSize)
	if err != nil {
		return "", 0, err
	}

	// NO WRAP NOTICE — deliberately. An earlier revision warned when
	// ticket > poolSize, on the theory that wrap is collision's precondition and
	// the only thing knowable for free. It is not knowable for free, because the
	// ticket counts CUMULATIVE allocations, not concurrent ones: seqKey has no
	// TTL and rides an appendonly volume, and one `go test ./...` allocates ~60
	// indices, so ticket > poolSize becomes permanently true after the first
	// ordinary run and the notice then fires on every allocation forever,
	// single-process included. Measured at ticket 171 against pool 15.
	//
	// That is precisely the property this design rejected the SETNX lease for
	// ("a detector whose steady state is false is worse than none in a
	// misdiagnosis-cost issue"), so applying the same standard removes it.
	// Detecting real collision needs concurrent LIVENESS, which needs a
	// registration this package deliberately does not keep — see the ceiling
	// documented in [internal]rules/tests.md § Test isolation.

	// Step 2: prove the index is actually selectable. Converts an out-of-range
	// SELECT (fallback pool larger than the server's real `databases`) into a
	// named error here rather than at a random call site later.
	allocatedURL, err := rewriteURL(base, idx)
	if err != nil {
		return "", 0, err
	}
	allocatedOpts, err := redis.ParseURL(allocatedURL)
	if err != nil {
		return "", 0, errURLWithheld
	}
	probe := redis.NewClient(allocatedOpts)
	defer func() { _ = probe.Close() }()
	if pingErr := probe.Ping(ctx).Err(); pingErr != nil {
		return "", 0, fmt.Errorf(
			"redistest: allocated DB index %d is not selectable (pool size %d may exceed the server's "+
				"`databases` setting): %w", idx, poolSize, pingErr)
	}

	return allocatedURL, idx, nil
}

func ensureAllocated(t testing.TB) {
	t.Helper()
	allocOnce.Do(runAllocate)
	if allocErr != nil {
		t.Fatalf("%v", allocErr)
	}
}

// URL returns the base Redis URL with this process's allocated DB index.
func URL(t testing.TB) string {
	t.Helper()
	ensureAllocated(t)
	return allocURL
}

// DB returns this process's allocated Redis logical database index (never 0).
func DB(t testing.TB) int {
	t.Helper()
	ensureAllocated(t)
	return allocDB
}

// Options returns fresh options with DB already set, for the case where a caller
// needs to mutate options before dialling. Prefer Client unless you actually do.
//
// Mutating Addr moves the client to a DIFFERENT SERVER; Reset refuses to flush
// in that case rather than trusting the index alone. (An earlier revision of this
// comment named internal/media and internal/keyrotation as callers that mutate —
// both use Client, and neither ever mutated. The claim was inherited from the
// spec rather than read off the call sites.)
func Options(t testing.TB) *redis.Options {
	t.Helper()
	ensureAllocated(t)
	opts, err := redis.ParseURL(allocURL)
	if err != nil {
		t.Fatalf("%v", errURLWithheld)
	}
	return opts
}

// Client dials the allocated database, pings, and registers cleanup.
//
// Fidelity gap, knowingly retained (#2680): this does NOT set
// ContextTimeoutEnabled, which internal/database/redis.go:18 sets on every
// production client. Changing client timeout semantics inside an isolation
// change would move an unrelated variable across ~140 call sites. Recorded in
// [internal]rules/tests.md.
func Client(t testing.TB) *redis.Client {
	t.Helper()
	c := redis.NewClient(Options(t))
	if err := c.Ping(context.Background()).Err(); err != nil {
		_ = c.Close()
		t.Fatal(pingFailureMessage(err))
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Reset flushes the caller's database after proving it is this process's own.
//
// This is the criterion-4 regression lock: a FLUSHDB that could reach a shared
// database cannot pass. It replaces every unscoped FlushDB in the test tree.
func Reset(ctx context.Context, c *redis.Client) error {
	if c == nil {
		return errors.New("redistest: Reset called with a nil client")
	}
	// Read the alloc* globals only through the Once. A caller on a goroutine that
	// never touched Client/URL/DB/Options otherwise reads them with no
	// happens-before edge — a data race under `go test -race`, which this repo
	// mandates — and would see the zero value.
	allocOnce.Do(runAllocate)
	if allocErr != nil {
		// Surface the real cause. Because the line above FORCES allocation, an
		// unallocated process at this point means allocation FAILED — not that the
		// caller forgot to call Client first, which is what resetGuard's message
		// says. Without this, a developer whose Redis is down or mis-credentialled
		// is told to add a Client call, does so, and only then sees the auth error.
		return fmt.Errorf("redistest: refusing to flush — this process could not allocate a "+
			"private Redis database, so the flush cannot be kept away from DB 0 (#2680): %w", allocErr)
	}

	opts := c.Options()
	if err := resetGuard(opts.Addr, opts.DB, allocAddr, allocDB); err != nil {
		return err
	}
	if err := c.Do(ctx, "FLUSHDB", "SYNC").Err(); err != nil {
		// Addr is host:port — credentials live in Options.Password, never here —
		// so naming it is safe and tells the reader which server refused.
		return fmt.Errorf("redistest: FLUSHDB on %s DB %d failed: %w", opts.Addr, opts.DB, err)
	}
	return nil
}

// resetGuard decides whether flushing clientDB is permitted for a process that
// owns allocatedDB. Split out of Reset so it is unit-testable without Redis —
// the unallocated case below cannot be reached from an integration test, because
// allocation is a package-level sync.Once that any earlier test has already run.
//
// The allocatedDB < 1 branch is load-bearing and is NOT defensive padding.
// Reset originally compared clientDB against allocDB directly, which fails OPEN
// in the single worst case: before any URL/DB/Options/Client call in a process,
// allocDB is still the zero value, so a client pointed at DB 0 compared EQUAL
// and was flushed. DB 0 holds the concord:testredis:seq ticket counter and the
// dev app's own data — the exact namespace every other rule here exists to keep
// FLUSHDB away from. An unallocated process must therefore be refused outright
// rather than compared. Found by the Task-1 implementer reviewing its own
// transcription against the plan (#2680).
func resetGuard(clientAddr string, clientDB int, allocatedAddr string, allocatedDB int) error {
	if allocatedDB < 1 {
		return errors.New(
			"redistest: refusing to flush — this process has not allocated a Redis DB index. " +
				"Call redistest.Client, Options, URL or DB first so the flush is scoped to a " +
				"private index instead of DB 0, which holds the ticket counter and dev data (#2680)")
	}
	// The server check is not redundant with the index check. An earlier revision
	// compared indices alone, so a client whose Addr had been repointed — at a
	// second instance, a staging box, a shared CI Redis — still matched on the
	// index and was flushed. Options() hands out a mutable struct, so that is a
	// one-line mistake, not a contrived one.
	if clientAddr != allocatedAddr {
		return fmt.Errorf(
			"redistest: refusing to flush %s — this process allocated its database on %s. "+
				"A flush is only ever scoped to the server this process allocated from (#2680)",
			clientAddr, allocatedAddr)
	}
	if clientDB != allocatedDB {
		return fmt.Errorf(
			"redistest: refusing to flush DB %d — this process owns DB %d. "+
				"Build the client via redistest.Client/Options so the flush cannot reach another "+
				"test process (#2680)", clientDB, allocatedDB)
	}
	return nil
}

// pingFailureMessage explains a failed test-Redis ping. Auth failures get their
// own message because docker-compose starts Redis with --requirepass, so an
// uncredentialed (NOAUTH) or stale-password (WRONGPASS) REDIS_URL fails every
// Redis-backed test identically and the bare driver error names no cause.
//
// redis.IsAuthError is the authoritative check — it covers NOAUTH, WRONGPASS and
// "unauthenticated", and unwraps. The string fallback exists because it matches
// on redis' own error TYPES, which callers cannot construct: proto.RedisError is
// internal, so the unit tests can only reach this branch by content.
//
// Never echo the URL — it carries a password.
func pingFailureMessage(err error) string {
	msg := err.Error()
	if redis.IsAuthError(err) || strings.Contains(msg, "NOAUTH") || strings.Contains(msg, "WRONGPASS") {
		return "redistest: redis rejected the connection (auth failed). REDIS_URL must carry the " +
			"docker-compose REDIS_PASSWORD, e.g. redis://:<password>@localhost:6379 — the DB index " +
			"is allocated per process and must not be set by hand; see docs/development.md"
	}
	return "redistest: failed to ping redis: " + msg
}
