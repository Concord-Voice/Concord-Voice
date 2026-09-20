package dm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tm(hour int) time.Time {
	return time.Date(2026, 7, 11, hour, 0, 0, 0, time.UTC)
}

func TestMergeRanges_OverlapCollapses(t *testing.T) {
	// [1,3) and [2,4) overlap → [1,4)
	got := mergeRanges([]Range{{From: tm(1), To: tm(3)}, {From: tm(2), To: tm(4)}})
	require.Len(t, got, 1)
	assert.Equal(t, tm(1), got[0].From)
	assert.Equal(t, tm(4), got[0].To)
}

func TestMergeRanges_AdjacencyCollapses(t *testing.T) {
	// [1,2) and [2,3) touch at 2 → [1,3)
	got := mergeRanges([]Range{{From: tm(1), To: tm(2)}, {From: tm(2), To: tm(3)}})
	require.Len(t, got, 1)
	assert.Equal(t, tm(1), got[0].From)
	assert.Equal(t, tm(3), got[0].To)
}

func TestMergeRanges_DisjointKept(t *testing.T) {
	// [1,2) and [5,6) do not touch → two ranges, sorted
	got := mergeRanges([]Range{{From: tm(5), To: tm(6)}, {From: tm(1), To: tm(2)}})
	require.Len(t, got, 2)
	assert.Equal(t, tm(1), got[0].From)
	assert.Equal(t, tm(5), got[1].From)
}

func TestMergeRanges_ContainedRangeAbsorbed(t *testing.T) {
	// [1,10) contains [3,5) → [1,10)
	got := mergeRanges([]Range{{From: tm(1), To: tm(10)}, {From: tm(3), To: tm(5)}})
	require.Len(t, got, 1)
	assert.Equal(t, tm(1), got[0].From)
	assert.Equal(t, tm(10), got[0].To)
}

func TestMergeRanges_EmptyAndSingle(t *testing.T) {
	assert.Empty(t, mergeRanges(nil))
	one := []Range{{From: tm(1), To: tm(2)}}
	assert.Equal(t, one, mergeRanges(one))
}

func TestMergeRanges_MergesSameProvenanceAcrossInterleavedRange(t *testing.T) {
	base := tm(1)
	got := mergeRanges([]Range{
		{From: base, To: base.Add(100 * time.Minute)},
		{From: base.Add(10 * time.Minute), To: base.Add(20 * time.Minute), IncludesOwn: true},
		{From: base.Add(30 * time.Minute), To: base.Add(40 * time.Minute)},
	})
	require.Len(t, got, 2)
	assert.False(t, got[0].IncludesOwn)
	assert.Equal(t, base, got[0].From)
	assert.Equal(t, base.Add(100*time.Minute), got[0].To)
	assert.True(t, got[1].IncludesOwn)
}

func TestMergeRanges_RepeatedClearRangesCoalesce(t *testing.T) {
	got := mergeRanges([]Range{
		{To: tm(3), IncludesOwn: true, infiniteFrom: true},
		{From: tm(1), To: tm(5), IncludesOwn: true},
		{From: tm(4), To: tm(6), IncludesOwn: true},
	})
	require.Len(t, got, 1)
	assert.True(t, got[0].IncludesOwn)
	assert.True(t, got[0].infiniteFrom)
	assert.Equal(t, tm(6), got[0].To)
}

func TestHiddenRangeFilter_ReferencesParamTwice(t *testing.T) {
	frag := hiddenRangeFilter(4)
	assert.Contains(t, frag, "$4")
	assert.Contains(t, frag, "m.user_id <> $4")
	assert.Contains(t, frag, "dm_message_hidden_ranges hr")
}
