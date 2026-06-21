import { app, ipcMain, type BrowserWindow } from 'electron';
import { getRemoteSpaUrl, onSpaStateChange } from '../spaState';

// Path-anchored so a URL like `https://evil.com/proxy/spa/abc/` doesn't
// extract a hash; only top-level `/spa/<hash>` is valid per the SPA-deploy
// contract. Module-level const per typescript:S6594 (RegExp prototype call
// preferred over String.match for typed-result clarity).
const SPA_HASH_RE = /^\/spa\/([a-f0-9]+)\/?/i;

export function extractSpaHash(url: string | null): string | null {
  if (!url) return null;
  try {
    const pathname = new URL(url).pathname;
    const match = SPA_HASH_RE.exec(pathname);
    return match ? match[1] : null;
  } catch {
    return null;
  }
}

export interface VersionString {
  appVersion: string;
  spaHash: string | null;
}

export function registerVersionInfoIpc(getWindow: () => BrowserWindow | null): void {
  ipcMain.handle(
    'window:getVersionString',
    (): VersionString => ({
      appVersion: app.getVersion(),
      spaHash: extractSpaHash(getRemoteSpaUrl()),
    })
  );

  onSpaStateChange((url) => {
    const win = getWindow();
    if (!win || win.isDestroyed()) return;
    win.webContents.send('spa:versionChanged', {
      spaHash: extractSpaHash(url),
    });
  });
}
