import { app, screen, type BrowserWindow } from 'electron';
import fs from 'node:fs';
import path from 'node:path';
import { validateBounds, type SavedState } from './windowStateValidator';
import { isWayland } from './waylandDetect';

const STATE_FILE = 'window-state.json';
const DEBOUNCE_MS = 500;

const DEFAULT_STATE = {
  x: undefined as number | undefined,
  y: undefined as number | undefined,
  width: 1200,
  height: 800,
  isMaximized: false,
};

function statePath(): string {
  return path.join(app.getPath('userData'), STATE_FILE);
}

export function loadWindowState(): {
  x: number | undefined;
  y: number | undefined;
  width: number;
  height: number;
  isMaximized: boolean;
} {
  let raw: unknown;
  try {
    const json = fs.readFileSync(statePath(), 'utf8');
    raw = JSON.parse(json);
  } catch {
    return { ...DEFAULT_STATE };
  }

  if (isWayland()) {
    const r = raw as Partial<SavedState>;
    const width = typeof r.width === 'number' && r.width >= 800 ? r.width : 1200;
    const height = typeof r.height === 'number' && r.height >= 600 ? r.height : 800;
    return {
      x: undefined,
      y: undefined,
      width,
      height,
      isMaximized: r.isMaximized === true,
    };
  }

  const displays = screen.getAllDisplays().map((d) => ({ workArea: d.workArea }));
  const validated = validateBounds(raw, displays);
  if (!validated) return { ...DEFAULT_STATE };
  return {
    x: validated.x,
    y: validated.y,
    width: validated.width,
    height: validated.height,
    isMaximized: validated.isMaximized,
  };
}

export function saveWindowState(input: {
  bounds: { x: number; y: number; width: number; height: number };
  isMaximized: boolean;
}): void {
  const wayland = isWayland();
  const payload = wayland
    ? {
        width: input.bounds.width,
        height: input.bounds.height,
        isMaximized: input.isMaximized,
      }
    : { ...input.bounds, isMaximized: input.isMaximized };

  const tmp = statePath() + '.tmp';
  const final = statePath();
  try {
    fs.writeFileSync(tmp, JSON.stringify(payload, null, 2), 'utf8');
    fs.renameSync(tmp, final);
  } catch (err) {
    console.error('[windowState] save failed:', (err as Error).message);
  }
}

let debounceTimer: NodeJS.Timeout | null = null;

export function attachWindowState(win: BrowserWindow): void {
  const debouncedSave = () => {
    if (debounceTimer) clearTimeout(debounceTimer);
    debounceTimer = setTimeout(() => {
      saveWindowState({
        bounds: win.getBounds(),
        isMaximized: win.isMaximized(),
      });
    }, DEBOUNCE_MS);
  };

  win.on('resize', debouncedSave);
  win.on('move', debouncedSave);
  win.on('maximize', debouncedSave);
  win.on('unmaximize', debouncedSave);

  win.on('close', () => {
    if (debounceTimer) clearTimeout(debounceTimer);
    saveWindowState({
      bounds: win.getBounds(),
      isMaximized: win.isMaximized(),
    });
  });
}
