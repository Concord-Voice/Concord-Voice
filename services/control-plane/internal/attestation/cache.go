package attestation

import (
	"context"
	"sync"
	"time"

	natsLib "github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	cpnats "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

const (
	subjectRegistryUpdated = "attestation.registry.updated"
	subjectRegistryRevoked = "attestation.registry.revoked"
	pollFallbackInterval   = 5 * time.Minute
)

// Re-exported in-package alias for the canonical key defined in types.go.
// Keeping the unqualified `revokedVersionsKey` identifier preserves existing
// call sites while pointing at the single source of truth. See #1264 finding
// #29 for the deduplication rationale.
const revokedVersionsKey = RevokedVersionsKey

// Reader is the subset of Repository methods the Cache requires.
// *Repository satisfies this interface structurally; tests may supply a fake.
type Reader interface {
	ListActiveBinaries(ctx context.Context) ([]ReleaseBinary, error)
	ListActiveSPAs(ctx context.Context) ([]ReleaseSPA, error)
}

// Cache is an in-memory mirror of the attestation registry.
// The hot path (RequireAttestation middleware + verify handler) reads from
// here without touching Postgres. Hydrated at startup, refreshed via NATS
// events, and poll-refreshed every pollFallbackInterval as defense-in-depth.
type Cache struct {
	repo Reader
	nc   *cpnats.Client
	rdb  *redis.Client
	log  *logger.Logger

	mu       sync.RWMutex
	binaries map[binaryKey]*ReleaseBinary
	spas     map[string]*ReleaseSPA
	lastSync time.Time

	healthMu         sync.Mutex
	events           securityevent.Emitter
	postgresDegraded bool
	redisDegraded    bool
}

type binaryKey struct {
	Version  string
	Platform Platform
}

// NewCache wires a Cache against the given dependencies.
// nc and rdb may be nil for tests or environments without those services.
func NewCache(repo Reader, nc *cpnats.Client, rdb *redis.Client, log *logger.Logger) *Cache {
	return &Cache{
		repo:     repo,
		nc:       nc,
		rdb:      rdb,
		log:      log,
		binaries: map[binaryKey]*ReleaseBinary{},
		spas:     map[string]*ReleaseSPA{},
		events:   securityevent.Discard,
	}
}

// SetSecurityEvents injects bounded cache-health telemetry.
func (c *Cache) SetSecurityEvents(events securityevent.Emitter) {
	if events == nil {
		events = securityevent.Discard
	}
	c.healthMu.Lock()
	c.events = events
	c.healthMu.Unlock()
}

// setDegraded preserves the package-local test seam for PostgreSQL failures.
func (c *Cache) setDegraded(ctx context.Context, degraded bool) {
	c.setPostgresDegraded(ctx, degraded)
}

func (c *Cache) setPostgresDegraded(ctx context.Context, degraded bool) {
	c.setDependencyDegraded(ctx, &degraded, nil)
}

func (c *Cache) setRedisDegraded(ctx context.Context, degraded bool) {
	c.setDependencyDegraded(ctx, nil, &degraded)
}

func (c *Cache) setDependencyDegraded(ctx context.Context, postgres, redis *bool) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	wasDegraded := c.postgresDegraded || c.redisDegraded
	if postgres != nil {
		c.postgresDegraded = *postgres
	}
	if redis != nil {
		c.redisDegraded = *redis
	}
	degraded := c.postgresDegraded || c.redisDegraded
	changed := wasDegraded != degraded
	events := c.events
	if !changed {
		return
	}
	if degraded {
		events.Emit(ctx, securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAttestationCacheDegraded})
		return
	}
	events.Emit(ctx, securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered})
}

// Hydrate replaces in-memory state with current DB rows. Safe to call
// concurrently; holds the write lock only for the swap.
func (c *Cache) Hydrate(ctx context.Context) error {
	bins, err := c.repo.ListActiveBinaries(ctx)
	if err != nil {
		c.setPostgresDegraded(ctx, true)
		return err
	}
	spas, err := c.repo.ListActiveSPAs(ctx)
	if err != nil {
		c.setPostgresDegraded(ctx, true)
		return err
	}

	c.mu.Lock()

	c.binaries = make(map[binaryKey]*ReleaseBinary, len(bins))
	for i := range bins {
		c.binaries[binaryKey{Version: bins[i].Version, Platform: bins[i].Platform}] = &bins[i]
	}
	c.spas = make(map[string]*ReleaseSPA, len(spas))
	for i := range spas {
		c.spas[spas[i].SpaVersion] = &spas[i]
	}
	c.lastSync = time.Now()
	c.mu.Unlock()
	c.setPostgresDegraded(ctx, false)
	return nil
}

