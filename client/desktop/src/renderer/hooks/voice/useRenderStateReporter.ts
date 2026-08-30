import { useEffect } from 'react';
import type { RemoteVideoRole } from '../../services/remoteVideoLayerPolicy';

/** The source-tagged render surface a report targets. Camera and screen keep
 *  independent receiver-demand maps + server gates (#1924). */
type RemoteVideoSource = 'camera' | 'screen';

/** Narrow view of voiceService this hook needs — dynamically imported so the
 *  reporter never pulls the full service into a component's static import graph. */
interface RenderStateReporterService {
  setRemoteVideoRenderState(
    userId: string,
    tileId: string,
    state: {
      visible: boolean;
      cssWidth: number;
      cssHeight: number;
      role: RemoteVideoRole;
      focusedWindow: boolean;
    },
    source: RemoteVideoSource
  ): void;
  removeRemoteVideoTile(userId: string, tileId: string, source: RemoteVideoSource): void;
}

export interface UseRenderStateReporterOptions {
  /** Producing user's id (the peer whose camera/screen this element renders). */
  userId: string;
  /** Stable per-instance tile id (from React `useId()`). */
  tileId: string;
  /** Which layered surface this element renders. */
  source: RemoteVideoSource;
  /** Element whose on-screen size + visibility feed receiver layer demand. */
  elementRef: React.RefObject<HTMLElement | null>;
  /** Render-size role bucket for this surface. */
  role: RemoteVideoRole;
  /** When false the reporter is inert (local tile, or no live media rendered). */
  enabled: boolean;
}

/**
 * Reports a rendered remote video element's size + visibility to the voice service,
 * which drives receiver-side layer demand (`set-preferred-layers`). Extracted from
 * `ParticipantTile` (#1541) so the same proven IntersectionObserver +
 * `getBoundingClientRect` reporter can serve BOTH the camera tile and the screen
 * render surfaces (#1924). Camera behavior is unchanged — the only generalization is
 * the `source` tag threaded to `setRemoteVideoRenderState` / `removeRemoteVideoTile`.
 *
 * The voice service is imported lazily; if it isn't ready (early teardown race) the
 * reporter degrades to a no-op — demand is an optimization, never load-bearing.
 */
export function useRenderStateReporter({
  userId,
  tileId,
  source,
  elementRef,
  role,
  enabled,
}: UseRenderStateReporterOptions): void {
  useEffect(() => {
    if (!enabled) return;
    const el = elementRef.current;
    if (!el) return;
    let disposed = false;
    let svc: RenderStateReporterService | null = null;
    let lastIntersecting = false;

    const report = () => {
      if (disposed || !svc) return;
      const rect = el.getBoundingClientRect();
      const windowVisible = document.visibilityState !== 'hidden';
      svc.setRemoteVideoRenderState(
        userId,
        tileId,
        {
          // Screen: a backgrounded/minimized window means the viewer is no longer
          // watching, so the tile stops contributing gate demand — otherwise a stale
          // `visible: true` demand would pin `screen-layering-gate` on and keep
          // publishers on 3-layer simulcast (Codex #1924 review). Camera keeps its
          // original semantics (visible == element intersection) unchanged.
          visible: source === 'screen' ? lastIntersecting && windowVisible : lastIntersecting,
          cssWidth: rect.width,
          cssHeight: rect.height,
          role,
          focusedWindow: windowVisible,
        },
        source
      );
    };

    const observer = new IntersectionObserver(
      ([entry]) => {
        lastIntersecting = entry.isIntersecting;
        report();
      },
      { threshold: 0 }
    );

    // #1924: IntersectionObserver does NOT fire when the WINDOW is backgrounded (the
    // element still intersects the viewport), so re-report on document visibilitychange
    // to update the demand on hide/restore. Without this the server keeps counting a
    // stale visible screen demand after the viewer minimizes Concord.
    const onVisibilityChange = () => report();
    document.addEventListener('visibilitychange', onVisibilityChange);

    // #1924: IntersectionObserver + visibilitychange both miss SIZE-only changes —
    // dragging a StreamBar / UserFrameBar / text-chat splitter or resizing the window
    // keeps the element visible and intersecting but changes its rendered box, which the
    // SFU layer picker keys on. Mirror the PiP fix: a ResizeObserver re-reports through
    // the same report() path on box changes. Skip a sub-1px (not-yet-laid-out) box.
    const resizeObserver =
      typeof ResizeObserver === 'undefined'
        ? null
        : new ResizeObserver(() => {
            const rect = el.getBoundingClientRect();
            if (rect.width < 1 || rect.height < 1) return;
            report();
          });
    resizeObserver?.observe(el);
    void import('../../services/voiceService')
      .then((m) => {
        if (disposed) return;
        const candidate = m.voiceService as Partial<RenderStateReporterService>;
        if (
          typeof candidate.setRemoteVideoRenderState !== 'function' ||
          typeof candidate.removeRemoteVideoTile !== 'function'
        ) {
          return;
        }
        svc = {
          setRemoteVideoRenderState: candidate.setRemoteVideoRenderState.bind(candidate),
          removeRemoteVideoTile: candidate.removeRemoteVideoTile.bind(candidate),
        };
        observer.observe(el);
      })
      .catch((err: unknown) => {
        // Render-state demand is an optimization; ignore late import teardown races.
        if (!disposed) console.debug('Voice render-state reporter unavailable', err);
      });
    return () => {
      disposed = true;
      observer.disconnect();
      resizeObserver?.disconnect();
      document.removeEventListener('visibilitychange', onVisibilityChange);
      // Deregister this tile (NOT "report hidden") so a closing surface doesn't freeze
      // media still visible in another surface (grid / bar / stage / PiP).
      svc?.removeRemoteVideoTile(userId, tileId, source);
    };
  }, [enabled, userId, tileId, source, role, elementRef]);
}
