import { FREE_ENTITLEMENT } from '../../stores/auth/subscriptionStore';

export interface VideoBitrateCaps {
  camera: number;
  screen: number;
}

function positiveBitrate(value: unknown, fallback: number): number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0 ? value : fallback;
}

/**
 * Pin video ceilings to the media session the SFU admitted. New media planes
 * report their actual caps in the join-room acknowledgment. For older peers,
 * use the control plane's join authorization, which may race a tier change
 * between the two independently authorized joins. Missing or malformed data
 * falls back to the free entitlement.
 */
export function resolveSessionVideoBitrateCaps(
  ack: { cameraMaxBitrateBps?: unknown; screenMaxBitrateBps?: unknown },
  joinMediaEntitlements: unknown
): VideoBitrateCaps {
  const join =
    joinMediaEntitlements && typeof joinMediaEntitlements === 'object'
      ? (joinMediaEntitlements as Record<string, unknown>)
      : {};
  const isPremium = join.tier === 'premium';
  const cameraFromJoin = positiveBitrate(
    join.camera_max_bitrate_bps,
    FREE_ENTITLEMENT.cameraMaxBitrate
  );
  const screenFromJoin = positiveBitrate(
    join.screen_max_bitrate_bps,
    FREE_ENTITLEMENT.streamMaxBitrate
  );
  const fallback: VideoBitrateCaps = {
    camera: isPremium
      ? cameraFromJoin
      : Math.min(cameraFromJoin, FREE_ENTITLEMENT.cameraMaxBitrate),
    screen: isPremium
      ? screenFromJoin
      : Math.min(screenFromJoin, FREE_ENTITLEMENT.streamMaxBitrate),
  };
  return {
    camera: positiveBitrate(ack.cameraMaxBitrateBps, fallback.camera),
    screen: positiveBitrate(ack.screenMaxBitrateBps, fallback.screen),
  };
}
