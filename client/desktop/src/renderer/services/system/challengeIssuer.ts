import type { RuntimeServerSelection } from './runtimeServerBase';

/**
 * Which server issued each two-factor challenge token that MFAChallengeModal
 * answers. The token is a bearer credential for its challenge, so the email
 * send and the verify for it go to the server that issued it, and nowhere
 * else. The challenge store does not carry that server, and by the time the
 * modal first renders a token the selection may already have moved on, so the
 * code that raises the challenge records the selection it was raised under
 * before publishing it. A token with no record has no server its traffic may
 * go to, and the modal sends nothing for it.
 */

/**
 * How long the server keeps a challenge: `challengeTTL` in
 * services/control-plane/internal/mfa/challenge.go. An entry kept for a
 * challenge is no use after that, so it counts as absent and is pruned.
 */
export const CHALLENGE_TTL_MS = 5 * 60_000;

/**
 * Whether an entry recorded at `recordedAt` still belongs to a challenge the
 * server could hold. A clock that steps back keeps an entry longer.
 */
export function challengeEntryIsLive(recordedAt: number, now: number): boolean {
  return now - recordedAt < CHALLENGE_TTL_MS;
}

/** Deletes every entry older than the challenge TTL. */
export function pruneExpiredChallengeEntries<Entry extends { readonly recordedAt: number }>(
  entries: Map<string, Entry>,
  now: number
): void {
  for (const [token, entry] of entries) {
    if (!challengeEntryIsLive(entry.recordedAt, now)) entries.delete(token);
  }
}

interface IssuerEntry {
  readonly selection: RuntimeServerSelection;
  readonly recordedAt: number;
}

const issuers = new Map<string, IssuerEntry>();

/**
 * Records the selection a challenge was raised under. Call it before the
 * challenge is published, with the selection captured before the request
 * that returned the token was sent. The first record for a token wins: a later
 * call cannot point a live challenge at another server.
 */
export function recordChallengeIssuer(token: string, selection: RuntimeServerSelection): void {
  const now = Date.now();
  const existing = issuers.get(token);
  if (existing && challengeEntryIsLive(existing.recordedAt, now)) return;
  pruneExpiredChallengeEntries(issuers, now);
  issuers.set(token, { selection, recordedAt: now });
}

/**
 * The selection a challenge was raised under, or null when none was recorded
 * or the record has outlived the challenge. Null means nothing is sent for the
 * token. The caller still checks that the selection is current before sending.
 */
export function challengeIssuerFor(token: string | null): RuntimeServerSelection | null {
  if (!token) return null;
  const entry = issuers.get(token);
  if (!entry || !challengeEntryIsLive(entry.recordedAt, Date.now())) return null;
  return entry.selection;
}

/**
 * Forget every record. Exported for tests ONLY: tokens repeat across a file's
 * tests, and Vitest isolates modules per file rather than per test. Production
 * never calls it.
 */
export function __resetChallengeIssuersForTests(): void {
  issuers.clear();
}

/** How many records are kept, expired or not. Exported for tests ONLY. */
export function __challengeIssuerCountForTests(): number {
  return issuers.size;
}
