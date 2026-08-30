import React, { useState, useEffect, useRef, useCallback } from 'react';
import { Download, FileText, Film, Music, File, Loader2, Maximize2 } from 'lucide-react';
import { apiFetch } from '../../services/apiClient';
import { e2eeService } from '../../services/e2eeService';
import {
  parseKeyVersionHeader,
  AttachmentKeyEpochError,
  decryptAttachmentBlob,
  UnsupportedAttachmentFormatError,
  AttachmentIntegrityError,
} from '../../utils/attachmentChunkedCrypto';
import { formatFileSize } from '../../utils/attachmentCrypto';
import {
  AttachmentTooLargeError,
  readBoundedBody,
  tooLargeMessage,
} from '../../utils/boundedResponseBody';
import { MAX_DECRYPTABLE_ATTACHMENT_BYTES } from '../../utils/entitlementLimits';
import type { AttachmentSummary } from '../../types/chat';
import { useSettingsStore } from '../../stores/ui/settingsStore';
import OverflowMarkdownAttachment from './OverflowMarkdownAttachment';
import ThemedMediaPlayer from './ThemedMediaPlayer';
import ImageLightbox from './ImageLightbox';
import ContextMenu from '../ui/ContextMenu';
import './AttachmentDisplay.css';

interface AttachmentDisplayProps {
  readonly attachments: AttachmentSummary[];
  readonly channelId: string;
  /** Already-decrypted message body — used as preview for text/markdown attachments. */
  readonly messageBody?: string;
}

interface AttachmentItemProps {
  readonly attachment: AttachmentSummary;
  readonly channelId: string;
}

// LRU cache for decrypted blob URLs — evicts oldest entries to bound memory.
/** Secondary entry-count cap: keeps a channel full of thumbnails from growing
 *  the Map without limit even when the byte budget is nowhere near. */
const BLOB_CACHE_MAX = 50;

/** 256 MiB — total decrypted bytes held live across the whole cache (#2157 A1).
 *  The entry cap alone made retained memory scale with the download guard
 *  (50 x 256 MiB = ~12.8 GB), because up to BLOB_CACHE_MAX decrypted blobs are
 *  alive simultaneously — the dominant memory term, not the per-file transient. */
const BLOB_CACHE_MAX_BYTES = 268_435_456;

/** Largest decrypted blob worth RETAINING, as HALF the budget.
 *
 *  A single entry at or near the whole budget is pathological: admitting it
 *  drains every other entry on the way in and then occupies all of it, so one
 *  large attachment empties the cache for everything else on screen. Capping
 *  admission at half guarantees two entries can always coexist, so no single
 *  admission can ever drain the cache. Anything larger is shown but never
 *  cached -- one re-fetch if the reader scrolls back, against evicting
 *  everything else exactly once.
 *
 *  Chunked uploads make this reachable: before them the upload ceiling held
 *  attachments to 128 MiB, and a 256 MiB file is now a normal premium upload.
 *
 *  A QUARTER was tried first and rejected on evidence. At 64 MiB the existing
 *  100 MiB eviction fixtures stopped being cached at all, which broke one test
 *  and — worse — made several neighbours pass VACUOUSLY: "never revokes a url
 *  while a surface is rendering it" holds trivially when nothing was cached.
 *  A threshold that converts working tests into vacuous ones is too aggressive
 *  for this budget, not a reason to rewrite the tests. */
export const BLOB_CACHE_RETAIN_MAX_BYTES = Math.floor(BLOB_CACHE_MAX_BYTES / 2);

interface CachedBlob {
  readonly url: string;
  readonly bytes: number;
}

const blobUrlCache = new Map<string, CachedBlob>();
let cachedBytes = 0;

/**
 * File ids currently rendered by at least one mounted surface, with a count.
 *
 * Deliberately NOT a refcount stored on the cache entry. A handoff scheme —
 * loader retains, component releases — leaks a permanent reference whenever a
 * surface unmounts while its load is still in flight, because the release never
 * runs. Keying on "is anything mounted showing this?" is derived state owned
 * entirely by the effect below, so an abandoned load simply never registers.
 */
const liveSurfaces = new Map<string, number>();

