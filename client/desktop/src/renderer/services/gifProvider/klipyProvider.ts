/**
 * KLIPY adapter that implements the vendor-neutral `GifProvider` interface.
 *
 * This is the only file that translates between KLIPY's response shapes and
 * the rest of Concord's GIF code. The picker, embed, and message composer talk
 * to `gifProvider` from `./index.ts` and never see KLIPY-specific types.
 *
 * Rendition selection follows the universal "MP4 over GIF" best practice:
 * if KLIPY returns an MP4 variant we use it (smaller, higher quality, plays in
 * a `<video>` element); otherwise we fall back to GIF or WEBP rendered in an
 * `<img>` element. The renderer chooses based on `GifResolved.animatedKind`.
 *
 * KLIPY ToS compliance:
 * - Media URLs are rewritten through the control-plane proxy (`/api/v1/klipy/media`)
 *   via `rewriteMediaUrl` so the user's IP is never exposed to KLIPY's CDN.
 * - We do NOT cache or store any KLIPY-derived metadata; the only field we
 *   persist anywhere is the slug (per-user bookmarks in `savedGifsStore`).
 * - "Powered by KLIPY" attribution is rendered in the picker footer (Section 4).
 * - Independence disclaimer is rendered next to the powered-by mark (Section 3).
 */

import type {
  GifProvider,
  GifResolved,
  GifSearchResult,
  GifSearchOpts,
  GifTrendingOpts,
  GifRecentOpts,
  GifCategoryOpts,
  GifCategory,
} from './types';
import {
  klipyClient,
  rewriteMediaUrl,
  type KlipyGifItem,
  type KlipyListResponse,
} from './klipyClient';

/** Translate a single KLIPY GIF item into the vendor-neutral resolved shape.
 *  Defensive against missing fields — different KLIPY endpoints sometimes
 *  return slightly different shapes for the same GIF object. */
/** Walk every plausible nesting path for a given format and return the first
 *  rendition that has a URL. KLIPY's real API nests under quality tiers
 *  (file.hd.gif.url) but the docs and demos sometimes show the flat shape
 *  (file.gif.url), so we check both. */
function pickRendition(
  item: KlipyGifItem,
  format: 'mp4' | 'gif' | 'webp'
): { url?: string; width?: number; height?: number } | undefined {
  return (
    item.file?.hd?.[format] ??
    item.file?.md?.[format] ??
    item.file?.sm?.[format] ??
    item.file?.[format] ??
    item.preview?.[format]
  );
}

function toResolved(item: KlipyGifItem): GifResolved | null {
  if (!item.slug) return null;

  // Pick the best animated rendition: MP4 first (renders as <video>), then
  // WEBP/GIF (rendered as <img>). Default `image` covers both fallback cases
  // so we only need to override when MP4 is selected.
  const mp4 = pickRendition(item, 'mp4');
  const webp = pickRendition(item, 'webp');
  const gif = pickRendition(item, 'gif');

  const animatedUrl = rewriteMediaUrl(mp4?.url ?? webp?.url ?? gif?.url ?? item.url);
  if (!animatedUrl) return null;
  const animatedKind: 'video' | 'image' = mp4?.url ? 'video' : 'image';
  let chosen: { url?: string; width?: number; height?: number } | undefined;
  if (mp4?.url) chosen = mp4;
  else if (webp?.url) chosen = webp;
  else chosen = gif;

  // Still rendition: prefer the explicit still URL, fall back to the
  // animated URL (the still will autoplay when used as <video poster> too)
  const rawStill = item.still?.url ?? mp4?.url ?? webp?.url ?? gif?.url ?? item.url;
  const stillUrl = rewriteMediaUrl(rawStill) ?? animatedUrl;

  return {
    slug: item.slug,
    width: chosen?.width ?? item.width,
    height: chosen?.height ?? item.height,
    animatedUrl,
    animatedKind,
    stillUrl,
  };
}

/** Unwrap KLIPY's nested `{ result, data: { data: [...], has_more } }` envelope.
 *  Falls back to legacy flat shapes if the API ever returns them. */
