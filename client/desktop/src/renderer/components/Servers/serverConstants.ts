export const MAX_ICON_SIZE = 1024 * 1024; // 1MB
export const MAX_BANNER_SIZE = 2 * 1024 * 1024; // 2MB
export const ALLOWED_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
export const NAME_MIN = 3;
export const NAME_MAX = 100;

export interface ServerFormErrors {
  name?: string;
  icon?: string;
  banner?: string;
  general?: string;
}
