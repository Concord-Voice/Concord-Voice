// System tray (#1099, status-aware since spec 2026-10-29-tray-status-design).
//
// Owns the Tray singleton. The renderer pushes a closed-enum snapshot through
// `tray:setStatus` (ipc/trayStatus.ts → applyTraySnapshot); main adds its own
// state (update phase, battery, window focus, OS motion settings), resolves it
// through the pure `resolveTrayVisual`, and applies the frame (via the bounded
// scheduler), the tooltip and the menu. Menu clicks that change client state go
// back to the renderer as a closed-enum `TrayAction`; Open, Quit and the two
// update actions are handled here without IPC (spec §4).
//
// The pure decision core below is exported for unit tests; only the shell
// touches Electron runtime APIs.
import {
  app,
  Menu,
  nativeImage,
  powerMonitor,
  systemPreferences,
  Tray,
  type BrowserWindow,
  type MenuItemConstructorOptions,
  type NativeImage,
} from 'electron';
import fs from 'node:fs';
import path from 'node:path';

import type { TrayAction, TrayStatusSnapshot } from '../shared/trayStatus';
import { createTrayScheduler, type MotionGate } from './trayScheduler';
import {
  resolveTrayVisual,
  type MainTrayState,
  type RenderMode,
  type TrayMenuId,
  type TrayMenuItem,
} from './trayVisual';

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

// ── Menu model → effects ────────────────────────────────────────────────────

export type TrayMenuEffect =
  | { type: 'activate' }
  | { type: 'activate-send'; action: TrayAction }
  | { type: 'send'; action: TrayAction }
  | { type: 'quit' }
  | { type: 'download-update' }
  | { type: 'restart-update' };

/**
 * What clicking a menu row does. `checked` is the state the row SHOWED when the
 * menu was built; mute/deafen send its inverse as a target value, so a stale
 * menu clicked after a hotkey change cannot flip the control the wrong way
 * (spec R3 — the renderer compares the target against its store).
 */
export function menuEffectFor(id: TrayMenuId, checked: boolean | undefined): TrayMenuEffect | null {
  switch (id) {
    case 'open':
      return { type: 'activate' };
    case 'unread':
      return { type: 'activate-send', action: { kind: 'open-unread' } };
    case 'quit':
      return { type: 'quit' };
    case 'download-update':
      return { type: 'download-update' };
    case 'restart-update':
      return { type: 'restart-update' };
    case 'mute':
      return { type: 'send', action: { kind: 'set-muted', value: checked !== true } };
    case 'deafen':
      return { type: 'send', action: { kind: 'set-deafened', value: checked !== true } };
    case 'leave':
      return { type: 'send', action: { kind: 'leave-voice' } };
    case 'stop-share':
      return { type: 'send', action: { kind: 'stop-share' } };
    case 'presence-online':
      return { type: 'send', action: { kind: 'set-presence', value: 'online' } };
    case 'presence-dnd':
      return { type: 'send', action: { kind: 'set-presence', value: 'dnd' } };
    case 'presence-invisible':
      return { type: 'send', action: { kind: 'set-presence', value: 'invisible' } };
    case 'status-line':
    case 'downloading':
    case 'presence':
      return null;
  }
}

/**
 * Electron template for a resolved menu model. Labels come only from the model,
 * whose text is the hardcoded TRAY_LABELS table (C4). Disabled rows get no
 * click handler at all.
 */
export function buildTrayMenuTemplate(
  model: readonly TrayMenuItem[],
  onClick: (id: TrayMenuId, checked: boolean | undefined) => void
): MenuItemConstructorOptions[] {
  return model.map((entry): MenuItemConstructorOptions => {
    if (entry.type === 'separator') return { type: 'separator' };
    const out: MenuItemConstructorOptions = {
      id: entry.id,
      label: entry.label,
      enabled: entry.enabled,
      type: entry.submenu ? 'submenu' : (entry.kind ?? 'normal'),
    };
    if (entry.kind !== undefined) out.checked = entry.checked === true;
    if (entry.submenu) out.submenu = buildTrayMenuTemplate(entry.submenu, onClick);
    else if (entry.enabled) {
      const { id, checked } = entry;
      out.click = () => onClick(id, checked);
    }
    return out;
  });
}

// ── Sprite assets ───────────────────────────────────────────────────────────

