import { useCallback, useEffect, useRef, useState } from 'react';

export interface HoverIntentOptions {
  /** Dwell inside the group before it counts as intent rather than a pass-by. */
  warmUpMs?: number;
  /** Grace after leaving before the group forgets it was warm. */
  coolDownMs?: number;
}

export interface HoverIntent {
  warm: boolean;
  /** Spread onto the GROUP element, not onto each item. */
  groupProps: {
    'data-warm': 'true' | 'false';
    onPointerEnter: () => void;
    onPointerLeave: () => void;
  };
}

/** Group-level hover intent: has the pointer settled in this row, or is it passing through?
 *
 *  Only the DELAY depends on this, never which item is showing — that stays pure CSS `:hover`,
 *  so the hook needs no per-item props, no id plumbing, and cannot disagree with what the
 *  pointer is actually over. One attribute on the container is the whole surface.
 *
 *  This replaces a CSS-only attempt that transitioned a registered `@property` with a 0s
 *  duration to schedule the same flip. It was measured and it did not work: the property set
 *  forward and never came back, so the group latched warm and the intent gate collapsed to
 *  zero permanently. The failure LOOKED like success — pills appeared promptly, which reads
 *  as snappy rather than broken — which is exactly why it needs real state and a real timer.
 *
 *  `warmUpMs` deliberately exceeds the CSS open delay. If the group warmed *during* the first
 *  pill's delay, that pill's `transition-delay` would be recomputed mid-wait and it would open
 *  early — the gate would fire late on entry and not at all thereafter, defeating both halves.
 *  Warming just after the first pill has opened means the first hover always pays in full.
 */
export function useHoverIntent({
  warmUpMs = 450,
  coolDownMs = 600,
}: HoverIntentOptions = {}): HoverIntent {
  const [warm, setWarm] = useState(false);
  const timerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const clearTimer = useCallback(() => {
    if (timerRef.current !== undefined) {
      clearTimeout(timerRef.current);
      timerRef.current = undefined;
    }
  }, []);

  // A pending timer outliving the component would call setState on an unmounted tree; the
  // header unmounts on every channel and conversation switch, so this is routine, not edge.
  useEffect(() => clearTimer, [clearTimer]);

  const onPointerEnter = useCallback(() => {
    // Cancels a pending cool-down as well as a pending warm-up, so re-entering within the
    // grace window keeps the group warm rather than restarting the gate.
    clearTimer();
    timerRef.current = setTimeout(() => setWarm(true), warmUpMs);
  }, [clearTimer, warmUpMs]);

  const onPointerLeave = useCallback(() => {
    clearTimer();
    timerRef.current = setTimeout(() => setWarm(false), coolDownMs);
  }, [clearTimer, coolDownMs]);

  return {
    warm,
    groupProps: { 'data-warm': warm ? 'true' : 'false', onPointerEnter, onPointerLeave },
  };
}
