/**
 * SPA Self-Heal — recovery primitive for the SPA deploy contract (ADR-0001).
 *
 * Called by:
 *   - The renderer-side spaSelfHealClient via the spa:requestSelfHeal IPC
 *     (from src/main/main.ts ipcMain.handle).
 *   - The main-process did-fail-load listener (also in src/main/main.ts).
 *
 * Both call sites converge on `attemptSelfHeal`, which gates concurrent
 * triggers via an `inFlight` promise reference and dispatches to
 * `performRecovery`. `performRecovery` owns the R2 retry state machine and
 * delegates the two side-effect paths to two helpers:
 *
 *   - `attemptRemoteLoad(wc, url)`  — wraps `wc.loadURL(...)`, returns success
 *   - `loadBundledFallback(wc)`     — wraps `wc.loadFile(bundled)`, swallows
 *
 * State is window-scoped and in-memory: it resets on app restart.
 *
 * R2 state machine:
 *   retryCount=0 → wait 500ms, resolveSpaSource(), attemptRemoteLoad on
 *                  remote success / loadBundledFallback on validator
 *                  rejection or loadURL throw. retryCount → 1.
 *   retryCount=1 → loadBundledFallback immediately. selfHealExhausted=true.
 *   exhausted    → log diagnostic (static message per CWE-134), noop.
 *   no-window    → log diagnostic, noop WITHOUT mutating state (transient
 *                  absence: window-being-destroyed mid-attempt or early-
 *                  startup race; the next valid trigger should still recover).
 *
 * In-flight idempotency: simultaneous triggers from multiple call sites
 * collapse to a single recovery attempt via the `inFlight` promise reference.
 *
 * Recursive-trigger note: if `attemptRemoteLoad` is called and `wc.loadURL`
 * itself fails, Electron emits another `did-fail-load` for the same URL with
 * `isMainFrame=true`. By the time that listener fires, the original
 * `inFlight` promise has settled (`.finally` cleared it), so the listener
 * starts a fresh attempt. This is the documented R2 step-up: first trigger
 * recovers (retryCount=0→1), second trigger (the listener-triggered cascade)
 * lands at the bundled fallback (retryCount=1 path). The state machine
 * correctly bounds this at one extra retry slot.
 */

import { app, BrowserWindow } from 'electron';
import { resolveSpaSource } from './spaLoader';
import { setRemoteSpaState } from './spaState';
import type { SelfHealReason } from '../shared/spaIpcTypes';

// SelfHealReason's canonical definition lives in `../shared/spaIpcTypes`
// because the renderer/main trust-boundary type guard
// (`isRendererSelfHealRequest`) must reference the same closed reason union
// (#753 reconciliation finding C1). External callers import the type from
// the shared module directly; this file imports it only for use in
// `SelfHealRequest` below.

export interface SelfHealRequest {
  reason: SelfHealReason;
  url?: string;
  errorCode?: number;
}

export interface SelfHealOutcome {
  mode: 'recovered' | 'fellBackToBundled' | 'noop';
  retryCount: number;
}

const RETRY_DELAY_MS = 500;

let retryCount = 0;
let selfHealExhausted = false;
let inFlight: Promise<SelfHealOutcome> | null = null;

/**
 * Test-only escape hatch — resets module state between Vitest cases.
 * Not exposed via index/preload; importable only from tests.
 */
export function __resetSelfHealState(): void {
  retryCount = 0;
  selfHealExhausted = false;
  inFlight = null;
}

function getPrimaryWebContents(): Electron.WebContents | null {
  const windows = BrowserWindow.getAllWindows();
  // The first non-destroyed window. PiP windows do not host the SPA, but the
  // primary window's webContents is always windows[0] in practice; if multiple
  // primary windows ever exist, we recover all of them by hitting the first.
  for (const win of windows) {
    if (!win.isDestroyed()) return win.webContents;
  }
  return null;
}

/**
 * Load the bundled fallback renderer. Errors are logged but never propagated
 * — the recovery primitive treats a failed bundled-load as exhausted, not as
 * an actionable failure (there's no further fallback layer).
 *
 * Log message is intentionally a static literal per CWE-134 (no interpolation
 * in console format strings). Caller-specific context is encoded in the
 * sequence of preceding log lines, not inside this helper's message.
 */