const RENDER_MODES: readonly RenderMode[] = Object.freeze(['template', 'white', 'black', 'colour']);
const FRAME_KEY_RE = /^[a-z0-9]+\.[a-z0-9]+\.[a-z0-9]+$/;
const FALLBACK_KEY = 'full.full.star';

export interface TrayManifest {
  keys: string[];
  modes: RenderMode[];
}

/**
 * Packaged: forge extraResource copies assets/tray → <Resources>/tray.
 * Dev/test: process.resourcesPath is undefined at runtime despite the Electron
 * type (same truthy-guard as buildInfo.ts) → repo-relative from client/desktop
 * (the cwd for electron-forge start and vitest).
 */
export function resolveTrayAssetDir(
  isPackaged: boolean,
  resourcesPath: string | undefined,
  cwd: string
): string {
  if (isPackaged && resourcesPath) return path.join(resourcesPath, 'tray');
  return path.join(cwd, 'assets', 'tray');
}

/**
 * Sprite file for one frame. darwin/win32 load the 16px base and nativeImage
 * picks up the `@2x` sibling on HiDPI; Linux loads the 22px StatusNotifierItem
 * size.
 */
export function resolveTrayIconPath(
  platform: NodeJS.Platform,
  isPackaged: boolean,
  resourcesPath: string | undefined,
  cwd: string,
  mode: RenderMode,
  key: string
): string {
  const file = platform === 'linux' ? `${key}-22.png` : `${key}.png`;
  return path.join(resolveTrayAssetDir(isPackaged, resourcesPath, cwd), mode, file);
}

/** Keeps only well-formed keys and known modes, so no path segment is free text. */
export function sanitizeTrayManifest(raw: unknown): TrayManifest {
  const record = typeof raw === 'object' && raw !== null ? (raw as Record<string, unknown>) : {};
  const keys = Array.isArray(record.keys)
    ? record.keys.filter((k): k is string => typeof k === 'string' && FRAME_KEY_RE.test(k))
    : [];
  const modes = Array.isArray(record.modes)
    ? RENDER_MODES.filter((m) => (record.modes as unknown[]).includes(m))
    : [];
  return { keys, modes };
}

export function loadTrayManifest(dir: string): TrayManifest {
  try {
    return sanitizeTrayManifest(
      JSON.parse(fs.readFileSync(path.join(dir, 'manifest.json'), 'utf8'))
    );
  } catch {
    return { keys: [], modes: [] };
  }
}

function fallbackModeFor(platform: NodeJS.Platform): RenderMode {
  return platform === 'darwin' ? 'template' : 'colour';
}

/** Decodes every frame for the current platform once (spec §4). Empty images are skipped. */
function decodeFrames(platform: NodeJS.Platform): Map<string, NativeImage> {
  const manifest = loadTrayManifest(
    resolveTrayAssetDir(app.isPackaged, process.resourcesPath, process.cwd())
  );
  const fallbackMode = fallbackModeFor(platform);
  const wanted = new Set<string>([`${fallbackMode}/${FALLBACK_KEY}`]);
  for (const mode of manifest.modes) {
    for (const key of manifest.keys) wanted.add(`${mode}/${key}`);
  }
  const frames = new Map<string, NativeImage>();
  for (const sig of wanted) {
    const [mode, key] = sig.split('/') as [RenderMode, string];
    const image = nativeImage.createFromPath(
      resolveTrayIconPath(platform, app.isPackaged, process.resourcesPath, process.cwd(), mode, key)
    );
    // createFromPath never throws — a missing/corrupt file yields an EMPTY image.
    if (image.isEmpty()) continue;
    image.setTemplateImage(mode === 'template');
    frames.set(sig, image);
  }
  return frames;
}

// ── Shell state ─────────────────────────────────────────────────────────────

// Module-scoped reference: Electron's Tray is GC-collected if unreferenced,
// which silently removes the OS icon. Held for the app's lifetime.
let tray: Tray | null = null;
let platform: NodeJS.Platform = process.platform;
let frames = new Map<string, NativeImage>();
let scheduler: ReturnType<typeof createTrayScheduler> | null = null;
let lastSnapshot: TrayStatusSnapshot | null = null;
let currentMenu: Menu | null = null;
let lastMenuKey: string | null = null;
let lastTooltip: string | null = null;
let actionSender: ((a: TrayAction) => void) | null = null;
let deps: TrayDeps | null = null;
let loggedMissingFrame = false;
let powerListeners: { onBattery: () => void; onAc: () => void } | null = null;

