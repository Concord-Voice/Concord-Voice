package redistest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveIndex_DistinctAcrossPool(t *testing.T) {
	const pool = 15
	seen := map[int]bool{}
	for ticket := int64(1); ticket <= pool; ticket++ {
		idx, err := resolveIndex(ticket, pool)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, idx, 1, "index 0 is reserved and must never be allocated")
		assert.LessOrEqual(t, idx, pool)
		assert.False(t, seen[idx], "ticket %d reused index %d — allocation must be birthday-free across one pool", ticket, idx)
		seen[idx] = true
	}
	assert.Len(t, seen, pool, "P consecutive tickets must map to P distinct indices")
}

func TestResolveIndex_WrapsAtPoolBoundary(t *testing.T) {
	const pool = 15

	last, err := resolveIndex(int64(pool), pool)
	require.NoError(t, err)
	assert.Equal(t, pool, last, "ticket == P must map to index P, not to 0")

	wrapped, err := resolveIndex(int64(pool)+1, pool)
	require.NoError(t, err)
	assert.Equal(t, 1, wrapped, "ticket P+1 must wrap to 1, never to 0")
}

func TestResolveIndex_RejectsClusterSizedPool(t *testing.T) {
	_, err := resolveIndex(1, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cluster",
		"a databases=1 server (cluster mode) must fail with a message naming the cause")
}

func TestPoolSizeFrom_HappyPath(t *testing.T) {
	assert.Equal(t, 15, poolSizeFrom(map[string]string{"databases": "16"}, nil))
	assert.Equal(t, 63, poolSizeFrom(map[string]string{"databases": "64"}, nil))
}

func TestPoolSizeFrom_FallsBackWhenUnavailable(t *testing.T) {
	assert.Equal(t, fallbackPoolSize, poolSizeFrom(nil, errors.New("CONFIG refused")),
		"a refused CONFIG GET must fail open to the fallback, never abort the run")
	assert.Equal(t, fallbackPoolSize, poolSizeFrom(map[string]string{}, nil),
		"a reply missing the key must fall back")
	assert.Equal(t, fallbackPoolSize, poolSizeFrom(map[string]string{"databases": "banana"}, nil),
		"a non-numeric reply must fall back")
}

func TestPoolSizeFrom_ClusterReplyPropagatesAsZero(t *testing.T) {
	assert.Equal(t, 0, poolSizeFrom(map[string]string{"databases": "1"}, nil),
		"databases=1 must yield pool 0 so resolveIndex can name cluster mode")
}

func TestRewriteURL_SetsDBSegment(t *testing.T) {
	got, err := rewriteURL("redis://:pw@localhost:6379", 7)
	require.NoError(t, err)
	assert.Equal(t, "redis://:pw@localhost:6379/7", got)
}

func TestRewriteURL_ReplacesExistingDBSegment(t *testing.T) {
	got, err := rewriteURL("redis://:pw@localhost:6379/1", 9)
	require.NoError(t, err)
	assert.Equal(t, "redis://:pw@localhost:6379/9", got,
		"an explicit DB in REDIS_URL must be overridden, not honoured — this is the CI-coverage lock")
}

func TestRewriteURL_RejectsUnparseableAndWithholdsSecrets(t *testing.T) {
	// Named canary, not "secret": a const named `secret` trips detect-secrets'
	// Secret Keyword detector, and annotating a synthetic sentinel with
	// `pragma: allowlist secret` would mislabel it as a real allowed credential.
	const canary = "n0t-a-r3al-p4ssw0rd"
	_, err := rewriteURL("redis://:"+canary+"@localhost:6379/1/2", 3)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), canary,
		"observability.md #1 regression lock: the URL carries a password and must never be echoed")
	assert.NotContains(t, err.Error(), "localhost",
		"withhold the host too — the whole URL stays out of error text")
}

func TestRewriteURL_RejectsNonRedisScheme(t *testing.T) {
	_, err := rewriteURL("http://localhost:6379", 1)
	require.Error(t, err)
	assert.False(t, strings.Contains(err.Error(), "localhost"))
}

