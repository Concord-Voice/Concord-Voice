package attestation

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// scanBatch is how many keys SCAN returns per cursor step. Bounded so a
// cleanup against a Redis with many attestation keys doesn't pull the
// universe in a single round-trip.
const scanBatch int64 = 100

// sessionIDLogPrefixLen is the number of leading characters of the session ID
// to retain in log lines. Eight chars of a 36-char UUID is enough operator
// signal (e.g., to disambiguate between concurrent cleanups in a log tail)
// without the full session ID appearing in any log sink. Per
// [internal]rules/observability.md "No PII" + the broader Class-1 secret-leak
// rule: session IDs are PII-adjacent identifiers and must not appear in full
// in production logs.
const sessionIDLogPrefixLen = 8

// redactSessionID returns a short, log-safe representation of a session ID:
// the first sessionIDLogPrefixLen characters followed by an ellipsis. Returns
// "<empty>" for an empty input (cleanup_session already short-circuits this,
// but the helper is defensive). Used in log "session_id_short" fields so
// operators retain enough signal to correlate concurrent cleanups without the
// full ID leaking.
func redactSessionID(sessionID string) string {
	if sessionID == "" {
		return "<empty>"
	}
	if len(sessionID) <= sessionIDLogPrefixLen {
		return sessionID + "..."
	}
	return sessionID[:sessionIDLogPrefixLen] + "..."
}

// CleanupTokensForSession deletes all attestation token keys matching
// attestation:<sessionID>:*. Called from the auth Logout handler (and any
// other session-revoking path) so attestation tokens don't outlive the
// session that bound them.
//
// Uses SCAN MATCH (not KEYS) so a Redis with many keys isn't blocked by the
// cleanup. Per-key DEL failures are logged but do not abort the loop —
// orphan keys are harmless (they auto-expire at TTL and are scoped to a
// session that no longer exists), so partial cleanup is acceptable.
//
// Returns the number of keys successfully deleted (best-effort count;
// callers should not rely on it for correctness, only observability).
//
// Observability discipline: log lines omit the full session ID and the raw
// SCAN pattern (both contain the session ID). The "session_id_short" field
// carries only the first 8 chars + ellipsis — enough to correlate
// concurrent cleanups without the full identifier appearing in any sink.
// Per [internal]rules/observability.md and finding #11 of the #1264 review.
func CleanupTokensForSession(ctx context.Context, rdb *redis.Client, log *logger.Logger, sessionID string) int {
	if sessionID == "" || rdb == nil {
		return 0
	}

	pattern := fmt.Sprintf("attestation:%s:*", sessionID)
	iter := rdb.Scan(ctx, 0, pattern, scanBatch).Iterator()
	shortID := redactSessionID(sessionID)

	var deleted int
	for iter.Next(ctx) {
		key := iter.Val()
		if err := rdb.Del(ctx, key).Err(); err != nil {
			if log != nil {
				log.With("event", "attestation.cleanup_del_failed",
					"session_id_short", shortID, "error", err.Error()).
					Warn("attestation token cleanup: failed to delete key")
			}
			continue
		}
		deleted++
	}
	if err := iter.Err(); err != nil && log != nil {
		log.With("event", "attestation.cleanup_scan_failed",
			"session_id_short", shortID, "error", err.Error()).
			Warn("attestation token cleanup: scan iteration error")
	}
	if deleted > 0 && log != nil {
		log.With("event", "attestation.cleanup_succeeded",
			"session_id_short", shortID, "deleted", deleted).
			Info("attestation tokens cleaned up")
	}
	return deleted
}
