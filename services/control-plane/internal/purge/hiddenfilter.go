package purge

import (
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmvisibility"
)

// HiddenRangeFilter returns the anti-join SQL fragment that excludes, from a
// dm_messages read, messages the requesting user has hidden. Legacy purge
// ranges hide peer messages; Clear ranges may also hide the actor's messages.
//
// alias is the dm_messages table alias used by the consuming query (e.g. "m" or
// "dm"); userParamPos is the positional parameter carrying the requesting user's
// id (referenced twice). This fragment MUST be applied to EVERY dm_messages read
// that returns content, last-message metadata, or counts to a requesting user —
// scroll fetches, conversation-list previews, pins, unread counts — or private
// removal is silently defeated.
//
// The pure predicate lives in internal/dmvisibility so WebSocket delivery can
// reuse it without pulling the purge engine's dependency graph. This wrapper
// preserves the established purge package API for its existing readers.
func HiddenRangeFilter(alias string, userParamPos int) string {
	return HiddenRangeFilterForViewerExpr(alias, fmt.Sprintf("$%d", userParamPos))
}

// HiddenRangeFilterForViewerExpr returns HiddenRangeFilter's provenance-aware
// predicate for a fixed SQL expression that identifies the viewer. Callers may
// supply only compile-time SQL identifiers/expressions (for example
// dm_participants.user_id); request data remains a query parameter.
func HiddenRangeFilterForViewerExpr(alias, viewerExpr string) string {
	return dmvisibility.HiddenRangeFilterForViewerExpr(alias, viewerExpr)
}