const mainState: MainTrayState = {
  update: 'none',
  downloadPercent: 0,
  windowFocused: true,
  onBattery: false,
  richAnimation: true,
  isDev: false,
};

export interface TrayDeps {
  getMainWindow: () => BrowserWindow | null;
  createWindow: () => void;
  /** The same function the `update:download` handler calls (spec R4). */
  downloadUpdate: () => Promise<unknown>;
  /** `safeQuitAndInstall` — the same path the renderer's install button takes. */
  installUpdate: () => Promise<unknown>;
}

function errMessage(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback;
}

function readAnimationSettings(): { rich: boolean; reduced: boolean } {
  try {
    const s = systemPreferences.getAnimationSettings();
    return { rich: s.shouldRenderRichAnimation, reduced: s.prefersReducedMotion };
  } catch {
    // Unknown → the conservative reading: no rich motion.
    return { rich: false, reduced: true };
  }
}

function setFrame(mode: RenderMode, key: string): void {
  if (tray === null) return;
  let image = frames.get(`${mode}/${key}`);
  if (image === undefined) {
    if (!loggedMissingFrame) {
      loggedMissingFrame = true;
      // Mode and key are both from closed tables (manifest / resolver), never user text.
      console.error('Tray frame missing:', `${mode}/${key}`);
    }
    image = frames.get(`${fallbackModeFor(platform)}/${FALLBACK_KEY}`);
  }
  if (image !== undefined) tray.setImage(image);
}

function sendAction(action: TrayAction): void {
  if (actionSender === null) return;
  try {
    actionSender(action);
  } catch (err) {
    console.error('Tray action send failed:', errMessage(err, 'tray_send_failed'));
  }
}

function activate(): void {
  if (deps === null) return;
  const win = deps.getMainWindow();
  applyActivate(win, decideActivateAction(win), deps.createWindow);
}

function runUpdateAction(run: (() => Promise<unknown>) | undefined): void {
  if (run === undefined) return;
  run().catch((err: unknown) => {
    console.error('Tray update action failed:', errMessage(err, 'tray_update_failed'));
  });
}

function onMenuClick(id: TrayMenuId, checked: boolean | undefined): void {
  const effect = menuEffectFor(id, checked);
  if (effect === null) return;
  switch (effect.type) {
    case 'activate':
      activate();
      return;
    case 'activate-send':
      activate();
      sendAction(effect.action);
      return;
    case 'send':
      sendAction(effect.action);
      return;
    case 'quit':
      // Bare app.quit(): before-quit sets isQuitting, the re-entrant window
      // close passes the #1383 guard, quit completes. The tray adds NO
      // preventDefault path.
      app.quit();
      return;
    case 'download-update':
      runUpdateAction(deps?.downloadUpdate);
      return;
    case 'restart-update':
      runUpdateAction(deps?.installUpdate);
      return;
  }
}

function render(): void {
  if (tray === null || tray.isDestroyed() || scheduler === null) return;
  const motion = readAnimationSettings();
  mainState.richAnimation = motion.rich;
  const visual = resolveTrayVisual(lastSnapshot, mainState, platform);
  const gate: MotionGate = {
    reducedMotion: (lastSnapshot?.reducedMotion ?? false) || motion.reduced,
    richAnimation: motion.rich,
    onBattery: mainState.onBattery,
    dnd: lastSnapshot?.presence === 'dnd',
    platform,
  };
  scheduler.apply(visual, gate);
  if (visual.tooltip !== lastTooltip) {
    tray.setToolTip(visual.tooltip);
    lastTooltip = visual.tooltip;
  }
  // Rebuild whole, re-set only when the model changed (R7). macOS never holds a
  // context menu: click and right-click pop the current one up (R6).
  if (visual.menuKey !== lastMenuKey) {
    currentMenu = Menu.buildFromTemplate(buildTrayMenuTemplate(visual.menu, onMenuClick));
    lastMenuKey = visual.menuKey;
    if (platform !== 'darwin') tray.setContextMenu(currentMenu);
  }
}

function safeRender(): void {
  try {
    render();
  } catch (err) {
    console.error('Tray render failed:', errMessage(err, 'tray_render_failed'));
  }
}

function popUpMenu(): void {
  if (tray === null || currentMenu === null) return;
  tray.popUpContextMenu(currentMenu);
}

