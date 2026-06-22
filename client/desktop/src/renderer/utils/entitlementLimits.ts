export const FREE_MESSAGE_CHARS = 5120;
export const PREMIUM_MESSAGE_CHARS = 10240;
export const PREMIUM_ATTACHMENT_BYTES = 536_870_912;

export function clampMessageCharsForTier(tier: string, value: number): number {
  const ceiling = tier === 'premium' ? PREMIUM_MESSAGE_CHARS : FREE_MESSAGE_CHARS;
  if (!Number.isFinite(value)) return ceiling;
  return Math.max(1, Math.min(Math.trunc(value), ceiling));
}