/**
 * Hold this file id for as long as a surface renders its url, so eviction
 * cannot revoke a blob out from under a mounted `<img>` (#2157 review,
 * VULN-002-C). A revoked url on screen is a silently broken image, and
 * `handleSaveImage`'s fetch of it fails into a swallowed catch.
 *
 * Known narrow window: a surface is protected from the moment its effect runs,
 * not from the moment its bytes are admitted. Several loads resolving inside a
 * single tick, before React flushes any effect, can still evict one another.
 * The realistic case — images already on screen when another arrives — is
 * covered, and the residual is a broken image rather than anything unsafe.
 */
function useRetainedBlobUrl(fileId: string, url: string | null): void {
  useEffect(() => {
    if (!url) return;
    liveSurfaces.set(fileId, (liveSurfaces.get(fileId) ?? 0) + 1);
    return () => {
      const remaining = (liveSurfaces.get(fileId) ?? 1) - 1;
      if (remaining > 0) {
        liveSurfaces.set(fileId, remaining);
        return;
      }
      liveSurfaces.delete(fileId);
      // Nothing in the cache means nothing will ever evict it, and eviction is
      // the only other revoker. An entry above BLOB_CACHE_RETAIN_MAX_BYTES is
      // deliberately never cached, so the last surface to let go owns the
      // revoke -- otherwise declining to cache would trade an eviction storm
      // for a permanent leak.
      if (!blobUrlCache.has(fileId)) URL.revokeObjectURL(url);
    };
  }, [fileId, url]);
}

/**
 * Drops the least-recently-used entry that no mounted surface is showing.
 *
 * Returns false when every candidate is on screen — which terminates the
 * eviction loop and is the correct answer: the budget bounds cache RETENTION,
 * not what the user is currently looking at, so it is exceeded rather than
 * breaking a live image.
 */
function evictOldestBlob(): boolean {
  for (const [fileId, entry] of blobUrlCache) {
    if (liveSurfaces.has(fileId)) continue; // on screen — try the next-oldest
    URL.revokeObjectURL(entry.url);
    cachedBytes -= entry.bytes;
    blobUrlCache.delete(fileId);
    return true;
  }
  return false;
}

/**
 * Drop every cached blob and all derived accounting.
 *
 * Exported for tests ONLY. `blobUrlCache`, `cachedBytes`, `liveSurfaces` and
 * `inFlightLoads` are module scope, and Vitest isolates modules per FILE rather
 * than per test — so without this, every test in a file shares one cache and
 * the byte-budget assertions silently depend on what earlier tests left behind.
 * Production code must never call it: revoking urls a surface still renders is
 * exactly what `liveSurfaces` exists to prevent.
 */
export function __resetBlobCacheForTests(): void {
  for (const entry of blobUrlCache.values()) URL.revokeObjectURL(entry.url);
  blobUrlCache.clear();
  liveSurfaces.clear();
  inFlightLoads.clear();
  cachedBytes = 0;
}

function cacheBlobUrl(fileId: string, url: string, bytes: number): void {
  // Too large to retain: show it, do not cache it. useRetainedBlobUrl revokes it
  // when the last surface unmounts -- without that this would leak, because the
  // cache is otherwise the ONLY thing that ever revokes a url.
  if (bytes > BLOB_CACHE_RETAIN_MAX_BYTES) return;

  const superseded = blobUrlCache.get(fileId);
  if (superseded !== undefined) {
    // Unreachable while loads are coalesced, but it must not leak if it ever
    // is: dropping an entry from the Map without revoking puts its url beyond
    // eviction's reach forever. Only safe to revoke when nothing is showing it.
    if (!liveSurfaces.has(fileId)) URL.revokeObjectURL(superseded.url);
    cachedBytes -= superseded.bytes;
    blobUrlCache.delete(fileId);
  }

  // Evict oldest-first until the incoming entry fits both bounds. Map preserves
  // insertion order, so the iteration order is LRU-first.
  while (
    (cachedBytes + bytes > BLOB_CACHE_MAX_BYTES || blobUrlCache.size >= BLOB_CACHE_MAX) &&
    evictOldestBlob()
  ) {
    /* eviction happens in the condition */
  }
  blobUrlCache.set(fileId, { url, bytes });
  cachedBytes += bytes;
}

/** Loads in progress, keyed by file id. */
const inFlightLoads = new Map<string, Promise<string>>();

