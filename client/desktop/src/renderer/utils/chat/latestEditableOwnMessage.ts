import type { MessageWithStatus } from '../../types/chat';

/**
 * Whether the current user can open this row's inline edit box (#1959). One rule for both
 * entry points — the composer's Up Arrow shortcut and the row's own Edit actions — so they
 * cannot disagree about which messages are editable.
 *
 * - **Own, ordinary message.** System rows (call events, expiration events) carry the actor's
 *   `user_id` but are never editable.
 * - **Server-acknowledged.** A `pending`, `sent` or `failed` row still has a client id.
 * - **Readable.** `decryptFailed` and `pendingKeys` rows have no plaintext to edit.
 * - **Has a body.** A live message is inserted as a fail-closed placeholder with `content: ''`
 *   before its decrypt settles (#1741), and nothing else marks it. Opening an edit there showed
 *   an empty box over a message the user could not see. Attachments and a GIF arrive with the
 *   row, so a message that has either is a real message with empty text and stays editable.
 *
 * Delete is gated separately (`canModify` in `Message.tsx`): an unreadable own message can
 * still be deleted.
 */
export function canEditMessage(message: MessageWithStatus, currentUserId: string): boolean {
  if (!currentUserId || message.user_id !== currentUserId) return false;
  if (message.type !== undefined && message.type !== 'user') return false;
  if (message.status !== undefined && message.status !== 'delivered') return false;
  if (message.decryptFailed || message.pendingKeys) return false;
  return (
    message.content !== '' || (message.attachments?.length ?? 0) > 0 || Boolean(message.gif_slug)
  );
}

/**
 * The id of the current user's NEWEST message, if that message can be edited (#1959).
 *
 * "Newest" is the last row by this user whose `type` is `'user'` or absent. If that newest row
 * is not editable (see `canEditMessage`) the answer is `null`, NOT the next older editable row.
 * Up Arrow means "edit what I just said"; skipping to an earlier message would silently open the
 * wrong one, so the shortcut stays a no-op until the newest row settles.
 *
 * `messages` is the store's order, oldest first. Only loaded rows are considered: a newest
 * message that pagination has not loaded yields `null`.
 */
export function latestEditableOwnMessage(
  messages: readonly MessageWithStatus[],
  currentUserId: string
): string | null {
  if (!currentUserId) return null;
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const message = messages[i];
    if (message.user_id !== currentUserId) continue;
    if (message.type !== undefined && message.type !== 'user') continue;
    return canEditMessage(message, currentUserId) ? message.id : null;
  }
  return null;
}
