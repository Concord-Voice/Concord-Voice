/**
 * SPA Self-Heal — renderer-side detection (#753, ADR-0001).
 *
 * Installs two global listeners early in main.tsx, before any lazy import()
 * can fire. Both listeners filter chunk-shaped failures and IPC the main
 * process to trigger attemptSelfHeal.
 *
 * The IPC payload's `url` field is diagnostic-only — the main-process
 * recovery primitive ignores it and refetches /api/v1/client/config from
 * scratch. No client-supplied URL ever flows into webContents.loadURL().
 */

import { SPA_CHUNK_URL_PATTERN } from '../shared/spaUrlPattern';

const CHUNK_LOAD_ERROR_PATTERN =
  /Failed to fetch dynamically imported module|Loading chunk \S+ failed/;

let installed = false;

/**
 * Fire an SPA self-heal request for a chunk-load failure. The main process
 * validates the request and drives a bounded recovery (config refetch / reload /
 * bundled app://concord fallback). Missing-bridge (no `electron` in a plain
 * browser context) and the async-rejection path are both swallowed here, so
 * callers never need to guard either.
 *
 * Exported so recovery-path catch handlers that intentionally swallow a
 * dynamic-import rejection — and therefore stop it from reaching the
 * `unhandledrejection` listener below — can still trigger self-heal. See
 * `runRecoveryModule` and `[internal]rules/electron.md` (SPA self-heal).
 */
export function triggerChunkSelfHeal(
  reason: 'chunk-load' | 'chunk-import-rejected',
  url?: string
): void {
  const payload = url ? { reason, url } : { reason };
  globalThis.electron?.spa?.requestSelfHeal(payload)?.catch(() => {
    /* swallow — main-process logs its own diagnostics */
  });
}

export function installSelfHealHandlers(): void {
  if (installed) return;
  installed = true;

  globalThis.addEventListener(
    'error',
    (e: Event) => {
      const target = e.target as HTMLScriptElement | HTMLLinkElement | null;
      if (!target) return;

      const url = (target as HTMLScriptElement).src ?? (target as HTMLLinkElement).href ?? '';

      if (!url || !SPA_CHUNK_URL_PATTERN.test(url)) return;

      // Cross-origin guard: SPA_CHUNK_URL_PATTERN is host-agnostic, so a
      // failing third-party script with a path that happens to match
      // `/spa/<sha>/assets/...` would otherwise trigger self-heal. Require
      // the failing URL's origin to match the page's own origin.
      try {
        if (new URL(url).origin !== globalThis.location.origin) return;
      } catch {
        return;
      }

      // Bridge-presence + async-rejection guards live in triggerChunkSelfHeal,
      // which also survives bundled-fallback mode where the renderer may load
      // before main is ready.
      triggerChunkSelfHeal('chunk-load', url);
    },
    /* useCapture */ true
  );

  globalThis.addEventListener('unhandledrejection', (e: PromiseRejectionEvent) => {
    const reason = e.reason;
    const message = reason instanceof Error ? reason.message : String(reason);

    if (!CHUNK_LOAD_ERROR_PATTERN.test(message)) return;

    triggerChunkSelfHeal('chunk-import-rejected');
  });
}
