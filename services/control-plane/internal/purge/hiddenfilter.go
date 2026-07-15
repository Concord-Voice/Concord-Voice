package purge

import "fmt"

// HiddenRangeFilter returns the anti-join SQL fragment that excludes, from a
// dm_messages read, messages the requesting user has hidden via the receiver-hide
// (#1352): messages authored by OTHERS whose created_at falls inside one of the
// user's dm_message_hidden_ranges for that conversation.
//
// alias is the dm_messages table alias used by the consuming query (e.g. "m" or
// "dm"); userParamPos is the positional parameter carrying the requesting user's
// id (referenced twice). This fragment MUST be applied to EVERY dm_messages read
// that returns content, last-message metadata, or counts to a requesting user —
// scroll fetches, conversation-list previews, pins, unread counts — or the
// receiver-hide is silently defeated (spec §7 / review finding M3).
//
// It lives in the purge package (not dm) because internal/messages also serves
// dm_messages content (DM pins) and dm imports messages — importing dm from
// messages would cycle.
func HiddenRangeFilter(alias string, userParamPos int) string {
	return fmt.Sprintf(` AND NOT EXISTS (
  SELECT 1 FROM dm_message_hidden_ranges hr
  WHERE hr.user_id = $%[2]d AND hr.conversation_id = %[1]s.conversation_id
    AND %[1]s.created_at >= hr.hidden_from AND %[1]s.created_at < hr.hidden_to
    AND %[1]s.user_id <> $%[2]d)`, alias, userParamPos)
}
