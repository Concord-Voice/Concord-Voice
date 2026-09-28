package channels

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTextOnlyAuthorityScopesAreNonNilEmpty(t *testing.T) {
	groupID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	textState := groupedChannelState{ID: "text", GroupID: &groupID, SyncPermissions: true}

	_, voiceIDs := syncedGroupChannelIDs([]groupedChannelState{textState})
	assert.NotNil(t, voiceIDs, "text-only group delete must pass an empty voice scope, not nil-means-all")

	_, voiceIDs = authorityAffectedChannelIDs(ReorderChannelsRequest{Channels: []ChannelPosition{{ChannelID: "text"}}}, map[string]groupedChannelState{
		"text": textState,
	})
	assert.NotNil(t, voiceIDs, "text-only reorder must pass an empty voice scope, not nil-means-all")

	target := newSyncedChannelMoveTarget(channelAuthorityState{GroupID: &groupID, SyncPermissions: true}, "text", UpdateChannelRequest{
		Type:    "text",
		GroupID: nil,
	})
	assert.NotNil(t, target.voiceIDs, "text-only channel move must pass an empty voice scope, not nil-means-all")
}
