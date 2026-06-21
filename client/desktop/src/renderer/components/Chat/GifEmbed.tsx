import React, { useState, useRef, useEffect } from 'react';
import { ImageOff } from 'lucide-react';
import { gifProvider, type GifResolved } from '../../services/gifProvider';
import './GifEmbed.css';

interface GifEmbedProps {
  slug: string;
  reduceMotion: boolean;
  loadAutomatically: boolean;
}

const GIF_MAX_W = 400;
const GIF_MAX_H = 300;
// Default skeleton ratio used until we know the actual GIF dimensions.
const SKELETON_W = 250;
const SKELETON_H = 180;

/** Clamp a (width, height) pair into the GIF embed display box, preserving
 *  aspect ratio. We compute the final rendered size up-front so the container
 *  reserves exactly that space — without this, the browser briefly paints the
 *  natural intrinsic size before the CSS max-width/max-height clamp kicks in,
 *  causing a visible vertical "expand then settle" jump on send.
 */
function clampGifSize(w: number | undefined, h: number | undefined) {
  if (!w || !h) return { width: SKELETON_W, height: SKELETON_H };
  const ratio = Math.min(GIF_MAX_W / w, GIF_MAX_H / h, 1);
  return { width: Math.round(w * ratio), height: Math.round(h * ratio) };
}

/** Inline GIF embed for chat messages.
 *
 *  Resolves a `slug` to a vendor-specific URL via the active `gifProvider`,
 *  then renders an MP4 `<video>` (preferred) or a GIF/WEBP `<img>` fallback.
 *  URLs are passed through verbatim — never construct or modify them.
 *  Reduce-motion mode shows the still rendition as an `<img>` instead.
 *  When `loadAutomatically` is false, the embed shows a "Click to load"
 *  placeholder until the user explicitly taps it.
 *
 *  GIFs intentionally do NOT honor `embeds_suppressed` — that flag gates
 *  link previews / image thumbnails / off-app trackers, while GIFs are an
 *  explicit user-chosen attachment. Privacy → "Load GIFs from KLIPY
 *  automatically" is the dedicated control for them.
 */
const GifEmbed: React.FC<GifEmbedProps> = ({ slug, reduceMotion, loadAutomatically }) => {
  const [resolved, setResolved] = useState<GifResolved | null>(null);
  const [error, setError] = useState(false);
  const [userClicked, setUserClicked] = useState(false);
  const [isVisible, setIsVisible] = useState(false);
  // Hover/focus state for reduce-motion mode: show the still frame by default
  // and play the animation only while the user is pointing at / focused on
  // the embed. On mouseleave / blur we snap back to the still. See QA bug
  // #571 item #6B.
  const [hovering, setHovering] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  // Lazy load via IntersectionObserver — defer the network call until the
  // embed is actually about to enter the viewport.
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const observer = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          setIsVisible(true);
          observer.disconnect();
        }
      },
      { rootMargin: '200px' }
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const shouldLoad = loadAutomatically || userClicked;

  // Fetch the resolved URLs when the embed becomes visible AND the user is
  // willing to load (auto-load on, or explicitly clicked).
  useEffect(() => {
    if (!isVisible || !shouldLoad || resolved || error) return;
    let cancelled = false;
    gifProvider
      .getBySlug(slug)
      .then((r) => {
        if (!cancelled) setResolved(r);
      })
      .catch(() => {
        if (!cancelled) setError(true);
      });
    return () => {
      cancelled = true;
    };
  }, [isVisible, shouldLoad, slug, resolved, error]);

  // Pre-compute the clamped display size so the embed reserves exactly the
  // final rendered space from the first paint, eliminating the brief vertical
  // expand-then-settle when the video/img replaces the skeleton.
  const display = clampGifSize(resolved?.width, resolved?.height);

  let body: React.ReactNode;
  if (!shouldLoad) {
    body = (
      <button
        className="gif-embed-placeholder"
        onClick={() => setUserClicked(true)}
        aria-label="Click to load GIF"
      >
        <ImageOff size={24} />
        <span>Click to load GIF</span>
      </button>
    );
  } else if (error) {
    body = (
      <div className="gif-embed-error">
        <ImageOff size={20} />
        <span>GIF unavailable</span>
      </div>
    );
  } else if (!resolved) {
    body = <div className="gif-embed-skeleton" />;
  } else if (reduceMotion && !hovering) {
    // Reduce-motion, not hovering: render the still frame as a static image.
    // Hover / focus will flip `hovering` true and fall through to the
    // animated branches below.
    body = (
      <>
        <img
          src={resolved.stillUrl}
          alt={`GIF from ${gifProvider.name}`}
          className="gif-embed-image loaded"
          width={display.width}
          height={display.height}
          draggable={false}
        />
        <span className="gif-embed-attribution">{gifProvider.poweredByText}</span>
      </>
    );
  } else if (resolved.animatedKind === 'video') {
    // MP4 / WEBM: use a <video> element with the still frame as the poster
    body = (
      <>
        <video
          src={resolved.animatedUrl}
          poster={resolved.stillUrl}
          autoPlay
          loop
          muted
          playsInline
          width={display.width}
          height={display.height}
          className="gif-embed-video loaded"
          aria-label={`GIF from ${gifProvider.name}`}
        />
        <span className="gif-embed-attribution">{gifProvider.poweredByText}</span>
      </>
    );
  } else {
    // GIF or WEBP: animated image
    body = (
      <>
        <img
          src={resolved.animatedUrl}
          alt={`GIF from ${gifProvider.name}`}
          width={display.width}
          height={display.height}
          className="gif-embed-image loaded"
          onError={() => setError(true)}
          draggable={false}
        />
        <span className="gif-embed-attribution">{gifProvider.poweredByText}</span>
      </>
    );
  }

  // Lock the container to the (clamped) final size so the message bubble
  // reserves the right vertical space from the first frame. The placeholder
  // and error states use intrinsic sizing (no inline style) so they don't
  // get stretched to a 250x180 box when nothing's loaded yet.
  const containerStyle: React.CSSProperties | undefined =
    shouldLoad && !error
      ? { width: `${display.width}px`, height: `${display.height}px` }
      : undefined;

  // Hover/focus handlers only matter in reduce-motion mode — when motion is
  // allowed the embed always autoplays and these are no-ops on the DOM.
  const hoverHandlers = reduceMotion
    ? {
        onMouseEnter: () => setHovering(true),
        onMouseLeave: () => setHovering(false),
        onFocus: () => setHovering(true),
        onBlur: () => setHovering(false),
        tabIndex: 0,
      }
    : {};

  return (
    <div ref={containerRef} className="gif-embed" style={containerStyle} {...hoverHandlers}>
      {body}
    </div>
  );
};

export default React.memo(GifEmbed);
