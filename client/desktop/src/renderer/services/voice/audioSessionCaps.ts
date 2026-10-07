import { FREE_ENTITLEMENT, type Entitlement } from '../../stores/auth/subscriptionStore';
import { AUDIO_QUALITY_TIERS, type AudioQualityTier } from '../../stores/voice/audioQualityTiers';

export interface SessionAudioCaps {
  allowedTiers: AudioQualityTier[];
  minPtimeMs: number;
  /** A fixed channel standard may grant audio quality above the personal tier. */
  channelUpliftTier: AudioQualityTier | null;
}

const AUDIO_TIERS = Object.keys(AUDIO_QUALITY_TIERS) as AudioQualityTier[];
const FREE_TIERS = FREE_ENTITLEMENT.allowedAudioTiers as AudioQualityTier[];

function knownTiers(value: unknown): AudioQualityTier[] | null {
  if (!Array.isArray(value) || value.length === 0) return null;
  if (
    !value.every(
      (tier): tier is AudioQualityTier =>
        typeof tier === 'string' && Object.hasOwn(AUDIO_QUALITY_TIERS, tier)
    )
  )
    return null;
  return value;
}

function validPtime(value: unknown): number | null {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 10 && value <= 60
    ? value
    : null;
}

function admittedChannelUpliftTier(
  admitted: unknown,
  joinFallback: AudioQualityTier | null
): AudioQualityTier | null {
  // Absent is an older SFU; explicit null from A2 clears the earlier REST grant.
  if (admitted === undefined) return joinFallback;
  if (typeof admitted !== 'string' || !Object.hasOwn(AUDIO_QUALITY_TIERS, admitted)) return null;
  return admitted as AudioQualityTier;
}

/** The SFU acknowledgment is the admitted policy; older SFUs fall back to join authorization. */
export function resolveSessionAudioCaps(
  ack: {
    allowedAudioTiers?: unknown;
    minPtimeMs?: unknown;
    channelAudioUpliftTier?: unknown;
  },
  joinMediaEntitlements: unknown,
  channelTier: unknown
): SessionAudioCaps {
  const join =
    joinMediaEntitlements && typeof joinMediaEntitlements === 'object'
      ? (joinMediaEntitlements as Record<string, unknown>)
      : {};
  const joinTiers = knownTiers(join.allowed_audio_tiers) ?? FREE_TIERS;
  const joinPtime = validPtime(join.min_ptime_ms) ?? FREE_ENTITLEMENT.minPtimeMs;
  const admittedTiers = knownTiers(ack.allowedAudioTiers);
  const joinUpliftTier =
    join.channel_audio_uplift === true &&
    typeof channelTier === 'string' &&
    Object.hasOwn(AUDIO_QUALITY_TIERS, channelTier)
      ? (channelTier as AudioQualityTier)
      : null;
  // A present SFU field belongs to the final A2 authorization snapshot.
  const upliftTier = admittedChannelUpliftTier(ack.channelAudioUpliftTier, joinUpliftTier);
  const fallbackTiers =
    join.tier === 'premium' || upliftTier
      ? joinTiers
      : joinTiers.filter((tier) => FREE_TIERS.includes(tier));
  const admittedFreeOnly = admittedTiers?.every((tier) => FREE_TIERS.includes(tier)) ?? false;
  const freePtimeFloor = Math.max(joinPtime, FREE_ENTITLEMENT.minPtimeMs);
  const fallbackPtime =
    !admittedFreeOnly && (join.tier === 'premium' || upliftTier) ? joinPtime : freePtimeFloor;
  return {
    allowedTiers: admittedTiers ?? (fallbackTiers.length > 0 ? fallbackTiers : FREE_TIERS),
    minPtimeMs: validPtime(ack.minPtimeMs) ?? fallbackPtime,
    channelUpliftTier: upliftTier,
  };
}

/** Intersect the live entitlement with the policy pinned at SFU admission. */
export function effectiveAudioCaps(
  session: SessionAudioCaps | null,
  live: Entitlement
): { maxBitrate: number; minPtimeMs: number } {
  const ceiling = (tiers: readonly string[]): number =>
    Math.max(
      0,
      ...tiers.map((tier) =>
        Object.hasOwn(AUDIO_QUALITY_TIERS, tier)
          ? AUDIO_QUALITY_TIERS[tier as AudioQualityTier].maxBitrate
          : 0
      )
    );
  const upliftBitrate = session?.channelUpliftTier
    ? AUDIO_QUALITY_TIERS[session.channelUpliftTier].maxBitrate
    : 0;
  const upliftPtime = session?.channelUpliftTier
    ? AUDIO_QUALITY_TIERS[session.channelUpliftTier].preferredFrameSize
    : Infinity;
  return {
    maxBitrate: Math.min(
      ceiling(session?.allowedTiers ?? FREE_TIERS) || AUDIO_QUALITY_TIERS.standard.maxBitrate,
      Math.max(
        ceiling(live.allowedAudioTiers) || AUDIO_QUALITY_TIERS.standard.maxBitrate,
        upliftBitrate
      )
    ),
    minPtimeMs: Math.max(
      session?.minPtimeMs ?? FREE_ENTITLEMENT.minPtimeMs,
      Math.min(validPtime(live.minPtimeMs) ?? FREE_ENTITLEMENT.minPtimeMs, upliftPtime)
    ),
  };
}

export function capAudioTier(requested: AudioQualityTier, maxBitrate: number): AudioQualityTier {
  const requestedBitrate = AUDIO_QUALITY_TIERS[requested].maxBitrate;
  return (
    [...AUDIO_TIERS]
      .reverse()
      .find(
        (tier) => AUDIO_QUALITY_TIERS[tier].maxBitrate <= Math.min(requestedBitrate, maxBitrate)
      ) ?? 'minimum'
  );
}

export function capAudioPtime(requestedMs: number, minimumMs: number): 10 | 20 | 40 | 60 {
  return (
    ([10, 20, 40, 60] as const).find((ptime) => ptime >= Math.max(requestedMs, minimumMs)) ?? 60
  );
}
