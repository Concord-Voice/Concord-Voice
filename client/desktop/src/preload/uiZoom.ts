/**
 * Preload UI zoom bridge (#2367 part 2).
 *
 * WHAT IT IS. The UI Scale slider drives real Chromium page zoom through
 * `webFrame.setZoomFactor`, exposed to the main world as
 * `electron.window.setZoomFactor`. Page zoom scales every CSS pixel — raw `px`
 * font sizes, icons, widths — and keeps `innerWidth`, media queries and pointer
 * coordinates consistent, which the `--ui-scale` custom property never could.
 *
 * WHY THE PRELOAD CLAMPS AGAIN. The main world may be remote-SPA code served from
 * a CDN origin, so this module does not trust the renderer's own clamp. A value
 * that is not a finite number is IGNORED — a no-op, not a throw and not a
 * fallback to some default zoom — and anything else is clamped to
 * [UI_ZOOM_MIN, UI_ZOOM_MAX]. `webFrame.setZoomFactor` itself throws for a factor
 * `<= 0`, so the clamp is also what keeps a hostile argument from surfacing a
 * preload exception in the main world.
 *
 * WHAT IT GRANTS, stated so it is neither oversold nor undersold. The caller can
 * set its OWN frame's zoom inside [0.5, 2]. That is no more than CSS `zoom` or a
 * `transform` on its own document already allows: no IPC channel, no
 * main-process handler, no other window (a measured Electron spike found the zoom
 * does not propagate to the same-origin PiP window). Hence no sender check —
 * there is no sender; this runs in the frame it zooms.
 *
 * WHY ITS OWN MODULE rather than inline in `preload.ts`: the same reason as
 * `audiocapRelay.ts`. `scripts/build-preload.mjs` bundles with
 * `external: ['electron']`, so this import is INLINED into
 * `dist/preload/preload.js` (no new runtime `require`, and
 * `tests/integration/preload-sandbox-contract.test.ts` still holds), and the
 * validation is unit-testable without executing the whole `contextBridge` surface.
 */

/** Smallest zoom factor the bridge will apply (50%). */
export const UI_ZOOM_MIN = 0.5;
/** Largest zoom factor the bridge will apply (200%). */
export const UI_ZOOM_MAX = 2;

/**
 * Validate a main-world zoom request. Returns `null` for anything that is not a
 * finite number (so the bridge ignores it), otherwise the value clamped to
 * [UI_ZOOM_MIN, UI_ZOOM_MAX].
 */
export function sanitizeZoomFactor(value: unknown): number | null {
  if (typeof value !== 'number' || !Number.isFinite(value)) return null;
  return Math.min(UI_ZOOM_MAX, Math.max(UI_ZOOM_MIN, value));
}

/**
 * The one `webFrame` method this module needs. Structural rather than
 * `Electron.WebFrame` so the unit test passes a plain fake.
 */
export interface ZoomableFrame {
  setZoomFactor(factor: number): void;
}

/**
 * Build the bridge function. The parameter is `unknown` on purpose: the declared
 * `(factor: number) => void` in `ElectronAPI` is what the renderer is told, not
 * what a hostile main world is bound by.
 */
export function createSetZoomFactor(frame: ZoomableFrame): (factor: unknown) => void {
  return (factor: unknown): void => {
    const sanitized = sanitizeZoomFactor(factor);
    if (sanitized === null) return;
    frame.setZoomFactor(sanitized);
  };
}
