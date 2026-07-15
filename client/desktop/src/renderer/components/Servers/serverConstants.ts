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

/**
 * Validate the server-name field. Shared by CreateServerModal and
 * ServerSettingsPage so the two forms cannot drift on the name rules.
 */
export function validateServerName(name: string): ServerFormErrors {
  const errors: ServerFormErrors = {};
  const trimmed = name.trim();
  if (!trimmed) {
    errors.name = 'Server name is required';
  } else if (trimmed.length < NAME_MIN) {
    errors.name = `Server name must be at least ${NAME_MIN} characters`;
  } else if (trimmed.length > NAME_MAX) {
    errors.name = `Server name must be at most ${NAME_MAX} characters`;
  }
  return errors;
}
