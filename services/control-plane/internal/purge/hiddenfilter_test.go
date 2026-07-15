package purge

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The receiver-hide anti-join is concatenated into several dm_messages reads across
// two packages, so its shape is locked here at the source (#1352).
func TestHiddenRangeFilter_BindsAliasAndParamPosition(t *testing.T) {
	frag := HiddenRangeFilter("m", 4)

	assert.Contains(t, frag, "dm_message_hidden_ranges hr")
	assert.Contains(t, frag, "hr.user_id = $4", "viewer id bound at the requested position")
	assert.Contains(t, frag, "m.conversation_id", "correlates on the caller's alias")
	assert.Contains(t, frag, "m.user_id <> $4", "never hides the requester's own messages")
	assert.True(t, strings.HasPrefix(frag, " AND NOT EXISTS"), "appends to an existing WHERE")
}

func TestHiddenRangeFilter_HonoursCallerAlias(t *testing.T) {
	// The DM-pins read aliases dm_messages as "dm", not "m".
	frag := HiddenRangeFilter("dm", 2)

	assert.Contains(t, frag, "dm.conversation_id")
	assert.Contains(t, frag, "dm.user_id <> $2")
	assert.NotContains(t, frag, "m.conversation_id = ", "must not hardcode the default alias")
}

// The window is half-open [from, to): a message exactly at hidden_to is NOT hidden,
// so a later message cannot be swallowed by an earlier purge's upper bound.
func TestHiddenRangeFilter_UsesHalfOpenWindow(t *testing.T) {
	frag := HiddenRangeFilter("m", 1)

	assert.Contains(t, frag, "m.created_at >= hr.hidden_from")
	assert.Contains(t, frag, "m.created_at < hr.hidden_to")
}