/** Why an attachment could not be shown.
 *
 *  A format this build cannot read, bytes that failed authentication, and a
 *  fault on THIS side are different situations calling for different responses
 *  from the reader, and collapsing them into "failed to load" tells them
 *  nothing. Only `generic` offers a retry, because only `generic` covers
 *  something a retry can change -- a dropped connection, a 5xx. The other three
 *  are terminal: retrying re-fetches identical bytes through identical code.
 *
 *  `client` is the one that is NOT the file's fault. A key the browser refused
 *  to use, an unavailable algorithm, an epoch header that arrived mangled --
 *  reporting any of those as damage sends the reader hunting a corrupted file
 *  over a bug on our side. */
type AttachmentFailure = null | 'generic' | 'unsupported' | 'integrity' | 'client';

function failureOf(err: unknown): Exclude<AttachmentFailure, null> {
  if (err instanceof UnsupportedAttachmentFormatError) return 'unsupported';
  if (err instanceof AttachmentIntegrityError) return 'integrity';
  if (err instanceof AttachmentKeyEpochError) return 'client';
  // A WebCrypto rejection that is NOT an authentication failure reaches here
  // untouched by decryptAttachmentBlob (see isAuthenticationFailure). It is a
  // fault in our call -- a key without the `decrypt` usage, a missing
  // algorithm -- and a retry cannot fix it.
  if (typeof err === 'object' && err !== null && (err as Error).name === 'InvalidAccessError') {
    return 'client';
  }
  return 'generic';
}

const FAILURE_COPY: Record<Exclude<AttachmentFailure, null>, string> = {
  generic: 'Failed to load image',
  unsupported: 'This attachment needs a newer version of Concord to open.',
  integrity: 'This attachment could not be verified — it may be damaged or altered.',
  client: 'Concord could not decrypt this attachment on this device.',
};

async function loadAndCache(
  fileId: string,
  channelId: string,
  declaredSize: number
): Promise<string> {
  // Guards run BEFORE the allocating operation, not between two of them.
  // `file_size` rides on the summary, so an oversized attachment costs no
  // network at all. It is server-supplied metadata though, so it is a fast
  // path, never the real bound — readBoundedBody measures the actual bytes.
  if (declaredSize > MAX_DECRYPTABLE_ATTACHMENT_BYTES) {
    throw new AttachmentTooLargeError(declaredSize);
  }

  const response = await apiFetch(`/api/v1/media/attachments/${fileId}`);
  if (!response.ok) throw new Error(`Failed to fetch attachment (${response.status})`);

  const mimeType = response.headers.get('X-File-Mime-Type') || 'application/octet-stream';
  // SELECT THE KEY BY EPOCH, not "whatever is current".
  //
  // Every revocation rotates the CSK, so decrypting with the latest key
  // permanently orphaned every attachment uploaded before the most recent
  // rotation -- and reported it as "damaged or altered", i.e. tampering. The
  // upload has attested this epoch since #2832; it just never reached the
  // client. Absent header = a row predating the attestation, where the current
  // key IS the right one.
  const keyVersion = parseKeyVersionHeader(response.headers.get('X-File-Key-Version'));
  const data = await readBoundedBody(response, MAX_DECRYPTABLE_ATTACHMENT_BYTES);

  const channelKey =
    keyVersion === null
      ? await e2eeService.getChannelKey(channelId)
      : await e2eeService.getChannelKeyByVersion(channelId, keyVersion);
  // Handles BOTH formats. Dispatch is deterministic and decided before any key
  // is used, so a v2 blob and a legacy one each take exactly one path -- see
  // classifyEnvelope. A Blob rather than an ArrayBuffer is the point on this
  // path: chunk plaintexts are appended as separate parts, so no single
  // contiguous copy of a 256 MiB file is ever built.
  const blob = await decryptAttachmentBlob(new Uint8Array(data), channelKey, mimeType);

  const url = URL.createObjectURL(blob);
  cacheBlobUrl(fileId, url, blob.size);
  return url;
}