function unwrapList(resp: KlipyListResponse): {
  items: KlipyGifItem[];
  hasMore: boolean | undefined;
} {
  // Current shape: { result: bool, data: { data: [...], has_more: bool } }
  if (resp.data && !Array.isArray(resp.data)) {
    return {
      items: resp.data.data ?? resp.data.result ?? [],
      hasMore: resp.data.has_more ?? resp.data.meta?.has_more,
    };
  }
  // Legacy flat shapes
  const items =
    (Array.isArray(resp.data) ? resp.data : undefined) ??
    (Array.isArray(resp.result) ? resp.result : undefined) ??
    [];
  return { items, hasMore: resp.has_more ?? resp.meta?.has_more };
}

function toSearchResult(resp: KlipyListResponse, page: number, perPage: number): GifSearchResult {
  const { items: raw, hasMore: rawHasMore } = unwrapList(resp);
  const items = raw.map(toResolved).filter((g): g is GifResolved => g !== null);
  const hasMore = rawHasMore ?? raw.length >= perPage;
  return {
    items,
    hasMore,
    nextOffset: hasMore ? page + 1 : undefined,
  };
}

export const klipyProvider: GifProvider = {
  name: 'KLIPY',
  searchPlaceholder: 'Search KLIPY',
  poweredByText: 'Powered by KLIPY',
  // KLIPY ships a Yellow&Black mark for light backgrounds and a Yellow&White
  // mark for dark backgrounds in their attribution kit. Use the official assets
  // verbatim to satisfy ToS Section 4 (no recoloring/modification).
  logoAssetLight: './branding/KLIPY/klipy-logo-light.svg',
  logoAssetDark: './branding/KLIPY/klipy-logo-dark.svg',
  // Surfaced in Settings > About (third-party services), not in the picker.
  independenceDisclaimer:
    'Concord Voice is independently developed and not affiliated with or endorsed by KLIPY.',
  supportsRecent: true,
  supportsCategories: true,

  async trending(opts: GifTrendingOpts): Promise<GifSearchResult> {
    // KLIPY uses 1-indexed pages; we expose offset semantics to the rest of
    // the codebase. Compute the page from offset/limit.
    const page = Math.floor(opts.offset / opts.limit) + 1;
    const resp = await klipyClient.trending(page, opts.limit, opts.locale);
    return toSearchResult(resp, page, opts.limit);
  },

  async search(opts: GifSearchOpts): Promise<GifSearchResult> {
    const page = Math.floor(opts.offset / opts.limit) + 1;
    const resp = await klipyClient.search(opts.q, page, opts.limit, opts.locale);
    return toSearchResult(resp, page, opts.limit);
  },

  async recent(opts: GifRecentOpts): Promise<GifSearchResult> {
    const page = Math.floor(opts.offset / opts.limit) + 1;
    const resp = await klipyClient.recent(page, opts.limit);
    return toSearchResult(resp, page, opts.limit);
  },

  async categories(opts: GifCategoryOpts): Promise<GifCategory[]> {
    const resp = await klipyClient.categories(opts.locale);
    // Same nested envelope as list responses: { data: { data: [...] } }.
    let raw: { name: string; preview?: KlipyGifItem }[] = [];
    if (resp.data && !Array.isArray(resp.data)) {
      raw = resp.data.data ?? resp.data.result ?? [];
    } else if (Array.isArray(resp.data)) {
      raw = resp.data;
    } else if (Array.isArray(resp.result)) {
      raw = resp.result;
    }
    const result: GifCategory[] = [];
    for (const cat of raw) {
      const preview = cat.preview ? toResolved(cat.preview) : null;
      if (cat.name && preview) {
        result.push({ name: cat.name, preview });
      }
    }
    return result;
  },

  async getBySlug(slug: string): Promise<GifResolved> {
    const item = await klipyClient.getBySlug(slug);
    if (!item) {
      throw new Error(`GIF not found: ${slug}`);
    }
    const resolved = toResolved(item);
    if (!resolved) {
      throw new Error(`GIF has no usable rendition: ${slug}`);
    }
    return resolved;
  },

  async notifyShared(slug: string): Promise<void> {
    await klipyClient.notifyShared(slug);
  },

  async report(slug: string): Promise<void> {
    await klipyClient.report(slug);
  },

  setPersonalizationEnabled(enabled: boolean): void {
    klipyClient.setPersonalizationEnabled(enabled);
  },
};
