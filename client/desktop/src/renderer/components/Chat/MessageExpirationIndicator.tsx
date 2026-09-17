import {
  EXPIRATION_WINDOW_LABELS,
  type ExpirationPolicy,
  type ExpirationWindowSeconds,
} from '../../services/messaging/expirationPolicyApi';

/** The retention half of the composer's status sentence.
 *
 *  Encryption and retention are now ONE sentence — "Messages are Encrypted End-to-End and
 *  expire after 24 hours" — rather than two stacked rows. Two short strings on two full-width
 *  rows spent vertical budget in the composer to leave most of each line empty, and read as
 *  two unrelated notices when they are one fact about what happens to what you are about to
 *  send. This function returns only the trailing clause; `MessageInput` owns the sentence so
 *  there is exactly one place the wording lives.
 *
 *  Returning a STRING rather than a node is what preserves the single-text-node property the
 *  indicator has always had: the caller interpolates it into one template literal, so the
 *  sentence reaches the DOM as one text node and `textContent` reads as a person would say it.
 *
 *  `null` means "say nothing about retention", and it is the fail-closed answer, not a
 *  cosmetic one. `unavailable` means the policy could not be read — NOT that messages are
 *  kept forever — so claiming "and never expire" there would manufacture a peace-of-mind
 *  guarantee out of a failed lookup. Silence is the only honest output for an unknown.
 *
 *  `loading` deliberately keeps the last known policy instead of blanking: `onRefresh` sets
 *  `loading` before EVERY fetch including the one `connection-recovered` triggers, so a
 *  state-gated clause would blink the retention claim out and back on every reconnect.
 */
export function expirationClause(
  policy: ExpirationPolicy | null,
  policyState: 'loading' | 'ready' | 'unavailable'
): string | null {
  if (policyState === 'unavailable') return null;
  if (!policy) return null;

  if (typeof policy.windowSeconds !== 'number') {
    return ' and never expire';
  }

  const label = EXPIRATION_WINDOW_LABELS[policy.windowSeconds as ExpirationWindowSeconds];
  if (!label) return null;

  // `backfillPending` is additive rather than a separate row for the same reason the two
  // rows merged: it is a qualifier on the retention claim, not an independent fact.
  return policy.backfillPending
    ? ` and expire after ${label} · still processing older messages`
    : ` and expire after ${label}`;
}

/** The header control's accessible name, and whether its glyph should read as lit.
 *
 *  This lives beside `expirationClause` because it answers the same question about the same
 *  policy, and the two MUST agree. They did not: the composer clause fail-closed on
 *  `unavailable` while both chat headers computed their own label from
 *  `policy?.windowSeconds ?? null` with no state check at all, so a failed policy read
 *  produced the accessible name "Message expiration: off". That is the manufactured
 *  guarantee `expirationClause` exists to refuse, in the sibling control added by the same
 *  change — and worse than the clause's version, because "off" is an AFFIRMATIVE claim that
 *  nothing is in force rather than a claim of permanence.
 *
 *  "off" is therefore only said when the server actually said so — `ready` with no window.
 *  An unknown degrades to the bare noun, which names the control without describing state.
 *
 *  `lit` is returned alongside rather than derived by the caller so the colour cannot
 *  disagree with the text. It previously could: a stale cached policy with a window lit the
 *  glyph while the name beside it read "off".
 */
export function expirationControlLabel(
  policy: ExpirationPolicy | null,
  policyState: 'loading' | 'ready' | 'unavailable'
): { label: string; lit: boolean } {
  const BARE = { label: 'Message expiration', lit: false };
  if (policyState === 'unavailable') return BARE;

  const windowSeconds = policy?.windowSeconds;
  if (typeof windowSeconds !== 'number') {
    // A missing window is "off" only once a successful read has said so. While loading with
    // nothing cached we know nothing, which is not the same as knowing it is off.
    return policyState === 'ready' ? { label: 'Message expiration: off', lit: false } : BARE;
  }

  const label = EXPIRATION_WINDOW_LABELS[windowSeconds as ExpirationWindowSeconds];
  // An unrecognised window is a version skew, not an "off" — same reasoning as the clause's
  // null return: say the control's name and nothing about its state.
  return label ? { label: `Message expiration: ${label}`, lit: true } : BARE;
}
