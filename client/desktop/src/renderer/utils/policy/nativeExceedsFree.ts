import type { VideoAxisLimit } from './videoLimits';

/**
 * Detected native device A/V ceilings (L6). `VideoConfigSection` derives these
 * from `electron.getDisplayInfo()` — the best display's height and the maximum
 * refresh rate across displays (see `bestDisplay` / `maxRefreshRate` there).
 */
export interface NativeCaps {
  /** Largest display height the device can capture/encode, in pixels. */
  nativeHeight: number;
  /** Highest display refresh rate across displays, in Hz. */
  nativeFps: number;
}

/**
 * Pure L6 native-exceeds guard (#1301 / spec §3 L6; split axes #1602). Given the
 * detected native device caps + a video-axis ceiling (the STREAM axis for
 * screen-share, since these ceilings drive the screen-share resolution/fps option
 * lists), decide whether the device can do MORE than the axis allows — if so, the
 * host shows the inline note "Your device supports more — unlock with Premium" and
 * clamps the offered option ceiling to the axis caps.
 *
 *  - `exceeds`       → true when EITHER native height OR native fps is above the
 *                      axis cap (the device is capable beyond what the axis allows).
 *  - `clampedHeight` → min(nativeHeight, axis.height) — the option ceiling the host
 *                      should offer (never above the axis cap; native axis → native).
 *  - `clampedFps`    → min(nativeFps, axis.fps) — same, for frame rate.
 *
 * A native/uncapped axis (Infinity ceiling, the premium sentinel) never `exceeds`
 * and clamps to the native value. Non-mutating, store-free; unit-testable.
 */
export function nativeExceedsFree(
  native: NativeCaps,
  axis: VideoAxisLimit
): { exceeds: boolean; clampedHeight: number; clampedFps: number } {
  const clampedHeight = Math.min(native.nativeHeight, axis.height);
  const clampedFps = Math.min(native.nativeFps, axis.fps);
  const exceeds = native.nativeHeight > axis.height || native.nativeFps > axis.fps;
  return { exceeds, clampedHeight, clampedFps };
}
