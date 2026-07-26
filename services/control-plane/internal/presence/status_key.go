package presence

import (
	"fmt"

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
