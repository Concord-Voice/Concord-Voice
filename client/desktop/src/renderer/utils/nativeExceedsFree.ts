import type { Entitlement } from '../stores/subscriptionStore';

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
 * Pure L6 native-exceeds guard (#1301 / spec §3 L6). Given the detected native
 * device caps + the (free) entitlement, decide whether the device can do MORE
 * than the free ceiling allows — if so, the host shows the inline note "Your
 * device supports more — unlock with Premium" and clamps the option ceiling to
 * the free caps.
 *
 *  - `exceeds`       → true when EITHER native height OR native fps is above the
 *                      free cap (the device is capable beyond what free allows).
 *  - `clampedHeight` → min(nativeHeight, maxVideoHeight) — the option ceiling the
 *                      host should offer (never above the free cap).
 *  - `clampedFps`    → min(nativeFps, maxVideoFps) — same, for frame rate.
 *
 * Non-mutating, store-free; unit-testable in isolation.
 */
export function nativeExceedsFree(
  native: NativeCaps,
  entitlement: Entitlement
): { exceeds: boolean; clampedHeight: number; clampedFps: number } {
  const clampedHeight = Math.min(native.nativeHeight, entitlement.maxVideoHeight);
  const clampedFps = Math.min(native.nativeFps, entitlement.maxVideoFps);
  const exceeds =
    native.nativeHeight > entitlement.maxVideoHeight || native.nativeFps > entitlement.maxVideoFps;
  return { exceeds, clampedHeight, clampedFps };
}
