package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/health"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const (
	// readyzProbeInterval is how often the background prober re-evaluates
	// dependencies. Matches the compose healthcheck's own interval (a sibling
	// deploy-config change) so a container's liveness cadence and the
	// readiness signal age at roughly the same rate; nothing REQUIRES them to
	// match.
	readyzProbeInterval = 5 * time.Second

	// readyzStaleAfter is the staleness fence (health.Prober.Current's second
	// return). It MUST exceed readyzProbeInterval + health.ProbeTimeout
	// (5s + 2s = 7s) or a perfectly healthy prober would trip its own fence on
	// ordinary scheduling jitter. 20s gives roughly 3x that floor: generous
	// enough that a single slow-but-completing probe never falsely reports
	// stale, while a genuinely stuck goroutine (deadlock, a driver ignoring
	// cancellation) is still caught well inside one compose healthcheck retry
	// budget (5 retries x (5s interval + 4s timeout) = 45s).
	//
	// PANIC is deliberately not in that list. An unrecovered panic terminates
	// the process, so nothing survives to serve a stale verdict and this fence
	// could never observe one; Evaluate recovers per-probe instead. The
	// prober.go copy of this claim was corrected first and this mirror was
	// missed -- hardening one copy leaves the other weaker.
	readyzStaleAfter = 20 * time.Second
)

// Check-specific failure causes. Each carries a BOUNDED class that health's
// classifier surfaces to the log sink; the wrapped error itself is never
// logged and never serialized (CWE-532), and the /readyz body still carries
// only {name, status, gating}.
//
// These exist because a class the health package can derive on its own --
// timeout, cancelled, panic -- cannot distinguish causes only the check knows.
// The two NATS cases are the reason: IsConnected() returns the same false for
// a transient reconnect window and for a client that was never constructed,
// and those are opposite operator actions.
var (
	errPoolSaturated = &health.ClassifiedError{
		Class: "pool_saturated", Err: errors.New("connection pool saturated"),
	}
	errNATSNotConnected = &health.ClassifiedError{
		Class: "nats_not_connected", Err: errors.New("nats not connected"),
	}
	// A nil *Client means natsclient.Connect hit a CONFIG fault -- an
	// unparseable URL, bad TLS or credentials. Unlike a reconnect window that
	// self-heals, this never recovers, and boot does NOT fail on it (there is
	// no such guard anywhere; router.go's comment claiming otherwise is
	// corrected in this change). Without its own class it was reported
	// identically to a blip that fixes itself.
	errNATSUnconfigured = &health.ClassifiedError{
		Class: "nats_unconfigured", Err: errors.New("nats client not constructed"),
	}
	// A nil handle is a WIRING bug. NATS was given two-level nil safety and
	// Postgres/Redis were not, so a nil there surfaced as a nil dereference
	// inside a background goroutine 0-5s later rather than as a failed check.
	errHandleUnwired = &health.ClassifiedError{
		Class: "handle_unwired", Err: errors.New("dependency handle not wired"),
	}
)

