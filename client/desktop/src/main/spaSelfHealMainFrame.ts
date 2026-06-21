/**
 * Pure helpers for the main-process did-fail-load listener and the
 * spa:requestSelfHeal IPC handler. Originally extracted just for filter
 * testability, then expanded (#753 reconciliation finding TA3) to also
 * own the handler bodies so the wiring is unit-testable in isolation
 * from BrowserWindow / Electron event-loop instantiation.
 *
 * `main.ts` registers the actual `app.on('web-contents-created')` and
 * `ipcMain.handle('spa:requestSelfHeal')` callbacks, which are now thin
 * adapters that unpack Electron's event-args, populate plain-data
 * structures, and delegate to the handlers below.
 */

import { isRendererSelfHealRequest } from '../shared/spaIpcTypes';
import { attemptSelfHeal, type SelfHealOutcome } from './spaSelfHeal';

export interface DidFailLoadFilterArgs {
  errorCode: number;
  validatedURL: string;
  isMainFrame: boolean;
  remoteSpaBaseUrl: string | null;
  /**
   * SPA base directory from `spaState.getRemoteSpaBaseDir()` — `'/'` for the
   * flat Pages host (post-#976) or `'/spa/<sha>/'` for the legacy host. The
   * path-prefix that marks an SPA frame/asset.
   */
  remoteSpaBaseDir: string | null;
}

export function shouldTriggerSelfHealForFailedLoad(args: DidFailLoadFilterArgs): boolean {
  // -3 ABORTED — user-initiated nav cancellation, common during fast nav.
  // Excluding this prevents denial-of-self-heal-budget via deliberate nav storms.
  if (args.errorCode === -3) return false;

  // No active SPA — we're already in bundled mode, nothing to recover to.
  if (!args.remoteSpaBaseUrl || !args.remoteSpaBaseDir) return false;

  let url: URL;
  try {
    url = new URL(args.validatedURL);
  } catch {
    return false;
  }

  // Origin equality against the dynamically-validated SPA origin is the trust
  // boundary; the SPA-base-dir prefix then distinguishes SPA frames/assets from
  // non-SPA same-origin paths (the co-hosted API, the apex, bare hash routes)
  // that must not consume the self-heal retry budget. On the flat dedicated
  // Pages host the base dir is '/' (the whole origin is the SPA); on the legacy
  // shared host it is '/spa/<sha>/'. Deriving the prefix from the runtime SPA
  // URL means a host/path migration can't silently re-break this (#976).
  if (url.origin !== args.remoteSpaBaseUrl) return false;

  if (args.isMainFrame) {
    // Main-frame failure: the SPA index itself is unreachable.
    return url.pathname.startsWith(args.remoteSpaBaseDir);
  }
  // Sub-resource failure: only the SPA's own chunk assets (`<baseDir>assets/`).
  return url.pathname.startsWith(args.remoteSpaBaseDir + 'assets/');
}

export interface SenderFrameValidationArgs {
  senderFrameUrl: string;
  remoteSpaBaseUrl: string | null;
  /** SPA base directory from `spaState.getRemoteSpaBaseDir()` — see above. */
  remoteSpaBaseDir: string | null;
}

export function validateSelfHealSenderFrame(args: SenderFrameValidationArgs): boolean {
  if (!args.senderFrameUrl || !args.remoteSpaBaseUrl || !args.remoteSpaBaseDir) return false;

  let url: URL;
  try {
    url = new URL(args.senderFrameUrl);
  } catch {
    return false;
  }

  // Trust boundary: the sender frame must be on the validated SPA origin AND
  // under the SPA's own base directory. The base dir is derived from the
  // runtime SPA URL ('/' for the flat dedicated Pages host post-#976 — any
  // same-origin frame on that dedicated host qualifies; '/spa/<sha>/' for the
  // legacy shared host — so the apex, bare hash routes, and the co-hosted API
  // stay rejected as before). Origin-only matching would accept any non-SPA
  // same-origin page; the base-dir prefix is the "SPA bundle code, not just
  // anything on the origin" tightening — now host-agnostic, so a host/path
  // migration cannot silently re-break self-heal the way the hardcoded
  // SPA_URL_PATTERN did (#976). NOTE: on the flat host remoteSpaBaseDir is '/',
  // so this prefix is vacuously true and the check collapses to origin-only —
  // safe only under the dedicated-SPA-origin invariant documented on
  // spaState.getRemoteSpaBaseDir().
  return url.origin === args.remoteSpaBaseUrl && url.pathname.startsWith(args.remoteSpaBaseDir);
}

/**
 * IPC handler body for `spa:requestSelfHeal`. Validates the sender frame,
 * validates the payload, and dispatches to `attemptSelfHeal`. Returns the
 * recovery outcome on success, or `null` on rejection (logged but not
 * re-thrown — the renderer's `?.catch(() => {})` would swallow it anyway).
 *
 * Extracted from main.ts so the trust-boundary wiring is unit-testable
 * without standing up an `ipcMain` mock or BrowserWindow.
 */
export async function handleSpaRequestSelfHeal(args: {
  senderFrameUrl: string;
  payload: unknown;
  remoteSpaBaseUrl: string | null;
  remoteSpaBaseDir: string | null;
}): Promise<SelfHealOutcome | null> {
  if (
    !validateSelfHealSenderFrame({
      senderFrameUrl: args.senderFrameUrl,
      remoteSpaBaseUrl: args.remoteSpaBaseUrl,
      remoteSpaBaseDir: args.remoteSpaBaseDir,
    })
  ) {
    console.warn('[main] spa:requestSelfHeal rejected — sender frame not from active SPA origin');
    return null;
  }

  if (!isRendererSelfHealRequest(args.payload)) {
    console.warn('[main] spa:requestSelfHeal rejected — payload not a valid renderer request');
    return null;
  }

  return attemptSelfHeal({ reason: args.payload.reason, url: args.payload.url });
}

/**
 * Listener body for `webContents.on('did-fail-load', ...)`. Filters for
 * SPA-relevant failures (excluding errorCode -3 ABORTED, requiring the
 * failed URL to be SPA-shaped, etc.) and dispatches to `attemptSelfHeal`
 * with the appropriate reason. Returns the recovery outcome on dispatch,
 * or `null` if the filter rejected the event.
 *
 * Extracted from main.ts for the same reason as `handleSpaRequestSelfHeal`.
 */
export async function handleDidFailLoad(args: {
  errorCode: number;
  validatedURL: string;
  isMainFrame: boolean;
  remoteSpaBaseUrl: string | null;
  remoteSpaBaseDir: string | null;
}): Promise<SelfHealOutcome | null> {
  const trigger = shouldTriggerSelfHealForFailedLoad(args);
  if (!trigger) return null;

  return attemptSelfHeal({
    reason: args.isMainFrame ? 'main-frame-load' : 'sub-resource',
    url: args.validatedURL,
    errorCode: args.errorCode,
  });
}