// LookupBinary returns the active binary entry for the given version+platform.
// The second return value reports whether a non-revoked entry was found.
//
// Returns by VALUE rather than by pointer (finding #23 of #1264 review): a
// caller that mutates the result would otherwise corrupt the in-memory cache.
// The struct is small (~120 bytes) so the copy is negligible compared to the
// safety win.
func (c *Cache) LookupBinary(version string, platform Platform) (ReleaseBinary, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	rb, ok := c.binaries[binaryKey{Version: version, Platform: platform}]
	if !ok || rb == nil || rb.RevokedAt != nil {
		return ReleaseBinary{}, false
	}
	return *rb, true
}

// LookupSPA returns the active SPA entry for the given spa_version. The second
// return value reports whether a non-revoked entry was found. Returns by VALUE
// for the same reason as LookupBinary (finding #23 of #1264 review).
func (c *Cache) LookupSPA(spaVersion string) (ReleaseSPA, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	rs, ok := c.spas[spaVersion]
	if !ok || rs == nil || rs.RevokedAt != nil {
		return ReleaseSPA{}, false
	}
	return *rs, true
}

// IsRevoked reports whether the given version is in the Redis revoked_versions
// set. On Redis error it returns true plus the underlying error so the caller
// can preserve the fail-closed decision while distinguishing dependency loss
// from a genuinely revoked release.
// Returns false when rdb is nil (test / dev environments without Redis).
//
// On Redis error a WARN log is emitted so operators can distinguish
// "version genuinely revoked" rejections from "Redis is down → fail-closed"
// rejections at the call site. Per finding #16 of the #1264 review and
// [internal]rules/observability.md: errors that change security behavior
// (here: blocking a non-revoked version) must be observable, not silent.
// The version field is the application-level version string, not user PII,
// so it is safe to include in the log line.
func (c *Cache) IsRevoked(ctx context.Context, version string) (bool, error) {
	if c.rdb == nil {
		return false, nil
	}
	isMember, err := c.rdb.SIsMember(ctx, revokedVersionsKey, version).Result()
	if err != nil {
		c.setRedisDegraded(ctx, true)
		if c.log != nil {
			c.log.With(
				"event", "attestation.is_revoked_redis_error",
				"version", version,
				"error", err.Error(),
			).Warn("attestation IsRevoked Redis error; failing closed")
		}
		return true, err // caller must fail closed without classifying as revoked
	}
	c.setRedisDegraded(ctx, false)
	return isMember, nil
}

// Start subscribes to NATS registry-change events and spawns the
// poll-fallback goroutine. Returns the live NATS subscriptions for
// graceful shutdown by the caller. nc may be nil; subscriptions are
// skipped but the poll loop still runs.
func (c *Cache) Start(ctx context.Context) ([]*natsLib.Subscription, error) {
	var subs []*natsLib.Subscription

	if c.nc != nil {
		sub1, err := c.nc.Subscribe(subjectRegistryUpdated, func(_ []byte) {
			if hErr := c.Hydrate(ctx); hErr != nil {
				c.log.With("error", hErr.Error()).Error("attestation cache refresh failed (NATS updated)")
			}
		})
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub1)

		sub2, err := c.nc.Subscribe(subjectRegistryRevoked, func(_ []byte) {
			if hErr := c.Hydrate(ctx); hErr != nil {
				c.log.With("error", hErr.Error()).Error("attestation cache refresh failed (NATS revoked)")
			}
		})
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub2)
	}

	go c.pollLoop(ctx)
	return subs, nil
}

// pollLoop refreshes the cache at pollFallbackInterval until ctx is cancelled.
func (c *Cache) pollLoop(ctx context.Context) {
	t := time.NewTicker(pollFallbackInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Hydrate(ctx); err != nil {
				c.log.With("error", err.Error()).Warn("attestation cache poll-fallback refresh failed")
			}
		}
	}
}
