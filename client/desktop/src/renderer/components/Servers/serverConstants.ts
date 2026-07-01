export const MAX_ICON_SIZE = 5 * 1024 * 1024; // 5 MiB
export const MAX_BANNER_SIZE = 5 * 1024 * 1024; // 5 MiB
export const MACH_ICON_SIZE = 8 * 1024 * 1024; // 8 MiB
export const MACH_BANNER_SIZE = 8 * 1024 * 1024; // 8 MiB

type ServerImageTier = 'groundspeed' | 'mach' | null | undefined;

export function maxServerIconSizeForTier(serverTier: ServerImageTier): number {
  return serverTier === 'mach' ? MACH_ICON_SIZE : MAX_ICON_SIZE;
}

export function maxServerBannerSizeForTier(serverTier: ServerImageTier): number {
  return serverTier === 'mach' ? MACH_BANNER_SIZE : MAX_BANNER_SIZE;
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
