package dm

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/purge"
)

// Range is a [From, To) hidden window for the DM receiver-hide (#1352). A participant who
// purges a 1:1/group conversation can hard-delete only messages they authored; messages they
// cannot delete-for-both are hidden from THEIR OWN view via these persistent ranges. The other
// participant(s) are unaffected. Range-based (one row per hide op) keeps this O(1), not O(N).
type Range struct {
	From time.Time
	To   time.Time
}

// mergeRanges collapses overlapping or adjacent ranges into a minimal, sorted set. Pure.
func mergeRanges(rs []Range) []Range {
	if len(rs) <= 1 {
		return rs
	}
	sorted := make([]Range, len(rs))
	copy(sorted, rs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].From.Before(sorted[j].From) })

	out := []Range{sorted[0]}
	for _, r := range sorted[1:] {
		last := &out[len(out)-1]
		if !r.From.After(last.To) { // r.From <= last.To → overlap or adjacency
			if r.To.After(last.To) {
				last.To = r.To
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// InsertHiddenRange records a hidden window for (userID, convID), merging it with the user's
// existing ranges for that conversation, and returns the count of OTHER participants' messages
// now covered by the newly-added window (for the audit hidden_count). Runs inside the caller's
// transaction so the merge + count are atomic. All values parameterized.
func InsertHiddenRange(ctx context.Context, tx *sql.Tx, userID, convID string, from, to time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT hidden_from, hidden_to FROM dm_message_hidden_ranges
		 WHERE user_id = $1 AND conversation_id = $2 FOR UPDATE`, userID, convID)
	if err != nil {
		return 0, fmt.Errorf("load hidden ranges: %w", err)
	}
	var existing []Range
	for rows.Next() {
		var r Range
		if err := rows.Scan(&r.From, &r.To); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan hidden range: %w", err)
		}
		existing = append(existing, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate hidden ranges: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close hidden ranges: %w", err)
	}

	merged := mergeRanges(append(existing, Range{From: from, To: to}))

	// Replace the user's ranges for this conversation with the merged set.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`,
		userID, convID); err != nil {
		return 0, fmt.Errorf("clear hidden ranges: %w", err)
	}
	for _, r := range merged {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to)
			 VALUES ($1, $2, $3, $4)`, userID, convID, r.From, r.To); err != nil {
			return 0, fmt.Errorf("insert hidden range: %w", err)
		}
	}

	// Count OTHER participants' messages now inside the just-added window.
	var hidden int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM dm_messages
		 WHERE conversation_id = $1 AND user_id <> $2 AND created_at >= $3 AND created_at < $4`,
		convID, userID, from, to).Scan(&hidden); err != nil {
		return 0, fmt.Errorf("count hidden messages: %w", err)
	}
	return hidden, nil
}

// hiddenRangeFilter returns the anti-join SQL fragment that excludes, from a dm_messages read,
// messages the requesting user has hidden (authored by others, within the user's hidden ranges).
// The consuming query MUST alias dm_messages as `m` and pass the requesting user's id as the
// positional parameter $userParamPos (referenced twice). Applied to EVERY dm_messages content
// read path so the receiver-hide is not defeated by conversation-list previews, pins, or counts.
// The fragment itself is centralized in purge.HiddenRangeFilter (also consumed by the DM-pins
// read in internal/messages, which cannot import this package — dm imports messages).
func hiddenRangeFilter(userParamPos int) string {
	return purge.HiddenRangeFilter("m", userParamPos)
}
