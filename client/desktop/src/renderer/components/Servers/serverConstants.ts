export const MAX_ICON_SIZE = 5 * 1024 * 1024; // 5 MiB
export const MAX_BANNER_SIZE = 5 * 1024 * 1024; // 5 MiB
export const MACH_ICON_SIZE = 8 * 1024 * 1024; // 8 MiB
export const MACH_BANNER_SIZE = 8 * 1024 * 1024; // 8 MiB

type ServerImageTier = 'groundspeed' | 'mach1' | 'mach2' | 'mach3' | 'selfhost' | null | undefined;

/** Ladder levels granting the 8 MiB image sizes — mirrors Go MaxServerIconBytes/
 *  MaxServerBannerBytes (server_entitlements.go, ADR-0028). Anything else —
 *  including the retired pre-ladder 'mach' string — fails closed to 5 MiB. */
const MACH_IMAGE_TIERS: ReadonlySet<string> = new Set(['mach1', 'mach2', 'mach3', 'selfhost']);

export function maxServerIconSizeForTier(serverTier: ServerImageTier): number {
  return serverTier != null && MACH_IMAGE_TIERS.has(serverTier) ? MACH_ICON_SIZE : MAX_ICON_SIZE;
}

export function maxServerBannerSizeForTier(serverTier: ServerImageTier): number {
  return serverTier != null && MACH_IMAGE_TIERS.has(serverTier)
    ? MACH_BANNER_SIZE
    : MAX_BANNER_SIZE;
}
export const ALLOWED_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
export const NAME_MIN = 3;
export const NAME_MAX = 100;

export interface ServerFormErrors {
  name?: string;
  icon?: string;
  banner?: string;
  general?: string;
}