async function fetchAndDecrypt(
  fileId: string,
  channelId: string,
  declaredSize: number
): Promise<string> {
  const cached = blobUrlCache.get(fileId);
  if (cached) {
    // Move to end for LRU freshness
    blobUrlCache.delete(fileId);
    blobUrlCache.set(fileId, cached);
    return cached.url;
  }

  // Coalesce concurrent loads of one attachment onto a single fetch + decrypt.
  // The same file legitimately appears on several surfaces at once (a message
  // and the pinned-message list, or repeated posts of one upload — the server
  // does not restrict a file to one message), and IntersectionObserver's 200px
  // rootMargin brings them in together. Without coalescing each surface minted
  // its own blob url, and every one but the last was dropped from the cache
  // WITHOUT being revoked: an unreachable, un-revokable Blob pinned for the
  // life of the document, and a byte budget that under-reported live memory by
  // a growing margin. Coalescing removes the duplicates rather than trying to
  // clean up after them, and saves N-1 fetches and decrypts besides.
  const existing = inFlightLoads.get(fileId);
  if (existing) return existing;

  const load = loadAndCache(fileId, channelId, declaredSize).finally(() => {
    inFlightLoads.delete(fileId);
  });
  inFlightLoads.set(fileId, load);
  return load;
}

/** Guard copy, rendered in the chip footprint so the refusal sits where the
 *  file is. Assertive because the user asked for this file and it will not
 *  arrive — the alternative to the sentence is an OOM. */
function AttachmentTooLargeNotice({
  bytes,
  truncated = false,
}: {
  readonly bytes: number;
  readonly truncated?: boolean;
}) {
  return (
    <div className="attachment-error" role="alert">
      {tooLargeMessage(bytes, truncated, formatFileSize)}
    </div>
  );
}

function FileIcon({ fileType }: { readonly fileType: string }) {
  switch (fileType) {
    case 'video':
      return <Film size={20} />;
    case 'audio':
      return <Music size={20} />;
    case 'photo':
    case 'animated':
      return <FileText size={20} />;
    default:
      return <File size={20} />;
  }
}

// Display constraints for image attachments — must match AttachmentDisplay.css
// (.attachment-image-container max-width / .attachment-image max-height).
// Kept in sync with the CSS so the JS clamp matches what the browser paints.
const ATTACHMENT_MAX_W = 400;
const ATTACHMENT_MAX_H = 300;

/** Clamps an attachment's natural pixel dimensions into the display box,
 *  preserving aspect ratio. Returns null if dimensions aren't known so the
 *  caller can fall back to intrinsic sizing on first load. */
function clampAttachmentSize(
  w: number | undefined,
  h: number | undefined
): { width: number; height: number } | null {
  if (!w || !h) return null;
  const ratio = Math.min(ATTACHMENT_MAX_W / w, ATTACHMENT_MAX_H / h, 1);
  return { width: Math.round(w * ratio), height: Math.round(h * ratio) };
}

/** Image MIME → file extension for the Save-As default filename. */
const IMAGE_EXT: Readonly<Record<string, string>> = {
  'image/png': '.png',
  'image/jpeg': '.jpg',
  'image/gif': '.gif',
  'image/webp': '.webp',
  'image/avif': '.avif',
  'image/svg+xml': '.svg',
};

/** Returns the extension for an image MIME type, or '' when unmapped/absent. */
export function extFromMime(mime: string | undefined): string {
  return (mime && IMAGE_EXT[mime]) || '';
}

