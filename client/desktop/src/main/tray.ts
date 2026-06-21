// System tray (#1099). Owns the Tray singleton: left-click (and macOS
// double-click) reveals/focuses the main window; the context menu offers
// "Open Concord Voice" and "Quit Concord Voice". The pure decision core
// below is exported for unit tests; only the shell touches Electron
// runtime APIs.
import {
  app,
  Menu,
  nativeImage,
  Tray,
  type BrowserWindow,
  type MenuItemConstructorOptions,
} from 'electron';
import path from 'node:path';

// Structural view of BrowserWindow used by the activation decision — tests
// pass plain objects, no Electron needed.
export interface ActivatableWindow {
  isDestroyed(): boolean;
  isMinimized(): boolean;
  isVisible(): boolean;
  isFocused(): boolean;
}

export type ActivateAction = 'create' | 'restore-focus' | 'show-focus' | 'focus' | 'noop';

// Decision table (spec §2): create when no usable window, restore/show as
// needed, focus unless already focused, no-op when focused (issue AC).
export function decideActivateAction(win: ActivatableWindow | null): ActivateAction {
  if (win === null || win.isDestroyed()) return 'create';
  if (win.isMinimized()) return 'restore-focus';
  if (!win.isVisible()) return 'show-focus';
  if (!win.isFocused()) return 'focus';
  return 'noop';
}

// Applies a decision to a real window. Window-typed loosely so unit tests can
// pass partial mocks; null with a non-create action degrades to createWindow
// (the window vanished between decide and apply — treat as 'create').
export function applyActivate(
  win: Pick<BrowserWindow, 'restore' | 'show' | 'focus'> | null,
  action: ActivateAction,
  createWindow: () => void
): void {
  if (action === 'create' || win === null) {
    createWindow();
    return;
  }
  if (action === 'noop') return;
  if (action === 'restore-focus') win.restore();
  if (action === 'show-focus') win.show();
  win.focus();
}

// Labels are hardcoded — no user input ever reaches a menu label (spec §7).
export function buildContextMenuTemplate(handlers: {
  onOpen: () => void;
  onQuit: () => void;
}): MenuItemConstructorOptions[] {
  return [
    { label: 'Open Concord Voice', click: handlers.onOpen },
    { type: 'separator' },
    { label: 'Quit Concord Voice', click: handlers.onQuit },
  ];
}

// Packaged: forge extraResource copies assets/tray → <Resources>/tray.
// Dev/test: process.resourcesPath is undefined at runtime despite the
// Electron type (same truthy-guard as buildInfo.ts) → repo-relative from
// client/desktop (the cwd for electron-forge start and vitest).
function trayIconFileFor(platform: NodeJS.Platform): string {
  if (platform === 'darwin') return 'iconTemplate.png'; // Template suffix → AppKit auto light/dark
  if (platform === 'linux') return 'icon-22.png'; // StatusNotifierItem 22px convention
  return 'icon.png'; // Windows 16px (+@2x sibling auto-detected)
}

export function resolveTrayIconPath(
  platform: NodeJS.Platform,
  isPackaged: boolean,
  resourcesPath: string | undefined,
  cwd: string
): string {
  const file = trayIconFileFor(platform);
  if (isPackaged && resourcesPath) {
    return path.join(resourcesPath, 'tray', file);
  }
  return path.join(cwd, 'assets', 'tray', file);
}

// Module-scoped reference: Electron's Tray is GC-collected if unreferenced,
// which silently removes the OS icon. Held for the app's lifetime.
let tray: Tray | null = null;

export interface TrayDeps {
  getMainWindow: () => BrowserWindow | null;
  createWindow: () => void;
}

export function initTray(deps: TrayDeps): void {
  if (tray !== null) return; // idempotent — one tray per app lifetime
  let t: Tray | null = null;
  try {
    const icon = nativeImage.createFromPath(
      resolveTrayIconPath(process.platform, app.isPackaged, process.resourcesPath, process.cwd())
    );
    // createFromPath never throws — a missing/corrupt file yields an EMPTY
    // image, and new Tray(emptyImage) can "succeed" as an invisible icon the
    // user cannot see or click. That would defeat the trayless fallbacks
    // (isTrayActive() true, nothing to click), so treat empty as init failure.
    if (icon.isEmpty()) {
      throw new Error('tray icon missing or unreadable');
    }
    t = new Tray(icon);

    const onActivate = (): void => {
      const win = deps.getMainWindow();
      applyActivate(win, decideActivateAction(win), deps.createWindow);
    };

    t.setToolTip('Concord Voice');
    t.setContextMenu(
      Menu.buildFromTemplate(
        buildContextMenuTemplate({
          onOpen: onActivate,
          // Bare app.quit(): before-quit sets isQuitting, the re-entrant
          // window close passes the #1383 guard, quit completes. The tray
          // adds NO preventDefault path (spec §3).
          onQuit: () => app.quit(),
        })
      )
    );
    t.on('click', onActivate);
    if (process.platform === 'darwin') {
      t.on('double-click', onActivate); // macOS convention (issue AC)
    }
    tray = t;
  } catch (err) {
    // Destroy a half-constructed tray (new Tray() succeeded but wiring threw)
    // so no handler-less orphan icon lingers until GC collects it.
    try {
      t?.destroy();
    } catch {
      // best-effort — the OS resource is already in an undefined state
    }
    // Tray is optional chrome: a sandboxed env or missing StatusNotifier host
    // must not take the app down. The close/minimize intercepts and
    // window-all-closed all check isTrayActive(), so a trayless session keeps
    // the pre-#1099 recoverable behaviors.
    // Message-only per [internal]rules/observability.md (no Error.cause leak).
    console.error('Tray init failed:', err instanceof Error ? err.message : 'tray_init_failed');
    tray = null;
  }
}

export function destroyTray(): void {
  if (tray !== null && !tray.isDestroyed()) {
    tray.destroy();
  }
  tray = null;
}

export function isTrayActive(): boolean {
  return tray !== null && !tray.isDestroyed();
}