function wireClicks(t: Tray): void {
  if (platform === 'darwin') {
    t.setIgnoreDoubleClickEvents(true);
    t.on('click', popUpMenu);
    t.on('right-click', popUpMenu);
  } else if (platform === 'win32') {
    // Right-click shows the native context menu set in render().
    t.on('click', popUpMenu);
    t.on('double-click', activate);
  } else {
    // Linux: SNI activation opens; the menu is the host's context menu.
    t.on('click', activate);
  }
}

function addPowerListeners(): void {
  const onBattery = (): void => {
    mainState.onBattery = true;
    safeRender();
  };
  const onAc = (): void => {
    mainState.onBattery = false;
    safeRender();
  };
  powerListeners = { onBattery, onAc };
  powerMonitor.on('on-battery', onBattery);
  powerMonitor.on('on-ac', onAc);
}

function removePowerListeners(): void {
  if (powerListeners === null) return;
  const { onBattery, onAc } = powerListeners;
  powerListeners = null;
  try {
    powerMonitor.removeListener('on-battery', onBattery);
    powerMonitor.removeListener('on-ac', onAc);
  } catch {
    // best-effort on teardown
  }
}

function resetRenderCaches(): void {
  scheduler?.dispose();
  scheduler = null;
  currentMenu = null;
  lastMenuKey = null;
  lastTooltip = null;
  frames = new Map();
}

export function initTray(trayDeps: TrayDeps): void {
  if (tray !== null) return; // idempotent — one tray per app lifetime
  let t: Tray | null = null;
  try {
    platform = process.platform;
    deps = trayDeps;
    mainState.isDev = !app.isPackaged;
    try {
      mainState.onBattery = powerMonitor.isOnBatteryPower();
    } catch {
      mainState.onBattery = false;
    }
    frames = decodeFrames(platform);
    const initial = frames.get(`${fallbackModeFor(platform)}/${FALLBACK_KEY}`);
    // An empty image can make new Tray() "succeed" as an invisible icon the user
    // cannot see or click. That would defeat the trayless fallbacks
    // (isTrayActive() true, nothing to click), so treat it as init failure.
    if (initial === undefined) {
      throw new Error('tray icon missing or unreadable');
    }
    t = new Tray(initial);
    const created = t;
    scheduler = createTrayScheduler({
      setFrame: (mode, key) => {
        if (tray === created) setFrame(mode, key);
      },
      now: () => Date.now(),
    });
    wireClicks(created);
    addPowerListeners();
    tray = created;
    // A snapshot that arrived before init was stored; render it (or the neutral state).
    render();
  } catch (err) {
    // Destroy a half-constructed tray (new Tray() succeeded but wiring threw)
    // so no handler-less orphan icon lingers until GC collects it.
    removePowerListeners();
    resetRenderCaches();
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
    console.error('Tray init failed:', errMessage(err, 'tray_init_failed'));
    tray = null;
  }
}

/**
 * Store the renderer's latest snapshot (or `null` = renderer gone / reloading,
 * spec R17) and render it when the tray is active. Never throws.
 */
export function applyTraySnapshot(s: TrayStatusSnapshot | null): void {
  lastSnapshot = s;
  if (!isTrayActive()) return;
  safeRender();
}

/** Main-owned update phase, from updater.ts (spec §4). */
export function notifyTrayUpdate(state: MainTrayState['update'], percent?: number): void {
  mainState.update = state;
  if (typeof percent === 'number' && Number.isFinite(percent)) mainState.downloadPercent = percent;
  safeRender();
}

/** Window focus gates the mention burst (spec §3). */
export function setTrayWindowFocused(focused: boolean): void {
  if (mainState.windowFocused === focused) return;
  mainState.windowFocused = focused;
  safeRender();
}

/** How `tray:action` reaches the renderer; main.ts binds it to the main window. */
export function setTrayActionSender(send: (a: TrayAction) => void): void {
  actionSender = send;
}

/** Live animation timers (0 or 1); exposed for teardown tests. */
export function trayActiveTimers(): number {
  return scheduler?.activeTimers() ?? 0;
}

/**
 * Teardown order (spec §4): timer, then powerMonitor listeners, then snapshot
 * and caches, then `tray.destroy()`.
 */
export function destroyTray(): void {
  scheduler?.dispose();
  removePowerListeners();
  lastSnapshot = null;
  resetRenderCaches();
  if (tray !== null && !tray.isDestroyed()) {
    tray.destroy();
  }
  tray = null;
}

export function isTrayActive(): boolean {
  return tray !== null && !tray.isDestroyed();
}
