package channels

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/stretchr/testify/assert"
)

func TestSameChannelIDSetRejectsDifferentLengthsAndValues(t *testing.T) {
	assert.False(t, sameChannelIDSet([]string{"a"}, nil))
	assert.False(t, sameChannelIDSet([]string{"a"}, []string{"b"}))
	assert.True(t, sameChannelIDSet([]string{"a", "b"}, []string{"a", "b"}))
}

func TestAuthorityRotationDeletedChannelIDsFlattensRotationResults(t *testing.T) {
	deleted := authorityRotationDeletedChannelIDs([]keyrotation.Rotation{
		{DeletedChannelIDs: []string{"channel-a", "channel-b"}},
		{DeletedChannelIDs: []string{"channel-b", "channel-c"}},
	})
	assert.Equal(t, map[string]struct{}{
		"channel-a": {}, "channel-b": {}, "channel-c": {},
	}, deleted)
}

func TestContainsChannelUserMatchesOnlyExactID(t *testing.T) {
	assert.True(t, containsChannelUser([]string{"user-a", "user-b"}, "user-b"))
	assert.False(t, containsChannelUser([]string{"user-a", "user-b"}, "user"))
}