function ImageAttachment({ attachment, channelId }: AttachmentItemProps) {
  const reduceAnimations = useSettingsStore((s) => s.appearance.reduceAnimations);
  // Animated GIF attachments under Reduce Animations play only on hover/focus.
  // See QA bug #571 item #6B. Static photos ignore this flag entirely.
  const isAnimated = attachment.file_type === 'animated';
  const gatedByHover = isAnimated && reduceAnimations;
  const [hovering, setHovering] = useState(false);
  const [url, setUrl] = useState<string | null>(blobUrlCache.get(attachment.id)?.url ?? null);
  const [loading, setLoading] = useState(!url);
  const [error, setError] = useState<AttachmentFailure>(null);
  // Keep this url alive while it is on screen (#2157 review, VULN-002-C):
  // eviction may not revoke a blob a mounted surface is still pointing at.
  useRetainedBlobUrl(attachment.id, url);
  // Byte count of a refusal, or null when the attachment was never refused.
  const [tooLarge, setTooLarge] = useState<{ bytes: number; truncated: boolean } | null>(null);
  // Aspect ratio learned from <img onLoad> as a fallback for messages whose
  // summary lacks pre-known width/height (e.g. older history rows fetched
  // from the server before the dim-plumbing was in place).
  const [naturalRatio, setNaturalRatio] = useState<number | null>(null);
  const observerRef = useRef<IntersectionObserver | null>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const [lightboxOpen, setLightboxOpen] = useState(false);
  const [menuPos, setMenuPos] = useState<{ x: number; y: number } | null>(null);

  // Save the decrypted image to disk via the native Save-As dialog (#1729).
  // The blob is already decrypted in-renderer; read its bytes and hand them to
  // the main process, which owns the dialog + write (the renderer cannot reach
  // a native "Save Image As…" on a blob: URL). User-cancel is a no-op success.
  // Sync wrapper whose promise chain ends in `.catch`, so the promise is handled
  // (not floating) without the `void` operator, and callers invoke it directly.
  const handleSaveImage = useCallback((): void => {
    if (!url) return;
    fetch(url)
      .then((r) => r.arrayBuffer())
      .then((bytes) =>
        globalThis.electron.saveImageAs(
          bytes,
          `image-${attachment.id}${extFromMime(attachment.mime_type)}`
        )
      )
      .catch(() => undefined); // Non-fatal: blob read or Save-As IPC failed.
  }, [url, attachment.id, attachment.mime_type]);

  const load = useCallback(async () => {
    try {
      setLoading(true);
      const blobUrl = await fetchAndDecrypt(attachment.id, channelId, attachment.file_size);
      setUrl(blobUrl);
    } catch (err) {
      // A refusal is a different failure from a broken fetch or decrypt: the
      // bytes were never requested, so say so instead of "failed to load".
      if (err instanceof AttachmentTooLargeError)
        setTooLarge({ bytes: err.byteLength, truncated: err.truncated });
      else setError(failureOf(err));
    } finally {
      setLoading(false);
    }
  }, [attachment.id, attachment.file_size, channelId]);

  useEffect(() => {
    if (url) return; // already loaded
    const el = containerRef.current;
    if (!el) return;

    observerRef.current = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          load();
          observerRef.current?.disconnect();
        }
      },
      { rootMargin: '200px' }
    );
    observerRef.current.observe(el);

    return () => observerRef.current?.disconnect();
  }, [url, load]);

  // Compute the reserved box from pre-known summary dims when available, so
  // the optimistic message paints at the final size with no layout shift.
  // Falls back to a learned aspect ratio (from a previous onLoad in this
  // session) for legacy messages without dims, and finally to a min-height
  // skeleton for the very first paint of a dim-less attachment.
  const clamped = clampAttachmentSize(attachment.width, attachment.height);
  let containerStyle: React.CSSProperties | undefined;
  if (clamped) {
    containerStyle = { width: `${clamped.width}px`, height: `${clamped.height}px` };
  } else if (naturalRatio) {
    containerStyle = { aspectRatio: String(naturalRatio), maxWidth: `${ATTACHMENT_MAX_W}px` };
  }

  // Under Reduce Animations, animated GIF attachments show a muted overlay
  // with "Hover to play" and only render the live <img> while hovered/focused
  // — the browser restarts the GIF animation every mount. Static photos and
  // non-reduced-motion sessions skip the overlay and render the image as-is.
  const showLiveImage = url && (!gatedByHover || hovering);
  const hoverHandlers = gatedByHover
    ? {
        onMouseEnter: () => setHovering(true),
        onMouseLeave: () => setHovering(false),
        onFocus: () => setHovering(true),
        onBlur: () => setHovering(false),
        tabIndex: 0,
      }
    : {};

  return (
    <div
      ref={containerRef}
      className="attachment-image-container"
      style={containerStyle}
      {...hoverHandlers}
    >
      {loading && (
        <div className="attachment-loading">
          <Loader2 size={20} className="spinner" />
        </div>
      )}
      {error !== null && (
        <div className="attachment-error" role="alert">
          {FAILURE_COPY[error]}
        </div>
      )}
      {tooLarge !== null && (
        <AttachmentTooLargeNotice bytes={tooLarge.bytes} truncated={tooLarge.truncated} />
      )}
      {showLiveImage && (
        <button
          type="button"
          className="attachment-image-btn"
          aria-label="Open image in viewer"
          onClick={() => setLightboxOpen(true)}
          onContextMenu={(e) => {
            e.preventDefault();
            setMenuPos({ x: e.clientX, y: e.clientY });
          }}
        >
          <img
            src={url}
            alt={`Attachment ${attachment.id}`}
            className="attachment-image"
            loading="lazy"
            onLoad={(e) => {
              if (clamped) return;
              const img = e.currentTarget;
              if (img.naturalWidth && img.naturalHeight) {
                setNaturalRatio(img.naturalWidth / img.naturalHeight);
              }
            }}
          />
        </button>
      )}
      {gatedByHover && !hovering && !loading && !error && tooLarge === null && (
        <div className="attachment-reduced-motion-hint" aria-hidden="true">
          Hover to play
        </div>
      )}
      {lightboxOpen && url && (
        <ImageLightbox
          src={url}
          alt={`Attachment ${attachment.id}`}
          onClose={() => setLightboxOpen(false)}
          onSave={handleSaveImage}
        />
      )}
      {menuPos && (
        <ContextMenu position={menuPos} onClose={() => setMenuPos(null)}>
          <ContextMenu.Item
            icon={<Maximize2 size={16} />}
            label="Open"
            onClick={() => {
              setMenuPos(null);
              setLightboxOpen(true);
            }}
          />
          <ContextMenu.Item
            icon={<Download size={16} />}
            label="Save image…"
            onClick={() => {
              setMenuPos(null);
              handleSaveImage();
            }}
          />
        </ContextMenu>
      )}
    </div>
  );
}