// The three tests below are PORTED, not new. They lived in
// internal/testhelpers/testredis_test.go and covered that package's
// pingFailureMessage. #2680 moved the function here and deleted the originals
// with the old helper, silently dropping a CWE-209 regression lock — caught by
// the Task-2 implementer and reported rather than absorbed. The DB-suffix
// assertion is the one deliberate change: the old message hard-coded "/1",
// which is exactly the fixed pin this issue removed, so it now asserts the
// message does NOT name a fixed index.

// Both shapes reach the same branch: NOAUTH is an uncredentialed URL, WRONGPASS
// is a stale or shell-overridden REDIS_PASSWORD. The second is not theoretical —
// a URL whose credential is sent as a 2-arg AUTH produces exactly this.
func TestPingFailureMessageExplainsAuthFailures(t *testing.T) {
	for _, driverErr := range []string{
		"NOAUTH Authentication required",
		"WRONGPASS invalid username-password pair or user is disabled",
	} {
		msg := pingFailureMessage(errors.New(driverErr))

		assert.Contains(t, msg, "REDIS_PASSWORD", "%q: message should name REDIS_PASSWORD", driverErr)
		assert.Contains(t, msg, "docs/development.md", "%q: message should point at the docs", driverErr)
		assert.NotContains(t, msg, "/1",
			"%q: the message must NOT name a fixed DB index — the pin is what #2680 removed", driverErr)
	}
}

// CWE-209: the diagnostic is printed on every failing Redis-backed test, so it
// must describe the credential without ever reproducing it.
func TestPingFailureMessageNeverLeaksThePassword(t *testing.T) {
	for _, driverErr := range []string{
		"NOAUTH Authentication required",
		"WRONGPASS invalid username-password pair or user is disabled",
		"dial tcp 127.0.0.1:6379: connect: connection refused",
	} {
		msg := pingFailureMessage(errors.New(driverErr))
		assert.NotContains(t, msg, devRedisVal, "%q: message leaked the password", driverErr)
	}
}

func TestPingFailureMessagePassesThroughOtherErrors(t *testing.T) {
	msg := pingFailureMessage(errors.New("dial tcp 127.0.0.1:6379: connect: connection refused"))

	assert.Contains(t, msg, "connection refused", "non-auth message should carry the driver error")
	assert.NotContains(t, msg, "REDIS_PASSWORD", "non-auth message should not give the auth hint")
}

const probeAddr = "localhost:6379"

func TestResetGuard_AllowsOwnDatabase(t *testing.T) {
	require.NoError(t, resetGuard(probeAddr, 5, probeAddr, 5))
}

func TestResetGuard_RefusesForeignDatabase(t *testing.T) {
	err := resetGuard(probeAddr, 3, probeAddr, 5)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to flush DB 3")
	assert.Contains(t, err.Error(), "owns DB 5")
}

// The regression lock for a guard that compared indices ONLY. Options() returns
// a mutable struct, so repointing Addr at another instance — a second local
// Redis, a staging box, a shared CI Redis — kept DB equal and the flush landed
// on a foreign server.
func TestResetGuard_RefusesForeignServer(t *testing.T) {
	err := resetGuard("staging.internal:6379", 5, probeAddr, 5)
	require.Error(t, err, "a matching index on a DIFFERENT server must not be flushable")
	assert.Contains(t, err.Error(), "staging.internal:6379")
	assert.Contains(t, err.Error(), probeAddr)
}

func TestResetGuard_RefusesBeforeAllocation(t *testing.T) {
	// The regression lock for the fail-OPEN this guard was extracted to close.
	// An unallocated process has allocDB == 0, so comparing clientDB against it
	// made a DB-0 client compare EQUAL and get flushed — taking out the ticket
	// counter and the dev app's live data. Refusal must not depend on the two
	// numbers differing.
	err := resetGuard(probeAddr, 0, probeAddr, 0)
	require.Error(t, err, "a DB-0 client must never be flushable, least of all when nothing is allocated")
	assert.Contains(t, err.Error(), "has not allocated")

	// A non-zero client on an unallocated process is refused for the same reason.
	require.Error(t, resetGuard(probeAddr, 7, probeAddr, 0))
	// And the unallocated check must fire BEFORE the address check, so the error
	// names the real cause rather than an empty allocated address.
	unallocated := resetGuard(probeAddr, 7, "", 0)
	require.Error(t, unallocated)
	assert.Contains(t, unallocated.Error(), "has not allocated")
}

