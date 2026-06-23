import { dialog, type BrowserWindow } from 'electron';
import { closeSplash } from './splashWindow';

export function revealLoadFailure(window: BrowserWindow | null, message: string): void {
  closeSplash();
  if (window && !window.isDestroyed()) {
    window.show();
  }
  dialog.showErrorBox('Concord Voice', message);
}