function MediaAttachment({ attachment, channelId }: AttachmentItemProps) {
  const [url, setUrl] = useState<string | null>(blobUrlCache.get(attachment.id)?.url ?? null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<AttachmentFailure>(null);
  // Keep this url alive while it is on screen (#2157 review, VULN-002-C):
  // eviction may not revoke a blob a mounted surface is still pointing at.
  useRetainedBlobUrl(attachment.id, url);
  const [tooLarge, setTooLarge] = useState<{ bytes: number; truncated: boolean } | null>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  // Videos auto-lazy-load so the browser can render the first frame as a natural
  // poster — no separate thumbnail pipeline needed. Audio stays click-to-load
  // because there's nothing visual to gain from prefetching the bytes.
  const autoLoad = attachment.file_type === 'video';

  const load = useCallback(async () => {
    if (url) return;
    setError(null);
    setLoading(true);
    try {
      const blobUrl = await fetchAndDecrypt(attachment.id, channelId, attachment.file_size);
      setUrl(blobUrl);
    } catch (err) {
      if (err instanceof AttachmentTooLargeError)
        setTooLarge({ bytes: err.byteLength, truncated: err.truncated });
      else setError(failureOf(err));
    } finally {
      setLoading(false);
    }
  }, [attachment.id, attachment.file_size, channelId, url]);

  useEffect(() => {
    if (!autoLoad || url) return;
    const el = containerRef.current;
    if (!el || typeof IntersectionObserver === 'undefined') return;
    const observer = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          load();
          observer.disconnect();
        }
      },
      { rootMargin: '200px' }
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, [autoLoad, url, load]);

  const Icon = attachment.file_type === 'video' ? Film : Music;
  const label = attachment.file_type === 'video' ? 'video' : 'audio';
  const sizeLabel = formatFileSize(attachment.file_size);
  // A format this build cannot read, and bytes that failed authentication, are
  // TERMINAL: retrying re-fetches the same bytes and fails the same way. Only a
  // generic failure (a dropped fetch, a transient 5xx) is worth another go.
  const terminal = error !== null && error !== 'generic';
  let loadTitle = `Load ${label}`;
  if (error === 'generic') loadTitle = `Failed to load ${label} — retry`;
  else if (loading) loadTitle = `Loading ${label}…`;

  return (
    <div className="attachment-media-container" ref={containerRef}>
      {url && attachment.file_type === 'video' && (
        <ThemedMediaPlayer src={url} variant="video" className="attachment-video" />
      )}
      {url && attachment.file_type === 'audio' && (
        <ThemedMediaPlayer src={url} variant="audio" className="attachment-audio" />
      )}
      {tooLarge !== null && (
        <AttachmentTooLargeNotice bytes={tooLarge.bytes} truncated={tooLarge.truncated} />
      )}
      {terminal && error !== null && (
        <div className="attachment-error" role="alert">
          {FAILURE_COPY[error]}
        </div>
      )}
      {!url && tooLarge === null && !terminal && (
        <button
          className="attachment-load-btn attachment-load-btn-rich"
          onClick={load}
          disabled={loading}
        >
          <Icon size={20} />
          <div className="attachment-load-info">
            <span className="attachment-load-title">{loadTitle}</span>
            <span className="attachment-load-meta">
              {attachment.mime_type} · {sizeLabel}
            </span>
          </div>
        </button>
      )}
    </div>
  );
}

