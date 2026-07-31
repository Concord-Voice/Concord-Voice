/**
 * A rolling token bucket over approval CEREMONIES SHOWN — never over probes
 * attempted. Re-probing an already-approved origin shows no dialog and must stay
 * unthrottled; only the modal itself is the scarce resource being rationed, since
 * a burst of them is what turns a compromised renderer into a click-fatigue lever
 * against the trust decision.
 *
 * 5 per rolling 10 minutes: a genuine self-hoster types one address and answers
 * once, and even the pathological honest case — several servers, each mistyped
 * a couple of times — stays under it, while an attacker gets no useful volume.
 *
 * In-memory only, deliberately. This bounds a burst WITHIN one app session. A
 * compromised renderer can reset it by invoking `app:relaunch` (preload
 * `relaunchApp()`), so it is NOT a cross-restart bound — accepted, because a
 * relaunch is user-visible and costs the attacker its own execution context.
 * A state file would add an IO failure mode to a path whose purpose is to stay
 * available.
 */
export const CEREMONY_BUDGET = 5;
export const CEREMONY_WINDOW_MS = 10 * 60 * 1000;

let shownAt: number[] = [];

/** Records one ceremony and returns true, or returns false without recording. */
export function consumeCeremonyToken(now: number = Date.now()): boolean {
  shownAt = shownAt.filter((t) => now - t < CEREMONY_WINDOW_MS);
  if (shownAt.length >= CEREMONY_BUDGET) return false;
  shownAt.push(now);
  return true;
}

export function _resetCeremonyBudgetForTesting(): void {
  shownAt = [];
}
