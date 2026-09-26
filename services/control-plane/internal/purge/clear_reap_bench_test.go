//go:build integration

package purge

import (
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMeasureClearReapBatchWallTime is the spec §12.6 measurement that fixes
// ClearReapMaxBatch (#3462 D5): keep 1000 only if p99 batch wall time is at or
// under 250 ms, because each batch holds the conversation lock and stalls
// sends. Opt-in: it seeds 10,000 messages and is not a regression test.
func TestMeasureClearReapBatchWallTime(t *testing.T) {
	if os.Getenv("CONCORD_CLEAR_REAP_BENCH") != "1" {
		t.Skip("measurement only: set CONCORD_CLEAR_REAP_BENCH=1")
	}
	f := seedClearReapConversation(t, true, false, 2)
	author := f.members[0]

	_, err := f.db.Exec(`
		INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at)
		SELECT $1, $2, 'bench', 'user', $3::timestamptz + g * interval '1 millisecond'
		  FROM generate_series(1, 10000) g`, f.conversationID, author, f.base)
	require.NoError(t, err)
	// Every fifth message carries its own tier-2 attachment (20%).
	_, err = f.db.Exec(`
		WITH picked AS (
		  SELECT id AS message_id, gen_random_uuid() AS file_id
		    FROM (SELECT id, row_number() OVER (ORDER BY created_at, id) AS rn
		            FROM dm_messages WHERE conversation_id = $1) m
		   WHERE rn % 5 = 0),
		files AS (
		  INSERT INTO media_files (id, uploader_id, file_type, media_tier, key_version,
		                           conversation_id, mime_type, file_size, storage_key)
		  SELECT file_id, $2, 'file', 2, 1, $1, 'application/octet-stream', 1,
		         'attachments/' || file_id::text
		    FROM picked RETURNING id)
		INSERT INTO dm_message_attachments (message_id, file_id, position)
		SELECT message_id, file_id, 0 FROM picked`, f.conversationID, author)
	require.NoError(t, err)
	for _, member := range f.members {
		f.clear(t, member, time.Now().UTC())
	}

	e := f.newEngine(ClearReapMaxBatch)
	var durations []time.Duration
	deleted := 0
	for {
		start := time.Now()
		res := f.reap(t, e)
		durations = append(durations, time.Since(start))
		deleted += res.DeletedCount
		if !res.More {
			break
		}
	}
	require.Equal(t, 10000, deleted)
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p := func(q float64) time.Duration { return durations[int(q*float64(len(durations)-1))] }
	t.Logf("clear reap batch wall time: stride=%d batches=%d p50=%s p99=%s max=%s",
		ClearReapMaxBatch, len(durations), p(0.5), p(0.99), durations[len(durations)-1])
}
