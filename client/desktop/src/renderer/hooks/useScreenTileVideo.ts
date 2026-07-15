import { useEffect, useId, useRef } from 'react';
import type { RemoteVideoRole } from '../services/remoteVideoLayerPolicy';
import { useRenderStateReporter } from './useRenderStateReporter';

export interface UseScreenTileVideoOptions {
  /**
   * Producing user of a REMOTE screen share — set only for a remote share so
   * this surface's rendered size feeds receiver screen layer demand (#1924).
   * Undefined for a local share (we never consume our own screen); the
   * reporter is inert in that case.
   */
  sharerUserId?: string;
  stream?: MediaStream;
  /** Local share detached by Auto-Pause — the <video> is not rendered. */
  isPaused?: boolean;
  /** Render-size role bucket for this surface. */
  role: RemoteVideoRole;
}

/**
 * Shared wiring for a screen-share tile surface: owns the <video> element
 * ref, reports the tile's rendered size/visibility for receiver screen layer
 * demand, and attaches/detaches the MediaStream to the element.
 *
 * #1924 invariant: EVERY surface that renders a remote screen <video> must
 * report demand — screen consumers seed at spatial layer 0 and only ramp up
 * on reported render-size demand, so an unwired surface stays blurry. Routing
 * all tile surfaces through this hook makes that wiring structural instead of
 * per-surface copy discipline (see [internal]rules/frontend.md § casting
 * eligibility toggles).
 */
export function useScreenTileVideo(
  options: UseScreenTileVideoOptions
): React.RefObject<HTMLVideoElement | null> {
  const { sharerUserId, stream, isPaused = false, role } = options;
  const videoRef = useRef<HTMLVideoElement>(null);
  const tileId = useId();

  useRenderStateReporter({
    userId: sharerUserId ?? '',
    tileId,
    source: 'screen',
    elementRef: videoRef,
    role,
    enabled: !!sharerUserId && !!stream && !isPaused,
  });

  useEffect(() => {
    const el = videoRef.current;
    if (!el) return;
    if (stream) {
      el.srcObject = stream;
      el.play().catch(() => {});
    } else {
      el.srcObject = null;
    }
    return () => {
      // el is the element captured at setup, not whatever videoRef.current
      // might point at by the time cleanup runs.
      el.srcObject = null;
    };
  }, [stream]);

  return videoRef;
}
