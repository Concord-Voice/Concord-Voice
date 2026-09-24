/**
 * Mic FEC headroom, extracted from VoiceService.calculateFecBitrate (#2153).
 *
 * Pure and import-free, on purpose: the media plane's audioPolicyParity.test.ts
 * imports it across workspaces and pins the policer's FEC_HEADROOM to
 * 1 + FEC_MAX_HEADROOM_PERCENT / 100. Raising this cap without the server
 * would let a stock client's loss-driven headroom exceed the policed audio
 * limit, and the policer would pause it.
 */
export const FEC_MAX_HEADROOM_PERCENT = 50;

/** Calculate FEC headroom bitrate multiplier based on loss % and tier. */
export function calculateFecBitrate(
  lossPercent: number,
  tierMaxBitrate: number,
  effectiveHeadroom: boolean
): number {
  if (!effectiveHeadroom || lossPercent <= 0) return tierMaxBitrate;

  let K: number;
  if (tierMaxBitrate < 64_000) K = 4;
  else if (tierMaxBitrate < 128_000) K = 2.5;
  else K = 1.5;
  const headroomPercent = Math.min(FEC_MAX_HEADROOM_PERCENT, lossPercent * K);
  return Math.round(tierMaxBitrate * (1 + headroomPercent / 100));
}
