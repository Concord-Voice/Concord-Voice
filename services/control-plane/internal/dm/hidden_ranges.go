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
// existing ranges for that conversation, and returns the count of OTHER participants' messages
// now covered by the newly-added window (for the audit hidden_count). Runs inside the caller's
// transaction so the merge + count are atomic. All values parameterized.
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

	merged := mergeRanges(append(existing, added))

	// Replace the user's ranges for this conversation with the merged set.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`,
		userID, convID); err != nil {
		return 0, fmt.Errorf("clear hidden ranges: %w", err)
	}
	for _, r := range merged {
		if err := insertStoredRange(ctx, tx, userID, convID, r); err != nil {
			return 0, fmt.Errorf("insert hidden range: %w", err)
		}
	}

	if !countPeers {
		return 0, nil
	}
	if added.infiniteFrom {
		return 0, fmt.Errorf("legacy hidden range cannot have infinite lower bound")
	}
	// Count OTHER participants' messages now inside the just-added window.
	var hidden int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM dm_messages
		 WHERE conversation_id = $1 AND user_id <> $2 AND created_at >= $3 AND created_at < $4`,
		convID, userID, added.From, added.To).Scan(&hidden); err != nil {
		return 0, fmt.Errorf("count hidden messages: %w", err)
	}
	return hidden, nil
}

func insertStoredRange(ctx context.Context, tx *sql.Tx, userID, convID string, r Range) error {
	if r.infiniteFrom {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
			 VALUES ($1, $2, '-infinity'::timestamptz, $3, $4)`, userID, convID, r.To, r.IncludesOwn)
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
		 VALUES ($1, $2, $3, $4, $5)`, userID, convID, r.From, r.To, r.IncludesOwn)
	return err
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