// newReadinessChecks builds the #3106 checks over the SERVING db pool, the
// serving Redis client, and the NATS client -- never a dedicated handle
// (R1: a second single-connection *sql.DB is a single-lane queue with no
// head-of-line escape, and a ctx-cancelled ping under MaxOpenConns(1)
// discards the connection). Postgres and Redis gate; pool pressure and NATS
// connectivity are reported only (D1/R2).
func newReadinessChecks(db *sql.DB, redisClient *redis.Client, natsClient *natsclient.Client) []health.Check {
	return []health.Check{
		{
			Name:   "postgres",
			Gating: true,
			Probe: func(ctx context.Context) error {
				if db == nil {
					return errHandleUnwired
				}
				// SELECT 1, not PingContext: Ping proves only that the
				// connection is usable, while a query round-trips to the
				// backend and proves it accepts and executes statements.
				// It does NOT prove catalog health: SELECT 1 is a constant
				// expression and never reads pg_catalog.
				//
				// It does NOT detect a hot standby or
				// default_transaction_read_only -- SELECT 1 succeeds in both,
				// exactly as PingContext does. An earlier version of this
				// comment claimed otherwise, crediting the probe with the very
				// property it was chosen for. Real write-authority detection
				// needs pg_is_in_recovery() or SHOW transaction_read_only and
				// belongs to #3111, so /readyz can currently report ready
				// against a read-only primary. The bounding deadline is the ctx that
				// health.Evaluate already derives (ProbeTimeout) rather than
				// a session-level `SET statement_timeout` -- the latter would
				// need an explicit reset before the pooled connection is
				// returned, or it leaks onto whichever request reuses that
				// connection next.
				var one int
				return db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
			},
		},
		{
			Name:   "pool",
			Gating: false,
			Probe: func(context.Context) error {
				// db.Stats() takes only db.mu and returns a snapshot; unlike
				// a query it never acquires a connection, so it cannot itself
				// queue behind the saturation it is reporting.
				// internal/websocket/hub.go:64-85 documents 29-against-25 as
				// DESIGNED over-subscription during deploy-time mass
				// disconnect, so this check never gates (R2).
				//
				// Read that as "this CHECK does not gate", not "saturation
				// cannot gate" -- the design doc claimed the latter and its
				// own A1 section refutes it. database/sql charges connection
				// ACQUISITION to the caller's context, so sustained saturation
				// times out the GATING postgres probe and 503s /readyz through
				// the reachability check regardless of this bit. That is left
				// deliberately: classifying a timed-out probe as mere
				// saturation would fail OPEN when Postgres genuinely dies,
				// because dying is itself what pins every connection.
				// The MaxOpenConnections > 0 guard is load-bearing, not
				// defensive: database/sql reports 0 when SetMaxOpenConns was
				// never called, and 0 means UNBOUNDED. Without the guard an
				// unbounded pool would satisfy `Open >= Max` on its very first
				// connection and report saturated forever.
				// InUse, NOT OpenConnections. OpenConnections counts idle
				// connections too, so a warm pool that has simply opened all
				// its slots would report saturated forever while nothing is
				// contended. InUse == Max means every connection is checked
				// out, which is the state a caller would actually queue behind.
				// (WaitCount/WaitDuration deltas would be a better signal
				// still, but they are cumulative counters and this probe is
				// point-in-time; deltas belong with the alerting that consumes
				// this field, not in the probe.)
				if db == nil {
					// Without this, db.Stats() panics before the probe can
					// return, Evaluate's recover catches it, and the check
					// reports probe_panic instead of handle_unwired -- the
					// wrong cause for a wiring bug. The postgres probe got this
					// guard and the pool probe did not.
					return errHandleUnwired
				}
				stats := db.Stats()
				if stats.MaxOpenConnections > 0 && stats.InUse >= stats.MaxOpenConnections {
					return errPoolSaturated
				}
				return nil
			},
		},
		{
			Name:   "redis",
			Gating: true,
			Probe: func(ctx context.Context) error {
				if redisClient == nil {
					return errHandleUnwired
				}
				return redisClient.Ping(ctx).Err()
			},
		},
		{
			Name:   "nats",
			Gating: false,
			Probe: func(context.Context) error {
				// Never gates: RetryOnFailedConnect(true) + MaxReconnects(-1)
				// (pkg/nats.Connect) mean a NATS outage self-heals, so gating
				// on it would 503 the only control-plane node during every
				// cold start where NATS lags Postgres.
				if natsClient == nil {
					return errNATSUnconfigured
				}
				if !natsClient.IsConnected() {
					return errNATSNotConnected
				}
				return nil
			},
		},
	}
}

// readyzCheck is the wire shape of one check inside the /readyz body. Only
// name, status, and gating -- never the underlying error, host, port, or
// driver text (CWE-532; router.go:614-620 is the precedent for this posture).
type readyzCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Gating bool   `json:"gating"`
}

// Severity levels for a prober log line, chosen in api so the stdlib-only
// health leaf never learns about log levels.
const (
	readinessLevelInfo  = "info"
	readinessLevelWarn  = "warn"
	readinessLevelError = "error"
)