async function loadBundledFallback(wc: Electron.WebContents): Promise<void> {
  // Clear SPA state so PiP and origin-only consumers see a consistent
  // "not in remote-SPA mode" signal once the renderer has fallen back to
  // bundled. Without this, a stale `remoteSpaUrl` from an earlier
  // successful remote load would cause PiP to keep loading the (broken)
  // remote SPA after the main window already swapped to bundled.
  setRemoteSpaState(null);
  // #830 review: in dev mode the app:// scheme is unregistered (the
  // registration in main.ts is gated on app.isPackaged). Self-heal SHOULDN'T
  // be reachable in dev — shouldTriggerSelfHealForFailedLoad's no-remote
  // filter blocks the trigger when remoteSpaBaseUrl is null, which is
  // always-true in dev — but defense-in-depth: log and noop instead of
  // attempting an unhandled-scheme loadURL that would throw with no
  // further fallback.
  if (!app.isPackaged) {
    console.warn('[spaSelfHeal] bundled fallback skipped in dev mode (app:// scheme unregistered)');
    return;
  }
  try {
    // #830: self-heal terminal state loads bundled via app:// scheme.
    await wc.loadURL('app://concord/index.html');
  } catch (err) {
    console.error('[spaSelfHeal] bundled loadURL failed:', (err as Error).message);
  }
}

/**
 * Attempt to load the remote SPA URL. Returns true on success, false on
 * loadURL throw (caller is responsible for the bundled fallback in that
 * case — keeps the failure-handling state mutation in performRecovery).
 */
async function attemptRemoteLoad(wc: Electron.WebContents, url: string): Promise<boolean> {
  try {
    await wc.loadURL(url);
    // Refresh SPA state — recovery may have landed on a different SHA than
    // the original load (e.g., if a deploy ran between failure and retry).
    // PiP needs the current full URL to reach the same nginx route the
    // main window is now serving.
    setRemoteSpaState(url);
    return true;
  } catch (err) {
    console.error('[spaSelfHeal] loadURL failed:', (err as Error).message);
    return false;
  }
}

async function performRecovery(_reason: SelfHealReason): Promise<SelfHealOutcome> {
  const wc = getPrimaryWebContents();
  if (!wc) {
    // No window to recover — could be a transient race (window destroyed
    // mid-attempt) or early-startup before any window exists. Log and noop
    // WITHOUT mutating retry state, so the next valid trigger can still
    // recover. (Earlier versions burned the retry budget here, conflating
    // transient absence with terminal exhaustion — fixed per #753 silent-
    // failure-hunter finding SF1.)
    console.warn('[spaSelfHeal] noop — no active window (transient; retry budget preserved)');
    return { mode: 'noop', retryCount };
  }

  if (selfHealExhausted) {
    // Static log message per CWE-134 (no interpolation of `reason` even though
    // it's a closed enum at the type level — runtime values aren't guaranteed
    // literal once they cross the IPC boundary). This exhausted-path warning
    // intentionally does not include the originating reason: first-trigger
    // failure CAUSES (resolveSpaSource throw, loadURL throw, bundled-load
    // throw) are already logged with their underlying error message at each
    // failure site, but the first-trigger REASON itself is not separately
    // logged at any site. Adding a literal-only first-trigger reason log
    // would require static enum branching to satisfy CWE-134 — not worth it.
    console.warn('[spaSelfHeal] noop — already exhausted (subsequent triggers ignored)');
    return { mode: 'noop', retryCount };
  }

  if (retryCount >= 1) {
    // Second trigger this session — fall back to bundled immediately.
    await loadBundledFallback(wc);
    selfHealExhausted = true;
    retryCount += 1;
    return { mode: 'fellBackToBundled', retryCount };
  }

  // First trigger — wait, refetch config, validate, loadURL.
  await new Promise((resolve) => setTimeout(resolve, RETRY_DELAY_MS));

  let resolved;
  try {
    resolved = await resolveSpaSource();
  } catch (err) {
    console.error('[spaSelfHeal] resolveSpaSource threw:', (err as Error).message);
    await loadBundledFallback(wc);
    selfHealExhausted = true;
    retryCount += 1;
    return { mode: 'fellBackToBundled', retryCount };
  }

  if (resolved.mode === 'remote' && resolved.url) {
    const ok = await attemptRemoteLoad(wc, resolved.url);
    if (!ok) {
      // loadURL failure → bundled fallback, still consume the retry slot.
      await loadBundledFallback(wc);
      retryCount += 1;
      selfHealExhausted = true;
      return { mode: 'fellBackToBundled', retryCount };
    }
    retryCount += 1;
    return { mode: 'recovered', retryCount };
  }

  // resolveSpaSource returned mode=bundled (validator rejection / fetch fail).
  await loadBundledFallback(wc);
  retryCount += 1;
  selfHealExhausted = true;
  return { mode: 'fellBackToBundled', retryCount };
}

export function attemptSelfHeal(req: SelfHealRequest): Promise<SelfHealOutcome> {
  if (inFlight) return inFlight;

  inFlight = performRecovery(req.reason).finally(() => {
    inFlight = null;
  });
  return inFlight;
}
