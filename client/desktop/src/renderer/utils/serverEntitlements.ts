// Client mirror of the server-axis entitlement ladder. SOURCE OF TRUTH:
// services/control-plane/internal/entitlements/server_entitlements.go
// (ADR-0028; founder entitlement matrix docs/design/entitlements/
// entitlement-matrix.md, 2026-06-18). Values are public marketing numbers,
// mirrored for lock-affordance UX only — the control-plane is authoritative
// and hard-enforces at the (future) create boundaries.
// -1 = unlimited (selfhost). Same mirror-comment discipline as
// AUDIO_QUALITY_TIERS in stores/voiceStore.ts.
//
// Rows are deep-frozen: unlike Go's ForServer (which returns a value copy),
// serverEntitlementsFor hands back the shared row object — freezing makes an
// accidental consumer mutation throw (strict mode) instead of silently
// corrupting every reader.

export interface ServerTierEntitlements {
  readonly maxCustomEmoji: number; // includes animated at every tier
  readonly maxStickers: number;
  readonly maxSoundboards: number;
  readonly maxUploadBytes: number; // per-file, server-wide
  readonly allowAnimatedBanner: boolean; // animated GIF server BANNER, Mach 1+ (#1302); icons stay static
}

export const SERVER_TIER_ENTITLEMENTS: Record<string, ServerTierEntitlements> = Object.freeze({
  groundspeed: Object.freeze({
    maxCustomEmoji: 75,
    maxStickers: 10,
    maxSoundboards: 15,
    maxUploadBytes: 33_554_432,
    allowAnimatedBanner: false,
  }),
  mach1: Object.freeze({
    maxCustomEmoji: 250,
    maxStickers: 75,
    maxSoundboards: 30,
    maxUploadBytes: 134_217_728,
    allowAnimatedBanner: true,
  }),
  mach2: Object.freeze({
    maxCustomEmoji: 350,
    maxStickers: 100,
    maxSoundboards: 40,
    maxUploadBytes: 268_435_456,
    allowAnimatedBanner: true,
  }),
  mach3: Object.freeze({
    maxCustomEmoji: 500,
    maxStickers: 150,
    maxSoundboards: 55,
    maxUploadBytes: 536_870_912,
    allowAnimatedBanner: true,
  }),
  selfhost: Object.freeze({
    maxCustomEmoji: -1,
    maxStickers: -1,
    maxSoundboards: -1,
    maxUploadBytes: -1,
    allowAnimatedBanner: true,
  }),
});

/** Fail-closed accessor: unknown/absent/retired tiers resolve to groundspeed. */
export function serverEntitlementsFor(tier?: string): ServerTierEntitlements {
  return SERVER_TIER_ENTITLEMENTS[tier ?? ''] ?? SERVER_TIER_ENTITLEMENTS.groundspeed;
}
