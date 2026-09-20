package dmvisibility

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHiddenRangeFilterForViewerExpr_UsesCallerAndViewer(t *testing.T) {
	fragment := HiddenRangeFilterForViewerExpr("message", "participant.user_id")

	assert.True(t, strings.HasPrefix(fragment, " AND NOT EXISTS"))
	assert.Contains(t, fragment, "hr.user_id = participant.user_id")
	assert.Contains(t, fragment, "hr.conversation_id = message.conversation_id")
	assert.Contains(t, fragment, "message.created_at >= hr.hidden_from")
	assert.Contains(t, fragment, "message.created_at < hr.hidden_to")
	assert.Contains(t, fragment, "hr.includes_own OR message.user_id <> participant.user_id")
}
