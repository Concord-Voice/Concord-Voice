package presence

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// statusRedisKeyFmt is the canonical Redis key format for a user's base
// presence status. It lives in this package because both internal/presence
// (rich-presence policy) and internal/websocket (the hub) must agree on it,
// and websocket imports presence, not the reverse. Duplicating this literal
// across packages is a silent-drift hazard on a privacy-critical key.
const statusRedisKeyFmt = "presence:%s"

// The canonical persisted base-presence status values. They live here for the
// same reason the key format does: both packages must agree on them, and
// internal/websocket aliases these rather than declaring its own copies.
const (
	StatusOnline    = "online"
	StatusDND       = "dnd"
	StatusInvisible = "invisible"
	StatusOffline   = "offline"
)

// StatusTTL is the lifetime of a base-presence key.
//
// It lives beside StatusRedisKey for the same reason the key format does: every
// writer of presence:<uuid> must agree on it. That agreement is load-bearing for
// the hub's presence sweeper (internal/websocket, sweepPresenceLiveness), which
// may only EXPIRE an existing key, and can therefore touch a key another path has
// just rewritten -- including the offline marker transitionUserOffline writes.
//
// Be precise about WHY, because the obvious phrasing is false: EXPIRE restarts
// the countdown from NOW, so renewing a key to its own TTL is an exact no-op only
// if issued at the instant of the write -- which it is not. Two things make it
// safe instead. EXPIRE cannot change the stored VALUE, so an offline marker stays
// offline and reads identically to an absent key. And the registration gate in
// noteInboundApplicationFrame bounds the sweeper's reach into a departing user's
// key to one snapshot-to-pipeline round trip, so the shift is sub-second rather
// than a fresh full lifetime from an arbitrary later moment. A sweeper carrying
// its OWN, larger TTL would have neither bound.
//
// Do not reintroduce a local 120*time.Second, and do not give the sweeper its own.
const StatusTTL = 120 * time.Second

// StatusRedisKey returns the Redis key holding one user's base presence status.
func StatusRedisKey(userID uuid.UUID) string {
	return fmt.Sprintf(statusRedisKeyFmt, userID)
}

// EmissionPermittedForStatus maps a persisted base-presence status to a
// rich-presence emission verdict.
//
// It fails CLOSED on everything that is not explicitly a visible status, so
// "invisible", an unknown value, and the empty string (which is what a caller
// has after a missing key) all suppress. Callers that need to distinguish those
// cases for logging must do so before calling this -- the mapping itself is
// intentionally total and silent, because internal/presence is AST-guarded
// against log emissions.
func EmissionPermittedForStatus(status string) bool {
	switch status {
	case StatusOnline, StatusDND:
		return true
	default:
		return false
	}
}