// readinessLogLevel picks the severity for a prober line. THREE levels, not
// two, because a two-way loss/no-loss split silently under-reported the case
// the widened log edge was added to surface.
//
//   - ERROR: readiness was lost ("to" is not_ready), or the probe reported ANY
//     failure_class -- ClassWedged from the watchdog, ClassPanic from a
//     recovered probe panic, or an unrecognised future one, which fails closed
//     to loud rather than silently to Info. All are pages.
//   - WARN:  readiness is unchanged but some check is DOWN. This is a
//     non-gating failure -- pool saturation, or NATS unreachable or
//     unconfigured. Ready does not move for these by design, so the earlier
//     predicate saw "to: ready" and sent them to Info, indistinguishable by
//     level from the per-request line middleware.Logger emits for every HTTP
//     call. A permanently unconfigured NATS client is not routine traffic.
//   - INFO:  everything up, including the recovery edge.
//
// Warn rather than Error is the deliberate half: these checks are declared
// non-gating precisely so they alert rather than page (design R2), and
// escalating them would re-create the fleet-wide paging that decision avoids.
// It FAILS CLOSED to Warn on any shape it does not recognise. This function
// matches two cross-package contracts that nothing else couples: the verdict
// words (health.WordReady / WordNotReady, exported for exactly this reason --
// a bare "not_ready" literal here would have silently dropped every readiness
// LOSS from Error to Info if that word were ever renamed, with nothing failing
// to compile) and failingChecks's []string return type. Both drifts used to
// land on Info, which is the one outcome that must never be reached by
// accident.
// routeReadinessLine sends one prober line to the sink its severity demands.
//
// It exists as a named function because the switch used to live inline in
// NewRouter's SetLogger closure, where nothing could reach it: replacing the
// whole adapter with a bare log.Info left readinessLogLevel still referenced
// from its own table test, so everything compiled, the table passed, and the
// production effect was silently gone.
func routeReadinessLine(msg string, kv []any, info, warn, errf func(string, ...any)) {
	switch readinessLogLevel(kv) {
	case readinessLevelError:
		errf(msg, kv...)
	case readinessLevelWarn:
		warn(msg, kv...)
	default:
		info(msg, kv...)
	}
}

func readinessLogLevel(kv []any) string {
	f := parseReadinessLog(kv)
	switch {
	case f.hasFailureClass:
		// Any failure_class pages, and it pages even during a drain: nothing
		// else will ever explain the probe_stale that follows.
		//
		// TWO values reach here, not one, which the old name (`wedged`) hid:
		// ClassWedged from probeWatched's watchdog and ClassPanic from
		// onProbePanic. Both warrant Error today, so this is a naming fix, not
		// a behaviour change -- pinned for both values by
		// TestReadinessLogLevelSeparatesLossFromNonGatingFailure.
		//
		// An UNRECOGNISED future class also lands here, deliberately: a health
		// signal fails closed to loud. If a new class should route to Warn
		// instead, this switch is where that decision has to be written down.
		//
		// Only the prober's own SetLogger adapter reaches this parser, so the
		// shutdown-stage and NATS failure_class values in cmd/server/main.go
		// (which log through log.Warn directly) never arrive here.
		return readinessLevelError
	case f.malformed, !f.sawVerdict:
		// Shape drift. Never silently downgrade to Info -- that is the one
		// outcome that must not be reachable by accident.
		return readinessLevelWarn
	case f.lost && !f.draining:
		return readinessLevelError
	case f.anyFailing:
		return readinessLevelWarn
	default:
		return readinessLevelInfo
	}
}

// readinessLogFacts is everything the severity decision needs from a prober
// line. Parsing and deciding are separate so the decision reads as a table.
type readinessLogFacts struct {
	hasFailureClass bool
	sawVerdict      bool
	lost            bool
	draining        bool
	anyFailing      bool
	malformed       bool
}

func parseReadinessLog(kv []any) readinessLogFacts {
	var f readinessLogFacts
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		switch key {
		case "failure_class":
			f.hasFailureClass = true
		case "to":
			f.readVerdict(kv[i+1])
		case "draining":
			// A PLANNED drain is not a dependency loss. Evaluate sets
			// Ready = !Draining, so the first tick after the latch reads
			// `to: not_ready` with an empty failing list -- identical, at this
			// function's inputs, to Postgres dying. drainSettle keeps the
			// prober ticking through the drain, so that tick lands on most
			// shutdowns.
			if v, ok := kv[i+1].(bool); ok {
				f.draining = v
			}
		case "failing":
			v, ok := kv[i+1].([]string)
			if !ok {
				f.malformed = true
				continue
			}
			f.anyFailing = len(v) > 0
		}
	}
	return f
}

