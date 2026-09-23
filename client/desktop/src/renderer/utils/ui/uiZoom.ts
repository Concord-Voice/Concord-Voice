/**
 * UI Scale on Electron page zoom (#2367 part 2). Pure helpers plus the capability
 * probe; the applier that calls the bridge lives beside the other DOM sinks in
 * `stores/ui/settingsStore.ts`. Design:
 * `[internal]specs/2026-09-23-2367-ui-zoom-design.md`.
 *
 * TWO ENGINES, CHOSEN BY CAPABILITY, NEVER BOTH.
 *  - A shell whose preload exposes `electron.window.setZoomFactor` (IPC contract
 *    v29+) gets real page zoom: every CSS pixel scales, and `innerWidth`, media
 *    queries and pointer coordinates stay consistent. `--ui-scale` is then pinned
 *    to 1, or text would be scaled twice.
 *  - An older shell keeps the pre-#2367 behaviour exactly: `--ui-scale` carries the
 *    choice, clamped to the legacy 0.85–1.3 band. `SPA_MIN_CONTRACT` stays 19
 *    because this degrades rather than demands.
 */

/** Narrowest window, in unzoomed CSS px, the layout is known to survive. It is
 *  the shell's `minWidth` (`browserWindowConfig.ts`): real 2x in a W-wide window
 *  lays out exactly like 1x at W/2, and below 800 the fixed sidebars crush the
 *  chat column (spec § Why). */
export const UI_ZOOM_LAYOUT_FLOOR_PX = 800;

/** The band `--ui-scale` is clamped to on a shell without the zoom bridge. The
 *  upper bound is where narrow surfaces started collapsing under the CSS-variable
 *  engine, which scales text but not the boxes around it. */
export const UI_SCALE_LEGACY_MIN = 0.85;
export const UI_SCALE_LEGACY_MAX = 1.3;

/**
 * Smallest change in the applied factor worth a `setZoomFactor` call.
 *
 * Why this cannot loop. Zooming fires `resize`, and the resize handler recomputes
 * from `innerWidth × applied`. `innerWidth` is an integer, so that product is off
 * from the true unzoomed width by at most `applied` px (≤ 2), which moves the
 * width cap by at most 2/800 = 0.0025 — a quarter of this epsilon. A zoom-induced
 * resize therefore always lands inside the guard, and only a real window resize
 * of roughly 8 px or more can move the factor.
 */
export const UI_ZOOM_EPSILON = 0.01;

export type SetZoomFactor = (factor: number) => void;

/**
 * The zoom bridge, or `null` on a shell that predates it. The PRESENCE of the
 * function is the capability probe (the `audiocap.onCapability` precedent), read
 * structurally so the probe is total whatever the preload happens to expose.
 */
export function getZoomBridge(): SetZoomFactor | null {
  const namespace: unknown = globalThis.electron?.window;
  if (typeof namespace !== 'object' || namespace === null) return null;
  if (!('setZoomFactor' in namespace)) return null;
  const fn = namespace.setZoomFactor;
  return typeof fn === 'function' ? (fn as SetZoomFactor) : null;
}

export function hasZoomBridge(): boolean {
  return getZoomBridge() !== null;
}

/** PiP windows (`#/pip/…`, the check `App.tsx` uses) never apply zoom: a 160 px
 *  tile at 50 % or 200 % is broken, and page zoom does not propagate between
 *  windows, so leaving it alone keeps it at 1x. */
export function isPipWindow(): boolean {
  return globalThis.location?.hash?.startsWith('#/pip/') ?? false;
}

/** The pre-#2367 `--ui-scale` value for a chosen scale. Total: a non-finite
 *  input is the neutral 1. */
export function legacyUiScale(chosen: number): number {
  if (!Number.isFinite(chosen)) return 1;
  return Math.min(UI_SCALE_LEGACY_MAX, Math.max(UI_SCALE_LEGACY_MIN, chosen));
}

/** What `--ui-scale` must carry on this shell: 1 when page zoom does the
 *  scaling, otherwise the legacy clamp. Every consumer that reasons about
 *  `--font-scale` (e.g. the voice grid's pill band) must use THIS, never the
 *  stored choice. Reads the capability itself, so no caller can pass a stale one. */
export function cssUiScaleFor(chosen: number): number {
  return hasZoomBridge() ? 1 : legacyUiScale(chosen);
}

/**
 * The page zoom to apply for a chosen scale in a window `unzoomedWidth` CSS px
 * wide at zoom 1. Zoom-out is never capped; zoom-in is capped so the zoomed
 * layout is never narrower than `UI_ZOOM_LAYOUT_FLOOR_PX`, and never below 1 —
 * a window narrower than the floor simply gets no zoom-in.
 *
 * Callers derive `unzoomedWidth` as `innerWidth × currentZoom`, because
 * `innerWidth` itself shrinks as the page zooms.
 */
export function computeAppliedZoom(chosen: number, unzoomedWidth: number): number {
  if (!Number.isFinite(chosen)) return 1;
  if (chosen <= 1) return chosen;
  const widthCap =
    Number.isFinite(unzoomedWidth) && unzoomedWidth > 0
      ? Math.max(1, unzoomedWidth / UI_ZOOM_LAYOUT_FLOOR_PX)
      : 1;
  return Math.min(chosen, widthCap);
}
