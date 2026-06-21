package attestation

import (
	"context"
	"fmt"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
)

// counterTTL is how long hourly bucket counters persist (7 days).
const counterTTL = 7 * 24 * time.Hour

// hourBucket formats the current UTC hour as YYYYMMDDHH.
func hourBucket(t time.Time) string {
	return t.UTC().Format("2006010215")
}

// LogRejected emits a structured `attestation.rejected` log + increments hourly counter.
// PII-free per [internal]rules/observability.md. The Redis writes are best-effort:
// errors do NOT break the request path, but per finding #25 of #1264 review
// each failure mode is now logged at WARN so operators can distinguish
// "counter is healthy" from "counter Incr failed" from "counter Expire failed
// → unbounded growth".
func LogRejected(ctx context.Context, log *logger.Logger, rdb *redis.Client, code ErrorCode, version string, platform Platform) {
	log.With(
		"event", "attestation.rejected",
		"reason", string(code),
		"version", version,
		"platform", string(platform),
	).Warn("attestation rejected")

	if rdb == nil {
		return
	}
	key := fmt.Sprintf("attestation:rejected:%s:%s", code, hourBucket(time.Now()))
	if err := rdb.Incr(ctx, key).Err(); err != nil {
		log.With(
			"event", "attestation.counter_incr_failed",
			"key", key,
			"error", err.Error(),
		).Warn("attestation rejected-counter Incr failed; metric for this bucket is under-counted")
		return
	}
	if err := rdb.Expire(ctx, key, counterTTL).Err(); err != nil {
		// Expire failure means the counter key has no TTL — without intervention
		// the SET grows unbounded across hour-buckets. Surface so operators can
		// reset the TTL manually or restart Redis.
		log.With(
			"event", "attestation.counter_expire_failed",
			"key", key,
			"error", err.Error(),
		).Warn("attestation rejected-counter Expire failed; counter key may live without TTL")
	}
}

// LogIssued emits successful verify telemetry + hourly counter.
// Same Incr/Expire failure-logging discipline as LogRejected (finding #25).
func LogIssued(ctx context.Context, log *logger.Logger, rdb *redis.Client, version, spaVersion string, platform Platform) {
	log.With(
		"event", "attestation.issued",
		"version", version,
		"spa_version", spaVersion,
		"platform", string(platform),
	).Info("attestation issued")

	if rdb == nil {
		return
	}
	key := fmt.Sprintf("attestation:issued:%s", hourBucket(time.Now()))
	if err := rdb.Incr(ctx, key).Err(); err != nil {
		log.With(
			"event", "attestation.counter_incr_failed",
			"key", key,
			"error", err.Error(),
		).Warn("attestation issued-counter Incr failed; metric for this bucket is under-counted")
		return
	}
	if err := rdb.Expire(ctx, key, counterTTL).Err(); err != nil {
		log.With(
			"event", "attestation.counter_expire_failed",
			"key", key,
			"error", err.Error(),
		).Warn("attestation issued-counter Expire failed; counter key may live without TTL")
	}
}
