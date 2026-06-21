import { app, ipcMain, type BrowserWindow } from 'electron';
import { DEFAULT_CLIENT_BEHAVIOR, type ClientBehavior } from '../../shared/clientBehavior';

let cachedClientBehavior: ClientBehavior = DEFAULT_CLIENT_BEHAVIOR;

const VALID_TO_TRAY = new Set(['close', 'minimize', 'none']);
const VALID_TO_TOOLBAR = new Set(['minimize', 'close']);
// CSS color allowlist: #hex (3/4/6/8 chars) or rgb()/rgba(). The renderer
// only ever sends hex values resolved from theme tokens, so a strict
// allowlist costs nothing at the IPC boundary. CWE-20 defense.
const CSS_COLOR_RE = /^(#[0-9a-fA-F]{3,8}|rgba?\([^)]+\))$/;

function isValidClientBehavior(value: unknown): value is ClientBehavior {
  if (value === null || typeof value !== 'object') return false;
  const v = value as Record<string, unknown>;
  return (
    typeof v.toTray === 'string' &&
    VALID_TO_TRAY.has(v.toTray) &&
    typeof v.toToolbar === 'string' &&
    VALID_TO_TOOLBAR.has(v.toToolbar)
  );
}

export function getCachedClientBehavior(): ClientBehavior {
  return cachedClientBehavior;
}

export function registerWindowControlsIpc(getWindow: () => BrowserWindow | null): void {
  ipcMain.handle('window:setClientBehavior', (_event, value: unknown) => {
    // Runtime input validation at the IPC trust boundary. A compromised
    // renderer could send arbitrary shapes; reject silently and keep the
    // existing cache so [X]/[-] intercept fall-through remains sane.
    if (!isValidClientBehavior(value)) {
      console.warn('[windowControls] setClientBehavior rejected: invalid shape from renderer');
      return;
    }
    cachedClientBehavior = value;
  });

  ipcMain.handle('window:quit', () => {
    app.quit();
  });

  ipcMain.handle('window:setTitleBarOverlayColor', (_event, options: unknown) => {
    // CSS color allowlist guard at the IPC trust boundary. Electron's
    // setTitleBarOverlay accepts a CSS color string; non-color input
    // throws or paints garbage. CWE-20 defense.
    if (
      options === null ||
      typeof options !== 'object' ||
      typeof (options as { color?: unknown }).color !== 'string' ||
      typeof (options as { symbolColor?: unknown }).symbolColor !== 'string' ||
      !CSS_COLOR_RE.test((options as { color: string }).color) ||
      !CSS_COLOR_RE.test((options as { symbolColor: string }).symbolColor)
    ) {
      console.warn('[windowControls] setTitleBarOverlayColor rejected: invalid CSS color');
      return;
    }
    const win = getWindow();
    if (!win || win.isDestroyed()) return;
    const opts = options as { color: string; symbolColor: string };
    win.setTitleBarOverlay({
      color: opts.color,
      symbolColor: opts.symbolColor,
      height: 32,
    });
  });
}
