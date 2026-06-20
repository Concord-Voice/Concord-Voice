import type { BrowserWindowConstructorOptions } from 'electron';

export interface BrowserWindowConfigInput {
  platform: NodeJS.Platform;
  isWayland: boolean;
  preloadPath: string;
  isPackaged: boolean;
  titleBarOverlay?: { color?: string; symbolColor?: string; height?: number };
}

const DEFAULT_OVERLAY = {
  color: '#1a1a1a',
  symbolColor: '#ffffff',
  height: 32,
};

export function buildBrowserWindowConfig(
  input: BrowserWindowConfigInput
): BrowserWindowConstructorOptions {
  const base: BrowserWindowConstructorOptions = {
    width: 1200,
    height: 800,
    minWidth: 800,
    minHeight: 600,
    backgroundColor: '#1a1a1a',
    show: false,
    webPreferences: {
      preload: input.preloadPath,
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: input.isPackaged,
      webSecurity: input.isPackaged,
    },
  };

  if (input.platform === 'darwin') {
    return { ...base, titleBarStyle: 'hiddenInset' };
  }

  const overlay = { ...DEFAULT_OVERLAY, ...input.titleBarOverlay };
  return {
    ...base,
    titleBarStyle: 'hidden',
    titleBarOverlay: overlay,
  };
}
