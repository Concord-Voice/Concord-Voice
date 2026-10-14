import type { ExpirationPolicy } from '../../services/messaging/expirationPolicyApi';

type PolicyState = 'loading' | 'ready' | 'unavailable';

export interface PinnedKeptNoteProps {
  policy?: ExpirationPolicy | null;
  policyState?: PolicyState;
}

/**
 * Whether the pinned panel says that pins are kept (#3459). The same discipline as the composer's
 * clause: pins are claimed as kept ONLY from a server-sent `includePinned === false` under a
 * window. An unreadable policy (`unavailable`), an absent field (a server that predates #3459, or
 * a claim the equal-revision merge dropped) and a policy with no timer all say nothing, because
 * the note is a retention promise and a promise cannot be built out of an unknown.
 *
 * `loading` keeps the last known policy rather than blanking, for the reason `expirationClause`
 * documents: every refresh, including the one a reconnect triggers, passes through `loading`.
 */
export function pinnedKeptNoteVisible(
  policy: ExpirationPolicy | null | undefined,
  policyState: PolicyState | undefined
): boolean {
  if (policyState === undefined || policyState === 'unavailable') return false;
  return typeof policy?.windowSeconds === 'number' && policy.includePinned === false;
}

/**
 * The one static note both pinned panels show. It exists because unpinning is the one action
 * that quietly changes a message's fate: a kept pin that is unpinned goes back on the timer, and
 * is deleted at the next sweep if its time has already run out. No per-pin control and no
 * countdown are offered (the server exposes no per-message expiry), so the sentence says it once.
 */
export default function PinnedKeptNote({ policy, policyState }: Readonly<PinnedKeptNoteProps>) {
  if (!pinnedKeptNoteVisible(policy, policyState)) return null;
  return (
    <p className="pinned-panel-note">
      Pinned messages are kept. Unpinning one puts it back on the timer, and it&apos;s deleted
      within minutes if its time has already run out.
    </p>
  );
}
