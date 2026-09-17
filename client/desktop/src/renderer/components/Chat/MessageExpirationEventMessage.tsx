import { Timer } from 'lucide-react';
import {
  EXPIRATION_WINDOW_LABELS,
  type ExpirationWindowSeconds,
} from '../../services/messaging/expirationPolicyApi';
import './messageExpirationEvent.css';

export type ExpirationEventKind = 'set' | 'changed' | 'cleared';

export interface MessageExpirationEventMessageProps {
  kind: ExpirationEventKind;
  /** Null iff kind === 'cleared'. The server owns that invariant; this component renders
   *  whatever it is handed rather than asserting it, so a version skew degrades to slightly
   *  odd copy instead of a blank row. */
  windowSeconds: ExpirationWindowSeconds | null;
  /** The acting user's display name, already resolved by the caller from the row's user JOIN.
   *  Undefined renders as "someone" — a row whose actor was erased is still a true record of
   *  what happened, so it must not be dropped.
   *
   *  ATTACKER-CONTROLLED. `validateDisplayName` (users/handlers.go) enforces a 100-character
   *  cap and nothing else — no charset, no control characters, no bidi overrides — so treat
   *  this as hostile input. It is sanitized below before it reaches the DOM. */
  actorName?: string;
  isSelf: boolean;
}

/** Bidi overrides, isolates and directional marks. These are FORMATTING, not content: they
 *  reorder the text around them without contributing a glyph. Removed outright rather than
 *  replaced, because a legitimate RTL name may carry one BETWEEN its letters and turning it
 *  into a space would split that name in two. */
const BIDI_MARKS = /[\u202A-\u202E\u2066-\u2069\u200E\u200F]/g;

/** C0/C1 controls (so tab and newline too) plus the Unicode line and paragraph separators.
 *  These SEPARATE, so each becomes a space and is then collapsed — stripping them outright
 *  would fuse the words on either side, turning "Alice\nBob" into "AliceBob". */
const SEPARATORS = /[\u0000-\u001F\u007F-\u009F\u2028\u2029]/g;

/** A display name is a label, never a sentence. Drop what could restructure the row, fold
 *  separators into single spaces, and clamp the length so a name cannot crowd out the fact
 *  beside it. An empty result degrades to the same fallback as a missing name. */
export function sanitizeActorName(name: string | undefined): string | undefined {
  if (!name) return undefined;
  const cleaned = name.replace(BIDI_MARKS, '').replace(SEPARATORS, ' ').replace(/\s+/g, ' ').trim();
  if (!cleaned) return undefined;
  return cleaned.length > 40 ? `${cleaned.slice(0, 40)}…` : cleaned;
}

/** The server's retention fact, stated on its own with no attribution in it. */
function statement(
  kind: ExpirationEventKind,
  windowSeconds: ExpirationWindowSeconds | null
): string {
  if (kind === 'cleared' || windowSeconds === null) {
    return 'Message expiration turned off';
  }
  return `Messages now expire after ${EXPIRATION_WINDOW_LABELS[windowSeconds]}`;
}

/** A durable, attributed record that the policy changed — the counterpart to the composer
 *  indicator, which says only what the policy IS now.
 *
 *  Geometry deliberately matches CallEventMessage so the two system-row types read as one
 *  family. The colour deliberately does NOT: CallEventMessage varies between --success and
 *  --text-muted because a call either connected or did not, and a policy change has no such
 *  axis. Borrowing its green would imply a "good outcome" relationship that does not exist.
 *
 *  Non-interactive, like every system row. It is history, not a control — the entry points
 *  for changing the policy are the header button and the conversation context menu.
 *
 *  THE SERVER'S FACT LEADS, IN ITS OWN ELEMENT. This row used to read
 *  `${actor} set messages to expire after 1 hour` as a single text node with the actor
 *  first. A display name is attacker-controlled and capped only at 100 characters, and in a
 *  1:1 DM EITHER participant may change the policy, so a peer could name themselves
 *  `Expiration is OFF for this chat. Nothing is deleted. (ignore: "` and mint the row at
 *  will — producing a trusted-looking system notice that opened with their sentence while
 *  the true retention fact trailed behind as apparent quoted noise. Proven with a working
 *  exploit during review of this PR.
 *
 *  The fix is structural, not filtering: the fact is rendered FIRST in its own node so no
 *  prefix can precede or outrank it, and the attribution follows inside a `<bdi>` so a name
 *  cannot reorder the text around it. `sanitizeActorName` is defence in depth on top of
 *  that ordering, never the load-bearing control — a filter is only ever a denylist.
 */
export default function MessageExpirationEventMessage({
  kind,
  windowSeconds,
  actorName,
  isSelf,
}: Readonly<MessageExpirationEventMessageProps>) {
  const actor = isSelf ? 'you' : (sanitizeActorName(actorName) ?? 'someone');
  return (
    <div className="expiration-event-message">
      <Timer size={16} className="expiration-event-message__icon" aria-hidden="true" />
      <span className="expiration-event-message__fact">{statement(kind, windowSeconds)}</span>
      <span className="expiration-event-message__actor">
        {' · set by '}
        <bdi>{actor}</bdi>
      </span>
    </div>
  );
}
