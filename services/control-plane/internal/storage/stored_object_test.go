package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// storedObjectBefore is the orphan reaper's write-race margin, expressed as a
// decision rather than as a filter buried in an SDK loop.
//
// BOTH attachment write paths put the OBJECT down before inserting its ROW, so
// between those two steps a perfectly healthy upload is indistinguishable from
// an orphan. Getting this boundary wrong in the permissive direction deletes an
// attachment out from under the request that is creating it.
func TestStoredObjectBefore(t *testing.T) {
	cutoff := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	t.Run("older than the cutoff is a candidate", func(t *testing.T) {
		obj, ok := storedObjectBefore("attachments/a", 42, cutoff.Add(-time.Second), cutoff)
		assert.True(t, ok)
		assert.Equal(t, "attachments/a", obj.Key)
		assert.Equal(t, int64(42), obj.Size, "size is carried through, not dropped")
		assert.Equal(t, cutoff.Add(-time.Second), obj.LastModified)
	})

	t.Run("exactly at the cutoff is NOT a candidate", func(t *testing.T) {
		_, ok := storedObjectBefore("attachments/b", 1, cutoff, cutoff)
		assert.False(t, ok,
			"the boundary is exclusive: at the margin, waiting another interval is the safe answer")
	})

	t.Run("newer than the cutoff is not a candidate", func(t *testing.T) {
		_, ok := storedObjectBefore("attachments/c", 1, cutoff.Add(time.Second), cutoff)
		assert.False(t, ok, "an object inside the write-race window may have a row moments away")
	})

	t.Run("a MISSING timestamp is rejected, not treated as ancient", func(t *testing.T) {
		_, ok := storedObjectBefore("attachments/d", 0, time.Time{}, cutoff)
		assert.False(t, ok,
			"time.Time{} predates every cutoff, so a bare Before test would admit an object "+
				"carrying no LastModified at all -- bypassing the write-race margin entirely, "+
				"on a delete path, for the input that tells us least")
	})
}
