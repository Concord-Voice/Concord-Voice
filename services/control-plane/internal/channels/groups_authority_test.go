package channels

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroupIDsForReorderDeduplicatesAndSortsSourceAndDestination(t *testing.T) {
	source, destination, ungrouped := "00000000-0000-0000-0000-000000000002", "00000000-0000-0000-0000-000000000001", ""
	got := groupIDsForReorder(ReorderChannelsRequest{Channels: []ChannelPosition{
		{ChannelID: "channel-a", GroupID: &source},
		{ChannelID: "channel-b", GroupID: &destination},
		{ChannelID: "channel-c", GroupID: &ungrouped},
	}}, map[string]groupedChannelState{
		"channel-a": {ID: "channel-a", GroupID: &source},
		"channel-b": {ID: "channel-b", GroupID: &source},
		"channel-c": {ID: "channel-c", GroupID: &source},
	})
	require.Equal(t, []string{destination, source}, got)
}

func TestFilterDeletedReorderedChannelsSuppressesOnlyDeletedChannels(t *testing.T) {
	deleted := map[string]struct{}{"channel-a": {}}
	got := filterDeletedReorderedChannels([]ChannelPosition{
		{ChannelID: "channel-a", Position: 1},
		{ChannelID: "channel-b", Position: 2},
	}, deleted)
	assert.Equal(t, []ChannelPosition{{ChannelID: "channel-b", Position: 2}}, got)
}
