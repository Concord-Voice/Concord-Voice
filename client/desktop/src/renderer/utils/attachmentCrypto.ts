/**
 * Attachment encryption/decryption utilities for E2EE file attachments.
 * Uses the same AES-256-GCM scheme as message encryption (12-byte IV prepended).
 */

const AES_GCM_IV_LENGTH = 12;

/** Maximum file size in bytes (25 MiB). */
export const MAX_FILE_SIZE = 26_214_400;

/** Maximum number of attachments per message. */
export const MAX_ATTACHMENTS = 5;

/** File type classifications matching backend FileType enum. */
export type FileTypeCategory = 'photo' | 'animated' | 'video' | 'audio' | 'file';

const IMAGE_TYPES = new Set([
  'image/png',
  'image/jpeg',
  'image/jpg',
  'image/webp',
  'image/heic',
  'image/heif',
  'image/bmp',
  'image/tiff',
]);

const ANIMATED_TYPES = new Set(['image/gif', 'image/apng']);

const VIDEO_TYPES = new Set([
  'video/mp4',
  'video/webm',
  'video/quicktime',
  'video/x-msvideo',
  'video/x-matroska',
]);

const AUDIO_TYPES = new Set([
  'audio/mpeg',
  'audio/mp3',
  'audio/ogg',
  'audio/wav',
  'audio/webm',
  'audio/flac',
  'audio/aac',
  'audio/x-m4a',
]);

/**
 * Classify a MIME type into the backend's file_type categories.
 */
export function classifyFileType(mimeType: string): FileTypeCategory {
  const lower = mimeType.toLowerCase();
  if (ANIMATED_TYPES.has(lower)) return 'animated';
  if (IMAGE_TYPES.has(lower)) return 'photo';
  if (VIDEO_TYPES.has(lower)) return 'video';
  if (AUDIO_TYPES.has(lower)) return 'audio';
  return 'file';
}

/**
 * Format a file size in bytes to a human-readable string.
 */
export function formatFileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * Check if a MIME type represents an image (including animated).
 */
export function isImageType(mimeType: string): boolean {
  const lower = mimeType.toLowerCase();
  return IMAGE_TYPES.has(lower) || ANIMATED_TYPES.has(lower);
}

/**
 * Encrypt a file's ArrayBuffer with a channel key (AES-256-GCM).
 * Returns ArrayBuffer with 12-byte IV prepended to ciphertext.
 */
export async function encryptFile(data: ArrayBuffer, channelKey: CryptoKey): Promise<ArrayBuffer> {
  const iv = crypto.getRandomValues(new Uint8Array(AES_GCM_IV_LENGTH));
  const ciphertext = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, channelKey, data);

  const result = new Uint8Array(iv.byteLength + ciphertext.byteLength);
  result.set(iv, 0);
  result.set(new Uint8Array(ciphertext), iv.byteLength);

  return result.buffer;
}

/**
 * Decrypt a file's ArrayBuffer with a channel key (AES-256-GCM).
 * Expects 12-byte IV prepended to ciphertext.
 */
export async function decryptFile(data: ArrayBuffer, channelKey: CryptoKey): Promise<ArrayBuffer> {
  const arr = new Uint8Array(data);
  const iv = arr.slice(0, AES_GCM_IV_LENGTH);
  const ciphertext = arr.slice(AES_GCM_IV_LENGTH);

  return crypto.subtle.decrypt({ name: 'AES-GCM', iv }, channelKey, ciphertext);
}
