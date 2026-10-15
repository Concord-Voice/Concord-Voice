/**
 * Pure helpers for the per-message character limit (#3857), shared by the composer's counter and
 * the inline edit box. No React: the limit itself comes from `useMessageCharLimit`, which clamps
 * the live entitlement by tier, and every function here takes it as an argument.
 *
 * The limit is enforced only by the client. Under E2EE the server sees only ciphertext, so it
 * cannot measure the plaintext length, and a modified client can store a longer message. The
 * composer enforces it by sending the overflow as a .md attachment; the edit box has no such path
 * and refuses an over-limit save.
 */

/** Renderer-side DoS ceiling: input that would make a draft longer than this is discarded. */
export const DOS_PROTECTION_LIMIT = 1_048_576; // 1 MiB plaintext

/** Counter announcement thresholds (a11y U1): announce ONLY at 75% / 90% /
 *  at-limit — never per keystroke. */
const COUNTER_ANNOUNCE_RATIOS = [0.75, 0.9, 1] as const;
// Counter visibility and warn thresholds are expressed as ratios of the
// active limit, so the same 75%/95% behaviour holds at any tier
// (e.g. free 5120: 3840/5120 = 0.75, 4864/5120 = 0.95; premium 10240 scales).
const COUNTER_VISIBLE_RATIO = 0.75;
const COUNTER_WARN_RATIO = 0.95;

export type CounterLevel = 'normal' | 'warn' | 'error';

/** The counter appears once the length reaches 75% of the limit. */
export function isCounterVisible(charCount: number, limit: number): boolean {
  return charCount >= Math.floor(limit * COUNTER_VISIBLE_RATIO);
}

/** `error` strictly above the limit (a length AT the limit is still allowed), `warn` from 95%. */
export function getCounterLevel(charCount: number, limit: number): CounterLevel {
  if (charCount > limit) return 'error';
  if (charCount >= Math.floor(limit * COUNTER_WARN_RATIO)) return 'warn';
  return 'normal';
}

/** The composer counter's class list for `getCounterLevel`. */
export function getCounterClass(charCount: number, limit: number): string {
  const level = getCounterLevel(charCount, limit);
  if (level === 'error') return 'counter error';
  if (level === 'warn') return 'counter warn';
  return 'counter';
}

/** The composer's threshold-only live-region text: empty below 75%, otherwise a function of the
 *  highest band crossed, so it changes (and is re-announced) only on a band transition. */
export function getCounterAnnouncement(charCount: number, limit: number): string {
  const announceRatio = [...COUNTER_ANNOUNCE_RATIOS]
    .reverse()
    .find((ratio) => charCount >= Math.floor(limit * ratio));

  if (announceRatio === 1) {
    return `Message at the ${limit}-character limit. Longer messages send as a .md attachment.`;
  }
  if (announceRatio === 0.9) {
    return `Approaching the ${limit}-character limit.`;
  }
  if (announceRatio === 0.75) {
    return `Message has reached 75% of the ${limit}-character limit.`;
  }
  return '';
}

/** What the edit box can do with a draft. `trimmed` is the exact text a save would send, and
 *  `length` is measured on it, so the counter, the guard and the save can never disagree. */
export interface EditVerdict {
  trimmed: string;
  length: number;
  /** The trimmed draft is longer than the limit. A length exactly AT the limit is not over. */
  overLimit: boolean;
  /** Save may proceed: non-empty, different from the original, and not over the limit. */
  canSave: boolean;
}

/**
 * The one decision behind both the Save button and the Enter key. The limit and the change check
 * run on the trimmed draft, because the trimmed text is what is sent: comparing the raw draft would
 * enable Save for an edit that only adds trailing whitespace (the save then does nothing), and
 * would count trailing whitespace against the limit. Save also needs the raw draft to differ, so
 * opening a message whose stored text has leading or trailing whitespace does not enable Save.
 */
export function evaluateEdit(draft: string, original: string, limit: number): EditVerdict {
  const trimmed = draft.trim();
  const length = trimmed.length;
  const overLimit = length > limit;
  return {
    trimmed,
    length,
    overLimit,
    canSave: length > 0 && draft !== original && trimmed !== original && !overLimit,
  };
}

/** Why an over-limit edit cannot be saved. It reads the same for a draft the user grew and for
 *  an old message that was written under a higher limit (premium to free), which opens already
 *  over the limit. Edited text is never truncated, so the only way out is shortening it. */
export function getOverLimitReason(limit: number): string {
  return `This message is over your ${limit.toLocaleString('en-US')}-character limit. Shorten it to save.`;
}
