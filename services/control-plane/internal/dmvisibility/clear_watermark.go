package dmvisibility

import (
	"database/sql"
	"errors"
	"time"
)

// ClearWatermarkLateral is the single definition of the clear watermark W
// (#3462). It is a LATERAL body over the outer alias c (dm_conversations): W is
// the MIN, over CURRENT participants, of each participant's latest Clear cutoff.
// Only Clear ranges count — includes_own with a -infinity lower bound — so
// legacy purge ranges and hidden_at never contribute, and a departed member's
// leftover ranges are ignored. `uncleared` counts participants with no Clear
// range, because MIN() skips their NULLs. The purge engine and discovery both
// consume this constant, so the two cannot diverge.
const ClearWatermarkLateral = `SELECT COUNT(p.user_id)::int AS participants,
       COUNT(p.user_id) FILTER (WHERE cr.hidden_to IS NULL)::int AS uncleared,
       MIN(cr.hidden_to) AS watermark
  FROM dm_participants p
  LEFT JOIN LATERAL (
    SELECT MAX(hr.hidden_to) AS hidden_to FROM dm_message_hidden_ranges hr
     WHERE hr.user_id = p.user_id AND hr.conversation_id = p.conversation_id
       AND hr.includes_own AND hr.hidden_from = '-infinity'::timestamptz) cr ON TRUE
 WHERE p.conversation_id = c.id`

// ClearWatermark is W. Unbounded means the conversation has no current
// participant, so no one can read any of its messages.
type ClearWatermark struct {
	At        time.Time
	Unbounded bool
}

// DecideClearWatermark turns the fragment's columns into W. It fails closed: an
// input it cannot explain is an error, and the caller deletes nothing.
func DecideClearWatermark(participants, uncleared int, wm sql.NullTime) (ClearWatermark, bool, error) {
	switch {
	case participants < 0 || uncleared < 0 || uncleared > participants:
		return ClearWatermark{}, false, errors.New("dmvisibility: clear watermark counts out of range")
	case participants == 0:
		return ClearWatermark{Unbounded: true}, true, nil
	case uncleared > 0:
		return ClearWatermark{}, false, nil
	case !wm.Valid:
		return ClearWatermark{}, false, errors.New("dmvisibility: every participant cleared but the watermark is NULL")
	default:
		return ClearWatermark{At: wm.Time}, true, nil
	}
}
