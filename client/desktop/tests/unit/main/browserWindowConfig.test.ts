import { describe, it, expect } from 'vitest';
import { buildBrowserWindowConfig } from '../../../src/main/browserWindowConfig';

describe('buildBrowserWindowConfig', () => {
  it('returns hiddenInset for darwin (macOS)', () => {
    const config = buildBrowserWindowConfig({
      platform: 'darwin',
      isWayland: false,
      preloadPath: '/path/to/preload.js',
      isPackaged: true,
    });
    expect(config.titleBarStyle).toBe('hiddenInset');
    expect(config.titleBarOverlay).toBeUndefined();
  });

  it('returns hidden + titleBarOverlay for win32', () => {
    const config = buildBrowserWindowConfig({
      platform: 'win32',
      isWayland: false,
      preloadPath: '/path/to/preload.js',
      isPackaged: true,
    });
    expect(config.titleBarStyle).toBe('hidden');
    expect(config.titleBarOverlay).toEqual(expect.objectContaining({ height: 32 }));
  });

  it('returns hidden + titleBarOverlay for linux (X11)', () => {
    const config = buildBrowserWindowConfig({
      platform: 'linux',
      isWayland: false,
      preloadPath: '/path/to/preload.js',
      isPackaged: true,
    });
    expect(config.titleBarStyle).toBe('hidden');
    expect(config.titleBarOverlay).toEqual(expect.objectContaining({ height: 32 }));
  });

  it('returns hidden + titleBarOverlay for linux (Wayland) — same as X11', () => {
    const config = buildBrowserWindowConfig({
      platform: 'linux',
      isWayland: true,
      preloadPath: '/path/to/preload.js',
      isPackaged: true,
    });
    expect(config.titleBarStyle).toBe('hidden');
    expect(config.titleBarOverlay).toBeDefined();
  });

  it('preserves common keys across all platforms', () => {
    const config = buildBrowserWindowConfig({
      platform: 'linux',
      isWayland: false,
      preloadPath: '/preload.js',
      isPackaged: true,
    });
    expect(config.width).toBe(1200);
    expect(config.height).toBe(800);
    expect(config.minWidth).toBe(800);
    expect(config.minHeight).toBe(600);
    expect(config.backgroundColor).toBe('#1a1a1a');
    expect(config.show).toBe(false);
    expect(config.webPreferences?.preload).toBe('/preload.js');
    expect(config.webPreferences?.contextIsolation).toBe(true);
    expect(config.webPreferences?.nodeIntegration).toBe(false);
    expect(config.webPreferences?.sandbox).toBe(true);
  });

  it('disables sandbox + webSecurity when not packaged (dev mode)', () => {
    const config = buildBrowserWindowConfig({
      platform: 'darwin',
      isWayland: false,
      preloadPath: '/preload.js',
      isPackaged: false,
    });
    expect(config.webPreferences?.sandbox).toBe(false);
    expect(config.webPreferences?.webSecurity).toBe(false);
  });
});
