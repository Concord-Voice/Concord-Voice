import React, { useState, useEffect, useRef, useCallback } from 'react';
import { Download, FileText, Film, Music, File, Loader2 } from 'lucide-react';
import { apiFetch } from '../../services/apiClient';
import { e2eeService } from '../../services/e2eeService';
import { decryptFile, formatFileSize } from '../../utils/attachmentCrypto';
import type { AttachmentSummary } from '../../types/chat';
import { useSettingsStore } from '../../stores/settingsStore';
import OverflowMarkdownAttachment from './OverflowMarkdownAttachment';
import ThemedMediaPlayer from './ThemedMediaPlayer';
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
const BLOB_CACHE_MAX = 50;
const blobUrlCache = new Map<string, string>();

function cacheBlobUrl(fileId: string, url: string): void {
  // Evict oldest entry if at capacity (Map preserves insertion order)
  if (blobUrlCache.size >= BLOB_CACHE_MAX) {
    const oldest = blobUrlCache.keys().next().value;
    if (oldest !== undefined) {
      const oldestUrl = blobUrlCache.get(oldest);
      if (oldestUrl !== undefined) {
        URL.revokeObjectURL(oldestUrl);
      }
      blobUrlCache.delete(oldest);
    }
  }
  blobUrlCache.set(fileId, url);
}

async function fetchAndDecrypt(fileId: string, channelId: string): Promise<string> {
  const cached = blobUrlCache.get(fileId);
  if (cached) {
    // Move to end for LRU freshness
    blobUrlCache.delete(fileId);
    blobUrlCache.set(fileId, cached);
    return cached;
  }

  const response = await apiFetch(`/api/v1/media/attachments/${fileId}`);
  if (!response.ok) throw new Error(`Failed to fetch attachment (${response.status})`);

  let data = await response.arrayBuffer();
  const mimeType = response.headers.get('X-File-Mime-Type') || 'application/octet-stream';

  const channelKey = await e2eeService.getChannelKey(channelId);
  data = await decryptFile(data, channelKey);

  const blob = new Blob([data], { type: mimeType });
  const url = URL.createObjectURL(blob);
  cacheBlobUrl(fileId, url);
  return url;
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

function ImageAttachment({ attachment, channelId }: AttachmentItemProps) {
  const reduceAnimations = useSettingsStore((s) => s.appearance.reduceAnimations);
  // Animated GIF attachments under Reduce Animations play only on hover/focus.
  // See QA bug #571 item #6B. Static photos ignore this flag entirely.
  const isAnimated = attachment.file_type === 'animated';
  const gatedByHover = isAnimated && reduceAnimations;
  const [hovering, setHovering] = useState(false);
  const [url, setUrl] = useState<string | null>(blobUrlCache.get(attachment.id) || null);
  const [loading, setLoading] = useState(!url);
  const [error, setError] = useState(false);
  // Aspect ratio learned from <img onLoad> as a fallback for messages whose
  // summary lacks pre-known width/height (e.g. older history rows fetched
  // from the server before the dim-plumbing was in place).
  const [naturalRatio, setNaturalRatio] = useState<number | null>(null);
  const observerRef = useRef<IntersectionObserver | null>(null);
  const containerRef = useRef<HTMLDivElement>(null);

  const load = useCallback(async () => {
    try {
      setLoading(true);
      const blobUrl = await fetchAndDecrypt(attachment.id, channelId);
      setUrl(blobUrl);
    } catch {
      // Fetch or decryption failed — show inline error state
      setError(true);
    } finally {
      setLoading(false);
    }
  }, [attachment.id, channelId]);

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
      {error && <div className="attachment-error">Failed to load image</div>}
      {showLiveImage && (
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
      )}
      {gatedByHover && !hovering && !loading && !error && (
        <div className="attachment-reduced-motion-hint" aria-hidden="true">
          Hover to play
        </div>
      )}
    </div>
  );
}

function MediaAttachment({ attachment, channelId }: AttachmentItemProps) {
  const [url, setUrl] = useState<string | null>(blobUrlCache.get(attachment.id) || null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  // Videos auto-lazy-load so the browser can render the first frame as a natural
  // poster — no separate thumbnail pipeline needed. Audio stays click-to-load
  // because there's nothing visual to gain from prefetching the bytes.
  const autoLoad = attachment.file_type === 'video';

  const load = useCallback(async () => {
    if (url) return;
    setError(false);
    setLoading(true);
    try {
      const blobUrl = await fetchAndDecrypt(attachment.id, channelId);
      setUrl(blobUrl);
    } catch {
      setError(true);
    } finally {
      setLoading(false);
    }
  }, [attachment.id, channelId, url]);

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
  let loadTitle = `Load ${label}`;
  if (error) loadTitle = `Failed to load ${label} — retry`;
  else if (loading) loadTitle = `Loading ${label}…`;

  return (
    <div className="attachment-media-container" ref={containerRef}>
      {url && attachment.file_type === 'video' && (
        <ThemedMediaPlayer src={url} variant="video" className="attachment-video" />
      )}
      {url && attachment.file_type === 'audio' && (
        <ThemedMediaPlayer src={url} variant="audio" className="attachment-audio" />
      )}
      {!url && (
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

  const handleDownload = async () => {
    setDownloading(true);
    try {
      const blobUrl = await fetchAndDecrypt(attachment.id, channelId);
      const a = document.createElement('a');
      a.href = blobUrl;
      a.download = `attachment-${attachment.id}`;
      document.body.appendChild(a);
      a.click();
      a.remove();
    } catch {
      // Non-fatal: download failed, no UI feedback needed for file cards
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
