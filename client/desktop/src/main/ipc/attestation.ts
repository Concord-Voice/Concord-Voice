/**
 * Attestation IPC bridge — exposes the main-process attestation token cache to
 * the renderer (#677).
 *
 * Channel surface (2):
 *   - `attestation:get-token`   (invoke) — return the cached attestation token
 *                                          (string), or null when none is cached
 *                                          or the cached token has expired. The
 *                                          renderer attaches this to outbound
 *                                          requests as `X-Attestation-Token`.
 *   - `attestation:clear-token` (invoke) — drop the cached token so the next
 *                                          request path re-attests. Called by the
 *                                          renderer after a 403 attestation
 *                                          failure triggers re-verification.
 *
 * The token itself is minted and cached by `attestationService.ts`; this module
 * is a thin, read-only-plus-clear bridge. It never mints — that happens in the
 * main process post-login flow, not in response to renderer requests.
 *
 * Sender-frame validation (the only layer the renderer cannot bypass) is
 * enforced via `isPermittedFrameUrl` — the same helper `sso.ts` and
 * `openExternal.ts` use, keeping the trust boundary consistent across IPC
 * handlers. An untrusted frame causes both invoke handlers to throw, so the
 * renderer sees a rejected promise rather than a token or a silent clear.
 */
import { ipcMain, type IpcMainInvokeEvent } from 'electron';

import { getAttestationToken, clearAttestationToken } from '../attestationService';

import { isPermittedFrameUrl } from './frameValidation';

type RemoteSpaOriginProvider = () => string | null;

/** Validates the IPC sender frame matches a permitted SPA origin. */
function checkFrame(event: IpcMainInvokeEvent, getSpaBaseUrl: RemoteSpaOriginProvider): boolean {
  const url = event.senderFrame?.url ?? '';
  return isPermittedFrameUrl(url, getSpaBaseUrl());
}

/**
 * registerAttestationIpc wires the two attestation:* IPC channels used by the
 * renderer to read and clear the main-process attestation token cache.
 *
 * `getSpaBaseUrl` returns the currently-active validated remote SPA origin (or
 * null when the bundled build is loaded). The accessor is injected so main.ts
 * can wire the live value once the SPA loader has decided which build to serve —
 * mirroring `registerSSOIPC` and `registerOpenExternalHandler`.
 */
export function registerAttestationIpc(getSpaBaseUrl: RemoteSpaOriginProvider): void {
  ipcMain.handle('attestation:get-token', (event): string | null => {
    if (!checkFrame(event, getSpaBaseUrl)) {
      throw new Error('attestation:get-token rejected: untrusted sender frame');
    }
    return getAttestationToken();
  });

  ipcMain.handle('attestation:clear-token', (event): void => {
    if (!checkFrame(event, getSpaBaseUrl)) {
      throw new Error('attestation:clear-token rejected: untrusted sender frame');
    }
    clearAttestationToken();
  });
}
