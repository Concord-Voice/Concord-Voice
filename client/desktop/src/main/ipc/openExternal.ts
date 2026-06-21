import { ipcMain, shell, type IpcMainInvokeEvent } from 'electron';
import { isPermittedFrameUrl } from './frameValidation';

/**
 * open-external IPC handler — routes renderer link clicks to `shell.openExternal`
 * after enforcing BOTH sender-frame validation AND protocol allowlisting.
 *
 * This is defense-in-depth layer 3 for the Markdown pipeline:
 *   1. rehype-sanitize rejects unsafe hrefs at the hast level.
 *   2. SafeLink.tsx re-validates the href before emitting the anchor.
 *   3. This handler re-validates in the main process, the only layer the
 *      renderer cannot bypass.
 *
 * The protocol allowlist (http:, https:, mailto:) MUST stay in sync with
 * SafeLink's `SAFE_PROTOCOLS` regex. Widening it here without widening the
 * renderer check is a security regression — and vice-versa.
 */

const CHANNEL = 'open-external';

const ALLOWED_PROTOCOLS = new Set(['http:', 'https:', 'mailto:']);

function isAllowedUrl(url: string): boolean {
  try {
    const parsed = new URL(url);
    return ALLOWED_PROTOCOLS.has(parsed.protocol);
  } catch {
    return false;
  }
}

type RemoteSpaOriginProvider = () => string | null;

export interface OpenExternalResult {
  ok: boolean;
  reason?: 'untrusted-sender' | 'invalid-protocol' | 'invalid-url' | 'open-failed';
}

/**
 * Register the open-external IPC handler with sender-frame + protocol validation.
 *
 * `remoteSpaOriginProvider` returns the currently-active validated remote SPA
 * origin (or null when the bundled build is loaded). Injected so main.ts can
 * wire the live value after the SPA loader decides which build to serve. Left
 * undefined in unit tests that only exercise the bundled/localhost frame paths.
 */
export function registerOpenExternalHandler(
  remoteSpaOriginProvider?: RemoteSpaOriginProvider
): void {
  ipcMain.handle(
    CHANNEL,
    async (event: IpcMainInvokeEvent, url: unknown): Promise<OpenExternalResult> => {
      const senderUrl = event.senderFrame?.url ?? '';
      const remoteSpaOrigin = remoteSpaOriginProvider?.() ?? null;
      if (!isPermittedFrameUrl(senderUrl, remoteSpaOrigin)) {
        return { ok: false, reason: 'untrusted-sender' };
      }
      if (typeof url !== 'string') {
        return { ok: false, reason: 'invalid-url' };
      }
      if (!isAllowedUrl(url)) {
        return { ok: false, reason: 'invalid-protocol' };
      }
      // shell.openExternal can reject (OS denies, no protocol handler, sandbox
      // restriction). Convert rejections into the structured {ok, reason}
      // contract so renderers never see the IPC invoke itself reject and can
      // branch on result.ok alone.
      try {
        await shell.openExternal(url);
        return { ok: true };
      } catch {
        return { ok: false, reason: 'open-failed' };
      }
    }
  );
}
