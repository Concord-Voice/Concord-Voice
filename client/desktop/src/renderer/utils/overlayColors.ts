/**
 * Resolve titleBarOverlay colors for the per-platform Electron native
 * window-control set (Win/Linux). macOS ignores titleBarOverlay — its
 * native traffic-light controls are themed by the OS.
 *
 * Called by the settingsStore theme subscriber and pushed to main via
 * window:setTitleBarOverlayColor IPC (#806 Task 7's surface).
 */

export interface OverlayColors {
  color: string;
  symbolColor: string;
}

const DARK: OverlayColors = {
  color: '#1a1a1a',
  symbolColor: '#ffffff',
};

const LIGHT: OverlayColors = {
  color: '#f5f5f5',
  symbolColor: '#1a1a1a',
};

export function deriveOverlayColors(theme: 'light' | 'dark'): OverlayColors {
  return theme === 'light' ? LIGHT : DARK;
}