// readVerdict matches health's EXPORTED vocabulary. It is exported for exactly
// this reason: a bare "not_ready" literal here would have dropped every
// readiness LOSS from Error to Info if that word were ever renamed, with
// nothing failing to compile.
func (f *readinessLogFacts) readVerdict(raw any) {
	v, ok := raw.(string)
	if !ok || (v != health.WordReady && v != health.WordNotReady) {
		f.malformed = true
		return
	}
	f.sawVerdict = true
	f.lost = v == health.WordNotReady
}

// ReadyzHandler serves GET/HEAD /readyz. It performs NO I/O: it reads the
// background prober's latest verdict (internal/health.Prober), so the
// endpoint cannot block, queue or time out -- which is what lets the prober
// share the serving connection pool (#3106).
//
// A stale verdict fails CLOSED as "probe_stale" -- never last-known-good.
//
// The body never carries a probe error, DSN, hostname, port or driver text --
// only {name, status, gating} per check, plus a staleness marker.
// lib/pq formats a dial failure as "dial tcp <ip>:5432: connect: connection
// refused"; pgx would add user and database. Same posture as
// router.go:614-620 (CWE-532).
func ReadyzHandler(p *health.Prober) gin.HandlerFunc {
	return func(c *gin.Context) {
		// The drain is read LIVE and takes precedence over both the cached
		// verdict and the staleness fence. The two are not the same kind of
		// fact: a dependency check is an OBSERVATION and may legitimately be
		// one tick old, while the drain is PROCESS STATE the process knows
		// instantly. Reading the verdict's cached Draining bit meant the drain
		// was never observed at all — srv.Shutdown closes the listener within
		// microseconds of the latch and the next tick is up to
		// readyzProbeInterval (5s) away, so it never landed.
		draining := p.Draining()
		v, fresh := p.Current()

		// The checks are carried on EVERY path -- but LABELLED when the
		// verdict is not fresh, never passed off as current.
		//
		// Two defects met here. The comment used to say "up to one tick
		// stale", which is true only while the prober is healthy: the drain
		// branch skips the staleness fence, so a wedged prober could report
		// `postgres: up` from an arbitrarily old verdict during a shutdown.
		// And the probe_stale branch built this slice and then discarded it --
		// dropping the last-known state in precisely the case an operator (and
		// concord-ctl.sh, which prints this body on a failed deploy) needs it.
		checks := make([]readyzCheck, 0, len(v.Results))
		for _, r := range v.Results {
			status := "up"
			if !r.Up {
				status = "down"
			}
			checks = append(checks, readyzCheck{Name: r.Name, Status: status, Gating: r.Gating})
		}

		// Drain is live and wins; then the staleness fence; then the verdict.
		// A stale verdict is still failed CLOSED -- the checks below it are
		// evidence, never grounds for a 200.
		code, status := http.StatusOK, "ready"
		switch {
		case draining:
			code, status = http.StatusServiceUnavailable, "draining"
		case !fresh:
			code, status = http.StatusServiceUnavailable, "probe_stale"
		case !v.Ready:
			code, status = http.StatusServiceUnavailable, "not_ready"
		}

		if c.Request.Method == http.MethodHead {
			// Mirrors healthHandler's RFC 7231 posture (router.go): a HEAD
			// response carries no body, so skip the marshal rather than let
			// net/http discard it.
			c.Status(code)
			return
		}

		body := gin.H{"status": status, "service": "control-plane", "checks": checks}
		if !fresh {
			body["checks_stale"] = true
			// Age is only meaningful once something has been published; a
			// never-published verdict has a zero ObservedAt, which would
			// render as a ~55-year age.
			if age := p.Age(v); age > 0 {
				body["checks_age_seconds"] = int(age.Seconds())
			}
		}
		c.JSON(code, body)
	}
}
