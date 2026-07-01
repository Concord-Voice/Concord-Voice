export const MAX_AVATAR_SIZE = 5 * 1024 * 1024; // 5 MiB
export const MAX_HEADER_SIZE = 5 * 1024 * 1024; // 5 MiB
export const ALLOWED_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
export const USERNAME_MIN = 3;
export const USERNAME_MAX = 32;
export const DISPLAY_NAME_MAX = 100;
export const BIO_MAX = 500;
export const MAX_LINKS = 5;

export interface ProfileFormErrors {
  username?: string;
  displayName?: string;
  bio?: string;
  avatar?: string;
  header?: string;
  general?: string;
  [key: string]: string | undefined;
}

export interface PasswordFormErrors {
  currentPassword?: string;
  newPassword?: string;
  confirmPassword?: string;
  general?: string;
}
