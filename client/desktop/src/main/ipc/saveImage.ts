import { dialog, ipcMain, type BrowserWindow, type IpcMainInvokeEvent } from 'electron';
import { writeFile } from 'node:fs/promises';
import { basename } from 'node:path';
import { isPermittedFrameUrl } from './frameValidation';

/**
 * image:saveAs IPC handler — writes a decrypted image attachment to a
 * user-chosen path via a native Save-As dialog (#1729).
 *
 * An E2EE attachment is decrypted only in the renderer (a `blob:` URL), so the
 * native "Save Image As…" context item cannot reach it — the bytes are passed
 * in over IPC. This handler is the main-process layer the renderer cannot
 * bypass, mirroring `openExternal`'s defense-in-depth posture:
 *   1. sender-frame validation (`isPermittedFrameUrl`),
 *   2. payload-shape + size validation,
 *   3. filename sanitisation (basename only — no path traversal),
 *   4. the destination path is chosen by the user in `showSaveDialog`, never by
 *      the renderer.
 */

const CHANNEL = 'image:saveAs';

// Cap the accepted payload — a compromised renderer cannot drive an unbounded
// disk write. Image attachments are bounded far below this server-side.
const MAX_BYTES = 100 * 1024 * 1024; // 100 MB

type RemoteSpaOriginProvider = () => string | null;
type WindowProvider = () => BrowserWindow | null;

export interface SaveImageResult {
  ok: boolean;
  canceled?: boolean;
  reason?: 'untrusted-sender' | 'invalid-args' | 'too-large' | 'write-failed';
}

/** Reduce a renderer-supplied name to a safe bare filename (no path, no reserved chars). */
export function safeImageName(name: unknown): string {
  if (typeof name !== 'string' || name.length === 0) return 'image';
  const cleaned = basename(name)
    .replace(/[<>:"/\\|?*]/g, '_')
    .trim();
  return cleaned.length > 0 ? cleaned.slice(0, 255) : 'image';
}

export function registerSaveImageHandler(
  windowProvider: WindowProvider,
  remoteSpaOriginProvider?: RemoteSpaOriginProvider
): void {
  ipcMain.handle(
    CHANNEL,
    async (
      event: IpcMainInvokeEvent,
      bytes: unknown,
      suggestedName: unknown
    ): Promise<SaveImageResult> => {
      const senderUrl = event.senderFrame?.url ?? '';
      if (!isPermittedFrameUrl(senderUrl, remoteSpaOriginProvider?.() ?? null)) {
        return { ok: false, reason: 'untrusted-sender' };
      }

      // Electron structured-clones ArrayBuffer / TypedArrays across the IPC boundary.
      let buf: Buffer;
      if (bytes instanceof ArrayBuffer) {
        buf = Buffer.from(bytes);
      } else if (ArrayBuffer.isView(bytes)) {
        buf = Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength);
      } else {
        return { ok: false, reason: 'invalid-args' };
      }
      if (buf.byteLength === 0) return { ok: false, reason: 'invalid-args' };
      if (buf.byteLength > MAX_BYTES) return { ok: false, reason: 'too-large' };

      const win = windowProvider();
      const options = { defaultPath: safeImageName(suggestedName) };
      const result = win
        ? await dialog.showSaveDialog(win, options)
        : await dialog.showSaveDialog(options);
      if (result.canceled || !result.filePath) {
        return { ok: true, canceled: true };
      }

      try {
        await writeFile(result.filePath, buf);
        return { ok: true };
      } catch {
        // Write failure (permissions, disk full, path vanished) — structured,
        // never leak the OS error or the path into the result.
        return { ok: false, reason: 'write-failed' };
      }
    }
  );
}
