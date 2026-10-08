package dm

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/lib/pq"
)

// Range is a [From, To) hidden window for one participant. Legacy purge ranges
// hide only peer messages; Clear ranges also hide the actor's own messages.
type Range struct {
	From         time.Time
	To           time.Time
	IncludesOwn  bool
	infiniteFrom bool
}

// mergeRanges collapses overlapping or adjacent ranges into a minimal, sorted set. Pure.
func mergeRanges(rs []Range) []Range {
	if len(rs) <= 1 {
		return rs
	}
	sorted := make([]Range, len(rs))
	copy(sorted, rs)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].IncludesOwn != sorted[j].IncludesOwn {
			return !sorted[i].IncludesOwn
		}
		if sorted[i].infiniteFrom != sorted[j].infiniteFrom {
			return sorted[i].infiniteFrom
		}
		return sorted[i].From.Before(sorted[j].From)
	})

	out := []Range{sorted[0]}
	for _, r := range sorted[1:] {
		last := &out[len(out)-1]
		if r.IncludesOwn == last.IncludesOwn && (r.infiniteFrom || !r.From.After(last.To)) { // overlap or adjacency
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
// existing ranges for that conversation, and returns the count of OTHER participants' unpinned
// messages now covered by the newly-added window (for the audit hidden_count). Runs inside the
// caller's transaction so the merge + count are atomic. All values parameterized.
//
// A pinned message is never hidden (#3458 §18): the read filter exempts it, so the count excludes
// it and the window itself needs no holes.
func InsertHiddenRange(ctx context.Context, tx *sql.Tx, userID, convID string, from, to time.Time) (int, error) {
	return storeHiddenRange(ctx, tx, userID, convID, Range{From: from, To: to}, true)
}

// InsertClearRange records a [-infinity, cutoff) range that hides both the
// actor's and peers' messages. It deliberately returns no count: unlike a
// legacy purge range, Clear is an actor-local history mutation.
func InsertClearRange(ctx context.Context, tx *sql.Tx, userID, convID string, cutoff time.Time) error {
	_, err := storeHiddenRange(ctx, tx, userID, convID, Range{
		To: cutoff, IncludesOwn: true, infiniteFrom: true,
	}, false)
	return err
}

// storeHiddenRange serializes every actor/conversation range change on the
// participant row, including an empty range set. Callers lock the users and
// dm_conversations parent rows in that order before this participant lock.
func storeHiddenRange(ctx context.Context, tx *sql.Tx, userID, convID string, added Range, countPeers bool) (int, error) {
	// Re-take the conversation row first so the pin state the count reads is
	// frozen: DM pin and unpin lock it FOR NO KEY UPDATE (#3458). Callers
	// already hold it, so this adds no lock-order edge.
	var lockedConv string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM dm_conversations WHERE id = $1 FOR SHARE`, convID).Scan(&lockedConv); err != nil {
		return 0, fmt.Errorf("lock hidden-range conversation: %w", err)
	}

	var participantID string
	if err := tx.QueryRowContext(ctx,
		`SELECT user_id FROM dm_participants WHERE user_id = $1 AND conversation_id = $2 FOR UPDATE`,
		userID, convID).Scan(&participantID); err != nil {
		return 0, fmt.Errorf("lock hidden-range participant: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT hidden_from::text, hidden_to, includes_own FROM dm_message_hidden_ranges
		 WHERE user_id = $1 AND conversation_id = $2 FOR UPDATE`, userID, convID)
	if err != nil {
		return 0, fmt.Errorf("load hidden ranges: %w", err)
	}
	var existing []Range
	for rows.Next() {
		var r Range
		var from string
		if err := rows.Scan(&from, &r.To, &r.IncludesOwn); err != nil {
			if closeErr := rows.Close(); closeErr != nil {
				return 0, fmt.Errorf("scan hidden range: %w; close hidden ranges: %v", err, closeErr)
			}
			return 0, fmt.Errorf("scan hidden range: %w", err)
		}
		if from == "-infinity" {
			r.infiniteFrom = true
		} else if r.From, err = parseHiddenRangeTime(from); err != nil {
			if closeErr := rows.Close(); closeErr != nil {
				return 0, fmt.Errorf("parse hidden range start: %w; close hidden ranges: %v", err, closeErr)
			}
			return 0, fmt.Errorf("parse hidden range start: %w", err)
		}
		existing = append(existing, r)
	}
	if err := rows.Err(); err != nil {
		if closeErr := rows.Close(); closeErr != nil {
			return 0, fmt.Errorf("iterate hidden ranges: %w; close hidden ranges: %v", err, closeErr)
		}
		return 0, fmt.Errorf("iterate hidden ranges: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close hidden ranges: %w", err)
	}

	if countPeers && added.infiniteFrom {
		return 0, fmt.Errorf("legacy hidden range cannot have infinite lower bound")
	}
	merged := mergeRanges(append(existing, added))

	// Replace the user's ranges for this conversation with the merged set.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`,
		userID, convID); err != nil {
		return 0, fmt.Errorf("clear hidden ranges: %w", err)
	}
	if err := insertStoredRanges(ctx, tx, userID, convID, merged); err != nil {
		return 0, fmt.Errorf("insert hidden ranges: %w", err)
	}

	if !countPeers {
		return 0, nil
	}
	// Count OTHER participants' unpinned messages inside the just-added
	// window; a pinned message is never hidden (#3458 §18).
	var hidden int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM dm_messages
		 WHERE conversation_id = $1 AND user_id <> $2 AND created_at >= $3 AND created_at < $4
		   AND pinned_at IS NULL`,
		convID, userID, added.From, added.To).Scan(&hidden); err != nil {
		return 0, fmt.Errorf("count hidden messages: %w", err)
	}
	return hidden, nil
}

// insertStoredRanges writes rs in one statement. Bounds travel as
// microsecond-exact text, with -infinity literal, so they survive the
// round trip exactly rather than through the driver's nanosecond encoding.
func insertStoredRanges(ctx context.Context, tx *sql.Tx, userID, convID string, rs []Range) error {
	froms, tos, owns := make([]string, len(rs)), make([]string, len(rs)), make([]bool, len(rs))
	for i, r := range rs {
		froms[i] = "-infinity"
		if !r.infiniteFrom {
			froms[i] = microText(r.From)
		}
		tos[i], owns[i] = microText(r.To), r.IncludesOwn
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
		 SELECT $1, $2, f::timestamptz, t::timestamptz, o
		 FROM unnest($3::text[], $4::text[], $5::boolean[]) AS u(f, t, o)`,
		userID, convID, pq.Array(froms), pq.Array(tos), pq.Array(owns))
	return err
}

func microText(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
}

func parseHiddenRangeTime(value string) (time.Time, error) {
	parsed, err := pq.ParseTimestamp(time.UTC, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp: %w", err)
	}
	return parsed, nil
}

// hiddenRangeFilter returns the anti-join SQL fragment that excludes, from a dm_messages read,
// messages the requesting user has hidden (within the user's hidden ranges;
// each range records whether it also hides the actor's messages).
// The consuming query MUST alias dm_messages as `m` and pass the requesting user's id as the
// positional parameter $userParamPos (referenced twice). Applied to EVERY dm_messages content
// read path so the receiver-hide is not defeated by conversation-list previews, pins, or counts.
// The fragment itself is centralized in purge.HiddenRangeFilter (also consumed by the DM-pins
// read in internal/messages, which cannot import this package — dm imports messages).
func hiddenRangeFilter(userParamPos int) string {
	return purge.HiddenRangeFilter("m", userParamPos)
}
