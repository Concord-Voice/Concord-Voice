import { API_BASE } from '../config';

/**
 * Absolutize a server-origin media URL for use in an <img>/<video>/<audio> src.
 *
 * The control-plane emits avatar/banner/icon URLs as host-relative
 * `/api/v1/media/*` paths (the documented MediaURL contract — see
 * `types/ws-events.ts`). In the remote-SPA renderer, `document.baseURI` is the
 * SPA origin (spa.example.com), so a raw relative <img src> resolves to
 * the wrong host. Prefix API_BASE so it resolves against the API host.
 *
 * At least as strict as the MediaURL zod schema on dangerous schemes (plus blob:
 * for local crop previews): a leading-`/` path is API_BASE-prefixed; data:/blob:/http(s)://
 * pass through unchanged; anything else (including javascript:) returns
 * undefined (do NOT render — never broaden what reaches an <img src>).
 *
 * NOTE: this deliberately does NOT use `new URL(url)` like `rewriteMediaUrl` —
 * that throws on relative paths. Branch on a leading `/` instead. This is a
 * DISPLAY helper; never feed its output back to the server (the wire value
 * stays relative, e.g. in `useImageUpload.imageUrl`).
 */
export function resolveMediaUrl(url?: string | null): string | undefined {
  if (!url) return undefined;
  if (url.startsWith('/')) return `${API_BASE}${url}`;
  if (url.startsWith('data:') || url.startsWith('blob:') || /^https?:\/\//i.test(url)) {
    return url;
  }
  return undefined;
}
