import React, { useState, useRef, useEffect } from 'react';
import { ImageOff, Pause } from 'lucide-react';
import { gifProvider, type GifResolved } from '../../services/messaging/gifProvider';
import {
  resolveGifPlayback,
  type GifPlaybackMode,
  type GifPlaybackVerdict,
} from '../../utils/ui/gifPlayback';
import { useWindowFocus } from '../../hooks/ui/useWindowFocus';
import './GifEmbed.css';

interface GifEmbedProps {
  slug: string;
  /** The user's stored gate; 'auto' follows Reduce Animations (#2369). */
  mode: GifPlaybackMode;
  reduceAnimations: boolean;
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
 *
 *  Playback is two axes (#2369, spec §0): the resolved GATE chooses the
 *  element (still vs animated), and window FOCUS chooses whether the mounted
 *  animated element runs. The video kind therefore keeps its `<video>` mounted
 *  and pauses it — a true pause with a true resume and no re-request — while
 *  the img kind, which has no pause API, swaps its `src` to the still.
 *  When `loadAutomatically` is false, the embed shows a "Click to load"
 *  placeholder until the user explicitly taps it.
 *
 *  GIFs intentionally do NOT honor `embeds_suppressed` — that flag gates
 *  link previews / image thumbnails / off-app trackers, while GIFs are an
 *  explicit user-chosen attachment. Privacy → "Load GIFs from KLIPY
 *  automatically" is the dedicated control for them.
 */
/** Choose what the embed renders. Extracted from the component purely to keep
 *  its cognitive complexity under the limit (SonarQube S3776): the branch chain
 *  is the complex part and it has no business being inline. Deliberately a
 *  plain function rather than a component — called, not mounted — so React
 *  sees the identical element tree and reconciliation is unchanged. */

/** The GIF/WEBP animated-image body. ONE element with a computed `src`, never
 *  two JSX branches: React reuses the DOM node and nothing reflows. The still
 *  costs one fetch per embed on first blur, then rides `max-age=3600`.
 *
 *  Split out of `renderGifBody` for the same reason `renderGifBody` was split
 *  out of the component: this arm stopped being a shape and started carrying
 *  its own source and handler decisions, which pushed the dispatcher one point
 *  past S3776's ceiling. Extracting it takes the dispatcher well clear rather
 *  than to exactly 15, so the next edit here does not re-break the gate. */
function renderAnimatedImageBody(args: {
  resolved: GifResolved;
  verdict: GifPlaybackVerdict;
  display: { width: number; height: number };
  hasUsableStill: boolean;
  stoppedBox: React.ReactNode;
  onImageError: () => void;
  onStillError: () => void;
}): React.ReactNode {
  const { resolved, verdict, display, hasUsableStill, stoppedBox, onImageError, onStillError } =
    args;
  return (
    <>
      {verdict.playing || hasUsableStill ? (
        <img
          src={verdict.playing ? resolved.animatedUrl : resolved.stillUrl}
          alt={`GIF from ${gifProvider.name}`}
          width={display.width}
          height={display.height}
          className="gif-embed-image loaded"
          // ONE element, TWO sources, so the handler must ask which one failed.
          // Animated failing is terminal; the still failing is not.
          onError={verdict.playing ? onImageError : onStillError}
          draggable={false}
        />
      ) : (
        stoppedBox
      )}
      <span className="gif-embed-attribution">{gifProvider.poweredByText}</span>
    </>
  );
}

function renderGifBody(args: {
  shouldLoad: boolean;
  error: boolean;
  resolved: GifResolved | null;
  verdict: GifPlaybackVerdict;
  display: { width: number; height: number };
  hasUsableStill: boolean;
  stoppedBox: React.ReactNode;
  videoRef: React.RefObject<HTMLVideoElement | null>;
  onLoadClick: () => void;
  onImageError: () => void;
  onStillError: () => void;
}): React.ReactNode {
  const {
    shouldLoad,
    error,
    resolved,
    verdict,
    display,
    hasUsableStill,
    stoppedBox,
    videoRef,
    onLoadClick,
    onImageError,
    onStillError,
  } = args;
  let body: React.ReactNode;
  if (!shouldLoad) {
    body = (
      <button
        className="gif-embed-placeholder"
        onClick={onLoadClick}
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
  } else if (verdict.surface === 'still') {
    // Gate is 'hover' and the pointer is elsewhere: render the still frame as a
    // static image. Hover / focus flips `hovering` true and falls through to the
    // animated branches below. Reaching this branch for the video kind is what
    // keeps Reduce-Animations users from fetching any mp4 at all (spec A6).
    body = (
      <>
        {hasUsableStill ? (
          <img
            src={resolved.stillUrl}
            alt={`GIF from ${gifProvider.name}`}
            className="gif-embed-image loaded"
            width={display.width}
            height={display.height}
            // A still that 404s must NOT kill the embed: the still is only the
            // PAUSE surface, and the animated URL beside it may be perfectly
            // good. `onStillError` retires the still alone, so this branch falls
            // to `stoppedBox` and refocus still returns real animation. Routing
            // it to `onImageError` (as this did) made the hover gate terminal.
            onError={onStillError}
            draggable={false}
          />
        ) : (
          stoppedBox
        )}
        <span className="gif-embed-attribution">{gifProvider.poweredByText}</span>
      </>
    );
  } else if (resolved.animatedKind === 'video') {
    // MP4 / WEBM: use a <video> element with the still frame as the poster
    body = (
      <>
        <video
          ref={videoRef}
          src={resolved.animatedUrl}
          poster={resolved.stillUrl}
          // Conditional so a video mounting while the window is unfocused never
          // starts at all. The effect above is what handles every later
          // transition; this only removes the one frame of playback between
          // mount and the effect firing.
          autoPlay={verdict.playing}
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
    body = renderAnimatedImageBody({
      resolved,
      verdict,
      display,
      hasUsableStill,
      stoppedBox,
      onImageError,
      onStillError,
    });
  }
  return body;
}

const GifEmbed: React.FC<GifEmbedProps> = ({ slug, mode, reduceAnimations, loadAutomatically }) => {
  const [resolved, setResolved] = useState<GifResolved | null>(null);
  const [error, setError] = useState(false);
  // Retires the still source ALONE. Separate from `error`, which is terminal:
  // `renderGifBody` checks `error` before every playback branch, so folding a
  // still failure into it made "GIF unavailable" permanent for an embed whose
  // animation still worked. Found by Codex review on PR #3291.
  const [stillError, setStillError] = useState(false);
  const [userClicked, setUserClicked] = useState(false);
  const [isVisible, setIsVisible] = useState(false);
  // Hover/focus state for reduce-motion mode: show the still frame by default
  // and play the animation only while the user is pointing at / focused on
  // the embed. On mouseleave / blur we snap back to the still. See QA bug
  // #571 item #6B.
  // TWO booleans, not one. They are independent conditions and collapsing them
  // loses playback in both directions: `onMouseLeave` would stop a GIF the user
  // is still focused on with the keyboard, and `onBlur` would stop one the
  // pointer is still resting over. Play when EITHER holds.
  // Found by Codex review on PR #3291.
  const [pointerOver, setPointerOver] = useState(false);
  const [focusWithin, setFocusWithin] = useState(false);
  const hovering = pointerOver || focusWithin;
  const containerRef = useRef<HTMLDivElement>(null);
  const videoRef = useRef<HTMLVideoElement>(null);
  // Subscribed here rather than threaded down from Message: the primitive is a
  // module singleton, so N embeds cost one listener pair, and a focus change
  // re-renders only the embeds rather than every message row.
  const windowFocused = useWindowFocus();
  const verdict = resolveGifPlayback({ mode, reduceAnimations, hovering, windowFocused });

  // Drive the mounted <video> rather than unmounting it: pausing costs nothing
  // and resuming re-requests nothing, which is what keeps a blur/focus cycle off
  // the 300/min /klipy/media budget entirely (spec X7).
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    if (!verdict.playing) {
      v.pause();
      return;
    }
    // `play()` is SPECIFIED to return a Promise, and both halves of this guard
    // are load-bearing anyway: jsdom returns `undefined` (so `.catch` on it is a
    // TypeError), and a synchronous throw is reachable under an autoplay policy
    // refusal. A refused start leaves the element paused, which is a correct
    // degradation — there is nothing to report and nothing to log
    // (logBufferService captures console.* into a buffer that reaches a public
    // repo through the feedback pipeline).
    try {
      const started: Promise<void> | undefined = v.play();
      if (started) void started.catch(() => undefined);
    } catch {
      /* autoplay refused — element stays paused */
    }
    // `resolved` is in the deps because the <video> MOUNTS when it arrives, which
    // can happen long after `verdict.playing` last changed: blur the window while
    // getBySlug() is still in flight and `playing` is false for the whole
    // resolution, so an effect keyed only on it never re-runs against the element
    // that did not exist the first time. Found by Codex review on PR #3291.
  }, [verdict.playing, resolved]);

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

  // A KLIPY "still" is frequently the ANIMATED url under another name:
  // `toCategory` assigns `stillUrl: proxied` — the same variable — and
  // `toResolved` falls back through `mp4 ?? webp ?? gif` whenever `item.still`
  // is absent. Swapping `src` to an aliased still is a no-op and the image
  // keeps animating, so for those GIFs the ONLY stop available is taking the
  // element off screen. This affects the hover gate exactly as much as the
  // unfocus pause — Reduce Animations was already silently inert on these.
  // Found by Codex review on PR #3291.
  const hasUsableStill =
    resolved !== null && resolved.stillUrl !== resolved.animatedUrl && !stillError;
  // The copy here is VISIBLE, not merely an accessible name. The spec's §2.4
  // rule was "exactly one surface renders paused chrome: ImageAttachment,
  // because it is the only one whose paused state is a blank box" — and the
  // aliased-still fix made this a second such surface, so that reasoning now
  // reaches here too: an unexplained grey rectangle reads as a broken embed.
  // Same two copies as the attachment, chosen the same way — one names an
  // action available now, the other a cause hovering cannot fix, and unfocus
  // wins when both hold. Real text also settles SonarQube S6819: no role="img"
  // is needed when the element has a native name. Found by Codex on PR #3291.
  const stoppedBox = (
    <div className="gif-embed-stopped" style={{ width: display.width, height: display.height }}>
      <Pause size={14} aria-hidden="true" />
      <span>{windowFocused ? 'Hover to play' : 'Paused — Concord is in the background'}</span>
    </div>
  );

  const body = renderGifBody({
    shouldLoad,
    error,
    resolved,
    verdict,
    display,
    hasUsableStill,
    stoppedBox,
    videoRef,
    onLoadClick: () => setUserClicked(true),
    onImageError: () => setError(true),
    onStillError: () => setStillError(true),
  });

  // Lock the container to the (clamped) final size so the message bubble
  // reserves the right vertical space from the first frame. The placeholder
  // and error states use intrinsic sizing (no inline style) so they don't
  // get stretched to a 250x180 box when nothing's loaded yet.
  const containerStyle: React.CSSProperties | undefined =
    shouldLoad && !error
      ? { width: `${display.width}px`, height: `${display.height}px` }
      : undefined;

  // Pointer and focus are tracked UNCONDITIONALLY, even under an 'always' gate
  // where they change nothing. Attaching them only while the gate is 'hover'
  // means a pointer that leaves during an 'always' spell never fires
  // `onMouseLeave`, so the flag stays stuck true and a later return to 'hover'
  // plays a GIF nobody is pointing at. React delegates all four at the root, so
  // this costs fiber props, not four DOM listeners per embed. The resolved gate
  // still decides the TAB STOP below. CodeRabbit review, PR #3291.
  // The wrapper is a tab stop ONLY when loaded media is sitting there waiting to
  // be played. Unlike AttachmentDisplay, this component has no always-mounted
  // focusable child to inherit focus from — its <video> is explicitly
  // tabIndex={-1} — so removing the tab stop outright would make a hover-gated
  // embed unreachable by keyboard. But in the not-yet-loaded state the "Click to
  // load GIF" button is already focusable, and in the error and skeleton states
  // there is nothing to play, so a tab stop there is a stop on nothing.
  // Found by Codex review on PR #3291.
  const mediaAwaitingPlay = shouldLoad && !error && resolved !== null;
  const hoverHandlers = {
    onMouseEnter: () => setPointerOver(true),
    onMouseLeave: () => setPointerOver(false),
    onFocus: () => setFocusWithin(true),
    onBlur: () => setFocusWithin(false),
    ...(verdict.gate === 'hover' && mediaAwaitingPlay ? { tabIndex: 0 } : {}),
  };

  return (
    <div ref={containerRef} className="gif-embed" style={containerStyle} {...hoverHandlers}>
      {body}
    </div>
  );
};

export default React.memo(GifEmbed);