func TestResetGuard_RefusesNegativeAllocation(t *testing.T) {
	require.Error(t, resetGuard(probeAddr, -1, probeAddr, -1))
}

// go-redis applies a `db` query parameter AFTER the path and it wins, so a
// REDIS_URL carrying one silently defeated the whole rewrite.
func TestRewriteURL_StripsDBQueryParam(t *testing.T) {
	got, err := rewriteURL("redis://:pw@localhost:6379/2?db=3", 11)
	require.NoError(t, err)

	// Assert through ParseURL, not on the string: the round trip is the actual
	// contract, and it survives a change in how the URL is serialised.
	opts, err := redis.ParseURL(got)
	require.NoError(t, err)
	assert.Equal(t, 11, opts.DB, "the allocated index must win over any db query param")
}

func TestRewriteURL_PreservesOtherQueryParams(t *testing.T) {
	got, err := rewriteURL("redis://:pw@localhost:6379?db=3&dial_timeout=5s", 9)
	require.NoError(t, err)

	opts, err := redis.ParseURL(got)
	require.NoError(t, err)
	assert.Equal(t, 9, opts.DB)
	assert.Equal(t, 5*time.Second, opts.DialTimeout,
		"stripping db must not blunt-force clear the whole query string")
}

// ParseURL accepts unix:// — where the path is the SOCKET ADDRESS. Rewriting it
// would overwrite the socket path, and the failure would surface as a
// pool-size diagnosis naming entirely the wrong cause.
func TestRewriteURL_RejectsUnixScheme(t *testing.T) {
	_, err := rewriteURL("unix:///var/run/redis.sock?db=1", 7)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unix", "the error must name the scheme, not the pool size")
	assert.NotContains(t, err.Error(), "redis.sock", "the rest of the URL stays withheld")
}

// The #2789-review regression lock, and the most dangerous shape found in this
// package: an OPAQUE URL silently defeated the entire rewrite.
//
// `redis:6379` — no "//" — parses with the authority in u.Opaque and an EMPTY
// u.Path, and URL.String() emits Opaque INSTEAD of Path. So `u.Path = "/7"` was
// discarded, rewriteURL returned the input unchanged WITH A NIL ERROR, and
// go-redis resolved it to DB 0. The scheme check above does not catch it (the
// scheme is a legitimate "redis"), and allocate()'s probe does not either — it
// dials the returned URL and PINGs DB 0 successfully, "proving" an index it
// never selected. Every client in the process then shared DB 0: the dev app's
// live data and this package's own ticket counter. #2680, restored in full, by
// a REDIS_URL missing two characters.
func TestRewriteURL_RejectsOpaqueURL(t *testing.T) {
	for _, base := range []string{"redis:6379", "redis:localhost:6379"} {
		t.Run(base, func(t *testing.T) {
			_, err := rewriteURL(base, 7)
			require.Error(t, err,
				"an opaque redis: URL discards the rewritten path and resolves to DB 0")
			assert.Contains(t, err.Error(), "resolves to 0",
				"the error must name what actually happened, not a generic parse failure")
			assert.NotContains(t, err.Error(), "6379",
				"the URL stays withheld — it carries a password")
		})
	}
}

// The positive control for the test above. Without it, a "fix" that rejected
// every URL would satisfy the opaque case and break the package entirely.
func TestRewriteURL_AcceptsOrdinaryURLUnchangedInShape(t *testing.T) {
	got, err := rewriteURL("redis://:pw@localhost:6379", 7)
	require.NoError(t, err)
	opts, err := redis.ParseURL(got)
	require.NoError(t, err)
	assert.Equal(t, 7, opts.DB, "the ordinary form must still be rewritten")
	assert.Equal(t, "localhost:6379", opts.Addr)
}
