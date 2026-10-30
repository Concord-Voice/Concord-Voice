/**
 * The `tray:setStatus` invoke (tray-status spec §4, IPC contract 33).
 *
 * Fences, in this order; nothing here throws. The result is `false` only when the
 * rate limit dropped the snapshot (the renderer resends it); every other outcome,
 * refusals included, is `true`:
 *   1. `requireTrustedSender` — a permitted SPA frame origin.
 *   2. The sender is the MAIN window's `webContents` and its main frame (R5). A PiP
 *      window shares the origin and would pass (1); only the main window may set
 *      tray status.
 *   3. A 20/s token bucket.
 *   4. `firstInvalidField` — the exact-key, closed-enum guard. A malformed payload is
 *      logged once per session with the field NAME only (a fixed-table token, never a
 *      value or a caller-chosen key).
 * Only then is the snapshot handed to `applyTraySnapshot`, which stores it while the
 * tray is inactive and renders it otherwise.
 *
 * `attachTrayResetHooks` covers R17: when the renderer reloads, crashes or hangs, the
 * tray drops back to the neutral icon and the Open/Quit menu rather than keeping live
 * voice controls for a session that may no longer exist. A hang is the one reset that
 * can end with the same session alive, so `responsive` re-applies the last accepted
 * snapshot: the renderer only sends on change and would otherwise leave the tray
 * neutral until some field moved.
 */
import { ipcMain, type BrowserWindow, type IpcMainInvokeEvent } from 'electron';

import {
  firstInvalidField,
  TRAY_SET_STATUS,
  type TrayStatusSnapshot,
} from '../../shared/trayStatus';
import { applyTraySnapshot } from '../tray';

import { requireTrustedSender } from './frameValidation';

const TRAY_STATUS_RATE_PER_SEC = 20;

// The last snapshot the handler accepted from the live main renderer; null once that
// renderer navigates, crashes or is destroyed.
let lastAccepted: TrayStatusSnapshot | null = null;

/**
 * Continuous-refill token bucket: capacity `ratePerSec`, starts full, refills at
 * `ratePerSec` tokens per second. A clock that goes backwards mints nothing.
 */
export function createTokenBucket(ratePerSec: number, now: () => number): () => boolean {
  let tokens = ratePerSec;
  let last = now();
  return () => {
    const t = now();
    const elapsed = Math.max(0, t - last);
    last = Math.max(last, t);
    tokens = Math.min(ratePerSec, tokens + (elapsed * ratePerSec) / 1000);
    if (tokens < 1) return false;
    tokens -= 1;
    return true;
  };
}

/**
 * True only for the main window's own main frame, on a trusted origin. `senderFrame`
 * is read once (inside `requireTrustedSender`), so the frame whose URL passed is the
 * frame whose id is compared. A destroyed window is refused explicitly; the catch is
 * the backstop for Electron's "Object has been destroyed" throw on property access.
 * Same construction as `isMainWindowMainFrame` in `audiocap.ts`.
 */
function isMainWindowMainFrame(
  event: IpcMainInvokeEvent,
  remoteSpaOrigin: string | null,
  getMainWindow: () => BrowserWindow | null
): boolean {
  const senderFrame = requireTrustedSender(event, remoteSpaOrigin);
  if (!senderFrame) return false;
  try {
    const win = getMainWindow();
    if (!win || win.isDestroyed()) return false;
    const contents = win.webContents;
    return (
      event.sender === contents &&
      senderFrame.frameTreeNodeId === contents.mainFrame.frameTreeNodeId
    );
  } catch {
    return false;
  }
}

export function registerTrayStatusIpc(
  getRemoteSpaBaseUrl: () => string | null,
  getMainWindow: () => BrowserWindow | null
): void {
  const allow = createTokenBucket(TRAY_STATUS_RATE_PER_SEC, () => Date.now());
  let loggedMalformed = false;
  ipcMain.handle(TRAY_SET_STATUS, (event: IpcMainInvokeEvent, payload: unknown): boolean => {
    try {
      if (!isMainWindowMainFrame(event, getRemoteSpaBaseUrl(), getMainWindow)) return true;
      if (!allow()) return false;
      const invalid = firstInvalidField(payload);
      if (invalid !== null) {
        if (!loggedMalformed) {
          loggedMalformed = true;
          console.warn(`tray:setStatus dropped a malformed snapshot (field ${invalid})`);
        }
        return true;
      }
      lastAccepted = { ...(payload as TrayStatusSnapshot) };
      applyTraySnapshot({ ...lastAccepted });
    } catch {
      // Contract: a tray update can never fail the renderer's invoke.
    }
    return true;
  });
}

/** The minimal `webContents` surface the reset hooks need. */
interface ResettableContents {
  on(event: string, listener: (...args: never[]) => void): unknown;
}

/** Reset the tray to neutral when the main renderer reloads, crashes, hangs or goes away (R17). */
export function attachTrayResetHooks(contents: ResettableContents): void {
  const reset = (): void => {
    lastAccepted = null;
    applyTraySnapshot(null);
  };
  // `did-navigate`, not `did-start-navigation`: the latter fires before main.ts's
  // `will-navigate` gate, so a navigation that gate cancels would clear the tray of a
  // session that is still alive. `did-navigate` fires only when a main-frame,
  // cross-document navigation commits (same pattern as audiocapHost.ts).
  contents.on('did-navigate', reset);
  // A failed main-frame load commits an error page without `did-navigate`; ERR_ABORTED
  // (-3) is a cancelled navigation that leaves the document in place.
  contents.on(
    'did-fail-load',
    (
      _event: unknown,
      errorCode: number,
      _description: string,
      _url: string,
      isMainFrame: boolean
    ) => {
      if (isMainFrame && errorCode !== -3) reset();
    }
  );
  contents.on('render-process-gone', reset);
  contents.on('unresponsive', () => applyTraySnapshot(null));
  contents.on('responsive', () => {
    if (lastAccepted) applyTraySnapshot({ ...lastAccepted });
  });
  contents.on('destroyed', reset);
}
