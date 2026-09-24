// client/desktop/src/renderer/services/voice/mediaPolicyEvents.ts
/**
 * #2153 media-rate policer — the renderer's validation boundary for the four
 * media-plane payloads the policer adds or widens (spec §5.7). Pure: no store, no
 * socket. A payload that fails here changes NO state; callers log a fixed verdict
 * string and nothing from the payload (observability rule 4).
 */
import { z } from 'zod';

export const MEDIA_POLICY_SOURCES = ['mic', 'camera', 'screen', 'screen-audio'] as const;
export type MediaPolicySource = (typeof MEDIA_POLICY_SOURCES)[number];

export const MediaPolicyNoticeSchema = z.object({
  producerId: z.string().min(1),
  kind: z.enum(['audio', 'video']),
  source: z.enum(MEDIA_POLICY_SOURCES),
  action: z.literal('paused'), // closed enum: an unknown action rejects the payload (R9)
});
export type MediaPolicyNotice = z.infer<typeof MediaPolicyNoticeSchema>;

/** Unknown kind/source degrade to absent (verified on zod 4.6.5), so the legacy-mute fallback still runs. */
export const ProducerStateChangeSchema = z.object({
  producerId: z.string().min(1),
  userId: z.string().min(1),
  kind: z.enum(['audio', 'video']).optional().catch(undefined),
  source: z.enum(MEDIA_POLICY_SOURCES).optional().catch(undefined),
});
export type ProducerStateChange = z.infer<typeof ProducerStateChangeSchema>;

export const RETRY_AFTER_SEC_MAX = 86_400;

/** Finite and in (0, 86400] → the value; anything else is treated as absent (A2/A3). */
export function parseRetryAfterSec(value: unknown): number | null {
  if (typeof value !== 'number' || !Number.isFinite(value)) return null;
  if (value <= 0 || value > RETRY_AFTER_SEC_MAX) return null;
  return value;
}

export const ForceDisconnectSchema = z.object({
  channelId: z.string().min(1),
  reason: z.enum(['access_revoked', 'media_policy']),
  retryAfterSec: z.unknown().optional(),
});

export interface ForceDisconnectEvent {
  channelId: string;
  reason: 'access_revoked' | 'media_policy';
  retryAfterSec: number | null;
}

export function parseMediaPolicyNotice(payload: unknown): MediaPolicyNotice | null {
  const result = MediaPolicyNoticeSchema.safeParse(payload);
  return result.success ? result.data : null;
}

export function parseProducerStateChange(payload: unknown): ProducerStateChange | null {
  const result = ProducerStateChangeSchema.safeParse(payload);
  return result.success ? result.data : null;
}

export function parseForceDisconnect(payload: unknown): ForceDisconnectEvent | null {
  const result = ForceDisconnectSchema.safeParse(payload);
  if (!result.success) return null;
  const { channelId, reason, retryAfterSec } = result.data;
  // The media plane sets retryAfterSec only for media_policy (spec §5.7).
  return {
    channelId,
    reason,
    retryAfterSec: reason === 'media_policy' ? parseRetryAfterSec(retryAfterSec) : null,
  };
}

/** Payload source, else the local producer/consumer map, else null (→ legacy isMuted). */
export function resolvePausedSource(
  event: ProducerStateChange,
  localSourceByProducerId: ReadonlyMap<string, MediaPolicySource>
): MediaPolicySource | null {
  return event.source ?? localSourceByProducerId.get(event.producerId) ?? null;
}

export function rejoinAtFrom(retryAfterSec: number | null, nowMs: number): number | null {
  return retryAfterSec === null ? null : nowMs + retryAfterSec * 1000;
}

const MINUTE_MS = 60_000;

/** The instant the dialog names: never earlier than the real cooldown end (handoff §1d). */
export function roundUpToMinute(ms: number): number {
  return Math.ceil(ms / MINUTE_MS) * MINUTE_MS;
}

/** Wall-clock, not a countdown (handoff T9). */
export function formatRejoinTime(rejoinAtMs: number): string {
  return new Intl.DateTimeFormat(undefined, { timeStyle: 'short' }).format(
    new Date(roundUpToMinute(rejoinAtMs))
  );
}