function FileAttachment({ attachment, channelId }: AttachmentItemProps) {
  const [downloading, setDownloading] = useState(false);
  const [tooLarge, setTooLarge] = useState<{ bytes: number; truncated: boolean } | null>(null);
  // Holds the url the download is reading.
  //
  // An earlier version deliberately did NOT retain here, reasoning that no
  // eviction can interleave because there is no `await` between the load
  // resolving and `a.click()`. That covers the interval BEFORE the click and
  // not the one after it: `a.click()` starts an asynchronous browser download
  // that reads the blob over time, and for an attachment near the 256 MiB guard
  // that read is long-lived. Another attachment loading during it would evict
  // and revoke the url mid-read (#2837 review, row 1).
  const [downloadedUrl, setDownloadedUrl] = useState<string | null>(null);
  useRetainedBlobUrl(attachment.id, downloadedUrl);

  const handleDownload = async () => {
    setDownloading(true);
    try {
      const blobUrl = await fetchAndDecrypt(attachment.id, channelId, attachment.file_size);
      setDownloadedUrl(blobUrl);
      const a = document.createElement('a');
      a.href = blobUrl;
      a.download = `attachment-${attachment.id}`;
      document.body.appendChild(a);
      a.click();
      a.remove();
    } catch (err) {
      // A refusal is the one download failure worth surfacing on a file card:
      // retrying can never succeed, so the card says why.
      if (err instanceof AttachmentTooLargeError)
        setTooLarge({ bytes: err.byteLength, truncated: err.truncated });
      // Otherwise non-fatal: download failed, no UI feedback needed here.
    } finally {
      setDownloading(false);
    }
  };

  return (
    <div className="attachment-file-card">
      <FileIcon fileType={attachment.file_type} />
      <div className="attachment-file-info">
        <span className="attachment-file-type">{attachment.mime_type}</span>
        <span className="attachment-file-size">{formatFileSize(attachment.file_size)}</span>
        {tooLarge !== null && (
          <AttachmentTooLargeNotice bytes={tooLarge.bytes} truncated={tooLarge.truncated} />
        )}
      </div>
      <button
        className="attachment-download-btn"
        onClick={handleDownload}
        disabled={downloading}
        aria-label="Download attachment"
      >
        {downloading ? <Loader2 size={16} className="spinner" /> : <Download size={16} />}
      </button>
    </div>
  );
}

const AttachmentDisplay: React.FC<AttachmentDisplayProps> = ({
  attachments,
  channelId,
  messageBody,
}) => {
  if (!attachments || attachments.length === 0) return null;

  return (
    <div className="attachment-display">
      {attachments.map((attachment) => {
        if (attachment.file_type === 'file' && attachment.mime_type === 'text/markdown') {
          return (
            <OverflowMarkdownAttachment
              key={attachment.id}
              attachment={attachment}
              previewBody={messageBody ?? ''}
              channelId={channelId}
            />
          );
        }
        if (attachment.file_type === 'photo' || attachment.file_type === 'animated') {
          return (
            <ImageAttachment key={attachment.id} attachment={attachment} channelId={channelId} />
          );
        }
        if (attachment.file_type === 'video' || attachment.file_type === 'audio') {
          return (
            <MediaAttachment key={attachment.id} attachment={attachment} channelId={channelId} />
          );
        }
        return <FileAttachment key={attachment.id} attachment={attachment} channelId={channelId} />;
      })}
    </div>
  );
};

export default AttachmentDisplay;
