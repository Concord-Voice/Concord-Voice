import { triggerChunkSelfHeal } from '../spaSelfHealClient';

/**
 * Lazily import a recovery/reset module and run an action on it, swallowing a
 * failure so it never surfaces as an `Uncaught (in promise)` and never strands
 * the connection-recovery state machine on a non-recovering screen.
 *
 * The dominant failure mode is a STALE SPA CHUNK: `recoveryService` /
 * `resetService` are lazy Vite chunks fetched on-demand DURING reconnect, so a
 * Cloudflare Pages redeploy that rotated their content hashes makes `import()`
 * reject (the missing `/assets/<hash>.js` 404s to the SPA `index.html`, served
 * as `text/html`). On failure we trigger SPA self-heal — a bounded,
 * main-process-driven reload that fetches fresh chunks — preserving the
 * recovery action a bare `import().then()` would have silently skipped. (Self-
 * heal is only effective once the host-shape patterns recognise the live SPA
 * origin; see the item-1/item-4 coupling and `[internal]rules/electron.md`.)
 *
 * The returned promise NEVER rejects, so callers may float it without producing
 * an unhandled rejection.
 *
 * @param importer dynamic `() => import('...')` thunk for the module
 * @param run      action to run with the resolved module
 * @param context  short label for the diagnostic log line (NOT the self-heal reason)
 */
export async function runRecoveryModule<T>(
  importer: () => Promise<T>,
  run: (mod: T) => void | Promise<void>,
  context: string
): Promise<void> {
  try {
    const mod = await importer();
    await run(mod);
  } catch (err) {
    // Constant format string (CWE-134): the dynamic parts are passed as
    // separate args, never interpolated into the format literal.
    console.debug(
      '[Recovery] module unavailable (likely stale SPA chunk):',
      context,
      err instanceof Error ? err.message : String(err)
    );
    triggerChunkSelfHeal('chunk-import-rejected');
  }
}
